package docs

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// decisionsFormsOwedRows is every row of the live decisions forms queue as "kind id", with the
// enqueued_at each holds, and decisionsFormsSeedOwedRows the same for its bootstrap half.
func decisionsFormsOwedRows(t *testing.T, database *store.Store) map[string]time.Time {
	t.Helper()
	return owedRowsOf(t, database, "refs_decisions_forms_owed")
}

func decisionsFormsSeedOwedRows(t *testing.T, database *store.Store) map[string]time.Time {
	t.Helper()
	return owedRowsOf(t, database, "refs_decisions_forms_seed_owed")
}

// owedRowsOf is every row of queue, a refs_*_owed table, as "kind id", with the enqueued_at each
// holds.
func owedRowsOf(t *testing.T, database *store.Store, queue string) map[string]time.Time {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select kind || ' ' || id::text, enqueued_at from `+queue)
	if err != nil {
		t.Fatalf("read %s: %v", queue, err)
	}
	defer rows.Close()
	owed := map[string]time.Time{}
	for rows.Next() {
		var row string
		var at time.Time
		if err := rows.Scan(&row, &at); err != nil {
			t.Fatalf("scan %s: %v", queue, err)
		}
		owed[row] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s: %v", queue, err)
	}
	return owed
}

// reconcileDecisionsForms runs one pass of the decisions queue's drain from the start of the queue,
// as a loop that has found no document live ahead before runs its first.
func reconcileDecisionsForms(t *testing.T, service *Service) RefsFormsTick {
	t.Helper()
	tick, err := service.ReconcileRefsForms(context.Background(), refs.RefsDecisionsFormsOwed, refs.Owed{}, newFormsLiveAhead())
	if err != nil {
		t.Fatalf("decisions forms drain pass: %v", err)
	}
	return tick
}

// settleTheFormsQueueAsTheReleaseBeforeDoes settles every row of the forms queue with the one
// statement the release before this one settles a row with once it has indexed the text itself,
// and which this release's trigger relays to the decisions queue.
func settleTheFormsQueueAsTheReleaseBeforeDoes(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `delete from refs_forms_owed`); err != nil {
		t.Fatalf("settle the forms queue as the release before does: %v", err)
	}
}

// The release before this one reads `dispatch://CORE-2/decisions` as no reference and the Decisions
// page's link, `/issues/CORE-1/decisions`, as the issue's retired decisions document. Having
// indexed a comment and a document that hold them it settles their rows of the forms queue, and
// this build, whose trigger relays what that settle deletes, re-indexes both from the decisions
// queue with one pass: the issue's edges are written back, the retired document's edge the older
// index wrote for the link is deleted from the document and from its block, and the queue is empty.
func TestTheDecisionsFormsDrainRestoresWhatTheReleaseBeforeIndexedAway(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	comment := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	notes := createIndexedNotes(t, database, service,
		"The decisions: dispatch://CORE-2/decisions.\n\nThe page: https://dispatch.example/issues/CORE-1/decisions.\n")
	settleTheFormsQueueAsTheReleaseBeforeDoes(t, database)
	owed := decisionsFormsOwedRows(t, database)
	if len(owed) != 2 || owed["comment "+comment].IsZero() || owed["artifact "+notes].IsZero() {
		t.Fatalf("the decisions forms queue holds %v after the release before settled the forms queue, want the comment and the document", owed)
	}
	if forms := formsOwedRows(t, database); len(forms) != 0 {
		t.Fatalf("the forms queue holds %v after the settle, want it empty", forms)
	}
	// What the release before wrote for the link: the retired document's edge, to the document and
	// to the block that holds the link.
	page := holderOf(t, service, notes, "https://dispatch.example/issues/CORE-1/decisions")
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'artifact', 'CORE-1/decisions-md')`, []any{notes}},
		{`insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version)
		  values ($1::uuid, $2, 'CORE-1/notes#' || $2, 'artifact', 'CORE-1/decisions-md', 1)`, []any{notes, page}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("write the edge the release before writes for the page's link: %v", err)
		}
	}

	tick := reconcileDecisionsForms(t, service)

	if tick.Texts != 1 || tick.Documents != 1 || tick.OwedTexts != 0 || tick.LiveAhead != 0 || tick.Unindexed != 0 {
		t.Fatalf("pass = %#v, want the comment and the document indexed and nothing left owed", tick)
	}
	assertRef(t, database, "comment", comment, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-1", refsGrammarWritten)
	if slices.ContainsFunc(refsFrom(t, database, "artifact", notes), func(ref refRow) bool { return ref.ToKind == "artifact" }) {
		t.Fatalf("the document keeps the retired document's edge its link does not write: %#v", refsFrom(t, database, "artifact", notes))
	}
	pairs := blockPairsOf(t, database, notes)
	if slices.ContainsFunc(pairs, func(pair blockPair) bool { return pair.ToKind == "artifact" }) {
		t.Fatalf("the document's blocks keep the retired document's edge: %#v", pairs)
	}
	if !slices.ContainsFunc(pairs, func(pair blockPair) bool {
		return pair.BlockID == page && pair.ToKind == "issue" && pair.ToID == "CORE-1"
	}) {
		t.Fatalf("the document's blocks hold %#v, want block %s citing CORE-1", pairs, page)
	}
	if forms := formsOwedRows(t, database); len(forms) != 0 {
		t.Fatalf("the forms queue holds %v after the pass, want it empty", forms)
	}
	if decisions := decisionsFormsOwedRows(t, database); len(decisions) != 0 {
		t.Fatalf("the decisions forms queue holds %v after the pass, want it empty", decisions)
	}
}

// A document that is gone owes no text. A row of the decisions queue for one, which a removal by
// the release before leaves, since its delete from the forms queue relays the row while the
// document's own still stands, is settled as gone.
func TestTheDecisionsFormsDrainSettlesAGoneDocument(t *testing.T) {
	database, service := blockIndexService(t)
	var gone string
	if err := database.Pool.QueryRow(context.Background(), `select gen_random_uuid()::text`).Scan(&gone); err != nil {
		t.Fatalf("draw a uuid: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `insert into refs_decisions_forms_owed (kind, id) values ('artifact', $1::uuid)`, gone); err != nil {
		t.Fatalf("owe the gone document: %v", err)
	}

	tick := reconcileDecisionsForms(t, service)

	if tick.Gone != 1 || tick.OwedTexts != 0 {
		t.Fatalf("pass = %#v, want the gone document's row settled", tick)
	}
}

// One tick of the forms duty runs a pass of each queue: the text each queue owes is re-indexed,
// and the wait before the next tick is the interval of a drained queue.
func TestOneTickOfTheFormsDutyDrainsBothQueues(t *testing.T) {
	database, service := blockIndexService(t)
	older := createRawComment(t, database, "The plan: dispatch://CORE-2/plan.")
	decisions := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	if _, err := database.Pool.Exec(context.Background(), `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, decisions); err != nil {
		t.Fatalf("settle the decisions comment's forms row as the release before does: %v", err)
	}

	wait := service.refsFormsTick(context.Background(), []*formsDrain{newFormsDrain(refs.RefsFormsOwed), newFormsDrain(refs.RefsDecisionsFormsOwed)})

	if wait != BlockIndexReconcileInterval {
		t.Fatalf("the tick asks to wait %v, want the interval %v: neither queue was full", wait, BlockIndexReconcileInterval)
	}
	if !citesIssue(t, database, "comment", older, "CORE-2") || !citesIssue(t, database, "comment", decisions, "CORE-2") {
		t.Fatalf("the forms queue's comment cites %#v and the decisions queue's %#v, want each to cite CORE-2",
			refsFrom(t, database, "comment", older), refsFrom(t, database, "comment", decisions))
	}
	if forms, owed := formsOwedRows(t, database), decisionsFormsOwedRows(t, database); len(forms) != 0 || len(owed) != 0 {
		t.Fatalf("the queues hold %v and %v after the tick, want both empty", forms, owed)
	}
}

// A queue whose batch is full does not hold back the other, and the tick asks for the short wait.
// The forms queue holds one document more than a pass indexes (blockIndexDocumentBatch), each of
// them gone and so settled at once, and the decisions queue one comment: the first pass leaves the
// last document, the second re-indexes the comment, and the wait is blockIndexStep.
func TestAFullFormsQueueDoesNotHoldBackTheDecisionsQueue(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		insert into refs_forms_owed (kind, id) select 'artifact', gen_random_uuid() from generate_series(1, $1::int)
	`, blockIndexDocumentBatch+1); err != nil {
		t.Fatalf("owe the forms queue more documents than a pass indexes: %v", err)
	}
	comment := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	if _, err := database.Pool.Exec(ctx, `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, comment); err != nil {
		t.Fatalf("settle the comment's forms row as the release before does: %v", err)
	}

	wait := service.refsFormsTick(ctx, []*formsDrain{newFormsDrain(refs.RefsFormsOwed), newFormsDrain(refs.RefsDecisionsFormsOwed)})

	if wait != blockIndexStep {
		t.Fatalf("the tick asks to wait %v, want %v: the forms queue's pass was full", wait, blockIndexStep)
	}
	if forms := formsOwedRows(t, database); len(forms) != 1 {
		t.Fatalf("the forms queue holds %v after the tick, want the one document its pass left", forms)
	}
	if !citesIssue(t, database, "comment", comment, "CORE-2") {
		t.Fatalf("the decisions queue's comment cites %#v after a tick whose forms pass was full, want CORE-2", refsFrom(t, database, "comment", comment))
	}
	if decisions := decisionsFormsOwedRows(t, database); len(decisions) != 0 {
		t.Fatalf("the decisions queue holds %v after the tick, want it empty", decisions)
	}
}

// A pass that fails does not keep the other from running. The forms queue's cursor here names a
// row no uuid spells, so its listing fails; the decisions queue's comment is re-indexed all the
// same, and a pass that failed asks for no short wait.
func TestAFailedFormsPassDoesNotHoldBackTheDecisionsQueue(t *testing.T) {
	database, service := blockIndexService(t)
	comment := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	if _, err := database.Pool.Exec(context.Background(), `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, comment); err != nil {
		t.Fatalf("settle the comment's forms row as the release before does: %v", err)
	}
	forms := newFormsDrain(refs.RefsFormsOwed)
	forms.cursor = refs.Owed{Queue: refs.RefsFormsOwed, Kind: "comment", ID: "not-a-uuid", EnqueuedAt: time.Now()}

	wait := service.refsFormsTick(context.Background(), []*formsDrain{forms, newFormsDrain(refs.RefsDecisionsFormsOwed)})

	if wait != BlockIndexReconcileInterval {
		t.Fatalf("the tick asks to wait %v, want the interval %v: no queue reported a full batch", wait, BlockIndexReconcileInterval)
	}
	if !citesIssue(t, database, "comment", comment, "CORE-2") {
		t.Fatalf("the decisions queue's comment cites %#v after a tick whose forms pass failed, want CORE-2", refsFrom(t, database, "comment", comment))
	}
}

// The duty runs under a lease of its own, so a pass of the release before, which holds `refs-forms`
// for the length of its pass, takes nothing from it: the decisions queue is drained while that
// lease is held, which a duty under the same name would find held and skip until the older pass
// ended, in a period of its own.
func TestTheFormsDutyDrainsWhileTheReleaseBeforeHoldsItsLease(t *testing.T) {
	database, service := blockIndexService(t)
	holder, mine := storetest.Leases(t, database.Pool), storetest.Leases(t, database.Pool)
	comment := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	if _, err := database.Pool.Exec(context.Background(), `delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, comment); err != nil {
		t.Fatalf("settle the comment's forms row as the release before does: %v", err)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		duty.Exclusive(context.Background(), nil, holder, "refs-forms", time.Now(), BlockIndexReconcileInterval, func(context.Context) {
			close(held)
			<-release
		})
	}()
	testcleanup.Add(t, func() {
		close(release)
		<-done
	})
	testwait.Receive[struct{}](t, "the stand-in for the release before's pass to take its lease", held)

	runExcerpts(t, func(ctx context.Context) { service.RunRefsFormsReconciliation(ctx, mine) })

	testwait.Eventually(t, "the duty to re-index the decisions queue's comment while the release before holds refs-forms", func() bool {
		return citesIssue(t, database, "comment", comment, "CORE-2")
	})
	if RefsFormsLease == "refs-forms" {
		t.Fatalf("the duty's lease is %q, the name the release before drains under", RefsFormsLease)
	}
}

// A text stored before this release is in the bootstrap queue alone, or in the live queue as well
// where a settle of the release before was relayed while the seed ran. One pass of the bootstrap
// queue re-indexes it as a comment and as a document, writes the issue's edges, deletes the
// retired document's, and leaves nothing owed in any of the three queues: the index clears the
// live row it settles beside its own.
func TestTheBootstrapQueueRestoresTextStoredBeforeTheReleaseAndClearsTheLiveRowToo(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	comment := createRawComment(t, database, "The decisions: dispatch://CORE-2/decisions.")
	notes := createIndexedNotes(t, database, service,
		"The decisions: dispatch://CORE-2/decisions.\n\nThe page: https://dispatch.example/issues/CORE-1/decisions.\n")
	page := holderOf(t, service, notes, "https://dispatch.example/issues/CORE-1/decisions")
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into refs (from_kind, from_id, to_kind, to_id) values ('artifact', $1, 'artifact', 'CORE-1/decisions-md')`, []any{notes}},
		{`insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, created_version)
		  values ($1::uuid, $2, 'CORE-1/notes#' || $2, 'artifact', 'CORE-1/decisions-md', 1)`, []any{notes, page}},
		{`insert into refs_decisions_forms_seed_owed (kind, id) values ('comment', $1::uuid), ('artifact', $2::uuid)`, []any{comment, notes}},
		// The settle of the release before that lands while the seed runs.
		{`delete from refs_forms_owed where kind = 'comment' and id = $1::uuid`, []any{comment}},
	} {
		if _, err := database.Pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("store the text as a build that does not read the page does: %v", err)
		}
	}
	if live := decisionsFormsOwedRows(t, database); len(live) == 0 || live["comment "+comment].IsZero() {
		t.Fatalf("the live decisions queue holds %v after the relay, want the comment", live)
	}

	tick, err := service.ReconcileRefsForms(ctx, refs.RefsDecisionsFormsSeedOwed, refs.Owed{}, newFormsLiveAhead())
	if err != nil {
		t.Fatalf("bootstrap drain pass: %v", err)
	}

	if tick.Texts != 1 || tick.Documents != 1 || tick.OwedTexts != 0 || tick.LiveAhead != 0 || tick.Unindexed != 0 {
		t.Fatalf("pass = %#v, want the comment and the document indexed and nothing left owed", tick)
	}
	assertRef(t, database, "comment", comment, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-2", refsGrammarWritten)
	assertRef(t, database, "artifact", notes, "issue", "CORE-1", refsGrammarWritten)
	if slices.ContainsFunc(refsFrom(t, database, "artifact", notes), func(ref refRow) bool { return ref.ToKind == "artifact" }) ||
		slices.ContainsFunc(blockPairsOf(t, database, notes), func(pair blockPair) bool { return pair.ToKind == "artifact" }) {
		t.Fatalf("the document keeps the retired document's edge: %#v, blocks %#v", refsFrom(t, database, "artifact", notes), blockPairsOf(t, database, notes))
	}
	for queue, owed := range map[string]map[string]time.Time{
		"refs_forms_owed":                formsOwedRows(t, database),
		"refs_decisions_forms_owed":      decisionsFormsOwedRows(t, database),
		"refs_decisions_forms_seed_owed": decisionsFormsSeedOwedRows(t, database),
	} {
		if len(owed) != 0 {
			t.Fatalf("%s holds %v after the pass, want it empty", queue, owed)
		}
	}
}

// Nothing writes the bootstrap queue after its migration, so the duty reads it until a pass finds
// it empty and no more: the queue holds one document more than a pass indexes, each gone and so
// settled at once, the first tick leaves the last, the second settles it and ends the drain, and a
// row written after that is never read. A pass that failed, though it left the queue empty, is not
// the last.
func TestTheFormsDutyStopsReadingTheBootstrapQueueOnceItIsEmpty(t *testing.T) {
	database, service := blockIndexService(t)
	ctx := context.Background()
	oweGoneDocuments := func(count int) {
		t.Helper()
		if _, err := database.Pool.Exec(ctx, `
			insert into refs_decisions_forms_seed_owed (kind, id) select 'artifact', gen_random_uuid() from generate_series(1, $1::int)
		`, count); err != nil {
			t.Fatalf("owe the bootstrap queue %d gone documents: %v", count, err)
		}
	}
	oweGoneDocuments(blockIndexDocumentBatch + 1)
	drains := newFormsDrains()
	seed := drains[len(drains)-1]
	if seed.queue != refs.RefsDecisionsFormsSeedOwed || !seed.oneShot {
		t.Fatalf("the duty's last drain is %#v, want the one-shot drain of the bootstrap queue", seed)
	}

	if wait := service.refsFormsTick(ctx, drains); wait != blockIndexStep || seed.done {
		t.Fatalf("after a pass that left a document owed the tick asks to wait %v and the drain is done %v, want %v and not done", wait, seed.done, blockIndexStep)
	}
	if wait := service.refsFormsTick(ctx, drains); wait != BlockIndexReconcileInterval || !seed.done {
		t.Fatalf("after the pass that emptied the queue the tick asks to wait %v and the drain is done %v, want %v and done", wait, seed.done, BlockIndexReconcileInterval)
	}

	oweGoneDocuments(1)
	if wait := service.refsFormsTick(ctx, drains); wait != BlockIndexReconcileInterval {
		t.Fatalf("the tick asks to wait %v after the drain was done, want %v", wait, BlockIndexReconcileInterval)
	}
	if owed := decisionsFormsSeedOwedRows(t, database); len(owed) != 1 {
		t.Fatalf("the bootstrap queue holds %v after a tick that should not read it, want the row written after the drain was done", owed)
	}

	failing := newFormsDrains()
	failing[len(failing)-1].cursor = refs.Owed{Queue: refs.RefsDecisionsFormsSeedOwed, Kind: "artifact", ID: "not-a-uuid", EnqueuedAt: time.Now()}
	if _, err := database.Pool.Exec(ctx, `delete from refs_decisions_forms_seed_owed`); err != nil {
		t.Fatalf("empty the bootstrap queue: %v", err)
	}
	service.refsFormsTick(ctx, failing)
	if failing[len(failing)-1].done {
		t.Fatal("a pass of the bootstrap queue that failed ended its drain, want it read again")
	}
	failing[len(failing)-1].cursor = refs.Owed{}
	service.refsFormsTick(ctx, failing)
	if !failing[len(failing)-1].done {
		t.Fatal("a pass of the bootstrap queue that ran and found it empty did not end its drain")
	}
}
