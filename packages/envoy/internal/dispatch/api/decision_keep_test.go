package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/decisionread"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A Keep acts on the decision's own node: it is two appends on the
// decision, in one write - a kept_by tuple to the person who kept it and a kept_text_sum value
// holding the decision's text sum at the Keep - and decision_keeps is their projection, written in
// the same transaction. The text sum covers the decision's own words and leaves the readings it
// closes with out. An agent's decision waits for review unless it is marked replaced or its
// kept_text_sum equals its current text sum; a Keep carrying a sum the decision no longer has is
// 409 DECISION_CHANGED and writes nothing.

// The agent's document the Keep test keeps a decision of: d1, which its reading g1 reads, and d2,
// which it marks replaced. It is no decisions.md: a Keep acts on a decision of any document of the
// issue.
const (
	keptWords    = "We ship REST first."
	keptReading  = "The agent reads it as REST for every service."
	keptDocument = "::::decision{#d1}\n" + keptWords + "\n\n:::reading{#g1}\n" + keptReading + "\n:::\n::::\n\n" +
		":::decision{#d2 replaced=\"true\"}\nWe shipped gRPC first.\n:::\n"
)

// keptRow is the decision_keeps row of one block, and the transaction that wrote it (xmin).
type keptRow struct {
	sum, keptBy, writtenBy string
	changedVersion         int
	keptAt                 time.Time
}

// same reports whether row is other, its time compared as an instant.
func (row keptRow) same(other keptRow) bool {
	return row.sum == other.sum && row.keptBy == other.keptBy && row.writtenBy == other.writtenBy &&
		row.changedVersion == other.changedVersion && row.keptAt.Equal(other.keptAt)
}

func TestAKeepIsTwoAppendsOnTheDecisionNodeThatDecisionKeepsProjects(t *testing.T) {
	handler, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	requireAgentWrite(t, "the agent's decisions", sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "choices.md", "content": keptDocument, "actor": sessionActor(),
	}), http.StatusCreated)
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")
	d1 := choices.address("d1")
	read := func(block string) model.Decision {
		t.Helper()
		document, err := s.loadArtifact(ctx, database.Pool, choices.id)
		if err != nil {
			t.Fatalf("load choices.md: %v", err)
		}
		decision, found, err := s.readDecisionNode(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, document, block)
		if err != nil || !found {
			t.Fatalf("read the decision %s: found %t, %v", block, found, err)
		}
		return decision
	}
	keep := func(login, sum string) model.DecisionKeep {
		t.Helper()
		kept := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+choices.id+"/blocks/d1/keep", map[string]any{"text_sha256": sum}, login)
		if kept.Code != http.StatusOK {
			t.Fatalf("%s keeps d1: status=%d body=%s", login, kept.Code, kept.Body.String())
		}
		return decodeBody[model.DecisionKeep](t, kept)
	}
	row := func() keptRow {
		t.Helper()
		var held keptRow
		if err := database.Pool.QueryRow(ctx, `
			select text_sha256, kept_by, xmin::text, changed_version, kept_at
			from decision_keeps where artifact_id = $1::uuid and block_id = 'd1'
		`, choices.id).Scan(&held.sum, &held.keptBy, &held.writtenBy, &held.changedVersion, &held.keptAt); err != nil {
			t.Fatalf("read the decision_keeps row of d1: %v", err)
		}
		return held
	}
	history := func(kind string) []graphstore.Tuple {
		t.Helper()
		held, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, d1, kind)
		if err != nil {
			t.Fatalf("read the %s history of d1: %v", kind, err)
		}
		return held
	}
	keptEvents := func() []int64 {
		t.Helper()
		rows, err := database.Pool.Query(ctx, `
			select id from events where type = 'decision.kept' and payload->>'artifact_id' = $1 order by id
		`, choices.id)
		if err != nil {
			t.Fatalf("read the decision.kept events: %v", err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan a decision.kept event: %v", err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	transactionOf := func(table string, key int64) string {
		t.Helper()
		var xmin string
		column := map[string]string{"graph_tuples": "seq", "events": "id"}[table]
		if err := database.Pool.QueryRow(ctx, `select xmin::text from `+table+` where `+column+` = $1`, key).Scan(&xmin); err != nil {
			t.Fatalf("read the transaction that wrote %s %d: %v", table, key, err)
		}
		return xmin
	}

	// The decision's words are its own: the reading g1 it closes with is none of them.
	before := read("d1")
	if want := ":::decision{#d1 replaced=\"false\"}\n" + keptWords + "\n:::\n"; before.Text != want || before.TextSHA256 != decisionread.TextSum(want) {
		t.Fatalf("d1 reads the words %q summed %s, want its own words %q and their sum %s, without the reading",
			before.Text, before.TextSHA256, want, decisionread.TextSum(want))
	}
	if !before.WaitingReview || before.Kept != nil || before.Replaced {
		t.Fatalf("the agent's d1 before any Keep = %+v, want it waiting for review, kept by nobody", before)
	}

	// alice keeps it: one kept_by tuple to her and one kept_text_sum value of its sum, under the
	// decision.kept the Keep appended, and the decision_keeps row of the same transaction.
	served := keep("alice", before.TextSHA256)
	events := keptEvents()
	if len(events) != 1 {
		t.Fatalf("the Keep appended %d decision.kept events, want one", len(events))
	}
	keptBy, keptSum := history(refs.TupleKeptBy), history(refs.ValueKeptTextSum)
	if len(keptBy) != 1 || !keptBy[0].Asserted || keptBy[0].ToKind != graphstore.KindPerson || keptBy[0].ToID != "alice" {
		t.Fatalf("d1 holds the kept_by tuples %+v, want one, to the person alice", keptBy)
	}
	if len(keptSum) != 1 || keptSum[0].ToKind != graphstore.KindValue || keptSum[0].ToID != before.TextSHA256 {
		t.Fatalf("d1 holds the kept_text_sum values %+v, want one, the decision's text sum %s", keptSum, before.TextSHA256)
	}
	for _, tuple := range []graphstore.Tuple{keptBy[0], keptSum[0]} {
		requireCaused(t, tuple.Kind+" of d1", tuple, "alice", events[0])
	}
	var changed int
	if err := database.Pool.QueryRow(ctx, `select changed_version from document_blocks where artifact_id = $1::uuid and block_id = 'd1'`, choices.id).Scan(&changed); err != nil {
		t.Fatalf("read d1's changed version: %v", err)
	}
	kept := row()
	if kept.sum != before.TextSHA256 || kept.keptBy != "alice" || !kept.keptAt.Equal(keptBy[0].At) || kept.changedVersion != changed {
		t.Fatalf("the decision_keeps row of d1 = %+v, want the projection of the two appends: sum %s, alice, at %s, at the version %d that last changed d1's text",
			kept, before.TextSHA256, keptBy[0].At, changed)
	}
	for what, xmin := range map[string]string{
		"the kept_by tuple": transactionOf("graph_tuples", keptBy[0].Seq), "the kept_text_sum value": transactionOf("graph_tuples", keptSum[0].Seq),
		"the decision.kept event": transactionOf("events", events[0]),
	} {
		if xmin != kept.writtenBy {
			t.Errorf("%s was written by transaction %s and the decision_keeps row by %s, want one transaction", what, xmin, kept.writtenBy)
		}
	}
	if served.BlockID != "d1" || served.KeptBy != "alice" || served.ChangedVersion != changed || !served.KeptAt.Equal(kept.keptAt) {
		t.Fatalf("the Keep answered %+v, want the decision_keeps row %+v", served, kept)
	}
	after := read("d1")
	if after.WaitingReview || after.Kept == nil || after.Kept.KeptBy != "alice" || after.Kept.TextSHA256 != before.TextSHA256 {
		t.Fatalf("d1 after alice's Keep = %+v (kept %+v), want kept by alice and waiting for no review", after, after.Kept)
	}

	// A reading's edit changes none of the decision's words: its sum and its Keep stand.
	agentEdit(t, handler, choices.id, latestVersion(t, database, choices.id).Number+1, replaceOp("for every service.", "for every public service."))
	if edited := read("d1"); edited.TextSHA256 != before.TextSHA256 || edited.WaitingReview {
		t.Fatalf("d1 after its reading's edit = %+v, want its sum %s and no review waiting", edited, before.TextSHA256)
	}

	// The reading's edit moved d1's row, whose text holds the reading, and none of d1's words: a
	// Keep after it records the version the words last changed in, the one alice kept it at, on
	// the row and in its decision.kept, and not the version the reading's edit wrote.
	var moved int
	if err := database.Pool.QueryRow(ctx, `select changed_version from document_blocks where artifact_id = $1::uuid and block_id = 'd1'`, choices.id).Scan(&moved); err != nil {
		t.Fatalf("read d1's row after its reading's edit: %v", err)
	}
	if moved == changed {
		t.Fatalf("the reading's edit left d1's row at version %d; this case needs it moved", changed)
	}
	keep("alice", before.TextSHA256)
	if again := row(); again.changedVersion != changed {
		t.Fatalf("the Keep after the reading's edit recorded the version %d on the row, want %d, the version d1's words last changed in, not %d, which moved only its reading",
			again.changedVersion, changed, moved)
	}
	var payloadVersion int
	if err := database.Pool.QueryRow(ctx, `
		select (payload->>'changed_version')::int from events
		where type = 'decision.kept' and payload->>'artifact_id' = $1 order by id desc limit 1
	`, choices.id).Scan(&payloadVersion); err != nil || payloadVersion != changed {
		t.Fatalf("the Keep after the reading's edit recorded the version %d in its decision.kept (%v), want %d", payloadVersion, err, changed)
	}
	kept = row()

	// The decision's own words change: it waits for review again, and a Keep of the old sum is
	// 409 DECISION_CHANGED, writing nothing.
	agentEdit(t, handler, choices.id, latestVersion(t, database, choices.id).Number+1, replaceOp(keptWords, "We ship gRPC first."))
	reworded := read("d1")
	if !reworded.WaitingReview || reworded.TextSHA256 == before.TextSHA256 {
		t.Fatalf("d1 after its words changed = %+v, want a new sum and waiting for review again", reworded)
	}
	keptByHeld, keptSumHeld, keptEventsHeld := len(history(refs.TupleKeptBy)), len(history(refs.ValueKeptTextSum)), len(keptEvents())
	expectRefusal(t, "a Keep of the sum d1 had before its words changed", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+choices.id+"/blocks/d1/keep", map[string]any{"text_sha256": before.TextSHA256}, "alice"),
		http.StatusConflict, "DECISION_CHANGED")
	if len(history(refs.TupleKeptBy)) != keptByHeld || len(history(refs.ValueKeptTextSum)) != keptSumHeld || len(keptEvents()) != keptEventsHeld || !row().same(kept) {
		t.Fatalf("the refused Keep wrote something: kept_by %+v, kept_text_sum %+v, %d decision.kept events, row %+v (was %+v)",
			history(refs.TupleKeptBy), history(refs.ValueKeptTextSum), len(keptEvents()), row(), kept)
	}

	// A credential that holds no person scope keeps nothing: the route is the person scope's.
	expectRefusal(t, "a session's Keep", sessionRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+choices.id+"/blocks/d1/keep", map[string]any{"text_sha256": reworded.TextSHA256}),
		http.StatusForbidden, codeScopeRequired)

	// bob keeps the decision as it reads now: the node holds one kept_by, to bob, alice's retracted.
	keep("bob", reworded.TextSHA256)
	current, err := graphstore.CurrentOut(ctx, database.Pool, refs.BlockNodeKind, d1, []string{refs.TupleKeptBy})
	if err != nil {
		t.Fatalf("read d1's current kept_by: %v", err)
	}
	if len(current) != 1 || current[0].ToID != "bob" {
		t.Fatalf("d1 holds the current kept_by %+v after bob's Keep, want one, to bob", current)
	}
	if again := read("d1"); again.WaitingReview || again.Kept == nil || again.Kept.KeptBy != "bob" || row().keptBy != "bob" {
		t.Fatalf("d1 after bob's Keep = %+v (kept %+v, row %+v), want kept by bob and waiting for no review", again, again.Kept, row())
	}

	// A decision marked replaced waits for no review, and Keep does not act on it.
	replaced := read("d2")
	if !replaced.Replaced || replaced.WaitingReview || replaced.Kept != nil {
		t.Fatalf("the replaced d2 = %+v, want replaced and waiting for no review", replaced)
	}
	expectRefusal(t, "a Keep of the replaced d2", dispatchRequest(t, handler, http.MethodPost,
		"/api/v1/artifacts/"+choices.id+"/blocks/d2/keep", map[string]any{"text_sha256": replaced.TextSHA256}, "alice"),
		http.StatusNotFound, "TARGET_NOT_FOUND")
}

// holdAgentVersionEvents makes every artifact.version event the agent of sessionActor appends wait
// on the advisory key 1150019, which the test's own transaction takes: the agent's write of a
// document of an issue holds the issue's owner lock from before its event to its commit, so it
// stops there holding that lock.
const holdAgentVersionEvents = `
	create function hold_agent_version_event() returns trigger language plpgsql as $$
	begin
		perform pg_advisory_xact_lock(tg_argv[0]::bigint);
		return new;
	end $$;
	create trigger hold_agent_version_event before insert on events for each row
		when (new.type = 'artifact.version' and new.actor->>'id' = '` + decidingSession + `')
		execute function hold_agent_version_event('1150019');`

// A Keep reads the decision on the pool, holding no lock, compares the posted sum with it, and only
// then takes the issue's owner lock, which an agent's write of the decision's document takes too.
// A write that commits while the Keep waits for that lock changes what the Keep would record, so
// the Keep reads the decision again under it: an edit of its words is 409 DECISION_CHANGED, and a
// version that marks it replaced is 404 TARGET_NOT_FOUND, each with no kept_by, no kept_text_sum,
// no decision_keeps row and no decision.kept written.
//
// The agent's write stops at its artifact.version, holding the issue's owner lock, behind a
// transaction of the test's own (holdAgentVersionEvents). The Keep, posted with the sum the
// decision read before the write, queues behind the write, and the test's transaction then lets
// the write go on and commit.
func TestAKeepThatWaitsBehindAWriteOfItsDecisionReadsItAgain(t *testing.T) {
	handler, database, _, s := newRegisteredTestServer(t, testServerOptions{})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "A spec.\n")
	owner := "/api/v1/issues/" + issue.Key + "/artifacts"
	requireAgentWrite(t, "the agent's decisions", sessionRequest(t, handler, http.MethodPost, owner, map[string]any{
		"name": "choices.md", "content": keptDocument, "actor": sessionActor(),
	}), http.StatusCreated)
	choices := readDecisionDocument(t, database, issue.Key, "choices-md")
	d1 := choices.address("d1")
	read := func() model.Decision {
		t.Helper()
		document, err := s.loadArtifact(ctx, database.Pool, choices.id)
		if err != nil {
			t.Fatalf("load choices.md: %v", err)
		}
		decision, found, err := s.readDecisionNode(ctx, database.Pool, model.Actor{Kind: "user", ID: "alice"}, document, "d1")
		if err != nil || !found {
			t.Fatalf("read the decision d1: found %t, %v", found, err)
		}
		return decision
	}
	if _, err := database.Pool.Exec(ctx, holdAgentVersionEvents); err != nil {
		t.Fatalf("install the trigger that holds the agent's versions: %v", err)
	}

	// Each write is its route and body, built before the write starts, so the goroutine that sends
	// it reads nothing of its own.
	for _, race := range []struct {
		what    string
		request func() (string, map[string]any)
		status  int
		code    string
		want    int
	}{
		{
			what: "an edit of its words",
			request: func() (string, map[string]any) {
				return "/api/v1/artifacts/" + choices.id + "/edits", map[string]any{
					"actor": sessionActor(), "ops": []map[string]string{replaceOp(keptWords, "We ship gRPC first.")},
				}
			},
			status: http.StatusConflict, code: "DECISION_CHANGED", want: http.StatusOK,
		},
		{
			what: "a version that marks it replaced",
			request: func() (string, map[string]any) {
				return owner, map[string]any{
					"name": "choices.md", "actor": sessionActor(), "precondition": uploadBaseNamed(t, handler, owner, "choices.md"),
					"content": "::::decision{#d1 replaced=\"true\"}\nWe ship gRPC first.\n\n:::reading{#g1}\n" + keptReading + "\n:::\n::::\n\n" +
						":::decision{#d2 replaced=\"true\"}\nWe shipped gRPC first.\n:::\n",
				}
			},
			status: http.StatusNotFound, code: "TARGET_NOT_FOUND", want: http.StatusCreated,
		},
	} {
		before := read()
		if before.Replaced {
			t.Fatalf("before %s d1 reads replaced; this case needs a decision a Keep acts on", race.what)
		}
		path, body := race.request()
		holder, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction that holds %s: %v", race.what, err)
		}
		t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
		if _, err := holder.Exec(ctx, `select pg_advisory_xact_lock(1150019)`); err != nil {
			t.Fatalf("take the key %s waits on: %v", race.what, err)
		}
		written := make(chan *httptest.ResponseRecorder, 1)
		go func() { written <- sessionRequest(t, handler, http.MethodPost, path, body) }()
		awaitLockWaiters(t, database, 1)
		waiting := lockWaiters(t, database)
		kept := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			kept <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+choices.id+"/blocks/d1/keep",
				map[string]any{"text_sha256": before.TextSHA256}, "alice")
		}()
		awaitLockWaiters(t, database, waiting+1)
		if err := holder.Commit(ctx); err != nil {
			t.Fatalf("let %s go on: %v", race.what, err)
		}
		var write, keep *httptest.ResponseRecorder
		select {
		case write = <-written:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s answered nothing 30s after it was let go", race.what)
		}
		requireAgentWrite(t, race.what, write, race.want)
		select {
		case keep = <-kept:
		case <-time.After(30 * time.Second):
			t.Fatalf("the Keep that waited behind %s answered nothing 30s after the write committed", race.what)
		}
		if keep.Code != race.status || !strings.Contains(keep.Body.String(), race.code) {
			t.Fatalf("the Keep that waited behind %s answers status=%d body=%s, want %d %s", race.what, keep.Code, keep.Body.String(), race.status, race.code)
		}
		for _, kind := range []string{refs.TupleKeptBy, refs.ValueKeptTextSum} {
			if held, err := graphstore.History(ctx, database.Pool, refs.BlockNodeKind, d1, kind); err != nil || len(held) != 0 {
				t.Fatalf("after the Keep that waited behind %s d1 holds the %s tuples %+v (%v), want none", race.what, kind, held, err)
			}
		}
		var rows, events int
		if err := database.Pool.QueryRow(ctx, `
			select (select count(*) from decision_keeps where artifact_id = $1::text::uuid),
			       (select count(*) from events where type = 'decision.kept' and payload->>'artifact_id' = $1::text)
		`, choices.id).Scan(&rows, &events); err != nil || rows != 0 || events != 0 {
			t.Fatalf("after the Keep that waited behind %s choices.md holds %d decision_keeps rows and %d decision.kept events (%v), want none",
				race.what, rows, events, err)
		}
	}
}
