package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// roleEventsLogSize is the event log the role events' plan is taken over: a million events, the
// size at which legion's readSessionIssues recorded the planner trading the issues' index for the
// session partial index, and above the 831,000 of ten times INFRA's rate.
const roleEventsLogSize = 1_000_000

// TestTheRoleEventsReadReadsTheIssuesRowsByIssue: the read of the first event each session wrote
// on each issue of a scope (roleEventsStatement) reads those issues' rows through events' unique
// (issue_key, seq), events_issue_key_seq_key, and never the partial index on a session's id
// (events_session_actor_created_at_idx) nor the whole table, under the plan with its values and
// under the generic plan a statement pgx caches can settle on, over a log of a million events.
//
// The log is three issues of fifty events each, half of them by role sessions, beside a million
// events every one of which a session wrote on another issue: the shape in which a statement that
// tests the issues and the session beside each other in one condition can read the session
// index, since the planner holds no statistics on actor's fields.
func TestTheRoleEventsReadReadsTheIssuesRowsByIssue(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	exec := func(what, sql string, args ...any) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	exec("write the project", `insert into projects (key, name, default_assignee) values ('INFRA', 'INFRA', 'alice')`)
	const fillerIssues, perIssue = 10_000, roleEventsLogSize / 10_000
	seeded := time.Now()
	exec("write the issues", `
		insert into issues (key, project_key, number, title, status, rank, created_by)
		select 'INFRA-' || n, 'INFRA', n, 'An issue', 'todo', lpad(n::text, 8, '0'), '{"kind":"user","id":"alice"}'::jsonb
		from generate_series(1, $1::int + 3) n`, fillerIssues)
	exec("write the million events", `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
		select 'INFRA-' || (4 + (n - 1) / $2::int), 1 + (n - 1) % $2::int, 'comment.created',
		       jsonb_build_object('kind', 'session', 'id', 's-' || (n % 5000)), '{}'::jsonb, false, now(),
		       now() - make_interval(secs => n)
		from generate_series(1, $1::int) n`, roleEventsLogSize, perIssue)
	exec("write the three issues' events", `
		insert into events (issue_key, seq, type, actor, payload, notify, published_at, created_at)
		select 'INFRA-' || issue, n, 'comment.created',
		       case when n % 2 = 0
		            then jsonb_build_object('kind', 'session', 'id', 's-INFRA-' || issue || '-' || n,
		                   'origin', jsonb_build_object('session_title', 'Legion implementer · INFRA-' || issue))
		            else '{"kind":"user","id":"alice"}'::jsonb end,
		       '{}'::jsonb, false, now(), now() - make_interval(mins => n)
		from generate_series(1, 3) issue, generate_series(1, 50) n`)
	exec("analyze the event log", `analyze events`)
	var logged int
	if err := database.Pool.QueryRow(ctx, `select count(*) from events`).Scan(&logged); err != nil {
		t.Fatalf("count the event log: %v", err)
	}
	if logged < roleEventsLogSize {
		t.Fatalf("the log holds %d events, want at least %d", logged, roleEventsLogSize)
	}
	t.Logf("seeded %d events over %d issues in %s", logged, fillerIssues+3, time.Since(seeded).Round(time.Millisecond))

	keys := []string{"INFRA-1", "INFRA-2", "INFRA-3"}
	var literal string
	if err := database.Pool.QueryRow(ctx, `select quote_literal($1::text[]::text)`, keys).Scan(&literal); err != nil {
		t.Fatalf("quote the keys: %v", err)
	}
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the generic plan: %v", err)
	}
	defer connection.Release()

	valued := func() json.RawMessage {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the plan's transaction: %v", err)
		}
		defer tx.Rollback(ctx)
		var raw []byte
		if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+roleEventsStatement, keys).Scan(&raw); err != nil {
			t.Fatalf("explain the role events' read: %v", err)
		}
		return explaintest.OnePlan(t, raw)
	}
	for _, plan := range []struct {
		mode string
		plan json.RawMessage
	}{
		{"with its values", valued()},
		{"generic", explaintest.AnalyzedGenericPlan(t, ctx, connection, "role_events", roleEventsStatement, []string{"text[]"}, []string{literal + "::text[]"})},
	} {
		var root struct {
			Total float64 `json:"Actual Total Time"`
		}
		if err := json.Unmarshal(plan.plan, &root); err != nil {
			t.Fatalf("read the plan's time: %v", err)
		}
		t.Logf("the plan %s ran in %.3f ms:\n%s", plan.mode, root.Total, explaintest.Outline(t, plan.plan))
		if explaintest.SeqScansRelation(t, plan.plan, "events") {
			t.Errorf("the plan %s reads the whole event log:\n%s", plan.mode, explaintest.Outline(t, plan.plan))
		}
		byIssue := false
		for _, scan := range explaintest.IndexScans(t, plan.plan) {
			t.Logf("  it reads %s, keyed by %s", scan, scan.Cond)
			switch scan.Index {
			case "events_session_actor_created_at_idx":
				t.Errorf("the plan %s reads the session partial index, every session event of the log:\n%s",
					plan.mode, explaintest.Outline(t, plan.plan))
			case "events_issue_key_seq_key":
				byIssue = byIssue || strings.Contains(scan.Cond, "issue_key")
			}
		}
		if !byIssue {
			t.Errorf("the plan %s reads the issues' events through no scan of events_issue_key_seq_key keyed by issue_key:\n%s",
				plan.mode, explaintest.Outline(t, plan.plan))
		}
	}

	population := &Population{Issues: map[string]*Issue{}}
	for _, key := range keys {
		population.Issues[key] = &Issue{Key: key, FirstRoleEvent: map[string]time.Time{}}
	}
	if err := population.readRoleEvents(ctx, database.Pool, keys); err != nil {
		t.Fatalf("read the role events: %v", err)
	}
	for _, key := range keys {
		if _, worked := population.Issues[key].FirstRoleEvent["implementer"]; !worked {
			t.Errorf("%s holds no first implementer event, want the first of its twenty-five: %v", key, population.Issues[key].FirstRoleEvent)
		}
	}
	t.Logf("each of %v holds its implementer's first event", keys)
}
