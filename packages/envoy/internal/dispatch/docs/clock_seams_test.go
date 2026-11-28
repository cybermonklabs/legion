package docs

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The seams the document service gives a test of its timers and bounds, each left at production's
// clock and bound unless a test hands it one: the rows of this file read one seam each, the way
// the package's later rows lean on it.

// newSeamService is a document service on database built from deps, shut down when the test ends.
func newSeamService(t *testing.T, deps Deps) *Service {
	t.Helper()
	service := New(deps)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(testwait.Context(t)); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service
}

// sessionsNamed is a count of the test database's sessions under one application_name, with the
// error of the read that counted them.
type sessionsNamed struct {
	count int
	err   error
}

// requireDueAt waits until what is armed on fake for due, moves fake to a nanosecond short of due,
// and requires it still armed there: the caller then moves the last nanosecond and waits for what
// the timer does.
func requireDueAt(t *testing.T, fake *clocktest.Fake, what string, due time.Time) {
	t.Helper()
	fake.AwaitArmedAt(t, what, due)
	fake.Advance(due.Sub(fake.Now()) - time.Nanosecond)
	if armed := fake.Armed(); !slices.ContainsFunc(armed, due.Equal) {
		t.Fatalf("%s, due at %v, is no longer pending a nanosecond short of it (pending: %v)", what, due, armed)
	}
}

// Deps.Clock left nil is the real clock, and the reference excerpt reconciliation hands its
// statements no moment, so the database's now() stamps and judges every retry; a settle delay left
// zero is DefaultSettle. A clock a test hands is the one the service's timers run on, and the
// reconciliation's moment is its reading; two moments stay the wall clock's whatever the clock
// (the drain deadline and the excerpt retry's due time, the last two rows of this file), and so do
// the timers duty runs until it takes a clock (INFRA-6392).
func TestDepsClockIsTheRealClockUnlessATestHandsOne(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	deps := testServiceDeps(database)
	deps.Settle = 0
	production := newSeamService(t, deps)
	if production.clock != clock.Real() {
		t.Fatalf("a service given no clock runs on %T, want the real clock", production.clock)
	}
	if moment := production.excerptMoment(); moment != nil {
		t.Fatalf("a service given no clock judges excerpt retries at %v, want no moment: the database's now()", *moment)
	}
	if production.settle != DefaultSettle {
		t.Fatalf("a service given no settle delay waits %v, want DefaultSettle, %v", production.settle, DefaultSettle)
	}

	fake := clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 123456789, time.UTC))
	deps = testServiceDeps(database)
	deps.Clock = fake
	faked := newSeamService(t, deps)
	if faked.clock != clock.Clock(fake) {
		t.Fatalf("a service given a fake runs on %T, want the fake", faked.clock)
	}
	if moment, want := faked.excerptMoment(), store.MomentOn(fake); moment == nil || !moment.Equal(*want) {
		t.Fatalf("a service on a fake judges excerpt retries at %v, want the fake's reading %v", moment, *want)
	}
}

// A room's settlement timer runs on Deps.Clock: armed the settle delay ahead of the fake's
// present, still pending when the fake is moved to a nanosecond short of it, and fired, starting
// the room's settlement, once the fake reaches it.
func TestTheSettleTimerFiresOnDepsClockOnlyOnceTheClockReachesIt(t *testing.T) {
	t.Parallel()
	const settle = time.Hour
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	fake := clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	deps := testServiceDeps(database)
	deps.Settle, deps.Clock = settle, fake
	service := newSeamService(t, deps)
	service.SetFormGate(formKept)
	seedServiceText(t, service, artifactID, "Before.\n")
	settleCurrentGeneration(t, service, artifactID)
	// Set once the seed's settlement has returned, and read by the settlement the timer starts.
	started := make(chan struct{}, 1)
	service.beforeSettleStart = func(room string) {
		if room != artifactID {
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
	}

	editLiveTree(t, service, artifactID, replaceRun("Before.", "After."))
	requireDueAt(t, fake, "the room's settlement after the write", fake.Now().Add(settle))
	select {
	case <-started:
		t.Fatal("the room's settlement started before the fake reached its timer")
	default:
	}
	fake.Advance(time.Nanosecond)
	testwait.Receive(t, "the room's settlement to start once the fake reached its timer", started)
}

// A socket deadline left zero hands ygo zero, its own bound; no end hands it the longest span a
// duration holds. Shutdown's drain keeps the deadline it was given, zero for ShutdownDrainBudget.
func TestADeadlineSeamLeftZeroIsMainsBound(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	production := newSeamService(t, testServiceDeps(database))
	if production.srv.PeerWriteTimeout != 0 || production.srv.CloseFrameTimeout != 0 || !production.shutdownDrain.IsZero() {
		t.Fatalf("a service given no deadline hands ygo a write bound of %v and a close-frame bound of %v and drains under %q, want zero for each: production's",
			production.srv.PeerWriteTimeout, production.srv.CloseFrameTimeout, production.shutdownDrain)
	}
	none := clock.DeadlineAt(time.Time{}, false)
	deps := testServiceDeps(database)
	deps.PeerWriteDeadline, deps.CloseFrameDeadline, deps.ShutdownDrainDeadline = none, none, none
	unbounded := newSeamService(t, deps)
	if unbounded.srv.PeerWriteTimeout != math.MaxInt64 || unbounded.srv.CloseFrameTimeout != math.MaxInt64 || unbounded.shutdownDrain != none {
		t.Fatalf("a service given no end hands ygo a write bound of %v and a close-frame bound of %v and drains under %q, want the longest span for each and no end",
			unbounded.srv.PeerWriteTimeout, unbounded.srv.CloseFrameTimeout, unbounded.shutdownDrain)
	}
}

// VerifyMark's wait for a mark, where Deps.Clock is set, is a timer on that clock, which fires once
// the clock reaches the wait and not before (marks_test.go holds the wall timer's).
func TestTheMarkWaitRunsOnDepsClock(t *testing.T) {
	t.Parallel()
	const wait = 300 * time.Millisecond
	fake := clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	due := fake.Now().Add(wait)
	fired, stop := markTimerOn(fake)(wait)
	defer stop()
	requireDueAt(t, fake, "VerifyMark's wait", due)
	fake.Advance(time.Nanosecond)
	testwait.Receive(t, "VerifyMark's wait to fire once the fake reached it", fired)
}

// The compaction queue's waits run on the clock it is started on: a noted room is checked
// firstCheck after its note on that clock, and not before.
func TestTheCompactionQueueWaitsOnItsClock(t *testing.T) {
	t.Parallel()
	fake := clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	stepped := make(chan string, 1)
	queue := startCompactionQueue(fake, func(_ context.Context, room string, _ bool) compactionNext {
		stepped <- room
		return compactionNext{kind: compactionDone}
	})
	defer func() {
		queue.shutDown()
		queue.awaitStopped(testwait.Context(t))
	}()
	due := fake.Now().Add(defaultCompactionTiming.firstCheck)
	queue.note("room")
	requireDueAt(t, fake, "the noted room's first check", due)
	fake.Advance(time.Nanosecond)
	if room := testwait.Receive(t, "the queue to check the noted room once the fake reached its time", stepped); room != "room" {
		t.Fatalf("the queue checked %q, want the noted room", room)
	}
}

// A room query's wait is roomQueryWait unless a test sets one: wait, a bound of its own, or
// waitEach, asked again at each ask ahead of wait.
func TestARoomQueryWaitAskedAtEachAskOverridesTheFixedOne(t *testing.T) {
	t.Parallel()
	queries := &roomQueries{}
	if got := queries.answerWait(); got != roomQueryWait {
		t.Fatalf("a query with no wait set waits %v, want roomQueryWait, %v", got, roomQueryWait)
	}
	queries.wait = 3 * time.Second
	if got := queries.answerWait(); got != 3*time.Second {
		t.Fatalf("a query with its wait set to 3s waits %v", got)
	}
	var asked atomic.Int32
	queries.waitEach = func() time.Duration {
		return time.Duration(asked.Add(1)) * time.Minute
	}
	if first, second := queries.answerWait(), queries.answerWait(); first != time.Minute || second != 2*time.Minute {
		t.Fatalf("a query asking its wait at each ask waited %v then %v, want 1m then 2m: waitEach asked each time, ahead of wait", first, second)
	}
}

// An ask's resolution is stamped at the moment its caller hands, in UTC, on the ask and in the
// bytes its row stores.
func TestAnAskResolutionIsStampedAtTheMomentItsCallerTakes(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 28, 10, 30, 0, 0, time.FixedZone("UTC+1", 3600))
	actor := model.Actor{Kind: "user", ID: "alice@example.com"}
	resolved, encoded, err := askResolution(model.Ask{ID: "ask-1", State: "open"}, model.AskResolutionWithdrawn, "Withdrawn.", actor, at)
	if err != nil {
		t.Fatalf("resolve the ask: %v", err)
	}
	if resolved.Resolution == nil || !resolved.Resolution.At.Equal(at) || resolved.Resolution.At.Location() != time.UTC {
		t.Fatalf("the resolution reads %+v, want it made at %v in UTC", resolved.Resolution, at)
	}
	var stored model.AskResolution
	if err := json.Unmarshal(encoded, &stored); err != nil || !stored.At.Equal(at) {
		t.Fatalf("the row stores %s (%v), want the resolution made at %v", encoded, err, at)
	}
}

// What asked for a catch-up carries to the next catch-up of the room to start, as a set, and is
// taken by that one alone; a catch-up this process asked for itself carries nothing, and a
// service without the hooks carries nothing at all.
func TestACatchUpCauseCarriesToTheNextCatchUpOfItsRoom(t *testing.T) {
	t.Parallel()
	hooks := newTestHooks()
	hooks.carryCatchUpCause("doc", catchUpAsked)
	hooks.carryCatchUpCause("doc", catchUpNotified)
	hooks.carryCatchUpCause("doc", catchUpSwept)
	if got := hooks.takeCatchUpCauses("doc"); got != catchUpNotified|catchUpSwept {
		t.Fatalf("the next catch-up took causes %b, want the notification's and the sweep's", got)
	}
	if got := hooks.takeCatchUpCauses("doc"); got != catchUpAsked {
		t.Fatalf("the catch-up after it took causes %b, want none: the first took them", got)
	}
	var off *testHooks
	off.carryCatchUpCause("doc", catchUpNotified)
	if got := off.takeCatchUpCauses("doc"); got != catchUpAsked {
		t.Fatalf("a service without the hooks carried causes %b", got)
	}
}

// A settlement's transaction names itself in pg_stat_activity where the test hooks are on, for as
// long as it runs (SettlementApplicationName); and the hook after it (afterSettleTransaction) runs
// once the transaction has ended, its row gone to every other reader, while the settlement still
// counts as under way.
func TestASettlementUnderTheHooksNamesItsTransactionAndRunsItsHookOnceItEnds(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	deps := testServiceDeps(database)
	// An unmoved fake, so no settle timer starts a settlement of its own: the row settles itself.
	deps.Clock, deps.TestHooks = clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)), true
	service := newSeamService(t, deps)
	service.SetFormGate(formKept)
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Before.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("Before.", "After."))

	// Each hook reports the first settlement of the document alone: the shutdown that ends the
	// test settles again, and finds both hooks spent.
	queued, release, ended := make(chan sessionsNamed, 1), make(chan struct{}), make(chan string, 1)
	var held, reported atomic.Bool
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered after the service, so it runs before the service's shutdown: a row that fails
	// with the settlement held lets it go.
	testcleanup.Add(t, letGo)
	state := service.room(artifactID)
	service.afterSettleVersionWrite = func(room string) error {
		if room != artifactID || !held.CompareAndSwap(false, true) {
			return nil
		}
		var named sessionsNamed
		named.err = database.Pool.QueryRow(ctx,
			`select count(*) from pg_stat_activity where datname = current_database() and application_name = $1`,
			SettlementApplicationName).Scan(&named.count)
		queued <- named
		<-release
		return nil
	}
	service.afterSettleTransaction = func(room string) {
		if room != artifactID || !reported.CompareAndSwap(false, true) {
			return
		}
		owed, err := settlementPending(ctx, database.Pool, artifactID)
		state.mu.Lock()
		settling := state.settling
		state.mu.Unlock()
		switch {
		case err != nil:
			ended <- "reading the pending row failed: " + err.Error()
		case owed:
			ended <- "the pending row still stood"
		case settling == 0:
			ended <- "the settlement no longer counted as under way"
		default:
			ended <- ""
		}
	}
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		settleNow(service, artifactID)
	}()
	if named := testwait.Receive(t, "the settlement to queue its version", queued); named.err != nil || named.count != 1 {
		t.Fatalf("while the settlement ran, %d sessions of the test's database were named %q (read: %v), want its transaction's one", named.count, SettlementApplicationName, named.err)
	}
	letGo()
	if failure := testwait.Receive(t, "the hook after the settlement's transaction to run", ended); failure != "" {
		t.Fatalf("when the hook after the settlement's transaction ran, %s", failure)
	}
	testwait.Receive(t, "the settlement to return", settled)
}

// A reconciliation pass on a fake clock, however far the fake stands from the wall clock, answers
// its next run on the wall clock, the one its loop arms a timer on (duty.RunWoken): the wait to
// the one retry owed, judged at the fake's reading, from the wall clock's present. A due time on
// the fake's own timeline, years behind the wall, would read to the loop as long past.
func TestAnExcerptPassOnAFakeClockAnswersItsRetryOnTheWallClock(t *testing.T) {
	t.Parallel()
	// A moment of the wall clock before this row was written: any due time on the wall clock is
	// after it, and any on the fake's 2020 timeline is years before.
	wallFloor := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	database := storetest.Open(t)
	ctx := context.Background()
	document := owedDocuments(t, database, 1)[0]
	deps := testServiceDeps(database)
	deps.Clock = clocktest.New(time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC))
	service := newSeamService(t, deps)
	deferred := service.excerptMoment()
	drifted, err := refs.DriftedExcerptsAt(ctx, database.Pool, 1, deferred)
	if err != nil || len(drifted) != 1 || drifted[0].FromID != document {
		t.Fatalf("the drift lists %+v (%v), want the one owed document %s", drifted, err, document)
	}
	if err := refs.DeferExcerptsAt(ctx, database.Pool, document, drifted[0].Version, time.Minute, referenceExcerptRetryCeiling, deferred); err != nil {
		t.Fatalf("defer the owed document: %v", err)
	}
	var retry time.Time
	if err := database.Pool.QueryRow(ctx,
		`select next_attempt_at from reference_excerpt_backoff where artifact_id = $1::uuid`, document).Scan(&retry); err != nil {
		t.Fatalf("read the owed document's retry: %v", err)
	}
	wait := retry.Sub(*deferred)
	if due := service.reconcileExcerptPass(ctx, referenceExcerptSchedule); due.Before(wallFloor.Add(wait)) {
		t.Fatalf("the pass answers its next run at %v, before %v, a moment of the wall clock's before this row was written, plus the retry's wait of %v: the due time is on the fake's timeline",
			due, wallFloor, wait)
	}
}

// Shutdown's drain ends at Deps.ShutdownDrainDeadline, a wall-clock moment, on the wall clock
// whatever Deps.Clock is: with a deadline the wall clock has passed and a fake a year short of it
// and never moved, a drain that a room's held append keeps waiting ends at once, and Shutdown
// returns with the append the drain did not see through. Measured and armed on the fake, the
// drain would never end.
func TestShutdownsDrainEndsAtItsDeadlineOnTheWallClockWhateverDepsClockIs(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	// Past for the wall clock and a year ahead for the fake: armed on the wall clock the drain ends
	// at once, armed on the fake it never ends, and the shared wait below names it.
	deps := testServiceDeps(database)
	deps.Persistence = appends
	deps.Clock = clocktest.New(time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC))
	deps.ShutdownDrainDeadline = clock.DeadlineAt(time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), true)
	service := newSeamService(t, deps)
	// Registered after the service, so they run before its shutdown, in reverse: a row that fails
	// with Shutdown still draining ends that drain, then lets the held append go.
	testcleanup.Add(t, appends.let)
	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	testcleanup.Add(t, cancelShutdown)
	service.SetFormGate(formKept)
	seedServiceText(t, service, artifactID, "Before.\n")
	settleCurrentGeneration(t, service, artifactID)
	appends.armed.Store(true)
	editLiveTree(t, service, artifactID, replaceRun("Before.", "After."))
	testwait.Receive(t, "the room's append to reach the store", appends.entered)

	shutdown := make(chan error, 1)
	go func() { shutdown <- service.Shutdown(shutdownCtx) }()
	err := testwait.Receive(t, "Shutdown to return at its drain deadline, long past on the wall clock", shutdown)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v, want the held append its drain deadline did not see through", err)
	}
	// The append lands once let go, so the shutdown the test's end runs finds nothing in flight.
	appends.let()
	if err := service.waitForDurableAppends(testwait.Context(t), artifactID); err != nil {
		t.Fatalf("wait for the held append to land once let go: %v", err)
	}
}
