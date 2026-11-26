package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The xmax hole, scripted with the counterexample the package documentation names. T2 took
// transaction id 100, T took 101 and drew event 7, then T2 drew event 8 and committed. The first
// observation's snapshot sees 8 and lists nothing in progress -- 101 is not below its xmax, so it
// is not in xip -- which is exactly the case where a rule trusting one snapshot's in-progress
// list would publish 8 while 7 can still commit. Its read of the event writers finds T, holding
// 101, below its own transaction id, and only a later read that finds no event writer below that
// id proves it.
//
// Gap tells the reader what each step waits for: the first observation alone is unproven, so
// the reader observes again at once; the second finds T still running below the first observer's
// id, so the gap is held until T ends; the third proves both and is unproven itself; the fourth
// proves it and leaves nothing above H.
func TestHorizonProvesAnObservationOnlyFromALaterSnapshot(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{script: []scriptedObservation{
		{newest: 8, writer: 101, xid: 102},
		{newest: 8, writer: 101, xid: 103}, // T is still open
		{newest: 9, writer: 104, xid: 104}, // T has ended, and so have both observers
		{newest: 9, writer: 105, xid: 105},
	}}
	horizon := NewHorizon(db)
	for step, want := range []struct {
		safe int64
		gap  Gap
	}{
		{safe: 0, gap: GapUnproven},
		{safe: 0, gap: GapHeld},
		{safe: 8, gap: GapUnproven},
		{safe: 9, gap: GapNone},
	} {
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("observation %d: %v", step+1, err)
		}
		if gap := horizon.Gap(); safe != want.safe || gap != want.gap {
			t.Fatalf("after observation %d: Safe = %d, Gap = %d; want %d, %d", step+1, safe, gap, want.safe, want.gap)
		}
	}
}

// A reader's read that leaves only its own observation unproven observes again at once, and that
// second observation proves the first whenever no older event writer is still open: the gap closes
// on two observations with no wait between. One held by an older event writer stays held for as
// long as that writer is open, however often the reader observes.
func TestHorizonGapClosesOnAnImmediateSecondObservation(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		second scriptedObservation
		safe   int64
		gap    Gap
	}{
		{name: "no older event writer open", second: scriptedObservation{newest: 8, writer: 102, xid: 102}, safe: 8, gap: GapNone},
		{name: "an older event writer still open", second: scriptedObservation{newest: 8, writer: 90, xid: 102}, safe: 0, gap: GapHeld},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &scriptedHorizonDB{script: []scriptedObservation{{newest: 8, writer: 90, xid: 101}, c.second}}
			horizon := NewHorizon(db)
			if safe, err := horizon.Safe(ctx); err != nil || safe != 0 || horizon.Gap() != GapUnproven {
				t.Fatalf("first observation: Safe = %d, %v, Gap = %d; want 0 and GapUnproven", safe, err, horizon.Gap())
			}
			if safe, err := horizon.Safe(ctx); err != nil || safe != c.safe || horizon.Gap() != c.gap {
				t.Fatalf("second observation: Safe = %d, %v, Gap = %d; want %d and %d", safe, err, horizon.Gap(), c.safe, c.gap)
			}
		})
	}
}

// Await shares the readers' cadence: while only its own observation is unproven it observes again
// at once, so with no older event write open it returns after two observations with no backoff
// between, and it backs off (10 ms, then 20 ms, ...) only while an older event writer holds its
// observation.
func TestHorizonAwaitBacksOffOnlyWhileAnOlderTransactionHoldsIt(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		script []scriptedObservation
		pauses []time.Duration
	}{
		{
			name: "no older event writer open",
			script: []scriptedObservation{
				{newest: 8, writer: 90, xid: 101},
				{newest: 8, writer: 102, xid: 102},
			},
		},
		{
			name: "an older event writer open for two backoffs",
			script: []scriptedObservation{
				{newest: 8, writer: 90, xid: 101},
				{newest: 8, writer: 90, xid: 102},
				{newest: 8, writer: 90, xid: 103},
				{newest: 8, writer: 104, xid: 104},
			},
			pauses: []time.Duration{horizonBackoffMin, 2 * horizonBackoffMin},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &scriptedHorizonDB{script: c.script}
			horizon := NewHorizon(db)
			var pauses []time.Duration
			horizon.pause = func(_ context.Context, d time.Duration) { pauses = append(pauses, d) }
			if safe, err := horizon.Await(ctx); err != nil || safe != 8 {
				t.Fatalf("Await = %d, %v; want 8", safe, err)
			}
			if len(db.script) != 0 {
				t.Fatalf("Await returned with %d scripted observations left, want every one taken", len(db.script))
			}
			if !slices.Equal(pauses, c.pauses) {
				t.Fatalf("Await paused %v, want %v", pauses, c.pauses)
			}
		})
	}
}

// Writes that overlap without a break keep every observation unproven for as long as the open
// transactions it overlaps. Scripted: observation i takes transaction id 1000+10i and sees id i,
// and the oldest event writer its read finds holds observation i-window's transaction id plus one,
// so it proves the observations up to i-window and no later one. Observations are 10 ms apart. H
// must then follow i-window closely after every observation, however many observations the window
// holds, and the pending list must stay within its bound. A window shorter than the bound keeps
// every observation, so H is exactly i-window. A longer one is thinned: thinning keeps neighbours at
// most step = ceil(4·span/horizonPendingLimit) apart, and span < window·10 ms, so neighbours are
// at most 4·window/horizonPendingLimit observations apart and H trails i-window by fewer than
// that: lagBound = 4·window/horizonPendingLimit (0 for the short window, 12 for the long one).
func TestHorizonFollowsContinuouslyOverlappingWrites(t *testing.T) {
	for _, window := range []int{200, 3 * horizonPendingLimit} {
		t.Run(fmt.Sprintf("window of %d observations", window), func(t *testing.T) {
			ctx := context.Background()
			lagBound := int64(4 * window / horizonPendingLimit)
			db := &scriptedHorizonDB{}
			horizon := NewHorizon(db)
			clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			horizon.now = func() time.Time { return clock }
			// The stall warning is not under test: the stalls these observations make warn into
			// nothing.
			horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			xid := func(i int) uint64 { return uint64(1000 + 10*i) }
			for i := range window + 4000 {
				clock = clock.Add(10 * time.Millisecond)
				writer := xid(0) // proves nothing
				if i >= window {
					writer = xid(i-window) + 1
				}
				db.script = []scriptedObservation{{newest: int64(i), writer: writer, xid: xid(i)}}
				safe, err := horizon.Safe(ctx)
				if err != nil {
					t.Fatalf("observation %d: %v", i, err)
				}
				if kept := len(horizon.pending); kept > horizonPendingLimit {
					t.Fatalf("after observation %d: %d pending observations, want at most %d", i, kept, horizonPendingLimit)
				}
				provable := max(int64(i-window), 0)
				if safe > provable || safe < provable-lagBound {
					t.Fatalf("after observation %d: Safe = %d, want it in [%d, %d]: proven through observation %d, within %d of it",
						i, safe, max(provable-lagBound, 0), provable, provable, lagBound)
				}
			}
		})
	}
}

// A writer held open across more observations than the pending list holds must cost no memory
// past the list's bound, and once it ends the horizon must reach the newest id observed, not the
// newest one that happened to fit in the list.
func TestHorizonReachesTheNewestIDAfterAStallLongerThanThePendingList(t *testing.T) {
	ctx := context.Background()
	const stuck = 3 * horizonPendingLimit
	db := &scriptedHorizonDB{}
	for i := range stuck {
		db.script = append(db.script, scriptedObservation{newest: int64(i + 1), writer: 10, xid: uint64(300 + i)})
	}
	db.script = append(db.script, scriptedObservation{newest: stuck, writer: 100_000, xid: 100_000})
	horizon := NewHorizon(db)
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon.now = func() time.Time { return clock }
	// The stall warning is not under test: the stall this writer makes warns into nothing.
	horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	for range stuck {
		clock = clock.Add(10 * time.Millisecond)
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe behind a writer open since transaction 10 = %d, %v; want 0", safe, err)
		}
		if kept := len(horizon.pending); kept > horizonPendingLimit {
			t.Fatalf("pending observations = %d, want at most %d", kept, horizonPendingLimit)
		}
	}
	if safe, err := horizon.Safe(ctx); err != nil || safe != stuck {
		t.Fatalf("Safe once the writer ended = %d, %v; want %d", safe, err, stuck)
	}
}

// A stall must name what causes it, at most once every warnEvery, and a diagnosis that fails
// must still leave the reader its horizon.
func TestHorizonStallWarningNamesTheHolderAndNeverFailsSafe(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{holder: func(...any) error { return errors.New("pg_stat_activity unavailable") }}
	for i := range 4 {
		db.script = append(db.script, scriptedObservation{newest: 5, writer: 10, xid: uint64(20 + i)})
	}
	logged := &storetest.LockedLog{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))

	for step, advance := range []time.Duration{0, 6 * time.Second, 14 * time.Second, 20 * time.Second} {
		now = now.Add(advance)
		if step == 3 {
			started := now.Add(-time.Minute)
			db.holder = func(dest ...any) error {
				*dest[0].(*string) = "backend"
				*dest[1].(*int32) = 4242
				*dest[2].(**time.Time) = &started
				*dest[3].(*string) = "idle in transaction"
				*dest[4].(*string) = "holder-app"
				*dest[5].(*string) = "dispatch"
				*dest[6].(*string) = "update issues set title = 'x'"
				return nil
			}
		}
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe at step %d = %d, %v; want 0 and no error", step, safe, err)
		}
		want := []int{0, 1, 1, 2}[step]
		if got := waitForStallWarnings(t, logged, want); got != want {
			t.Fatalf("stall warnings after step %d = %d, want %d; log:\n%s", step, got, want, logged.String())
		}
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if !strings.Contains(lines[0], "diagnosis_error=") || !strings.Contains(lines[0], "pending_for=6s") {
		t.Fatalf("first warning does not report its failed diagnosis and its 6 s wait: %s", lines[0])
	}
	for _, field := range []string{"pid=4242", "application_name=holder-app", "state=\"idle in transaction\"", "xact_start="} {
		if !strings.Contains(lines[1], field) {
			t.Fatalf("second warning lacks %s: %s", field, lines[1])
		}
	}
}

// The stall diagnosis scans pg_stat_activity on h.db, which for a server is the horizon pool:
// the two connections the horizon's own observations use. One diagnosis is bounded by
// horizonDiagnosisTimeout, so at the production warnEvery of 30 s it can never meet the next
// one; but a test process can run warnEvery an order of magnitude below that bound
// (SetStallWarning), where every observation taken while a transaction stands would otherwise
// spawn a diagnosis of its own, several at once would queue for those two connections, and the
// observation the waiting reader needs would queue behind them. So one diagnosis is in flight at
// a time, and the next is due once it has ended.
//
// The Horizon reads a scripted database here and does no network I/O, so the test runs in a
// synctest bubble, whose Wait returns once every goroutine the Horizon started is blocked: a
// second diagnosis started while the first is in flight is then blocked handing itself over,
// where the test finds it.
func TestHorizonRunsOneStallDiagnosisAtATime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		// The diagnosis hands over and then holds: it is in flight from the send until release
		// closes, so a second one started meanwhile is a second send waiting on entered.
		entered := make(chan struct{})
		release := make(chan struct{})
		var diagnoses atomic.Int32
		db := &scriptedHorizonDB{holder: func(...any) error {
			diagnoses.Add(1)
			entered <- struct{}{}
			<-release
			return pgx.ErrNoRows
		}}
		const observations = 5
		for i := range observations {
			db.script = append(db.script, scriptedObservation{newest: 5, writer: 10, xid: uint64(20 + i)})
		}
		logged := &storetest.LockedLog{}
		now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
		horizon := NewHorizon(db)
		horizon.now = func() time.Time { return now }
		horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
		// The production thresholds, with the clock moved warnEvery on before every observation
		// after the first: each of those is a stall old enough to warn about and none is held
		// back by the rate limit, so only the diagnosis in flight can suppress a warning.
		observe := func(what string) {
			t.Helper()
			if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
				t.Fatalf("Safe %s = %d, %v; want 0 and no error", what, safe, err)
			}
		}

		observe("at the stall's start")
		now = now.Add(horizonWarnEvery)
		observe("once the stall is old enough to warn about")
		<-entered // the first diagnosis is in flight, and stays so until release closes
		for step := range 2 {
			now = now.Add(horizonWarnEvery)
			observe(fmt.Sprintf("at step %d, while a diagnosis is in flight,", step))
		}
		// The read is horizonDiagnosisTimeout on the bubble's clock after every goroutine has
		// blocked (testwait.Idle), which costs nothing. The bubble's only timer of the Horizon's is
		// the first diagnosis's bound (horizonDiagnosisTimeout), armed before Idle began, so it has
		// fired by the read, and a second diagnosis that its firing starts has started by then. The
		// scripted holder does not watch that bound, so the first diagnosis is still in flight.
		testwait.Idle(t, horizonDiagnosisTimeout, "the first diagnosis's bound, horizonDiagnosisTimeout, is the one timer "+
			"the Horizon holds in the bubble, armed before this span began: a second diagnosis that a timer of the "+
			"Horizon's own starts while the first is in flight has started by the read")
		select {
		case <-entered:
			t.Fatalf("a second stall diagnosis started while the first was in flight; %d have run", diagnoses.Load())
		default:
		}

		close(release)
		synctest.Wait()
		if got := strings.Count(logged.String(), StallWarning); got != 1 {
			t.Fatalf("stall warnings once the first diagnosis ended = %d, want 1; log:\n%s", got, logged.String())
		}
		horizon.mu.Lock()
		diagnosing := horizon.diagnosing
		horizon.mu.Unlock()
		if diagnosing {
			t.Fatal("the first stall diagnosis logged its warning but never cleared the flag")
		}
		// Suppression while one stands, not one warning for the whole stall: the next observation
		// diagnoses again.
		now = now.Add(horizonWarnEvery)
		observe("once the first diagnosis ended")
		<-entered
		synctest.Wait()
		if got := strings.Count(logged.String(), StallWarning); got != 2 {
			t.Fatalf("stall warnings over %d stalled observations = %d, want 2; log:\n%s", observations-1, got, logged.String())
		}
		if got := diagnoses.Load(); got != 2 {
			t.Fatalf("%d stalled observations ran %d diagnoses, want 2: one while each was in flight was suppressed", observations-1, got)
		}
	})
}

// A long-lived Await (the event relay, at the start of each generation of it) waits on a
// lifetime, so an observation that fails every time (a horizon pool that cannot dial) must reach
// its caller as an error to log and retry on once observations have failed for
// horizonAwaitFailFor, not only when ctx ends. One success between failures restarts the bound:
// a database that fails now and then is one an Await rides out. Each observation advances the
// clock a second: five failures, one success that proves nothing, then failures, of which Await
// must return the first one horizonAwaitFailFor after the first since the success.
func TestHorizonAwaitReturnsAnObservationErrorThatPersists(t *testing.T) {
	ctx := testwait.Context(t)
	refused := errors.New("horizon pool: dial refused")
	const tick = time.Second
	failFor := int(horizonAwaitFailFor / tick)
	db := &scriptedHorizonDB{}
	for range 5 {
		db.script = append(db.script, scriptedObservation{err: refused})
	}
	db.script = append(db.script, scriptedObservation{newest: 5, writer: 10, xid: 20})
	for range failFor + 5 {
		db.script = append(db.script, scriptedObservation{err: refused})
	}
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	observations := 0
	db.observed = func() { observations++; clock = clock.Add(tick) }
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return clock }
	horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	safe, err := horizon.Await(ctx)
	if !errors.Is(err, refused) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await while every observation fails = %d, %v; want the observation's error before ctx's deadline", safe, err)
	}
	if want := 5 + 1 + failFor + 1; observations != want {
		t.Fatalf("Await returned its error after %d observations, want %d: five failures, a success, then %d s of failures",
			observations, want, failFor)
	}
}

// An observation that fails must say so in the log, since a long-lived Await otherwise waits
// horizonAwaitFailFor with nothing to show why. It is logged at most once every warnEvery, since
// Await re-observes every 250 ms, and not at all when the caller's own cancellation failed it.
func TestHorizonLogsAFailedObservationAtMostOnceEveryWarnEvery(t *testing.T) {
	refused := errors.New("horizon pool: dial refused")
	db := &scriptedHorizonDB{}
	logged := &storetest.LockedLog{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
	warnings := func() int { return strings.Count(logged.String(), "event horizon observation failed") }

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	db.script = []scriptedObservation{{err: context.Canceled}}
	if _, err := horizon.Safe(cancelled); err == nil {
		t.Fatal("Safe on a cancelled context returned no error")
	}
	if got := warnings(); got != 0 {
		t.Fatalf("failure warnings after the caller's own cancellation = %d, want 0; log:\n%s", got, logged.String())
	}

	// At 0, 1, 11, 29, 30 and 31 s: the second warning is due at 30 s, warnEvery after the first.
	for step, advance := range []time.Duration{0, time.Second, 10 * time.Second, 18 * time.Second, time.Second, time.Second} {
		now = now.Add(advance)
		db.script = []scriptedObservation{{err: refused}}
		if _, err := horizon.Safe(context.Background()); !errors.Is(err, refused) {
			t.Fatalf("Safe at step %d = %v, want the observation's error", step, err)
		}
		if got, want := warnings(), []int{1, 1, 1, 1, 2, 2}[step]; got != want {
			t.Fatalf("failure warnings after step %d = %d, want %d; log:\n%s", step, got, want, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "dial refused") {
		t.Fatalf("failure warning does not carry the observation's error:\n%s", logged.String())
	}
}

// A write that drew a lower id and is still open keeps the horizon below it however often a reader
// asks, while a higher id beside it has committed; once it commits, Await reaches past both.
func TestHorizonStaysBelowAnOpenLowerWrite(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}

	lower, _ := openEventWriter(ctx, t, database)
	a := insertHorizonEvent(ctx, t, lower)
	higher, _ := openEventWriter(ctx, t, database)
	b := insertHorizonEvent(ctx, t, higher)
	if err := higher.Commit(ctx); err != nil {
		t.Fatalf("commit the higher write: %v", err)
	}
	if b <= a {
		t.Fatalf("higher write drew id %d, not above the open write's %d", b, a)
	}

	requireHorizonBelow(ctx, t, horizon, a)
	if err := lower.Commit(ctx); err != nil {
		t.Fatalf("commit the lower write: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < b {
		t.Fatalf("Await after both writes committed = %d, %v; want at least %d", safe, err, b)
	}
}

// Only a transaction of Dispatch's own database that writes its event log can hold an event id, so
// a transaction that writes no event holds no reader back: one in another database on the
// instance, as another service's job, an operator's psql session or a test database's drop is, and
// one in Dispatch's own database that holds a transaction id and writes another table. An event
// committed while both stay open is final at once: one observation proves it, and Await answers
// the next one while both stay open, on the shared wait's context, so a horizon those
// transactions held would fail the row by name at the give-up.
func TestHorizonIsNotHeldByATransactionThatWritesNoEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	storetest.HoldClusterTransactionID(t)
	storetest.HoldTransactionIDWithoutAnEvent(t, database)

	id := commitHorizonEvent(ctx, t, database)
	horizon := NewHorizon(database.Pool)
	if safe, err := horizon.Safe(ctx); err != nil || safe < id || horizon.Gap() != GapNone {
		t.Fatalf("Safe while a transaction is open in another database and one that wrote no event is open here = %d, %v, gap %d; want at least %d and no gap",
			safe, err, horizon.Gap(), id)
	}
	next := commitHorizonEvent(ctx, t, database)
	if safe, err := horizon.Await(ctx); err != nil || safe < next {
		t.Fatalf("Await while those transactions stay open = %d, %v; want at least %d", safe, err, next)
	}
}

// With no event write open in Dispatch's database, one observation proves itself: its read of the
// event writers finds none below its own transaction id, so the id its snapshot saw is final at
// once. H must then reach a freshly committed event after a single Safe, with no gap left open and
// nothing to observe again for. Only an event writer of this database takes that away, and the
// test's database has none but the test's own, so one attempt is the whole test: a transaction of
// another package's test, in its own database on the shared Postgres, holds nothing here.
func TestHorizonProvesItsOwnObservationWithNoEventWriteOpen(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	for range 3 {
		id := commitHorizonEvent(ctx, t, database)
		if safe, err := horizon.Safe(ctx); err != nil || safe < id || horizon.Gap() != GapNone {
			t.Fatalf("Safe after committing id %d with no event write open = %d, %v, gap %d; want at least %d and no gap",
				id, safe, err, horizon.Gap(), id)
		}
	}
}

// An open event write takes the shortcut away: the observer's read of the event writers finds it
// below the observer's own transaction id, so the observation does not prove its own A however
// quiet the log is, and an event that committed before that write even drew its id waits for it,
// since the read cannot tell which ids a writer holds. H moves once the write ends and a later
// observation runs.
func TestHorizonDoesNotProveItsOwnObservationWhileAnEventWriteIsOpen(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	id := commitHorizonEvent(ctx, t, database)
	holder, holderXid := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, holder)
	observation, _, err := horizon.take(ctx)
	if err != nil {
		t.Fatalf("take one observation: %v", err)
	}
	if observation.selfProven {
		t.Fatalf("an observation proved itself while the write of event %d, holding transaction id %d, is open", open, holderXid)
	}
	if observation.newest < id {
		t.Fatalf("the observation saw id %d, not the committed %d", observation.newest, id)
	}
	requireHorizonBelow(ctx, t, horizon, id)

	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("end the open event write: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < open {
		t.Fatalf("Await after the write ended = %d, %v; want at least %d", safe, err, open)
	}
}

// A session that took a transaction id below an event writer's and has written no event holds no
// event id: an observation of that writer's committed event proves itself beside it. Once the
// session inserts an event it is an event writer, holding a transaction id below every later
// observer's, and the next event committed beside it waits for it.
func TestHorizonIsHeldByASessionOnlyOnceItWritesAnEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	holder, holderXid := openEventWriter(ctx, t, database)
	writer, writerXid := openEventWriter(ctx, t, database)
	id := insertHorizonEvent(ctx, t, writer)
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the event's writer: %v", err)
	}
	if writerXid <= holderXid {
		t.Fatalf("the event's writer took transaction id %d, not above the open session's %d", writerXid, holderXid)
	}
	observation, _, err := horizon.take(ctx)
	if err != nil {
		t.Fatalf("take one observation: %v", err)
	}
	if !observation.selfProven || observation.newest < id {
		t.Fatalf("an observation beside a session holding transaction id %d that wrote no event = %+v; want it to see %d and prove itself",
			holderXid, observation, id)
	}

	open := insertHorizonEvent(ctx, t, holder)
	next := commitHorizonEvent(ctx, t, database)
	requireHorizonBelow(ctx, t, horizon, open)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the session's event %d: %v", open, err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < next {
		t.Fatalf("Await after the session's write ended = %d, %v; want at least %d", safe, err, next)
	}
}

// An observation reads the event writers after the mark, in the same transaction: an event write
// that opens between the two reads takes its transaction id after the observer's, and so draws
// its event id after the mark's snapshot, above the mark's newest id. Read the other way round,
// the writers' read would find no writer, and the mark would then see an event committed above
// the write's open id and prove it at once, so a reader would pass the open id for good. Here the
// observation waits between its two reads on an advisory lock the test holds, while the test opens
// an event write and commits a higher event; the horizon must stay below the open id.
func TestAnObservationReadsTheEventWritersAfterItsMark(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	before := commitHorizonEvent(ctx, t, database)
	gate := holdObservationGate(ctx, t, database)
	db := &pausedHorizonDB{HorizonDB: database.Pool}
	db.armed.Store(true)
	horizon := NewHorizon(db)
	answered := safeInBackground(ctx, horizon)
	awaitObservationAtGate(ctx, t, database, "between its two reads")
	writer, _ := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, writer)
	committed := commitHorizonEvent(ctx, t, database)
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	got := testwait.Receive(t, "Safe of the observation the gate held to answer once the gate was released", answered)
	if got.err != nil || got.safe >= open {
		t.Fatalf("Safe of an observation whose two reads an event write opened between = %d, %v; want below the open event %d (the log stood at %d)",
			got.safe, got.err, open, before)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the open event: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < committed {
		t.Fatalf("Await once the write committed = %d, %v; want at least %d", safe, err, committed)
	}
}

// An event writer that prepares its transaction (PREPARE TRANSACTION) leaves its backend: Postgres
// lists it in pg_prepared_xacts, then moves its locks to the prepared transaction, which pg_locks
// shows with no pid, so from then on the read of the event writers finds it only among the
// prepared transactions. That read lists them after it has read pg_locks. A list read before
// pg_locks and used again after it misses a writer that prepared between the two, and the
// observation then proves an id above the writer's open event. Here the writer takes its
// transaction id and inserts an event, a higher event commits, and the observation then waits
// inside its read of the event writers, after its first step and before pg_locks
// (pausedHorizonDB.withinWriters), on an advisory lock the test holds, while the writer prepares.
// The horizon must stay below the writer's event until it commits. The server must allow
// prepared transactions (max_prepared_transactions above 0). An event insert notifies
// dispatch_events (the events_notify trigger), and Postgres refuses to prepare a transaction that
// has notified, so only an event write that bypassed that trigger can prepare: the writer here
// bypasses it as a logical replication apply worker does (session_replication_role = replica).
func TestAnObservationFindsAnEventWriterThatPreparesDuringItsRead(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	var allowed int
	if err := database.Pool.QueryRow(ctx, `select current_setting('max_prepared_transactions')::int`).Scan(&allowed); err != nil {
		t.Fatalf("read max_prepared_transactions: %v", err)
	}
	if allowed == 0 {
		t.Skip("the server runs with max_prepared_transactions = 0, so PREPARE TRANSACTION fails; " +
			"start it with -c max_prepared_transactions=10 to run this test")
	}
	if n := strings.Count(oldestEventWriterSQL, observerXactIDCall); n != 1 {
		t.Fatalf("oldestEventWriterSQL calls %s %d times, not once between its two steps: the test cannot pause it there",
			observerXactIDCall, n)
	}
	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`
		create function %s() returns xid8 language plpgsql as $$
		begin
			perform pg_advisory_xact_lock(%d);
			return pg_current_xact_id_if_assigned();
		end
		$$
	`, pausedXactIDFunction, pausedObservationLock)); err != nil {
		t.Fatalf("create %s: %v", pausedXactIDFunction, err)
	}

	writer, writerXid := openEventWriter(ctx, t, database)
	if _, err := writer.Exec(ctx, `set local session_replication_role = replica`); err != nil {
		t.Fatalf("turn the event writer's triggers off: %v", err)
	}
	open := insertHorizonEvent(ctx, t, writer)
	committed := commitHorizonEvent(ctx, t, database)
	gate := holdObservationGate(ctx, t, database)
	db := &pausedHorizonDB{HorizonDB: database.Pool, withinWriters: true}
	db.armed.Store(true)
	horizon := NewHorizon(db)
	answered := safeInBackground(ctx, horizon)
	awaitObservationAtGate(ctx, t, database, "inside its read of the event writers")
	// A prepared transaction's name is the server's, not the database's, and so is a transaction
	// id: a name made from the writer's id is one no other test's prepared transaction has.
	gid := fmt.Sprintf("horizon-test-%d", writerXid)
	if _, err := writer.Exec(ctx, "prepare transaction '"+gid+"'"); err != nil {
		t.Fatalf("prepare the event writer's transaction: %v", err)
	}
	t.Cleanup(func() { _, _ = database.Pool.Exec(context.Background(), "rollback prepared '"+gid+"'") })
	if err := gate.Rollback(ctx); err != nil {
		t.Fatalf("release the gate: %v", err)
	}
	got := testwait.Receive(t, "Safe of the observation the gate held inside its read of the event writers to answer once the gate was released", answered)
	if got.err != nil || got.safe >= open {
		t.Fatalf("Safe of an observation during whose read the writer of event %d prepared = %d, %v; want below %d (event %d committed above it)",
			open, got.safe, got.err, open, committed)
	}
	requireHorizonBelow(ctx, t, horizon, open)
	if _, err := database.Pool.Exec(ctx, "commit prepared '"+gid+"'"); err != nil {
		t.Fatalf("commit the prepared event write: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < committed {
		t.Fatalf("Await once the prepared write committed = %d, %v; want at least %d", safe, err, committed)
	}
}

// observationAnswer is what Safe answered on the goroutine safeInBackground ran it on.
type observationAnswer struct {
	safe int64
	err  error
}

// safeInBackground runs horizon.Safe on a goroutine of its own and sends its answer on the channel
// it returns.
func safeInBackground(ctx context.Context, horizon *Horizon) <-chan observationAnswer {
	answered := make(chan observationAnswer, 1)
	go func() {
		safe, err := horizon.Safe(ctx)
		answered <- observationAnswer{safe, err}
	}()
	return answered
}

// holdObservationGate takes the advisory lock pausedObservationLock in a transaction of its own and
// returns that transaction: a pausedHorizonDB's observation waits on the lock until it ends.
func holdObservationGate(ctx context.Context, t *testing.T, database *store.Store) pgx.Tx {
	t.Helper()
	gate, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback(context.Background()) })
	if _, err := gate.Exec(ctx, `select pg_advisory_xact_lock($1)`, pausedObservationLock); err != nil {
		t.Fatalf("take the gate's lock: %v", err)
	}
	return gate
}

// awaitObservationAtGate waits, through the shared wait, until a backend of this database waits on
// the gate's lock (holdObservationGate): the paused observation has reached the gate. where names
// the place the observation waits at, for the failure.
//
// The wait is for this database's waiter on the gate's own key: a waiter on an advisory lock in
// another database of the shared server would end it before the observation reached the gate, and
// the test would then pass with the order it checks broken.
func awaitObservationAtGate(ctx context.Context, t *testing.T, database *store.Store, where string) {
	t.Helper()
	testwait.Eventually(t, "the observation to reach the gate "+where, func() bool {
		var waiting bool
		if err := database.Pool.QueryRow(ctx, `
			select exists (select 1 from pg_locks
				where locktype = 'advisory' and not granted
					and database = (select oid from pg_database where datname = current_database())
					and classid = 0 and objid = $1 and objsubid = 1)
		`, pausedObservationLock).Scan(&waiting); err != nil {
			t.Fatalf("read whether the observation waits on the gate: %v", err)
		}
		return waiting
	})
}

// pausedObservationLock is the advisory lock key a pausedHorizonDB's observation waits on.
const pausedObservationLock = 4913

// observerXactIDCall is the call oldestEventWriterSQL makes between its two steps, for the
// observer's own transaction id.
const observerXactIDCall = "pg_current_xact_id_if_assigned()"

// pausedXactIDFunction names the function a pausedHorizonDB with withinWriters set calls in place
// of observerXactIDCall: it waits on pausedObservationLock, then answers that call. The test that
// sets withinWriters creates it in its database.
const pausedXactIDFunction = "horizon_test_paused_xact_id"

// pausedHorizonDB, once armed, holds the next observation on the advisory lock
// pausedObservationLock and passes every other batch through. It holds the observation between
// its two reads, with one statement added there that waits on the lock, or, with withinWriters
// set, inside its read of the event writers (oldestEventWriterSQL), which then calls
// pausedXactIDFunction in place of observerXactIDCall: after the read's first step has read the
// backends and the prepared transactions, and before its second reads pg_locks.
type pausedHorizonDB struct {
	HorizonDB
	armed         atomic.Bool
	withinWriters bool
}

func (db *pausedHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if len(batch.QueuedQueries) != 5 || !db.armed.CompareAndSwap(true, false) {
		return db.HorizonDB.SendBatch(ctx, batch)
	}
	paused := &pgx.Batch{}
	for index, query := range batch.QueuedQueries {
		sql := query.SQL
		switch {
		case index == 3 && db.withinWriters:
			sql = strings.Replace(sql, observerXactIDCall, pausedXactIDFunction+"()", 1)
		case index == 3:
			paused.Queue(`select pg_advisory_xact_lock($1)`, pausedObservationLock)
		}
		paused.Queue(sql, query.Arguments...)
	}
	if db.withinWriters {
		return db.HorizonDB.SendBatch(ctx, paused)
	}
	return &pausedResults{BatchResults: db.HorizonDB.SendBatch(ctx, paused)}
}

// pausedResults reads past the added statement's result before the observation's fourth.
type pausedResults struct {
	pgx.BatchResults
	read int
}

func (r *pausedResults) next() {
	if r.read++; r.read == 4 {
		_, _ = r.BatchResults.Exec()
	}
}

func (r *pausedResults) Exec() (pgconn.CommandTag, error) {
	r.next()
	return r.BatchResults.Exec()
}

func (r *pausedResults) QueryRow() pgx.Row {
	r.next()
	return r.BatchResults.QueryRow()
}

// The xmax hole on a real Postgres. T2 takes its transaction id first; T takes a higher one
// and draws the lower event id; T2 draws the higher id and commits. A snapshot taken then has its
// xmax one past T2's id, the newest completed one, so T's id is not below it and T is missing
// from its in-progress list although T is open and holds a lower id than the snapshot sees: a
// rule that trusted that one snapshot would publish T2's id. The horizon must stay below T's id
// until T commits.
//
// The snapshot is instance-wide: a transaction elsewhere on the instance (another test's, or this
// process's background database drops) that completes with a higher id in between moves xmax past
// T and closes the hole. So the scenario is built again, up to a bound, until a snapshot shows the
// hole, and the test logs whether it did. The horizon must stay below T's id either way: its read
// of the event writers finds T, whatever the snapshot lists.
func TestHorizonDoesNotPassAWriterAboveTheSnapshotXmax(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}

	const attempts = 10
	for attempt := 1; ; attempt++ {
		t2, _ := openEventWriter(ctx, t, database)
		writer, writerXid := openEventWriter(ctx, t, database)
		lowerID := insertHorizonEvent(ctx, t, writer)
		higherID := insertHorizonEvent(ctx, t, t2)
		if err := t2.Commit(ctx); err != nil {
			t.Fatalf("commit T2: %v", err)
		}

		var visible int64
		var xmaxText string
		if err := database.Pool.QueryRow(ctx,
			`select coalesce(max(id), 0), pg_snapshot_xmax(pg_current_snapshot())::text from events`,
		).Scan(&visible, &xmaxText); err != nil {
			t.Fatalf("read one snapshot: %v", err)
		}
		xmax, err := strconv.ParseUint(xmaxText, 10, 64)
		if err != nil {
			t.Fatalf("parse snapshot xmax %q: %v", xmaxText, err)
		}
		// A snapshot lists in-progress ids only below its xmax, so an id at or above it is not
		// listed: T is open, yet missing from the snapshot's in-progress list.
		hole := writerXid >= xmax && visible >= higherID
		if !hole && attempt < attempts {
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit T: %v", err)
			}
			continue
		}
		t.Logf("xmax hole reproduced: %v on attempt %d (snapshot xmax %d sees id %d; T holds xid %d and id %d)",
			hole, attempt, xmax, visible, writerXid, lowerID)

		// The hole is why no snapshot alone proves anything: the observer's read of the event
		// writers finds T below the observer's own transaction id, whether the snapshot lists T in
		// progress or hides it above xmax.
		observation, _, err := horizon.take(ctx)
		if err != nil {
			t.Fatalf("take one observation: %v", err)
		}
		if observation.selfProven {
			t.Fatalf("an observation proved itself while T, holding id %d and transaction id %d, is open", lowerID, writerXid)
		}
		requireHorizonBelow(ctx, t, horizon, lowerID)
		if err := writer.Commit(ctx); err != nil {
			t.Fatalf("commit T: %v", err)
		}
		if safe, err := horizon.Await(ctx); err != nil || safe < higherID {
			t.Fatalf("Await after T committed = %d, %v; want at least %d", safe, err, higherID)
		}
		return
	}
}

// Await must cover every id committed before it was called, so it may not settle for an
// observation taken before the call: on a quiet log each Await after a commit returns exactly
// the new head.
func TestHorizonAwaitCoversEveryIDCommittedBeforeTheCall(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	for range 4 {
		id := commitHorizonEvent(ctx, t, database)
		if safe, err := horizon.Await(ctx); err != nil || safe != id {
			t.Fatalf("Await after committing id %d on a quiet log = %d, %v; want %d", id, safe, err, id)
		}
	}
}

// A reset log (truncate events restart identity) hands out ids from 1 again, so a horizon that
// kept its old H would tell a reader that ids it has not yet seen are final, and skip them.
func TestHorizonResetForgetsTheOldLog(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != 3 {
		t.Fatalf("Await before the reset = %d, %v; want 3", safe, err)
	}

	resetEventLog(ctx, t, database)
	head := commitHorizonEvent(ctx, t, database)
	horizon.Reset()
	if gap := horizon.Gap(); gap != GapNone {
		t.Fatalf("Gap right after Reset = %d, want GapNone", gap)
	}
	// The first observation after Reset answers the new log alone: it reaches the new head when
	// its own read of the event writers proves it, and 0 when it does not, never the old log's
	// horizon.
	if safe, err := horizon.Safe(ctx); err != nil || safe > head {
		t.Fatalf("Safe right after Reset = %d, %v; want no more than the new log's head %d", safe, err, head)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != head {
		t.Fatalf("Await after Reset = %d, %v; want the new head %d", safe, err, head)
	}
}

// An observation of the old log that is still in flight when Reset runs must not be recorded
// after it: a later observation would prove it safe and raise H back to the old log's head.
func TestHorizonResetDiscardsAnObservationInFlight(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	gated := &gatedHorizonDB{HorizonDB: database.Pool, observed: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(gated)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != 3 {
		t.Fatalf("Await before the reset = %d, %v; want 3", safe, err)
	}

	gated.armed.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		done <- err
	}()
	testwait.Receive(t, "the gated observation to read the old log and commit", gated.observed)
	resetEventLog(ctx, t, database)
	head := commitHorizonEvent(ctx, t, database)
	horizon.Reset()
	close(gated.release)
	if err := testwait.Receive(t, "Safe to return across the reset once the gate was released", done); err != nil {
		t.Fatalf("Safe across the reset: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe != head {
		t.Fatalf("Await after Reset = %d, %v; want the new head %d", safe, err, head)
	}
}

// Safe must not settle for an observation already in flight when it is called: that one's
// snapshot can predate an event committed just before the call, and with its A at or below H it
// leaves no gap (Gap), so a reader would arm no re-read and the event would wait for the next
// notification however long that takes. Whatever Safe returns, an event committed before the call
// must be at or below H or keep a gap open. The test lets the observation in flight end only once
// the call has parked on it, so the call always finds it in flight, however slowly it gets there.
func TestHorizonSafeWaitsOutAnObservationInFlightBeforeTheCall(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	gated := &gatedHorizonDB{HorizonDB: database.Pool, observed: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(gated)
	first := commitHorizonEvent(ctx, t, database)
	if safe, err := horizon.Await(ctx); err != nil || safe != first {
		t.Fatalf("Await on a quiet log = %d, %v; want %d", safe, err, first)
	}

	// The stale observation's caller runs it, so only the call below parks.
	parked := make(chan struct{}, 1)
	horizon.afterObservationParked = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
	}
	gated.armed.Store(true)
	stale := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		stale <- err
	}()
	testwait.Receive(t, "the stale observation to read the log and commit", gated.observed)
	id := commitHorizonEvent(ctx, t, database)
	type result struct {
		safe int64
		err  error
	}
	called := make(chan result, 1)
	go func() {
		safe, err := horizon.Safe(ctx)
		called <- result{safe, err}
	}()
	testwait.Receive(t, "the call to park on the stale observation in flight", parked)
	close(gated.release)
	if err := testwait.Receive(t, "the Safe that ran the stale observation to return", stale); err != nil {
		t.Fatalf("Safe that ran the stale observation: %v", err)
	}
	got := testwait.Receive(t, "the Safe called while the stale observation was in flight to return", called)
	if got.err != nil {
		t.Fatalf("Safe after committing id %d: %v", id, got.err)
	}
	if got.safe < id && horizon.Gap() == GapNone {
		t.Fatalf("Safe after committing id %d = %d with no gap open: a reader would never re-read for it", id, got.safe)
	}
	horizon.mu.Lock()
	observed := horizon.newest
	horizon.mu.Unlock()
	if observed < id {
		t.Fatalf("Safe after committing id %d returned from observations that saw only up to %d", id, observed)
	}
}

// Every reader of a process calls Safe; concurrent calls must share observations, since each one
// costs a round trip and a transaction id. Callers that arrive while one is in flight wait it out
// and share the next, whether they reach it while it runs or only after it has ended.
func TestHorizonConcurrentSafeCallsShareAnObservation(t *testing.T) {
	database := storetest.Open(t)
	ctx := t.Context()
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)

	const callers = 8
	// The first caller's observation is held in flight, and every other caller calls only once it
	// is, so each of them must wait it out and take the next. The test waits for all of them to
	// be parked on it before it lets it end: a caller that called only after the next one had
	// ended would rightly run a third. Waiting on the parking itself is what the run's own
	// machinery reports; a sleep long enough on an idle machine is not long enough on a loaded
	// one.
	parked := make(chan struct{}, callers)
	horizon.afterObservationParked = func() { parked <- struct{}{} }
	errs := make(chan error, callers)
	safe := func() {
		_, err := horizon.Safe(ctx)
		errs <- err
	}
	go safe()
	testwait.Receive(t, "the first caller's observation to reach the database", counting.entered)
	for range callers - 1 {
		go safe()
	}
	for i := range callers - 1 {
		testwait.Receive(t, fmt.Sprintf("Safe call %d of the %d later ones to park on the held observation", i+1, callers-1), parked)
	}
	close(counting.release)
	for i := range callers {
		if err := testwait.Receive(t, fmt.Sprintf("Safe call %d of %d to return", i+1, callers), errs); err != nil {
			t.Fatalf("concurrent Safe: %v", err)
		}
	}
	// The held observation, and one more that the late callers share: the first of them to reach
	// the lock again runs it, and each of the others joins it in flight or, when it has already
	// ended, takes it as it stands, in whatever order the scheduler wakes them.
	if observations := counting.batches.Load(); observations != 2 {
		t.Fatalf("%d concurrent Safe calls ran %d observations, want 2: the one in flight when %d of them called, and the next, which those %d share", callers, observations, callers-1, callers-1)
	}
}

// heldScriptedHorizonDB is a scriptedHorizonDB that counts the observations it is asked for and
// holds the first, once entered closes, until release closes.
func heldScriptedHorizonDB(script ...scriptedObservation) (db *scriptedHorizonDB, observations *atomic.Int32, entered, release chan struct{}) {
	observations = &atomic.Int32{}
	entered, release = make(chan struct{}), make(chan struct{})
	db = &scriptedHorizonDB{script: script, observed: func() {
		if observations.Add(1) == 1 {
			close(entered)
			<-release
		}
	}}
	return db, observations, entered, release
}

// startFirstSafe calls horizon.Safe on a goroutine of its own, the first caller, whose observation
// a heldScriptedHorizonDB holds, and waits through the shared wait for that observation to reach
// the database (entered). It returns the join of that call: the shared wait for it to return,
// which fails t with the error it returned.
func startFirstSafe(t *testing.T, ctx context.Context, horizon *Horizon, entered <-chan struct{}) (join func()) {
	t.Helper()
	first := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		first <- err
	}()
	testwait.Receive(t, "the first caller's observation to reach the database", entered)
	return func() {
		t.Helper()
		if err := testwait.Receive(t, "the Safe that ran the first observation to return", first); err != nil {
			t.Fatalf("Safe that ran the first observation: %v", err)
		}
	}
}

// A caller that waited out an observation and wakes only after the next one has ended takes that
// one as it ended, since it started after the call, and runs none of its own. The test holds six
// of the seven late callers in the parking hook, before they wait on the observation in flight,
// until the seventh has run the next observation to its end, so each of the six wakes to find it
// ended, however the scheduler wakes them.
func TestHorizonCallersThatWakeAfterTheNextObservationEndedTakeIt(t *testing.T) {
	ctx := t.Context()
	const callers = 8
	var script []scriptedObservation
	for i := range callers {
		script = append(script, scriptedObservation{newest: 8, xid: uint64(101 + i), selfProves: true})
	}
	db, observations, entered, release := heldScriptedHorizonDB(script...)
	horizon := NewHorizon(db)
	// A caller that wakes late and runs an observation of its own can park on another's, so the
	// hook may run more than once per caller; parked holds every such call.
	parked := make(chan struct{}, 2*callers)
	late := make(chan struct{})
	var parks atomic.Int32
	horizon.afterObservationParked = func() {
		parked <- struct{}{}
		if parks.Add(1) < callers-1 {
			<-late
		}
	}
	errs := make(chan error, callers)
	safe := func() {
		_, err := horizon.Safe(ctx)
		errs <- err
	}
	go safe()
	testwait.Receive(t, "the first caller's observation to reach the database", entered)
	for range callers - 1 {
		go safe()
	}
	for i := range callers - 1 {
		testwait.Receive(t, fmt.Sprintf("Safe call %d of the %d later ones to park on the held observation", i+1, callers-1), parked)
	}
	close(release)
	// The first caller's answer, and that of the late caller the hook let through, which ran the
	// next observation: once both are in, that observation has ended.
	for i := range 2 {
		if err := testwait.Receive(t, fmt.Sprintf("answer %d of 2 before the held callers wake: the first caller's and the next observation's runner's", i+1), errs); err != nil {
			t.Fatalf("Safe: %v", err)
		}
	}
	close(late)
	for i := range callers - 2 {
		if err := testwait.Receive(t, fmt.Sprintf("Safe call %d of the %d woken after the next observation had ended to return", i+1, callers-2), errs); err != nil {
			t.Fatalf("Safe of a caller that woke after the next observation had ended: %v", err)
		}
	}
	if got := observations.Load(); got != 2 {
		t.Fatalf("%d Safe calls, %d of them woken after the next observation had ended, ran %d observations, want 2", callers, callers-2, got)
	}
}

// A caller whose context is live takes no ended observation that its runner's cancellation
// abandoned, even one that started after the call: it runs one of its own and returns that one's
// answer, not a cancellation that is not its own.
func TestHorizonACallerThatWokeLateTakesNoOtherCallersCancellation(t *testing.T) {
	ctx := t.Context()
	db, observations, entered, release := heldScriptedHorizonDB(
		scriptedObservation{newest: 8, xid: 101, selfProves: true},
		scriptedObservation{err: context.Canceled},
		scriptedObservation{newest: 8, xid: 103, selfProves: true},
	)
	horizon := NewHorizon(db)
	parked, late := make(chan struct{}), make(chan struct{})
	horizon.afterObservationParked = func() {
		close(parked)
		<-late
	}
	joinFirst := startFirstSafe(t, ctx, horizon, entered)
	waiter := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		waiter <- err
	}()
	testwait.Receive(t, "the second caller to park on the held observation", parked)
	close(release)
	joinFirst()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := horizon.Safe(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Safe on a cancelled context = %v, want its own cancellation", err)
	}
	close(late)
	if err := testwait.Receive(t, "the Safe of the caller that woke late to return", waiter); err != nil {
		t.Fatalf("Safe of the caller that woke late = %v, want the answer of an observation of its own", err)
	}
	if got := observations.Load(); got != 3 {
		t.Fatalf("ran %d observations, want 3: the one waited out, the cancelled one and the late caller's own", got)
	}
}

// Await asks observe again past the observation it took before, so a caller can join an
// observation numbered above that one which its runner's cancellation then abandons. Callers whose
// contexts are live wait that one out too and share the next, even one that wakes only after the
// next has ended.
func TestHorizonCallersOfAnAbandonedObservationShareTheNext(t *testing.T) {
	ctx := t.Context()
	runnerCtx, cancelRunner := context.WithCancel(ctx)
	defer cancelRunner()
	db, observations, entered, release := heldScriptedHorizonDB(
		scriptedObservation{err: context.Canceled},
		scriptedObservation{newest: 8, xid: 102, selfProves: true},
		scriptedObservation{newest: 8, xid: 103, selfProves: true},
	)
	horizon := NewHorizon(db)
	runner := make(chan error, 1)
	go func() {
		_, _, err := horizon.observe(runnerCtx, 0)
		runner <- err
	}()
	testwait.Receive(t, "the runner's observation to reach the database", entered)
	// The second caller to park is held before it waits, until the first has run the next
	// observation to its end.
	parked := make(chan struct{}, 2)
	late := make(chan struct{})
	var parks atomic.Int32
	horizon.afterObservationParked = func() {
		parked <- struct{}{}
		if parks.Add(1) == 2 {
			<-late
		}
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := horizon.observe(ctx, 0)
			errs <- err
		}()
	}
	for i := range 2 {
		testwait.Receive(t, fmt.Sprintf("caller %d of 2 to park on the runner's observation", i+1), parked)
	}
	cancelRunner()
	close(release)
	if err := testwait.Receive(t, "the runner's observe to return its cancellation", runner); !errors.Is(err, context.Canceled) {
		t.Fatalf("observe whose context ended = %v, want its own cancellation", err)
	}
	if err := testwait.Receive(t, "the observe of the caller that ran the next observation to return", errs); err != nil {
		t.Fatalf("observe of the caller that ran the next observation: %v", err)
	}
	close(late)
	if err := testwait.Receive(t, "the observe of the caller that woke after the next observation had ended to return", errs); err != nil {
		t.Fatalf("observe of the caller that woke after the next observation had ended: %v", err)
	}
	if got := observations.Load(); got != 2 {
		t.Fatalf("ran %d observations, want 2: the abandoned one and the next, which both callers share", got)
	}
}

// A Safe call that read the count while an observation was in flight, and looks for one only
// after another caller has run the next one to its end, takes that one, since it started after the
// call, and runs none of its own. The test holds the call in the boundary hook, after it read the
// count, until the other caller has its answer.
func TestHorizonACallerThatLooksLateAfterItsBoundaryTakesTheObservationThatEndedMeanwhile(t *testing.T) {
	ctx := t.Context()
	db, observations, entered, release := heldScriptedHorizonDB(
		scriptedObservation{newest: 8, xid: 101, selfProves: true},
		scriptedObservation{newest: 8, xid: 102, selfProves: true},
		scriptedObservation{newest: 8, xid: 103, selfProves: true},
	)
	horizon := NewHorizon(db)
	joinFirst := startFirstSafe(t, ctx, horizon, entered)
	counted, late := make(chan struct{}), make(chan struct{})
	var boundaries atomic.Int32
	horizon.afterObservationBoundary = func() {
		if boundaries.Add(1) == 1 {
			close(counted)
			<-late
		}
	}
	parked := make(chan struct{}, 1)
	horizon.afterObservationParked = func() { parked <- struct{}{} }
	held := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		held <- err
	}()
	testwait.Receive(t, "the held call to read the count at its boundary", counted)
	other := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		other <- err
	}()
	testwait.Receive(t, "the other call to park on the first observation", parked)
	close(release)
	joinFirst()
	if err := testwait.Receive(t, "the Safe that ran the next observation to return", other); err != nil {
		t.Fatalf("Safe that ran the next observation: %v", err)
	}
	close(late)
	if err := testwait.Receive(t, "the Safe held after its boundary to return", held); err != nil {
		t.Fatalf("Safe held after its boundary: %v", err)
	}
	if got := observations.Load(); got != 2 {
		t.Fatalf("ran %d observations, want 2: the one in flight at both later calls, and the next, which the call held after its boundary takes", got)
	}
}

// A Safe call answers from an observation recorded in the generation it returns H of, never from
// one a Reset discarded: that one's record is gone, so its answer is the H of 0 that the Reset
// left, with no gap open, and an event of the new log committed before the call is neither at or
// below H nor kept above it by a gap, so its reader would arm no re-read for it. The old log was
// observed through event 8, then the log was reset and its new event 1 committed before the calls.
// The test holds one call after its boundary while another runs the next observation, which sees
// the new event 1, and runs the Reset once that call has its answer, or while its observation is
// in flight. The run with no Reset is the control: the held call takes the observation that ended
// meanwhile and runs none of its own.
func TestHorizonACallerAcrossAResetAnswersFromAnObservationOfTheNewGeneration(t *testing.T) {
	const newEvent = 1
	for _, c := range []struct {
		name string
		// reset is whether the Reset runs, and inFlight whether it runs while the other call's
		// observation is in flight rather than once that call has its answer.
		reset, inFlight bool
		// other and held are the H the other call and the held call answer. observations is how
		// many observations run in all, and the number of the newest the current generation
		// recorded: the old log's, the other call's and, after a Reset, one of the new generation.
		other, held  int64
		observations int32
	}{
		{name: "no Reset", other: 8, held: 8, observations: 2},
		{name: "Reset after the other call has its answer", reset: true, other: 8, held: newEvent, observations: 3},
		{name: "Reset while the other call's observation is in flight", reset: true, inFlight: true, other: newEvent, held: newEvent, observations: 3},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			var observations atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			db := &scriptedHorizonDB{
				script: []scriptedObservation{
					{newest: 8, xid: 101, selfProves: true}, // the old log
					{newest: newEvent, xid: 102, selfProves: true},
					{newest: newEvent, xid: 103, selfProves: true},
				},
				observed: func() {
					if observations.Add(1) == 2 && c.inFlight {
						close(entered)
						<-release
					}
				},
			}
			horizon := NewHorizon(db)
			if safe, err := horizon.Safe(ctx); err != nil || safe != 8 {
				t.Fatalf("Safe on the old log = %d, %v; want 8", safe, err)
			}
			counted, late := make(chan struct{}), make(chan struct{})
			var boundaries atomic.Int32
			horizon.afterObservationBoundary = func() {
				if boundaries.Add(1) == 1 {
					close(counted)
					<-late
				}
			}
			type answer struct {
				safe int64
				gap  Gap
				err  error
			}
			call := func(answers chan<- answer) {
				safe, err := horizon.Safe(ctx)
				answers <- answer{safe, horizon.Gap(), err}
			}
			// check reports a call's answer and goes on, so a run shows each call's.
			check := func(who string, got answer, want int64) {
				t.Helper()
				switch {
				case got.err != nil:
					t.Errorf("Safe of %s: %v", who, got.err)
				case got.safe < newEvent && got.gap == GapNone:
					t.Errorf("Safe of %s = %d with no gap open, though event %d committed before the call: a reader would never re-read for it", who, got.safe, newEvent)
				case got.safe != want:
					t.Errorf("Safe of %s = %d, want %d", who, got.safe, want)
				}
			}
			held, other := make(chan answer, 1), make(chan answer, 1)
			go call(held)
			testwait.Receive(t, "the held call to read the count at its boundary", counted)
			go call(other)
			if c.inFlight {
				testwait.Receive(t, "the other call's observation to reach the database", entered)
				horizon.Reset()
				close(release)
			}
			check("the other call", testwait.Receive(t, "the other call to return", other), c.other)
			if c.reset && !c.inFlight {
				horizon.Reset()
			}
			close(late)
			check("the call held after its boundary", testwait.Receive(t, "the call held after its boundary to return", held), c.held)
			if got := observations.Load(); got != c.observations {
				t.Errorf("ran %d observations, want %d", got, c.observations)
			}
			horizon.mu.Lock()
			recorded := horizon.recorded
			horizon.mu.Unlock()
			if recorded != uint64(c.observations) {
				t.Errorf("the newest observation this generation recorded is %d, want %d", recorded, c.observations)
			}
		})
	}
}

// A caller after a Reset takes no ended observation whose record the Reset discarded: Reset
// forgets the one that ended before it, and one in flight across it does not stand as the newest
// ended observation once it ends, so the caller runs one of the new generation instead. Were
// either kept, a caller would be handed an observation whose record is gone, which Safe would
// discard before it looked again. The run with no Reset is the control: the caller takes the
// observation that ended and runs none of its own.
func TestHorizonACallerAfterAResetTakesNoObservationTheResetDiscarded(t *testing.T) {
	for _, c := range []struct {
		name string
		// reset is whether the Reset runs, and inFlight whether it runs while the first
		// observation is in flight rather than once it has ended.
		reset, inFlight bool
		// number and generation are the observation the later caller answers, and observations
		// how many ran in all.
		number, generation uint64
		observations       int32
	}{
		{name: "no Reset", number: 1, generation: 0, observations: 1},
		{name: "Reset after the first observation ended", reset: true, number: 2, generation: 1, observations: 2},
		{name: "Reset while the first observation is in flight", reset: true, inFlight: true, number: 2, generation: 1, observations: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			var observations atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			db := &scriptedHorizonDB{
				script: []scriptedObservation{
					{newest: 8, xid: 101, selfProves: true},
					{newest: 1, xid: 102, selfProves: true},
				},
				observed: func() {
					if observations.Add(1) == 1 && c.inFlight {
						close(entered)
						<-release
					}
				},
			}
			horizon := NewHorizon(db)
			type answer struct {
				number, generation uint64
				err                error
			}
			first := make(chan answer, 1)
			go func() {
				number, generation, err := horizon.observe(ctx, 0)
				first <- answer{number, generation, err}
			}()
			if c.inFlight {
				testwait.Receive(t, "the first observation to reach the database", entered)
				horizon.Reset()
				close(release)
			}
			// The first observation answers the generation it started in, which a Reset while it
			// was in flight has since left: that is what tells Safe to discard it.
			if got := testwait.Receive(t, "the first observation to end", first); got.err != nil || got.number != 1 || got.generation != 0 {
				t.Fatalf("observe of the first observation = observation %d of generation %d, %v; want observation 1 of generation 0", got.number, got.generation, got.err)
			}
			if c.reset && !c.inFlight {
				horizon.Reset()
			}
			number, generation, err := horizon.observe(ctx, 0)
			if err != nil || number != c.number || generation != c.generation {
				t.Errorf("observe after the first observation ended = observation %d of generation %d, %v; want observation %d of generation %d", number, generation, err, c.number, c.generation)
			}
			if got := observations.Load(); got != c.observations {
				t.Errorf("ran %d observations, want %d", got, c.observations)
			}
		})
	}
}

// A caller that waited out an observation takes the next one that ended with an answer, even when
// an observation of another caller, abandoned by that caller's cancellation, ended after it.
func TestHorizonALateCallerTakesTheNextObservationPastAnotherCallersCancellation(t *testing.T) {
	ctx := t.Context()
	db, observations, entered, release := heldScriptedHorizonDB(
		scriptedObservation{newest: 8, xid: 101, selfProves: true},
		scriptedObservation{newest: 8, xid: 102, selfProves: true},
		scriptedObservation{err: context.Canceled},
		scriptedObservation{newest: 8, xid: 104, selfProves: true},
	)
	horizon := NewHorizon(db)
	// The second caller to park is held before it waits, until the first has run the next
	// observation and a third caller's cancelled one has ended.
	parked := make(chan struct{}, 2)
	late := make(chan struct{})
	var parks atomic.Int32
	horizon.afterObservationParked = func() {
		parked <- struct{}{}
		if parks.Add(1) == 2 {
			<-late
		}
	}
	joinFirst := startFirstSafe(t, ctx, horizon, entered)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := horizon.Safe(ctx)
			errs <- err
		}()
	}
	for i := range 2 {
		testwait.Receive(t, fmt.Sprintf("caller %d of 2 to park on the first observation", i+1), parked)
	}
	close(release)
	joinFirst()
	if err := testwait.Receive(t, "the Safe that ran the next observation to return", errs); err != nil {
		t.Fatalf("Safe that ran the next observation: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := horizon.Safe(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Safe on a cancelled context = %v, want its own cancellation", err)
	}
	close(late)
	if err := testwait.Receive(t, "the Safe of the caller that woke after the cancelled observation ended to return", errs); err != nil {
		t.Fatalf("Safe of the caller that woke after the cancelled observation ended: %v", err)
	}
	if got := observations.Load(); got != 3 {
		t.Fatalf("ran %d observations, want 3: the one waited out, the next, which both late callers share, and the cancelled caller's own", got)
	}
}

// Await looks next for an observation numbered above every one that had ended when it last read
// the gap. One its gap check already counted tells it nothing new: taking it after a backoff
// would only leave the gap as it was and cost a second backoff before the observation that can
// prove anything. The test has Await join the first observation, numbered above its count, and
// holds it in the parking hook, before it waits on that one, until another caller has run the
// next to its end. An older transaction holds both, so Await's gap check reads both, held, and it
// backs off once; the third observation then proves them.
func TestHorizonAwaitBacksOffOnceForObservationsItsGapCheckRead(t *testing.T) {
	ctx := t.Context()
	db, observations, entered, release := heldScriptedHorizonDB(
		scriptedObservation{newest: 8, writer: 100, xid: 101},
		scriptedObservation{newest: 8, writer: 100, xid: 102}, // event writer 100 is still open
		scriptedObservation{newest: 8, xid: 103, selfProves: true},
	)
	horizon := NewHorizon(db)
	var pauses []time.Duration
	horizon.pause = func(_ context.Context, d time.Duration) { pauses = append(pauses, d) }
	counted, looks := make(chan struct{}), make(chan struct{})
	var boundaries atomic.Int32
	horizon.afterObservationBoundary = func() {
		if boundaries.Add(1) == 1 {
			close(counted)
			<-looks
		}
	}
	parked, late := make(chan struct{}), make(chan struct{})
	var parks atomic.Int32
	horizon.afterObservationParked = func() {
		if parks.Add(1) == 1 {
			close(parked)
			<-late
		}
	}
	var safe int64
	awaited := make(chan error, 1)
	go func() {
		var err error
		safe, err = horizon.Await(ctx)
		awaited <- err
	}()
	testwait.Receive(t, "Await to read the count at its boundary", counted) // 0: it looks only once the first observation is in flight
	joinFirst := startFirstSafe(t, ctx, horizon, entered)
	close(looks)
	testwait.Receive(t, "Await to park on the first observation", parked)
	close(release)
	joinFirst()
	if _, err := horizon.Safe(ctx); err != nil {
		t.Fatalf("Safe that ran the second observation: %v", err)
	}
	close(late)
	if err := testwait.Receive(t, "Await to return", awaited); err != nil || safe != 8 {
		t.Fatalf("Await = %d, %v; want 8", safe, err)
	}
	if got := observations.Load(); got != 3 {
		t.Fatalf("ran %d observations, want 3", got)
	}
	if want := []time.Duration{horizonBackoffMin}; !slices.Equal(pauses, want) {
		t.Fatalf("Await paused %v, want %v: after its one backoff it took again the second observation, which its gap check had already read", pauses, want)
	}
}

// The stall warning's diagnosis query must run on a real Postgres and name the event writer that
// holds the horizon: an open insert of this database's event log, not an older transaction in
// another database on the instance, which holds no event id.
func TestHorizonStallWarningNamesTheOpenEventWriter(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	storetest.HoldClusterTransactionID(t)
	commitHorizonEvent(ctx, t, database)
	holder, _ := openEventWriter(ctx, t, database)
	if _, err := holder.Exec(ctx, `set local application_name = 'horizon-stall-holder'`); err != nil {
		t.Fatalf("name the holder: %v", err)
	}
	insertHorizonEvent(ctx, t, holder)
	logged := &storetest.LockedLog{}
	horizon := NewHorizon(database.Pool)
	now := time.Now()
	horizon.now = func() time.Time { return now }
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
	// The production stallAfter: the first observation is the stall's start, and the second,
	// that span later on the Horizon's clock, finds it old enough to warn about.
	if _, err := horizon.Safe(ctx); err != nil {
		t.Fatalf("Safe: %v", err)
	}
	now = now.Add(horizonStallAfter)
	if _, err := horizon.Safe(ctx); err != nil {
		t.Fatalf("Safe: %v", err)
	}
	waitForStallWarnings(t, logged, 1)
	warning := logged.Lines(StallWarning)
	own := "database=" + database.Pool.Config().ConnConfig.Database
	if warning == "" || !strings.Contains(warning, "application_name=horizon-stall-holder") || !strings.Contains(warning, own) {
		t.Fatalf("stall warning does not name the open event writer (horizon-stall-holder, %s):\n%s", own, logged.String())
	}
	t.Log(strings.TrimSpace(warning))
}

// waitForStallWarnings waits for logged to hold want stall warnings, which a Horizon logs on a
// goroutine of its own, and returns how many it holds then.
func waitForStallWarnings(t *testing.T, logged *storetest.LockedLog, want int) int {
	t.Helper()
	return testwait.Until(t, fmt.Sprintf("%d stall warnings in the log", want), func() (int, bool) {
		got := strings.Count(logged.String(), StallWarning)
		return got, got >= want
	})
}

// requireHorizonBelow asserts that Safe stays below id however often it is asked, and that Await
// keeps waiting, while the write holding id is open.
func requireHorizonBelow(ctx context.Context, t *testing.T, horizon *Horizon, id int64) {
	t.Helper()
	for range 10 {
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("Safe: %v", err)
		}
		if safe >= id {
			t.Fatalf("Safe = %d while the write holding id %d is open", safe, id)
		}
	}
	if horizon.Gap() == GapNone {
		t.Fatalf("no gap open while a committed id above the open write's %d is observed", id)
	}
	// Await is cancelled once it has observed the log five times since the call, its backoff
	// past what a quick answer would have taken, and must answer the cancellation: an Await that
	// settled for an observation that cannot prove the open write would have answered first.
	type answer struct {
		safe int64
		err  error
	}
	horizon.mu.Lock()
	from := horizon.started
	horizon.mu.Unlock()
	waiting, stop := context.WithCancel(ctx)
	defer stop()
	answered := make(chan answer, 1)
	go func() {
		safe, err := horizon.Await(waiting)
		answered <- answer{safe, err}
	}()
	testwait.Eventually(t, fmt.Sprintf("Await to observe the log five times while the write holding id %d is open", id), func() bool {
		horizon.mu.Lock()
		defer horizon.mu.Unlock()
		return horizon.started >= from+5
	})
	stop()
	if got := testwait.Receive(t, "Await's answer to its cancellation", answered); !errors.Is(got.err, context.Canceled) {
		t.Fatalf("Await while the write holding id %d is open = %d, %v; want it to wait until cancelled", id, got.safe, got.err)
	}
}

// openEventWriter begins a transaction and takes its transaction id before anything else, as
// Broker.Append does before it draws an event id (the writer invariant).
func openEventWriter(ctx context.Context, t *testing.T, database *store.Store) (pgx.Tx, uint64) {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin event writer: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var text string
	if err := tx.QueryRow(ctx, `select pg_current_xact_id()::text`).Scan(&text); err != nil {
		t.Fatalf("take the writer's transaction id: %v", err)
	}
	xid, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		t.Fatalf("parse transaction id %q: %v", text, err)
	}
	return tx, xid
}

// insertHorizonEvent inserts one ownerless event row in tx and returns its id.
func insertHorizonEvent(ctx context.Context, t *testing.T, tx pgx.Tx) int64 {
	t.Helper()
	var id int64
	if err := tx.QueryRow(ctx, `
		insert into events (seq, type, actor, payload, notify)
		values (0, 'horizon.test', '{"kind":"user","id":"alice"}', '{}', false)
		returning id
	`).Scan(&id); err != nil {
		t.Fatalf("insert event: %v", err)
	}
	return id
}

// commitHorizonEvent inserts one event row in a transaction of its own and returns its id.
func commitHorizonEvent(ctx context.Context, t *testing.T, database *store.Store) int64 {
	t.Helper()
	tx, _ := openEventWriter(ctx, t, database)
	id := insertHorizonEvent(ctx, t, tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit event: %v", err)
	}
	return id
}

// resetEventLog empties the event log and restarts its ids, as e2e/seed.ts's reset does: with every
// table that references events (project_access_changes), which a truncate of events alone refuses.
func resetEventLog(ctx context.Context, t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
}

// scriptedHorizonDB answers each observation with the next scripted row (or its scripted
// failure) and the stall diagnosis with holder. observed, when set, runs as each observation
// starts.
type scriptedHorizonDB struct {
	script   []scriptedObservation
	holder   func(dest ...any) error
	observed func()
}

// scriptedObservation is one observation's answer: the newest id it sees, its observer's
// transaction id, and writer, the oldest transaction id an event writer holds as its read finds
// it. selfProves is a read that finds no event writer open at all, which is what makes an
// observation prove itself; without it the script alone decides which observation proves which,
// since every script here has writer at or below xid.
type scriptedObservation struct {
	newest     int64
	writer     uint64
	xid        uint64
	selfProves bool
	err        error
}

func (db *scriptedHorizonDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	if db.observed != nil {
		db.observed()
	}
	if len(db.script) == 0 {
		return &scriptedResults{err: errors.New("observation script exhausted")}
	}
	next := db.script[0]
	db.script = db.script[1:]
	return &scriptedResults{observation: next, err: next.err}
}

func (db *scriptedHorizonDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if db.holder == nil {
		return scanRow(func(...any) error { return pgx.ErrNoRows })
	}
	return scanRow(db.holder)
}

// scriptedResults answers an observation's batch: its first row is the mark's (A, its created_at
// and X), its second the event writers' read.
type scriptedResults struct {
	observation scriptedObservation
	err         error
	rows        int
}

func (r *scriptedResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r *scriptedResults) Query() (pgx.Rows, error)         { return nil, errors.New("not scripted") }
func (r *scriptedResults) Close() error                     { return r.err }
func (r *scriptedResults) Tx() pgx.Tx                       { return nil }

func (r *scriptedResults) QueryRow() pgx.Row {
	r.rows++
	writers := r.rows == 2
	return scanRow(func(dest ...any) error {
		if r.err != nil {
			return r.err
		}
		if writers {
			if r.observation.selfProves {
				*dest[0].(**string) = nil
				return nil
			}
			writer := strconv.FormatUint(r.observation.writer, 10)
			*dest[0].(**string) = &writer
			return nil
		}
		*dest[0].(*int64) = r.observation.newest
		*dest[1].(**time.Time) = nil
		*dest[2].(*string) = strconv.FormatUint(r.observation.xid, 10)
		return nil
	})
}

type scanRow func(dest ...any) error

func (s scanRow) Scan(dest ...any) error { return s(dest...) }

// countingHorizonDB counts observations and holds the first until release closes.
type countingHorizonDB struct {
	HorizonDB
	batches atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (db *countingHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if db.batches.Add(1) == 1 {
		close(db.entered)
		<-db.release
	}
	return db.HorizonDB.SendBatch(ctx, batch)
}

// gatedHorizonDB, once armed, lets one observation run to its commit and then holds it, before
// the Horizon sees its result, until release closes.
type gatedHorizonDB struct {
	HorizonDB
	armed    atomic.Bool
	observed chan struct{}
	release  chan struct{}
}

func (db *gatedHorizonDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	results := db.HorizonDB.SendBatch(ctx, batch)
	if !db.armed.CompareAndSwap(true, false) {
		return results
	}
	return &gatedResults{BatchResults: results, db: db}
}

type gatedResults struct {
	pgx.BatchResults
	db *gatedHorizonDB
}

func (r *gatedResults) Close() error {
	err := r.BatchResults.Close()
	close(r.db.observed)
	<-r.db.release
	return err
}

// WriterFloor bounds the ids a writer can still hold open by the newest mark older than every
// transaction that holds a write lock on events: an event insert open since before a mark may hold
// an id below its Newest, drawn before the mark's snapshot. An insert open since before the newer
// mark leaves only the older one, and once it ends the newer one counts again; with no mark older
// than it, the floor is H alone.
func TestWriterFloorTakesTheNewestMarkOlderThanEveryEventInsert(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	first := commitHorizonEvent(ctx, t, database)
	older, err := horizon.Mark(ctx)
	if err != nil || older.Newest != first {
		t.Fatalf("older mark = %+v, %v; want newest %d", older, err, first)
	}
	writer, _ := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, writer)
	second := commitHorizonEvent(ctx, t, database)
	newer, err := horizon.Mark(ctx)
	if err != nil || newer.Newest != second {
		t.Fatalf("newer mark = %+v, %v; want newest %d", newer, err, second)
	}

	if floor, ok, err := horizon.WriterFloor(ctx, newer, older); err != nil || !ok || floor != first {
		t.Fatalf("WriterFloor with an insert of event %d open since before the newer mark = %d, %t, %v; want the older mark's %d", open, floor, ok, err, first)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, newer); err != nil || ok || floor != 0 {
		t.Fatalf("WriterFloor with only a mark newer than an open write = %d, %t, %v; want H (0), not ok", floor, ok, err)
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatalf("end the open write: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, newer, older); err != nil || !ok || floor != second {
		t.Fatalf("WriterFloor once the write ended = %d, %t, %v; want the newer mark's %d", floor, ok, err, second)
	}
}

// Only a transaction in Dispatch's own database can write its event log, so one holding a
// transaction id in another database on the instance holds no event id: it holds back neither H
// (TestHorizonIsNotHeldByATransactionThatWritesNoEvent) nor any mark, and leaves every mark good.
func TestWriterFloorIgnoresATransactionInAnotherDatabase(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	storetest.HoldClusterTransactionID(t)

	newest := commitHorizonEvent(ctx, t, database)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != newest {
		t.Fatalf("WriterFloor with a transaction open in another database = %d, %t, %v; want the mark's %d", floor, ok, err, newest)
	}
}

// A mark counts only while the log it saw stands. A reset restarts the ids at 1, so a writer of the
// new log draws ids at or below an old mark's Newest after that mark: once the new log has climbed
// past it, the mark would vouch for ids still open. WriterFloor passes over a mark whose newest row
// is gone or carries another created_at.
func TestWriterFloorPassesOverAMarkFromBeforeAReset(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	commitHorizonEvent(ctx, t, database)
	old := commitHorizonEvent(ctx, t, database)
	before, err := horizon.Mark(ctx)
	if err != nil || before.Newest != old {
		t.Fatalf("mark before the reset = %+v, %v; want newest %d", before, err, old)
	}
	resetEventLog(ctx, t, database)
	for range 3 {
		commitHorizonEvent(ctx, t, database)
	}
	writer, _ := openEventWriter(ctx, t, database)
	open := insertHorizonEvent(ctx, t, writer)
	if open <= before.Newest {
		t.Fatalf("the new log's open id %d is not above the old mark's newest %d, so this test proves nothing", open, before.Newest)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, before); err != nil || ok || floor != 0 {
		t.Fatalf("WriterFloor with only a mark from before the reset = %d, %t, %v; want H (0), not ok", floor, ok, err)
	}
}

// A transaction that holds a transaction id but has inserted no event holds no event id: any it
// draws later it draws after every mark taken meanwhile, above their Newest. So it keeps no mark
// from counting, in Dispatch's own database as in another (a read in a transaction, a write to
// another table, an operator's open session).
func TestWriterFloorIgnoresATransactionThatHasInsertedNoEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)

	holder, _ := openEventWriter(ctx, t, database)
	newest := commitHorizonEvent(ctx, t, database)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != newest {
		t.Fatalf("WriterFloor with a transaction id held and no event inserted = %d, %t, %v; want the mark's %d", floor, ok, err, newest)
	}
	// Once it inserts, its event id stands above the mark.
	if id := insertHorizonEvent(ctx, t, holder); id <= mark.Newest {
		t.Fatalf("the holder's insert drew id %d, not above the mark's newest %d", id, mark.Newest)
	}
}

// A Horizon that persists writes its H to event_horizon once it advances, and another Horizon (a
// later process) reads it: every event at or below it is final, whichever process proved it. A
// value of a log since reset is not read: its event is gone, or carries another created_at.
func TestAPersistedHorizonIsReadByALaterProcessAndNotAcrossAReset(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	first := NewHorizon(database.Pool)
	first.PersistProven()
	later := NewHorizon(database.Pool)

	newest := commitHorizonEvent(ctx, t, database)
	testwait.Until(t, fmt.Sprintf("the persisted horizon to reach %d", newest), func() (int64, bool) {
		if _, err := first.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
		persisted, err := later.Persisted(ctx)
		if err != nil {
			t.Fatalf("read the persisted horizon: %v", err)
		}
		return persisted, persisted >= newest
	})
	if later.Proven() != 0 {
		t.Fatalf("the later process proved %d itself, so this test proves nothing", later.Proven())
	}

	resetEventLog(ctx, t, database)
	for range int(newest) + 2 {
		commitHorizonEvent(ctx, t, database)
	}
	if got, err := later.Persisted(ctx); err != nil || got != 0 {
		t.Fatalf("the persisted horizon after a reset of the log = %d, %v; want 0, the old log's value passed over", got, err)
	}
}

// persistCountingDB counts the persist writes (persistHorizonSQL) sent through it, and refuses
// each while fail is set; every other batch and read goes to the wrapped database.
type persistCountingDB struct {
	HorizonDB
	fail     bool
	persists atomic.Int32
}

func (db *persistCountingDB) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	if len(batch.QueuedQueries) == 1 && batch.QueuedQueries[0].SQL == persistHorizonSQL {
		db.persists.Add(1)
		if db.fail {
			return &scriptedResults{err: errors.New("connection refused")}
		}
	}
	return db.HorizonDB.SendBatch(ctx, batch)
}

// A persist write the database refuses is tried again only once horizonPersistEvery has passed,
// not by every observation that finds H above what was last written: each try runs on an
// observer's goroutine for up to horizonPersistTimeout, so a refusing database would otherwise hold
// a reader up at every observation.
func TestAFailedPersistIsTriedAgainOnlyOnceEveryPersistInterval(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	db := &persistCountingDB{HorizonDB: database.Pool, fail: true}
	horizon := NewHorizon(db)
	horizon.PersistProven()
	horizon.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	var clockMu sync.Mutex
	clock := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	horizon.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advance := func(by time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = clock.Add(by)
	}
	observeAdvancing := func(times int) {
		t.Helper()
		for range times {
			before := horizon.Proven()
			commitHorizonEvent(ctx, t, database)
			for horizon.Proven() == before {
				if _, err := horizon.Safe(ctx); err != nil {
					t.Fatalf("observe: %v", err)
				}
			}
		}
	}

	observeAdvancing(10)
	if got := db.persists.Load(); got != 1 {
		t.Fatalf("ten advances of H within one persist interval, every write refused, tried %d persist writes, want 1", got)
	}
	advance(horizonPersistEvery)
	observeAdvancing(3)
	if got := db.persists.Load(); got != 2 {
		t.Fatalf("after one more persist interval, %d persist writes tried, want 2", got)
	}
}

// A persist write decided before a Reset sends nothing once the Reset has moved the generation:
// the H it carries is the old log's.
func TestAPersistDecidedBeforeAResetWritesNothing(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	db := &persistCountingDB{HorizonDB: database.Pool}
	horizon := NewHorizon(db)
	newest := commitHorizonEvent(ctx, t, database)
	for horizon.Proven() < newest {
		if _, err := horizon.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	horizon.mu.Lock()
	safe, safeAt, generation := horizon.safe, horizon.safeAt, horizon.generation
	horizon.persisting = true
	horizon.mu.Unlock()

	horizon.Reset()
	horizon.writePersisted(safe, safeAt, generation)
	if got := db.persists.Load(); got != 0 {
		t.Fatalf("a persist decided before a Reset sent %d writes after it, want none", got)
	}
	horizon.mu.Lock()
	persisting := horizon.persisting
	horizon.mu.Unlock()
	if persisting {
		t.Fatal("a persist skipped for a Reset left the Horizon persisting, so it would never persist again")
	}
}

// A persist write that reaches the database after a reset of the log, with the Horizon not yet
// Reset, writes nothing: the event now at H's id is the new log's, which does not carry the
// created_at H was observed with, so the old log's H is never stamped as a value of the new log
// that a later process would trust (Persisted).
func TestAPersistOfAnOldLogsHorizonWritesNothingAfterTheLogIsReset(t *testing.T) {
	database := storetest.Open(t)
	ctx := testwait.Context(t)
	horizon := NewHorizon(database.Pool)
	newest := commitHorizonEvent(ctx, t, database)
	for horizon.Proven() < newest {
		if _, err := horizon.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
	}
	horizon.mu.Lock()
	safe, safeAt, generation := horizon.safe, horizon.safeAt, horizon.generation
	horizon.persisting = true
	horizon.mu.Unlock()
	if safeAt.IsZero() {
		t.Fatalf("H %d carries no created_at", safe)
	}

	resetEventLog(ctx, t, database)
	for range int(safe) + 1 {
		commitHorizonEvent(ctx, t, database)
	}
	horizon.writePersisted(safe, safeAt, generation)
	var rows int
	if err := database.Pool.QueryRow(ctx, `select count(*) from event_horizon`).Scan(&rows); err != nil {
		t.Fatalf("count event_horizon: %v", err)
	}
	if rows != 0 {
		t.Fatalf("a persist of the old log's H %d after a reset wrote %d rows, want none", safe, rows)
	}
	if got, err := horizon.Persisted(ctx); err != nil || got != 0 {
		t.Fatalf("Persisted after a reset = %d, %v; want 0", got, err)
	}
}
