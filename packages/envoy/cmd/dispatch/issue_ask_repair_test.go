package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

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
	var ask string
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, state) values ('TEST-1', '{"kind":"session","id":"planner"}', 'Still open?', 'open') returning id::text
	`).Scan(&ask); err != nil {
		t.Fatalf("seed the open ask: %v", err)
	}
	broker := events.NewBroker()
	issueKey := "TEST-1"
	closer := model.Actor{Kind: "user", ID: "alice"}
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

// The started server's repair pass judges the form of The Spec as a reopen does (R49: "This holds
// for each route that opens an approval request"). Two issues under the root ACME-1 each hold an
// approval request that a close withdrew, and a release older than the withdrawal reopened both.
// The boot's pass leaves resolved, with the findings, the request whose spec breaks the form, and
// restores the clean one. The process itself builds the dependencies the pass runs on, so only the
// started binary shows which ones the pass gets.
func TestTheStartedServersRepairPassJudgesTheFormOfTheSpec(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	options := dispatchOptions{Env: map[string]string{"DISPATCH_SPEC_ROOTS": "ACME-1"}}
	first := startDispatchProcessWith(t, databaseURL, options)
	first.waitHealthy(t)
	first.createProject(t, "ACME", "Acme")
	if root, _ := first.createIssueIn(t, "ACME", "The root of The Spec", "# The Spec\n\nThe design of the written account.\n"); root != "ACME-1" {
		t.Fatalf("the first issue of ACME is %s, want ACME-1", root)
	}
	broken := first.withdrawnRequestUnderTheRoot(t, "A broken request",
		"Dispatch stores each comment. Dispatch indexes each comment. Dispatch shows each comment.")
	clean := first.withdrawnRequestUnderTheRoot(t, "A clean request", "Dispatch stores each comment.")
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	reopener := model.Actor{Kind: "session", ID: "session-reopener"}
	for _, request := range []withdrawnRequest{broken, clean} {
		reopenAsAnOlderRelease(t, database, request.issue, reopener)
	}
	second := startDispatchProcessWith(t, databaseURL, options)
	second.waitHealthy(t)

	if lines := second.logLines("repaired the asks of closed and reopened issues"); len(lines) != 1 ||
		!holdsAll(lines[0], "restored=1", "left_resolved=1") {
		t.Errorf("the boot's repair logged %q, want one line with restored=1 and left_resolved=1", lines)
	}
	wantReason := "Dispatch did not open this approval request again, because 1 text breaks the form of The Spec.\n" +
		"- statement store-comment in spec.md version 1: it has 3 sentences, and a statement has one or two.\n" +
		"Fix each text, then request approval again."
	if got := second.readRepairedAsk(t, broken.ask); got.State != "resolved" || got.Resolution == nil ||
		got.Resolution.Kind != "resolved" || got.Resolution.Reason != wantReason || got.Resolution.Actor.ID != reopener.ID {
		t.Errorf("the broken request reads %s with resolution %+v, want it left resolved by %s with the reason\n%s",
			got.State, got.Resolution, reopener.ID, wantReason)
	}
	if got := second.readRepairedAsk(t, clean.ask); got.State != "open" || got.Resolution != nil {
		t.Errorf("the clean request reads %s with resolution %+v, want it restored open", got.State, got.Resolution)
	}
}

// The started server's repair pass judges a withdrawn request against the list of terms too
// (R50). The root's terms.md, which the person approved, defines session. Two issues under the
// root each hold an approval request that a close withdrew, and a release older than the
// withdrawal reopened both. The boot's pass leaves resolved, with the finding, the request whose
// spec defines session again, and restores the one whose spec defines a new term.
func TestTheStartedServersRepairPassLeavesASecondDefinitionResolved(t *testing.T) {
	database := storetest.Open(t)
	databaseURL := database.Pool.Config().ConnString()
	options := dispatchOptions{Env: map[string]string{"DISPATCH_SPEC_ROOTS": "ACME-1"}}
	first := startDispatchProcessWith(t, databaseURL, options)
	first.waitHealthy(t)
	first.createProject(t, "ACME", "Acme")
	if root, _ := first.createIssueIn(t, "ACME", "The root of The Spec", "# The Spec\n\nThe design of the written account.\n"); root != "ACME-1" {
		t.Fatalf("the first issue of ACME is %s, want ACME-1", root)
	}
	uploaded := first.call(t, http.MethodPost, "/api/v1/issues/ACME-1/artifacts",
		`{"name":"terms.md","content":"# Terms\n\n- **session**: One conversation of an agent, with its id, in which the agent does its work.\n"}`,
		http.StatusCreated)
	var terms struct {
		Artifact struct {
			ID string `json:"id"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal(uploaded, &terms); err != nil || terms.Artifact.ID == "" {
		t.Fatalf("upload terms.md: %s (%v), want the document's id", uploaded, err)
	}
	first.call(t, http.MethodPost, "/api/v1/artifacts/"+terms.Artifact.ID+"/reviews", `{"state":"approved"}`, http.StatusCreated)
	second := first.withdrawnSpecUnderTheRoot(t, "Defines session again", "# Spec\n\n## Terms\n\n- **session**: The record of one agent process in Envoy.\n")
	clean := first.withdrawnSpecUnderTheRoot(t, "Defines lease", "# Spec\n\n## Terms\n\n- **lease**: The right of one session to one work unit for a time.\n")
	first.Terminate(t)
	first.WaitExit(t, "SIGTERM")

	reopener := model.Actor{Kind: "session", ID: "session-reopener"}
	for _, request := range []withdrawnRequest{second, clean} {
		reopenAsAnOlderRelease(t, database, request.issue, reopener)
	}
	restarted := startDispatchProcessWith(t, databaseURL, options)
	restarted.waitHealthy(t)

	if lines := restarted.logLines("repaired the asks of closed and reopened issues"); len(lines) != 1 ||
		!holdsAll(lines[0], "restored=1", "left_resolved=1") {
		t.Errorf("the boot's repair logged %q, want one line with restored=1 and left_resolved=1", lines)
	}
	wantReason := "Dispatch did not open this approval request again, because 1 text breaks the form of The Spec.\n" +
		"- definition session in spec.md version 1: terms.md version 1 of ACME-1 already defines this term. " +
		"Use that definition, or change it there and request approval of that document. When that issue has the status done, reopen it first.\n" +
		"Fix each text, then request approval again."
	if got := restarted.readRepairedAsk(t, second.ask); got.State != "resolved" || got.Resolution == nil ||
		got.Resolution.Kind != "resolved" || got.Resolution.Reason != wantReason || got.Resolution.Actor.ID != reopener.ID {
		t.Errorf("the request with a second definition reads %s with resolution %+v, want it left resolved by %s with the reason\n%s",
			got.State, got.Resolution, reopener.ID, wantReason)
	}
	if got := restarted.readRepairedAsk(t, clean.ask); got.State != "open" || got.Resolution != nil {
		t.Errorf("the request with a new term reads %s with resolution %+v, want it restored open", got.State, got.Resolution)
	}
}

// withdrawnRequest is an issue and the approval request of its spec that its close withdrew.
type withdrawnRequest struct{ issue, ask string }

// withdrawnRequestUnderTheRoot makes an issue outside every root whose spec holds statement, opens
// the approval request of the spec as an agent, moves the issue under ACME-1 and closes it, which
// withdraws the request. Outside the root the request opens whatever the statement says.
func (p *dispatchProcess) withdrawnRequestUnderTheRoot(t *testing.T, title, statement string) withdrawnRequest {
	t.Helper()
	return p.withdrawnSpecUnderTheRoot(t, title, "# Spec\n\n:::requirement{#store-comment}\n"+statement+"\n:::\n")
}

// withdrawnSpecUnderTheRoot is withdrawnRequestUnderTheRoot for an issue whose whole spec is spec.
func (p *dispatchProcess) withdrawnSpecUnderTheRoot(t *testing.T, title, spec string) withdrawnRequest {
	t.Helper()
	key, artifactID := p.createIssueIn(t, "ACME", title, spec)
	answer := p.send(t, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests",
		`{"actor":{"kind":"session","id":"session-0123456789abcdef"}}`,
		http.Header{"Authorization": {"Bearer " + dispatchTestAgentToken}})
	var opened struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(answer.body, &opened); answer.status != http.StatusCreated || err != nil || opened.Ask.ID == "" {
		t.Fatalf("request approval of %s: %s (%v), want 201 with the ask", key, answer, err)
	}
	p.call(t, http.MethodPatch, "/api/v1/issues/"+key, `{"parent":"ACME-1"}`, http.StatusOK)
	p.call(t, http.MethodPatch, "/api/v1/issues/"+key, `{"status":"done"}`, http.StatusOK)
	return withdrawnRequest{issue: key, ask: opened.Ask.ID}
}

// reopenAsAnOlderRelease reopens issue as a release older than the withdrawal did, with no process
// serving: the issue's row reads todo, an issue.updated event names actor, and its withdrawn asks
// stay withdrawn.
func reopenAsAnOlderRelease(t *testing.T, database *store.Store, issue string, actor model.Actor) {
	t.Helper()
	ctx := store.WithTransactionTracking(context.Background())
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the reopen of %s: %v", issue, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `update issues set status = 'todo', closed_at = null, updated_at = now() where key = $1`, issue); err != nil {
		t.Fatalf("reopen %s: %v", issue, err)
	}
	event := model.Event{IssueKey: &issue, Type: "issue.updated", Actor: actor, Payload: map[string]any{"key": issue, "status": "todo"}}
	if _, err := events.NewBroker().Append(ctx, tx, event); err != nil {
		t.Fatalf("append the issue.updated event of %s: %v", issue, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the reopen of %s: %v", issue, err)
	}
}

// readRepairedAsk is the ask as the process's API reads it (GET /api/v1/asks/{id}).
func (p *dispatchProcess) readRepairedAsk(t *testing.T, askID string) model.Ask {
	t.Helper()
	answer := p.call(t, http.MethodGet, "/api/v1/asks/"+askID, "", http.StatusOK)
	var read struct {
		Ask model.Ask `json:"ask"`
	}
	if err := json.Unmarshal(answer, &read); err != nil {
		t.Fatalf("decode ask %s: %v: %s", askID, err, answer)
	}
	return read.Ask
}
