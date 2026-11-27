package docs

import (
	"context"
	"errors"
	"fmt"
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
// history of several updates, the copy LoadDocContext decodes, the load hook's decode into the room,
// bootstrapLoadedState), the fold of a cold read, a version read or a compaction (foldRead) - a
// cold read keeps its fold's grant while it reads the tree of the document it folded, renders it
// and walks it (foldDocument, foldLoaded), and a read of a room behind the stored head keeps it
// while it merges what that room has not stored, applies the merge to the document and builds its
// tree, raised where that merge or that apply is heavy (storedWithUpdates) - the replica's build
// from a base (renderedReplica.start), the merge of a room's not-yet-stored updates a share
// answers another process with (unstoredShare), the copy a server write's decode-cap count takes
// (structsAfter), a rebuild's decodes (its check of a resident room and of the stored history,
// nearACap, rebuildSource, which keeps its grant while it reads the history's margin records and
// tree, RebuildTx), the read-back of each heavy update a room's persistence worker stores
// (StoreUpdateStored, AppendUpdatesWithCredit, heavyUpdateWeight), a published write's decode of
// the stored document it judges the write's text on (lostAtStoredHead) and the append gate's fold
// of a history replaced under a room and its decode of the run onto the document that fold makes
// (storedHistoryHolds), which share one grant holding that document from its first row until the
// run has been judged against it. The replica's build may not wait, so it takes its bytes only
// where they are free at once and otherwise decodes beside the pool's holders (holdFreeLoad); the
// read-back of a lighter update takes nothing, as its apply takes nothing, and a merge of lighter
// ones nothing more (heavyUpdateWeight, mergeWeight); every other decode waits for its bytes. What
// each leaves behind is its owner's: a room's two copies are charged by the room budget, a fold's
// state goes back to its caller, and a read-back's copy and the gate's document are dropped once
// their decodes are done.
//
// The second is a live write's fork, held for the fork's whole life rather than for a decode
// alone: from the moment before the fork is built - cloned from the room's replica (warmFork,
// Service.forkPoint) or decoded from the store or from the copy a read took of a room whose
// replica held no state (coldFork, rebuildFork, buildForkHeld) - through the trees its
// operations read off it and the markdown they render, until the fork is dropped
// (liveWrite.releaseFork). A write never gives its grant back while it holds a fork, and the
// decodes it makes inside the grant it already holds take nothing more, but where they weigh more
// than it holds - the decode-cap count's copy of a room that grew since the fork, a warm fork's
// clone of a room that grew while its cover waited (Service.forkPoint) or decode of the copy its
// read took of such a room (buildForkHeld), a later fork of the same transaction, which shares its
// grant, and a read the transaction makes beside its fork - each of which takes the difference
// where the pool has it free at once and is refused otherwise (withLoadSlot, holdLoad,
// leavesRoomPastDecodeCap, liveWrite.coverFork).
//
// The third is one apply of a browser's update over unslottedApplyBytes, or declaring more than
// unslottedApplyStructs structs, held from before the hook reads the update until the room's
// update observer for the apply's commit has decoded that commit into the room's replica; where
// the apply was refused or rolled back, until the room has taken it back out, before the hook
// judges the refusal, whose judgement of a used-up room's scan total waits for a grant of its own
// (rebuildTakes); and where it changed nothing, until the hook returns (Service.weighPeerUpdate,
// roomApplies.releaseGrant): a reconnect storm's whole-state handoffs then decode as many at a
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
// Every taker takes its bytes before any connection it would wait for while it holds them, but a
// connection of the rooms pool, which a holder of the pool can need while it holds - a
// transaction's cold read of a later document - and whose holders wait for nothing of the pool
// (loadSlots sets the order), and a wait a request's context ends is answered errWriteBusy, the
// API's 503 DOC_SERVICE_UNAVAILABLE. The copy a read of a resident room takes while the room's
// replica is busy (holdLive, snapshotDocument) takes nothing: its callers hand it no request
// context to bound a wait by.

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
// Two decodes of a browser's update make this judgement: its apply's, on the frame
// (Service.weighPeerUpdate), and the room's persistence worker's, on the commit it reads back as
// it stores it (StoreUpdateStored, AppendUpdatesWithCredit); so do the merges of what a room has
// not stored yet, on each update they merge (mergeWeight) - a read's of a room behind the stored
// head (storedWithUpdates), which judges its apply of the merge to the stored document too, and a
// share's answered to another process (unstoredShare). A keystroke takes nothing at any of them,
// so its store never waits behind the heavy takers queued for the pool, as its apply never does.
// The room's replica decodes the commit's update too, and makes no judgement: it applies it to its
// own document (renderedReplica.applyUpdate) in the room's update observer, which for a heavy
// apply's commit runs inside that apply's grant and gives it back as it ends
// (roomApplies.releaseGrant), or in a read at an exact commit position that drains the room's log
// first (renderedReplica.atPoint), and takes nothing of the pool of its own for it, heavy update
// or not.
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

// holdUpdateLoad holds what a decode of update, a browser's, takes of the pod's load concurrency
// as heavyUpdateWeight judges it, and returns what ends the hold: a heavy update's copy cost,
// waited for without a deadline (holdServerLoad), and nothing for a lighter one. A lighter
// update's decode never touches the pool, since even a take of nothing waits while takers are
// queued for it (semaphore.Weighted.TryAcquire), and a keystroke's decode must not.
func holdUpdateLoad(ctx context.Context, update []byte) func() {
	if weight, heavy := heavyUpdateWeight(update); heavy {
		return holdServerLoad(ctx, weight)
	}
	return func() {}
}

// mergeWeight is what a merge of updates, the ones a resident room holds that the store may not
// (unstoredParts), takes of the pod's load concurrency: the merge decodes every struct of every
// one of them (crdt.MergeUpdatesV1), so it takes what each heavy one's decode costs, summed
// (heavyUpdateWeight), up to one slot, as every grant is capped, and nothing for a lighter one,
// whose apply took nothing either. A share of lighter updates alone takes nothing, and so does one
// of a single update, which mergeUpdates hands back as it is, decoding nothing.
func mergeWeight(updates [][]byte) int64 {
	if len(updates) < 2 {
		return 0
	}
	var weight int64
	for _, update := range updates {
		if each, heavy := heavyUpdateWeight(update); heavy {
			weight += each
		}
	}
	return min(weight, loadSlotBytes)
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
// nothing another holder or a waiter can hold for good, and it waits for no more of the pool while
// it holds some. The order of the queue is what makes that last rule bind: a holder queued for
// more waits behind every waiter ahead of it, whatever is free, so a holder that queued for more
// behind a waiter needing the bytes it holds would wait on that waiter, and the waiter on it, with
// most of the pool free. That is what a rewritten document's drive met in CI: a settlement's room
// load held about 377 MB, a write's cold fork queued for 377 MB with 223 MB free, and the load's
// read-back queued behind the write for 39 MB more, until the write's 45-second bound answered it
// 503 (dispatch://INFRA-179, job 136940412632).
//
// The rule is take's, not each caller's: a take on a grant that already holds bytes takes the
// difference only where the pool gives it at once, and is refused otherwise (errGrantShort). A
// holder that holds nothing it built under its grant gives the grant back and takes the total
// whole instead, waiting holding nothing (retake): a fold's read-back, whose own document is
// dropped by then, the decode of a log's runs, the decode of a history its read handed back, a
// write's first fork, and a state answer's grant topped up. A holder that holds what it built
// drops it, takes the total whole and does its work again under it (redoWhole): a read of a room
// behind the stored head (storedWithUpdates) and the append gate's check of a replaced history
// (storedHistoryHolds). A write's decode-cap count holds the write's fork, which it cannot give
// back, so the one total it cannot have at once refuses the write busy (leavesRoomPastDecodeCap).
//
// A transaction holds one grant, whichever documents it forks and reads (Ledger.forkSlot). Join's
// context carries it, so each fork of the transaction covers itself on it (liveWrite.coverFork),
// and so does each decode a read under the transaction makes (withLoadSlot), rather than wait for
// a grant of its own beside the one the transaction holds. Its first take waits holding nothing
// of the pool, as every first take does: the first fork's cover, which, where the pool does not
// give it what it weighs past what the fork's own read took, gives that back and takes the fork's
// weight whole (retake), or a read's while no fork stands, which gives the grant back as it ends.
// Every take after that while a fork stands - a later document's fork and the cold read that
// builds it, a read of another document, the decode-cap count's copy, a warm fork's clone of a
// room that grew while its cover waited (Service.forkPoint) - takes the difference at once or is
// refused busy (errGrantShort), the 503 its caller retries. None of them gives the grant back to
// take it whole: a fork stands under it, and the bytes that fork holds would go uncounted while it
// waited. A transaction that knows, before its first fork, every document it is about to write -
// an issue's close, reopen, sweep and repair, which write its ask states into each of its
// documents in turn (Docs.WriteIssueAskStates) - weighs them all first, and its first fork takes
// their sum, capped at one slot as every weight is (Service.forksAhead), so a later fork takes
// nothing more unless it takes more than it was weighed at (forksAhead lists when).
//
// A decode's grant, held between its take and its release, decodes, renders or reads the rows of
// the transaction it already holds, takes no lock held across a wait, takes no pooled connection
// but one of the rooms pool, whose holders wait for nothing of the pool (below) - a fold of a
// document load covers the grant holding no connection, its rows read and its rooms transaction
// committed or not yet begun (loadStored, foldRead), a compaction's fold its own compaction
// connection alone, and a room's load gives its grant back before onLoadDocument reads the store -
// and runs no update observer, since ygo decodes nothing of a loading room's state and no holder
// commits on a resident room.
//
// A live write's grant is held far longer - from before its fork is built until its fork is
// dropped - so the things it holds it across are named here too, each with what keeps that wait
// from being a cycle:
//
//   - its own transaction, its owner row and the document's advisory lock, all taken before the
//     grant, and its writer slot, also taken before it (openLiveWrite): a waiter for a writer
//     slot holds nothing of the pool, since it has not forked yet. A transaction's later document
//     is the exception: its owner row, its writer slot and its advisory lock are waited for while
//     the transaction holds the grant its first document's fork took (forkSlot, joinLiveWrite,
//     readFork), though never more of the pool (above). A waiter of the pool can hold that
//     advisory lock - another live write of the document waiting for its fork's bytes
//     (warmFork), or the room's persistence worker in its gate's fold (AppendRoomFlush,
//     storedHistoryHolds) - so the transaction can wait for a lock whose holder waits for the
//     bytes it holds. Join's WriteRequestBound ends that wait for a caller's request: the
//     transaction is answered 503 and gives its grant back
//     (TestAJoinedTransactionsWaitForItsSecondDocumentsLockEndsAtTheWriteBound). The server's own
//     two such transactions, the close's sweep and each issue's of the ask repair, run under a
//     deadline of their own, WriteRequestBound from their begin, which ends it for them at the
//     latest: each rolls back and gives its grant back, and the ask repair's next pass takes up
//     what it left (api's sweepClosedIssueAsks, repairIssueAsksOf).
//   - a slot of the buffered folds (foldSlots) and a room's load, neither of which it waits for
//     while its grant holds bytes, but for the one case below. A holder of a fold slot, and a
//     room's load, can wait for the pool, so the transaction takes a fold slot only where one is
//     free at once - where none is, its document load streams its rows on one whole slot the pool
//     gives it at once, or is refused busy (takeFoldSlot, loadStoredOnce, errFoldSlotsBusy) - and
//     loads no room: a read under it of a document no room holds here reads the stored document on
//     the transaction's own grant (docTree, captureLiveTextAndAuthors, storedTree), and an Apply
//     that would load one is refused (allowApply, errRoomLoadUnderGrant), each the 503 its caller
//     retries (transactionHolds). A room the transaction found resident that ygo's idle sweep
//     retires, or a close the service makes takes out, before ygo looks it up for the Apply is
//     refused at its load the same way (servicePersistenceAdapter.LoadDocContext) - unless another
//     caller's load of that room is already under way by then, which the Apply waits on holding
//     the grant, until the transaction's write bound at the latest: a stated risk, which allowApply
//     describes.
//   - a connection of the rooms pool, for the cold read of a later document or a read beside its
//     fork (coldFork, coldRead, storedTree). No holder of one waits for the pool or a fold slot
//     (the order below), so that wait ends.
//   - the room replica's lock, waited for to clone the replica (warmFork). Nothing under that
//     lock waits for the pool: the one decode there takes what it needs where the pool has room
//     at once and otherwise runs beside its holders (renderedReplica.start, holdFreeLoad).
//   - the publish of its own committed updates, which may load a room and so take a grant of
//     that load's own. The transaction drops every fork, and with them its grant, before it
//     publishes any of them (Ledger.publish).
//
// An apply's grant is given back at the end of the room's own update observer for the commit the
// apply makes, once that observer has decoded the commit into the room's replica, a decode the
// grant covers (Service.weighPeerUpdate, roomApplies.releaseGrant), and an apply refused or rolled
// back gives it back before its refusal is judged, whose judgement of a used-up room's scan total
// waits for a grant of its own (rebuildTakes). ygo runs that observer before
// its persistence observer, on the goroutine that commits: the room registers it in its load hook,
// ygo registers the persistence observer once that hook has returned
// (provider/websocket/server.go, loadRoom), and ygo calls a document's update observers in the
// order they were registered. The persistence observer hands the commit to the room's persistence
// worker through a queue of 256 updates and waits while that queue is full, and once the worker
// has begun to exit waits for it to finish, the compaction it runs as it exits included (ygo's
// persistStranded, which a socket still committing at ygo's shutdown reaches). So the worker waits
// for its bytes as every other decode does - its read-back of each heavy update it stores
// (StoreUpdateStored, AppendUpdatesWithCredit), its gate's decodes of a history replaced under the
// room (storedHistoryHolds) and its compaction as it exits (servicePersistenceAdapter.Compact).
// Its gate's decodes wait holding the document's lock and a connection of the room-appends pool
// (AppendRoomFlush), and one holder of the pool waits for that lock: a transaction that holds the
// grant its first document's fork took and waits for its later document's advisory lock
// (joinLiveWrite, readFork), though never for more of the pool, so the worker can wait for bytes
// that transaction holds while the transaction waits for the worker's lock. Nothing in that pair
// gives way; Join's WriteRequestBound ends the transaction's wait for a caller's request, which is
// answered 503 and gives its grant back, the transaction's own deadline ends it at the latest for
// the server's two such transactions, and the worker then takes its bytes (above). No other
// holder waits on the worker: an apply holds no grant by the time its commit reaches that queue, a
// live write publishes once its transaction has given its grant back (Ledger.publish), a server
// write's decode-cap copy gives its grant back before the write commits (structsAfter), and no
// holder waits for a failed room's recovery, whose eviction waits for the worker's exit and so
// for that compaction: a transaction refuses the failed room, and so does a read whose grant
// holds bytes, a socket admission's (awaitRoomRecovery, heldGrant).
// Before it gives its grant back, the room's observer waits for the replica's lock, and no holder
// of that lock waits for the pool (renderedReplica.start, holdFreeLoad, above), so that wait ends.
//
// No holder takes a second grant: a decode under a context carrying one covers itself on that one
// (withLoadSlot), so a room load's fold, its copy's decode and the load hook's decode into the
// room share the load's, and a write's fork decode and its decode-cap copy share the write's;
// one transaction's forks and reads share one grant (forkSlot), which Join's context carries,
// so a transaction writing two documents, or reading one beside its fork of another, never waits
// for the bytes its own fork holds, nor for a grant of its own beside them; and a refused apply
// gives its grant back before its refusal's re-scan takes one (weighPeerUpdate, rebuildTakes). A
// transaction holding its grant that applies to a room it found resident (allowApply) loads none
// either where ygo's idle sweep retires that room, or a close the service makes takes it out,
// before ygo looks it up: ygo hands the room's load the Apply's context, and the load refuses it
// busy before it reads anything (servicePersistenceAdapter.LoadDocContext, errRoomLoadUnderGrant),
// the 503 that write's caller retries. Where another caller's load of that room is under way by
// then, the Apply takes no grant of its own but waits on that load holding the transaction's, the
// stated risk allowApply describes.
//
// Beside the pool, a holder takes four things in one order, and waits for none of them while it
// holds one that comes later: a slot of the buffered folds (foldSlots), then its grant of the
// pool, then a connection of the rooms pool, then a room's document locks - ygo's lock on the
// room's document, which an Apply and each apply of a browser's update take, and the room
// replica's lock. A fold slot's holder waits for its grant once it holds its rows (foldRead). A
// holder of a grant takes a fold slot only where one is free at once - where none is, a document
// load streams its rows on one whole slot the pool gives it at once and is refused busy otherwise,
// and any other read is refused busy (takeFoldSlot, heldGrant, loadStoredOnce) - and of the other
// two waits for a rooms connection (a transaction's cold read of a later document, coldRead's
// DocumentStamp) and for a room's document locks (warmFork, residentTree, an Apply to a resident
// room, the room's update observer holding a heavy apply's grant). No holder of a rooms connection
// waits for any of the four: a document load takes its fold slot before its rooms connection, and
// the whole slot its streamed rows take before it too, and its buffered fold takes its grant once
// that connection's transaction has committed (loadStored); every other holder of one runs single
// statements on it. No holder of a room's document locks waits for any of them: the replica's one
// decode under its lock takes only bytes free at once (renderedReplica.start, holdFreeLoad), a
// write's clone under it takes what the room grew past the write's cover only at once
// (forkPoint), and a heavy apply takes its grant before ygo takes the document's lock
// (weighPeerUpdate). An Apply's callback runs outside ygo's document lock, which ygo takes only
// for each transaction the callback makes (transact, ygo's provider/websocket/inject.go): most
// callbacks decode nothing, merging and decoding once their Apply has returned (docTree,
// VerifyMark), but the block-id stamp's and the sweep of unrecorded marks' (stampBlockIDs,
// unmarkExpired) make the decode-cap count of their server write inside the callback before
// their transaction, waiting for the pool with no deadline and holding none of the room's
// document locks while they wait (leavesPastDecodeCap, holdServerLoad). So each wait is on a
// holder whose own waits come later in the order, and each ends - save one: a transaction's Apply
// waiting, holding its grant, on another caller's load of a room that left after its look, which
// ends with that load or else at the transaction's write bound, since the load can be waiting for
// the bytes the transaction holds (allowApply).
//
// A write's database locks for its first document - its transaction's connection, its owner row,
// its writer slot and the document's advisory lock (openLiveWrite, readFork) - come before all
// four, and so do the append gate's (the document's advisory lock and a connection of the
// room-appends pool, AppendRoomFlush, storedHistoryHolds) and a compaction's (its compaction
// connection, and the document's advisory lock for its write): a holder of the pool waits for
// none of them, nor for a connection of the room-appends pool or the compaction pool. The one
// exception is a transaction's later document, whose owner row, writer slot and advisory lock it
// waits for holding the grant its first document's fork took (above), and which Join's
// WriteRequestBound bounds (TestAJoinedTransactionsWaitForItsSecondDocumentsLockEndsAtTheWriteBound),
// or the transaction's own deadline for the server's two such transactions (above). A waiter
// whose request ends returns its context's error holding nothing.
type loadSlots struct {
	// size is how many slots the pool is sized from, always set by whoever built it
	// (newLoadSlots): the process's MemoryBounds.LoadSlots, or a test's.
	size int64
	once sync.Once
	free *semaphore.Weighted
	// held is how many bytes of the pool are held now.
	held atomic.Int64
	// waiting runs, in a test, as a taker finds too little of the pool free and starts to wait,
	// with the bytes it asks; taken runs once it holds its own bytes, with the bytes held then.
	waiting func(bytes int64)
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

// take makes g hold bytes of the pool: what the copy it covers costs (copyWeight), or
// loadSlotBytes where its caller can name no state, which is one whole slot and so is always
// admitted on its own. A holder of nothing waits for them within ctx.
//
// A holder that already holds a grant keeps it and takes only the difference, where this copy
// weighs more than the grant covers: one transaction's forks and reads share one grant (forkSlot)
// and every decode made under a carried one covers itself on it (withLoadSlot), so without the
// difference a transaction that wrote a small document and then a large one was charged the
// small one alone - measured at 32,676 bytes of the pool for a pair whose second fork weighed
// 32,148,200, which Docs.WriteIssueAskStates does on every issue close, reopen and sweep
// (dispatch://INFRA-179, review 5820480859's finding 10). The difference is given back with the
// rest, so the grant always covers the heaviest copy held under it.
//
// It never waits for the difference while it holds: it takes it only where the pool gives it at
// once, which is also only while nobody is queued for the pool (semaphore.Weighted.TryAcquire),
// and otherwise answers errGrantShort and leaves the grant as it was (loadSlots says why). Its
// caller then gives the grant back and takes the total whole where it holds nothing it built
// under it (retake), does its work again under the whole grant where it does (redoWhole), or
// refuses. No holder is excepted: a transaction whose fork stands under its grant has its later
// forks and reads refused busy rather than wait (loadSlots).
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
		if !g.slots.semaphore().TryAcquire(more) {
			return errGrantShort
		}
		g.slots.held.Add(more)
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

// errGrantShort is take's refusal of the difference a grant that already holds bytes needs, where
// the pool does not give it at once: a holder waits for no more of the pool while it holds some
// (loadSlots). It is ErrServiceUnavailable, the API's 503 DOC_SERVICE_UNAVAILABLE, wherever it
// reaches a caller: a write whose decode-cap count meets it while the write holds its fork
// (leavesRoomPastDecodeCap); a write's fork decode that weighs more than its cover took
// (buildForkHeld); a transaction's later fork, the cold read that builds it, or a read the
// transaction makes beside a fork that stands, each of which takes on the transaction's grant
// (liveWrite.coverFork, withLoadSlot); a read of a room behind the stored head whose stored
// history grew between two passes of its work (storedWithUpdates, redoWhole); and a check of a
// live room near its caps, or a write's clone of a room, that weighed the room before it waited
// and finds it grew (nearACapOfRoom, forkPoint). A cold read whose load waited past its bound for
// its turn - holding nothing, or holding its grant before its fold began reading the rows - is
// refused with it too, the same 503 (foldDocument). Every other holder that can meet it gives its
// grant back and takes the total whole instead (retake, redoWhole).
var errGrantShort = fmt.Errorf("%w: a holder of the pod's load concurrency needs more of it than is free at once, and waits for no more while it holds some", ErrServiceUnavailable)

// retake makes g hold bytes as take does, for a holder that holds nothing it built under g's
// grant, which is wholly its own: where the pool does not give the difference at once, it gives
// the grant back and takes bytes whole, waiting within ctx as a first take does and holding
// nothing of the pool meanwhile, rather than refusing. What the grant covered is already dropped,
// so nothing goes uncounted while it waits. A wait ctx ends returns ctx's error and leaves g
// holding nothing.
func (g *loadSlot) retake(ctx context.Context, bytes int64) error {
	err := g.take(ctx, bytes)
	if !errors.Is(err, errGrantShort) {
		return err
	}
	g.release()
	return g.take(ctx, bytes)
}

// redoWhole runs work under g's grant: work takes bytes of the pool on g only through the take it
// is handed, each the total g must hold at that point of the work, and drops whatever it built
// before it returns a take's error. Where g held nothing as redoWhole began, a take the pool does
// not give at once (errGrantShort) has redoWhole give the grant back, take that total whole,
// waiting within ctx holding nothing of the pool, and run work again from its start, whose takes
// up to that total then take nothing. A refused total is past the grant the pass ran under, and
// every total is capped at one slot, so the passes end. Where g held bytes as it began, which may
// cover what another holder built, the refusal is work's error, and so is a refusal of a take
// work made on g some other way.
func (g *loadSlot) redoWhole(ctx context.Context, work func(take func(total int64) error) error) error {
	owned := !g.held
	for {
		var short int64
		err := work(func(total int64) error {
			err := g.take(ctx, total)
			if owned && errors.Is(err, errGrantShort) {
				short = total
			}
			return err
		})
		if err == nil || short == 0 {
			return err
		}
		g.release()
		if err := g.take(ctx, short); err != nil {
			return err
		}
	}
}

// ownedKey marks a context whose grant its scope holds alone (ownedBy).
type ownedKey struct{}

// ownedBy is ctx marking g's grant as wholly its scope's own, where g holds nothing yet as the
// scope takes it up - a fold's read (foldRead, MaterializeAt), a read of a history it decodes
// (loadDecodedHistory) or the append gate's check of a replaced history (storedHistoryHolds) - and
// ctx as it is otherwise. Whatever g holds under such a scope then covers what the scope builds
// alone, so a decode in it that holds nothing it built may give the grant back and take what it
// needs whole (unbuiltTake). A grant held as the scope begins may cover a copy another holder
// built - a transaction's fork, beside which the transaction reads - and is never marked.
func (g *loadSlot) ownedBy(ctx context.Context) context.Context {
	if g.held {
		return ctx
	}
	return context.WithValue(ctx, ownedKey{}, g)
}

// unbuiltTake is the take a decode makes on slot, the grant ctx carries, where the decode holds
// nothing it built under that grant as it begins: retake where ctx marks the grant as its scope's
// own (ownedBy), since nothing it covers would go uncounted while it waited holding none of it,
// and take otherwise, since some other holder's copy may stand under it.
func unbuiltTake(ctx context.Context, slot *loadSlot) func(context.Context, int64) error {
	if owner, _ := ctx.Value(ownedKey{}).(*loadSlot); owner == slot {
		return slot.retake
	}
	return slot.take
}

// acquire takes bytes of the pool for g, waiting within ctx and telling a test's hook as the wait
// begins, and counts them as held. It is the one place the pool's semaphore is waited on, and only
// a holder of nothing waits on it (take), so every first take is admitted the same way and in the
// same order as every other waiter (semaphore.Weighted).
func (g *loadSlot) acquire(ctx context.Context, bytes int64) error {
	slots := g.slots.semaphore()
	if !slots.TryAcquire(bytes) {
		if g.slots.waiting != nil {
			g.slots.waiting(bytes)
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

// withLoadSlot is ctx carrying a grant, the grant, and what ends this scope's use of it: the grant
// ctx already carries, which its owner releases, with a done that releases nothing; the grant of
// the transaction Join joined ctx to (transactionGrant), with a done that gives it back where no
// fork of the transaction holds it, so a read under the transaction holds its bytes no longer than
// a grant of its own would have; or a new one, with a done that releases it. A decode under ctx
// covers itself on that grant (holdLoad), so the decodes one holder makes in turn share the one
// grant rather than wait for a second inside the first.
func withLoadSlot(ctx context.Context) (context.Context, *loadSlot, func()) {
	if slot, ok := ctx.Value(loadSlotKey{}).(*loadSlot); ok {
		return ctx, slot, func() {}
	}
	if grant := transactionGrant(ctx); grant != nil {
		return grant.slot.carriedBy(ctx), grant.slot, grant.releaseUnforked
	}
	slot := newLoadSlot()
	return slot.carriedBy(ctx), slot, slot.release
}

// carriesLoadGrant reports whether a decode under ctx covers itself on a grant its caller holds
// (withLoadSlot) rather than on one of its own: a grant ctx carries, or the grant of the
// transaction Join joined ctx to, made or not.
func carriesLoadGrant(ctx context.Context) bool {
	if _, carried := ctx.Value(loadSlotKey{}).(*loadSlot); carried {
		return true
	}
	_, joined := txFromContext(ctx)
	return joined
}

// transactionHolds reports whether ctx is joined to a transaction whose grant holds bytes of the
// pool (Ledger.forkSlot): a fork of the transaction stands under it, or a read of the transaction
// holds what it took. Such a transaction waits for nothing another holder of the pool can hold
// while it waits for the pool (loadSlots): it takes more of its grant only at once (take), takes a
// slot of the buffered folds only at once (takeFoldSlot, heldGrant), and loads no room
// (allowApply, servicePersistenceAdapter.LoadDocContext, docTree, captureLiveTextAndAuthors,
// VerifyMark).
func transactionHolds(ctx context.Context) bool {
	ledger := ledgerFrom(ctx)
	return ledger != nil && ledger.tx != nil && ledger.forks != nil && ledger.forks.slot.held
}

// heldGrant is the grant a decode under ctx covers itself on (withLoadSlot) where that grant
// holds bytes of the pool, and nil where it holds none or ctx has none: the grant ctx carries, or
// where it carries none the grant of the transaction Join joined ctx to (transactionHolds). Its
// holder waits for no slot of the buffered folds (takeFoldSlot) and takes more of the pool only at
// once (loadSlot.take), as a write's clone of a room takes what the room grew while the write's
// cover waited (forkPoint).
func heldGrant(ctx context.Context) *loadSlot {
	if slot, ok := ctx.Value(loadSlotKey{}).(*loadSlot); ok {
		if slot.held {
			return slot
		}
		return nil
	}
	if transactionHolds(ctx) {
		return ledgerFrom(ctx).forks.slot
	}
	return nil
}

// RefusedBusy reports whether err is a holder's refusal of what it would otherwise have waited for
// holding its grant (transactionHolds): more of the pool (errGrantShort) or a slot of the buffered
// folds (errFoldSlotsBusy); a room's load refused to a caller that may load no room - a transaction
// holding its grant, or the awareness relay's Inject - which every caller waiting on that load
// meets too (errRoomLoadUnderGrant); a transaction's first fork whose wait for the pool ended at a
// deadline - the server's wait bound or the transaction's own - before the pool gave it its grant
// (liveWrite.coverFork, errWriteBusy wrapping context.DeadlineExceeded); or a cold read's refusal
// once its wait for its turn passed its bound (foldDocument, errGrantShort). Each is the 503 its
// caller retries and none is the document's own failure, so no room is failed for it, and a
// background duty that meets one logs it at Warn with what retries it, keeping Error for every
// other error (LogDutyFailure); the unrecorded-mark sweep, which meets it again each settle delay
// while it lasts, logs only the first of a run at Warn (sweepUnrecordedMarks). A first fork whose
// caller went away (errWriteBusy wrapping context.Canceled) is not one: nobody waits for its retry.
func RefusedBusy(err error) bool {
	return errors.Is(err, errGrantShort) || errors.Is(err, errFoldSlotsBusy) || errors.Is(err, errRoomLoadUnderGrant) ||
		errors.Is(err, errWriteBusy) && errors.Is(err, context.DeadlineExceeded)
}

// LogDutyFailure logs err, a background duty's failure, as msg with args: at Warn with retry,
// what retries the duty, where err is a busy refusal (RefusedBusy), and at Error otherwise. Its
// callers are api's sweepClosedIssueAsks and repairIssueAsks, and the block index's and the
// reference excerpts' reads.
func LogDutyFailure(msg, retry string, err error, args ...any) {
	if RefusedBusy(err) {
		slog.Warn(msg, append(args, "retry", retry, "error", err)...)
		return
	}
	slog.Error(msg, append(args, "error", err)...)
}

// holdLoad covers a whole-document decode on the grant ctx carries, or on one of its own of
// bytes - what the copy costs (copyWeight), or loadSlotBytes where the caller can name no state
// - and returns what ends the hold: nothing for a grant ctx carries, whose owner releases it,
// and the release of its own.
//
// A grant ctx carries is its owner's own weight, taken for the copy that owner is building: a
// decode made under it is covered by that weight rather than weighed again, which is what keeps
// a holder from waiting inside its own grant, and bytes is then only what this decode would
// have taken on its own. Where bytes is past that weight, the take is the difference, which the
// grant takes only where the pool gives it at once and which is refused otherwise (take,
// errGrantShort).
func holdLoad(ctx context.Context, bytes int64) (func(), error) {
	ctx, slot, done := withLoadSlot(ctx)
	if err := slot.take(ctx, bytes); err != nil {
		return nil, err
	}
	return done, nil
}

// holdUnbuiltLoad is holdLoad for a decode that holds nothing it built under the grant ctx
// carries as it begins: a fold's read-back, whose own document is dropped by then (fold), and the
// decode of a log's runs where the fold did not read back (runsDocument). Where that grant is
// wholly its scope's own (ownedBy), a difference the pool does not give at once gives the grant
// back and takes bytes whole, waiting holding nothing of the pool (unbuiltTake, retake), since
// nothing it covered is held to go uncounted. A grant some other holder's copy may stand under is
// taken as holdLoad takes it.
func holdUnbuiltLoad(ctx context.Context, bytes int64) (func(), error) {
	ctx, slot, done := withLoadSlot(ctx)
	if err := unbuiltTake(ctx, slot)(ctx, bytes); err != nil {
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
	// LoadDocContext's failure of the load). A history that decodes here and parks past a room's
	// cap is no more loadable, and is not refused here: each reader judges decoded against the
	// room's caps itself (parkedPastRoomCaps), and the rebuild builds from that decode.
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
// and the decode runs inside it. Where the grant held nothing as this began, it is this read's own
// (ownedBy), and a history weighing more than the fold left it holding takes the difference as
// the fold's read-back does: at once, or by giving the grant back and taking the history's weight
// whole, since the read handed back bytes and nothing decoded (unbuiltTake, retake). The error is
// the read's, the refusal's or the grant's, a wait ctx ended included, and leaves nothing decoded;
// ygo's refusal of the bytes themselves is the history's refused. A history whose runs do not
// apply in order is the read's error (unappliedHistory): the store has no bytes of it to decode.
func loadDecodedHistory(ctx context.Context, load VersionedStore, artifactID string, maxPending int, refuse func(update []byte) error) (loadedHistory, error) {
	ctx, slot, done := withLoadSlot(ctx)
	defer done()
	ctx = slot.ownedBy(ctx)
	take := unbuiltTake(ctx, slot)
	loaded, err := load.Load(ctx, artifactID)
	if err == nil && refuse != nil {
		err = refuse(loaded.Update)
	}
	if err == nil {
		err = take(ctx, storedCopyWeight(loaded.Update))
	}
	if err != nil {
		return loadedHistory{}, err
	}
	decoded, refused := decodeStoredCopy(loaded.Update, maxPending)
	return loadedHistory{loaded: loaded, decoded: decoded, refused: refused}, nil
}

// holdServerLoad is holdLoad for a decode of the server's own that no request waits on: a server
// write's decode-cap copy, the copy of a room loaded without this service's persistence, and the
// decode a room's persistence worker makes of each heavy update it stores (StoreUpdateStored,
// AppendUpdatesWithCredit, heavyUpdateWeight). Its wait has no deadline, and ends, since every
// holder gives its bytes back (loadSlots).
//
// It takes ctx for the grant ctx carries alone, never for its deadline. A caller that carries
// none - a socket's own copy, a room's persistence worker - takes bytes of its own and waits for
// them, and is never refused. The decode-cap copy (structsAfter) carries one, which has taken the
// copy's bytes before it copies, so its take here takes nothing more: a live write's, which took
// them at once or refused the write (leavesRoomPastDecodeCap), so the transaction holds one grant
// from its first fork's build to its last fork's release; and one of the three server writes
// outside applyLive, which waited for them without a deadline and weighed the room again after
// each wait until they covered it (leavesPastDecodeCap). Where ctx's grant refuses the take
// (errGrantShort), which none of those callers meets, it returns a release of nothing rather than
// nil, so no caller's deferred release panics, and the decode runs beside the pool's holders, as
// one that may not wait does (holdFreeLoad). A decode whose caller can refuse takes its grant
// through holdLoad instead (buildForkHeld).
func holdServerLoad(ctx context.Context, bytes int64) func() {
	release, err := holdLoad(context.WithoutCancel(ctx), bytes)
	if err != nil {
		return func() {}
	}
	return release
}

// holdFreeLoad covers a whole-document decode on bytes of the pool that are free at once, and
// runs beside the pool's holders where they are not. It is what the one decode that may not wait
// takes: the replica's build from a base (renderedReplica.start), which runs under the replica's
// lock. A live write holds its grant for its fork's whole life and waits for that lock to clone
// the replica (warmFork), and the room's update observer waits for it holding a heavy apply's
// grant until it has drained that apply's commit (roomApplies.releaseGrant), so a wait here would
// hold that write or that apply and that replica against each other, each waiting for what the
// other holds.
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
