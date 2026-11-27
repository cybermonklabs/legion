package docs

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"weak"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// This file tests the pod's load concurrency's rule that no holder of a grant waits for more of
// the pool while it holds it (loadSlots, loadSlot.take): each holder that needs more takes it at
// once, gives its grant back and takes the total whole, does its work again under the whole
// grant, or refuses busy, and no holder is excepted - a transaction holds one grant for its forks
// and its reads (Ledger.forkSlot, withLoadSlot), and every take of it after its first fits at once
// or is refused busy. The pool serves its waiters in order (semaphore.Weighted), so most tests
// queue a taker for more of the pool than is free beside the holder under test and then have that
// holder need more: a holder that queued behind the taker for the difference would wait for it,
// while the taker waits for the bytes the holder holds, and the test fails at holdAndWaitBound
// naming that wait. With take put back to wait for the difference while it holds, every test here
// but the leak's and the up-front take's fails: at that bound, or at the state answer's own
// WriteWaitBound, which refuses it.

// holdAndWaitBound bounds each wait here for a holder that needs more of the pool. The work each
// does once it has its bytes is a fold or a decode of a few hundred kilobytes, or none, and takes
// milliseconds; a holder that queued for more behind a taker waiting for its bytes never finishes
// while the test holds the rest of the pool, so the test fails at this bound by name, the bound
// besideAFullPoolWait gives work a full pool holds up.
const holdAndWaitBound = 30 * time.Second

// poolWaits makes a pool of one slot the process's load concurrency for the rest of t
// (poolOfOneSlot), whose waiting hook is a signal: each taker that finds too little of the pool
// free and starts to wait sends on it. It is set before any taker can reach the pool.
func poolWaits(t *testing.T) (*loadSlots, <-chan struct{}) {
	t.Helper()
	waits := make(chan struct{}, 64)
	pool, _ := poolOfOneSlot(t, func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	})
	return pool, waits
}

// queueTaker starts a taker of bytes of the pool that holds nothing, on a goroutine of its own,
// and returns once it waits for them (poolWaits): bytes past what the pool has free beside the
// holder under test, and within what it has once that holder gives its own back. The taker gives
// them back as soon as it holds them, and served closes then.
func queueTaker(t *testing.T, waits <-chan struct{}, bytes int64) (served <-chan struct{}) {
	t.Helper()
	for len(waits) > 0 {
		<-waits
	}
	done := make(chan struct{})
	go func() {
		taker := newLoadSlot()
		if err := taker.take(context.Background(), bytes); err == nil {
			taker.release()
		}
		close(done)
	}()
	select {
	case <-waits:
	case <-done:
		t.Fatalf("the taker of %d bytes was served at once: the pool had them free beside the holder, so nothing here is tested", bytes)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the taker of %d bytes neither waited for the pool nor was served within %s", bytes, holdAndWaitBound)
	}
	return done
}

// answeredWithin is what answered sends within holdAndWaitBound, failing the test with what names
// the holder where nothing comes by then.
func answeredWithin(t *testing.T, answered <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-answered:
		return err
	case <-time.After(holdAndWaitBound):
		t.Fatalf("%s did not finish within %s: it waited for more of the pool while it held its grant, which the pool could not give it while that grant stayed held", what, holdAndWaitBound)
		return nil
	}
}

// requireServed waits for the taker queueTaker started to be served.
func requireServed(t *testing.T, served <-chan struct{}) {
	t.Helper()
	select {
	case <-served:
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the queued taker was not served within %s once the holder had finished", holdAndWaitBound)
	}
}

// pause is a goroutine's stop at a hook of the code under test, until the test lets it go on:
// reached closes as it stops there, and goOn lets it go on, once, and runs as the test ends in any
// case.
type pause struct {
	reached chan struct{}
	goOn    func()
	proceed chan struct{}
}

func newPause(t *testing.T) *pause {
	t.Helper()
	p := &pause{reached: make(chan struct{}), proceed: make(chan struct{})}
	p.goOn = sync.OnceFunc(func() { close(p.proceed) })
	t.Cleanup(p.goOn)
	return p
}

// stop is the hook's side: it reports the stop and waits to be let go on.
func (p *pause) stop() {
	close(p.reached)
	<-p.proceed
}

// await waits for the hook to stop, failing the test with what names it where it never does.
func (p *pause) await(t *testing.T, answered <-chan error, what string) {
	t.Helper()
	select {
	case <-p.reached:
	case err := <-answered:
		t.Fatalf("%s finished (%v) before it stopped where the test holds it", what, err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("%s never stopped where the test holds it within %s", what, holdAndWaitBound)
	}
}

// pauseAtFirstGrant stops the taker of the first grant pool gives (loadSlots.taken) until the
// pause it answers goes on.
func pauseAtFirstGrant(t *testing.T, pool *loadSlots) *pause {
	t.Helper()
	granted := newPause(t)
	var once atomic.Bool
	pool.taken = func(int64) {
		if once.CompareAndSwap(false, true) {
			granted.stop()
		}
	}
	return granted
}

// splitCharacters and splitInserts shape splitHistory for the two folds' test: rows weighed by
// their bytes, a state that declares an item more for each insert and so weighs more than they do,
// and less than twice as much.
const splitCharacters, splitInserts = 20_000, 400

// splitHistory is two rows of one text: characters characters typed in one update, and inserts
// characters typed into them in a second, each at a place of its own between them. A document
// applying both splits the first row's one item at every insert, so the state the fold encodes
// declares more structs than the rows declare between them, and weighs more than they do: the
// shape of an agent's rewrite, whose folded state CI found declaring a tenth more structs than its
// rows (browser_memory_test.go's agentRewrittenDocument, dispatch://INFRA-179).
func splitHistory(characters, inserts int) [][]byte {
	doc := crdt.New()
	text := doc.GetText("split")
	write := func(change func(txn *crdt.Transaction)) []byte {
		before := doc.StateVector()
		doc.Transact(change)
		return crdt.EncodeStateAsUpdateV1(doc, before)
	}
	typed := write(func(txn *crdt.Transaction) { text.Insert(txn, 0, strings.Repeat("x", characters), nil) })
	split := write(func(txn *crdt.Transaction) {
		for insert := inserts; insert > 0; insert-- {
			text.Insert(txn, insert*characters/(inserts+1), "y", nil)
		}
	})
	return [][]byte{typed, split}
}

// Two folds of one history whose state weighs more than its rows both finish, in a pool with room
// for one fold's read-back and not for two folds' rows: the first fold holds its rows' grant while
// the second queues for its own, which is more than is free, and the first fold's read-back then
// needs more than it holds. That is the cycle CI met between a settlement's room load and a
// write's cold fork of one rewritten document (job 136940412632, dispatch://INFRA-179): the
// read-back queued behind the second fold for bytes the pool had free, and the second fold waited
// for the first's, until the write's 45-second bound answered it 503. The read-back holds nothing
// it built by then - the fold's own document is dropped - so it gives its grant back and takes the
// state's weight whole (fold, holdUnbuiltLoad): the second fold takes the bytes given back, folds
// and reads back in turn, and each finishes. With take put back to wait for the difference while
// it holds, the first fold's read-back and the second fold wait on each other until this test's
// bound.
func TestTwoFoldsOfOneHistoryFinishWhereTheFirstsReadBackNeedsMoreThanItHolds(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	rows := splitHistory(splitCharacters, splitInserts)
	storeRows(t, database, artifactID, rows)
	rowsWeight := storedCopyWeight(rows...)
	state, _, _, misread, err := fold(ctx, memoryUpdates(ctx, rows), maxUpdateItems)
	if err != nil || misread != nil {
		t.Fatalf("fold the history: %v, %v", err, misread)
	}
	stateWeight := storedCopyWeight(state)
	if stateWeight <= rowsWeight || stateWeight >= 2*rowsWeight {
		t.Fatalf("the history's rows weigh %d and the state they fold into %d, want the state heavier than the rows and lighter than two folds' rows, or nothing here is tested",
			rowsWeight, stateWeight)
	}

	versioned := NewPgVersioned(database)
	pool, waits := poolWaits(t)
	// The pool has free what one fold's read-back takes: one fold's rows fit, a second fold's rows
	// do not fit beside them, and the read-back's difference does.
	filler := fillPoolBut(t, pool, stateWeight)
	held := newPause(t)
	var first atomic.Bool
	versioned.beforeFold = func(string) {
		if first.CompareAndSwap(false, true) {
			held.stop()
		}
	}
	loads := make(chan error, 2)
	load := func() {
		_, err := versioned.Load(ctx, artifactID)
		loads <- err
	}
	go load()
	held.await(t, loads, "the first fold")
	go load()
	select {
	case <-waits:
	case err := <-loads:
		t.Fatalf("the second fold finished (%v) while the first held its rows' grant, want it queued for its own", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the second fold neither queued for the pool nor finished within %s", holdAndWaitBound)
	}
	held.goOn()
	for range 2 {
		if err := answeredWithin(t, loads, "a fold of the history"); err != nil {
			t.Fatalf("a fold of the history: %v", err)
		}
	}
	if got := pool.held.Load(); got != filler.bytes {
		t.Fatalf("the pool holds %d bytes once both folds returned, want the test's %d alone", got, filler.bytes)
	}
	t.Logf("two folds of rows weighing %d into a state weighing %d finished in a pool with %d bytes free", rowsWeight, stateWeight, stateWeight)
}

// A cold write whose fork's read fails after its fold took bytes on the transaction's grant leaves
// none of them held. The fold takes them before any fork's cover does (foldRead, coldFork), so
// the write's own release of its share, which needs that cover, gave back nothing (giveFork), and
// the pool was short by those bytes until the process restarted. Here the write's request ends as
// its fold begins, so the fold, holding its grant, answers the request's end; the read that leaves
// no fork gives the grant back (forkLive, releaseUnforked), and the next write lands. Before, the
// pool still held what the fold took once the transaction had ended.
func TestAColdWriteThatFailsAfterItsFoldTookTheGrantLeavesNoneOfItHeld(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if _, err := service.persistence.AppendUpdate(ctx, artifactID, emptyUpdate); err != nil {
		t.Fatal(err)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the room is resident, so the write would fork warm and fold nothing")
	}
	versioned, ok := service.persistence.(*PgVersioned)
	if !ok {
		t.Fatalf("the service reads through %T, which has no seam before its fold", service.persistence)
	}
	pool := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	pool.taken = taken.record
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	versioned.beforeFold = func(room string) {
		if room == artifactID {
			cancel()
		}
	}
	err := grantWrite(request, service, artifactID, grantWriteHeading, "Written")
	if err == nil {
		t.Fatal("the write went through though its request ended inside its fork's fold")
	}
	grants := taken.all()
	if len(grants) == 0 {
		t.Fatalf("the write failed (%v) having taken nothing of the pool: its fold took no grant, so nothing here is tested", err)
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the write failed (%v) and its transaction ended, want none: what its cold fork's fold took on the transaction's grant (grants left %v held) was given back by nothing",
			held, err, grants)
	}
	versioned.beforeFold = nil
	if err := grantWrite(ctx, service, artifactID, grantWriteHeading, "Written"); err != nil {
		t.Fatalf("the write after it: %v", err)
	}
	requireHeading(t, service, artifactID, false)
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the next write landed, want none", held)
	}
	t.Logf("the write failed (%v) after its fold took %d bytes, and the pool held none of them after", err, grants[0])
}

// The decode of a log's runs after a fold that did not read back runs where nothing the fold built
// is held, on a grant its read holds alone (runsDocument, holdUnbuiltLoad), so where the runs
// weigh more than that grant holds and the pool does not give the difference at once, it gives
// the grant back and takes the runs' weight whole. Here a taker queued for more than is free
// beside the grant gets the bytes given back, and the decode then takes its own and finishes.
func TestARunsDecodeGivesItsReadsGrantBackRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	runs := [][]byte{proseState(t, 16<<10)}
	weight := storedCopyWeight(runs...)
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, weight)
	ctx, slot, done := withLoadSlot(context.Background())
	defer done()
	ctx = slot.ownedBy(ctx)
	// What the read's fold left its grant holding, lighter than the runs.
	if err := slot.take(ctx, weight/2); err != nil {
		t.Fatalf("take the fold's grant: %v", err)
	}
	served := queueTaker(t, waits, weight-weight/2+1)
	decoded := make(chan error, 1)
	go func() {
		_, err := runsDocument(ctx, runs, maxUpdateItems)
		decoded <- err
	}()
	if err := answeredWithin(t, decoded, "the runs' decode"); err != nil {
		t.Fatalf("decode the runs: %v", err)
	}
	requireServed(t, served)
	if slot.bytes != weight || pool.held.Load() != filler.bytes+weight {
		t.Fatalf("the read's grant holds %d bytes and the pool %d once the runs decoded, want the runs' %d and the test's %d beside them",
			slot.bytes, pool.held.Load(), weight, filler.bytes)
	}
}

// loadLeavingLess is a store whose Load takes took on the grant ctx carries, as a fold of rows
// lighter than the state they fold into leaves it, stops at then, and answers state.
type loadLeavingLess struct {
	VersionedStore
	state []byte
	took  int64
	then  func()
}

func (l *loadLeavingLess) Load(ctx context.Context, _ string) (persistence.LoadResult, error) {
	_, slot, _ := withLoadSlot(ctx)
	if err := slot.take(ctx, l.took); err != nil {
		return persistence.LoadResult{}, err
	}
	l.then()
	return persistence.LoadResult{Update: l.state, Version: 2}, nil
}

// A read of a stored history whose state weighs more than the grant its read left (the decode of a
// log's runs encoded anew, which can) takes the difference with nothing decoded yet: the read
// handed back bytes (loadDecodedHistory). On a grant it took up holding nothing, where the pool
// does not give the difference at once, it gives the grant back and takes the history's weight
// whole, and a taker queued for more than is free beside the grant gets the bytes given back.
func TestAReadHistoryHeavierThanItsReadsGrantGivesItBackRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	state := proseState(t, 16<<10)
	weight := storedCopyWeight(state)
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, weight)
	loaded := newPause(t)
	store := &loadLeavingLess{state: state, took: weight / 2, then: loaded.stop}
	read := make(chan error, 1)
	go func() {
		history, err := loadDecodedHistory(context.Background(), store, "room", maxUpdateItems, nil)
		if err == nil && history.decoded.doc == nil {
			err = errors.New("the history decoded into nothing")
		}
		read <- err
	}()
	loaded.await(t, read, "the history's read")
	served := queueTaker(t, waits, weight-weight/2+1)
	loaded.goOn()
	if err := answeredWithin(t, read, "the history's decode"); err != nil {
		t.Fatalf("read and decode the history: %v", err)
	}
	requireServed(t, served)
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the history's read returned, want the test's %d alone", held, filler.bytes)
	}
}

// A transaction's first fork's cover takes its bytes on a grant that holds at most what the fork's
// own read took - a cold read's fold - and nothing that read built (liveWrite.coverFork), so where
// the pool does not give the difference at once it gives the grant back and takes the fork's
// weight whole. A taker queued for more than is free beside the read's grant gets the bytes given
// back, and the cover then takes the fork's.
func TestAFirstForksCoverGivesItsReadsGrantBackRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	const weight = 1 << 20
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, weight)
	write := &liveWrite{slot: &forkSlot{slot: newLoadSlot()}}
	ctx := write.carriedBy(context.Background())
	// What the cold read's fold took on the transaction's grant, which no fork holds yet.
	if err := write.slot.slot.take(ctx, weight/2); err != nil {
		t.Fatalf("take the fold's grant: %v", err)
	}
	served := queueTaker(t, waits, weight-weight/2+1)
	covered := make(chan error, 1)
	go func() { covered <- write.coverFork(ctx, weight) }()
	if err := answeredWithin(t, covered, "the first fork's cover"); err != nil {
		t.Fatalf("cover the first fork: %v", err)
	}
	requireServed(t, served)
	if !write.holdsSlot || write.slot.forks != 1 || write.slot.slot.bytes != weight {
		t.Fatalf("the cover left the write holding a share %t of a grant of %d bytes held by %d forks, want a share of the fork's %d held by one",
			write.holdsSlot, write.slot.slot.bytes, write.slot.forks, int64(weight))
	}
	write.giveFork()
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the fork's share was given back, want the test's %d alone", held, filler.bytes)
	}
}

// joinedTransaction begins a transaction on service's store and joins it (Service.Join), as an
// API write's handler does, and answers the context its operations run with, its ledger, and what
// ends it: the ledger discarded and the transaction rolled back, once, which the test's end runs
// too. A committed transaction is ended the same way.
func joinedTransaction(t *testing.T, ctx context.Context, service *Service) (context.Context, *Ledger, func()) {
	t.Helper()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction: %v", err)
	}
	joined, ledger := service.Join(ctx, tx)
	end := sync.OnceFunc(func() {
		ledger.Discard()
		_ = tx.Rollback(context.Background())
	})
	t.Cleanup(end)
	return joined, ledger, end
}

// requireGrantShort fails the test unless err, what answered, is the pool's refusal of a take it
// does not give at once while its holder holds some (errGrantShort), which the API answers 503
// DOC_SERVICE_UNAVAILABLE (ErrServiceUnavailable).
func requireGrantShort(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, errGrantShort) || !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("%s was answered %v, want it refused busy: %v", what, err, errGrantShort)
	}
}

// residentGrantDocuments creates one document on each of the issues numbered, its stored history
// grantTestState's of the bytes given for it, and loads a room of each, so a write's fork of it
// is the room cloned and weighs what a copy of the room weighs (copyOfRoom). It answers the
// documents and their weights, in the order given.
func residentGrantDocuments(t *testing.T, service *Service, issues []int, bytes []int) ([]string, []int64) {
	t.Helper()
	documents, weights := make([]string, len(issues)), make([]int64, len(issues))
	for index, issue := range issues {
		documents[index] = createIssueDocument(t, service.store, issue, "Written whole by this test.")
		storeHistory(t, service, documents[index], grantTestState(t, bytes[index]))
		if err := loadRoomOf(service, documents[index]); err != nil {
			t.Fatalf("load room %s: %v", documents[index], err)
		}
		weights[index] = service.copyOfRoom(service.srv.GetDoc(documents[index]))
	}
	return documents, weights
}

// grantEdit is the write the tests below make of each document: its heading replaced.
var grantEdit = []model.EditOp{{Op: "replace", Find: grantWriteHeading, With: "Written"}}

// applyGrantEdit makes grantEdit to artifactID in the transaction joined is joined to, on a
// goroutine of its own, and answers what the write answers.
func applyGrantEdit(service *Service, joined context.Context, artifactID string) <-chan error {
	answered := make(chan error, 1)
	go func() {
		_, err := service.ApplyOps(joined, artifactID, grantEdit, grantWriter, nil)
		answered <- err
	}()
	return answered
}

// A transaction's later document takes what its fork weighs past the grant its first document's
// fork holds only where the pool gives it at once (loadSlot.take): refused busy while the pool
// lacks the difference, which leaves both documents as they were, and landed once the pool has it
// free. Before, the cover waited for the difference holding the first fork's grant, so the write
// here was answered by nothing while the pool lacked it.
func TestAJoinedTransactionsSecondForkTakesWhatItWeighsPastTheFirstsAtOnceOrIsRefusedBusy(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, weights := residentGrantDocuments(t, service, []int{31, 32}, []int{16 << 10, 64 << 10})
	first, second := documents[0], documents[1]
	firstWeight, secondWeight := weights[0], weights[1]
	if secondWeight <= firstWeight {
		t.Fatalf("the second room's copy weighs %d and the first's %d, want the second heavier, or nothing here is tested", secondWeight, firstWeight)
	}
	pool := useLoadSlots(t, newLoadSlots(1))
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, first); err != nil {
		t.Fatalf("the transaction's write of the first document: %v", err)
	}
	if held := pool.held.Load(); held != firstWeight {
		t.Fatalf("the pool holds %d bytes once the first document's fork is built, want its %d", held, firstWeight)
	}
	filler := fillPoolBut(t, pool, secondWeight-firstWeight-1)
	err := answeredWithin(t, applyGrantEdit(service, joined, second), "the transaction's write of the second document")
	requireGrantShort(t, err, "the transaction's write of the second document, while the pool lacked one byte of what its fork weighs past the first's")
	if held := pool.held.Load(); held != filler.bytes+firstWeight {
		t.Fatalf("the pool holds %d bytes once the second document's fork was refused, want the first fork's %d beside the test's %d", held, firstWeight, filler.bytes)
	}
	end()
	requireHeading(t, service, first, true)
	requireHeading(t, service, second, true)
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the refused transaction ended, want the test's %d alone", held, filler.bytes)
	}
	filler.release()

	retried, ledger, endRetry := joinedTransaction(t, ctx, service)
	for _, document := range documents {
		if err := <-applyGrantEdit(service, retried, document); err != nil {
			t.Fatalf("the retry's write of %s, the pool free: %v", document, err)
		}
	}
	if held := pool.held.Load(); held != secondWeight {
		t.Fatalf("the pool holds %d bytes once the retry's second fork is built, want the grant raised to its %d", held, secondWeight)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the retry: %v", err)
	}
	endRetry()
	requireHeading(t, service, first, false)
	requireHeading(t, service, second, false)
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the retry committed, want none", held)
	}
}

// Two transactions, each holding its first document's fork and each needing, for a second
// document's fork, more than the pool has free - bytes only the other's grant would free - both
// finish: the first is refused busy at once and rolls back, which frees what the second needs, so
// the second lands, and the first's retry lands after it. Before, a later fork waited for the
// difference holding its first fork's grant: the first queued for bytes the second held, the
// second queued behind it for bytes the first held, and neither was served until this test's
// bound.
func TestTwoJoinedTransactionsEachNeedingWhatTheOthersGrantHoldsBothFinish(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, weights := residentGrantDocuments(t, service, []int{33, 34, 35, 36}, []int{16 << 10, 64 << 10, 16 << 10, 64 << 10})
	// Transaction one writes documents 0 and 1, transaction two documents 2 and 3.
	needOne, needTwo := weights[1]-weights[0], weights[3]-weights[2]
	free := min(needOne, needTwo) - 1
	if needOne <= 0 || needTwo <= 0 || needTwo > free+weights[0] || needOne > free+weights[2] {
		t.Fatalf("the documents weigh %v, so the two transactions do not each need what only the other's grant frees, and nothing here is tested", weights)
	}
	pool, waits := poolWaits(t)
	one, _, endOne := joinedTransaction(t, ctx, service)
	two, ledgerTwo, endTwo := joinedTransaction(t, ctx, service)
	for _, write := range []struct {
		joined   context.Context
		document string
	}{{one, documents[0]}, {two, documents[2]}} {
		if err := <-applyGrantEdit(service, write.joined, write.document); err != nil {
			t.Fatalf("a transaction's write of its first document: %v", err)
		}
	}
	filler := fillPoolBut(t, pool, free)
	for len(waits) > 0 {
		<-waits
	}
	answeredOne := applyGrantEdit(service, one, documents[1])
	var err error
	select {
	case err = <-answeredOne:
	case <-waits:
		// The first transaction queued for bytes the second's grant holds: the second's write of
		// its own second document then queues behind it for bytes the first's holds.
		answeredTwo := applyGrantEdit(service, two, documents[3])
		select {
		case err = <-answeredOne:
		case err = <-answeredTwo:
		case <-time.After(holdAndWaitBound):
			t.Fatalf("neither transaction's write of its second document was answered within %s: each queued for more of the pool holding its first fork's grant, for bytes only the other's grant would free", holdAndWaitBound)
		}
		t.Fatalf("the first transaction's write of its second document queued for more of the pool holding its first fork's grant, and a write was answered (%v) only after", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the first transaction's write of its second document neither waited for the pool nor was answered within %s", holdAndWaitBound)
	}
	requireGrantShort(t, err, "the first transaction's write of its second document, which needs what the second's grant holds")
	endOne()
	if held := pool.held.Load(); held != filler.bytes+weights[2] {
		t.Fatalf("the pool holds %d bytes once the first transaction ended, want the second's %d beside the test's %d", held, weights[2], filler.bytes)
	}
	if err := <-applyGrantEdit(service, two, documents[3]); err != nil {
		t.Fatalf("the second transaction's write of its second document, once the first gave its grant back: %v", err)
	}
	if err := ledgerTwo.Commit(ctx); err != nil {
		t.Fatalf("commit the second transaction: %v", err)
	}
	endTwo()
	requireHeading(t, service, documents[0], true)
	requireHeading(t, service, documents[1], true)
	requireHeading(t, service, documents[2], false)
	requireHeading(t, service, documents[3], false)

	retried, ledger, endRetry := joinedTransaction(t, ctx, service)
	for _, document := range documents[:2] {
		if err := <-applyGrantEdit(service, retried, document); err != nil {
			t.Fatalf("the first transaction's retry of %s: %v", document, err)
		}
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the first transaction's retry: %v", err)
	}
	endRetry()
	requireHeading(t, service, documents[0], false)
	requireHeading(t, service, documents[1], false)
	filler.release()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once both transactions committed, want none", held)
	}
}

// A transaction holding its first document's fork, beside a waiter queued for bytes that fork's
// grant holds, whose second document's fork weighs more past that grant than the pool has free,
// finishes: refused busy at once, since a take on a held grant never queues (loadSlot.take), and
// the waiter is served once the transaction gives its grant back. Before, the cover queued for the
// difference behind the waiter, which waited for the bytes the transaction held, until this test's
// bound.
func TestAJoinedTransactionBesideAWaiterForItsGrantFinishesAndTheWaiterGoesOn(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, weights := residentGrantDocuments(t, service, []int{37, 38}, []int{16 << 10, 64 << 10})
	need := weights[1] - weights[0]
	if need <= 0 {
		t.Fatalf("the documents weigh %v, want the second heavier, or nothing here is tested", weights)
	}
	pool, waits := poolWaits(t)
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, documents[0]); err != nil {
		t.Fatalf("the transaction's write of the first document: %v", err)
	}
	filler := fillPoolBut(t, pool, need-1)
	// The waiter needs more than is free beside the transaction's grant, and no more than is free
	// once that grant is given back.
	served := queueTaker(t, waits, need)
	err := answeredWithin(t, applyGrantEdit(service, joined, documents[1]), "the transaction's write of the second document, beside the waiter")
	requireGrantShort(t, err, "the transaction's write of the second document, beside a waiter for its grant's bytes")
	end()
	requireServed(t, served)
	requireHeading(t, service, documents[0], true)
	requireHeading(t, service, documents[1], true)
	filler.release()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended and the waiter was served, want none", held)
	}
}

// An issue's ask-state write knows its documents before its first fork (WriteIssueAskStates), so
// its first fork takes what both documents' forks weigh, summed (Service.forksAhead,
// liveWrite.coverFork), and the second fork takes nothing more: the pool records one take, of
// both weights, for the whole transaction. Before, the first fork took its own weight and the
// second the difference past it, a second take that a pool short of it refuses busy.
func TestAnIssuesAskStateWriteTakesBothDocumentsWeightsAtItsFirstForkAndNeverAgain(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	_, weights := residentGrantDocuments(t, service, []int{41, 41}, []int{16 << 10, 64 << 10})
	sum := weights[0] + weights[1]
	pool := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	pool.taken = taken.record
	joined, _, end := joinedTransaction(t, ctx, service)
	if _, err := service.WriteIssueAskStates(joined, "DOC-41", nil, grantWriter, true); err != nil {
		t.Fatalf("write the issue's ask states: %v", err)
	}
	if takes := taken.all(); len(takes) != 1 || takes[0] != sum {
		t.Fatalf("the pool recorded takes leaving it holding %v for the transaction's two forks, want one take of both weights, %d and %d, summed: %d", takes, weights[0], weights[1], sum)
	}
	if held := pool.held.Load(); held != sum {
		t.Fatalf("the pool holds %d bytes once both forks are built, want their %d", held, sum)
	}
	end()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended, want none", held)
	}
}

// A read of a stored document under a transaction holding a fork takes on the transaction's
// grant, which Join's context carries (withLoadSlot, transactionGrant): what it weighs past the
// fork's grant, at once, so the pool holds one grant, the transaction's, raised to the read's
// weight, and no second grant is taken. The pool here has free what the read weighs past the
// fork's grant and not the read's whole weight, so a read that took a grant of its own waited for
// it holding the fork's grant, as it did before, until this test's bound.
func TestAStoredReadUnderATransactionTakesOnTheTransactionsGrant(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, weights := residentGrantDocuments(t, service, []int{43}, []int{16 << 10})
	stored := createIssueDocument(t, service.store, 44, "Written whole by this test.")
	state := grantTestState(t, 64<<10)
	storeHistory(t, service, stored, state)
	if service.srv.GetDoc(stored) != nil {
		t.Fatal("a room holds the stored document, so its read would read the room")
	}
	storedWeight := storedCopyWeight(state)
	if storedWeight <= weights[0] {
		t.Fatalf("the stored document weighs %d and the fork %d, want the stored one heavier, or nothing here is tested", storedWeight, weights[0])
	}
	pool, waits := poolWaits(t)
	joined, ledger, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, documents[0]); err != nil {
		t.Fatalf("the transaction's write: %v", err)
	}
	filler := fillPoolBut(t, pool, storedWeight-weights[0])
	read := make(chan error, 1)
	var tree *pmdoc.Node
	go func() {
		var err error
		tree, err = service.storedTree(joined, stored)
		read <- err
	}()
	if err := answeredWithin(t, read, "the stored read under the transaction"); err != nil || tree == nil {
		t.Fatalf("the stored read under the transaction answered %v with a tree %t", err, tree != nil)
	}
	if len(waits) > 0 {
		t.Fatal("a taker waited for the pool during the read: the read took a grant of its own beside the transaction's")
	}
	if grant, held := ledger.forks.slot.bytes, pool.held.Load(); grant != storedWeight || held != filler.bytes+storedWeight {
		t.Fatalf("the transaction's grant holds %d bytes and the pool %d once the stored read returned, want the transaction's one grant raised to the read's %d beside the test's %d",
			grant, held, storedWeight, filler.bytes)
	}
	end()
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the transaction ended, want the test's %d alone", held, filler.bytes)
	}
}

// twoRowDocument creates a document on issue number whose stored history is two rows -
// grantTestState's of bytes and an empty update after it - so a read of it folds them in memory
// under a slot of the buffered folds (foldRead), and answers it with what those rows weigh, which
// the fold takes before it decodes them.
func twoRowDocument(t *testing.T, service *Service, number, bytes int) (string, int64) {
	t.Helper()
	document := createIssueDocument(t, service.store, number, "Written whole by this test.")
	state := grantTestState(t, bytes)
	storeHistory(t, service, document, state)
	if _, err := service.persistence.AppendUpdate(context.Background(), document, emptyUpdate); err != nil {
		t.Fatalf("append the second row of %s: %v", document, err)
	}
	if service.srv.GetDoc(document) != nil {
		t.Fatalf("a room holds %s, so its read would fold nothing", document)
	}
	return document, storedCopyWeight(state, emptyUpdate)
}

// A transaction holding its first document's fork whose cold read of a later document finds every
// slot of the buffered folds held, by a fold waiting for the bytes the transaction holds, is
// refused busy at once (takeFoldSlot, errFoldSlotsBusy), and the fold goes on once the transaction
// gives its grant back. Before, the read waited for the slot holding its grant, and the fold for
// the grant's bytes, until this test's bound.
func TestAJoinedTransactionsColdReadBesideHeldFoldSlotsIsRefusedBusyAndTheFoldGoesOn(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	written, weights := residentGrantDocuments(t, service, []int{45}, []int{16 << 10})
	later, _ := twoRowDocument(t, service, 46, 16<<10)
	folding, foldingWeight := twoRowDocument(t, service, 47, 64<<10)
	versioned, _ := oneFoldSlot(t, service)
	pool, waits := poolWaits(t)
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, written[0]); err != nil {
		t.Fatalf("the transaction's write of its first document: %v", err)
	}
	// The other fold takes the one slot and then waits for its rows' weight, which the pool has
	// free only once the transaction gives its grant back.
	filler := fillPoolBut(t, pool, foldingWeight-1)
	if foldingWeight-1+weights[0] < foldingWeight {
		t.Fatal("the transaction's grant would not free what the other fold waits for, so nothing here is tested")
	}
	folded := foldWaitingForThePool(t, ctx, versioned, folding, waits)
	err := answeredWithin(t, applyGrantEdit(service, joined, later), "the transaction's write of its later document, a cold read beside a held fold slot")
	if !errors.Is(err, errFoldSlotsBusy) || !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("the transaction's cold read beside a held fold slot was answered %v, want it refused busy: %v", err, errFoldSlotsBusy)
	}
	end()
	if err := answeredWithin(t, folded, "the other fold, once the transaction gave its grant back"); err != nil {
		t.Fatalf("the other fold: %v", err)
	}
	requireHeading(t, service, written[0], true)
	filler.release()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended and the fold finished, want none", held)
	}
}

// A transaction holding its first document's fork reads a later document no room holds here, while
// the pool is filled so that a room's load would queue behind a waiter that needs the bytes the
// transaction holds: it loads no room, reads the stored document on its own grant and finishes
// (docTree, transactionHolds), and the waiter goes on once the transaction ends. Before, it loaded
// the room (roomBudget.openServerRoom), whose load took a grant of its own on ygo's load goroutine
// and queued behind the waiter, while the transaction waited for the load, until this test's bound.
func TestAJoinedTransactionsReadOfADocumentNoRoomHoldsLoadsNoRoom(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	written, weights := residentGrantDocuments(t, service, []int{48}, []int{16 << 10})
	stored := createIssueDocument(t, service.store, 49, "Written whole by this test.")
	state := grantTestState(t, 8<<10)
	storeHistory(t, service, stored, state)
	if service.srv.GetDoc(stored) != nil {
		t.Fatal("a room holds the stored document, so its read loads nothing")
	}
	if storedCopyWeight(state) > weights[0] {
		t.Fatalf("the stored document weighs %d, past the fork's %d, so its read would take more and nothing here is tested", storedCopyWeight(state), weights[0])
	}
	pool, waits := poolWaits(t)
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, written[0]); err != nil {
		t.Fatalf("the transaction's write: %v", err)
	}
	filler := fillPoolBut(t, pool, 0)
	served := queueTaker(t, waits, weights[0])
	read := make(chan error, 1)
	var tree *pmdoc.Node
	go func() {
		var err error
		tree, err = service.docTree(joined, stored)
		read <- err
	}()
	if err := answeredWithin(t, read, "the transaction's read of a document no room holds"); err != nil || tree == nil {
		t.Fatalf("the transaction's read of a document no room holds answered %v with a tree %t", err, tree != nil)
	}
	if slices.Contains(service.srv.Rooms(), stored) {
		t.Fatal("the transaction's read loaded a room of the stored document")
	}
	end()
	requireServed(t, served)
	filler.release()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended and the waiter was served, want none", held)
	}
}

// versionedOf answers service's store and its rooms pool, failing the test, with what it cannot
// do, where the service reads through another store.
func versionedOf(t *testing.T, service *Service, cannot string) (*PgVersioned, *pgxpool.Pool) {
	t.Helper()
	versioned, ok := service.persistence.(*PgVersioned)
	if !ok {
		t.Fatalf("the service reads through %T, whose %s", service.persistence, cannot)
	}
	rooms, err := service.store.Pool.Rooms()
	if err != nil {
		t.Fatalf("the rooms pool: %v", err)
	}
	return versioned, rooms
}

// streamEveryFold makes service's store stream every log of several rows rather than read it into
// memory (foldRead, foldBufferLimit), and answers that store and its rooms pool.
func streamEveryFold(t *testing.T, service *Service) (*PgVersioned, *pgxpool.Pool) {
	t.Helper()
	versioned, rooms := versionedOf(t, service, "folds this test cannot make stream")
	versioned.foldBufferLimit = 1
	return versioned, rooms
}

// oneFoldSlot leaves service's store one slot of the buffered folds (foldSlots), so the test can
// hold that slot or have one fold hold it, and answers that store and its rooms pool.
func oneFoldSlot(t *testing.T, service *Service) (*PgVersioned, *pgxpool.Pool) {
	t.Helper()
	versioned, rooms := versionedOf(t, service, "fold slots this test cannot hold")
	versioned.foldSlots = make(chan struct{}, 1)
	return versioned, rooms
}

// foldWaitingForThePool starts versioned's load of folding, the other fold, on a goroutine of its
// own and returns once that fold, holding the one fold slot (oneFoldSlot), waits for the pool
// (poolWaits' waits), failing the test where it finishes first or does not wait within
// holdAndWaitBound. It answers what the load answers.
func foldWaitingForThePool(t *testing.T, ctx context.Context, versioned *PgVersioned, folding string, waits <-chan struct{}) <-chan error {
	t.Helper()
	folded := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, folding)
		folded <- err
	}()
	select {
	case <-waits:
	case err := <-folded:
		t.Fatalf("the other fold finished (%v) while the pool lacked its rows' weight, want it waiting for that holding the fold slot", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the other fold never came to wait for the pool within %s", holdAndWaitBound)
	}
	return folded
}

// A document load whose fold streams its rows waits for its grant of the pod's load concurrency
// holding no connection of the rooms pool: it takes the grant before the transaction that streams
// the rows begins (loadStored). Before, the fold took it inside that transaction, holding its rooms
// connection while it waited.
func TestADocumentLoadsStreamedFoldWaitsForItsGrantHoldingNoRoomsConnection(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	document, _ := twoRowDocument(t, service, 51, 16<<10)
	versioned, rooms := streamEveryFold(t, service)
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, 0)
	loaded := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, document)
		loaded <- err
	}()
	select {
	case <-waits:
	case err := <-loaded:
		t.Fatalf("the load finished (%v) while the pool was full, want it waiting for its grant", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the load never came to wait for the pool within %s", holdAndWaitBound)
	}
	if held := rooms.Stat().AcquiredConns(); held != 0 {
		t.Fatalf("the streamed fold waits for its grant holding %d connections of the rooms pool, want none", held)
	}
	filler.release()
	if err := answeredWithin(t, loaded, "the load, once the pool had room"); err != nil {
		t.Fatalf("the load: %v", err)
	}
}

// A document load whose rows stream gives its slot of the buffered folds back once it knows they
// stream, before it streams them (loadStoredOnce): a streamed fold holds no row in memory, so the
// slot would only keep a buffered fold of another document waiting for the whole of the stream.
// Here the store has one slot and streams every log, and the load's second pass, on its whole
// slot, takes the free fold slot before its rows transaction. Before, the load held that slot
// until its streamed fold and decode had returned.
func TestADocumentLoadWhoseRowsStreamHoldsNoFoldSlotWhileItStreamsThem(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	document, _ := twoRowDocument(t, service, 84, 16<<10)
	versioned, _ := oneFoldSlot(t, service)
	versioned.foldBufferLimit = 1
	held := -1
	versioned.beforeFold = func(room string) {
		if room == document {
			held = len(versioned.foldSlots)
		}
	}
	if _, err := versioned.Load(ctx, document); err != nil {
		t.Fatalf("the load: %v", err)
	}
	switch held {
	case -1:
		t.Fatal("the load never came to stream its rows, so nothing here is tested")
	case 0:
	default:
		t.Fatalf("the load streamed its rows holding %d slots of the buffered folds, want none", held)
	}
}

// A transaction holding its first document's fork, beside a streamed fold that waits for the bytes
// the transaction holds, reads a later document from the store through the rooms pool's one free
// connection and finishes: the fold holds no rooms connection while it waits (loadStored), so the
// read takes that connection, reads the stored log, gives the connection back, and is refused busy
// for the one slot its streamed rows take, which the full pool lacks past the fork's grant. Before,
// the fold waited holding that connection, and the transaction's read waited for it, until this
// test's bound.
func TestAJoinedTransactionsColdReadBesideAStreamedFoldWaitingForItsBytesFinishes(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	written, _ := residentGrantDocuments(t, service, []int{52}, []int{16 << 10})
	streaming, _ := twoRowDocument(t, service, 53, 16<<10)
	later := createIssueDocument(t, service.store, 54, "Written whole by this test.")
	storeHistory(t, service, later, grantTestState(t, 16<<10))
	versioned, rooms := streamEveryFold(t, service)
	pool, waits := poolWaits(t)
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, written[0]); err != nil {
		t.Fatalf("the transaction's write of its first document: %v", err)
	}
	filler := fillPoolBut(t, pool, 0)
	// Every connection of the rooms pool but one is held, so the streamed fold and the
	// transaction's read need the same one.
	for range store.RoomsPoolSize - 1 {
		conn, err := rooms.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a connection of the rooms pool: %v", err)
		}
		t.Cleanup(conn.Release)
	}
	folded := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, streaming)
		folded <- err
	}()
	select {
	case <-waits:
	case err := <-folded:
		t.Fatalf("the streamed fold finished (%v) while the pool was full, want it waiting for the transaction's bytes", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the streamed fold never came to wait for the pool within %s", holdAndWaitBound)
	}
	err := answeredWithin(t, applyGrantEdit(service, joined, later), "the transaction's cold write of its later document, beside the streamed fold")
	requireGrantShort(t, err, "the transaction's cold write of its later document, whose streamed rows take a slot the full pool lacks")
	end()
	filler.release()
	if err := answeredWithin(t, folded, "the streamed fold, once the transaction gave its grant back"); err != nil {
		t.Fatalf("the streamed fold: %v", err)
	}
	requireHeading(t, service, later, true)
}

// A browser's first sync's answer weighs the room once it holds a grant and tops the grant up
// where the room grew while it waited (weighStateAnswer). Nothing of the answer is built yet, so
// where the pool does not give the difference at once the top-up gives the grant back and takes
// the new weight whole, and a taker queued for more than is free beside the first grant gets the
// bytes given back. Here the answer's info was read before the room's last thousand keystrokes.
func TestAStateAnswersTopUpGivesItsGrantBackRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	room := newDocumentCopy()
	text := room.GetText("t")
	for range 1_000 {
		room.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
	}
	info := websocket.StateAnswerInfo{Room: "room", Doc: room}
	before := min(answerWeight(room, 0, 0), loadSlotBytes)
	after := min(answerWeight(room, 0, int64(info.StructsNow())), loadSlotBytes)
	if after <= before {
		t.Fatalf("the answer weighs %d before the room grew and %d after, want more after, or nothing here is tested", before, after)
	}
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, after)
	granted := pauseAtFirstGrant(t, pool)
	answered := make(chan error, 1)
	var release func()
	go func() {
		var err error
		release, err = service.weighStateAnswer(info)
		answered <- err
	}()
	granted.await(t, answered, "the answer's first grant")
	served := queueTaker(t, waits, after-before+1)
	granted.goOn()
	if err := answeredWithin(t, answered, "the answer's top-up"); err != nil {
		t.Fatalf("the answer was refused: %v", err)
	}
	requireServed(t, served)
	if held := pool.held.Load(); held != filler.bytes+after {
		t.Fatalf("the pool holds %d bytes once the answer is admitted, want its %d beside the test's %d", held, after, filler.bytes)
	}
	release()
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the answer was sent, want the test's %d alone", held, filler.bytes)
	}
}

// updatesPastTheStore stores a prose history as artifactID's and answers a room of it that two
// updates have moved past the stored head - a light one, and a heavy one whose apply takes a grant
// (unslottedApplyStructs) - those updates, unstored, and extra, what their merge and its apply
// take on top of the stored document (storedWithUpdates): the merge's weight, or the apply's
// where that is more.
func updatesPastTheStore(t *testing.T, service *Service, artifactID string) (room *crdt.Doc, unstored [][]byte, extra int64) {
	t.Helper()
	state := proseState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	room = copyOf(t, state)
	commit := func(edit func()) []byte {
		t.Helper()
		before := room.StateVector()
		edit()
		return crdt.EncodeStateAsUpdateV1(room, before)
	}
	light := commit(func() { chainedRun(t, room, lastCharacter(t, room), 0x5000, 1) })
	heavy := commit(func() { chainedRun(t, room, lastCharacter(t, room), 0x7e57, unslottedApplyStructs+100) })
	unstored = [][]byte{light, heavy}
	merge := mergeWeight(unstored)
	if merge == 0 {
		t.Fatal("the room's updates merge on no grant, so nothing here is tested")
	}
	merged, err := mergeUpdates(unstored)
	if err != nil {
		t.Fatal(err)
	}
	extra = merge
	if apply, heavy := heavyUpdateWeight(merged); heavy && apply > merge {
		extra = apply
	}
	return room, unstored, extra
}

// loadDocumentAgain is the service's store, stopping at first as its first LoadDocument read of
// room returns, with the document that read decoded still held under the read's grant, and at
// again as its second read of room begins, holding the grant that read is made under and nothing
// that read takes, and counting its reads of room.
type loadDocumentAgain struct {
	VersionedStore
	room         string
	reads        atomic.Int32
	first, again func()
}

func (s *loadDocumentAgain) LoadDocument(ctx context.Context, room string) (LoadedDocument, error) {
	var read int32
	if room == s.room {
		read = s.reads.Add(1)
	}
	if read == 2 {
		s.again()
	}
	loaded, err := s.VersionedStore.LoadDocument(ctx, room)
	if read == 1 {
		s.first()
	}
	return loaded, err
}

// A read of a room behind the stored head holds the stored document it loaded as it takes what the
// merge of the room's unstored updates and its apply decode on top of it (storedWithUpdates), so
// it does not wait for that: where the pool does not give it at once, the read drops the document,
// gives its grant back, takes the total whole and reads again under it (redoWhole). A taker queued
// for more than is free beside the load's grant gets the bytes given back, and the read's second
// pass then holds the total and merges the room's updates into the document it loads.
func TestAStoredReadBehindTheHeadReadsAgainUnderTheWholeGrantRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	room, unstored, extra := updatesPastTheStore(t, service, artifactID)

	pool, waits := poolWaits(t)
	loaded := newPause(t)
	reads := &loadDocumentAgain{VersionedStore: service.persistence, room: artifactID, first: loaded.stop, again: func() {}}
	service.persistence = reads
	read := make(chan error, 1)
	var tree *pmdoc.Node
	go func() {
		var err error
		tree, err = service.storedWithUpdates(ctx, artifactID, unstored)
		read <- err
	}()
	loaded.await(t, read, "the stored read's load")
	load := pool.held.Load()
	if load <= 0 {
		t.Fatal("the stored read's load holds nothing of the pool, so nothing here is tested")
	}
	filler := fillPoolBut(t, pool, extra)
	served := queueTaker(t, waits, extra+1)
	loaded.goOn()
	if err := answeredWithin(t, read, "the stored read"); err != nil {
		t.Fatalf("read the stored document behind the room's updates: %v", err)
	}
	requireServed(t, served)
	if markdown, err := renderTree(tree); err != nil || markdown != docMarkdown(room) {
		t.Fatalf("the read rendered %d bytes (%v), want the %d the room holds", len(markdown), err, len(docMarkdown(room)))
	}
	if got := reads.reads.Load(); got != 2 {
		t.Fatalf("the stored document was loaded %d times, want twice: once before the merge's take was refused, and once more under the whole grant", got)
	}
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the read returned, want the test's %d alone", held, filler.bytes)
	}
	t.Logf("the read's load held %d bytes, its merge and apply %d more, and it read again under the whole grant", load, extra)
}

// A read of a room behind the stored head that is joined to no transaction, whose first pass's
// take was refused and whose later pass holds the total its first pass was refused
// (storedWithUpdates, redoWhole), waits for no slot of the buffered folds while it holds that
// grant: with the one slot held by another document's fold that waits for the bytes the read
// holds, the later pass's load takes no slot, the pool gives it no whole slot at once to stream
// on, and it is refused busy (loadStoredOnce, errFoldSlotsBusy); the fold goes on once the read
// has given its grant back. Before, the later pass waited for the slot holding its grant, with no
// deadline, while the fold waited for the grant's bytes, until this test's bound.
func TestAStoredReadsLaterPassWaitsForNoFoldSlotWhileItHoldsItsGrant(t *testing.T) {
	ctx := context.Background()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	_, unstored, extra := updatesPastTheStore(t, service, artifactID)
	folding, foldingWeight := twoRowDocument(t, service, 58, 64<<10)
	versioned, _ := oneFoldSlot(t, service)

	pool, waits := poolWaits(t)
	first, again := newPause(t), newPause(t)
	service.persistence = &loadDocumentAgain{VersionedStore: versioned, room: artifactID, first: first.stop, again: again.stop}
	read := make(chan error, 1)
	go func() {
		_, err := service.storedWithUpdates(ctx, artifactID, unstored)
		read <- err
	}()
	first.await(t, read, "the stored read's first pass")
	filler := fillPoolBut(t, pool, extra)
	served := queueTaker(t, waits, extra+1)
	first.goOn()
	again.await(t, read, "the stored read's later pass")
	requireServed(t, served)
	// The later pass holds the total its first pass was refused. The other fold takes the one
	// slot and then waits for its rows' weight, which the pool has free only once the read gives
	// its grant back. The signals of the waits before it - the later pass's own wait for its total
	// - are drained first, so the one awaited below is the fold's, sent once it holds the slot.
	if free := pool.poolBytes() - pool.held.Load(); free >= foldingWeight {
		t.Fatalf("the pool has %d bytes free beside the read's later pass, enough for the other fold's %d, so nothing here is tested", free, foldingWeight)
	}
	for len(waits) > 0 {
		<-waits
	}
	folded := foldWaitingForThePool(t, ctx, versioned, folding, waits)
	again.goOn()
	err := answeredWithin(t, read, "the stored read's later pass, beside a fold slot held by a fold waiting for the bytes it holds")
	if !errors.Is(err, errFoldSlotsBusy) || !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("the stored read's later pass beside a held fold slot was answered %v, want it refused busy: %v", err, errFoldSlotsBusy)
	}
	filler.release()
	if err := answeredWithin(t, folded, "the other fold, once the read gave its grant back"); err != nil {
		t.Fatalf("the other fold: %v", err)
	}
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the read was refused and the fold finished, want none", held)
	}
}

// The append gate's check of a history replaced under a room weighs the history's rows before its
// fold decodes any of them and takes their weight whole (storedHistoryHolds), so the fold takes
// nothing more row by row; and it holds the document that history makes as it takes what the run
// onto it costs, so it does not wait for that: where the pool does not give it at once, it drops
// the document, gives its grant back, takes the total whole and decodes the history again under it
// (redoWhole). A taker queued for more than is free beside the rows' grant gets the bytes given
// back, and the check then holds the total and judges the run.
func TestTheGatesCheckOfAReplacedHistoryDecodesAgainUnderTheWholeGrantRatherThanQueueForMoreWhileItHoldsIt(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	rows, run := deletedHistory(deletedCharacters)
	storeRows(t, database, artifactID, rows)
	rowsWeight := storedCopyWeight(rows...)
	runWeight := storedCopyWeight(run)

	pool, waits := poolWaits(t)
	granted := newPause(t)
	var first atomic.Bool
	var firstHeld atomic.Int64
	pool.taken = func(held int64) {
		if first.CompareAndSwap(false, true) {
			firstHeld.Store(held)
			granted.stop()
		}
	}
	checked := make(chan error, 1)
	var holds bool
	go func() {
		var err error
		holds, err = storedHistoryHolds(ctx, database.Pool, artifactID, int64(len(rows)), []BrowserUpdate{{Update: run, ContentChanged: true}}, maxUpdateItems)
		checked <- err
	}()
	granted.await(t, checked, "the gate's check")
	if got := firstHeld.Load(); got != rowsWeight {
		t.Fatalf("the gate's first grant left %d bytes held, want the %d the history's rows weigh, taken before its fold decodes any of them", got, rowsWeight)
	}
	filler := fillPoolBut(t, pool, runWeight)
	served := queueTaker(t, waits, runWeight+1)
	granted.goOn()
	if err := answeredWithin(t, checked, "the gate's check"); err != nil {
		t.Fatalf("the gate's check: %v", err)
	}
	if !holds {
		t.Fatal("the gate's check found the history does not hold the run, which builds on its clocks alone")
	}
	requireServed(t, served)
	if held := pool.held.Load(); held != filler.bytes {
		t.Fatalf("the pool holds %d bytes once the check returned, want the test's %d alone", held, filler.bytes)
	}
}

// A whole copy of a resident room weighed (copyOfRoom) as its take begins to wait for the pool, and
// made once that wait is over, is covered by a grant weighed again once the wait is over: what the
// room committed meanwhile is in the copy, so the grant takes it, at once here since the pool has
// room by then. Each site below waits for the full pool while a browser's commit lands in the
// room, and the grant held as the copy is made covers the room as the copy found it: a write's warm
// fork, whose clone weighs the room again (forkPoint); a server write's decode-cap count
// (leavesPastDecodeCap); and the check of a live room near its caps the rebuild route and a refused
// apply's offer of a rebuild make (nearACapOfRoom). Before, each copied the room under the weight
// it had before the wait, short by what the commit added.
func TestACopyOfARoomWeighedBeforeItsWaitIsCoveredAsTheRoomStandsOnceTheWaitIsOver(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, _ := residentGrantDocuments(t, service, []int{71, 72, 73}, []int{16 << 10, 16 << 10, 16 << 10})
	for index, site := range []struct {
		name string
		// copy starts the copy of live on a goroutine of its own and answers what the copy
		// answers, with held, the bytes of the pool its grant held as the copy was made, read
		// once the copy has answered; pool is the test's pool, its fill given back by then.
		copy func(t *testing.T, pool *loadSlots, document string, live *crdt.Doc) (answered <-chan error, held func() int64)
	}{
		{"a write's warm fork, a clone of the room's replica", func(t *testing.T, _ *loadSlots, document string, live *crdt.Doc) (<-chan error, func() int64) {
			write := &liveWrite{artifactID: document, clientID: crdt.NewClientID(), state: newRoomState(), slot: &forkSlot{slot: newLoadSlot()}}
			t.Cleanup(write.slot.slot.release)
			answered := make(chan error, 1)
			go func() {
				_, err := service.warmFork(write.carriedBy(context.Background()), write, live)
				answered <- err
			}()
			return answered, func() int64 { return write.slot.slot.bytes }
		}},
		{"a server write's decode-cap count, a decode of the room's state", func(t *testing.T, pool *loadSlots, _ string, live *crdt.Doc) (<-chan error, func() int64) {
			var held int64
			count := serverWrite{apply: func(*crdt.Doc) { held = pool.held.Load() }}
			answered := make(chan error, 1)
			go func() {
				service.leavesPastDecodeCap(context.Background(), live, count)
				answered <- nil
			}()
			return answered, func() int64 { return held }
		}},
		{"the check of a live room near its caps, an encode and decode of the room's state", func(t *testing.T, _ *loadSlots, _ string, live *crdt.Doc) (<-chan error, func() int64) {
			return startNearACapCheck(t, service, live)
		}},
	} {
		t.Run(site.name, func(t *testing.T) {
			document := documents[index]
			live := service.srv.GetDoc(document)
			if live == nil {
				t.Fatalf("no room of %s is resident", document)
			}
			before := service.copyOfRoom(live)
			pool, waits := poolWaits(t)
			filler := fillPoolBut(t, pool, before-1)
			answered, held := site.copy(t, pool, document, live)
			after := commitWhileTheCopyWaits(t, service, live, waits, answered, site.name, before)
			filler.release()
			if err := answeredWithin(t, answered, site.name); err != nil {
				t.Fatalf("%s: %v", site.name, err)
			}
			if got := held(); got < after {
				t.Fatalf("%s held %d bytes of the pool as it copied the room, want the %d the room weighs once its wait was over: it was weighed at %d before the wait, short of the commit it copied",
					site.name, got, after, before)
			}
		})
	}
}

// A whole copy of a resident room weighed (copyOfRoom) as its take begins to wait for the pool,
// whose room grows by a browser's commit during that wait, is refused busy where the pool gives the
// take what it weighed but not what the room grew: the copy weighs the room again once the wait is
// over, takes the difference only at once (loadSlot.take), and is otherwise answered errGrantShort,
// the 503 its caller retries, rather than made under a grant short of the room it copies. A write's
// warm fork clones nothing and is refused errWriteBusy (forkPoint), and the rebuild route's check of
// a live room near its caps reads nothing, which RebuildDocument answers ErrServiceUnavailable, the
// route's 503 DOC_SERVICE_UNAVAILABLE (nearACapOfRoom). Each keeps the grant its wait took as it
// was, and neither fails the room. The test before this one makes the same wait where the pool has
// the difference. With either take of the difference taken out, the copy is made under the first
// weight and answers no refusal.
func TestACopyOfARoomThatGrewWhileItWaitedIsRefusedBusyWhereThePoolLacksTheGrowth(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, _ := residentGrantDocuments(t, service, []int{75, 76}, []int{16 << 10, 16 << 10})
	for index, site := range []struct {
		name string
		// copy starts the copy of live on a goroutine of its own and answers what the copy
		// answers, with held, the bytes its grant holds, read once the copy has answered.
		copy func(t *testing.T, document string, live *crdt.Doc) (answered <-chan error, held func() int64)
		// refused is what the refusal wraps beside errGrantShort, nil for nothing more.
		refused error
	}{
		{"a write's warm fork, a clone of the room's replica", func(t *testing.T, document string, live *crdt.Doc) (<-chan error, func() int64) {
			write := &liveWrite{artifactID: document, clientID: crdt.NewClientID(), state: newRoomState(), slot: &forkSlot{slot: newLoadSlot()}}
			t.Cleanup(write.slot.slot.release)
			answered := make(chan error, 1)
			go func() {
				fork, err := service.warmFork(write.carriedBy(context.Background()), write, live)
				if err == nil && fork == nil {
					err = errors.New("the warm fork answered no fork and no error")
				}
				answered <- err
			}()
			return answered, func() int64 { return write.slot.slot.bytes }
		}, errWriteBusy},
		{"the rebuild route's check of a live room near its caps", func(t *testing.T, _ string, live *crdt.Doc) (<-chan error, func() int64) {
			return startNearACapCheck(t, service, live)
		}, nil},
	} {
		t.Run(site.name, func(t *testing.T) {
			document := documents[index]
			live := service.srv.GetDoc(document)
			if live == nil {
				t.Fatalf("no room of %s is resident", document)
			}
			before := service.copyOfRoom(live)
			pool, waits := poolWaits(t)
			filler := fillPoolBut(t, pool, before-1)
			// Once armed, the first grant the pool gives - the copy's, of the room's weight before
			// its wait - has every byte left free taken at once, so none of what the room grew
			// meanwhile is free when the copy weighs the room again.
			var armed atomic.Bool
			var rest *loadSlot
			pool.taken = func(int64) {
				if !armed.CompareAndSwap(true, false) {
					return
				}
				taker := newLoadSlot()
				if taker.tryTake(pool.poolBytes() - pool.held.Load()) {
					rest = taker
				}
			}
			answered, held := site.copy(t, document, live)
			commitWhileTheCopyWaits(t, service, live, waits, answered, site.name, before)
			armed.Store(true)
			filler.release()
			err := answeredWithin(t, answered, site.name)
			if rest == nil {
				t.Fatalf("the pool's free bytes were not taken once %s's wait was over (it answered %v), so the pool had what the room grew and nothing here is tested", site.name, err)
			}
			t.Cleanup(rest.release)
			requireGrantShort(t, err, site.name+", whose room grew by more than the pool had free once its wait was over")
			if site.refused != nil && !errors.Is(err, site.refused) {
				t.Fatalf("%s was answered %v, want it to wrap %v", site.name, err, site.refused)
			}
			if got := held(); got != before {
				t.Fatalf("%s's grant holds %d bytes once it was refused, want the %d its wait took, as it was", site.name, got, before)
			}
			if total := pool.held.Load(); total != rest.bytes+before {
				t.Fatalf("the pool holds %d bytes once %s was refused, want the %d its grant holds beside the test's %d: the refusal took part of the room's growth", total, site.name, before, rest.bytes)
			}
			if service.roomFailed(document) {
				t.Fatalf("%s's refusal failed the room, whose document is sound", site.name)
			}
		})
	}
}

// startNearACapCheck starts service's check of live near its caps (nearACapOfRoom) on a goroutine
// of its own, under a grant of its own, and answers what the check answers, with a read of the
// bytes its grant holds, made once the check has answered.
func startNearACapCheck(t *testing.T, service *Service, live *crdt.Doc) (<-chan error, func() int64) {
	t.Helper()
	slot := newLoadSlot()
	t.Cleanup(slot.release)
	answered := make(chan error, 1)
	go func() {
		_, _, err := service.nearACapOfRoom(slot.carriedBy(context.Background()), live, service.roomWeighingOf(live).roomStateBytes())
		answered <- err
	}()
	return answered, func() int64 { return slot.bytes }
}

// commitWhileTheCopyWaits waits for name, the copy that answers on answered, to wait for the pool
// (poolWaits' waits), failing the test where it finishes first or does not wait within
// holdAndWaitBound; then lands a browser's commit in live, and answers what the room weighs once
// the commit is in (copyOfRoom), failing the test where that is no more than before.
func commitWhileTheCopyWaits(t *testing.T, service *Service, live *crdt.Doc, waits <-chan struct{}, answered <-chan error, name string, before int64) (after int64) {
	t.Helper()
	select {
	case <-waits:
	case err := <-answered:
		t.Fatalf("%s finished (%v) while the pool lacked the room's weight, want it waiting for that", name, err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("%s never came to wait for the pool within %s", name, holdAndWaitBound)
	}
	// A browser's commit lands in the room while the copy's take waits.
	if _, applied := newRoomCommitter(t, live).commitKeys(200); <-applied != nil {
		t.Fatal("the commit to the room was refused")
	}
	after = service.copyOfRoom(live)
	if after <= before {
		t.Fatalf("the room weighs %d after the commit and %d before it, want more after, or nothing here is tested", after, before)
	}
	return after
}

// A server write's decode-cap count (leavesPastDecodeCap) whose top-up waits a second time weighs
// the room again once that wait is over, and copies the room on a grant covering it as it stands
// then: what the room committed during the second wait is in the copy. Here the count's first
// grant is held while a browser's commit lands in the room and the rest of the pool stays filled,
// so the top-up gives the grant back and waits for the room's new weight whole, and a second
// commit lands during that wait. Before, the count copied the room on the grant its second
// weighing took, short of the room by the second commit.
func TestAServerWritesDecodeCapCountWeighsTheRoomAgainAfterEachWait(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, _ := residentGrantDocuments(t, service, []int{74}, []int{16 << 10})
	live := service.srv.GetDoc(documents[0])
	if live == nil {
		t.Fatalf("no room of %s is resident", documents[0])
	}
	commit := func(what string) int64 {
		t.Helper()
		before := service.copyOfRoom(live)
		if _, applied := newRoomCommitter(t, live).commitKeys(200); <-applied != nil {
			t.Fatalf("%s to the room was refused", what)
		}
		after := service.copyOfRoom(live)
		if after <= before {
			t.Fatalf("the room weighs %d after %s and %d before it, want more after, or nothing here is tested", after, what, before)
		}
		return after
	}
	first := service.copyOfRoom(live)
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, first)
	granted := pauseAtFirstGrant(t, pool)
	var held int64
	count := serverWrite{apply: func(*crdt.Doc) { held = pool.held.Load() }}
	answered := make(chan error, 1)
	go func() {
		service.leavesPastDecodeCap(context.Background(), live, count)
		answered <- nil
	}()
	granted.await(t, answered, "the count's first grant")
	second := commit("the first commit")
	for len(waits) > 0 {
		<-waits
	}
	granted.goOn()
	select {
	case <-waits:
	case <-answered:
		t.Fatal("the count copied the room while the pool lacked the room's new weight, want its top-up waiting for that")
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the count's top-up never came to wait for the pool within %s", holdAndWaitBound)
	}
	third := commit("the commit during the second wait")
	filler.release()
	if err := answeredWithin(t, answered, "the count"); err != nil {
		t.Fatalf("the count: %v", err)
	}
	if held < third {
		t.Fatalf("the count held %d bytes of the pool as it copied the room, want the %d the room weighs once its second wait was over: its grant was weighed at %d before that wait, short of the commit it copied",
			held, third, second)
	}
}

// A browser's first sync's answer whose top-up waits a second time weighs the room again once that
// wait is over, and is admitted on a grant covering the room as it stands then (weighStateAnswer):
// what the room committed during the second wait is in the answer ygo builds next. Here the
// answer's info was read before the room's first thousand keystrokes, its first grant is held
// while they land and the rest of the pool stays filled, so the top-up gives the grant back and
// waits for the new weight whole, and the room's next thousand keystrokes land during that wait.
// Before, the answer was admitted on the grant its second weighing took, short of the room by
// those keystrokes.
func TestAStateAnswerWeighsTheRoomAgainAfterEachWait(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	room := newDocumentCopy()
	text := room.GetText("t")
	info := websocket.StateAnswerInfo{Room: "room", Doc: room, Structs: room.StructCount()}
	weight := func() int64 { return min(answerWeight(room, 0, int64(info.StructsNow())), loadSlotBytes) }
	typeKeys := func(what string) int64 {
		t.Helper()
		before := weight()
		for range 1_000 {
			room.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "x", nil) })
		}
		after := weight()
		if after <= before {
			t.Fatalf("the answer weighs %d after %s and %d before them, want more after, or nothing here is tested", after, what, before)
		}
		return after
	}
	first := weight()
	pool, waits := poolWaits(t)
	filler := fillPoolBut(t, pool, first)
	granted := pauseAtFirstGrant(t, pool)
	answered := make(chan error, 1)
	var release func()
	go func() {
		var err error
		release, err = service.weighStateAnswer(info)
		answered <- err
	}()
	granted.await(t, answered, "the answer's first grant")
	second := typeKeys("the first thousand keystrokes")
	for len(waits) > 0 {
		<-waits
	}
	granted.goOn()
	select {
	case <-waits:
	case err := <-answered:
		t.Fatalf("the answer was admitted (%v) while the pool lacked the room's new weight, want its top-up waiting for that", err)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the answer's top-up never came to wait for the pool within %s", holdAndWaitBound)
	}
	third := typeKeys("the keystrokes during the second wait")
	filler.release()
	if err := answeredWithin(t, answered, "the answer"); err != nil {
		t.Fatalf("the answer was refused: %v", err)
	}
	defer release()
	if held := pool.held.Load(); held < third {
		t.Fatalf("the answer was admitted holding %d bytes of the pool, want the %d the room weighs once its second wait was over: its grant was weighed at %d before that wait, short of the keystrokes it answers with",
			held, third, second)
	}
}

// A write's decode-cap count copies the room the write's fork was read from, on the write's own
// grant, which holds the fork and cannot be given back while the operation runs: where the copy
// weighs more than the grant holds and the pool does not give the difference at once, the count
// refuses the write busy, the 503 its caller retries, and takes nothing (leavesRoomPastDecodeCap).
// Before, it waited for the difference without a deadline while the write held its fork. The room
// here stands within a thousand structs of the cap and the write adds two thousand, so only the
// copy answers; the room is no room this service loaded, so its copy weighs a whole slot
// (copyOfRoom), and the pool lacks one byte of what that weighs past the fork's grant.
func TestAWritesDecodeCapCountIsRefusedBusyRatherThanQueueForMoreWhileItHoldsItsFork(t *testing.T) {
	skipAtTheBoundsUnderRace(t)
	const written = 2_000
	live := typedRoom(t, maxUpdateItems-written/2, 0)
	// The write's fork as its operations left it, here only what they added, so all its structs
	// are the transaction's (forkStructs zero, updatesAdd).
	fork := newDocumentCopy()
	text := fork.GetText("t")
	fork.Transact(func(txn *crdt.Transaction) {
		for range written {
			text.Insert(txn, 0, "w", nil)
		}
	})
	pool := useLoadSlots(t, newLoadSlots(1))
	write := &liveWrite{clientID: crdt.NewClientID(), forkedFrom: weak.Make(live), slot: &forkSlot{slot: newLoadSlot(), forks: 1}, holdsSlot: true}
	if _, decided := decodeCapWithoutCopy(live, serverWrite{adds: write.updatesAdd(fork), bounded: true}); decided {
		t.Fatal("the room's own total answers the decode cap without a copy, so nothing here is tested")
	}
	service := calibratedService()
	copied := service.copyOfRoom(live)
	ctx := write.carriedBy(context.Background())
	const forkGrant = 1 << 20
	if err := write.slot.slot.take(ctx, forkGrant); err != nil {
		t.Fatalf("take the fork's grant: %v", err)
	}
	filler := fillPoolBut(t, pool, copied-forkGrant-1)
	refused := make(chan error, 1)
	go func() { refused <- service.refuseUndecodable(ctx, write, fork, nil) }()
	err := answeredWithin(t, refused, "the write's decode-cap count")
	if !errors.Is(err, ErrServiceUnavailable) || !strings.Contains(err.Error(), "a write holding its fork waits for no more of the pool") {
		t.Fatalf("the write's decode-cap count answered %v, want it refused busy (ErrServiceUnavailable) naming the wait it will not make", err)
	}
	if held := write.slot.slot.bytes; held != forkGrant {
		t.Fatalf("the write's grant holds %d bytes once its count was refused, want the fork's %d as it was", held, int64(forkGrant))
	}
	if held := pool.held.Load(); held != filler.bytes+forkGrant {
		t.Fatalf("the pool holds %d bytes once the count was refused, want the fork's %d beside the test's %d", held, int64(forkGrant), filler.bytes)
	}
	write.slot.slot.release()
	runtime.KeepAlive(live)
	t.Logf("the count of a copy weighing %d beside a fork's grant of %d was refused: %v", copied, int64(forkGrant), err)
}

// refusedWithoutPanic is what call answers, failing the test, named by what, where call panics
// instead.
func refusedWithoutPanic(t *testing.T, what string, call func() error) error {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked rather than answer: %v", what, recovered)
		}
	}()
	return call()
}

// A write's fork decode that weighs more than the grant its cover took takes the difference only
// where the pool gives it at once, and is refused busy otherwise, errWriteBusy wrapping
// errGrantShort, the 503 its caller retries, leaving the grant as it was (buildForkHeld): a warm
// fork's decode of the copy its read took of a room no replica stood behind (Service.forkPoint),
// weighed on that copy where its cover was weighed on the room before a commit grew it, and a
// cold fork's decode of the stored history on a grant its write already holds its share of, as a
// rebuilt fork's cover finds it (rebuildFork). Neither fails the room, since the document is
// sound. Before, the decode's hold dropped the refusal and handed back no release at all, and the
// write panicked as the decode returned and called it.
func TestAForkDecodeWeighingPastItsCoverIsRefusedBusyAndFailsNoRoom(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, artifactID, state)
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("a room holds the document, so its cold fork would fork the room")
	}
	// A document no room's load listed: the warm fork's read finds no replica to clone, and
	// decodes the copy it takes of the document itself (atPoint).
	unlisted := copyOf(t, state)
	for _, site := range []struct {
		name   string
		weight int64
		fork   func(ctx context.Context, write *liveWrite) error
	}{
		{"a warm fork's decode of the copy its read took", storedCopyWeight(crdt.EncodeStateAsUpdateV1(unlisted, nil)), func(ctx context.Context, write *liveWrite) error {
			_, _, _, err := service.forkPoint(ctx, artifactID, unlisted, write.clientID, nil)
			return err
		}},
		{"a cold fork's decode of the stored history", storedCopyWeight(state), func(ctx context.Context, write *liveWrite) error {
			_, err := service.coldFork(ctx, write, 0)
			return err
		}},
	} {
		t.Run(site.name, func(t *testing.T) {
			pool := useLoadSlots(t, newLoadSlots(1))
			// The write holds its share of its transaction's grant, which its cover took before
			// the decode, at half what the decode weighs, and the pool has none of the rest free.
			write := &liveWrite{artifactID: artifactID, clientID: crdt.NewClientID(), slot: &forkSlot{slot: newLoadSlot(), forks: 1}, holdsSlot: true}
			ctx := write.carriedBy(context.Background())
			cover := site.weight / 2
			if err := write.slot.slot.take(ctx, cover); err != nil {
				t.Fatalf("take the cover's grant: %v", err)
			}
			filler := fillPoolBut(t, pool, 0)
			err := refusedWithoutPanic(t, site.name, func() error { return site.fork(ctx, write) })
			if !errors.Is(err, errWriteBusy) {
				t.Fatalf("%s was answered %v, want the write refused busy: %v", site.name, err, errWriteBusy)
			}
			requireGrantShort(t, err, site.name)
			if held := write.slot.slot.bytes; held != cover {
				t.Fatalf("the write's grant holds %d bytes once %s was refused, want the cover's %d as it was", held, site.name, cover)
			}
			if held := pool.held.Load(); held != filler.bytes+cover {
				t.Fatalf("the pool holds %d bytes once %s was refused, want the cover's %d beside the test's %d", held, site.name, cover, filler.bytes)
			}
			if service.roomFailed(artifactID) {
				t.Fatalf("%s's refusal failed the room, whose document is sound", site.name)
			}
			write.slot.slot.release()
		})
	}
}

// An issue's ask-state write whose later document is a cold log its load streams - past the
// store's fold buffer, for which the load takes one whole slot before it reads a row (loadStored)
// - weighs that document at the whole slot before its first fork (forkWeight), so its first fork
// takes the slot and the later fork's read and decode take nothing more: the write lands with a
// taker queued for the pool behind it, and the pool records one take for the whole transaction.
// Before, that log was weighed on its rows' bytes and structs, below the slot its load takes, so
// the later fork's load took the difference on the held grant, which a taker waiting in the pool's
// queue refuses at once, and the write was refused busy though no document grew. The store here
// streams every log of more than a byte (streamEveryFold), as it streams one past foldBufferBytes,
// so a small log stands for one past it.
func TestAnIssuesAskStateWriteWeighsAColdLogItsLoadStreamsAtAWholeSlot(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents := []string{
		createIssueDocument(t, service.store, 56, "Written whole by this test."),
		createIssueDocument(t, service.store, 56, "Written whole by this test."),
	}
	// The write forks the issue's documents in the order of their ids (issueDocumentIDs).
	slices.Sort(documents)
	resident, streamed := documents[0], documents[1]
	storeHistory(t, service, resident, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, resident); err != nil {
		t.Fatalf("load room %s: %v", resident, err)
	}
	state := grantTestState(t, 16<<10)
	storeHistory(t, service, streamed, state)
	if service.srv.GetDoc(streamed) != nil {
		t.Fatal("a room holds the later document, so its fork would read the room")
	}
	if rows := storedCopyWeight(state); rows >= loadSlotBytes {
		t.Fatalf("the later document's rows weigh %d, a whole slot, so nothing here is tested", rows)
	}
	streamEveryFold(t, service)
	pool, waits := poolWaits(t)
	var taken slotsHeld
	pool.taken = taken.record
	read := newPause(t)
	var first atomic.Bool
	service.afterForkRead = func(room string) {
		if room == resident && first.CompareAndSwap(false, true) {
			read.stop()
		}
	}
	joined, _, end := joinedTransaction(t, ctx, service)
	written := make(chan error, 1)
	go func() {
		_, err := service.WriteIssueAskStates(joined, "DOC-56", nil, grantWriter, true)
		written <- err
	}()
	read.await(t, written, "the write's first fork")
	// A taker of a whole slot waits behind the write's first fork, whatever that fork took, so
	// a later take on the write's grant finds the pool's queue taken.
	served := queueTaker(t, waits, loadSlotBytes)
	read.goOn()
	if err := answeredWithin(t, written, "the issue's ask-state write"); err != nil {
		t.Fatalf("the issue's ask-state write, with a taker queued for the pool behind its first fork, was answered %v: its later document took more than it was weighed at", err)
	}
	if takes := taken.all(); len(takes) != 1 || takes[0] != loadSlotBytes {
		t.Fatalf("the pool recorded takes leaving it holding %v for the transaction's two forks, want one take of the whole slot the streamed log's load takes", takes)
	}
	end()
	requireServed(t, served)
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended and the taker was served, want none", held)
	}
}

// A heavy browser update refused for its scans, in a room whose scan total is used up, gives its
// grant of the pod's load concurrency back before the hook judges whether the rebuild route takes
// the room (refusedByApply, rebuildTakes), whose re-scan waits for a grant of its own of a whole
// copy of the room. The one slot here has free beside the apply's grant one byte less than that
// copy, and a taker holding nothing queues for a whole copy while the apply holds its grant: the
// refusal is answered at once, offering the rebuild the route takes (the room's state re-scans
// near the total), and the taker is served. Before, the re-scan queued for its copy behind the
// taker, which waited for the apply's grant, and the hook gave that grant back only as it
// returned: the refusal waited out rebuildTakes's WriteWaitBound, answered DOC_TOO_TANGLED, and
// held the taker as long.
func TestARefusedHeavyApplyGivesItsGrantBackBeforeItsRefusalReadsTheRoom(t *testing.T) {
	state := tangledHistory(t, 2_000, roomScanTotal)
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	service := calibratedService()
	replica := service.keepReplica(room, nil, loadRendering(rendering), nil)
	defer runtime.KeepAlive(replica)
	loaded, err := rescan(state)
	if err != nil {
		t.Fatal(err)
	}
	replica.scans.rescanned(loaded)
	if _, left := replica.scans.figures(); left > usedUpScanBudget {
		t.Fatalf("the tangled history re-scans %d items, leaving %d of the %d total: tangle it further", loaded, left, roomScanTotal)
	}
	const typist crdt.ClientID = 5
	typeOnto(t, service, room, typist, 30)
	// More items at the typed place than an apply takes no slot for, each scanning past the typing:
	// a frame heavy in its structs, which the used-up room refuses for its scans.
	frame := scanningFrame(10, crdt.ID{Client: typist}, unslottedApplyStructs+1)
	grant, heavy := heavyUpdateWeight(frame)
	if !heavy {
		t.Fatalf("a frame of %d items is not heavy, so its apply takes no grant and nothing here is tested", unslottedApplyStructs+1)
	}
	copied := service.copyOfRoom(room)
	pool, waits := poolWaits(t)
	// The pool has free beside the apply's grant one byte less than the copy the re-scan takes. A
	// copy weighed at a whole slot needs no filling: the grant and it cannot both fit.
	if leave := grant + copied - 1; leave < pool.poolBytes() {
		fillPoolBut(t, pool, leave)
	}
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	served := make(chan struct{})
	answered := make(chan error, 1)
	started := time.Now()
	go func() {
		answered <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: frame}, func() error {
			// While the apply holds its grant, a taker holding nothing queues for a whole copy of
			// the room, which the pool has free only once that grant is back.
			go func() {
				taker := newLoadSlot()
				if err := taker.take(context.Background(), copied); err == nil {
					taker.release()
				}
				close(served)
			}()
			select {
			case <-waits:
			case <-time.After(holdAndWaitBound):
				return errors.New("the taker never queued for the pool beside the apply's grant")
			}
			return crdt.ApplyUpdateV1(room, frame, nil)
		})
	}()
	err = answeredWithin(t, answered, "the refusal of a heavy update in a used-up room")
	if took := time.Since(started); took >= WriteWaitBound {
		t.Fatalf("the refusal was answered after %s, past rebuildTakes's %s: its re-scan waited for a grant of its own while the apply held its grant", took, WriteWaitBound)
	}
	requireRefusedNeedsRebuild(t, err, room, before)
	requireServed(t, served)
}

// A document load whose rows fold in memory waits for its slot of the buffered folds holding no
// connection of the rooms pool: it takes the slot before the transaction that reads the rows
// (loadStored, takeFoldSlot), and folds once the slot is free. Before, it began that transaction
// and waited for the slot inside it, holding its rooms connection.
func TestADocumentLoadsBufferedFoldWaitsForItsFoldSlotHoldingNoRoomsConnection(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	document, _ := twoRowDocument(t, service, 57, 16<<10)
	versioned, rooms := oneFoldSlot(t, service)
	versioned.foldSlots <- struct{}{}
	loaded := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, document)
		loaded <- err
	}()
	if testwait.Holds(time.Second, testwait.Poll, func() bool { return rooms.Stat().AcquiredConns() != 0 || len(loaded) != 0 }) {
		select {
		case err := <-loaded:
			t.Fatalf("the load finished (%v) while the only fold slot was held, want it waiting for that slot", err)
		default:
		}
		// The load is let go before the test fails, so it gives its rooms connection back and the
		// database's teardown waits on no open transaction.
		held := rooms.Stat().AcquiredConns()
		<-versioned.foldSlots
		<-loaded
		t.Fatalf("the buffered load waits for its fold slot holding %d connections of the rooms pool, want none", held)
	}
	<-versioned.foldSlots
	if err := answeredWithin(t, loaded, "the load, once its fold slot was free"); err != nil {
		t.Fatalf("the load: %v", err)
	}
}

// A transaction holding its first document's fork, beside a buffered load waiting for the one slot
// of the buffered folds, whose holder - another load's fold - waits for the bytes the transaction
// holds, finishes: its cold read of a later document is refused busy at once for that slot
// (takeFoldSlot, errFoldSlotsBusy), and once it gives its grant back the fold and the waiting load
// go on. The waiting load holds no rooms connection while it waits (loadStored). Before, it held
// one: with the rest of the rooms pool held, the transaction's cold read waited for that
// connection, held by a load that waited for the slot, whose holder waited for the transaction's
// bytes, until this test's bound.
func TestAJoinedTransactionsColdReadBesideALoadWaitingForAFoldSlotFinishes(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	written, weights := residentGrantDocuments(t, service, []int{58}, []int{16 << 10})
	later, _ := twoRowDocument(t, service, 59, 16<<10)
	folding, foldingWeight := twoRowDocument(t, service, 60, 64<<10)
	waiting, _ := twoRowDocument(t, service, 61, 16<<10)
	versioned, rooms := oneFoldSlot(t, service)
	pool, waits := poolWaits(t)
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, written[0]); err != nil {
		t.Fatalf("the transaction's write of its first document: %v", err)
	}
	// The other fold takes the one slot and then waits for its rows' weight, which the pool has
	// free only once the transaction gives its grant back.
	filler := fillPoolBut(t, pool, foldingWeight-1)
	if foldingWeight-1+weights[0] < foldingWeight {
		t.Fatal("the transaction's grant would not free what the other fold waits for, so nothing here is tested")
	}
	folded := foldWaitingForThePool(t, ctx, versioned, folding, waits)
	// Every connection of the rooms pool but one is held, so the waiting load and the
	// transaction's cold read need the same one.
	for range store.RoomsPoolSize - 1 {
		conn, err := rooms.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a connection of the rooms pool: %v", err)
		}
		t.Cleanup(conn.Release)
	}
	loadedWaiting := make(chan error, 1)
	go func() {
		_, err := versioned.Load(ctx, waiting)
		loadedWaiting <- err
	}()
	// The waiting load reaches its wait for the slot before the transaction reads: a load that took
	// the rooms pool's last connection before that wait holds it by then.
	waitFor(t, holdAndWaitBound, "the waiting load to wait for the fold slot", func() bool { return foldSlotWaiters() == 1 })
	err := answeredWithin(t, applyGrantEdit(service, joined, later), "the transaction's write of its later document, beside a load waiting for the fold slot")
	if !errors.Is(err, errFoldSlotsBusy) || !errors.Is(err, ErrServiceUnavailable) {
		t.Fatalf("the transaction's cold read beside a load waiting for the held fold slot was answered %v, want it refused busy: %v", err, errFoldSlotsBusy)
	}
	end()
	if err := answeredWithin(t, folded, "the other fold, once the transaction gave its grant back"); err != nil {
		t.Fatalf("the other fold: %v", err)
	}
	if err := answeredWithin(t, loadedWaiting, "the waiting load, once the fold slot was free"); err != nil {
		t.Fatalf("the waiting load: %v", err)
	}
	requireHeading(t, service, written[0], true)
	filler.release()
	if held := pool.held.Load(); held != 0 {
		t.Fatalf("the pool holds %d bytes once the transaction ended and both loads finished, want none", held)
	}
}

// A transaction holding its first document's fork that applies to another document's room it
// found resident - a version's capture of it, or a mark's watch (captureLiveTextAndAuthors,
// VerifyMark) - is answered busy at once where that room leaves between ygo's look at whether it
// is resident (allowApply) and ygo's lookup of it, whichever takes it out: ygo's idle sweep, which
// retires a room no peer and no caller has held for the idle timeout, or a close the service makes
// (Evict). ygo hands the room's load the Apply's context, and the load refuses it before it records
// or reads anything (servicePersistenceAdapter.LoadDocContext, errRoomLoadUnderGrant), the 503 its
// caller retries. The rest of the pool is held beside the transaction's grant, so a load that ran
// on the transaction's goroutine would wait for bytes nothing gives back while the transaction
// holds its own, and answer nothing. The refusal fails no room: once the transaction has ended, a
// write holding no grant lands on the document. Before, the load ran on that goroutine under a
// grant of its own, which it waited for, and a close the service makes waited for the Apply
// instead (roomServer.Apply's hold of the close gate), which was handed the room it found.
func TestATransactionsApplyToARoomThatLeftAfterItsLookIsRefusedBusyAtTheLoad(t *testing.T) {
	// goneBound bounds the room's going once the Apply has passed allowApply: the idle sweep runs
	// every second, and a close of a room no caller is inside closes it at once.
	const goneBound = 20 * time.Second
	// refusalBound bounds the Apply's answer once the room has gone: the refusal takes no lock and
	// reads nothing, and a load waiting for the pool beside the transaction's grant has answered
	// nothing by then.
	const refusalBound = 10 * time.Second
	for _, route := range []struct {
		name string
		// idleTimeout is the server's room idle timeout. ygo's sweeper reads it when the first room
		// starts it, so it is set before any room loads.
		idleTimeout time.Duration
		// takeOut starts the room's going, from inside allowApply on the Apply's own goroutine,
		// and answers what reports the end of the close it began, or nil where it began none.
		takeOut func(service *Service, room string) <-chan error
	}{
		{"ygo's idle sweep retires it", 2 * time.Second, func(*Service, string) <-chan error { return nil }},
		{"a close the service makes closes it", time.Hour, func(service *Service, room string) <-chan error {
			closed := make(chan error, 1)
			go func() { closed <- service.Evict(context.Background(), room) }()
			return closed
		}},
	} {
		t.Run(route.name, func(t *testing.T) {
			ctx := context.Background()
			service, _ := newTestService(t)
			service.hooks = newTestHooks()
			service.settle = time.Hour
			service.srv.RoomIdleTimeout = route.idleTimeout
			documents, _ := residentGrantDocuments(t, service, []int{62, 63}, []int{16 << 10, 8 << 10})
			written, other := documents[0], documents[1]
			pool := useLoadSlots(t, newLoadSlots(1))
			joined, _, end := joinedTransaction(t, ctx, service)
			if err := <-applyGrantEdit(service, joined, written); err != nil {
				t.Fatalf("the transaction's write of its first document: %v", err)
			}
			if !transactionHolds(joined) {
				t.Fatal("the transaction's grant holds nothing once its fork stands, so nothing here is tested")
			}
			// The room is looked up once more here, which stamps it idle from now, so the idle
			// sweep retires it no sooner than the idle timeout on, and the Apply below finds it.
			if err := loadRoomOf(service, other); err != nil {
				t.Fatalf("look up the room of %s: %v", other, err)
			}
			if service.srv.GetDoc(other) == nil {
				t.Fatalf("no room of %s is resident, so the transaction's Apply would be refused before ygo looks it up", other)
			}
			record, _ := service.roomLoads.Load(other)
			filler := fillPoolBut(t, pool, 0)

			var reached atomic.Bool
			var closed <-chan error
			gone := make(chan error, 1)
			service.afterApplyAllowed = func(room string) {
				if room != other || !reached.CompareAndSwap(false, true) {
					return
				}
				closed = route.takeOut(service, room)
				if !testwait.Holds(goneBound, testwait.Poll, func() bool { return service.srv.GetDoc(room) == nil }) {
					gone <- fmt.Errorf("the room did not go within %s while the transaction's Apply stood between ygo's look and its lookup: something held it there", goneBound)
					return
				}
				gone <- nil
			}
			var applyErr error
			applied := make(chan struct{})
			go func() {
				defer close(applied)
				applyErr = service.srv.Apply(joined, other, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
			}()
			// letGo gives the pool back and waits for the Apply and for the close it began, so no load
			// or close runs past the test's end, and answers the close's error. It runs once, after
			// the hook has run or where it never will.
			letGo := func() (closeErr error) {
				filler.release()
				select {
				case <-applied:
				case <-time.After(holdAndWaitBound):
					t.Errorf("the transaction's Apply did not end within %s once the pool was free", holdAndWaitBound)
				}
				if closed == nil {
					return nil
				}
				select {
				case closeErr = <-closed:
				case <-time.After(holdAndWaitBound):
					t.Errorf("the close did not end within %s once the pool was free", holdAndWaitBound)
				}
				return closeErr
			}
			select {
			case err := <-gone:
				if err != nil {
					letGo()
					t.Fatal(err)
				}
			case <-applied:
				t.Fatalf("the transaction's Apply was answered %v without reaching allowApply's end, so nothing here is tested", applyErr)
			case <-time.After(goneBound + holdAndWaitBound):
				letGo()
				t.Fatalf("the transaction's Apply did not reach allowApply's end within %s", goneBound+holdAndWaitBound)
			}
			select {
			case <-applied:
			case <-time.After(refusalBound):
				letGo()
				t.Fatalf("the transaction's Apply was not answered within %s once %s: the room's load ran on its goroutine and waited for the pool while the transaction held its grant", refusalBound, route.name)
			}
			if !errors.Is(applyErr, errRoomLoadUnderGrant) || !errors.Is(applyErr, ErrServiceUnavailable) || errors.Is(applyErr, websocket.ErrInjectRefused) {
				letGo()
				t.Fatalf("the transaction's Apply, once %s after ygo's look, was answered %v, want the room's load refused busy: %v", route.name, applyErr, errRoomLoadUnderGrant)
			}
			if current, recorded := service.roomLoads.Load(other); recorded && current != record {
				letGo()
				t.Fatal("a load of the room began on the transaction's goroutine")
			}
			end()
			if err := letGo(); err != nil {
				t.Fatalf("the close: %v", err)
			}
			if failure := service.roomFailure(other); failure != nil {
				t.Fatalf("the room is failed once its load was refused busy: %v", failure)
			}
			counters, err := service.RoomCounters(other)
			if err != nil {
				t.Fatalf("read the room's counters: %v", err)
			}
			if counters.RoomFailures != 0 {
				t.Fatalf("the document's room failed %d times, want none: a busy refusal of its load is not the room's failure", counters.RoomFailures)
			}
			if err := grantWrite(ctx, service, other, grantWriteHeading, "Written"); err != nil {
				t.Fatalf("a write holding no grant, once the transaction ended: %v", err)
			}
			requireHeading(t, service, other, false)
		})
	}
}

// A room's load whose context carries no transaction whose grant holds bytes loads the room as
// before: ygo hands the load its caller's context (servicePersistenceAdapter.LoadDocContext), and
// only a grant holding bytes refuses it. An Apply under no transaction, and one under a
// transaction whose grant holds nothing yet - it has forked and read nothing - each load the room
// of a document no room holds.
func TestARoomsLoadUnderNoGrantOrAGrantHoldingNothingLoadsTheRoom(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	alone := createIssueDocument(t, service.store, 64, "Loaded under no transaction.")
	underTx := createIssueDocument(t, service.store, 65, "Loaded under a transaction holding nothing.")
	for _, document := range []string{alone, underTx} {
		storeHistory(t, service, document, grantTestState(t, 1<<10))
	}
	if err := loadRoomOf(service, alone); err != nil {
		t.Fatalf("load the room of a document under no transaction: %v", err)
	}
	if service.srv.GetDoc(alone) == nil {
		t.Fatal("an Apply under no transaction left no room of its document resident")
	}
	joined, _, _ := joinedTransaction(t, ctx, service)
	if transactionHolds(joined) {
		t.Fatal("the transaction's grant holds bytes before it forked or read anything, so nothing here is tested")
	}
	err := service.srv.Apply(joined, underTx, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("an Apply under a transaction whose grant holds nothing: %v", err)
	}
	if service.srv.GetDoc(underTx) == nil {
		t.Fatal("an Apply under a transaction whose grant holds nothing left no room of its document resident")
	}
}

// roomLoadWaiters is how many goroutines wait on a room's load another caller began, read off the
// goroutine dump (parkedIn), since nothing reports that a caller has reached that wait: parked in
// ygo's getOrCreateRoom, on the select over the loading room's ready barrier.
func roomLoadWaiters() int {
	return parkedIn([]string{"[select"}, "(*Server).getOrCreateRoom(")
}

// refusedLoadUnderGrant writes written in a transaction it joins, so the transaction's grant holds
// that fork, and starts the transaction's Apply to other's resident room. A close the service
// makes takes that room out between allowApply's look and ygo's lookup, so the Apply makes the
// room's placeholder, and the room's load is held at its refusal (beforeRoomLoadRefused) under the
// transaction's grant (servicePersistenceAdapter.LoadDocContext, errRoomLoadUnderGrant). It
// returns once the load is held there and answers what ends the transaction, and letGo, which lets
// the refusal go on and fails the test unless the Apply then ends refused busy.
func refusedLoadUnderGrant(t *testing.T, ctx context.Context, service *Service, written, other string) (func(), func()) {
	t.Helper()
	joined, _, end := joinedTransaction(t, ctx, service)
	if err := <-applyGrantEdit(service, joined, written); err != nil {
		t.Fatalf("the transaction's write of its first document: %v", err)
	}
	if !transactionHolds(joined) {
		t.Fatal("the transaction's grant holds nothing once its fork stands, so nothing here is tested")
	}
	if service.srv.GetDoc(other) == nil {
		t.Fatalf("no room of %s is resident, so the transaction's Apply would be refused before ygo looks it up", other)
	}

	var closedOnce atomic.Bool
	service.afterApplyAllowed = func(room string) {
		if room != other || !closedOnce.CompareAndSwap(false, true) {
			return
		}
		if err := service.Evict(context.Background(), room); err != nil {
			t.Errorf("close the room between the transaction's look and ygo's lookup: %v", err)
		}
	}
	refusing := make(chan struct{})
	refuse := sync.OnceFunc(func() { close(refusing) })
	release := make(chan struct{})
	letRefusalGo := sync.OnceFunc(func() { close(release) })
	t.Cleanup(letRefusalGo)
	service.beforeRoomLoadRefused = func(room string) {
		if room != other {
			return
		}
		refuse()
		<-release
	}
	var applyErr error
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		applyErr = service.srv.Apply(joined, other, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	}()
	select {
	case <-refusing:
	case <-applied:
		t.Fatalf("the transaction's Apply was answered %v without its load refusing itself, so nothing here is tested", applyErr)
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the transaction's load did not reach its refusal within %s", holdAndWaitBound)
	}
	letGo := func() {
		t.Helper()
		letRefusalGo()
		select {
		case <-applied:
		case <-time.After(holdAndWaitBound):
			t.Fatalf("the transaction's Apply did not end within %s once its load's refusal went on", holdAndWaitBound)
		}
		if !errors.Is(applyErr, errRoomLoadUnderGrant) {
			t.Fatalf("the transaction's Apply was answered %v, want its load refused busy: %v", applyErr, errRoomLoadUnderGrant)
		}
	}
	return end, letGo
}

// requireSettlementCommits runs room's settlement now (SettleNow) and fails the test unless a run
// of it commits within holdAndWaitBound, which clears the document's pending-settlement row; what
// names that run.
func requireSettlementCommits(t *testing.T, ctx context.Context, service *Service, room, what string) {
	t.Helper()
	service.SettleNow(room)
	waitFor(t, holdAndWaitBound, what, func() bool {
		owes, err := settlementPending(ctx, service.store.Pool, room)
		return err == nil && !owes
	})
}

// A settlement whose warm-up waits on a room's load that a transaction holding its grant began
// meets that load's refusal (servicePersistenceAdapter.LoadDocContext, errRoomLoadUnderGrant),
// which is no fault of the room's or the document's: the settlement is armed again and counted no
// failure, as one the room budget had no opening for is (settleNoRoom), and the document's
// pending-settlement row stands until the settlement's next run commits. The transaction's Apply
// finds the room it looked at gone - a close the service makes takes it out between allowApply's
// look and ygo's lookup - and makes the room's placeholder, and its load is held at its refusal
// (beforeRoomLoadRefused) until the settlement's warm-up waits on that placeholder. Before, the
// warm-up's refusal stopped the settlement short as a fault (settleRetry) and was counted toward
// maxSettleFailures, so three such meetings failed a healthy room.
func TestASettlementWhoseWarmUpMeetsALoadRefusedUnderAGrantCountsNoFailure(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, _ := residentGrantDocuments(t, service, []int{66, 67}, []int{16 << 10, 8 << 10})
	written, other := documents[0], documents[1]
	// The document owes a settlement: a write lands on it, which leaves its pending-settlement row.
	if err := grantWrite(ctx, service, other, grantWriteHeading, "Written"); err != nil {
		t.Fatalf("the write that leaves the document owing a settlement: %v", err)
	}
	if owes, err := settlementPending(ctx, service.store.Pool, other); err != nil || !owes {
		t.Fatalf("the written document owes a settlement: %t (%v), want true", owes, err)
	}
	end, letGo := refusedLoadUnderGrant(t, ctx, service, written, other)

	settled := make(chan struct{})
	go func() {
		defer close(settled)
		settleNow(service, other)
	}()
	waitFor(t, holdAndWaitBound, "the settlement's warm-up to wait on the load the transaction began", func() bool { return roomLoadWaiters() == 1 })
	letGo()
	select {
	case <-settled:
	case <-time.After(holdAndWaitBound):
		t.Fatalf("the settlement did not end within %s once the load it waited on was refused", holdAndWaitBound)
	}

	state := service.lockExistingState(other)
	if state == nil {
		t.Fatal("the service holds no record of the document once its settlement ended, so nothing arms it again")
	}
	failures, armed := state.settleFailures, state.settle != nil
	service.unlockState(other, state)
	if failures != 0 {
		t.Fatalf("the settlement counted %d failures once its warm-up met a load refused under another transaction's grant, want none: the refusal was that transaction's load's, not the room's", failures)
	}
	if !armed {
		t.Fatal("the settlement is not armed again once its warm-up met the refused load")
	}
	if failure := service.roomFailure(other); failure != nil {
		t.Fatalf("the room is failed once a settlement's warm-up met the refused load: %v", failure)
	}
	if owes, err := settlementPending(ctx, service.store.Pool, other); err != nil || !owes {
		t.Fatalf("the document owes a settlement once its warm-up met the refused load: %t (%v), want true", owes, err)
	}

	end()
	requireSettlementCommits(t, ctx, service, other, "the settlement's next run to commit")
}

// A committed write whose publish waits on a room's load that a transaction holding its grant
// began finds no room to publish into once that load refuses itself (errRoomLoadUnderGrant), and
// ends as a publish that found none does (publishedWithoutARoom): the write's text is judged on
// the stored document, so its lost-edit verdict is reached (Ledger.LostOps), the document's
// settlement is armed as the room's update observer would have armed it, and no room fails. The
// transaction's Apply finds the room it looked at gone - a close the service makes takes it out
// between allowApply's look and ygo's lookup - and makes the room's placeholder, which the write's
// publish finds listed (roomHeld), and its load is held at its refusal (beforeRoomLoadRefused)
// until the publish waits on it. Before, the publish logged the refusal as an error and returned,
// with no verdict on the write's text and no settlement armed for it, which then waited for the
// room's next load or the resumption.
func TestAPublishWaitingOnALoadRefusedUnderAGrantEndsAsOneThatFoundNoRoom(t *testing.T) {
	ctx := context.Background()
	service, _ := newTestService(t)
	service.settle = time.Hour
	documents, _ := residentGrantDocuments(t, service, []int{68, 69}, []int{16 << 10, 8 << 10})
	written, other := documents[0], documents[1]
	end, letGo := refusedLoadUnderGrant(t, ctx, service, written, other)

	// Another transaction writes the document: it forks the stored document, since no room of it
	// is published (readFork), and its commit publishes, holding no grant by then (Ledger.publish),
	// into the placeholder ygo lists.
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the writing transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	writeCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)
	if _, err := service.ApplyOps(writeCtx, other, grantEdit, grantWriter, nil); err != nil {
		t.Fatalf("the write to the document whose load the transaction holds at its refusal: %v", err)
	}
	committed := make(chan error, 1)
	go func() { committed <- ledger.Commit(ctx) }()
	waitFor(t, holdAndWaitBound, "the write's publish to wait on the load the transaction began", func() bool { return roomLoadWaiters() == 1 })
	letGo()
	if err := answeredWithin(t, committed, "the write's commit and publish, once the load its publish waited on was refused"); err != nil {
		t.Fatalf("the write's commit: %v", err)
	}

	lost, judged := ledger.LostOps(other)
	if !judged {
		t.Fatal("the write's lost-edit verdict was not reached: its publish ended on the refused load without judging the write's text on the stored document")
	}
	if len(lost) != 0 {
		t.Fatalf("the write's lost-edit verdict names operations %v as lost, want none: nothing else wrote the document", lost)
	}
	state := service.lockExistingState(other)
	if state == nil {
		t.Fatal("the service holds no record of the document once its write was published, so no settlement is armed for it")
	}
	armed := state.settle != nil
	service.unlockState(other, state)
	if !armed {
		t.Fatal("no settlement is armed for the published write once its publish met the refused load")
	}
	if failure := service.roomFailure(other); failure != nil {
		t.Fatalf("the room is failed once a publish met the refused load: %v", failure)
	}
	if owes, err := settlementPending(ctx, service.store.Pool, other); err != nil || !owes {
		t.Fatalf("the written document owes a settlement: %t (%v), want true", owes, err)
	}

	end()
	requireSettlementCommits(t, ctx, service, other, "the settlement the publish armed to commit")
	requireHeading(t, service, other, false)
}
