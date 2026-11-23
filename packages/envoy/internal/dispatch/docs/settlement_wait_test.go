package docs

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Shutdown's drain waits for each room's appends to land before it settles the room, and a
// shutdown's settlement waits a bounded time for its document's lock. Neither wait is the
// settlement's own work, so what each costs is the question here: a wait that polls costs the
// process a wake-up every millisecond for as long as the room's appends lag, and a lock wait that
// polls costs the database a statement every millisecond. (A timer's settlement waits for no
// append: it stores the room's unstored updates itself, Ledger.flushThrough.)

// A room's append held for half a second while Shutdown drains the room: the drain waits for it
// (waitForDurableAppends) and settles the document once it lands. Its wait wakes when the append
// lands, not every millisecond of the half second, and the drain's settlement starts once the
// append commits with the service's clock standing still. The drain's budget runs on a fake this
// test never moves, so no timer of the service's can have woken the drain: the append's commit
// did, through the room's idle signal. What woke it is read in process, as the settlement's start
// (beforeSettleStart) after the append's commit (afterAppendCommit) with the budget still pending,
// and no span of the machine's clock is part of the verdict, so a loaded machine slows the run and
// fails none of it.
func TestShutdownsDrainWakesOnceWhenARoomsHeldAppendLands(t *testing.T) {
	const held = 500 * time.Millisecond
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	persist := NewPgVersioned(database)
	var counting, committed atomic.Bool
	persist.afterAppendCommit = func(room string, _ persistence.Version) {
		if room == artifactID && counting.Load() {
			committed.Store(true)
		}
	}
	appends := &heldStore{VersionedStore: persist, room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(appends.let)
	// The drain's budget runs on fake, which this test never moves.
	start := time.Now()
	fake := clocktest.New(start)
	service := newShutdownTestServiceOn(t, database, appends, fake)
	var wakes atomic.Int64
	service.settlementWoke = func(room string) {
		if room == artifactID && counting.Load() {
			wakes.Add(1)
		}
	}
	// pendingAtStart is what the service's clock held pending as the drain's settlement started,
	// once the held append had committed.
	var pendingAtStart atomic.Pointer[[]time.Time]
	service.beforeSettleStart = func(room string) {
		if room == artifactID && counting.Load() && committed.Load() {
			armed := fake.Armed()
			pendingAtStart.CompareAndSwap(nil, &armed)
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
	// Shutdown's drain is bounded by its own budget (ShutdownDrainBudget), on the service's clock;
	// the context bounds only the rest of its work, and ends where testwait gives up rather than
	// at a span of the test's.
	shutdownCtx := testwait.Context(t)
	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	// The hold counts from the drain's wait on the room's appends, so a machine slow to run
	// Shutdown does not let the append go before there is a wait for it to wake.
	waitUntil(t, "Shutdown's drain to wait on the room's held append", func() bool {
		state := service.lockExistingState(artifactID)
		if state == nil {
			return false
		}
		defer service.unlockState(artifactID, state)
		return state.appendWaiters > 0
	})
	time.Sleep(held)
	appends.let()
	if err := testwait.Receive(t, "Shutdown to return once the room's append was let go", shutdown); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	counting.Store(false)

	if got := wakes.Load(); got > 3 {
		t.Errorf("the drain woke %d times while its room's append was held for %s, want at most 3", got, held)
	}
	budget := start.Add(ShutdownDrainBudget)
	switch pending := pendingAtStart.Load(); {
	case !committed.Load():
		t.Errorf("the held append's commit was never seen")
	case pending == nil:
		t.Errorf("no settlement started after the held append committed")
	case !slices.ContainsFunc(*pending, budget.Equal):
		t.Errorf("the drain's settlement started with %v pending on the service's clock, want its drain budget (%s) among them: only on that clock does no timer's move wake the drain",
			*pending, budget.Format(time.RFC3339Nano))
	}
	number, markdown, _ := latestDocumentVersion(t, observer, artifactID)
	if number <= versioned || markdown != "after\n" {
		t.Errorf("the drain versioned %q as version %d, want the held edit after version %d", markdown, number, versioned)
	}
}

// A settlement during Shutdown does not wait behind a writer that holds its document's lock: it
// gives up after about 100 ms and leaves the document to resume, as it always has. What the wait
// costs is the question: one lock statement with a bounded wait, not a try-lock every millisecond.
// The bound is the database's own (lock_timeout), and the drain's budget runs on a fake this test
// never moves: a settlement that waited for the lock until that budget ended would hold Shutdown
// until testwait gives up and ends it by name, so Shutdown returning at all is the settlement
// giving up on the lock by itself. Its 100 ms is a floor, which a loaded machine only lengthens.
func TestShutdownsSettlementLockWaitIsOneStatement(t *testing.T) {
	database := storetest.Open(t)
	tracer := &statementTracer{}
	traced := storetest.TracedStore(t, database, tracer)
	observer := storetest.SecondProcess(t, database)
	artifactID := createDocument(t, database, "before")
	// The drain's budget runs on fake, which this test never moves.
	start := time.Now()
	fake := clocktest.New(start)
	service := newShutdownTestServiceOn(t, traced, nil, fake)
	// pendingAtStart is what the service's clock held pending as the shutdown's settlement started.
	var pendingAtStart atomic.Pointer[[]time.Time]
	service.beforeSettleStart = func(room string) {
		if room == artifactID && service.shuttingDown(room) {
			armed := fake.Armed()
			pendingAtStart.CompareAndSwap(nil, &armed)
		}
	}
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("before", "after"))
	waitForSettlementOwed(t, service, artifactID)
	waitForLandedAppends(t, service, artifactID)

	versioned, _, _ := latestDocumentVersion(t, observer, artifactID)
	unlock := holdDocumentLock(t, observer, artifactID)
	since := tracer.mark()
	ctx := testwait.Context(t)
	began := time.Now()
	if err := service.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown with the document's lock held elsewhere: %v", err)
	}
	took := time.Since(began)
	unlock()

	if locks := tracer.statements(artifactID, "advisory_xact_lock(hashtext(", since); locks != 1 {
		t.Errorf("the shutdown's settlement sent %d lock statements for the document, want 1", locks)
	}
	if owed, err := settlementPending(context.Background(), observer.Pool, artifactID); err != nil || !owed {
		t.Errorf("the document owes no settlement (%v) after a shutdown whose settlement could not lock it, want it left to resume", err)
	}
	if number, _, _ := latestDocumentVersion(t, observer, artifactID); number != versioned {
		t.Errorf("the document's latest version is %d, want %d: the settlement left it", number, versioned)
	}
	budget := start.Add(ShutdownDrainBudget)
	switch pending := pendingAtStart.Load(); {
	case pending == nil:
		t.Errorf("no settlement of the document started during Shutdown")
	case !slices.ContainsFunc(*pending, budget.Equal):
		t.Errorf("the shutdown's settlement started with %v pending on the service's clock, want its drain budget (%s) among them: only on that clock can the budget not end the lock wait",
			*pending, budget.Format(time.RFC3339Nano))
	}
	if took < 100*time.Millisecond {
		t.Errorf("Shutdown took %s, want at least the 100ms the settlement waits for the lock", took.Round(time.Millisecond))
	}
}
