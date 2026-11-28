package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// TestPresentAskReadsDoNotUseEventsAskState holds the plans of the three present reads of an
// ask's events to not reading the index for cuts. Its predicate includes created_at is not null
// because a comparison with a cut implies it but a present lookup by ask id does not.
// The control adds the same index with only the type predicate: at least one of the same
// statements must take it, so a fixture too small to prefer it cannot pass the test.
func TestPresentAskReadsDoNotUseEventsAskState(t *testing.T) {
	ctx := t.Context()
	probe := newRelationProbe()
	handler, db, _, _ := newRegisteredTestServer(t, testServerOptions{tracer: probe})
	issue := createInteractionIssue(t, handler, "APIX", "One ask to read", "")
	noise := asOfIssue(t, handler, map[string]any{"project": "APIX", "title": "Other asks"})
	list := "/api/v1/issues/" + issue.Key + "/asks"
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, list,
		map[string]any{"question": "Which plan reads the history?"}))
	one := "/api/v1/asks/" + ask.ID
	edited := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPatch, one,
		map[string]any{"question": "Which index reads the history?"}))
	asOfWrite(t, handler, http.MethodPost, one+"/answer", map[string]any{
		"text": "An index keyed by ask ID.", "expected_edited_at": edited.EditedAt,
	}, "alice")

	// The answers feed needs a recorded delivery of the answer to this session. Give only that
	// event the destination: the other asks make the log large without widening any read.
	tag, err := db.Pool.Exec(ctx, `update events set published_destinations = array[$2::text]
		where type = 'ask.answered' and payload->>'id' = $1`, ask.ID, contracts.LegacyAgentSubject(asOfAgentSession))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("mark the answer delivered: %s, %v", tag, err)
	}
	types := asof.AskStateTypes()
	if _, err := db.Pool.Exec(ctx, `
		with other_asks as (
			insert into asks (id, issue_key, author, question, options)
			select md5('outside ask ' || n)::uuid, $1, '{"kind":"user","id":"alice"}', 'An unrelated ask', '[]'
			from generate_series(1, 4000) n returning id
		)
		insert into events (issue_key, seq, type, actor, payload, notify)
		select i.key, i.last_seq + row_number() over (), s.type, '{"kind":"user","id":"alice"}',
		       jsonb_build_object('id', a.id), false
		from issues i cross join other_asks a cross join unnest($2::text[]) as s(type)
		where i.key = $1`, noise.Key, types); err != nil {
		t.Fatalf("seed outside asks and all their state event types: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}

	answers := "/api/v1/agents/" + asOfAgentSession + "/answers"
	routes := []asOfProbeRead{
		{route: "GET /api/v1/issues/{key}/asks", target: list + "?state=all&thread=1", caller: "alice"},
		{route: "GET /api/v1/asks/{id}", target: one, caller: "alice"},
		{route: "GET /api/v1/agents/{session_id}/answers", target: answers, caller: "alice"},
	}
	for _, read := range routes {
		r := httptest.NewRequestWithContext(context.WithValue(ctx, probedRead{}, read), http.MethodGet, read.target, nil)
		r.Header.Set("X-Dispatch-User", read.caller)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", read.route, response.Code, response.Body)
		}
		if read.target == answers {
			feed := decodeBody[model.SessionAnswersResponse](t, response)
			if len(feed.Items) != 1 || feed.Items[0].ID != ask.ID || feed.Items[0].Kind != model.SessionAnswerKindAnswer {
				t.Fatalf("%s: want the seeded answer, got %+v", read.route, feed.Items)
			}
		}
	}
	statements := probe.recorded()
	explain := func(q explaintest.Queryer, index string) int {
		t.Helper()
		readEvents := map[string]int{}
		indexUses := 0
		for _, s := range statements {
			if !planned(s.sql) { // EXPLAIN cannot plan transaction control or session settings.
				continue
			}
			var raw []byte
			if err := q.QueryRow(ctx, "explain (format json) "+s.sql, dismissedScaleArgs(s.args)...).Scan(&raw); err != nil {
				t.Fatalf("%s: explain %s: %v", s.read.route, s.sql, err)
			}
			plan := explaintest.OnePlan(t, raw)
			scansEvents := false
			for _, scan := range explaintest.Nodes(t, plan) {
				scansEvents = scansEvents || scan.Relation == "events"
				if scan.Index != index {
					continue
				}
				indexUses++
				if index == "events_ask_state" {
					t.Errorf("%s reads %s in statement:\n%s", s.read.route, index, s.sql)
				} else {
					t.Logf("control: %s reads %s (%s)", s.read.route, index, scan.Type)
				}
			}
			if scansEvents {
				readEvents[s.read.route]++
			}
		}
		for _, read := range routes {
			if readEvents[read.route] == 0 {
				t.Errorf("%s: no recorded statement planned an events read", read.route)
			}
		}
		return indexUses
	}

	const twin = "events_ask_state_types_only"
	// Use fresh plans of the same SQL and arguments. Check the default plans first. Then
	// compare the two predicates without events_ask_payload_id, whose lower cost can hide
	// an eligible state index on a small fixture. Roll back both index changes.
	tx, err := db.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	explain(tx, "events_ask_state")
	if _, err := tx.Exec(ctx, `drop index events_ask_payload_id`); err != nil {
		t.Fatalf("remove the competing index for both control plans: %v", err)
	}
	if _, err := tx.Exec(ctx, `set local enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if explain(tx, "events_ask_state") != 0 {
		return // Report the changed predicate's failure before adding its equivalent twin.
	}
	literals := make([]string, len(types))
	for i, kind := range types {
		literals[i] = store.QuoteLiteral(kind)
	}
	if _, err := tx.Exec(ctx, `create index `+twin+` on events ((payload->>'id'), id desc)
		include (created_at) where type in (`+strings.Join(literals, ", ")+")"); err != nil {
		t.Fatalf("build the type-list-only control index: %v", err)
	}
	if explain(tx, twin) == 0 {
		t.Fatalf("none of the present ask statements chose %s", twin)
	}
}
