package docs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// This file tests a heavy browser apply's grant of the pod's load concurrency given back once the
// apply has integrated and passed its check, before its commit's observers run
// (Service.weighPeerUpdate, crdt.ApplyBudget.Integrated), and the waits of a room's persistence
// worker that the grant, held through the commit, closed into a cycle. One of the commit's
// observers hands the update to the worker through a queue of 256 updates and waits while it is
// full, or for the worker's exit once the worker has begun to exit (ygo's persistStranded), so a
// worker that waited for the pool while the apply held it waited for itself. The grant goes back
// before that observer runs, so each wait ends. Each test holds the pool full with that apply's
// grant in it and fails by its timeout with the hook's use reverted.

// heavyRoomUpdate is an update a browser could send live, a room's document: a chain of
// chainStructs characters under the margin (marginChain), declaring more structs than an apply
// takes no slot for, and the grant the peer update hook takes for its apply (copyWeight). The
// commit the room makes of it declares the same structs, so its stored copy weighs that grant too.
func heavyRoomUpdate(t *testing.T, live *crdt.Doc) (update []byte, grant int64) {
	t.Helper()
	writer := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatalf("copy the room for the heavy update: %v", err)
	}
	update = marginChain(t, writer, chainStructs)
	requireWeighedByItsStructs(t, update)
	return update, copyWeight(int64(len(update)), int64(declaredStructs(update)))
}

// applyHeavy hands update to the peer update hook for live, artifactID's room, on a goroutine of
// its own, as ygo hands a browser's frame to the hook on that browser's read loop, with the apply
// ygo would run, and answers what the hook returned.
func applyHeavy(service *Service, artifactID string, live *crdt.Doc, update []byte) <-chan error {
	answered := make(chan error, 1)
	origin := new(int)
	go func() {
		answered <- service.weighPeerUpdate(websocket.PeerUpdateInfo{Room: artifactID, Doc: live, Update: update, Origin: origin},
			func() error { return crdt.ApplyUpdateV1(live, update, origin) })
	}()
	return answered
}

// fillPoolBut takes every byte of pool but leave, and gives them back as the test ends, before
// its service shuts down.
func fillPoolBut(t *testing.T, pool *loadSlots, leave int64) {
	t.Helper()
	filler := newLoadSlot()
	if take := pool.poolBytes() - pool.held.Load() - leave; !filler.tryTake(take) {
		t.Fatalf("the pool refused %d of its %d bytes with %d held", take, pool.poolBytes(), pool.held.Load())
	}
	t.Cleanup(filler.release)
}

// observerGate is the first commit of a room to reach the room's update observer once the gate
// is armed (beforeObserveUpdate): the apply has integrated and passed its check, the document is
// released, and the commit's persistence observer, which ygo runs after the room's own, has not
// run. reached closes as that commit arrives there; release lets a held commit go on.
type observerGate struct {
	reached chan struct{}
	release func()
}

// holdNextObserver arms an observerGate on artifactID's room, holding the commit there until
// released where hold is set, and releasing it as the test ends in any case.
func holdNextObserver(t *testing.T, service *Service, artifactID string, hold bool) *observerGate {
	t.Helper()
	gate := &observerGate{reached: make(chan struct{})}
	proceed := make(chan struct{})
	gate.release = sync.OnceFunc(func() { close(proceed) })
	var first atomic.Bool
	service.beforeObserveUpdate = func(room string) {
		if room != artifactID || !first.CompareAndSwap(false, true) {
			return
		}
		close(gate.reached)
		if hold {
			<-proceed
		}
	}
	t.Cleanup(gate.release)
	return gate
}

// holdNextAppend holds artifactID's persistence worker at its next store of a browser's update
// (beforeRoomAppend), before the store decodes or takes anything, until release; reached closes
// once it is held. The worker reads nothing of the room's queue of updates to store meanwhile. It
// is released as the test ends in any case.
func holdNextAppend(t *testing.T, service *Service, artifactID string) (reached <-chan struct{}, release func()) {
	t.Helper()
	held := make(chan struct{})
	proceed := make(chan struct{})
	release = sync.OnceFunc(func() { close(proceed) })
	var first atomic.Bool
	service.beforeRoomAppend = func(room string) error {
		if room == artifactID && first.CompareAndSwap(false, true) {
			close(held)
			<-proceed
		}
		return nil
	}
	t.Cleanup(release)
	return held, release
}

// roomCommitter commits margin keys to a room's document as a remote peer's updates, each a
// commit the room's persistence observer hands to the room's worker. made is how many keys it
// has set, and commits how many commits it has made.
type roomCommitter struct {
	live    *crdt.Doc
	writer  *crdt.Doc
	made    int
	commits int
}

func newRoomCommitter(t *testing.T, live *crdt.Doc) *roomCommitter {
	t.Helper()
	writer := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(live, nil), nil); err != nil {
		t.Fatalf("copy the room for its committer: %v", err)
	}
	return &roomCommitter{live: live, writer: writer}
}

// commit makes one commit of one margin key (commitKeys).
func (c *roomCommitter) commit() <-chan error {
	_, applied := c.commitKeys(1)
	return applied
}

// commitKeys makes one commit setting keys margin keys on a goroutine of its own, and answers
// its update and, once its observers have run, the persistence observer's hand-over to the worker
// among them, the apply's error. Each key is a struct of its own, which no other merges into.
func (c *roomCommitter) commitKeys(keys int) (update []byte, applied <-chan error) {
	held := c.writer.StateVector()
	margin := c.writer.GetMap(marksMapName)
	c.writer.Transact(func(txn *crdt.Transaction) {
		for range keys {
			margin.Set(txn, fmt.Sprintf("queued-%d", c.made), crdt.NewTextPrelim())
			c.made++
		}
	})
	update = crdt.EncodeStateAsUpdateV1(c.writer, held)
	c.commits++
	answered := make(chan error, 1)
	go func() { answered <- crdt.ApplyUpdateV1(c.live, update, "peer") }()
	return update, answered
}

// queueFullWait is how long one commit's persistence observer may take to hand its update to the
// room's worker before fill takes the room's queue to be full: a hand-over to a queue with room
// takes no time at all, and a commit's own work, a margin key on a room of a paragraph, is a
// millisecond or so.
const queueFullWait = 2 * time.Second

// maxQueuedCommits is how many commits fill makes before it gives up on the queue filling: four
// times ygo's 256.
const maxQueuedCommits = 1024

// fill commits until one commit's hand-over to the worker has waited queueFullWait: the room's
// queue of updates to store (ygo's persistCh, 256 updates) is then full, and every later commit's
// hand-over waits for the worker as well. The worker must already be held where it reads nothing
// of the queue. The commit that waits stays waiting on its goroutine until the worker reads the
// queue again.
func (c *roomCommitter) fill(t *testing.T) {
	t.Helper()
	for range maxQueuedCommits {
		select {
		case err := <-c.commit():
			if err != nil {
				t.Fatalf("commit %d to the room: %v", c.commits, err)
			}
		case <-time.After(queueFullWait):
			t.Logf("commit %d waited %s to hand its update to the room's worker: the room's queue of updates to store is full", c.commits, queueFullWait)
			return
		}
	}
	t.Fatalf("%d commits handed their updates over while the room's worker read nothing of its queue", maxQueuedCommits)
}

// awaitHeavyApply waits for the heavy apply's commit to reach observer, failing the test where
// the hook answers first or it never comes.
func awaitHeavyApply(t *testing.T, observer *observerGate, heavy <-chan error) {
	t.Helper()
	select {
	case <-observer.reached:
	case err := <-heavy:
		t.Fatalf("the heavy apply returned (%v) before its commit reached the room's observer", err)
	case <-time.After(roomWait(t)):
		t.Fatal("the heavy apply's commit never reached the room's observer")
	}
}

// requireAnswered waits until deadline for what answered sends, failing the test with what names
// the wait where nothing comes by then, and fails it where the answer is an error.
func requireAnswered(t *testing.T, answered <-chan error, deadline <-chan time.Time, what string, wait time.Duration, waited *atomic.Bool) {
	t.Helper()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-deadline:
		t.Fatalf("%s did not finish within %s while a heavy apply's grant filled the pool (a taker began to wait for the pool: %t)",
			what, wait, waited.Load())
	}
}

// loadedRoom loads artifactID's room holding text once every append of it has landed, and makes
// the heavy update for it (heavyRoomUpdate).
func loadedRoom(t *testing.T, service *Service, artifactID, text string) (live *crdt.Doc, update []byte, grant int64) {
	t.Helper()
	seedServiceText(t, service, artifactID, text)
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	waitForLandedAppends(t, service, artifactID)
	live = roomDocument(t, service, artifactID)
	update, grant = heavyRoomUpdate(t, live)
	return live, update, grant
}

// poolOfOneSlot gives the process a load concurrency of one slot for the rest of t, whose takers
// call waiting as they begin to wait, set before any taker can reach it, and reports whether one
// has begun to wait.
func poolOfOneSlot(t *testing.T, waiting func()) (*loadSlots, *atomic.Bool) {
	t.Helper()
	slots := newLoadSlots(1)
	waited := new(atomic.Bool)
	slots.waiting = func() {
		waited.Store(true)
		if waiting != nil {
			waiting()
		}
	}
	return useLoadSlots(t, slots), waited
}

// holdWorkerBehindAFullQueue holds artifactID's persistence worker at its store of a commit made
// to live, the room's document, setting firstKeys margin keys (roomCommitter.commitKeys), before
// that store decodes or takes anything (holdNextAppend), and fills the room's queue of updates to
// store behind it (roomCommitter.fill), so the next commit's hand-over to the worker waits until
// release lets the worker go on. It answers the held commit's update too.
func holdWorkerBehindAFullQueue(t *testing.T, service *Service, artifactID string, live *crdt.Doc, firstKeys int) (first []byte, release func()) {
	t.Helper()
	appending, release := holdNextAppend(t, service, artifactID)
	queue := newRoomCommitter(t, live)
	first, applied := queue.commitKeys(firstKeys)
	select {
	case <-appending:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's worker never came to store the first commit")
	}
	if err := <-applied; err != nil {
		t.Fatalf("the first commit: %v", err)
	}
	queue.fill(t)
	return first, release
}

// heapGivenBackTolerance is how far the live heap a heavy apply leaves behind it as its commit's
// first observer runs may stand past what the room budget charges the room for that commit: what
// the process allocates beside the room between the two readings - the test's own bookkeeping,
// the scheduler's and the database driver's buffers - which no room budget counts. A mebibyte;
// the readings this test logs stand millions of bytes inside the charge.
const heapGivenBackTolerance = 1 << 20

// A heavy browser apply holds its grant of the pod's load concurrency while it decodes and
// integrates the update and its check renders what it left, and gives it back once the apply
// has integrated and passed that check, before its commit's first observer runs
// (Service.weighPeerUpdate, crdt.ApplyBudget.Integrated): the heap it gave back is the transient
// heap of the decode and the check, and what the apply leaves is the room's, which the room budget
// charges. So as the commit's first observer runs, the pool holds nothing of the apply's, the
// room's charge stands where it stood before the apply (the release moves nothing of it), and the
// live heap, read the way liveHeap does, has grown by no more than the charge the room's observer
// then adds for the commit, with heapGivenBackTolerance beside it. Before, the grant was given back
// only as the hook returned, after every observer, one of which waits while the room's queue of
// updates to store is full.
func TestAHeavyApplysGrantGoesBackBeforeItsCommitsObserversAndWhatItLeavesIsTheRooms(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // no settlement takes a grant or charges the room beside the apply
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	charge := service.roomWeighingOf(live).roomCharge()
	if charge == nil {
		t.Fatal("the loaded room holds no charge in the room budget")
	}
	pool := useLoadSlots(t, newLoadSlots(1))
	var taken slotsHeld
	pool.taken = taken.record

	type reading struct {
		held, charged int64
		heap          uint64
	}
	var atObserver reading
	var observed atomic.Bool
	service.beforeObserveUpdate = func(room string) {
		if room == artifactID && observed.CompareAndSwap(false, true) {
			atObserver = reading{held: pool.held.Load(), charged: chargedWeight(charge), heap: liveHeap()}
		}
	}
	charged := chargedWeight(charge)
	heap := liveHeap()
	select {
	case err := <-applyHeavy(service, artifactID, live, update):
		if err != nil {
			t.Fatalf("the hook answered %v, want the heavy update applied", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("the heavy apply did not return")
	}
	if !observed.Load() {
		t.Fatal("the heavy apply's commit never reached the room's observer")
	}
	chargedAfter := chargedWeight(charge)
	grown := int64(atObserver.heap) - int64(heap)
	t.Logf("the apply took %d bytes of the pool; at its commit's first observer the pool held %d, the room's charge stood at %d (%d before the apply, %d once its observers had run, %+d), and the live heap at %d (%d before, %+d against the charge's %+d and a tolerance of %d)",
		grant, atObserver.held, atObserver.charged, charged, chargedAfter, chargedAfter-charged,
		atObserver.heap, heap, grown, chargedAfter-charged, heapGivenBackTolerance)

	if grants := taken.all(); len(grants) == 0 || grants[0] != grant {
		t.Fatalf("the pool's grants left %v held, want the apply's own %d first (copyWeight)", grants, grant)
	}
	if atObserver.held != 0 {
		t.Fatalf("the pool held %d bytes as the commit's first observer ran, want none: the apply held its %d-byte grant through its commit", atObserver.held, grant)
	}
	if atObserver.charged != charged {
		t.Fatalf("the room's charge stood at %d as the commit's first observer ran, want the %d it stood at before the apply: giving the grant back moved the room's charge", atObserver.charged, charged)
	}
	if chargedAfter <= charged {
		t.Fatalf("the room's charge stood at %d once the apply had returned, want past the %d before it: the room charges the structs the apply left", chargedAfter, charged)
	}
	if grown > chargedAfter-charged+heapGivenBackTolerance {
		t.Fatalf("the live heap grew by %d bytes by the commit's first observer, past the %d the room's charge grew by and %d beside it: the heap the apply gave back was not transient",
			grown, chargedAfter-charged, heapGivenBackTolerance)
	}
}

// heldCommitKeys is how many margin keys the commit cycle (i)'s worker is held at sets: each key a
// struct of its own, so the commit declares more structs than an update may and still be stored
// with no grant (heavyUpdateWeight), and its read-back waits for the pool, while what it takes
// stays within the heavy apply's grant, which declares chainStructs.
const heldCommitKeys = 2 * unslottedApplyStructs

// Cycle (i). The room's persistence worker decodes each heavy update it stores into a fresh copy
// before it stores it (StoreUpdateStored, holdServerLoad, heavyUpdateWeight), waiting for its bytes
// of the pod's load concurrency. A heavy apply's commit hands its update to that worker through the
// room's queue of updates to store and waits while the queue is full. With the queue full behind a
// heavy commit the worker holds, and the pool full but for the apply's own grant, the worker's
// decode of that commit waits for the pool, and the room stores it, and the apply returns, because
// the apply gave its grant back before its commit's observers ran. Held through the commit, the
// grant is what the decode waited for while the apply waited for the decode's worker. The decode
// took only free bytes before (holdFreeLoad), which held no apply and worker against each other
// but let it run beside a full pool counted by nothing. A lighter commit's decode takes nothing,
// so the worker the test holds holds a heavy one.
func TestTheWorkersReadBackWaitsForAPoolAHeavyApplyGaveBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)
	before := storedHead(t, service.store, artifactID)
	first, releaseWorker := holdWorkerBehindAFullQueue(t, service, artifactID, live, heldCommitKeys)
	if weight, heavy := heavyUpdateWeight(first); !heavy || weight > grant {
		t.Fatalf("the commit the worker holds is %d bytes declaring %d structs, weighed at %d (heavy: %t), want a heavy update weighed within the apply's %d-byte grant",
			len(first), declaredStructs(first), weight, heavy, grant)
	}
	observer := holdNextObserver(t, service, artifactID, false)
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)

	// The worker goes on to decode the first commit's update with every byte of the pool held
	// but what the apply gave back, if it did.
	releaseWorker()
	wait := min(roomWait(t), besideAFullPoolWait)
	end := time.Now().Add(wait)
	requireAnswered(t, heavy, time.After(time.Until(end)), "the heavy apply", wait, waited)
	waitFor(t, time.Until(end), "the room to store the first commit", func() bool {
		return storedHead(t, service.store, artifactID) > before
	})
}

// Cycle (vi). A live write takes the document's advisory lock before its fork's grant
// (headUnderDocumentLock, coverFork), and the room's persistence worker stores a room's updates
// under that same lock (AppendRoomFlush). So a write of the server's own, which waits for its
// fork's bytes with no deadline, held the lock the worker waited for while it waited for bytes a
// heavy apply held through its commit, and the apply waited for that worker to read the room's
// full queue of updates to store. With the pool full but for half the apply's grant, the write
// commits and the room stores its updates and the apply returns, because the apply gave its grant
// back before its commit's observers ran: the write's fork, a copy of the room the apply grew,
// fits in what the apply gave back and the half beside it, and in the half alone it does not.
func TestTheWorkersWaitForTheDocumentsLockEndsOnceAHeavyApplyGaveItsGrantBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	forkWaits := make(chan struct{})
	pool, waited := poolOfOneSlot(t, sync.OnceFunc(func() { close(forkWaits) }))
	fillPoolBut(t, pool, grant+grant/2)
	before := storedHead(t, service.store, artifactID)
	_, releaseWorker := holdWorkerBehindAFullQueue(t, service, artifactID, live, 1)
	observer := holdNextObserver(t, service, artifactID, false)
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)
	if need := service.copyOfRoom(live); need <= grant/2 || need > grant+grant/2 {
		t.Fatalf("the write's fork weighs %d bytes, want past the %d left beside the apply's grant and within the %d it leaves once it gives that grant back",
			need, grant/2, grant+grant/2)
	}

	// The write of the server's own takes the document's lock while the worker is held short of
	// it, and then waits for its fork's bytes if the apply still holds its grant.
	committed := make(chan struct{})
	var committedOnce sync.Once
	service.afterLiveCommit = func(room string) {
		if room == artifactID {
			committedOnce.Do(func() { close(committed) })
		}
	}
	written := make(chan error, 1)
	go func() {
		written <- cancelWrite(context.Background(), service, artifactID, "First.", "First, rewritten.", nil)
	}()
	select {
	case <-committed:
	case <-forkWaits:
	case err := <-written:
		if err != nil {
			t.Fatalf("the write of the server's own: %v", err)
		}
		written <- nil
	case <-time.After(roomWait(t)):
		t.Fatal("the write of the server's own neither committed nor waited for its fork's bytes")
	}

	// The worker goes on to store the first commit under the document's lock.
	releaseWorker()
	wait := min(roomWait(t), besideAFullPoolWait)
	deadline := time.After(wait)
	requireAnswered(t, heavy, deadline, "the heavy apply", wait, waited)
	requireAnswered(t, written, deadline, "the write of the server's own", wait, waited)
	if moved := storedHead(t, service.store, artifactID); moved <= before {
		t.Fatalf("the document stands at update %d once the write and the apply returned, want past %d", moved, before)
	}
}

// closeBehindAHeavyApply shuts ygo's server down while a heavy apply of update into live,
// artifactID's room, is inside its commit, as a browser's read loop can still commit while ygo
// shuts down: the room's persistence worker retires from the room's queue of updates to store and
// compacts the room as it exits (servicePersistenceAdapter.Compact), and the apply's commit, held
// at the room's observer until the worker has begun that compaction (beforeClosingCompaction),
// then hands its update to ygo's stranded persistence, which waits for the worker to exit
// (persistStranded). beforeClose, where it is not nil, runs once the apply's commit is held and
// before the shutdown begins. It answers the shutdown's error and the hook's.
func closeBehindAHeavyApply(t *testing.T, service *Service, artifactID string, live *crdt.Doc, update []byte, beforeClose func()) (closed, applied <-chan error) {
	t.Helper()
	compacting := make(chan struct{})
	var compactingOnce sync.Once
	service.beforeClosingCompaction = func(room string) {
		if room == artifactID {
			compactingOnce.Do(func() { close(compacting) })
		}
	}
	observer := holdNextObserver(t, service, artifactID, true)
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)
	if beforeClose != nil {
		beforeClose()
	}
	shut := make(chan error, 1)
	go func() { shut <- service.srv.Shutdown(context.Background()) }()
	select {
	case <-compacting:
	case <-time.After(roomWait(t)):
		t.Fatal("the room's persistence worker never began the compaction it runs as it exits")
	}
	observer.release()
	return shut, heavy
}

// requireFoldedThrough fails the test unless artifactID's lowest stored row is foldPoint: the
// rows below it were folded into it.
func requireFoldedThrough(t *testing.T, service *Service, artifactID string, foldPoint int64) {
	t.Helper()
	if rows := storedRows(t, service.store, artifactID); len(rows) == 0 || rows[0].version != foldPoint {
		t.Fatalf("the document holds rows %v, want the fold point %d lowest", rowVersions(rows), foldPoint)
	}
}

// Cycle (iv). The compaction a room's persistence worker runs as it exits folds rows read into
// memory, and a fold of those takes one of the process's buffered folds first (foldRead,
// foldSlots), whose holders then wait for their bytes of the pod's load concurrency. A commit made
// once the worker has retired waits for it to exit (ygo's persistStranded). With the only buffered
// fold held by a holder waiting for the pool, and the pool full but for a heavy apply's grant,
// the room's close finishes and the apply returns, because the apply gave its grant back before
// its commit's observers ran. Held through the commit, the grant kept the holder waiting, the
// holder kept the compaction waiting, and the compaction kept the apply waiting.
func TestTheClosingCompactionsWaitForABufferedFoldEndsOnceAHeavyApplyGaveItsGrantBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	appends, ok := service.persistence.(*PgVersioned)
	if !ok {
		t.Fatalf("the service appends through %T, which has no buffered folds", service.persistence)
	}
	appends.foldSlots = make(chan struct{}, 1)
	live := roomDocument(t, service, artifactID)
	update, grant := heavyRoomUpdate(t, live)
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)

	closed, heavy := closeBehindAHeavyApply(t, service, artifactID, live, update, func() {
		// A fold holding the only buffered fold, waiting for a byte of the pool.
		appends.foldSlots <- struct{}{}
		go func() {
			defer func() { <-appends.foldSlots }()
			holder := newLoadSlot()
			if err := holder.take(context.Background(), 1); err == nil {
				holder.release()
			}
		}()
	})
	wait := min(roomWait(t), besideAFullPoolWait)
	deadline := time.After(wait)
	requireAnswered(t, closed, deadline, "the room's close at ygo's shutdown", wait, waited)
	requireAnswered(t, heavy, deadline, "the heavy apply", wait, waited)
	requireFoldedThrough(t, service, artifactID, foldPoint)
}

// Cycle (v). The compaction a room's persistence worker runs as it exits reads the rows it folds
// through one of the compaction pool's connections, and a fold of rows past foldBufferBytes holds
// its connection while it waits for its bytes of the pod's load concurrency (foldRead). A commit
// made once the worker has retired waits for it to exit (ygo's persistStranded). With every
// connection of the compaction pool held by a fold waiting for the pool, and the pool full but
// for a heavy apply's grant, the room's close finishes and the apply returns, because the apply
// gave its grant back before its commit's observers ran. Held through the commit, the grant kept
// those folds waiting, they kept the compaction waiting for a connection, and the compaction kept
// the apply waiting.
func TestTheClosingCompactionsWaitForACompactionConnectionEndsOnceAHeavyApplyGaveItsGrantBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	foldPoint := writeRoomRows(t, service, artifactID)
	ageUpdatesThrough(t, service.store, artifactID, foldPoint, time.Hour)
	compaction, err := service.store.Pool.Compaction()
	if err != nil {
		t.Fatalf("open the compaction pool: %v", err)
	}
	live := roomDocument(t, service, artifactID)
	update, grant := heavyRoomUpdate(t, live)
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)

	closed, heavy := closeBehindAHeavyApply(t, service, artifactID, live, update, func() {
		// A fold on each of the compaction pool's connections, waiting for a byte of the pool.
		for range compaction.Stat().MaxConns() {
			conn, err := compaction.Acquire(context.Background())
			if err != nil {
				t.Fatalf("hold a connection of the compaction pool: %v", err)
			}
			go func() {
				defer conn.Release()
				holder := newLoadSlot()
				if err := holder.take(context.Background(), 1); err == nil {
					holder.release()
				}
			}()
		}
	})
	wait := min(roomWait(t), besideAFullPoolWait)
	deadline := time.After(wait)
	requireAnswered(t, closed, deadline, "the room's close at ygo's shutdown", wait, waited)
	requireAnswered(t, heavy, deadline, "the heavy apply", wait, waited)
	requireFoldedThrough(t, service, artifactID, foldPoint)
}

// joinedWriteBoundMargin is how far past Join's WriteRequestBound a caller's write may be answered
// in the test below: the statement the bound cancels, and the round trips of the cancel itself.
const joinedWriteBoundMargin = 10 * time.Second

// joinedWriteTimeLeft is what the test below leaves of WriteRequestBound when the write begins, so
// the write meets the bound inside the test's time rather than 45 seconds on.
const joinedWriteTimeLeft = 10 * time.Second

// A joined transaction's second document is the one wait of a holder of the pod's load
// concurrency on a document's advisory lock: the transaction holds the grant its first document's
// fork took (forkSlot) as its write of the second document takes that document's lock
// (headUnderDocumentLock). The room's persistence worker holds its document's lock while its gate
// folds a history replaced under the room (AppendRoomFlush, storedHistoryHolds) and waits for the
// pool, so the transaction can wait for a lock whose holder waits for the bytes it holds. Join's
// WriteRequestBound ends that wait: the transaction is answered within the bound, with an error
// the API answers 503 DOC_SERVICE_UNAVAILABLE (ErrServiceUnavailable or its request's end,
// writeHandlerError), and gives its grant back, and the room then stores what it held. This tests
// a bound, not a fix: nothing in this package breaks the wait, so no revert makes it fail.
func TestAJoinedTransactionsWaitForItsSecondDocumentsLockEndsAtTheWriteBound(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	first := createDocument(t, database, "# First")
	second := createDocument(t, database, "before")
	service, server := newTestServiceInstance(t, database)
	storeHistory(t, service, first, grantTestState(t, 16<<10))
	seedServiceText(t, service, second, "before")
	waitForLandedAppends(t, service, second)
	alice := connectWithScope(t, server.URL, second, "alice", "read-write", "before\n")
	head := storedHead(t, database, second)

	// Alice's keystroke reaches the second room's worker, which waits for the document's lock
	// while a compaction of another process folds the history past the room's cursor under it.
	holder, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the document's lock: %v", err)
	}
	defer holder.Rollback(ctx)
	if err := lockDocumentRoom(ctx, holder, second); err != nil {
		t.Fatalf("hold the document's lock: %v", err)
	}
	typeOver(t, alice, "before", "first keystroke")
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
		t.Fatal("alice's append never came to wait on the document's lock")
	}
	foldHistoryPastRoom(t, holder, second, head)

	foldWaits := make(chan struct{})
	pool, _ := poolOfOneSlot(t, sync.OnceFunc(func() { close(foldWaits) }))
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	spent := WriteRequestBound - joinedWriteTimeLeft
	began := time.Now().Add(-spent)
	caller := CallerRequestSinceForTest(request, spent)
	tx, err := database.Pool.Begin(caller)
	if err != nil {
		t.Fatalf("begin the joined transaction: %v", err)
	}
	defer tx.Rollback(context.Background())
	joined, ledger := service.Join(caller, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, first, []model.EditOp{{Op: "replace", Find: grantWriteHeading, With: "Written"}}, grantWriter, nil); err != nil {
		t.Fatalf("the joined transaction's write of the first document: %v", err)
	}
	if pool.held.Load() == 0 {
		t.Fatal("the joined transaction's write of the first document holds nothing of the pool")
	}

	// The rest of the pool is held, and the worker's gate folds the history under the
	// document's lock, waiting for the pool.
	fillPoolBut(t, pool, 0)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("release the document's lock: %v", err)
	}
	select {
	case <-foldWaits:
	case <-time.After(roomWait(t)):
		t.Fatal("the second room's worker never came to wait for the pool in its gate's fold")
	}

	answered := make(chan error, 1)
	go func() {
		_, err := service.ApplyOps(joined, second, []model.EditOp{{Op: "replace", Find: "first keystroke", With: "second write"}}, grantWriter, nil)
		answered <- err
	}()
	waitFor(t, roomWait(t), "the joined transaction's write of the second document to wait on its lock", func() bool {
		select {
		case err := <-answered:
			t.Fatalf("the write of the second document was answered (%v) before it waited on the document's lock", err)
		default:
		}
		return lockWaits(t, ctx, database, "select pg_advisory_xact_lock(%") > 0
	})
	select {
	case err := <-answered:
		elapsed := time.Since(began)
		t.Logf("the joined transaction's write of the second document was answered %s after its request began (WriteRequestBound %s): %v", elapsed, WriteRequestBound, err)
		if err == nil {
			t.Fatal("the write of the second document went through while the document's lock was held")
		}
		if !errors.Is(err, ErrServiceUnavailable) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("the write of the second document was answered %v, want ErrServiceUnavailable or its request's end, which the API answers 503", err)
		}
		if elapsed > WriteRequestBound+joinedWriteBoundMargin {
			t.Fatalf("the write of the second document was answered %s after its request began, past WriteRequestBound %s and %s beside it", elapsed, WriteRequestBound, joinedWriteBoundMargin)
		}
	case <-time.After(joinedWriteTimeLeft + joinedWriteBoundMargin):
		t.Fatalf("the joined transaction's write of the second document was not answered within %s of WriteRequestBound", joinedWriteBoundMargin)
	}

	// The transaction gives its grant back, and the second room's worker stores alice's keystroke.
	ledger.Discard()
	_ = tx.Rollback(context.Background())
	waitFor(t, roomWait(t), "the second room to store alice's keystroke once the transaction ended", func() bool {
		return storedHead(t, database, second) > head+1
	})
}
