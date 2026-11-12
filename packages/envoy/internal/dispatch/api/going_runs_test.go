package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// goingRunsRig is a server on the harness's database: its handler, which the route is read
// through, and the same server called directly, which is how the run writes its record.
type goingRunsRig struct {
	handler  http.Handler
	server   *server
	database *store.Store
}

func newGoingRunsRig(t *testing.T) goingRunsRig {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	return goingRunsRig{handler: handler, server: directServer(deps), database: database}
}

// run records one run for login and answers its id.
func (rig goingRunsRig) run(t *testing.T, login, words string) string {
	t.Helper()
	id := uuid.NewString()
	chip := &goingContext{Path: "/issues/DEMO-12", Project: "DEMO", Issue: "DEMO-12"}
	if err := rig.server.recordGoingRun(context.Background(), id, login, words, chip); err != nil {
		t.Fatalf("record the run %q: %v", words, err)
	}
	return id
}

// age moves a run's created_at back by age.
func (rig goingRunsRig) age(t *testing.T, id string, age time.Duration) {
	t.Helper()
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update going_runs set created_at = now() - $2::interval where id = $1::uuid
	`, id, age.String()); err != nil {
		t.Fatalf("age the run: %v", err)
	}
}

// write appends one write to the author's run.
func (rig goingRunsRig) write(t *testing.T, id, login string, write goingRunWrite, issue string) {
	t.Helper()
	if err := rig.server.appendGoingRunWrite(context.Background(), id, login, write, issue); err != nil {
		t.Fatalf("append write %s: %v", write.ID, err)
	}
}

// undo appends one undo to the author's run.
func (rig goingRunsRig) undo(t *testing.T, id, login, writeID string, ok bool) {
	t.Helper()
	undo := goingRunUndo{WriteID: writeID, Outcome: goingWriteOutcome{OK: ok, Status: http.StatusOK}, At: time.Now().UTC()}
	if !ok {
		undo.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: "CHANGED_SINCE", Message: "the field changed since"}
	}
	if err := rig.server.appendGoingRunUndo(context.Background(), id, login, undo); err != nil {
		t.Fatalf("append undo of %s: %v", writeID, err)
	}
}

// list reads GET /api/v1/me/going-runs as login.
func (rig goingRunsRig) list(t *testing.T, login, query string) goingRunsPage {
	t.Helper()
	response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/going-runs"+query, nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("list %s's going runs%s: status=%d body=%s", login, query, response.Code, response.Body.String())
	}
	return decodeBody[goingRunsPage](t, response)
}

func goingRunIDs(page goingRunsPage) []string {
	ids := make([]string, 0, len(page.Rows))
	for _, run := range page.Rows {
		ids = append(ids, run.ID)
	}
	return ids
}

// statusWrite is a write of an issue's status that went through.
func statusWrite(id, issue string) goingRunWrite {
	return goingRunWrite{
		ID: id, Action: "dispatch_issue_update", Target: issue, Field: "status",
		Before: json.RawMessage(`"todo"`), After: json.RawMessage(`"in_progress"`),
		Outcome:  goingWriteOutcome{OK: true, Status: http.StatusOK},
		Approved: "auto", At: time.Now().UTC().Truncate(time.Microsecond),
	}
}

// A run is stored under its caller and read back by that caller alone, newest first; a second
// person reads none of it, and the route takes no login that could name another.
func TestGoingRunsAreReadByTheirCallerAloneNewestFirst(t *testing.T) {
	rig := newGoingRunsRig(t)
	older := rig.run(t, "alice", "what moved yesterday?")
	rig.age(t, older, time.Hour)
	newer := rig.run(t, "alice", "close DEMO-12")

	page := rig.list(t, "alice", "")
	if got := goingRunIDs(page); !slices.Equal(got, []string{newer, older}) || page.Total != 2 || page.Limit != 50 || page.Offset != 0 {
		t.Fatalf("alice's runs = %v (total %d, limit %d, offset %d), want [%s %s] newest first on the default page",
			got, page.Total, page.Limit, page.Offset, newer, older)
	}
	if run := page.Rows[0]; run.AuthorLogin != "alice" || run.Words != "close DEMO-12" ||
		run.Context == nil || run.Context.Issue != "DEMO-12" || run.Context.Path != "/issues/DEMO-12" {
		t.Fatalf("the newest run reads %+v, want alice's words and context", run)
	}
	if got := rig.list(t, "alice", "?limit=1&offset=1"); !slices.Equal(goingRunIDs(got), []string{older}) || got.Total != 2 {
		t.Fatalf("alice's second page of one = %v (total %d), want [%s] of 2", goingRunIDs(got), got.Total, older)
	}
	if got := rig.list(t, "bob", ""); len(got.Rows) != 0 || got.Total != 0 {
		t.Fatalf("bob reads %v of alice's runs, want none", goingRunIDs(got))
	}
	if _, err := rig.server.readGoingRun(context.Background(), newer, "bob"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bob reading alice's run by id: %v, want pgx.ErrNoRows", err)
	}
	if err := rig.server.appendGoingRunWrite(context.Background(), newer, "bob", statusWrite("w-bob", "DEMO-12"), "DEMO-12"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("bob appending a write to alice's run: %v, want pgx.ErrNoRows", err)
	}
}

// ?issue= keeps the caller's runs whose writes or hits touched the issue, and a second person
// who has no such run reads nothing over the same issue.
func TestGoingRunsIssueFilterReadsThePersonsRunAlone(t *testing.T) {
	rig := newGoingRunsRig(t)
	ctx := context.Background()
	wrote := rig.run(t, "alice", "start DEMO-12")
	rig.write(t, wrote, "alice", statusWrite("w-1", "DEMO-12"), "DEMO-12")
	found := rig.run(t, "alice", "what blocks DEMO-7?")
	if err := rig.server.fillGoingRun(ctx, found, goingRunParts{Hits: []going.Hit{
		{Kind: "issue", ID: "DEMO-7", Title: "Seven", Href: "/issues/DEMO-7"},
		{Kind: "document", ID: "doc-1", Title: "A plan", Href: "/documents/doc-1"},
	}}); err != nil {
		t.Fatalf("fill the hits: %v", err)
	}
	rig.run(t, "alice", "nothing about either")
	bobs := rig.run(t, "bob", "start DEMO-99")
	rig.write(t, bobs, "bob", statusWrite("w-2", "DEMO-99"), "DEMO-99")

	if got := goingRunIDs(rig.list(t, "alice", "?issue=DEMO-12")); !slices.Equal(got, []string{wrote}) {
		t.Fatalf("alice's runs over DEMO-12 = %v, want the run that wrote it [%s]", got, wrote)
	}
	if got := goingRunIDs(rig.list(t, "alice", "?issue=DEMO-7")); !slices.Equal(got, []string{found}) {
		t.Fatalf("alice's runs over DEMO-7 = %v, want the run that found it [%s]", got, found)
	}
	if got := goingRunIDs(rig.list(t, "bob", "?issue=DEMO-12")); len(got) != 0 {
		t.Fatalf("bob's runs over DEMO-12 = %v, want none: the run over it is alice's", got)
	}
	if got := goingRunIDs(rig.list(t, "alice", "?issue=DEMO-99")); len(got) != 0 {
		t.Fatalf("alice's runs over DEMO-99 = %v, want none: the run over it is bob's", got)
	}
}

// A run nobody finished keeps every part it stored and reads unfinished, and each write's
// before, after and outcome read back from the route as they were written.
func TestGoingRunsAnAbandonedRunKeepsItsOutcomesAndReadsUnfinished(t *testing.T) {
	rig := newGoingRunsRig(t)
	ctx := context.Background()
	id := rig.run(t, "alice", "assign DEMO-12 and DEMO-13 to me")
	structured := "refused"
	if err := rig.server.fillGoingRun(ctx, id, goingRunParts{
		Hits:             []going.Hit{{Kind: "issue", ID: "DEMO-13", Title: "Thirteen", Href: "/issues/DEMO-13"}},
		Decision:         json.RawMessage(`{"actions":[{"action":"dispatch_issue_update","confidence":0.93}],"act":true}`),
		Proposal:         json.RawMessage(`{"dispatch_issue_update":[{"key":"DEMO-12"},{"key":"DEMO-13"}]}`),
		StructuredOutput: &structured,
	}); err != nil {
		t.Fatalf("fill the run: %v", err)
	}
	applied := goingRunWrite{
		ID: "w-applied", Action: "dispatch_issue_update", Target: "DEMO-12", Field: "assignee",
		Before: json.RawMessage(`null`), After: json.RawMessage(`"alice"`),
		Outcome:  goingWriteOutcome{OK: true, Status: http.StatusOK},
		Approved: "person", At: time.Now().UTC().Truncate(time.Microsecond),
	}
	refused := goingRunWrite{
		ID: "w-refused", Action: "dispatch_issue_update", Target: "DEMO-13", Field: "assignee",
		Before: json.RawMessage(`"bob"`), After: json.RawMessage(`"bob"`),
		Outcome:  goingWriteOutcome{Status: http.StatusForbidden, Code: "SCOPE_REQUIRED", Message: "not yours to assign"},
		Approved: "confirmed", At: time.Now().UTC().Truncate(time.Microsecond),
	}
	rig.write(t, id, "alice", applied, "DEMO-12")
	rig.write(t, id, "alice", refused, "DEMO-13")

	page := rig.list(t, "alice", "")
	if len(page.Rows) != 1 {
		t.Fatalf("alice's runs = %v, want the one", goingRunIDs(page))
	}
	run := page.Rows[0]
	if run.Finished || run.FinishedAt != nil {
		t.Fatalf("the abandoned run reads finished %v at %v, want unfinished", run.Finished, run.FinishedAt)
	}
	if len(run.Writes) != 2 {
		t.Fatalf("the run reads %d writes, want both: %+v", len(run.Writes), run.Writes)
	}
	for index, want := range []goingRunWrite{applied, refused} {
		got := run.Writes[index]
		if got.ID != want.ID || got.Target != want.Target || got.Field != want.Field || got.Approved != want.Approved ||
			string(got.Before) != string(want.Before) || string(got.After) != string(want.After) ||
			got.Outcome != want.Outcome || !got.At.Equal(want.At) {
			t.Fatalf("write %d reads %+v (before %s, after %s), want %+v (before %s, after %s)",
				index, got, got.Before, got.After, want, want.Before, want.After)
		}
	}
	if issues := slices.Sorted(slices.Values(run.Issues)); !slices.Equal(issues, []string{"DEMO-12", "DEMO-13"}) {
		t.Fatalf("the run's issues = %v, want the hit's and both writes' once each, DEMO-12 and DEMO-13", run.Issues)
	}
	if len(run.Hits) != 1 || run.Hits[0].ID != "DEMO-13" || run.StructuredOutput == nil || *run.StructuredOutput != "refused" ||
		!jsonEqual(t, run.Decision, `{"actions":[{"action":"dispatch_issue_update","confidence":0.93}],"act":true}`) ||
		!jsonEqual(t, run.Proposal, `{"dispatch_issue_update":[{"key":"DEMO-12"},{"key":"DEMO-13"}]}`) {
		t.Fatalf("the run's parts read hits %+v, decision %s, proposal %s, structured_output %v; want what it stored",
			run.Hits, run.Decision, run.Proposal, run.StructuredOutput)
	}

	if err := rig.server.fillGoingRun(ctx, id, goingRunParts{Finished: true}); err != nil {
		t.Fatalf("finish the run: %v", err)
	}
	finished, err := rig.server.readGoingRun(ctx, id, "alice")
	if err != nil {
		t.Fatalf("read the finished run: %v", err)
	}
	if !finished.Finished || finished.FinishedAt == nil || len(finished.Writes) != 2 || len(finished.Hits) != 1 {
		t.Fatalf("the finished run reads %+v, want it finished with every part kept", finished)
	}
}

// ?undoable=true keeps the runs holding a write that went through and that no undo reverted, and
// ?q= the runs whose words hold the text in any case.
func TestGoingRunsUndoableAndWordsFilters(t *testing.T) {
	rig := newGoingRunsRig(t)
	pending := rig.run(t, "alice", "Start DEMO-1")
	rig.write(t, pending, "alice", statusWrite("w-pending", "DEMO-1"), "DEMO-1")
	undone := rig.run(t, "alice", "start DEMO-2")
	rig.write(t, undone, "alice", statusWrite("w-undone", "DEMO-2"), "DEMO-2")
	rig.undo(t, undone, "alice", "w-undone", true)
	failedUndo := rig.run(t, "alice", "start DEMO-3")
	rig.write(t, failedUndo, "alice", statusWrite("w-stuck", "DEMO-3"), "DEMO-3")
	rig.undo(t, failedUndo, "alice", "w-stuck", false)
	refused := rig.run(t, "alice", "start DEMO-4")
	write := statusWrite("w-refused", "DEMO-4")
	write.Outcome = goingWriteOutcome{Status: http.StatusConflict, Code: "CLAIM_HELD", Message: "held"}
	rig.write(t, refused, "alice", write, "DEMO-4")
	rig.run(t, "alice", "what is DEMO-5?")

	if got := goingRunIDs(rig.list(t, "alice", "?undoable=true")); !slices.Equal(got, []string{failedUndo, pending}) {
		t.Fatalf("alice's undoable runs = %v, want the one whose undo failed and the one never undone [%s %s]",
			got, failedUndo, pending)
	}
	if got := goingRunIDs(rig.list(t, "alice", "?undoable=true&limit=1")); !slices.Equal(got, []string{failedUndo}) {
		t.Fatalf("alice's newest undoable run = %v, want [%s]", got, failedUndo)
	}
	if got := rig.list(t, "alice", "?q=start%20demo"); len(got.Rows) != 4 || got.Total != 4 {
		t.Fatalf("alice's runs whose words hold 'start demo' = %v, want the four in any case", goingRunIDs(got))
	}
	if got := goingRunIDs(rig.list(t, "alice", "?q=DEMO-5")); len(got) != 1 {
		t.Fatalf("alice's runs whose words hold DEMO-5 = %v, want one", got)
	}
	if response := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/me/going-runs?undoable=yes", nil, "alice"); response.Code != http.StatusBadRequest {
		t.Fatalf("undoable=yes answers %d %s, want 400", response.Code, response.Body.String())
	}
	if err := rig.server.appendGoingRunUndo(context.Background(), pending, "alice", goingRunUndo{WriteID: "w-nobody-wrote"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an undo of a write the run does not hold: %v, want pgx.ErrNoRows", err)
	}
}

// A run older than thirty days is not read, even before the sweep deletes it; one inside them is.
func TestGoingRunsOlderThanThirtyDaysAreNotReturned(t *testing.T) {
	rig := newGoingRunsRig(t)
	old := rig.run(t, "alice", "a month and a day ago")
	rig.write(t, old, "alice", statusWrite("w-old", "DEMO-12"), "DEMO-12")
	rig.age(t, old, 31*24*time.Hour)
	kept := rig.run(t, "alice", "four weeks ago")
	rig.write(t, kept, "alice", statusWrite("w-kept", "DEMO-12"), "DEMO-12")
	rig.age(t, kept, 29*24*time.Hour)

	for _, query := range []string{"", "?issue=DEMO-12", "?undoable=true"} {
		if got := goingRunIDs(rig.list(t, "alice", query)); !slices.Equal(got, []string{kept}) {
			t.Fatalf("alice's runs%s = %v, want the 29-day run alone [%s]", query, got, kept)
		}
	}
	if _, err := rig.server.readGoingRun(context.Background(), old, "alice"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("reading the 31-day run: %v, want pgx.ErrNoRows", err)
	}
	if err := rig.server.appendGoingRunUndo(context.Background(), old, "alice", goingRunUndo{WriteID: "w-old"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("an undo appended to the 31-day run: %v, want pgx.ErrNoRows", err)
	}
}

// The maintenance pass deletes a run older than thirty days and keeps one inside them.
func TestGoingRunsSweepDeletesARunPastThirtyDays(t *testing.T) {
	rig := newGoingRunsRig(t)
	old := rig.run(t, "alice", "a month and a day ago")
	rig.age(t, old, 31*24*time.Hour)
	kept := rig.run(t, "alice", "four weeks ago")
	rig.age(t, kept, 29*24*time.Hour)

	stored := func(id string) bool {
		var count int
		if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from going_runs where id = $1::uuid`, id).Scan(&count); err != nil {
			t.Fatalf("count the run: %v", err)
		}
		return count == 1
	}
	leases := sweepLeases(t, rig.database)
	runSweep(t, func(ctx context.Context) {
		RunMaintenanceSweep(ctx, rig.database.Pool, maintenanceTestInterval, leases, nil)
	})
	waitUntil(t, 30*time.Second, "the 31-day run deleted by a maintenance pass", func() bool { return !stored(old) })
	if !stored(kept) {
		t.Fatal("the maintenance pass deleted the 29-day run, want it kept")
	}
}
