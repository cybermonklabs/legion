package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// This file tests the deadline of the two transactions of the server's own that write an issue's
// documents in turn on a context Join puts no bound on (docs' boundCallerWrite): the close's sweep
// on the context an unkeyed close detaches from its request (outlivingTheClient), and each issue's
// transaction of the ask repair's pass. Such a transaction can wait for its second document's lock
// while the document's persistence worker, holding that lock, waits in its gate's fold for the
// grant the transaction's first document's fork took (docs' loadSlots). Each transaction ends no
// later than docs.WriteRequestBound after it begins and gives its grant back, and the worker then
// stores what it held. Before, nothing ended that wait, and each test fails by its own timeout,
// writeBoundMargin past the bound, with the transaction still waiting.

// writeBoundMargin is how far past docs.WriteRequestBound a transaction the bound ends may answer:
// the statement the bound cancels and the rollback after it.
const writeBoundMargin = 10 * time.Second

// heldSecondDocumentWait bounds each wait of the setup below for work that runs at once: a lock
// waiter queueing, the worker's gate reaching the pool, and a keystroke stored once the pool has
// the bytes for it.
const heldSecondDocumentWait = 30 * time.Second

// unindexedAsk is the ask block the issue's first document holds with no ask row, which the close's
// final sweep marks resolved (docs.WriteIssueAskStates with markUnindexed).
const unindexedAsk = `:::ask{#late-ask urgency="low" multiple="false"}` + "\nWritten after the close's fork?\n:::\n"

// unindexedAskResolved is unindexedAsk as the sweep leaves it.
const unindexedAskResolved = `#late-ask urgency="low" multiple="false" state="resolved"`

// firstDocumentProse is the prose the issue's first document holds beside its ask block, about 17
// KiB: the grant its fork takes (docs' copyWeight) then covers the fold of the second document's
// small history, so the worker's fold takes it once the transaction gives it back, while the rest
// of the pool stays held.
var firstDocumentProse = strings.Repeat(strings.Repeat("Prose that weighs the first document's fork. ", 12)+"\n\n", 32)

// heldSecondDocument is a closed issue of two documents, first and second in the id order the
// close's sweep and the ask repair write them (docs.WriteIssueAskStates), set up for a transaction
// under test to write them. first holds an ask block no settlement has indexed and owes a
// settlement, as a document a browser wrote after the close's fork does, so the ask repair sweeps
// the issue. Alice's keystroke on second waits at the document's lock, which holder holds, and
// holder has written second past alice's room and folded its history, so the room's persistence
// worker, once it has the lock, decodes that history in its gate's fold (docs'
// storedHistoryHolds). The process's load concurrency is one slot, pool, whose first taker to
// wait closes foldWaits.
type heldSecondDocument struct {
	repairFixture
	served *server
	issue  string
	first  string
	second string
	// firstUpdates is how many updates first's stored history holds before the transaction under
	// test, which a transaction that rolled back leaves as it is.
	firstUpdates int
	// head is second's durable head before alice's keystroke: holder's write is head+1, and the
	// keystroke is stored after it.
	head      int64
	holder    pgx.Tx
	pool      *docs.LoadPoolForTest
	foldWaits <-chan struct{}
}

func newHeldSecondDocument(t *testing.T) *heldSecondDocument {
	t.Helper()
	ctx := context.Background()
	// An hour, so no settlement runs but the ones this setup runs itself. The service names the
	// person a document socket's header names, as a browser's sign-in does.
	var documents *docs.Service
	handler, database, deps, served := newInteractionServer(t, func(database *store.Store) docs.API {
		documents = docs.New(docs.Deps{Store: database, Identity: headerIdentity(database), Settle: time.Hour})
		t.Cleanup(func() { _ = documents.Shutdown(context.Background()) })
		return documents
	})
	issue := createInteractionIssue(t, handler, "TEST", "Swept while its second document's lock is held", "Context\n")
	uploaded := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"actor": sessionActor(), "name": "notes.md", "content": "before\n",
	})
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the issue's second document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	if err := documents.SettleIssueDocuments(ctx, issue.Key); err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	rows, err := database.Pool.Query(ctx, `select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id`, issue.Key)
	if err != nil {
		t.Fatalf("list the issue's documents: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(ids) != 2 {
		t.Fatalf("the issue's documents are %v (%v), want two", ids, err)
	}
	f := &heldSecondDocument{
		repairFixture: repairFixture{handler: handler, database: database, documents: documents, deps: deps},
		served:        served,
		issue:         issue.Key,
		first:         ids[0],
		second:        ids[1],
	}
	awaitSettled(t, database, f.first, f.second)

	// The first document's weight and its ask block go to the store while no room holds it, as a
	// write another process stored after the close's fork, and leave it owing a settlement. The
	// settlement above loaded its room, which would stay until ygo's idle minute ran out.
	if err := documents.Evict(ctx, f.first); err != nil {
		t.Fatalf("close the first document's room: %v", err)
	}
	if slices.Contains(documents.ResidentRooms(), f.first) {
		t.Fatal("the first document's room is still resident once it was closed")
	}
	if err := docs.AppendStoredChangeForTest(ctx, database, f.first, appendStoredBlocks(firstDocumentProse+unindexedAsk)); err != nil {
		t.Fatalf("write the first document's ask block: %v", err)
	}
	awaitOwedSettlement(t, database, f.first)
	f.firstUpdates = storedUpdateCount(t, database, f.first)

	// Alice's keystroke on the second document reaches its room's worker, which waits for the
	// document's lock while another process writes the document past the room's cursor and folds
	// its history under it.
	alice := connectBrowserPeer(t, documents, f.second)
	f.head = f.secondHead(t)
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the second document's lock: %v", err)
	}
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	if err := docs.LockDocumentRoom(ctx, holder, f.second); err != nil {
		t.Fatalf("hold the second document's lock: %v", err)
	}
	alice.appendParagraph(t, "Typed before the close.\n")
	alice.barrier(t)
	storetest.WaitForLockWaiters(t, holder, 1)
	// The issue closes as a release older than the withdrawal closed one, after alice's keystroke,
	// which the append gate then stores (docs' observedBeforeClose). Nothing tells the rooms.
	if _, err := database.Pool.Exec(ctx, `update issues set status = 'done', closed_at = now() where key = $1`, f.issue); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	appendIssueEvent(t, f.repairFixture, f.issue, "issue.closed", "done", model.Actor{Kind: "user", ID: "alice"})
	if err := docs.FoldHistoryPastRoomForTest(ctx, holder, f.second, f.head); err != nil {
		t.Fatalf("fold the second document's history past alice's room: %v", err)
	}

	foldWaits := make(chan struct{})
	pool, restore := docs.UseOneLoadSlotForTest(sync.OnceFunc(func() { close(foldWaits) }))
	t.Cleanup(restore)
	f.holder, f.pool, f.foldWaits = holder, pool, foldWaits
	return f
}

// appendStoredBlocks is the change docs.AppendStoredChangeForTest makes to append markdown's
// blocks to a document.
func appendStoredBlocks(markdown string) func(*crdt.Transaction, *crdt.YXmlFragment) error {
	return func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		tree, err := pmdoc.ReadInTransaction(txn, fragment)
		if err != nil {
			return err
		}
		typed, err := pmdoc.Parse(markdown)
		if err != nil {
			return err
		}
		tree.Children = append(tree.Children, typed.Children...)
		return pmdoc.Update(txn, fragment, tree)
	}
}

// secondHead is the second document's durable head.
func (f *heldSecondDocument) secondHead(t *testing.T) int64 {
	t.Helper()
	var head int64
	if err := f.database.Pool.QueryRow(context.Background(), `
		select coalesce(max(version), 0) from doc_updates where artifact_id = $1
	`, f.second).Scan(&head); err != nil {
		t.Fatalf("read the second document's head: %v", err)
	}
	return head
}

// sweepAsAnUnkeyedClose runs served's close's sweep of issue (sweepClosedIssueAsks) on a goroutine
// of its own, on the context an unkeyed close hands it: its request's, marked as every request is
// (trackTransactions: the caller's mark markCallerRequest puts on a row's request, and the
// transaction tracking routeContext adds) and detached from its client (outlivingTheClient), whose
// client has then gone away. It answers when the sweep began, and answered, sent nil once the
// sweep has returned.
func sweepAsAnUnkeyedClose(served *server, issue string) (started time.Time, answered <-chan error) {
	client, leave := context.WithCancel(context.Background())
	var outliving context.Context
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/issues/"+issue, nil).WithContext(client)
	trackTransactions(func(_ http.ResponseWriter, r *http.Request) { outliving = outlivingTheClient(r) })(httptest.NewRecorder(), request)
	leave()
	swept := make(chan error, 1)
	started = time.Now()
	go func() {
		served.sweepClosedIssueAsks(outliving, issue, nil, model.Actor{Kind: "user", ID: "alice"})
		swept <- nil
	}()
	return started, swept
}

// repairInThePass runs the ask repair's pass (RepairIssueAsks) on a goroutine of its own, as the
// boot does, under no bound of the pass's. It answers when the pass began, and answered, sent the
// pass's error once it has returned.
func (f *heldSecondDocument) repairInThePass() (started time.Time, answered <-chan error) {
	repaired := make(chan error, 1)
	started = time.Now()
	go func() {
		_, err := RepairIssueAsks(context.Background(), f.deps, nil)
		repaired <- err
	}()
	return started, repaired
}

// holdTheCycle closes the cycle around the transaction under test, what, which runs on a goroutine
// of its own and answers on answered: once it has written the first document and waits for the
// second's lock behind alice's append, holding its first fork's grant, the rest of the pool is
// filled and holder lets go of the lock, so the worker takes the lock and its gate's fold waits for
// the pool while the transaction waits for the lock the worker holds. It answers what gives the
// filled bytes back, which the test's end gives back too.
func (f *heldSecondDocument) holdTheCycle(t *testing.T, what string, answered <-chan error) (release func()) {
	t.Helper()
	ctx := context.Background()
	type seen struct {
		waiting int
		err     error
		early   bool
	}
	got := testwait.UntilPolling(t, 10*time.Millisecond, fmt.Sprintf("%s to wait for the second document's lock behind alice's append, or to answer (what was last read)", what), func() (seen, bool) {
		select {
		case err := <-answered:
			return seen{err: err, early: true}, true
		default:
		}
		waiting, err := storetest.LockWaiters(ctx, f.holder)
		if err != nil {
			t.Fatal(err)
		}
		return seen{waiting: waiting}, waiting >= 2
	})
	if got.early {
		t.Fatalf("%s answered (%v) before it waited for the second document's lock", what, got.err)
	}
	if f.pool.HeldForTest() == 0 {
		t.Fatalf("%s waits for the second document's lock holding nothing of the pool", what)
	}
	release, filled := f.pool.FillForTest()
	if !filled {
		t.Fatal("the pool refused the bytes its holders do not hold")
	}
	t.Cleanup(release)
	if err := f.holder.Commit(ctx); err != nil {
		t.Fatalf("let go of the second document's lock: %v", err)
	}
	select {
	case <-f.foldWaits:
	case err := <-answered:
		t.Fatalf("%s answered (%v) before the second room's worker waited for the pool", what, err)
	case <-time.After(heldSecondDocumentWait):
		t.Fatal("the second room's worker never came to wait for the pool in its gate's fold")
	}
	// The worker queued for the lock first and took it: the transaction still waits for it.
	if waiting, err := storetest.CountLockWaits(ctx, f.database, "select pg_advisory_xact_lock(%"); err != nil || waiting < 1 {
		t.Fatalf("%s no longer waits for the second document's lock once the worker waits for the pool (%d waiting, %v): it took the lock first, and nothing here holds the cycle", what, waiting, err)
	}
	return release
}

// requireEndedAtTheBound waits for answered, the answer of what, the transaction under test that
// began at started, and fails the test unless it comes docs.WriteRequestBound after that start,
// within writeBoundMargin. It answers what answered sent.
func requireEndedAtTheBound(t *testing.T, what string, started time.Time, answered <-chan error) error {
	t.Helper()
	select {
	case err := <-answered:
		elapsed := time.Since(started)
		t.Logf("%s answered %s after it began (docs.WriteRequestBound %s): %v", what, elapsed.Round(time.Millisecond), docs.WriteRequestBound, err)
		if elapsed < docs.WriteRequestBound-time.Second {
			t.Fatalf("%s answered %s after it began, before docs.WriteRequestBound %s: something other than its bound ended it", what, elapsed, docs.WriteRequestBound)
		}
		return err
	case <-time.After(time.Until(started.Add(docs.WriteRequestBound + writeBoundMargin))):
		t.Fatalf("%s still waited for the second document's lock %s after it began, past docs.WriteRequestBound %s and %s beside it: nothing ended the wait, and the worker holding that lock waits for the grant the transaction holds",
			what, time.Since(started).Round(time.Second), docs.WriteRequestBound, writeBoundMargin)
		return nil
	}
}

// requireGrantGivenBack fails the test unless the second room's worker stores alice's keystroke
// while the rest of the pool stays filled, its gate's fold then holding the bytes the transaction
// under test gave back, and unless that transaction committed nothing of the first document.
func (f *heldSecondDocument) requireGrantGivenBack(t *testing.T) {
	t.Helper()
	if !testwait.Holds(heldSecondDocumentWait, testwait.Poll, func() bool { return f.secondHead(t) > f.head+1 }) {
		t.Fatalf("the second room's worker did not store alice's keystroke within %s of the transaction's end: the transaction kept the grant its fold waits for", heldSecondDocumentWait)
	}
	if stored := storedUpdateCount(t, f.database, f.first); stored != f.firstUpdates {
		t.Fatalf("the first document holds %d stored updates, want the %d it held before: the transaction the bound ended committed", stored, f.firstUpdates)
	}
}

// replacedHistoryRoomClosed is the start of the line docs' closeRoomOnReplacedHistory logs as it
// closes a room whose browser's update was stored on a history replaced under it, for its
// reload; the room's id follows it.
const replacedHistoryRoomClosed = `msg="dispatch: closed a document room whose browser's update was stored on a history replaced under it" room=`

// requireNextRepairSweeps gives the filled bytes back (release), waits for the second document's
// room, which stored alice's keystroke on a history replaced under it, to close for its reload
// (replacedHistoryRoomClosed), since a write meeting that close is refused for a later pass to
// retry, and fails the test unless the ask repair's next pass then sweeps the closed issue
// (requireRepairSweepsFirst).
func (f *heldSecondDocument) requireNextRepairSweeps(t *testing.T, logs *storetest.LockedLog, release func()) {
	t.Helper()
	release()
	closed := replacedHistoryRoomClosed + f.second
	testwait.Eventually(t, "the second document's room to close for its reload", func() bool {
		return strings.Contains(logs.String(), closed)
	})
	f.requireRepairSweepsFirst(t)
}

// requireRepairSweepsFirst runs the ask repair's next pass, which sweeps the closed issue, its
// first document owing a settlement. It fails the test unless that pass marks the first
// document's ask block resolved: the closed state the sweep writes.
func (f *heldSecondDocument) requireRepairSweepsFirst(t *testing.T) {
	t.Helper()
	repair := repairNow(t, f.repairFixture)
	if repair.Swept != 1 || repair.Withdrawn != 0 || repair.Restored != 0 {
		t.Fatalf("the next repair pass = %+v, want the first document's ask block swept", repair)
	}
	requireLiveText(t, f.handler, f.first, unindexedAskResolved)
}

// The close's sweep of an unkeyed close runs on its request's context with the client's
// cancellation taken off, which has no Done, so Join put no deadline on its transaction. Waiting
// for its second document's lock while the worker holding that lock waits for the grant its first
// document's fork took, it waited for good. Its own deadline ends that wait at
// docs.WriteRequestBound - from its request's start, whose mark the context keeps (docs'
// boundCallerWrite) - and the sweep rolls back, gives its grant back and logs its end, naming the
// issue; the worker then stores alice's keystroke.
func TestAnUnkeyedClosesSweepWaitingForItsSecondDocumentsLockEndsAtTheWriteBound(t *testing.T) {
	f := newHeldSecondDocument(t)
	logs := captureLogs(t)
	started, answered := sweepAsAnUnkeyedClose(f.served, f.issue)
	f.holdTheCycle(t, "the close's sweep", answered)
	requireEndedAtTheBound(t, "the close's sweep", started, answered)
	logged := logs.String()
	if !strings.Contains(logged, `msg="dispatch: sweep the ask blocks of a closed issue"`) || !strings.Contains(logged, "issue="+f.issue) ||
		!strings.Contains(logged, "a write bound ended the sweep's transaction") {
		t.Fatalf("the sweep logged:\n%s\nwant its end at its bound, naming %s", logged, f.issue)
	}
	f.requireGrantGivenBack(t)
}

// The ask repair's pass is the server's own work, with no caller's mark, so Join put no deadline on
// an issue's transaction, and the pass's own bound - none for this pass, 30 s at boot, 5 minutes
// for an interval pass - was all that ended its wait for the second document's lock. The issue's
// transaction now ends docs.WriteRequestBound after it begins, rolls back and gives its grant back,
// and the pass logs that end, naming the issue; the next pass repairs the issue.
func TestAnAskRepairsTransactionWaitingForItsSecondDocumentsLockEndsAtTheWriteBound(t *testing.T) {
	f := newHeldSecondDocument(t)
	logs := captureLogs(t)
	started, answered := f.repairInThePass()
	release := f.holdTheCycle(t, "the ask repair's transaction", answered)
	err := requireEndedAtTheBound(t, "the ask repair's transaction", started, answered)
	if err == nil || !strings.Contains(err.Error(), f.issue) || !strings.Contains(err.Error(), "the issue's write bound of "+docs.WriteRequestBound.String()+" ended its transaction") {
		t.Fatalf("the repair pass answered %v, want the issue %s ended at its bound", err, f.issue)
	}
	logged := logs.String()
	if !strings.Contains(logged, `msg="dispatch: repair the asks of an issue"`) || !strings.Contains(logged, "issue="+f.issue) ||
		!strings.Contains(logged, "the issue's write bound of") {
		t.Fatalf("the repair pass logged:\n%s\nwant the issue's end at its bound, naming %s", logged, f.issue)
	}
	f.requireGrantGivenBack(t)
	f.requireNextRepairSweeps(t, logs, release)
}

// A close's sweep its bound ended rolled back, leaving the ask block a browser wrote after the
// close's fork reading open. The document owes a settlement, so the ask repair's next pass, which
// sweeps a closed issue whose document owes one, sweeps the issue and marks the block resolved, as
// the sweep would have.
func TestASweepTheWriteBoundEndedIsFinishedByTheNextAskRepair(t *testing.T) {
	f := newHeldSecondDocument(t)
	logs := captureLogs(t)
	started, answered := sweepAsAnUnkeyedClose(f.served, f.issue)
	release := f.holdTheCycle(t, "the close's sweep", answered)
	requireEndedAtTheBound(t, "the close's sweep", started, answered)
	f.requireGrantGivenBack(t)
	f.requireNextRepairSweeps(t, logs, release)
}
