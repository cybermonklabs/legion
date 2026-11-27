package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// This file tests a heavy browser apply's grant of the pod's load concurrency, held through the
// room's update observer for the apply's commit, whose drain decodes the commit into the room's
// replica, and given back as that observer ends, before ygo's persistence observer runs
// (Service.weighPeerUpdate, roomApplies.releaseGrant); and the waits of a room's persistence
// worker that the grant, held through the persistence observer, closed into a cycle. The
// persistence observer hands the update to the worker through a queue of 256 updates and waits
// while it is full, or for the worker's exit once the worker has begun to exit (ygo's
// persistStranded), so a worker that waited for the pool while the apply held it waited for
// itself. The grant goes back before that observer runs, so each wait ends. Each test of a cycle
// holds the pool full with that apply's grant in it and fails by its timeout with the grant held
// until the hook returns.

// heavyRoomUpdate is an update a browser could send live, a room's document: a chain of
// chainStructs characters under the margin (marginChain), declaring more structs than an apply
// takes no slot for, and the grant the peer update hook takes for its apply (storedCopyWeight).
// The commit the room makes of it declares the same structs, so its stored copy weighs that grant
// too.
func heavyRoomUpdate(t *testing.T, live *crdt.Doc) (update []byte, grant int64) {
	t.Helper()
	update = marginChain(t, copyOf(t, crdt.EncodeStateAsUpdateV1(live, nil)), chainStructs)
	requireWeighedByItsStructs(t, update)
	return update, storedCopyWeight(update)
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
// its service shuts down. It answers the holder of those bytes, which the test may give back
// sooner.
func fillPoolBut(t *testing.T, pool *loadSlots, leave int64) *loadSlot {
	t.Helper()
	filler := newLoadSlot()
	if take := pool.poolBytes() - pool.held.Load() - leave; !filler.tryTake(take) {
		t.Fatalf("the pool refused %d of its %d bytes with %d held", take, pool.poolBytes(), pool.held.Load())
	}
	t.Cleanup(filler.release)
	return filler
}

// observerGate is the first commit of a room to reach the room's update observer
// (beforeObserveUpdate) once the gate is armed (arm): the apply has integrated and passed its
// check, the document is released, the apply still holds its grant, which it gives back as the
// room's observer ends, and the commit's persistence observer, which ygo runs after the room's own,
// has not run. reached closes as that commit arrives there; release lets a held commit go on.
type observerGate struct {
	armed   atomic.Bool
	reached chan struct{}
	release func()
}

// installObserverGate installs an observerGate on artifactID's room, unarmed, holding the commit
// it takes there until released where hold is set, and releasing it as the test ends in any case.
// The room's update observer reads the hook with no lock, on the goroutine of whatever commit it
// observes, so a test installs the gate before anything commits to the room and arms it once the
// commits it lets by are made: the commit fill leaves waiting on the room's full queue has read the
// hook already, on a goroutine the test never waits for, and a hook assigned after it races with
// that read.
func installObserverGate(t *testing.T, service *Service, artifactID string, hold bool) *observerGate {
	t.Helper()
	gate := &observerGate{reached: make(chan struct{})}
	proceed := make(chan struct{})
	gate.release = sync.OnceFunc(func() { close(proceed) })
	var first atomic.Bool
	service.beforeObserveUpdate = func(room string) {
		if room != artifactID || !gate.armed.Load() || !first.CompareAndSwap(false, true) {
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

// arm makes the next commit to reach the room's update observer the gate's.
func (g *observerGate) arm() { g.armed.Store(true) }

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
	return &roomCommitter{live: live, writer: copyOf(t, crdt.EncodeStateAsUpdateV1(live, nil))}
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

// fullPoolWait is a test's wait for the work a pool that a heavy apply's grant fills holds up: it
// lasts wait and ends at end, when deadline fires, and waited reports whether a taker began to wait
// for the pool (poolOfOneSlot).
type fullPoolWait struct {
	end      time.Time
	deadline <-chan time.Time
	wait     time.Duration
	waited   *atomic.Bool
}

// waitBesideAFullPool begins a fullPoolWait now, bounded by besideAFullPoolWait.
func waitBesideAFullPool(t *testing.T, waited *atomic.Bool) fullPoolWait {
	t.Helper()
	wait := min(roomWait(t), besideAFullPoolWait)
	return fullPoolWait{end: time.Now().Add(wait), deadline: time.After(wait), wait: wait, waited: waited}
}

// requireAnswered waits until within's deadline for what answered sends, failing the test with
// what names the wait where nothing comes by then, and fails it where the answer is an error.
func requireAnswered(t *testing.T, answered <-chan error, what string, within fullPoolWait) {
	t.Helper()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-within.deadline:
		t.Fatalf("%s did not finish within %s while a heavy apply's grant filled the pool (a taker began to wait for the pool: %t)",
			what, within.wait, within.waited.Load())
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

// poolOfOneSlot gives the process a load concurrency of one slot for the rest of t
// (UseOneLoadSlotForTest), whose takers call waiting as they begin to wait, set before any taker
// can reach it, and reports whether one has begun to wait.
func poolOfOneSlot(t *testing.T, waiting func()) (*loadSlots, *atomic.Bool) {
	t.Helper()
	waited := new(atomic.Bool)
	pool, restore := UseOneLoadSlotForTest(func() {
		waited.Store(true)
		if waiting != nil {
			waiting()
		}
	})
	t.Cleanup(restore)
	return pool.slots, waited
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

// heapGivenBackTolerance is how far the live heap a heavy apply leaves behind it, once it has given
// its grant back, may stand past what the room budget charges the room for that commit: what the
// process allocates beside the room between the two readings - the test's own bookkeeping, the
// scheduler's and the database driver's buffers - which no room budget counts. A mebibyte; the
// readings this test logs stand millions of bytes inside the charge.
const heapGivenBackTolerance = 1 << 20

// A heavy browser apply holds its grant of the pod's load concurrency while it decodes and
// integrates the update and its check renders what it left, and on through the room's update
// observer for its commit, whose drain decodes the commit's update a second time, into the room's
// replica (renderedReplica.applyUpdate); it gives the grant back as that observer ends, before
// ygo's persistence observer runs (Service.weighPeerUpdate, roomApplies.releaseGrant). So while
// the replica decodes the commit the pool holds the apply's whole grant, and the room's charge
// still stands where it stood before the apply, since the drain charges the room only once it has
// taken the commit (chargeAdded). By the time the room's persistence worker comes to store the
// commit, which ygo's persistence observer hands it after the room's observer has returned, the
// pool holds nothing of the apply's, the room's charge stands where the hook leaves it, past where
// it stood before, and the live heap, read the way liveHeap does, has grown by no more than that
// charge, with heapGivenBackTolerance beside it: what the apply gave back was the transient heap of
// its decode, its check and the replica's decode, and what it leaves is the room's, which the room
// budget charges.
//
// The apply's goroutine is held, until the worker has read the pool, in an update observer of the
// test's own, which ygo runs after the persistence observer since the test registers it once the
// room has loaded, so the worker reads the pool before the hook returns. Given back at
// crdt.ApplyBudget.Integrated, as the commit's observers begin, the grant would leave the
// replica's decode outside the pool; given back as the hook returns, it would be held through the
// persistence observer, which waits while the room's queue of updates to store is full.
func TestAHeavyApplysGrantCoversTheReplicasDecodeAndGoesBackBeforeThePersistenceObserver(t *testing.T) {
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
	// The replica's decode of the commit: ygo calls the replica document's update observers inside
	// that decode's apply (renderedReplica.applyUpdate), under the replica's lock.
	var atDecode reading
	var decoded atomic.Bool
	replica := replicaFor(t, service, live)
	replica.mu.Lock()
	if replica.doc == nil {
		replica.mu.Unlock()
		t.Fatal("the loaded room's replica holds no document")
	}
	stopDecode := replica.doc.OnUpdate(func([]byte, any) {
		if decoded.CompareAndSwap(false, true) {
			atDecode = reading{held: pool.held.Load(), charged: chargedWeight(charge)}
		}
	})
	replica.mu.Unlock()
	t.Cleanup(stopDecode)
	var observed atomic.Bool
	service.beforeObserveUpdate = func(room string) {
		if room == artifactID {
			observed.Store(true)
		}
	}
	// The worker's store of the commit, which it begins once the persistence observer has handed
	// it the commit: the room's observer has returned by then.
	var atStore reading
	read := make(chan struct{})
	var readOnce sync.Once
	service.beforeRoomAppend = func(room string) error {
		if room == artifactID && decoded.Load() {
			readOnce.Do(func() {
				atStore = reading{held: pool.held.Load(), charged: chargedWeight(charge), heap: liveHeap()}
				close(read)
			})
		}
		return nil
	}
	stopHold := live.OnUpdate(func(_ []byte, origin any) {
		if _, heavy := origin.(*int); heavy {
			select {
			case <-read:
			case <-time.After(roomWait(t)):
			}
		}
	})
	t.Cleanup(stopHold)

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
	if !decoded.Load() {
		t.Fatal("the room's replica never decoded the heavy apply's commit")
	}
	select {
	case <-read:
	default:
		t.Fatal("the room's worker never came to store the heavy apply's commit before the hook returned")
	}
	chargedAfter := chargedWeight(charge)
	grown := int64(atStore.heap) - int64(heap)
	t.Logf("the apply took %d bytes of the pool; while the replica decoded its commit the pool held %d and the room's charge stood at %d (%d before the apply); as the worker came to store the commit the pool held %d, the room's charge stood at %d (%d once the hook had returned, %+d), and the live heap at %d (%d before, %+d against the charge's %+d and a tolerance of %d)",
		grant, atDecode.held, atDecode.charged, charged, atStore.held, atStore.charged, chargedAfter, chargedAfter-charged,
		atStore.heap, heap, grown, chargedAfter-charged, heapGivenBackTolerance)

	if grants := taken.all(); len(grants) == 0 || grants[0] != grant {
		t.Fatalf("the pool's grants left %v held, want the apply's own %d first (copyWeight)", grants, grant)
	}
	if atDecode.held != grant {
		t.Fatalf("the pool held %d bytes while the room's replica decoded the commit, want the apply's whole %d-byte grant held during the replica's decode: the apply gave its grant back before the room's observer drained its commit", atDecode.held, grant)
	}
	if atDecode.charged != charged {
		t.Fatalf("the room's charge stood at %d while the room's replica decoded the commit, want the %d it stood at before the apply: the room is charged once the drain has taken the commit", atDecode.charged, charged)
	}
	if atStore.held != 0 {
		t.Fatalf("the pool held %d bytes as the room's worker came to store the commit, want none: the apply held its %d-byte grant past the room's observer, through the persistence observer", atStore.held, grant)
	}
	if chargedAfter <= charged {
		t.Fatalf("the room's charge stood at %d once the apply had returned, want past the %d before it: the room charges the structs the apply left", chargedAfter, charged)
	}
	if atStore.charged != chargedAfter {
		t.Fatalf("the room's charge stood at %d as the room's worker came to store the commit, with the grant given back, and at %d once the hook had returned: giving the grant back moved the room's charge", atStore.charged, chargedAfter)
	}
	if grown > chargedAfter-charged+heapGivenBackTolerance {
		t.Fatalf("the live heap grew by %d bytes by the worker's store of the commit, past the %d the room's charge grew by and %d beside it: the heap the apply gave back was not transient",
			grown, chargedAfter-charged, heapGivenBackTolerance)
	}
}

// observerOrder is the order in which a test saw the steps of one commit's observers it names.
type observerOrder struct {
	mu    sync.Mutex
	steps []string
}

func (o *observerOrder) record(step string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.steps = append(o.steps, step)
}

func (o *observerOrder) recorded() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.steps)
}

// ygo calls a document's update observers in the order they were registered, and the room's own
// observer, which the room registers in its load hook (onLoadDocument), comes before ygo's
// persistence observer, which ygo registers once that hook has returned
// (provider/websocket/server.go, loadRoom). A heavy apply's grant goes back as the room's observer
// for its commit ends (Service.weighPeerUpdate, roomApplies.releaseGrant), so that order is what
// keeps the persistence observer from closing a cycle: with the room's persistence worker held at
// its store of a heavy commit behind a full queue of updates to store, and the pool full but for
// the apply's grant, the apply's commit reaches the room's observer while the worker is held, and
// the persistence observer, which waits while the queue is full, returns only after it; and the
// worker, let go, waits for the pool to read the commit it holds back (StoreUpdateStored,
// holdServerLoad), gets what the apply gave back, and stores it, and the apply's commit hands its
// update over and the hook returns.
//
// The test records the order of the two observers' calls on the apply's commit: the room's as it
// begins (beforeObserveUpdate), and the persistence observer's as it returns, which an update
// observer of the test's own reads, since ygo runs it after the persistence observer once the
// test registers it after the room has loaded. With the room's observer after the persistence
// observer, the commit reaches the room's observer only once the worker reads the queue again,
// which it does not while the test holds it, so the test fails there; and the grant, held through
// the persistence observer's wait, leaves the worker's read-back waiting for the pool for good,
// which is what fails it by its timeout with the grant held until the hook returns.
func TestTheRoomsUpdateObserverRunsBeforeThePersistenceObserverSoTheWorkersReadBackGetsTheGrant(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	// The room's observer records its step for the commit the gate takes, the first once it is
	// armed; both hooks go in before anything commits to the room (installObserverGate).
	const roomObserver, persistenceObserver = "the room's update observer", "the persistence observer"
	var order observerOrder
	observer := installObserverGate(t, service, artifactID, false)
	gate := service.beforeObserveUpdate
	service.beforeObserveUpdate = func(room string) {
		if room == artifactID && observer.armed.Load() {
			select {
			case <-observer.reached:
			default:
				order.record(roomObserver)
			}
		}
		gate(room)
	}
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)
	before := storedHead(t, service.store, artifactID)
	first, releaseWorker := holdWorkerBehindAFullQueue(t, service, artifactID, live, heldCommitKeys)
	if weight, heavy := heavyUpdateWeight(first); !heavy || weight > grant {
		t.Fatalf("the commit the worker holds is %d bytes declaring %d structs, weighed at %d (heavy: %t), want a heavy update weighed within the apply's %d-byte grant",
			len(first), declaredStructs(first), weight, heavy, grant)
	}

	observer.arm()
	stopAfter := live.OnUpdate(func(_ []byte, origin any) {
		if _, heavy := origin.(*int); heavy {
			order.record(persistenceObserver)
		}
	})
	t.Cleanup(stopAfter)
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)
	if steps := order.recorded(); !slices.Equal(steps, []string{roomObserver}) {
		t.Fatalf("with the room's worker held behind a full queue the apply's commit reached %q, want the room's update observer alone: the persistence observer waits for the worker", steps)
	}

	// The worker goes on to read the first commit back with every byte of the pool held but what
	// the apply gave back, if it did.
	releaseWorker()
	within := waitBesideAFullPool(t, waited)
	requireAnswered(t, heavy, "the heavy apply", within)
	waitFor(t, time.Until(within.end), "the room to store the first commit", func() bool {
		return storedHead(t, service.store, artifactID) > before
	})
	if steps := order.recorded(); !slices.Equal(steps, []string{roomObserver, persistenceObserver}) {
		t.Fatalf("the apply's commit ran its observers in the order %q, want %q", steps, []string{roomObserver, persistenceObserver})
	}
}

// heldCommitKeys is how many margin keys the commit the worker is held at sets in
// TestTheWorkersReadBackWaitsForAPoolAHeavyApplyGaveBack and in
// TestTheRoomsUpdateObserverRunsBeforeThePersistenceObserverSoTheWorkersReadBackGetsTheGrant: each
// key a struct of its own, so the commit declares more structs than an update may and still be
// stored with no grant (heavyUpdateWeight), and its read-back waits for the pool, while what it
// takes stays within the heavy apply's grant, which declares chainStructs.
const heldCommitKeys = 2 * unslottedApplyStructs

// The room's persistence worker decodes each heavy update it stores into a fresh copy before it
// stores it (StoreUpdateStored, holdServerLoad, heavyUpdateWeight), waiting for its bytes of the
// pod's load concurrency. A heavy apply's commit hands its update to that worker through the
// room's queue of updates to store and waits while the queue is full. With the queue full behind a
// heavy commit the worker holds, and the pool full but for the apply's own grant, the worker's
// decode of that commit waits for the pool, and the room stores it, and the apply returns, because
// the apply gave its grant back before its commit's persistence observer ran. Held through that
// observer, the grant is what the decode waited for while the apply waited for the decode's
// worker. A lighter commit's decode takes nothing, so the worker the test holds holds a heavy one.
func TestTheWorkersReadBackWaitsForAPoolAHeavyApplyGaveBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	observer := installObserverGate(t, service, artifactID, false)
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)
	before := storedHead(t, service.store, artifactID)
	first, releaseWorker := holdWorkerBehindAFullQueue(t, service, artifactID, live, heldCommitKeys)
	if weight, heavy := heavyUpdateWeight(first); !heavy || weight > grant {
		t.Fatalf("the commit the worker holds is %d bytes declaring %d structs, weighed at %d (heavy: %t), want a heavy update weighed within the apply's %d-byte grant",
			len(first), declaredStructs(first), weight, heavy, grant)
	}
	observer.arm()
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)

	// The worker goes on to decode the first commit's update with every byte of the pool held
	// but what the apply gave back, if it did.
	releaseWorker()
	within := waitBesideAFullPool(t, waited)
	requireAnswered(t, heavy, "the heavy apply", within)
	waitFor(t, time.Until(within.end), "the room to store the first commit", func() bool {
		return storedHead(t, service.store, artifactID) > before
	})
}

// A live write takes the document's advisory lock before its fork's grant (headUnderDocumentLock,
// coverFork), and the room's persistence worker stores a room's updates under that same lock
// (AppendRoomFlush). So a write of the server's own, which waits for its fork's bytes with no
// deadline, held the lock the worker waited for while it waited for bytes a heavy apply held
// through its commit's persistence observer, and the apply waited there for that worker to read
// the room's full queue of updates to store. With the pool full but for half the apply's grant,
// the write commits and the room stores its updates and the apply returns, because the apply gave
// its grant back before its commit's persistence observer ran: the write's fork, a copy of the
// room the apply grew, fits in what the apply gave back and the half beside it, and in the half
// alone it does not.
func TestTheWorkersWaitForTheDocumentsLockEndsOnceAHeavyApplyGaveItsGrantBack(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	observer := installObserverGate(t, service, artifactID, false)
	live, update, grant := loadedRoom(t, service, artifactID, "First.\n")
	forkWaits := make(chan struct{})
	pool, waited := poolOfOneSlot(t, sync.OnceFunc(func() { close(forkWaits) }))
	fillPoolBut(t, pool, grant+grant/2)
	before := storedHead(t, service.store, artifactID)
	_, releaseWorker := holdWorkerBehindAFullQueue(t, service, artifactID, live, 1)
	observer.arm()
	heavy := applyHeavy(service, artifactID, live, update)
	awaitHeavyApply(t, observer, heavy)
	if need := service.copyOfRoom(live); need <= grant/2 || need > grant+grant/2 {
		t.Fatalf("the write's fork weighs %d bytes, want past the %d left beside the apply's grant and within the %d it leaves once it gives that grant back",
			need, grant/2, grant+grant/2)
	}

	// The write of the server's own takes the document's lock while the worker is held short of
	// it, and then waits for its fork's bytes if the apply still holds its grant.
	committed := make(chan struct{})
	service.afterLiveCommit = onFirst(artifactID, func() { close(committed) })
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
	within := waitBesideAFullPool(t, waited)
	requireAnswered(t, heavy, "the heavy apply", within)
	requireAnswered(t, written, "the write of the server's own", within)
	if moved := storedHead(t, service.store, artifactID); moved <= before {
		t.Fatalf("the document stands at update %d once the write and the apply returned, want past %d", moved, before)
	}
}

// closeBehindAHeavyApply shuts ygo's server down while a heavy apply of update into live,
// artifactID's room, is inside its commit, as a browser's read loop can still commit while ygo
// shuts down: the room's persistence worker retires from the room's queue of updates to store and
// compacts the room as it exits (servicePersistenceAdapter.Compact), and the apply's commit, held
// at the room's observer, its grant still held, until the worker has begun that compaction
// (beforeClosingCompaction), then drains into the room's replica, gives its grant back as the
// room's observer ends, and hands its update to ygo's stranded persistence, which waits for the
// worker to exit (persistStranded). beforeClose, where it is not nil, runs once the apply's commit
// is held and before the shutdown begins. It answers the shutdown's error and the hook's. It
// installs the gate on the room's observer and arms it at once (installObserverGate): no commit is
// in flight, since its callers' writes to the room ran on the test's goroutine and have landed,
// and no settlement runs.
func closeBehindAHeavyApply(t *testing.T, service *Service, artifactID string, live *crdt.Doc, update []byte, beforeClose func()) (closed, applied <-chan error) {
	t.Helper()
	compacting := make(chan struct{})
	service.beforeClosingCompaction = onFirst(artifactID, func() { close(compacting) })
	observer := installObserverGate(t, service, artifactID, true)
	observer.arm()
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

// requireClosesBesideAFullPool fills the pool but for the grant of a heavy apply into artifactID's
// room and shuts ygo's server down behind that apply, with beforeClose (closeBehindAHeavyApply),
// and fails the test unless the room's close and the apply each return within besideAFullPoolWait
// and the close's compaction folded the rows below foldPoint into it.
func requireClosesBesideAFullPool(t *testing.T, service *Service, artifactID string, foldPoint int64, beforeClose func()) {
	t.Helper()
	live := roomDocument(t, service, artifactID)
	update, grant := heavyRoomUpdate(t, live)
	pool, waited := poolOfOneSlot(t, nil)
	fillPoolBut(t, pool, grant)
	closed, heavy := closeBehindAHeavyApply(t, service, artifactID, live, update, beforeClose)
	within := waitBesideAFullPool(t, waited)
	requireAnswered(t, closed, "the room's close at ygo's shutdown", within)
	requireAnswered(t, heavy, "the heavy apply", within)
	requireFoldedThrough(t, service, artifactID, foldPoint)
}

// The compaction a room's persistence worker runs as it exits folds rows read into memory, and a
// fold of those takes one of the process's buffered folds first (foldRead, foldSlots), whose
// holders then wait for their bytes of the pod's load concurrency. A commit made once the worker
// has retired waits for it to exit (ygo's persistStranded). With the only buffered fold held by a
// holder waiting for the pool, and the pool full but for a heavy apply's grant, the room's close
// finishes and the apply returns, because the apply gave its grant back before its commit's
// persistence observer ran. Held through that observer, the grant kept the holder waiting, the
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
	requireClosesBesideAFullPool(t, service, artifactID, foldPoint, func() {
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
}

// The compaction a room's persistence worker runs as it exits reads the rows it folds through one
// of the compaction pool's connections, and a fold of rows past foldBufferBytes holds its
// connection while it waits for its bytes of the pod's load concurrency (foldRead). A commit made
// once the worker has retired waits for it to exit (ygo's persistStranded). With every connection
// of the compaction pool held by a fold waiting for the pool, and the pool full but for a heavy
// apply's grant, the room's close finishes and the apply returns, because the apply gave its grant
// back before its commit's persistence observer ran. Held through that observer, the grant kept
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
	requireClosesBesideAFullPool(t, service, artifactID, foldPoint, func() {
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
// the API answers 503 DOC_SERVICE_UNAVAILABLE (ErrServiceUnavailable, or the end of that bound or
// of its request: api's writeHandlerError, WriteRequestBoundPassed), and gives its grant back, and
// the room then stores what it held. This tests a bound, not a fix: nothing in this package breaks
// the wait, so no revert makes it fail.
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
	holder := foldPastAHeldKeystroke(t, database, alice, second, head)

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
			t.Fatalf("the write of the second document was answered %v, want ErrServiceUnavailable or the end of its write's bound or of its request, which the API answers 503", err)
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
