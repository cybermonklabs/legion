package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// owedIndexSpec is fenceSpec with a question block, q1, after its two requirements; owedIndexChecks
// is the Verification document of the issue key, whose one check serves the first requirement alone
// and whose words write `#q1`, the id of the spec's question block: a bare `#id` candidate
// (refs.HoldsQuestionCandidate), so the scope drain of the block-index tick (docs' reindexScope)
// owes the document a block index for each scope row of it the re-index queue holds.
const owedIndexSpec = fenceSpec + "\n:::ask{#q1}\nWhich store do the writers take?\n:::\n"

func owedIndexChecks(key string) string {
	return fmt.Sprintf(":::check{#c1 serves=\"dispatch://%s/spec#b-r1\" surface=\"cli\"}\nThe first requirement holds, as #q1 asks of it.\n:::\n", key)
}

// owedDocumentNames is each document the coverage read lists as owing its block index, by its name,
// its id and the version whose blocks wait, as the read's own gap line names it (owedGap).
func owedDocumentNames(t *testing.T, database *store.Store, owed []model.OwedDocument) []string {
	t.Helper()
	names := make([]string, 0, len(owed))
	for _, document := range owed {
		var name string
		if err := database.Pool.QueryRow(context.Background(), `select name from artifacts where id = $1::uuid`, document.ArtifactID).Scan(&name); err != nil {
			t.Fatalf("read the name of the owed document %s: %v", document.ArtifactID, err)
		}
		names = append(names, fmt.Sprintf("%s (%s) version %d", name, document.ArtifactID, document.Version))
	}
	return names
}

// queuedRowsOf is each row of refs_grammar_owed that names a document of the issue issueKey, its
// tree artifact and its decisions query among them, as its kind, the document's name and its id.
func queuedRowsOf(t *testing.T, database *store.Store, issueKey string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select o.kind || ' ' || a.name || ' (' || a.id::text || ')'
		from refs_grammar_owed o
		join artifacts a on a.id = o.id
		where a.issue_key = $1
		order by a.name, o.kind`, issueKey)
	if err != nil {
		t.Fatalf("read the re-index queue of the documents of %s: %v", issueKey, err)
	}
	queued, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the re-index queue of the documents of %s: %v", issueKey, err)
	}
	return queued
}

// formsQueues are the forms re-index queues a document's text can be owed in: refs_forms_owed, and
// the two halves of the Decisions page's queue, the live one the relay of a delete from
// refs_forms_owed writes (refs_decisions_forms_owed) and the bootstrap one the seed writes
// (refs_decisions_forms_seed_owed).
var formsQueues = []refs.OwedQueue{refs.RefsFormsOwed, refs.RefsDecisionsFormsOwed, refs.RefsDecisionsFormsSeedOwed}

// formsQueuedRowsOf is each row of the forms queue queue that names a document of the issue
// issueKey, its tree artifact and its decisions query among them, as its kind, the document's name
// and its id.
func formsQueuedRowsOf(t *testing.T, database *store.Store, queue refs.OwedQueue, issueKey string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select o.kind || ' ' || a.name || ' (' || a.id::text || ')'
		from `+queue.Table()+` o
		join artifacts a on a.id = o.id
		where a.issue_key = $1
		order by a.name, o.kind`, issueKey)
	if err != nil {
		t.Fatalf("read %s of the documents of %s: %v", queue.Table(), issueKey, err)
	}
	queued, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s of the documents of %s: %v", queue.Table(), issueKey, err)
	}
	return queued
}

// queuedKindsOf is the kind of each row of the re-index queue that names the document artifactID,
// in kind order: `artifact` for its own text, `scope` for its scope.
func queuedKindsOf(t *testing.T, database *store.Store, artifactID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select kind from refs_grammar_owed where id = $1::uuid order by kind`, artifactID)
	if err != nil {
		t.Fatalf("read the re-index queue of %s: %v", artifactID, err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the re-index queue of %s: %v", artifactID, err)
	}
	return kinds
}

// settleReindexQueue runs passes of the block-index reconciliation until no document of the issue
// issueKey owes its block index and the re-index queue holds no row of any of them, so a single
// tick the test runs later reads only what the writes after this left.
func settleReindexQueue(t *testing.T, handler http.Handler, database *store.Store, deps Deps, issueKey string) {
	t.Helper()
	for range 8 {
		if len(readCoverage(t, handler, issueKey).OwedDocuments) == 0 && len(queuedRowsOf(t, database, issueKey)) == 0 {
			return
		}
		reconcileBlockIndexPass(t, deps)
	}
	t.Fatalf("eight passes of the reconciliation left %s owing %+v, with the re-index queue holding %q",
		issueKey, readCoverage(t, handler, issueKey).OwedDocuments, queuedRowsOf(t, database, issueKey))
}

// TestARecordedOrRetractedResultLeavesNoDocumentOwedItsBlockIndex: a result an agent session
// records on a converted issue, and its retraction by that session, leave no document of the issue
// owing its block index, neither at once nor after the next tick of the block-index
// reconciliation, so the coverage read still draws the requirement no check serves: `r2 has no
// check`. The issue's spec holds two requirements and a question block, q1, and its Verification
// document one check, which serves the first and whose words write `#q1`.
//
// Once its settlements and their drains have run, and the reconciliation has left no document owed
// and no row of the re-index queue (refs_grammar_owed) naming one, an agent session opens an ask on
// the issue, as the issue page's seeded planner does (e2e/issue-layout-seed-support.ts), which
// moves the answer of the issue's decisions query and writes no version of it. The result's drain
// (docs.Service.DrainOwedQueries) then writes a version of the Verification document holding the
// bytes of the one before, and a version of the decisions query holding the ask's question, whose
// bytes moved; the retraction's drain writes a version of the Verification document again. Each
// insert owes its document its block index (migration 0122's trigger) and two rows of the re-index
// queue, its own text's and its scope's (migration 20261103205041's trigger). The drain stands each
// version's index (docs' queueRevisionStand) and deletes the two rows the insert made
// (docs' restoreReindexQueue). After each, the test reads the coverage at once, then runs one tick
// as the server's loop runs it and reads again: the tick indexed no document, left none owed, and
// the queue holds no row of the issue's documents.
//
// Without the stand, the read made at once names the decisions query owed. Without the delete, the
// queue read made at once holds the artifact and scope rows of verification.md and of the decisions
// query, and the tick's text pass drains them by owing both documents a block index
// (docs' drainOwedDocument, and reindexScope for verification.md, whose `#q1` is a candidate): the
// tick then reports OwedDocuments 2, and the coverage read after it names both documents owed and
// draws no `r2 has no check`, until the tick after that indexes them.
//
// Last, a person's edit of the check leaves verification.md's scope row standing, as every indexed
// version write does, and a result recorded after it writes a version holding the edit's bytes:
// the drain deletes the artifact row its insert made and leaves the scope row, which stood before
// it and is still owed. Without the delete the artifact row stands too; a delete of both rows would
// take the scope row.
func TestARecordedOrRetractedResultLeavesNoDocumentOwedItsBlockIndex(t *testing.T) {
	t.Parallel()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createReferenceAPIProject(t, handler, "OWED")
	issue := createDigestIssue(t, handler, database, "OWED", "A result on a converted issue", owedIndexSpec, false)
	checks := owedIndexChecks(issue.Key)
	if !refs.HoldsQuestionCandidate(checks, testServerURL) {
		t.Fatalf("the Verification document %q writes no `#id` candidate, so its scope row owes it nothing and this case proves nothing", checks)
	}
	uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md", checks)
	verification := readDecisionDocument(t, database, issue.Key, "verification-md")
	decisions, _ := decisionsQueryVersions(t, database, issue.Key)
	if decisions == "" {
		t.Fatalf("%s holds no decisions query node: the issue is not born converted, so this case proves nothing", issue.Key)
	}
	documentsAt := func() string {
		return fmt.Sprintf("verification.md at version %d, the decisions query %s at version %d",
			newestQueryVersion(t, database, verification.id).number, decisions, newestQueryVersion(t, database, decisions).number)
	}
	requireDrawn := func(what string) {
		t.Helper()
		coverage := readCoverage(t, handler, issue.Key)
		if owed := owedDocumentNames(t, database, coverage.OwedDocuments); len(owed) > 0 {
			t.Errorf("%s: owed_documents names %s, with the gaps %q (%s), want no document owed its block index",
				what, strings.Join(owed, ", "), coverage.Gaps, documentsAt())
		}
		if gaps := requirementOf(t, coverage, "r2").Gaps; !slices.Contains(gaps, noCheckReason) || !slices.Contains(coverage.Gaps, noCheckGap("r2")) {
			t.Errorf("%s: the coverage read gives r2 the gaps %q and the issue the gaps %q (%s), want %q among them",
				what, gaps, coverage.Gaps, documentsAt(), noCheckGap("r2"))
		}
	}
	requireNothingQueued := func(what string) {
		t.Helper()
		if queued := queuedRowsOf(t, database, issue.Key); len(queued) > 0 {
			t.Errorf("%s: the re-index queue holds %q (%s), want no row of a document of %s", what, queued, documentsAt(), issue.Key)
		}
		for _, queue := range formsQueues {
			if queued := formsQueuedRowsOf(t, database, queue, issue.Key); len(queued) > 0 {
				t.Errorf("%s: %s holds %q (%s), want no row of a document of %s", what, queue.Table(), queued, documentsAt(), issue.Key)
			}
		}
	}
	// oneTick runs the block-index reconciliation once, as the server's loop runs a tick, and reads
	// what it did and left.
	oneTick := func(what string) {
		t.Helper()
		tick := reconcileBlockIndexPass(t, deps)
		if tick.Documents != 0 || tick.OwedDocuments != 0 {
			t.Errorf("%s: the next tick indexed %d documents and left %d owed (%s), want none of either: the drain left a document no index work",
				what, tick.Documents, tick.OwedDocuments, documentsAt())
		}
		requireNothingQueued(what + ", after one tick")
		requireDrawn(what + ", after one tick")
	}

	renderAtVersionQuiet(t, database, issue.Key, issue.PrimaryArtifactID)
	settleReindexQueue(t, handler, database, deps, issue.Key)
	requireDrawn("the upload, once settled and indexed")

	const question = "Which list do the readers open first?"
	unasked := newestQueryVersion(t, database, decisions)
	openAskAs(t, handler, issue.Key, "ses-planner", question)
	renderAtVersionQuiet(t, database, issue.Key, issue.PrimaryArtifactID)
	requireDrawn("the ask")
	requireNothingQueued("the ask")

	before := newestQueryVersion(t, database, verification.id)
	result := recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
	if held := newestQueryVersion(t, database, verification.id); held.number != before.number+1 || held.markdown != before.markdown {
		t.Errorf("the result leaves verification.md at version %d holding %q, want the drained version %d holding the bytes of the one before, %q",
			held.number, held.markdown, before.number+1, before.markdown)
	}
	if held := newestQueryVersion(t, database, decisions); held.number <= unasked.number || !strings.Contains(held.markdown, question) {
		t.Errorf("after the result the decisions query %s stands at version %d holding %q, want a version past %d holding the ask's question %q: "+
			"the case this test reads is a drained version of the decisions query whose bytes moved",
			decisions, held.number, held.markdown, unasked.number, question)
	}
	requireDrawn("a result an agent session recorded")
	requireNothingQueued("a result an agent session recorded")
	oneTick("a result an agent session recorded")

	before = newestQueryVersion(t, database, verification.id)
	if retracted := retractAsSession(t, handler, result.ID, "ses-recorder", "The run measured the wrong build."); retracted.Code != http.StatusCreated {
		t.Fatalf("the recording session retracts the result %s: status=%d body=%s", result.ID, retracted.Code, retracted.Body.String())
	}
	if held := newestQueryVersion(t, database, verification.id); held.number != before.number+1 || held.markdown != before.markdown {
		t.Errorf("the retraction leaves verification.md at version %d holding %q, want the drained version %d holding the bytes of the one before, %q",
			held.number, held.markdown, before.number+1, before.markdown)
	}
	requireDrawn("its retraction by the session that recorded it")
	requireNothingQueued("its retraction by the session that recorded it")
	oneTick("its retraction by the session that recorded it")

	// A scope row that stood before the drain's insert stays: a person's edit of the check, which
	// versions verification.md and indexes it in its own transaction, settles its artifact row and
	// leaves its scope row for the sources around it.
	edited := newestQueryVersion(t, database, verification.id).number + 1
	edit := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+verification.id+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "The first requirement holds", "with": "The first requirement still holds"}},
	}, "alice")
	if edit.Code != http.StatusOK {
		t.Fatalf("alice edits the check of verification.md: status=%d body=%s", edit.Code, edit.Body.String())
	}
	waitForArtifactVersion(t, handler, verification.id, edited)
	renderAtVersionQuiet(t, database, issue.Key, issue.PrimaryArtifactID)
	requireDrawn("the edit of the check")
	if kinds := queuedKindsOf(t, database, verification.id); !slices.Equal(kinds, []string{"scope"}) {
		t.Fatalf("the edit of the check leaves the re-index queue holding %q of verification.md (%s), want its scope row alone: "+
			"the case this step reads is a scope row that stood before the drain's insert", kinds, documentsAt())
	}
	before = newestQueryVersion(t, database, verification.id)
	recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("c1", "fail", headCommit, map[string]any{"failed": 1}))
	if held := newestQueryVersion(t, database, verification.id); held.number != before.number+1 || held.markdown != before.markdown {
		t.Errorf("the result after the edit leaves verification.md at version %d holding %q, want the drained version %d holding the bytes of the one before, %q",
			held.number, held.markdown, before.number+1, before.markdown)
	}
	if kinds := queuedKindsOf(t, database, verification.id); !slices.Equal(kinds, []string{"scope"}) {
		t.Errorf("the drain of a result after the edit leaves the re-index queue holding %q of verification.md (%s), want the scope row that stood before its insert, and no artifact row",
			kinds, documentsAt())
	}
	for _, queue := range formsQueues {
		if forms := formsRowsOf(t, database, queue, verification.id); len(forms) > 0 {
			t.Errorf("the drain of a result after the edit leaves %s holding %q of verification.md (%s), want none",
				queue.Table(), forms, documentsAt())
		}
	}
	requireDrawn("a result recorded over a scope row that stood")

	// A refs_forms_owed row that already stood before the result (as an older build's write
	// leaves it) stays standing across a result's drain, and since the drain deletes none of it,
	// the relay of a delete from refs_forms_owed owes neither half of the Decisions page's queue
	// verification.md.
	if _, err := database.Pool.Exec(context.Background(), `
		insert into refs_forms_owed (kind, id) values ('artifact', $1::uuid)
		on conflict (kind, id) do update set enqueued_at = now()
	`, verification.id); err != nil {
		t.Fatalf("plant a standing refs_forms_owed row for verification.md: %v", err)
	}
	before = newestQueryVersion(t, database, verification.id)
	recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("c1", "pass", headCommit, map[string]any{"passed": 1}))
	if held := newestQueryVersion(t, database, verification.id); held.number != before.number+1 || held.markdown != before.markdown {
		t.Errorf("the result over a standing forms row leaves verification.md at version %d holding %q, want the drained version %d holding the bytes of the one before, %q",
			held.number, held.markdown, before.number+1, before.markdown)
	}
	if forms := formsRowsOf(t, database, refs.RefsFormsOwed, verification.id); !slices.Equal(forms, []string{"artifact"}) {
		t.Errorf("the drain of a result over a standing forms row leaves refs_forms_owed holding %q of verification.md (%s), want the artifact row that stood before its insert",
			forms, documentsAt())
	}
	for _, queue := range []refs.OwedQueue{refs.RefsDecisionsFormsOwed, refs.RefsDecisionsFormsSeedOwed} {
		if forms := formsRowsOf(t, database, queue, verification.id); len(forms) > 0 {
			t.Errorf("the drain of a result over a standing forms row leaves %s holding %q of verification.md (%s), want none",
				queue.Table(), forms, documentsAt())
		}
	}
}

// formsRowsOf is the kind of each row of the forms queue queue that names the document artifactID,
// in kind order.
func formsRowsOf(t *testing.T, database *store.Store, queue refs.OwedQueue, artifactID string) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(),
		`select kind from `+queue.Table()+` where id = $1::uuid order by kind`, artifactID)
	if err != nil {
		t.Fatalf("read %s of %s: %v", queue.Table(), artifactID, err)
	}
	kinds, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read %s of %s: %v", queue.Table(), artifactID, err)
	}
	return kinds
}

// TestTheDecisionsQueryOwesNoReindexQueueOnCreateOrConversion: an issue's decisions query node
// holds no fragment of a room and no block (R15), so neither its version 1 (at issue create or at
// issue conversion) nor a version the drain writes leaves any row naming it in refs_grammar_owed
// or in a forms queue (formsQueues: refs_forms_owed and both halves of the Decisions page's
// queue), and no tick owes it a block index even when its stored rendering writes a bare `#id`
// candidate (a decision referencing `#q1`).
//
// Subcase 1 (born converted): an issue created with a spec holding a decision that writes `#q1`
// renders that decision into version 1 of its decisions query (api's queueDecisionsFirstVersion).
// Right after the create, and after one tick of the block-index reconciliation, neither queue
// holds a row of the decisions query and the coverage read names no document owed. A second
// version written by the drain over a pre-existing scope row in refs_grammar_owed (as an earlier
// database could hold) clears both rows of refs_grammar_owed and refs_forms_owed anyway
// (docs' restoreReindexQueue), leaving nothing queued and no document owed after the next tick.
//
// Subcase 2 (converted from an existing issue): an issue created unconverted with the same spec
// and converted through docs.Service.ConvertIssue (docs' writeDecisionsQuery) likewise leaves no
// row of its decisions query in either queue and no document owed, right after the conversion and
// after one tick.
func TestTheDecisionsQueryOwesNoReindexQueueOnCreateOrConversion(t *testing.T) {
	t.Parallel()
	const decisionSpec = fenceSpec + "\n:::decision{#d1}\nFollow #q1 for the store choice.\n:::\n"
	if !refs.HoldsQuestionCandidate(decisionSpec, testServerURL) {
		t.Fatalf("the spec %q writes no `#id` candidate, so a standing scope row would owe nothing and this case proves nothing", decisionSpec)
	}

	requireDecisionsClean := func(t *testing.T, handler http.Handler, database *store.Store, what, issueKey, decisions string) {
		t.Helper()
		if kinds := queuedKindsOf(t, database, decisions); len(kinds) > 0 {
			t.Errorf("%s: refs_grammar_owed holds %q for the decisions query %s of %s, want none", what, kinds, decisions, issueKey)
		}
		for _, queue := range formsQueues {
			if kinds := formsRowsOf(t, database, queue, decisions); len(kinds) > 0 {
				t.Errorf("%s: %s holds %q for the decisions query %s of %s, want none", what, queue.Table(), kinds, decisions, issueKey)
			}
		}
		if owed := owedDocumentNames(t, database, readCoverage(t, handler, issueKey).OwedDocuments); len(owed) > 0 {
			t.Errorf("%s: owed_documents of %s names %s, want no document owed its block index", what, issueKey, strings.Join(owed, ", "))
		}
	}

	t.Run("born converted", func(t *testing.T) {
		t.Parallel()
		handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
		documents := deps.Docs.(*docs.Service)
		createReferenceAPIProject(t, handler, "BORN")
		issue := createDigestIssue(t, handler, database, "BORN", "A born-converted issue with a #id decision", decisionSpec, false)
		decisions, newest := decisionsQueryVersions(t, database, issue.Key)
		if decisions == "" || newest != 1 {
			t.Fatalf("%s holds decisions query %q at version %d, want version 1 right after create", issue.Key, decisions, newest)
		}
		if v1 := newestQueryVersion(t, database, decisions); !refs.HoldsQuestionCandidate(v1.markdown, testServerURL) {
			t.Fatalf("version 1 of the decisions query %s stores %q, want a bare `#id` candidate so a left scope row would owe its block index", decisions, v1.markdown)
		}
		// The spec's own text also writes `#q1`, so its own scope row owes the spec an index at the
		// first tick; clear only the spec's scope row after checking the decisions query itself so
		// the tick below measures the decisions query alone.
		requireDecisionsClean(t, handler, database, "right after create", issue.Key, decisions)
		if _, err := database.Pool.Exec(context.Background(),
			`delete from refs_grammar_owed where kind = 'scope' and id = $1::uuid`, issue.PrimaryArtifactID); err != nil {
			t.Fatalf("clear the spec's own scope row: %v", err)
		}
		reconcileBlockIndexPass(t, deps)
		requireDecisionsClean(t, handler, database, "after create and one tick", issue.Key, decisions)

		// Even if a scope row of the decisions query stood before a drain (from a database written
		// before this fix), the drain deletes every row of the decisions query from both queues.
		if _, err := database.Pool.Exec(context.Background(),
			`insert into refs_grammar_owed (kind, id) values ('scope', $1::uuid) on conflict (kind, id) do update set enqueued_at = now()`,
			decisions); err != nil {
			t.Fatalf("plant a standing scope row of the decisions query: %v", err)
		}
		const question = "Which store holds the decisions?"
		openAskAs(t, handler, issue.Key, "ses-planner", question)
		uploadDocument(t, handler, "/api/v1/issues/"+issue.Key, "verification.md",
			fmt.Sprintf(":::check{#c1 serves=\"dispatch://%s/spec#b-r1\" surface=\"cli\"}\nCheck one.\n:::\n", issue.Key))
		settleIssue(t, documents, issue.Key)
		recordResultAsSession(t, handler, issue.Key, "ses-recorder", resultBody("c1", "pass", headCommit, map[string]any{"passed": 1}))
		if v := newestQueryVersion(t, database, decisions); v.number <= 1 || !strings.Contains(v.markdown, question) || !refs.HoldsQuestionCandidate(v.markdown, testServerURL) {
			t.Fatalf("after the drain the decisions query %s stands at version %d holding %q, want a drained version past 1 holding both %q and `#q1`",
				decisions, v.number, v.markdown, question)
		}
		requireDecisionsClean(t, handler, database, "right after drain over a standing scope row", issue.Key, decisions)
		reconcileBlockIndexPass(t, deps)
		requireDecisionsClean(t, handler, database, "after drain and one tick", issue.Key, decisions)
	})

	t.Run("converted existing issue", func(t *testing.T) {
		t.Parallel()
		handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
		documents := deps.Docs.(*docs.Service)
		createReferenceAPIProject(t, handler, "CONV")
		issue := createDigestIssue(t, handler, database, "CONV", "An existing issue with a #id decision", decisionSpec, true)
		settleIssue(t, documents, issue.Key)
		settleReindexQueue(t, handler, database, deps, issue.Key)
		converted, err := documents.ConvertIssue(context.Background(), issue.Key, docs.IssueRoomGate, false)
		if err != nil || converted.Outcome != docs.IssueConverted {
			t.Fatalf("convert %s: %+v, %v; want it converted", issue.Key, converted, err)
		}
		decisions, newest := decisionsQueryVersions(t, database, issue.Key)
		if decisions == "" || newest < 1 {
			t.Fatalf("converted %s holds decisions query %q at version %d, want at least version 1", issue.Key, decisions, newest)
		}
		requireDecisionsClean(t, handler, database, "right after conversion", issue.Key, decisions)
		// Clear the spec's own scope row that the conversion's version of spec.md left, so the
		// tick below measures only what the decisions query left.
		if _, err := database.Pool.Exec(context.Background(),
			`delete from refs_grammar_owed where kind = 'scope' and id = $1::uuid`, issue.PrimaryArtifactID); err != nil {
			t.Fatalf("clear the converted spec's own scope row: %v", err)
		}
		reconcileBlockIndexPass(t, deps)
		requireDecisionsClean(t, handler, database, "after conversion and one tick", issue.Key, decisions)
	})
}
