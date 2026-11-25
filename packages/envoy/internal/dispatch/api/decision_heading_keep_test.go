package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A Keep of a heading of an issue's decisions document takes the
// issue's owner row before it writes, as the decisions step takes it for the whole of its
// conversion and retirement of the document, and reads under it whether the document still
// stands. A Keep that starts while the step holds the row - the person read the heading while the
// document stood - waits for the step, and once the step has retired the document it answers
// 404 TARGET_NOT_FOUND and writes no decision_keeps row: a row on a retired document is one the
// step carried onto no node, and a 200 for it would tell the person a Keep stands that nothing
// holds.
//
// The test's own transaction takes the issue's row, as the step does, and retires the document
// in it. The Keep, posted with the sum the heading's section reads at, queues behind that
// transaction, which then commits.
func TestAHeadingKeepThatWaitsBehindTheStepsRetirementIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, service := settlingCarryDocuments(t, 20*time.Millisecond)
	createReferenceAPIProject(t, handler, "KEEP")
	issue := createSpecIssue(t, handler, "KEEP", "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	const heading = "## The census runs first"
	decisions := seedDecisionsDocument(t, database, service, issue.Key, "# Decisions\n\n"+heading+"\n\nThe census reads the corpus before any view of it.\n")
	headingID := blockIDsByText(t, handler, decisions)[heading]
	if headingID == "" {
		t.Fatalf("the decisions document's heading %q has no block id", heading)
	}
	section, found, err := service.DecisionHeading(ctx, database.Pool, decisions, headingID)
	if err != nil || !found {
		t.Fatalf("the heading %s reads no section (%t, %v), want the section it opens", headingID, found, err)
	}

	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the step's transaction: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select 1 from issues where key = $1 for no key update`, issue.Key); err != nil {
		t.Fatalf("take the issue's row: %v", err)
	}
	if _, err := docs.RetireDocument(ctx, holder, decisions, docs.DecisionStepActor,
		"The decisions step converted every section of the issue's decisions document."); err != nil {
		t.Fatalf("retire the decisions document: %v", err)
	}
	kept := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		kept <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+decisions+"/blocks/"+headingID+"/keep",
			map[string]any{"text_sha256": section.TextSHA256}, "alice")
	}()
	storetest.WaitForLockWaiters(t, holder, 1, 10*time.Second)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the retirement: %v", err)
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-kept:
	case <-time.After(30 * time.Second):
		t.Fatal("the Keep answered nothing 30s after the retirement committed")
	}
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("the Keep that waited behind the retirement answers status=%d body=%s, want 404 TARGET_NOT_FOUND", response.Code, response.Body.String())
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from decision_keeps where artifact_id = $1::uuid`, decisions).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the retired document holds %d decision_keeps rows (%v), want none", rows, err)
	}
	var keptEvents int
	if err := database.Pool.QueryRow(ctx, `select count(*) from events where issue_key = $1 and type = 'decision.kept'`, issue.Key).Scan(&keptEvents); err != nil || keptEvents != 0 {
		t.Fatalf("the issue holds %d decision.kept events (%v), want none", keptEvents, err)
	}
}

// holdVersionEvents makes every artifact.version event the decisions step appends wait on the
// advisory key 1150005, which the test's own transaction takes: the step's write of an issue
// holds the issue's row from its start to its commit, so it stops there holding the row, and no
// other writer's version waits.
const holdVersionEvents = `
	create function hold_version_event() returns trigger language plpgsql as $$
	begin
		perform pg_advisory_xact_lock(tg_argv[0]::bigint);
		return new;
	end $$;
	create trigger hold_version_event before insert on events for each row
		when (new.type = 'artifact.version' and new.actor->>'id' = 'dispatch-decisions-step')
		execute function hold_version_event('1150005');`

// A Keep of a heading whose section the decisions step converts while the Keep waits for the
// issue's row answers 404 TARGET_NOT_FOUND and writes nothing: a row on it would sit on a heading
// the step carried onto no decision node. The document's other section holds an ask block no row
// names, which the step moves into the spec by R9's first further kind, so its section converts
// too and the step retires the document: under the row the Keep finds the document retired.
//
// The step's pass of the issue stops at the first version it writes, holding the issue's row,
// behind a transaction of the test's own (holdVersionEvents). The Keep, posted with the sum the
// heading's section read at before the pass, queues behind the pass, and the test's transaction
// then lets the pass go on and commit.
func TestAHeadingKeepThatWaitsBehindTheStepsConversionOfItsSectionIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, service := settlingCarryDocuments(t, 20*time.Millisecond)
	createReferenceAPIProject(t, handler, "KEPT")
	issue := createSpecIssue(t, handler, "KEPT", "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n")
	const heading = "## The census runs first"
	decisions := seedDecisionsDocument(t, database, service, issue.Key, "# Decisions\n\n"+heading+"\n\nThe census reads the corpus before any view of it.\n\n"+
		"## Which region goes first\n\n:::ask{#q-open urgency=\"low\" multiple=\"false\"}\nWhich region goes first?\n:::\n")
	headingID := blockIDsByText(t, handler, decisions)[heading]
	if headingID == "" {
		t.Fatalf("the decisions document's heading %q has no block id", heading)
	}
	section, found, err := service.DecisionHeading(ctx, database.Pool, decisions, headingID)
	if err != nil || !found {
		t.Fatalf("the heading %s reads no section (%t, %v), want the section it opens", headingID, found, err)
	}
	if _, err := database.Pool.Exec(ctx, holdVersionEvents); err != nil {
		t.Fatalf("install the trigger that holds the step's first version: %v", err)
	}
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the step: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(1150005)`); err != nil {
		t.Fatalf("take the key the step's version waits on: %v", err)
	}
	step := docs.NewDecisionStep(database, "", nil)
	step.UseDocuments(service)
	type pass struct {
		done   []docs.DecisionIssue
		failed []docs.DecisionIssueFailure
		err    error
	}
	passed := make(chan pass, 1)
	go func() {
		done, failed, err := step.RestoreAndConvert(ctx, "KEPT", true, false)
		passed <- pass{done, failed, err}
	}()
	awaitLockWaiters(t, database, 1)
	kept := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		kept <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+decisions+"/blocks/"+headingID+"/keep",
			map[string]any{"text_sha256": section.TextSHA256}, "alice")
	}()
	awaitLockWaiters(t, database, 2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("let the step's pass go on: %v", err)
	}
	var result pass
	select {
	case result = <-passed:
	case <-time.After(60 * time.Second):
		t.Fatal("the step's pass answered nothing 60s after it was let go")
	}
	if result.err != nil || len(result.failed) > 0 || len(result.done) != 1 || result.done[0].Decisions != 1 || result.done[0].Left != 0 || !result.done[0].Retired {
		t.Fatalf("the pass did %+v, %+v, %v, want the heading's section a decision, the ask's section converted and the document retired", result.done, result.failed, result.err)
	}
	var response *httptest.ResponseRecorder
	select {
	case response = <-kept:
	case <-time.After(30 * time.Second):
		t.Fatal("the Keep answered nothing 30s after the pass committed")
	}
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "TARGET_NOT_FOUND") {
		t.Fatalf("the Keep that waited behind the conversion of its section answers status=%d body=%s, want 404 TARGET_NOT_FOUND", response.Code, response.Body.String())
	}
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from decision_keeps where artifact_id = $1::uuid`, decisions).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("the decisions document holds %d decision_keeps rows (%v), want none", rows, err)
	}
}
