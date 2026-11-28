package docs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// This file stores a room's browser updates in batches on the room-appends pool
// (store.Pool.RoomAppends): one transaction stores a run of the room's queued updates in the
// room's commit order, one row per update. ygo's persistence worker calls StoreUpdateContext with
// an update; StoreUpdateContext calls AppendRoomFlush, which runs appendRoomTx.
//
// The batch. StoreUpdateContext can wait until roomAppendLinger after the room's observer took
// its update for more of the room's updates, then reads the commit position its batch runs
// through (roomBatchThrough). appendRoomTx reads the run under the document's advisory lock
// (Service.flushRun): the durable classes of the update's room instance queued at a position
// through that one and not marked stored, in the queue's order, which need not be the order of
// the calls.
//
// The gate. Under the same lock, for an append a room cursor gates (appendingThrough),
// appendRoomTx calls gateAppend, which decides which updates of a document whose issue has closed
// are stored: the prefix of the run that observedBeforeClose admits. gateAppend refuses the run
// (ErrIssueClosed) when that prefix is empty. Otherwise appendRoomTx writes the prefix, and once
// the writes pipeline and its commit have returned without error it records the rest of a flush's
// run on the flush (RoomFlush.cut); those classes stay queued, not marked stored.
//
// A nil return. A nil return from StoreUpdateContext does not mean its update is durable. The
// call returns nil without storing its update when the room has failed (its roomFailed checks),
// when the update's class is marked stored or a suppression slot takes an update with no class,
// which leave the update to another writer, and when the gate refuses the update (ErrIssueClosed)
// or cuts it (RoomFlush.cut). It fails the room (failRoom) before it returns an error.

// roomAppendLinger is how long after the room's observer took an update the update's call can
// wait for more of the room's updates before it runs its batch (roomBatchThrough,
// StoreUpdateContext).
const roomAppendLinger = 50 * time.Millisecond

// roomAppendBatchUpdates and roomAppendBatchBytes cap the run batchRunLocked takes from the update
// whose call runs the batch: after that update, it takes another only while the run stays within
// both. The batch also stores the unstored updates queued before that update (flushRun), which
// the caps do not count. queueUpdateClass wakes the waiting lingers when the room's queue reaches
// either.
const (
	roomAppendBatchUpdates = 256
	roomAppendBatchBytes   = 1 << 20
)

// BrowserUpdate is one update an append stores (appendWrites): its bytes, whether it changes the
// document's rendered content, its browser edit's in-flight credit (nil for none), and Observed,
// when the room's observer took it, which gateAppend judges against the document's issue close in
// a browser's append (appendBrowser, observedBeforeClose). Observed is zero for an update that
// carries no observed time, such as the parked bytes AppendParkedUpdate stores; once the issue has
// closed, gateAppend does not admit such an update to a browser's append. Its fields are exported
// so a test store in another package can hand a batch on.
type BrowserUpdate struct {
	Update         []byte
	ContentChanged bool
	Credit         *UpdateCredit
	Observed       time.Time
}

// AppendRoomFlush runs appendRoomTx for flush: one transaction, under the document's advisory
// lock, that stores the run of flush's room instance through `through` read off the room's queue
// (Service.flushRun), or the prefix of it the gate admits (gateAppend). The update bytes are not
// passed: the queue holds them. It returns the last version the transaction appended, and zero
// when it appended none or failed, with appendRoomTx's error. StoreUpdateContext calls it, and
// credit is the calling update's credit: appendRoomTx lands it once holdRoom has succeeded, and
// StoreUpdateContext lands it as it returns.
func (p *PgVersioned) AppendRoomFlush(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit) (persistence.Version, error) {
	_, err := p.appendRoomTx(ctx, room, flush, through, credit, nil, true)
	if err != nil || len(flush.versions) == 0 {
		return 0, err
	}
	return flush.versions[len(flush.versions)-1], nil
}

// AppendUpdatesWithCredit validates each of updates on its own (validateUpdate), stores them, or
// the prefix the gate admits (gateAppend), in one appendRoomTx transaction gated on the room
// cursor ctx carries, if any, and returns the version the first was stored at. It returns an
// error before any transaction when updates is empty or one does not validate.
// StoreUpdateContext calls it for an update no class holds.
func (p *PgVersioned) AppendUpdatesWithCredit(ctx context.Context, room string, updates []BrowserUpdate) (persistence.Version, error) {
	return p.appendUpdates(ctx, room, updates, true)
}

// appendUpdates is AppendUpdatesWithCredit with owes, whether the transaction marks the document
// owing a settlement; AppendParkedUpdate calls it with owes false.
func (p *PgVersioned) appendUpdates(ctx context.Context, room string, updates []BrowserUpdate, owes bool) (persistence.Version, error) {
	if len(updates) == 0 {
		return 0, fmt.Errorf("append document updates: no update to append")
	}
	for _, update := range updates {
		if err := validateUpdate(update.Update, p.maxPendingItems); err != nil {
			return 0, err
		}
	}
	return p.appendRoomTx(ctx, room, nil, 0, nil, updates, owes)
}

// appendRoomTx stores a run of room's updates in one transaction and returns the version the
// run's first update was stored at, zero for an empty run, which it does not commit. For a flush
// the run is flush's classes through `through`, read off the room's queue under the document's
// lock (Service.flushRun); otherwise it is given. owes is whether the transaction marks the
// document owing a settlement (appendWrites).
//
// It takes the room's in-process lock and then a connection of the room-appends pool
// (store.Pool.RoomAppends), through holdRoom, and sends on that connection, in order: a pipeline
// that takes the document's session-level advisory lock, begins and reads the gate
// (queueAppendGate); the gate read again under a prune checkpoint's ceiling when the first read
// finds one; the reads gateAppend makes of a replaced history (storedHistoryHolds); and, once
// gateAppend has passed the run where a room cursor gates it, a pipeline of the writes
// (appendWrites) and the commit. It takes no pg_advisory_xact_lock: the session holds
// the document's lock already.
//
// Once the writes pipeline and its commit have returned without error, before the unlock, it
// records what it stored: for a flush, the run and its cut on flush (RoomFlush.appendedRun,
// RoomFlush.cut), the versions on the instance's cursor, the stored classes marked stored with
// each credit held and landed (RoomFlush.markStored), the room's shadow dropped if drained
// (dropDrainedShadowLocked) and the batch counted
// (committed); for given updates, their credits held and the versions on ctx's room cursor. Then
// finish, which runs once the opening pipeline was sent unless that pipeline failed with a
// context error, lands the credits, attempts a rollback of a transaction still open and calls
// freeRoomLock; holdRoom's release gives up the in-process lock; and a committed transaction
// notes its last version for compaction (PgVersioned.noted).
//
// On an error it returns zero and the error of holdRoom, a pipeline, the gate read, gateAppend
// (ErrIssueClosed when it admits no update of the run, ErrRoomBehindHead) or appendWrites.
//
// The prune's deletes go in the writes pipeline, not between the gate reads: a delete assigns the
// transaction an id, and there it would hold the id across the gate's decode (storedHistoryHolds,
// INFRA-636). The opening pipeline's lock, begin and reads assign none.
func (p *PgVersioned) appendRoomTx(ctx context.Context, room string, flush *RoomFlush, through uint64, credit *UpdateCredit, given []BrowserUpdate, owes bool) (first persistence.Version, err error) {
	conn, ctx, release, err := p.holdRoom(ctx, room, true, func(ctx context.Context) (*pgxpool.Conn, error) {
		appends, err := p.store.Pool.RoomAppends()
		if err != nil {
			return nil, err
		}
		conn, err := appends.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire document append connection: %w", err)
		}
		return conn, nil
	})
	if err != nil {
		return 0, err
	}
	// stored is the last version the transaction committed, -1 while it has committed none.
	// Deferred before release, the compaction note (PgVersioned.noted) runs after it, once the
	// append holds no lock.
	stored := int64(-1)
	defer func() {
		if stored >= 0 {
			p.noted(room, stored)
		}
	}()
	defer release()

	trips := 0
	send := func(ctx context.Context, batch *pgx.Batch) error {
		trips++
		return conn.SendBatch(ctx, batch).Close()
	}
	open := false
	// land lands credit and the credits of given (UpdateCredit.landed, which lands a credit once).
	// finish calls it before freeRoomLock. Deferred here, it also lands them where a context error
	// of the opening pipeline skips finish. A holdRoom failure returns before this defer and lands
	// nothing here.
	land := func() {
		credit.landed()
		for _, update := range given {
			update.Credit.landed()
		}
	}
	defer land()
	// finish ends the append once the opening pipeline may have taken the session lock, in this
	// order: it lands the credits, attempts a rollback of a transaction still open, calls
	// freeRoomLock, and for a flush adds the round trips counted in trips to the room's figures.
	//
	// The rollback runs under duty.LeaseCall rather than ctx, which may have ended, or an unbounded
	// context, under which a rollback on a half-open connection would hold this call's locks until
	// TCP gives the connection up.
	finish := func() {
		land()
		if open {
			if p.beforeAppendRollback != nil {
				p.beforeAppendRollback(room, conn)
			}
			rollback := &pgx.Batch{}
			rollback.Queue(`rollback`)
			rollbackCtx, cancel := duty.LeaseCall()
			_ = send(rollbackCtx, rollback)
			cancel()
		}
		if p.beforeAppendUnlock != nil {
			p.beforeAppendUnlock(room, conn)
		}
		// The unlock counts as a trip whether or not freeRoomLock sends it.
		trips++
		freeRoomLock(conn, room)
		if flush != nil {
			flush.service.appendFigures.roundTrips.Add(uint64(trips))
		}
	}

	opening := &pgx.Batch{}
	opening.Queue(`select pg_advisory_lock(hashtext($1))`, room)
	opening.Queue(`begin`)
	gate := queueAppendGate(opening, room, nil)
	// finish is deferred only once the opening pipeline, which takes the session lock, has
	// answered. On a context error pgx sends nothing or closes the connection, so the session holds
	// no lock to give back and finish is skipped. On another error the lock may be held, since a
	// session-level lock outlives the implicit transaction a later statement's error rolls back,
	// so finish runs.
	open = true
	if err := send(ctx, opening); err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			finish()
		}
		return 0, fmt.Errorf("begin document update: %w", err)
	}
	defer finish()
	read, err := gate.result()
	if err != nil {
		return 0, err
	}
	var pruneCeiling *int64
	if read.pruneCeiling != nil {
		// A crash left a prune half done. Its deletes are queued with the writes below, since a
		// delete here would assign the transaction an id before the gate's decode
		// (storedHistoryHolds). This pipeline reads the gate again under the checkpoint's ceiling,
		// the history those deletes leave.
		pruneCeiling = read.pruneCeiling
		rereading := &pgx.Batch{}
		gate = queueAppendGate(rereading, room, pruneCeiling)
		if err := send(ctx, rereading); err != nil {
			return 0, fmt.Errorf("finish the document's prune: %w", err)
		}
		if read, err = gate.result(); err != nil {
			return 0, err
		}
	}
	// The cursor is read once the opening pipeline holds the document's lock (appendingThrough).
	heldThrough, gated := appendingThrough(ctx, room, flush)
	var updates []BrowserUpdate
	var taking []*queuedClass
	if flush != nil {
		updates, taking = flush.service.flushRun(flush, through)
	} else {
		updates = given
	}
	if len(updates) == 0 {
		return 0, nil
	}
	head, admitted := read.latest, len(updates)
	if gated {
		var replaced bool
		head, admitted, replaced, err = gateAppend(ctx, conn, room, heldThrough, appendBrowser, updates, read, p.maxPendingItems, p.closeMargin)
		if err != nil {
			return 0, err
		}
		if replaced {
			p.markRunReplaced(room, roomCursorFrom(ctx), flush)
		}
	}
	// A closed issue's gate admits a prefix of the run (gateAppend). For a flush, the cut classes
	// stay queued, not marked stored, and are recorded on it once the writes and commit return
	// without error (RoomFlush.cut); given updates past the prefix are not stored.
	updates = updates[:admitted]
	var cut []*queuedClass
	if flush != nil {
		taking, cut = taking[:admitted], taking[admitted:]
	}
	first = persistence.Version(head + 1)
	// nil tx: this batch commits on its own connection, not in a caller's transaction.
	plan, err := p.appendWrites(ctx, nil, room, head, updates, owes)
	if err != nil {
		return 0, err
	}
	second := &pgx.Batch{}
	// A half-done prune's statements go first, so the rows above the ceiling are deleted before
	// the insert writes rows at the head the ceiling-bounded read gave.
	if pruneCeiling != nil {
		store.QueueExec(second, takePruneCheckpointWhat, takePruneCheckpointStatement, room)
		store.QueueExec(second, finishPruneWhat, finishPruneStatement, room, *pruneCeiling)
	}
	for _, statement := range plan.statements {
		statement.queue(second)
	}
	// The commit ends the writes pipeline, so the transaction holds its id for that one round trip.
	second.Queue(`commit`)
	if err := send(ctx, second); err != nil {
		return 0, fmt.Errorf("commit document update: %w", err)
	}
	last := plan.last
	open, stored = false, int64(last)
	if p.afterAppendCommit != nil {
		for version := head + 1; version <= int64(last); version++ {
			p.afterAppendCommit(room, persistence.Version(version))
		}
	}
	if flush == nil {
		for index, update := range updates {
			update.Credit.held(first + persistence.Version(index))
		}
		// The versions this commit stored are recorded on this instance's cursor before the
		// unlock (advanceRoomCursor).
		cursor := roomCursorFrom(ctx)
		for version := head + 1; version <= int64(last); version++ {
			advanceRoomCursor(cursor, persistence.Version(version))
		}
		return first, nil
	}
	flush.appendedRun(taking, updates, persistence.Version(head+1))
	flush.cut = cut
	advanceRoomCursorThrough(flush.cursor, flush.appended(nil))
	for _, unlanded := range flush.markStored(nil) {
		unlanded.landed()
	}
	state := flush.state
	state.mu.Lock()
	dropped := state.dropDrainedShadowLocked()
	flush.service.unlockState(room, state)
	dropped.release()
	flush.service.appendFigures.committed(len(updates))
	return first, nil
}

// roomBatchThrough answers, under room's state lock, the commit position the call for class runs
// its batch through: the position of the last class of the run batchRunLocked takes from class,
// or class's own position when room has no record, its queue no longer holds class, or a
// suppression slot is queued for room (suppressionQueued). With linger, for a run that is not
// full, it answers instead the channel to wait on and how long to wait, until roomAppendLinger
// after class was observed, unless the service is stopping, room is shutting down, the record
// counts an append waiter (appendWaiters) or its appends were asked to go after class was
// observed (appendsFlushed). StoreUpdateContext waits, and then returns or calls it again without
// linger.
func (s *Service) roomBatchThrough(room string, class *queuedClass, linger bool) (through uint64, wake <-chan struct{}, wait time.Duration) {
	state := s.lockExistingState(room)
	if state == nil {
		return class.seq, nil, 0
	}
	defer s.unlockState(room, state)
	at := slices.Index(state.updateClasses, class)
	if at < 0 || s.suppressionQueued(room) {
		return class.seq, nil, 0
	}
	end, full := batchRunLocked(state.updateClasses, at)
	if linger && !full && state.appendWaiters == 0 && !s.stopping.Load() && !s.shuttingDown(room) && class.observed.After(state.appendsFlushed) {
		if wait = time.Until(class.observed.Add(roomAppendLinger)); wait > 0 {
			return 0, state.appendLinger.wait(), wait
		}
	}
	return state.updateClasses[end-1].seq, nil, 0
}

// askRoomAppends cuts short the lingers of room's appends on the record it finds: it sets the
// record's appendsFlushed to now and counts an append waiter on it until done is called, the two
// marks roomBatchThrough reads, and wakes the lingers waiting on it (appendLinger). With no record
// it changes nothing, and done does nothing. roomQueries.query calls it.
func (s *Service) askRoomAppends(room string) (done func()) {
	state := s.lockExistingState(room)
	if state == nil {
		return func() {}
	}
	state.appendsFlushed = time.Now()
	state.appendWaiters++
	state.appendLinger.wake()
	s.unlockState(room, state)
	return func() {
		state.mu.Lock()
		state.appendWaiters--
		s.unlockState(room, state)
	}
}

// batchRunLocked answers end, where the run a batch for queue[at] takes ends: queue[at:end]. The
// run takes queue[at], then each later class until one that is stored, handed, not durable or of
// another room instance (cursor) than queue[at]'s, or one more would pass roomAppendBatchUpdates
// or roomAppendBatchBytes. full reports that a cap ended the run, or that the run reached the
// queue's end at a cap; a run stopped by a class it may not take reports not full, at a cap or
// not. Its caller holds the queue's state.mu.
func batchRunLocked(queue []*queuedClass, at int) (end int, full bool) {
	cursor := queue[at].cursor
	count, size := 0, 0
	for index := at; index < len(queue); index++ {
		class := queue[index]
		if index > at && (class.stored.Load() || class.handed || !class.durable || class.cursor != cursor) {
			return index, false
		}
		if count == roomAppendBatchUpdates || (count > 0 && size+len(class.update) > roomAppendBatchBytes) {
			return index, true
		}
		count++
		size += len(class.update)
	}
	return len(queue), count == roomAppendBatchUpdates || size >= roomAppendBatchBytes
}

// suppressionQueued is whether a suppression slot is queued for room's persistence. Its caller
// holds room's state.mu, which is taken before suppressMu, as failRoomLocked takes the two.
func (s *Service) suppressionQueued(room string) bool {
	s.suppressMu.Lock()
	defer s.suppressMu.Unlock()
	return len(s.suppressed[room]) > 0
}

// warmRoomAppends opens a connection of the room-appends pool (store.Pool.RoomAppends) on a
// goroutine of its own and gives it back to the pool, when the pool holds no connection and no
// other warm-up runs (warmingAppends), so a room's next batch can find one idle rather than dial
// one while its update waits to be durable. It does not wait for the open and drops its error.
// onLoadDocument calls it.
func (s *Service) warmRoomAppends() {
	appends, err := s.store.Pool.RoomAppends()
	if err != nil || appends.Stat().TotalConns() > 0 || !s.warmingAppends.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.warmingAppends.Store(false)
		if conn, err := appends.Acquire(context.Background()); err == nil {
			conn.Release()
		}
	}()
}

// roomAppendBatchSizes are the upper bounds of the batch-size histogram's buckets: 1, 2, 3-4, 5-8,
// and so on to 129-256, roomAppendBatchUpdates. committed counts a larger batch in the last
// bucket: a batch also stores the unstored updates queued before its caller's.
var roomAppendBatchSizes = [...]int{1, 2, 4, 8, 16, 32, 64, 128, 256}

// roomAppendFigures counts the rooms' flushes (AppendRoomFlush) for the room figures: batches and
// updates, the committed batches and the updates they stored; sizes, how many committed batches
// stored how many updates (committed, one count per bucket of roomAppendBatchSizes); and
// roundTrips, the round trips appendRoomTx's finish adds for a flush, committed or not.
type roomAppendFigures struct {
	batches    atomic.Uint64
	updates    atomic.Uint64
	roundTrips atomic.Uint64
	sizes      [len(roomAppendBatchSizes)]atomic.Uint64
}

// committed counts one committed batch of size updates.
func (f *roomAppendFigures) committed(size int) {
	f.batches.Add(1)
	f.updates.Add(uint64(size))
	f.sizes[min(sort.SearchInts(roomAppendBatchSizes[:], size), len(f.sizes)-1)].Add(1)
}

// dropUpdate takes a failed room's update, which it does not store, off its record: the class
// consumeUpdateClass finds for update, if any, leaves the queue and ends the append the record
// counts for it (finishUpdateClass), the class leaves its instance's unstored updates, and its
// credit lands. Left counted, the append would keep the record's count of appends above zero.
// StoreUpdateContext calls it when the room has failed.
func (a *servicePersistenceAdapter) dropUpdate(room string, update []byte) {
	class, counted, found := a.service.consumeUpdateClass(room, update)
	if found {
		a.service.finishUpdateClass(room, counted, class)
	}
	class.instance.unlistUnstored(class)
	class.credit.landed()
}
