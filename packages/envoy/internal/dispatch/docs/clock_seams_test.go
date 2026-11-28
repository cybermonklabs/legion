package docs

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
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
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown document service: %v", err)
		}
	})
	return service
}

// Deps.Clock left nil is the real clock, and the reference excerpt reconciliation hands its
// statements no moment, so the database's now() stamps and judges every retry; a settle delay left
// zero is DefaultSettle. A clock a test hands is the one every bound runs on, and the
// reconciliation's moment is its reading.
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
	due := fake.Now().Add(settle)
	fake.AwaitArmedAt(t, "the room's settlement after the write", due)
	fake.Advance(settle - time.Nanosecond)
	if armed := fake.Armed(); !slices.ContainsFunc(armed, due.Equal) {
		t.Fatalf("the settlement due at %v is no longer pending a nanosecond short of it (pending: %v)", due, armed)
	}
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

// VerifyMark's wait for a mark is the wall timer where Deps.Clock is nil, and otherwise a timer on
// that clock, which fires once the clock reaches the wait and not before.
func TestTheMarkWaitRunsOnDepsClock(t *testing.T) {
	t.Parallel()
	if got, want := reflect.ValueOf(markTimerOn(nil)).Pointer(), reflect.ValueOf(wallMarkTimer).Pointer(); got != want {
		t.Fatal("a service given no clock arms VerifyMark's wait on something other than the wall timer")
	}
	const wait = 300 * time.Millisecond
	fake := clocktest.New(time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC))
	due := fake.Now().Add(wait)
	fired, stop := markTimerOn(fake)(wait)
	defer stop()
	fake.AwaitArmedAt(t, "VerifyMark's wait", due)
	fake.Advance(wait - time.Nanosecond)
	if armed := fake.Armed(); !slices.ContainsFunc(armed, due.Equal) {
		t.Fatalf("VerifyMark's wait due at %v is no longer pending a nanosecond short of it (pending: %v)", due, armed)
	}
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
	fake.AwaitArmedAt(t, "the noted room's first check", due)
	fake.Advance(defaultCompactionTiming.firstCheck - time.Nanosecond)
	if armed := fake.Armed(); !slices.ContainsFunc(armed, due.Equal) {
		t.Fatalf("the first check due at %v is no longer pending a nanosecond short of it (pending: %v)", due, armed)
	}
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

// The relay counts, for the test hooks, each awareness change of a host that counts them, as the
// service does: a state another process relayed, its removal, and a removal the room made itself
// of a client it had taken a state of from another process (Expired); a removal of this
// process's own client takes no entry.
func TestTheRelayCountsAwarenessChangesForAServiceWithTheHooks(t *testing.T) {
	t.Parallel()
	service := &Service{hooks: newTestHooks()}
	relay := newAwarenessRelay(service)
	relay.countAwarenessChange("doc", 7, ownRemoval)
	relay.countAwarenessChange("doc", 9, relayedState)
	relay.countAwarenessChange("doc", 9, ownRemoval)
	relay.countAwarenessChange("doc", 9, relayedRemoval)
	counters := service.hooks.counters["doc"]
	if counters == nil {
		t.Fatal("the relay counted no awareness change of a service whose test hooks are on")
	}
	want := map[uint64]RelayedAwarenessChanges{9: {States: 1, Removals: 1, Expired: 1}}
	if !reflect.DeepEqual(counters.RelayedAwareness, want) {
		t.Fatalf("the hooks counted %+v, want %+v", counters.RelayedAwareness, want)
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
	deps.Settle, deps.TestHooks = time.Hour, true
	service := newSeamService(t, deps)
	service.SetFormGate(formKept)
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "Before.\n")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, replaceRun("Before.", "After."))

	// Each hook reports the first settlement of the document alone: the shutdown that ends the
	// test settles again, and finds both hooks spent.
	queued, release, ended := make(chan int, 1), make(chan struct{}), make(chan string, 1)
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
		var named int
		if err := database.Pool.QueryRow(ctx,
			`select count(*) from pg_stat_activity where datname = current_database() and application_name = $1`,
			SettlementApplicationName).Scan(&named); err != nil {
			named = -1
		}
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
	if named := testwait.Receive(t, "the settlement to queue its version", queued); named != 1 {
		t.Fatalf("while the settlement ran, %d sessions of the test's database were named %q, want its transaction's one", named, SettlementApplicationName)
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
	before := time.Now()
	if due := service.reconcileExcerptPass(ctx, referenceExcerptSchedule); due.Before(before.Add(wait)) {
		t.Fatalf("the pass answers its next run at %v, before the wall clock's %v read ahead of it plus the retry's wait of %v",
			due, before, wait)
	}
}

// Shutdown's drain ends at Deps.ShutdownDrainDeadline, a wall-clock moment, on the wall clock
// whatever Deps.Clock is: on a fake years behind the wall and never moved, a drain that a room's
// held append keeps waiting still ends once the wall clock reaches the deadline, and Shutdown
// returns then, and not before, with the append the drain did not see through. Measured and
// armed on the fake, the drain would last the years between the two clocks.
func TestShutdownsDrainEndsAtItsDeadlineOnTheWallClockWhateverDepsClockIs(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	appends := &heldStore{VersionedStore: NewPgVersioned(database), room: artifactID, entered: make(chan struct{}), release: make(chan struct{})}
	deadline := time.Now().Add(3 * time.Second)
	deps := testServiceDeps(database)
	deps.Persistence = appends
	deps.Clock = clocktest.New(time.Date(2020, 1, 1, 9, 0, 0, 0, time.UTC))
	deps.ShutdownDrainDeadline = clock.DeadlineAt(deadline, true)
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
	err := testwait.Receive(t, "Shutdown to return once the wall clock reached its drain deadline", shutdown)
	if returned := time.Now(); returned.Before(deadline) {
		t.Fatalf("Shutdown returned at %v, before its drain deadline %v", returned, deadline)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown returned %v, want the held append its drain deadline did not see through", err)
	}
	// The append lands once let go, so the shutdown the test's end runs finds nothing in flight.
	appends.let()
	if err := service.waitForDurableAppends(testwait.Context(t), artifactID); err != nil {
		t.Fatalf("wait for the held append to land once let go: %v", err)
	}
}
