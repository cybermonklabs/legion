package docs

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"golang.org/x/sync/semaphore"
)

// The pod's load concurrency bounds the whole-document copy heap it builds at once. It is one
// pool of bytes - MemoryBounds.LoadSlots slots of loadSlotBytes each - and a taker takes what
// its own copy costs: the larger of the state it is built from at bytesOfHeapAStateByte and
// the structs it holds at bytesOfHeapAStruct, at least a byte and at most one slot's worth
// (copyWeight). So the copies the pool can hold side by side run side by side - a fork of a
// mebibyte of prose costs 51,599,080 bytes, eleven of which fit the one slot the calibration
// has - while the heaviest whole-document decode measured, 320,762,656
// bytes above what it leaves behind, still fits the slot the pool was sized from. A taker that
// can name no state takes a whole slot, which is what every taker took before, and which is why
// a burst of 320 live writes applied 5 of them (dispatch://INFRA-179). Four kinds of work take
// from the pool.
//
// The first is one whole-document decode, held while it decodes: a room's load (its fold of a
// history of several updates, the copy LoadDoc decodes, the load hook's decode into the room,
// bootstrapLoadedState), the fold of a cold read, a version read or a compaction (foldRead), the
// replica's build from a base (renderedReplica.start), the copy a server write's decode-cap count
// takes (structsAfter), a rebuild's decodes (its check of a resident room and of the stored
// history, nearACap, rebuildSource, RebuildTx), the read-back of each heavy update a room's
// persistence worker stores (StoreUpdateStored, AppendUpdatesWithCredit, heavyUpdateWeight) and
// the append gate's fold of a history replaced under a room and its decode of the run onto the
// document that fold makes (storedHistoryHolds), which share one grant holding that document from
// its first row until the run has been judged against it. The replica's build may not wait, so it
// takes its bytes only where they are free at once and otherwise decodes beside the pool's holders
// (holdFreeLoad); the read-back of a lighter update takes nothing, as its apply takes nothing
// (heavyUpdateWeight); every other decode waits for its bytes. What each leaves behind is its
// owner's: a room's two copies are charged by the room budget, a fold's state goes back to its
// caller, and a read-back's copy and the gate's document are dropped once their decodes are done.
//
// The second is a live write's fork, held for the fork's whole life rather than for a decode
// alone: from the moment before the fork is built - cloned from the room's replica (warmFork,
// Service.forkPoint) or decoded from the store or from the copy a read took of a room whose
// replica held no state (coldFork, rebuildFork, buildForkHeld) - through the trees its
// operations read off it and the markdown they render, until the fork is dropped
// (liveWrite.releaseFork). A write never gives its grant back while it holds a fork, and the
// decodes it makes inside the grant it already holds take nothing more (withLoadSlot, holdLoad).
//
// The third is one apply of a browser's update over unslottedApplyBytes, or declaring more than
// unslottedApplyStructs structs, held from before the hook reads the update until the apply has
// integrated and passed its check, or until a refused apply returns (Service.weighPeerUpdate,
// crdt.ApplyBudget.Integrated): a reconnect storm's whole-state handoffs then decode as many at a
// time as the pool holds rather than all at once, while a keystroke, far under both, never waits
// for the pool, at its apply or at its store (heavyUpdateWeight).
//
// The fourth is the answer to one browser's first sync, held from before that answer is built
// until its frames are in the browser's write queue (Service.weighStateAnswer): answering a
// SyncStep1 cuts the whole of what the browser is missing into frames and holds every one of
// them at once (ygo's sync.EncodeSyncStep2Split), so a room many browsers join at once holds its
// own state once more per join, and nothing in ygo bounds how many of those run together. Every
// room's answer is cut from its own structs, decodes nothing of the document and carries nothing
// the room parks, so each answer takes the cut's weight (answerWeight).
//
// Every taker takes its bytes before it takes a database connection, so no grant is held behind
// a connection pool a holder of another grant could fill, and a wait a request's context ends is
// answered errWriteBusy, the API's 503 DOC_SERVICE_UNAVAILABLE. The copy a read of a resident
// room takes while the room's replica is busy (holdLive, snapshotDocument) takes nothing: its
// callers hand it no request context to bound a wait by.

// unslottedApplyBytes is the update's own bytes at or under which an apply of a browser's update
// takes no slot, where it declares no more than unslottedApplyStructs structs: 2 MiB, the figure
// the apply-heap bound used before the cut, over thirty times the editor's 64 KiB paste and far
// past any keystroke, so typing never waits on a reconnect storm's handoffs. A whole-state handoff
// is above it and waits (dispatch://INFRA-179/artifact/infra179-simplify-measured-md).
const unslottedApplyBytes = 2 << 20

// unslottedApplyStructs is the most structs an update may declare and still apply with no slot,
// and the most values a read-only socket's sync step 2 is decoded to
// (Server.ReadOnlySyncStep2MaxValues, New): unslottedApplyBytes at bytesOfHeapAStruct, 3,328
// structs. The bytes alone do not bound what a decode builds, since a struct can take two bytes of
// the frame and an item of the heap: a 2,000,010-byte frame of a million GC structs allocated
// 204,963,448 bytes to apply through the hook, 102.5 times the frame, and 196,950,064 bytes to
// decode for a read-only socket's answer
// (TestRV9AReadOnlyStepTwoOfAMillionStructsCostsNoMoreThanAWritersApply, 2026-11-20). An update
// declaring more takes a slot of what its structs cost (copyWeight), and a read-only socket's
// step 2 carrying more values is answered not applied once its decode reaches them, which costs
// the page nothing: it keeps its saved rows, as for any answer not applied. A keystroke declares
// a struct or two.
const unslottedApplyStructs = unslottedApplyBytes / bytesOfHeapAStruct

// heavyUpdateWeight is what a decode of a browser's update takes of the pod's load concurrency,
// and whether it takes anything. An update is heavy past unslottedApplyBytes or past
// unslottedApplyStructs declared structs, since a frame of a million GC structs is 2 MB and
// allocated 102.5 times that to apply; a heavy one takes what its copy costs, its bytes and the
// structs it declares (copyWeight, declaredStructs, which reads each group's header and passes the
// structs of every group but the last, building nothing). A lighter one, a keystroke or a paste,
// takes nothing. A frame of at most twice unslottedApplyStructs bytes cannot declare more structs
// than that, so a keystroke is not read for them.
//
// Both decodes a browser's update gets make this judgement: its apply's, on the frame
// (Service.weighPeerUpdate), and the room's persistence worker's, on the commit it reads back as
// it stores it (StoreUpdateStored, AppendUpdatesWithCredit). A keystroke takes nothing at either,
// so its store never waits behind the heavy takers queued for the pool, as its apply never does.
func heavyUpdateWeight(update []byte) (weight int64, heavy bool) {
	var declared uint64
	if len(update) > 2*unslottedApplyStructs {
		declared = declaredStructs(update)
	}
	if len(update) <= unslottedApplyBytes && declared <= unslottedApplyStructs {
		return 0, false
	}
	return copyWeight(int64(len(update)), int64(declared)), true
}

// loadSlotsName is what the slots are logged as (logLoadSlot).
const loadSlotsName = "load"

// loadSlots is the pod's load concurrency: one pool of MemoryBounds.LoadSlots slots of
// loadSlotBytes each, held in bytes, out of which a whole-document decode, a live write's fork
// or a heavy apply takes what its own copy costs (copyWeight). Waiters are served in the order
// they came (semaphore.Weighted), and a release wakes them from the front of the queue, stopping
// at the first whose bytes do not fit rather than passing over it, so a heavy taker is never
// starved by the lighter ones behind it.
//
// It cannot deadlock, because every holder finishes and gives its bytes back: it waits for
// nothing another holder or a waiter can hold for good, and it takes nothing more while it holds.
//
// A decode's grant, held between its take and its release, decodes, renders or reads the rows of
// the transaction it already holds, takes no lock held across a wait, takes no pooled connection
// - a fold covers the grant holding its own transaction's connection alone, or, once that
// transaction has ended, none (foldRead), and a room's load gives its grant back before
// onLoadDocument reads the store - and runs no update observer, since ygo decodes nothing of a
// loading room's state and no holder commits on a resident room.
//
// A live write's grant is held far longer - from before its fork is built until its fork is
// dropped - so the things it holds it across are named here too, each with what keeps that wait
// from being a cycle:
//
//   - its own transaction, its owner row and the document's advisory lock, all taken before the
//     grant, and its writer slot, also taken before it (openLiveWrite): a waiter for a writer
//     slot holds nothing of the pool, since it has not forked yet. A joined transaction's second
//     document is the exception: its owner row, its writer slot and its advisory lock are waited
//     for while the transaction holds the grant its first document's fork took (forkSlot,
//     joinLiveWrite, readFork). A waiter of the pool can hold that advisory lock - another live
//     write of the document waiting for its fork's bytes (warmFork), or the room's persistence
//     worker in its gate's fold (AppendRoomFlush, storedHistoryHolds) - so the transaction can
//     wait for a lock whose holder waits for the bytes it holds. Join's WriteRequestBound ends
//     that wait for a caller's request: the transaction is answered 503 and gives its grant back
//     (TestAJoinedTransactionsWaitForItsSecondDocumentsLockEndsAtTheWriteBound).
//   - the room replica's lock, waited for to clone the replica (warmFork). Nothing under that
//     lock waits for the pool: the one decode there takes what it needs where the pool has room
//     at once and otherwise runs beside its holders (renderedReplica.start, holdFreeLoad).
//   - the publish of its own committed updates, which may load a room and so take a grant of
//     that load's own. The transaction drops every fork, and with them its grant, before it
//     publishes any of them (Ledger.publish).
//
// An apply's grant is given back before the commit the apply makes runs its observers
// (Service.weighPeerUpdate, crdt.ApplyBudget.Integrated). One of them hands the commit to the
// room's persistence worker through a queue of 256 updates and waits while that queue is full
// (ygo's persistence observer), and once the worker has begun to exit waits for it to finish, the
// compaction it runs as it exits included (ygo's persistStranded, which a socket still committing
// at ygo's shutdown reaches). So the worker waits for its bytes as every other decode does - its
// read-back of each heavy update it stores (StoreUpdateStored, AppendUpdatesWithCredit), its gate's
// decodes of a history replaced under the room (storedHistoryHolds) and its compaction as it exits
// (servicePersistenceAdapter.Compact) - and no holder waits on it: an apply holds no grant by the
// time its commit reaches that queue, a live write publishes once its transaction has given its
// grant back (Ledger.publish), and a server write's decode-cap copy gives its grant back before
// the write commits (structsAfter).
//
// No holder takes a second grant: a decode under a context carrying one covers itself on that one
// (withLoadSlot), so a room load's fold, its copy's decode and the load hook's decode into the
// room share the load's, and a write's fork decode and its decode-cap copy share the write's;
// and one transaction's live writes share one grant (forkSlot), so a transaction writing two
// documents never waits for the bytes its own other fork holds.
//
// A waiter may hold anything - a write's transaction, its owner row and advisory lock, a
// compaction's advisory lock and connection, a connection of the rooms pool or of the shared
// pool, a slot of a buffered fold (foldSlots), a replica's lock, a loading room's ready barrier, a
// room's persistence worker's session lock on its document, its connection of the room-appends
// pool and its place as its room's one reader of updates to store - since no holder waits for any
// of them, and so every holder finishes and gives its bytes back. The one exception is a joined
// transaction's second document, whose owner row, writer slot and advisory lock it waits for
// holding its first document's grant, and which Join's WriteRequestBound bounds (above). A waiter
// whose request ends returns its context's error holding nothing.
type loadSlots struct {
	// size is how many slots the pool is sized from, always set by whoever built it
	// (newLoadSlots): the process's MemoryBounds.LoadSlots, or a test's.
	size int64
	once sync.Once
	free *semaphore.Weighted
	// held is how many bytes of the pool are held now.
	held atomic.Int64
	// waiting runs, in a test, as a taker finds too little of the pool free and starts to wait;
	// taken runs once it holds its own bytes, with the bytes held then.
	waiting func()
	taken   func(held int64)
}

// newLoadSlots is a load concurrency of slots slots, at least one: a pod with no slot could load
// no document at all, and one slot is what a taker that can name no state takes whole.
func newLoadSlots(slots int) *loadSlots {
	return &loadSlots{size: int64(max(slots, 1))}
}

// poolBytes is the whole heap the pod builds copies in at once: its slots at loadSlotBytes each.
// It is the one figure the semaphore is made of, so no taker's bytes can pass it (copyWeight
// caps a taker at one slot).
func (h *loadSlots) poolBytes() int64 {
	return h.size * loadSlotBytes
}

// podLoadSlots is the process's load concurrency, shared by every service and store in it as the
// memory it bounds is: the calibration's until a service built at another Go memory limit sizes
// it from that limit (New), and a test swaps in one of its own (useLoadSlots).
var podLoadSlots atomic.Pointer[loadSlots]

func init() { podLoadSlots.Store(newLoadSlots(calibratedBounds.LoadSlots)) }

// semaphore is the pool itself, in bytes, made on first use.
func (h *loadSlots) semaphore() *semaphore.Weighted {
	h.once.Do(func() { h.free = semaphore.NewWeighted(h.poolBytes()) })
	return h.free
}

// loadSlot is one holder's grant: the bytes of the pool it holds, or nothing once released. One
// holder uses it at a time: the goroutine whose decodes it covers, or for a room's load the
// load's record, whose lock it is given back under (roomLoad.take).
type loadSlot struct {
	slots *loadSlots
	held  bool
	// bytes is what it holds of the pool, the weight its take asked for, given back exactly
	// (release) and 0 while it holds nothing.
	bytes int64
	// waited is how long it waited for its bytes, and since when it has held them, both for its
	// release's log line (logLoadSlot).
	waited time.Duration
	since  time.Time
}

// newLoadSlot is a holder of the pod's load concurrency holding nothing.
func newLoadSlot() *loadSlot {
	return &loadSlot{slots: podLoadSlots.Load()}
}

// take makes g hold bytes of the pool, waiting within ctx: what the copy it covers costs
// (copyWeight), or loadSlotBytes where its caller can name no state, which is one whole slot and
// so is always admitted on its own.
//
// A holder that already holds a grant keeps it and takes only the difference, where this copy
// weighs more than the grant covers: one transaction's forks share the first fork's grant
// (forkSlot) and every decode made under a carried one covers itself on it (withLoadSlot), so
// without the difference a transaction that wrote a small document and then a large one was
// charged the small one alone - measured at 32,676 bytes of the pool for a pair whose second
// fork weighed 32,148,200, which Docs.WriteIssueAskStates does on every issue close, reopen and
// sweep (dispatch://INFRA-179, review 5820480859's finding 10). The difference waits under the
// same bound as a first take and is given back with the rest, so the grant always covers the
// heaviest copy held under it.
//
// A wait ctx ends returns ctx's error and leaves the grant exactly as it was: a holder keeps what
// it already held, since the copies it covers are built and a caller refused here abandons the
// copy it was about to build, not the ones it holds.
func (g *loadSlot) take(ctx context.Context, bytes int64) error {
	if g.held {
		if bytes <= g.bytes {
			return nil
		}
		more := bytes - g.bytes
		if err := g.acquire(ctx, more); err != nil {
			return err
		}
		g.bytes = bytes
		if g.slots.taken != nil {
			g.slots.taken(g.slots.held.Load())
		}
		return nil
	}
	started := time.Now()
	if err := g.acquire(ctx, bytes); err != nil {
		return err
	}
	g.held, g.bytes, g.since = true, bytes, time.Now()
	g.waited = g.since.Sub(started)
	held := g.slots.held.Load()
	if g.slots.taken != nil {
		g.slots.taken(held)
	}
	return nil
}

// acquire takes bytes of the pool for g, waiting within ctx and telling a test's hook as the wait
// begins, and counts them as held. It is the one place the pool's semaphore is waited on, so a
// first take and the difference a later copy needs are admitted the same way and in the same
// order as every other waiter (semaphore.Weighted).
func (g *loadSlot) acquire(ctx context.Context, bytes int64) error {
	slots := g.slots.semaphore()
	if !slots.TryAcquire(bytes) {
		if g.slots.waiting != nil {
			g.slots.waiting()
		}
		if err := slots.Acquire(ctx, bytes); err != nil {
			return err
		}
	}
	g.slots.held.Add(bytes)
	return nil
}

// release gives g's bytes back, exactly the weight it took, and logs the grant where it waited
// long (logLoadSlot). A holder of nothing gives back nothing.
func (g *loadSlot) release() {
	if g == nil || !g.held {
		return
	}
	g.slots.held.Add(-g.bytes)
	g.slots.semaphore().Release(g.bytes)
	logLoadSlot(g.bytes, g.slots.poolBytes(), g.waited, time.Since(g.since))
	g.held, g.bytes = false, 0
}

// tryTake makes g, which holds nothing, hold bytes of the pool where that much of it is free at
// once, and reports whether it does. It never waits, so a decode that may not wait still runs
// under a grant wherever the pool has room for one (holdFreeLoad), and holds nothing when it
// reports false.
func (g *loadSlot) tryTake(bytes int64) bool {
	if g.held {
		return true
	}
	if !g.slots.semaphore().TryAcquire(bytes) {
		return false
	}
	g.held, g.bytes, g.since, g.waited = true, bytes, time.Now(), 0
	held := g.slots.held.Add(bytes)
	if g.slots.taken != nil {
		g.slots.taken(held)
	}
	return true
}

// loadSlotKey is the context key a slot travels under (carriedBy).
type loadSlotKey struct{}

// carriedBy is ctx carrying g, so a decode under it covers itself on g (holdLoad) rather than
// take bytes of its own inside g's.
func (g *loadSlot) carriedBy(ctx context.Context) context.Context {
	return context.WithValue(ctx, loadSlotKey{}, g)
}

// withLoadSlot is ctx carrying a grant, and the grant: the one ctx already carries, which its
// owner releases, with a done that releases nothing, or a new one, with a done that releases it.
// A decode under ctx covers itself on that grant (holdLoad), so the decodes one holder makes in
// turn share the one grant rather than wait for a second inside the first.
func withLoadSlot(ctx context.Context) (context.Context, *loadSlot, func()) {
	if slot, ok := ctx.Value(loadSlotKey{}).(*loadSlot); ok {
		return ctx, slot, func() {}
	}
	slot := newLoadSlot()
	return slot.carriedBy(ctx), slot, slot.release
}

// holdLoad covers a whole-document decode on the grant ctx carries, or on one of its own of
// bytes - what the copy costs (copyWeight), or loadSlotBytes where the caller can name no state
// - and returns what ends the hold: nothing for a grant ctx carries, whose owner releases it,
// and the release of its own.
//
// A grant ctx carries is its owner's own weight, taken for the copy that owner is building: a
// decode made under it is covered by that weight rather than weighed again, which is what keeps
// a holder from waiting inside its own grant, and bytes is then only what this decode would
// have taken on its own.
func holdLoad(ctx context.Context, bytes int64) (func(), error) {
	ctx, slot, done := withLoadSlot(ctx)
	if err := slot.take(ctx, bytes); err != nil {
		return nil, err
	}
	return done, nil
}

// loadedHistory is a document's stored history, read and decoded under one grant of the pod's
// load concurrency (loadDecodedHistory).
type loadedHistory struct {
	// loaded is what the read returned: the history's bytes and the version they stand at.
	loaded persistence.LoadResult
	// decoded is the decode of those bytes, as a room's load decodes them (decodeStoredCopy),
	// and nothing where refused is not nil. Its doc is nil for an empty history, which decodes
	// into nothing.
	decoded decodedState
	// refused is ygo's refusal of the history, the document's own failure rather than the
	// service's, and nil for one that decoded: a history no room can load, which each reader
	// answers its own way (loadTree's ErrDocumentUnloadable, rebuildSource's reason to rebuild,
	// LoadDoc's failure of the load). A history that decodes here and parks past a room's cap is
	// no more loadable, and is not refused here: each reader judges decoded against the room's
	// caps itself (parkedPastRoomCaps), and the rebuild builds from that decode.
	refused error
}

// loadDecodedHistory reads artifactID's stored history through load and decodes it, both covered
// by one grant of the pod's load concurrency: the grant ctx carries, whose owner gives it back,
// or one of its own, which it gives back before it returns, so no caller releases a grant by
// hand (withLoadSlot). It is the one way the service reads a history it decodes - a socket's
// admission (loadTree), a room's load (loadDecoded) and a rebuild's source (rebuildSource) - so
// that no new way out of one of them can leave a grant held. It decodes under maxPending, the
// pending queue the service that reads decodes its copies of stored state under
// (Service.maxPendingItems), which parks what the history parks, for the reader to hold against a
// room's caps (parkedPastRoomCaps).
// The grant is the history's own weight, which the read has in hand by the time the grant is
// taken: its bytes and the structs their headers declare (storedCopyWeight), read before anything
// decodes it, so a history heavy in structs is weighed by them; a room of a mebibyte of prose loads
// beside thirteen others rather than behind them.
//
// refuse, where it is not nil, is read from the history before the grant is taken and before
// anything is decoded (loadableHistory), for a history no decode should be paid for.
//
// The read's fold, when the history holds several updates, takes the grant on the rows it reads -
// their bytes, and the structs their headers declare where it holds them in memory (foldRead) -
// and the decode runs inside it. The error is the read's, the refusal's or the grant's, a wait ctx
// ended included, and leaves nothing decoded; ygo's refusal of the bytes themselves is the
// history's refused. A history whose runs do not apply in order is the read's error
// (unappliedHistory): the store has no bytes of it to decode.
func loadDecodedHistory(ctx context.Context, load VersionedStore, artifactID string, maxPending int, refuse func(update []byte) error) (loadedHistory, error) {
	ctx, slot, done := withLoadSlot(ctx)
	defer done()
	loaded, err := load.Load(ctx, artifactID)
	if err == nil && refuse != nil {
		err = refuse(loaded.Update)
	}
	if err == nil {
		err = slot.take(ctx, storedCopyWeight(loaded.Update))
	}
	if err != nil {
		return loadedHistory{}, err
	}
	decoded, refused := decodeStoredCopy(loaded.Update, maxPending)
	return loadedHistory{loaded: loaded, decoded: decoded, refused: refused}, nil
}

// holdServerLoad is holdLoad for a decode of the server's own that no request waits on: a write's
// fork read outside a transaction, a server write's decode-cap copy, the copy of a room loaded
// without this service's persistence, and the decode a room's persistence worker makes of each
// heavy update it stores (StoreUpdateStored, AppendUpdatesWithCredit, heavyUpdateWeight). Its wait
// has no deadline, and ends, since every holder gives its bytes back (loadSlots).
//
// It takes ctx for the grant ctx carries alone, never for its deadline: a decode made inside a
// live write - its fork's own (buildForkHeld) and the decode-cap copy of the room it was read
// from (structsAfter) - covers the transaction's grant, so it waits for nothing and the
// transaction holds one grant from its first fork's build to its last fork's release. A caller
// that carries none - the three server writes outside applyLive, a socket's own copy, a room's
// persistence worker - takes bytes of its own and waits for them without a deadline, and is never
// refused.
func holdServerLoad(ctx context.Context, bytes int64) func() {
	release, _ := holdLoad(context.WithoutCancel(ctx), bytes)
	return release
}

// holdFreeLoad covers a whole-document decode on bytes of the pool that are free at once, and
// runs beside the pool's holders where they are not. It is what the one decode that may not wait
// takes: the replica's build from a base (renderedReplica.start), which runs under the replica's
// lock. A live write holds its grant for its fork's whole life and waits for that lock to clone
// the replica (warmFork), so a wait here would hold that write and that replica against each
// other, each waiting for what the other holds.
//
// What such a decode holds outside the pool is one room's own second copy and the decoder above
// it (roomCopies), which the room budget charges where the replica keeps it; and a room builds
// its replica from a base only after a recover or a load that handed it no copy, never while it
// is typed into.
func holdFreeLoad(bytes int64) func() {
	slot := newLoadSlot()
	if !slot.tryTake(bytes) {
		return func() {}
	}
	return slot.release
}

// copyOfRoom is what one whole copy of the room holding live costs the pod's load concurrency: a
// live write's fork of it, cloned or decoded (warmFork), and the decode-cap count's copy
// (structsAfter). It is what the room holds and the structs it holds at copyWeight - the bytes of
// the stored state the room's load decoded plus what each commit since added as the library
// reports it, with what its pending queue holds beside them (roomResidentBytes, residentBytes),
// and the structs the room's last committed apply reported it holding (roomApplies.structsHeld,
// crdt.ApplyReport.Structs), which a split-dense copy is as heavy in the pool by as it is in the
// budget. Both are read off the room's own listing without a lock, an encode or a walk of the
// document.
//
// A copy carries nothing of the room's pending queue: a fork is the room's replica cloned, and the
// replica applies only what each commit integrated (renderedReplica.takeLocked), and the count's
// copy is a decode of the room's encoded state, which leaves what the room parks out
// (leaveParkedOut). Both figures count the queue all the same - its content in the resident bytes
// and its structs in the report - so a room holding a parked backlog is weighed past what its copy
// holds by that backlog, the safe side for a bound on the pool, and the room keeps no struct
// figure without the queue to weigh it more closely. The delete ranges the queue parks are in
// neither figure, and no copy holds them either.
//
// A document no room's load listed is tracked at nothing, and nothing here can name its size
// without encoding it, so a copy of one takes a whole slot: the writes that reach this on such a
// document are a test's own forks and a server write on a document whose room has just been
// evicted, and one slot is what every taker took before this was weighed at all.
func (s *Service) copyOfRoom(live *crdt.Doc) int64 {
	weighing := s.roomWeighingOf(live)
	if tracked := weighing.roomResidentBytes(); tracked > 0 {
		return copyWeight(tracked, int64(weighing.applies().structsHeld(live)))
	}
	return loadSlotBytes
}

// loadSlotLogged is how long a holder of a grant may have waited for it before its release is
// logged (logLoadSlot): a tenth of a second, past any wait on a lock or the scheduler and short
// of the second or more a heavy apply or decode takes, so the line names the turns a burst of
// heavy work made a holder wait for and stays quiet while the pool is not crowded.
const loadSlotLogged = 100 * time.Millisecond

// logLoadSlot logs a grant of the pod's load concurrency as it is given back, where it waited
// loadSlotLogged or longer for it: how long it waited, how long it held it, the bytes of the
// pool it held and the bytes the whole pool has, the line's time being when it gave them back.
// An operator reads from it how long a burst of heavy work made each holder wait its turn and
// how much of the pool each of them was holding; a real-process drive reads from it which
// holders ran at once and the longest wait (cmd/dispatch,
// TestTheHeaviestApplyBesideAFullCopyBoundStaysUnderThePodsMemory).
func logLoadSlot(bytes, pool int64, waited, held time.Duration) {
	if waited < loadSlotLogged {
		return
	}
	slog.Info("dispatch: a load slot was given back",
		"bound", loadSlotsName, "bytes", bytes, "pool", pool, "waited", waited, "held", held)
}
