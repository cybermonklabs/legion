package main

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// The boot's repair withdraws the open ask a close by an older release left on a closed issue,
// naming that close's actor, and logs its counts.
func TestRepairIssueAsksAtBootWithdrawsAndLogsItsCounts(t *testing.T) {
	// Not parallel: it replaces the process's default logger (logtest.SetDefault).
	ctx := store.WithTransactionTracking(context.Background())
	database := storetest.Open(t)
	for _, query := range []string{
		`insert into projects (key, name, default_assignee) values ('TEST', 'Test', 'alice')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank, closed_at) values ('TEST-1', 'TEST', 1, 'Closed issue', 'done', '{"kind":"user","id":"alice"}', 'U', now() - interval '1 hour')`,
	} {
		if _, err := database.Pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed the closed issue: %v", err)
		}
	}
	broker := events.NewBroker()
	issueKey := "TEST-1"
	closer := model.Actor{Kind: "user", ID: "alice"}
	ask := seedOpenAskClosedBy(t, ctx, database, broker, issueKey, closer)
	documents := docs.New(docs.Deps{Store: database, Events: broker})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	var logs bytes.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	repairIssueAsks(ctx, api.Deps{Store: database, Docs: documents, Events: broker}, issueAskRepairBootTimeout)

	if out := logs.String(); !strings.Contains(out, "repaired the asks of closed and reopened issues") || !strings.Contains(out, "withdrawn=1 restored=0 swept=0") {
		t.Errorf("logged, want the counts withdrawn=1 restored=0 swept=0:\n%s", out)
	}
	var state, kind, actor string
	if err := database.Pool.QueryRow(ctx, `
		select state, resolution->>'kind', resolution->'actor'->>'id' from asks where issue_key = $1
	`, issueKey).Scan(&state, &kind, &actor); err != nil {
		t.Fatalf("read the ask: %v", err)
	}
	if state != "resolved" || kind != "withdrawn" || actor != closer.ID {
		t.Errorf("the ask = %s %s by %s, want resolved withdrawn by %s", state, kind, actor, closer.ID)
	}
	if count := 0; database.Pool.QueryRow(ctx, `select count(*) from events where type = 'ask.resolved' and payload->>'id' = $1`, ask).Scan(&count) != nil || count != 1 {
		t.Errorf("the ask has %d ask.resolved events, want 1", count)
	}
}

// The boot's repair whose every failure is a busy refusal of the document service logs its pass
// at Warn, naming the next pass that repairs the issues, as the interval pass logs such a pass
// (api.LogIssueAskRepairFailure). Here a closed issue's open ask is withdrawn with a sweep of the
// issue's document, whose room is resident, so the sweep's fork is a warm one whose first wait is
// its cover's take of the pod's load concurrency, which the test holds full: the boot's bound ends
// that wait, and the fork is refused busy (docs' errWriteBusy wrapping the deadline,
// docs.RefusedBusy). Before, the boot logged every failure of its pass at Error.
func TestRepairIssueAsksAtBootLogsAPassRefusedOnlyBusyAtWarn(t *testing.T) {
	// refusedBootTimeout stands for the boot's bound (issueAskRepairBootTimeout), which ends the
	// sweep's wait for the full pool here as that bound would.
	const refusedBootTimeout = 2 * time.Second
	const spec = "6b1f8e0a-6f0c-4c55-9a4e-2f1d3c4b5a60"
	const issueKey = "TEST-1"
	ctx := context.Background()
	database := storetest.Open(t)
	for _, query := range []string{
		`insert into projects (key, name, default_assignee) values ('TEST', 'Test', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('TEST-1', 'TEST', 1, 'Closed while the pool is full', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := database.Pool.Exec(ctx, query); err != nil {
			t.Fatalf("seed the issue: %v", err)
		}
	}
	broker := events.NewBroker()
	documents := docs.New(docs.Deps{Store: database, Events: broker, Settle: time.Hour})
	t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
	alice := model.Actor{Kind: "user", ID: "alice"}
	seed, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Rollback(ctx)
	seedCtx, ledger := documents.Join(ctx, seed)
	defer ledger.Discard()
	if _, err := seed.Exec(ctx, `
		insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, 'TEST-1', 'TEST', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
	`, spec); err != nil {
		t.Fatalf("seed the issue's document: %v", err)
	}
	markdown, err := documents.SeedText(seedCtx, spec, "Context\n", alice)
	if err != nil {
		t.Fatalf("seed the document's text: %v", err)
	}
	if _, err := seed.Exec(ctx, `insert into artifact_versions (artifact_id, number, markdown) values ($1, 1, $2)`, spec, markdown); err != nil {
		t.Fatalf("seed the document's version: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the document's seed: %v", err)
	}
	// The settlement loads the document's room, which stays until ygo's idle minute runs out, so
	// the sweep's fork is a warm one, whose first wait is its cover's take of the pool.
	if err := documents.SettleIssueDocuments(ctx, issueKey); err != nil {
		t.Fatalf("settle the issue's document: %v", err)
	}
	if !slices.Contains(documents.ResidentRooms(), spec) {
		t.Fatal("the issue's document has no resident room, so the sweep's fork would read the store first and nothing here is tested")
	}
	// The issue closes as a release older than the withdrawal closed one, leaving its ask open.
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, issueKey); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	ask := seedOpenAskClosedBy(t, ctx, database, broker, issueKey, alice)
	pool, restore := docs.UseOneLoadSlotForTest(nil)
	t.Cleanup(restore)
	release, filled := pool.FillForTest()
	if !filled {
		t.Fatal("the pool refused the bytes its holders do not hold")
	}
	t.Cleanup(release)
	var logs logtest.Buffer
	logtest.SetDefault(t, slog.NewTextHandler(&logs, nil))

	repairIssueAsks(ctx, api.Deps{Store: database, Docs: documents, Events: broker}, refusedBootTimeout)

	logged := logs.Lines(`msg="dispatch: repair the asks of closed and reopened issues"`)
	if strings.Count(logged, "\n") != 1 || !strings.Contains(logged, "level=WARN") ||
		!strings.Contains(logged, `retry="the next pass repairs the issues"`) ||
		!strings.Contains(logged, "as many whole-document copies as its memory allows") {
		t.Fatalf("the boot's repair logged:\n%s\nwant one line at Warn with its retry, naming the sweep's busy refusal", logged)
	}
	if state := ""; database.Pool.QueryRow(ctx, `select state from asks where id = $1`, ask).Scan(&state) != nil || state != "open" {
		t.Errorf("the ask reads %q once the refused repair ended, want it open: the repair committed", state)
	}
}

// seedOpenAskClosedBy seeds an open ask of planner's on issueKey and appends, in one transaction,
// its ask.opened event and the issue.closed event of closer's close, and answers the ask's id.
func seedOpenAskClosedBy(t *testing.T, ctx context.Context, database *store.Store, broker *events.Broker, issueKey string, closer model.Actor) string {
	t.Helper()
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, state) values ($1, '{"kind":"session","id":"planner"}', 'Still open?', 'open') returning id::text
	`, issueKey).Scan(&ask); err != nil {
		t.Fatalf("seed the open ask: %v", err)
	}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	opened := model.NewAskEventPayload(model.Ask{
		ID: ask, IssueKey: &issueKey, Author: model.Actor{Kind: "session", ID: "planner"}, Question: "Still open?", State: "open",
	}, model.ReferenceChanges{})
	for _, event := range []model.Event{
		{IssueKey: &issueKey, Type: "ask.opened", Actor: model.Actor{Kind: "session", ID: "planner"}, Payload: opened},
		{IssueKey: &issueKey, Type: "issue.closed", Actor: closer, Payload: map[string]any{"key": issueKey, "status": "done"}},
	} {
		if _, err := broker.Append(ctx, tx, event); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ask
}
