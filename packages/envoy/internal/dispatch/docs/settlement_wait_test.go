package docs

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Shutdown's drain waits for each room's appends to land before it settles the room, and a
// shutdown's settlement waits a bounded time for its document's lock. Neither wait is the
// settlement's own work, so what each costs is the question here: a wait that polls costs the
// process a wake-up every millisecond for as long as the room's appends lag, and a lock wait that
// polls costs the database a statement every millisecond. (A timer's settlement waits for no
// append: it stores the room's unstored updates itself, Ledger.flushThrough.)

// A room's append held while Shutdown drains the room: the drain waits for it
// (waitForDurableAppends) and settles the document once it lands. Its wait wakes when the append
// lands and on nothing else, and the drain's settlement starts on the wake the landing gives it.
// The service's clock is a fake, held, so no timer of the service's - a poll, a pause before
// settling - fires by itself; what the drain arms on it is read instead: once the drain is parked
// in its wait (the room's appendWaiters), the clock holds nothing beyond what it held before
// Shutdown but the drain's budget (ShutdownDrainBudget), so the wait arms no poll, and once
// Shutdown has returned nothing beyond it at all, so no wait left a timer behind. The wakes are
// read off the order of the events, in process: the append's commit (afterAppendCommit), each
// wake of the drain's wait (settlementWoke) and the settlement's start (beforeSettleStart), each
// numbered from one counter as it happens, with exactly one wake between the commit and the
// start: the landing's. A wait the service took off its clock, on the wall clock, which neither
// would show, is caught by `c-clock`'s listing at INFRA-3149's final head, which lists every
// wall-clock wait of the product's source that its allow-list gives no reason for.
func TestShutdownsDrainWakesOnceWhenARoomsHeldAppendLands(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	persist := NewPgVersioned(database)
	var counting atomic.Bool
	// order numbers each event of the drain this row reads, as it happens; committed, started
	// and the two wake counts are read at the commit and at the start.
	var order, committed, started, wakes, wakesAtCommit, wakesAtStart atomic.Int64
	persist.afterAppendCommit = func(room string, _ persistence.Version) {
		if room == artifactID && counting.Load() && committed.CompareAndSwap(0, order.Add(1)) {
			wakesAtCommit.Store(wakes.Load())
		}
	}
	appends := &heldStore{VersionedStore: persist, room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	serviceClock := clocktest.Held()
	service := newShutdownTestServiceOn(t, database, appends, serviceClock)
	// Registered after the service, so it runs before its shutdown: a row that fails while the
	// room's append is held lets it go rather than leave that shutdown waiting on it.
	testcleanup.Add(t, appends.let)
	service.settlementWoke = func(room string) {
		if room == artifactID && counting.Load() {
			order.Add(1)
			wakes.Add(1)
		}
	}
	service.beforeSettleStart = func(room string) {
		if room == artifactID && counting.Load() && committed.Load() != 0 && started.CompareAndSwap(0, order.Add(1)) {
			wakesAtStart.Store(wakes.Load())
		}
	}
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)
	versioned, _, _ := latestDocumentVersion(t, database, artifactID)
	observer := storetest.SecondProcess(t, database)

	// No timer's settlement runs before the drain's.
	service.settle = 30 * time.Minute
	counting.Store(true)
	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	testwait.Receive(t, "the room's append to reach the store", appends.entered)
	before := serviceClock.Armed()
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(testwait.Context(t)) }()
	testwait.Until(t, "Shutdown's drain budget armed on the service's clock", func() ([]time.Time, bool) {
		armed := serviceClock.Armed()
		return armed, len(armedBeyond(before, armed)) > 0
	})
	testwait.Eventually(t, "the drain parked in its wait for the room's held append", func() bool {
		return appendWaiters(service, artifactID) == 1
	})
	if extra := armedBeyond(before, serviceClock.Armed()); len(extra) != 1 {
		t.Errorf("with the drain parked in its wait the service's clock holds %v beyond what it held before Shutdown, want the drain's budget alone: a wait that polls arms a timer for each poll", extra)
	}
	appends.let()
	if err := testwait.Receive(t, "Shutdown to return once the room's append was let go", shutdown); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	counting.Store(false)
	if extra := armedBeyond(before, serviceClock.Armed()); len(extra) != 0 {
		t.Errorf("once Shutdown returned the service's clock holds %v beyond what it held before Shutdown, want nothing: a wait of the drain left a timer armed", extra)
	}

	if got := wakes.Load(); got > 3 {
		t.Errorf("the drain woke %d times while its room's append was held, want at most 3", got)
	}
	switch {
	case committed.Load() == 0:
		t.Errorf("the held append's commit was never seen")
	case started.Load() == 0:
		t.Errorf("no settlement started after the held append committed")
	case started.Load() < committed.Load():
		t.Errorf("the drain's settlement started (event %d) before its room's append committed (event %d)", started.Load(), committed.Load())
	default:
		if between := wakesAtStart.Load() - wakesAtCommit.Load(); between != 1 {
			t.Errorf("the drain woke %d times between its room's append committing and its settlement starting, want once: the landing's wake, on which it settles", between)
		}
	}
	number, markdown, _ := latestDocumentVersion(t, observer, artifactID)
	if number <= versioned || markdown != "after\n" {
		t.Errorf("the drain versioned %q as version %d, want the held edit after version %d", markdown, number, versioned)
	}
}

// A settlement during Shutdown does not wait behind a writer that holds its document's lock: it
// gives up after about 100 ms and leaves the document to resume, as it always has. What the wait
// costs is the question: one lock statement with a bounded wait, not a try-lock every millisecond.
func TestShutdownsSettlementLockWaitIsOneStatement(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := storetest.TracedStore(t, database, tracer)
	observer := storetest.SecondProcess(t, database)
	artifactID := createDocument(t, database, "before")
	service := newShutdownTestService(t, traced, nil)
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)

	versioned, _, _ := latestDocumentVersion(t, observer, artifactID)
	unlock := holdDocumentLock(t, observer, artifactID)
	since := tracer.mark()
	if err := docstest.Shutdown(t, testwait.Context(t), service); err != nil {
		t.Errorf("Shutdown with the document's lock held elsewhere: %v", err)
	}
	unlock()

	// pg_advisory_xact_lock is the lock that waits; pg_try_advisory_xact_lock, which gives up at
	// once, is not it, and the shutdown's settlement sends none.
	if locks := tracer.statements(artifactID, "pg_advisory_xact_lock(hashtext(", since); locks != 1 {
		t.Errorf("the shutdown's settlement sent %d waiting lock statements for the document, want 1", locks)
	}
	if tries := tracer.statements(artifactID, "pg_try_advisory_xact_lock(hashtext(", since); tries != 0 {
		t.Errorf("the shutdown's settlement sent %d try-locks for the document, want none: a try-lock waits for nothing", tries)
	}
	// The wait's bound is Postgres's own: the lock statement goes out in the pipeline that first
	// sets lock_timeout to 100 ms, so the database ends the wait there - not a try-lock that waits
	// for nothing, and not a wait for the shutdown's whole budget.
	if !tracer.lockWaitsUnder(artifactID, "100ms", since) {
		t.Errorf("the shutdown's settlement took the document's lock outside a pipeline that sets lock_timeout to 100ms first")
	}
	if owed, err := settlementPending(context.Background(), observer.Pool, artifactID); err != nil || !owed {
		t.Errorf("the document owes no settlement (%v) after a shutdown whose settlement could not lock it, want it left to resume", err)
	}
	if number, _, _ := latestDocumentVersion(t, observer, artifactID); number != versioned {
		t.Errorf("the document's latest version is %d, want %d: the settlement left it", number, versioned)
	}
}

// lockWaitsUnder reports whether a pipeline recorded since sets lock_timeout to timeout and then,
// in that pipeline, takes room's transaction-level advisory lock with the statement that waits for
// it (pg_advisory_xact_lock, not pg_try_advisory_xact_lock).
func (t *statementTracer) lockWaitsUnder(room, timeout string, since int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, trip := range t.trips[since:] {
		set := false
		for _, statement := range trip.statements {
			if strings.Contains(statement.sql, "set_config('lock_timeout', '"+timeout+"'") {
				set = true
			}
			if set && strings.Contains(statement.sql, "pg_advisory_xact_lock(hashtext(") && statement.names(room) {
				return true
			}
		}
	}
	return false
}

// armedBeyond is the deadlines of armed that before does not hold, each as often as armed holds
// it more times than before does: what a fake's timers gained since before was read, whatever
// it lost meanwhile.
func armedBeyond(before, armed []time.Time) []time.Time {
	left := slices.Clone(before)
	var gained []time.Time
	for _, at := range armed {
		if index := slices.IndexFunc(left, at.Equal); index >= 0 {
			left = slices.Delete(left, index, index+1)
			continue
		}
		gained = append(gained, at)
	}
	return gained
}

// appendWaiters is how many waits for room's appends its record counts (waitForRoomIdle,
// askRoomAppends), read under the record's lock; none when the room has no record.
func appendWaiters(service *Service, room string) int {
	state := service.lockExistingState(room)
	if state == nil {
		return 0
	}
	defer service.unlockState(room, state)
	return state.appendWaiters
}
