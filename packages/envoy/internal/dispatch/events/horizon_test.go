package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The xmax hole, scripted with the counterexample the package documentation names. T2 took
// transaction id 100, T took 101 and drew event 7, then T2 drew event 8 and committed. The first
// observation sees 8 while T is open holding 7 and its write lock, so the oldest event writer it
// reads is T, 101. Its snapshot's xmax is 101, one past the newest completed id, and it lists
// nothing in progress, since 100 has completed and 101 is not below xmax: a rule trusting one
// snapshot's in-progress list would publish 8 here. That snapshot does not prove itself, because
// its xmax is not the observer's own id, 102. Only a later observation whose oldest event writer
// has passed the first observer's own transaction id proves it.
//
// Gap tells the reader what each step waits for: the first observation alone is unproven, so
// the reader observes again at once; the second finds T still running below the first observer's
// id, so the gap is held until T ends; the third proves both and is unproven itself; the fourth
// proves it and leaves nothing above H.
func TestHorizonProvesAnObservationOnlyFromALaterOne(t *testing.T) {
	ctx := context.Background()
	db := &scriptedHorizonDB{script: []scriptedObservation{
		{newest: 8, writer: 101, xmax: 101, xipEmpty: true, xid: 102},
		{newest: 8, writer: 101, xid: 103}, // T is still open
		{newest: 9, writer: 104, xid: 104}, // T has ended, and no event writer is open
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
// second observation proves the first whenever no older event writer is still running: the gap
// closes on two observations with no wait between. One held by an older event writer stays held
// for as long as that writer runs, however often the reader observes.
func TestHorizonGapClosesOnAnImmediateSecondObservation(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		second scriptedObservation
		safe   int64
		gap    Gap
	}{
		{name: "no older transaction running", second: scriptedObservation{newest: 8, writer: 102, xid: 102}, safe: 8, gap: GapNone},
		{name: "an older transaction still running", second: scriptedObservation{newest: 8, writer: 90, xid: 102}, safe: 0, gap: GapHeld},
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
// at once, so on a quiet instance it returns after two observations with no backoff between, and
// it backs off (10 ms, then 20 ms, ...) only while an older event writer holds its observation.
func TestHorizonAwaitBacksOffOnlyWhileAnOlderTransactionHoldsIt(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		script []scriptedObservation
		pauses []time.Duration
	}{
		{
			name: "no older transaction running",
			script: []scriptedObservation{
				{newest: 8, writer: 90, xid: 101},
				{newest: 8, writer: 102, xid: 102},
			},
		},
		{
			name: "an older transaction running for two backoffs",
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
// and the oldest event writer it reads holds an id just above observation i-window's, so it proves
// the observations up to i-window and no later one. Observations are 10 ms apart. H must then follow
// i-window closely after every observation, however many observations the window holds, and the
// pending list must stay within its bound. A window shorter than the bound keeps every
// observation, so H is exactly i-window. A longer one is thinned: thinning keeps neighbours at
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
			horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
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
	horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
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
		if got := waitForStallWarnings(logged, want); got != want {
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
// one; a test process runs warnEvery an order of magnitude below that bound, where every
// observation taken while a transaction stands would otherwise spawn a diagnosis of its own,
// several at once would queue for those two connections, and the observation the waiting reader
// needs would queue behind them. So one diagnosis is in flight at a time, and the next is due
// once it has ended.
func TestHorizonRunsOneStallDiagnosisAtATime(t *testing.T) {
	ctx := context.Background()
	// The diagnosis hands over and then holds: it is in flight from the send until release
	// closes, so a second one started meanwhile shows up as a second receive below.
	entered := make(chan struct{})
	release := make(chan struct{})
	var diagnoses atomic.Int32
	db := &scriptedHorizonDB{holder: func(...any) error {
		diagnoses.Add(1)
		entered <- struct{}{}
		<-release
		return pgx.ErrNoRows
	}}
	const observations = 4
	for i := range observations {
		db.script = append(db.script, scriptedObservation{newest: 5, writer: 10, xid: uint64(20 + i)})
	}
	logged := &storetest.LockedLog{}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	horizon := NewHorizon(db)
	horizon.now = func() time.Time { return now }
	// Thresholds a test process runs: every observation here is a stall old enough to warn
	// about, and none is held back by the rate limit, so only the diagnosis in flight can
	// suppress a warning.
	horizon.stallAfter = 0
	horizon.warnEvery = 0
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))

	if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
		t.Fatalf("Safe = %d, %v; want 0 and no error", safe, err)
	}
	<-entered // the first diagnosis is in flight, and stays so until release closes
	for step := range 2 {
		now = now.Add(time.Second)
		if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
			t.Fatalf("Safe at step %d, while a diagnosis is in flight, = %d, %v; want 0 and no error", step, safe, err)
		}
	}
	select {
	case <-entered:
		t.Fatalf("a second stall diagnosis started while the first was in flight; %d have run", diagnoses.Load())
	case <-time.After(100 * time.Millisecond): // let a diagnosis spawned by either observation reach its query
	}

	close(release)
	if got := waitForStallWarnings(logged, 1); got != 1 {
		t.Fatalf("stall warnings once the first diagnosis ended = %d, want 1; log:\n%s", got, logged.String())
	}
	// The flag is cleared in warnStall's defer, after the line is logged, so the next
	// observation waits on the flag rather than on the log.
	for ended := time.Now().Add(10 * time.Second); ; {
		horizon.mu.Lock()
		diagnosing := horizon.diagnosing
		horizon.mu.Unlock()
		if !diagnosing {
			break
		}
		if time.Now().After(ended) {
			t.Fatal("the first stall diagnosis logged its warning but never cleared the flag")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Suppression while one stands, not one warning for the whole stall: the next observation
	// diagnoses again.
	now = now.Add(time.Second)
	if safe, err := horizon.Safe(ctx); err != nil || safe != 0 {
		t.Fatalf("Safe once the first diagnosis ended = %d, %v; want 0 and no error", safe, err)
	}
	<-entered
	if got := waitForStallWarnings(logged, 2); got != 2 {
		t.Fatalf("stall warnings over %d stalled observations = %d, want 2; log:\n%s", observations, got, logged.String())
	}
	if got := diagnoses.Load(); got != 2 {
		t.Fatalf("%d stalled observations ran %d diagnoses, want 2: one while each was in flight was suppressed", observations, got)
	}
}

// A long-lived Await (the event relay, at the start of each generation of it) waits on a
// lifetime, so an observation that fails every time (a horizon pool that cannot dial) must reach
// its caller as an error to log and retry on once observations have failed for
// horizonAwaitFailFor, not only when ctx ends. One success between failures restarts the bound:
// a database that fails now and then is one an Await rides out. Each observation advances the
// clock a second: five failures, one success that proves nothing, then failures, of which Await
// must return the first one horizonAwaitFailFor after the first since the success.
func TestHorizonAwaitReturnsAnObservationErrorThatPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
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
	horizon.stallAfter = 24 * time.Hour // the stall warning is not under test
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
	horizon.warnEvery = 30 * time.Second
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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

// An observation whose own snapshot lists nothing in progress and whose xmax is the observer's own
// transaction id proves itself: every id below that id had ended before the snapshot was taken,
// so the id the snapshot saw is final at once, and H reaches it after a single Safe with no gap
// left open. Each half alone proves nothing: a snapshot whose xmax is below the observer's id can
// miss a writer above it (the xmax hole), and one that lists a transaction in progress can miss
// that transaction's event.
func TestHorizonPublishesAnObservationItsOwnSnapshotProves(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name        string
		observation scriptedObservation
		safe        int64
		gap         Gap
	}{
		{"nothing in progress, and xmax is the observer's id", scriptedObservation{newest: 8, writer: 103, xmax: 103, xipEmpty: true, xid: 103}, 8, GapNone},
		{"nothing in progress below an xmax under the observer's id", scriptedObservation{newest: 8, writer: 103, xmax: 102, xipEmpty: true, xid: 103}, 0, GapUnproven},
		{"a transaction in progress, and xmax is the observer's id", scriptedObservation{newest: 8, writer: 103, xmax: 103, xid: 103}, 0, GapUnproven},
	} {
		horizon := NewHorizon(&scriptedHorizonDB{script: []scriptedObservation{c.observation}})
		safe, err := horizon.Safe(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if gap := horizon.Gap(); safe != c.safe || gap != c.gap {
			t.Errorf("%s: after one observation Safe = %d, Gap = %d; want %d, %d", c.name, safe, gap, c.safe, c.gap)
		}
	}
}

// On a real Postgres, an event committed to a database with no event write open is published
// within two observations taken back to back, whatever the rest of the instance runs. The first
// proves itself when its own snapshot does (TestHorizonPublishesAnObservationItsOwnSnapshotProves
// holds that rule); a snapshot is the whole instance's, so any transaction running anywhere on it
// keeps that from happening, and the observation is then the only one unproven, with nothing of
// this database holding it back. The second then proves it, since no event writer of this
// database is open below the first observer's id. So the test asserts only what this database
// decides, and logs which of the two it took.
func TestHorizonPublishesAnEventOfAQuietDatabaseWithinTwoObservations(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	released := make(chan struct{})
	close(released)
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: released}
	horizon := NewHorizon(counting)

	id := commitHorizonEvent(ctx, t, database)
	safe, err := horizon.Safe(ctx)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if gap := horizon.Gap(); safe >= id {
		if gap != GapNone {
			t.Fatalf("Safe = %d after committing id %d, with Gap = %d; want no gap once the id is published", safe, id, gap)
		}
		t.Logf("the first observation proved itself: H = %d after committing id %d", safe, id)
		return
	} else if gap != GapUnproven {
		t.Fatalf("after one observation Safe = %d, Gap = %d, with no event write of this database open; want the observation unproven and held by nothing (Gap %d)",
			safe, gap, GapUnproven)
	}
	safe, err = horizon.Safe(ctx)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if gap := horizon.Gap(); safe < id || gap != GapNone {
		t.Fatalf("after a second observation Safe = %d, Gap = %d; want %d and no gap: no event writer of this database holds the first back",
			safe, gap, id)
	}
	if observations := counting.batches.Load(); observations != 2 {
		t.Fatalf("two Safe calls ran %d observations, want 2", observations)
	}
	t.Logf("the first observation's snapshot did not prove it, and the second proved it: H = %d after committing id %d", safe, id)
}

// An event writer open beside a committed event holds H below that event, whichever of the two
// began first, and H moves once the writer ends and a later observation runs. The writer holds the
// write lock an insert into events takes before it draws its id, and the horizon cannot tell which
// id that insert will draw, so an event committed before the writer even began waits for it too.
// No observation proves itself meanwhile: its snapshot lists the writer's transaction id in
// progress, or, when the event's writer took a higher id and committed, its xmax is below the
// observer's own id (the hole, which TestHorizonDoesNotPassAWriterAboveTheSnapshotXmax builds with
// the open writer holding the higher id). (A session that holds only an id holds no event id and
// does not hold H back: TestHorizonWaitsOnATransactionOnlyOnceItHasInsertedAnEvent.)
func TestHorizonWaitsForAnEventWriterAcrossBothCommitOrderings(t *testing.T) {
	for _, c := range []struct {
		name        string
		writerFirst bool
	}{
		{"the event committed before the writer began", false},
		{"the event committed after the writer began", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			database := storetest.Open(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			horizon := NewHorizon(database.Pool)

			var writer pgx.Tx
			openWriter := func() {
				writer, _ = openEventWriter(ctx, t, database)
				takeEventWriteLock(ctx, t, writer)
			}
			if c.writerFirst {
				openWriter()
			}
			id := commitHorizonEvent(ctx, t, database)
			if !c.writerFirst {
				openWriter()
			}
			requireHorizonBelow(ctx, t, horizon, id)

			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("end the open event writer: %v", err)
			}
			if safe, err := horizon.Await(ctx); err != nil || safe < id {
				t.Fatalf("Await after the open event writer ended = %d, %v; want at least %d", safe, err, id)
			}
		})
	}
}

// The xmax hole on a real Postgres. T2 takes its transaction id first; T takes a higher one
// and draws the lower event id; T2 draws the higher id and commits. A snapshot taken then has its
// xmax one past T2's id, the newest completed one, so T's id is not below it and T is missing
// from its in-progress list although T is open and holds a lower id than the snapshot sees: a
// rule that trusted that one snapshot would publish T2's id. The horizon must stay below T's id
// until T commits.
//
// The hole is a snapshot's, and snapshots are instance-wide: a transaction elsewhere on the
// instance (another test's, or this process's background database drops) that completes with a
// higher id in between moves xmax past T and closes it. So the scenario is built again, pausing
// between attempts, until a snapshot shows the hole, for up to thirty seconds; the test logs the
// attempt it took, and fails if no snapshot showed the hole, since it would then have proved
// nothing about it. The rule itself does not wait on the instance: the scripted hole of
// TestHorizonProvesAnObservationOnlyFromALaterOne holds it whatever the instance runs.
func TestHorizonDoesNotPassAWriterAboveTheSnapshotXmax(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
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
		if !hole {
			if err := writer.Commit(ctx); err != nil {
				t.Fatalf("commit T: %v", err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("after %d attempts over 30 s no snapshot showed the xmax hole (the last: xmax %d sees id %d; T held xid %d): "+
					"a transaction elsewhere on the instance completed with a higher id each time, so this test proved nothing about the hole",
					attempt, xmax, visible, writerXid)
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		t.Logf("xmax hole reproduced on attempt %d (snapshot xmax %d sees id %d; T holds xid %d and id %d)",
			attempt, xmax, visible, writerXid, lowerID)

		// The hole is why an empty in-progress list alone proves nothing: the observer's own
		// transaction id is above the snapshot's xmax here, so its observation cannot prove
		// itself whether T is listed in progress or hidden above xmax.
		observation, _, err := horizon.take(ctx, 0)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	// its own snapshot proves it, and 0 when it does not, never the old log's horizon.
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	<-gated.observed // the observation has read the old log and committed
	resetEventLog(ctx, t, database)
	head := commitHorizonEvent(ctx, t, database)
	horizon.Reset()
	close(gated.release)
	if err := <-done; err != nil {
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
// must be at or below H or keep a gap open.
func TestHorizonSafeWaitsOutAnObservationInFlightBeforeTheCall(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	gated := &gatedHorizonDB{HorizonDB: database.Pool, observed: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(gated)
	first := commitHorizonEvent(ctx, t, database)
	if safe, err := horizon.Await(ctx); err != nil || safe != first {
		t.Fatalf("Await on a quiet log = %d, %v; want %d", safe, err, first)
	}

	gated.armed.Store(true)
	stale := make(chan error, 1)
	go func() {
		_, err := horizon.Safe(ctx)
		stale <- err
	}()
	<-gated.observed // the stale observation has read the log and committed
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
	select {
	case got := <-called:
		t.Fatalf("Safe = %d, %v while the observation in flight before the call is held", got.safe, got.err)
	case <-time.After(100 * time.Millisecond): // let the call reach the observation in flight
	}
	close(gated.release)
	if err := <-stale; err != nil {
		t.Fatalf("Safe that ran the stale observation: %v", err)
	}
	got := <-called
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
// and share the next.
func TestHorizonConcurrentSafeCallsShareAnObservation(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	counting := &countingHorizonDB{HorizonDB: database.Pool, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)

	const callers = 8
	// Every caller but the one running the observation parks on it, and the test waits for all
	// of them to be parked before it lets that observation end: a caller still on its way would
	// find no observation in flight and run one of its own, which is the very thing this test
	// says must not happen. Waiting on the parking itself is what the run's own machinery
	// reports; a sleep long enough on an idle machine is not long enough on a loaded one.
	parked := make(chan struct{}, callers)
	horizon.afterObservationParked = func() { parked <- struct{}{} }
	errs := make(chan error, callers)
	for range callers {
		go func() {
			_, err := horizon.Safe(ctx)
			errs <- err
		}()
	}
	<-counting.entered
	for range callers - 1 {
		select {
		case <-parked:
		case <-ctx.Done():
			t.Fatalf("only some of the %d concurrent Safe calls reached the observation in flight: %v", callers, ctx.Err())
		}
	}
	close(counting.release)
	for range callers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent Safe: %v", err)
		}
	}
	if observations := counting.batches.Load(); observations > 2 {
		t.Fatalf("%d concurrent Safe calls ran %d observations, want at most 2: the one in flight and one they share", callers, observations)
	}
}

// Callers that waited out one observation share the next one, however late each of them wakes:
// the first to wake runs it, and one that wakes only after it has ended takes it, since it
// started after that caller's call. The hook holds every waiter but the first until that next
// observation has ended, the interleaving a loaded machine can give, under which each held waiter
// would otherwise find no observation in flight and run one of its own.
func TestHorizonCallersThatWaitedOutAnObservationShareTheNext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	script := make([]scriptedObservation, 16)
	for i := range script {
		script[i] = scriptedObservation{newest: 1, writer: uint64(100 + i), xid: uint64(100 + i)}
	}
	counting := &countingHorizonDB{HorizonDB: &scriptedHorizonDB{script: script}, entered: make(chan struct{}), release: make(chan struct{})}
	horizon := NewHorizon(counting)

	const waiters = 7
	parked := make(chan struct{}, waiters)
	horizon.afterObservationParked = func() { parked <- struct{}{} }
	var woke atomic.Int32
	gate := make(chan struct{})
	horizon.afterParkedObservationEnded = func() {
		if woke.Add(1) > 1 {
			<-gate
		}
	}
	errs := make(chan error, waiters+1)
	safe := func() {
		_, err := horizon.Safe(ctx)
		errs <- err
	}
	// The first caller runs the first observation, which the counting DB holds; every waiter
	// calls Safe while it is in flight, so each needs an observation numbered above it.
	go safe()
	<-counting.entered
	for range waiters {
		go safe()
	}
	for range waiters {
		select {
		case <-parked:
		case <-ctx.Done():
			t.Fatalf("only some of the %d waiters parked on the observation in flight: %v", waiters, ctx.Err())
		}
	}
	close(counting.release)
	// The first caller returns on the first observation, and the first waiter to wake on the
	// second, which it runs; only then do the other waiters look.
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Safe: %v", err)
		}
	}
	close(gate)
	for range waiters - 1 {
		if err := <-errs; err != nil {
			t.Fatalf("Safe: %v", err)
		}
	}
	if observations := counting.batches.Load(); observations != 2 {
		t.Fatalf("%d waiters of one observation, one waking at once and the rest after its next had ended, ran %d observations in all, want 2: the one they waited out and one they share",
			waiters, observations)
	}
}

// The stall warning's diagnosis query must run on a real Postgres and name the event writer that
// holds the horizon back: here the test's own open insert, and not an older transaction of
// another database on the instance, which holds no event id of this log.
func TestHorizonStallWarningNamesAnOpenTransaction(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	storetest.HoldTransactionIDInThePostgresDatabase(t)
	commitHorizonEvent(ctx, t, database)
	holder, _ := openEventWriter(ctx, t, database)
	if _, err := holder.Exec(ctx, `set local application_name = 'horizon-stall-holder'`); err != nil {
		t.Fatalf("name the holder: %v", err)
	}
	insertHorizonEvent(ctx, t, holder)
	logged := &storetest.LockedLog{}
	horizon := NewHorizon(database.Pool)
	horizon.stallAfter = 0
	horizon.logger = slog.New(slog.NewTextHandler(logged, nil))
	if _, err := horizon.Safe(ctx); err != nil {
		t.Fatalf("Safe: %v", err)
	}
	waitForStallWarnings(logged, 1)
	warning := logged.Lines(StallWarning)
	if warning == "" || strings.Contains(warning, "diagnosis_error") || !strings.Contains(warning, "pid=") {
		t.Fatalf("stall warning does not name an open transaction:\n%s", logged.String())
	}
	if !strings.Contains(warning, "application_name=horizon-stall-holder") {
		t.Fatalf("stall warning does not name the open event insert of this database, which holds the horizon back:\n%s", logged.String())
	}
	t.Log(strings.TrimSpace(warning))
}

// waitForStallWarnings waits up to ten seconds for logged to hold want stall warnings, which a
// Horizon logs on a goroutine of its own, and returns how many it holds.
func waitForStallWarnings(logged *storetest.LockedLog, want int) int {
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := strings.Count(logged.String(), StallWarning)
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireHorizonBelow asserts that Safe stays below id however often it is asked, and that Await
// does not return within a short wait, while the write holding id is open.
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
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if safe, err := horizon.Await(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Await while the write holding id %d is open = %d, %v; want it to wait", id, safe, err)
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

// takeEventWriteLock takes in tx the write lock an insert into events takes before its default
// draws the row's id, RowExclusiveLock, so tx is an event writer that has not yet drawn an id.
func takeEventWriteLock(ctx context.Context, t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, `lock table events in row exclusive mode`); err != nil {
		t.Fatalf("take the write lock on events: %v", err)
	}
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

func resetEventLog(ctx context.Context, t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(ctx, `truncate events restart identity`); err != nil {
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

// scriptedObservation is one observation's answer. xmax is its snapshot's xmax, the observer's own
// transaction id xid when left 0, as it is when nothing took an id between the snapshot and the
// observer, and xipEmpty is whether the snapshot lists nothing in progress: an observation proves
// itself only when both hold, its xmax being its own id and its list empty. Without them the
// script alone decides which observation proves which.
type scriptedObservation struct {
	newest   int64
	writer   uint64
	xmax     uint64
	xipEmpty bool
	xid      uint64
	err      error
}

func (db *scriptedHorizonDB) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	if db.observed != nil {
		db.observed()
	}
	if len(db.script) == 0 {
		return scriptedResults{err: errors.New("observation script exhausted")}
	}
	next := db.script[0]
	db.script = db.script[1:]
	return scriptedResults{observation: next, err: next.err}
}

func (db *scriptedHorizonDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if db.holder == nil {
		return scanRow(func(...any) error { return pgx.ErrNoRows })
	}
	return scanRow(db.holder)
}

type scriptedResults struct {
	observation scriptedObservation
	err         error
}

func (r scriptedResults) Exec() (pgconn.CommandTag, error) { return pgconn.CommandTag{}, r.err }
func (r scriptedResults) Query() (pgx.Rows, error)         { return nil, errors.New("not scripted") }
func (r scriptedResults) Close() error                     { return r.err }
func (r scriptedResults) Tx() pgx.Tx                       { return nil }

func (r scriptedResults) QueryRow() pgx.Row {
	return scanRow(func(dest ...any) error {
		if r.err != nil {
			return r.err
		}
		*dest[0].(*int64) = r.observation.newest
		*dest[1].(**time.Time) = nil
		*dest[2].(*string) = strconv.FormatUint(r.observation.writer, 10)
		xmax := r.observation.xmax
		if xmax == 0 {
			xmax = r.observation.xid
		}
		*dest[3].(*string) = strconv.FormatUint(xmax, 10)
		*dest[4].(*bool) = r.observation.xipEmpty
		*dest[5].(*string) = strconv.FormatUint(r.observation.xid, 10)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
// transaction id in another database on the instance leaves every mark good.
func TestWriterFloorIgnoresATransactionInAnotherDatabase(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	storetest.HoldTransactionIDInThePostgresDatabase(t)

	newest := commitHorizonEvent(ctx, t, database)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != newest {
		t.Fatalf("WriterFloor with a transaction open in another database = %d, %t, %v; want the mark's %d", floor, ok, err, newest)
	}
}

// safeAfterTwoObservations answers H after two observations: the second proves the first's A
// once no event writer older than the first observer is open.
func safeAfterTwoObservations(ctx context.Context, t *testing.T, horizon *Horizon) int64 {
	t.Helper()
	var safe int64
	for range 2 {
		var err error
		if safe, err = horizon.Safe(ctx); err != nil {
			t.Fatalf("Safe: %v", err)
		}
	}
	return safe
}

// Only a transaction of Dispatch's own database can write its event log. A transaction holding a
// transaction id in another database on the instance (another service's job, another package's
// create or drop database on a shared test server) and an event write open in another Dispatch
// database on the same instance, whose events table has this one's oid, hold no event id of this
// log, so H passes an event committed while both are open as soon as a later observation looks.
func TestHorizonDoesNotWaitOnATransactionInAnotherDatabase(t *testing.T) {
	database := storetest.Open(t)
	other := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	var here, there uint32
	if err := database.Pool.QueryRow(ctx, `select 'events'::regclass::oid`).Scan(&here); err != nil {
		t.Fatalf("read this database's events oid: %v", err)
	}
	if err := other.Pool.QueryRow(ctx, `select 'events'::regclass::oid`).Scan(&there); err != nil {
		t.Fatalf("read the other database's events oid: %v", err)
	}
	if here != there {
		t.Fatalf("the two databases' events tables have oids %d and %d, so only the relation, not the database, would tell their writes apart, and this test proves less than it says", here, there)
	}

	storetest.HoldTransactionIDInThePostgresDatabase(t)
	otherWriter, _ := openEventWriter(ctx, t, other)
	insertHorizonEvent(ctx, t, otherWriter)

	id := commitHorizonEvent(ctx, t, database)
	if safe := safeAfterTwoObservations(ctx, t, horizon); safe != id {
		t.Fatalf("Safe after two observations, with a transaction id held in the postgres database and an event write open in another Dispatch database = %d; want %d, the event this database committed", safe, id)
	}
}

// A transaction of this database that holds a transaction id and has inserted no event holds no
// event id: nextval hands any id it draws later above every id observed so far. So it does not
// hold H back (a write doing other work before its append, a read in a transaction, an operator's
// open session). Once it inserts an event, that event's write lock keeps H below its id until it
// ends, however often a reader asks.
func TestHorizonWaitsOnATransactionOnlyOnceItHasInsertedAnEvent(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)

	holder, _ := openEventWriter(ctx, t, database)
	first := commitHorizonEvent(ctx, t, database)
	if safe := safeAfterTwoObservations(ctx, t, horizon); safe != first {
		t.Fatalf("Safe after two observations, with a transaction id held in this database and no event inserted = %d; want %d", safe, first)
	}

	open := insertHorizonEvent(ctx, t, holder)
	if open <= first {
		t.Fatalf("the holder's insert drew id %d, not above %d, which H already passed", open, first)
	}
	higher := commitHorizonEvent(ctx, t, database)
	requireHorizonBelow(ctx, t, horizon, open)
	if err := holder.Commit(ctx); err != nil {
		t.Fatalf("commit the holder: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < higher {
		t.Fatalf("Await after the holder committed = %d, %v; want at least %d", safe, err, higher)
	}
}

// A RowExclusiveLock on events whose transaction id the lock table does not show holds H back
// when the lock table is read, and keeps every mark taken after it from counting: a transaction
// takes that lock when its INSERT, UPDATE or DELETE is parsed and its transaction id only when it
// first writes a tuple, so the read cannot tell such a holder from a writer about to draw an event
// id. A transaction of another database holds the snapshot's xmin back throughout, so the lock
// table is what each observation reads. An observation that does not read it (its xmin passes,
// or its snapshot proves it) does not wait for such a holder, which the writer invariant makes
// safe: a writer takes its transaction id before it draws an event id, so an undated holder holds
// none yet.
func TestAWriteLockWithNoTransactionIDHoldsTheHorizonBack(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}
	storetest.HoldTransactionIDInThePostgresDatabase(t)

	before, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	locker := lockEventsWithNoTransactionID(ctx, t, database, "row exclusive")
	id := commitHorizonEvent(ctx, t, database)
	if safe := safeAfterTwoObservations(ctx, t, horizon); safe >= id {
		t.Fatalf("Safe after two observations while a write lock on events with no transaction id is held = %d; want below %d", safe, id)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, before); err != nil || ok {
		t.Fatalf("WriterFloor while a write lock on events with no transaction id is held = %d, %t, %v; want no mark to count", floor, ok, err)
	}
	if err := locker.Rollback(ctx); err != nil {
		t.Fatalf("end the locker: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < id {
		t.Fatalf("Await once the locker ended = %d, %v; want at least %d", safe, err, id)
	}
}

// A lock on events of another mode draws no event id: VACUUM, ANALYZE and CREATE INDEX
// CONCURRENTLY of events hold ShareUpdateExclusiveLock, with no transaction id, for as long as
// they run. So such a lock holds neither H nor WriterFloor's marks back. A transaction of another
// database holds the snapshot's xmin back throughout, so the lock table is what each observation
// reads.
func TestALockOfAnotherModeHoldsNoEventID(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}
	storetest.HoldTransactionIDInThePostgresDatabase(t)

	lockEventsWithNoTransactionID(ctx, t, database, "share update exclusive")
	id := commitHorizonEvent(ctx, t, database)
	if safe := safeAfterTwoObservations(ctx, t, horizon); safe != id {
		t.Fatalf("Safe after two observations while a lock on events in share update exclusive mode is held = %d; want %d, the event committed meanwhile", safe, id)
	}
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || !ok || floor != id {
		t.Fatalf("WriterFloor while a lock on events in share update exclusive mode is held = %d, %t, %v; want the mark's %d", floor, ok, err, id)
	}
}

// An insert into events lists its RowExclusiveLock even when its transaction also holds a
// stronger lock on events, which moves that lock out of the lock table's fast path: the lock table
// gives a row for each mode a transaction holds. So such a writer still holds H below its event,
// and keeps a mark taken while it is open from counting.
func TestAnInsertUnderAStrongerLockIsStillAnEventWriter(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	if _, err := horizon.Await(ctx); err != nil {
		t.Fatalf("await the empty log: %v", err)
	}
	storetest.HoldTransactionIDInThePostgresDatabase(t)

	committed := commitHorizonEvent(ctx, t, database)
	writer, _ := openEventWriter(ctx, t, database)
	if _, err := writer.Exec(ctx, `lock table events in share row exclusive mode`); err != nil {
		t.Fatalf("lock events in share row exclusive mode: %v", err)
	}
	open := insertHorizonEvent(ctx, t, writer)
	mark, err := horizon.Mark(ctx)
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	if safe := safeAfterTwoObservations(ctx, t, horizon); safe >= committed {
		t.Fatalf("Safe after two observations while an insert of event %d under a share row exclusive lock is open = %d; want below %d, which the writer's older transaction id keeps unproven", open, safe, committed)
	}
	if floor, ok, err := horizon.WriterFloor(ctx, mark); err != nil || ok {
		t.Fatalf("WriterFloor of a mark taken while an insert under a share row exclusive lock is open = %d, %t, %v; want no mark to count", floor, ok, err)
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	if safe, err := horizon.Await(ctx); err != nil || safe < open {
		t.Fatalf("Await once the writer committed = %d, %v; want at least %d", safe, err, open)
	}
}

// lockEventsWithNoTransactionID begins a transaction that locks events in mode and takes no
// transaction id, as a statement that has locked its table and written no tuple yet, or a VACUUM,
// does. It rolls back when the test ends.
func lockEventsWithNoTransactionID(ctx context.Context, t *testing.T, database *store.Store, mode string) pgx.Tx {
	t.Helper()
	locker, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Rollback(context.Background()) })
	if _, err := locker.Exec(ctx, `lock table events in `+mode+` mode`); err != nil {
		t.Fatalf("lock events in %s mode: %v", mode, err)
	}
	var xid *string
	if err := locker.QueryRow(ctx, `select pg_current_xact_id_if_assigned()::text`).Scan(&xid); err != nil || xid != nil {
		t.Fatalf("the locker's transaction id = %v, %v; want none, or this test proves nothing", xid, err)
	}
	return locker
}

// The lock table gives 32-bit transaction ids, and a writer's id is the least of the ids it holds
// once each is widened to 64 bits against the reader's xmax. Run over lock rows of the test's own
// (eventWritersFrom), since no server a test runs against stands at a multiple of 2^32: a writer
// whose top id lies below 2^32 and whose subtransaction took an id past it is dated by its top id,
// not by the subtransaction's, whose low 32 bits are the smaller.
func TestTheOldestEventWriterIsDatedAcrossAnEpochBoundary(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const epoch = 1 << 32
	writeLock := func(vxid, mode string) string {
		return fmt.Sprintf(`('relation', (select oid from pg_database where datname = current_database()), 'events'::regclass::oid, null::xid, '%s', -7, '%s', true)`, vxid, mode)
	}
	idLock := func(vxid string, xid uint64) string {
		return fmt.Sprintf(`('transactionid', null::oid, null::oid, '%d'::xid, '%s', -7, 'ExclusiveLock', true)`, xid%epoch, vxid)
	}
	for _, row := range []struct {
		name  string
		xmax  uint64
		locks []string
		want  string
	}{
		{"a writer whose top id lies below 2^32 and whose subtransaction's lies past it", epoch + 150,
			[]string{writeLock("9/1", "RowExclusiveLock"), idLock("9/1", epoch-96), idLock("9/1", epoch+100)}, strconv.FormatUint(epoch-96, 10)},
		{"a writer above an xmax just below 2^32, whose id lies past it (the xmax hole)", epoch - 6,
			[]string{writeLock("9/1", "RowExclusiveLock"), idLock("9/1", epoch+200)}, strconv.FormatUint(epoch+200, 10)},
		{"two writers, the older in the epoch before the xmax's", 3*epoch + 10,
			[]string{writeLock("9/1", "RowExclusiveLock"), idLock("9/1", 3*epoch+5), writeLock("9/2", "RowExclusiveLock"), idLock("9/2", 3*epoch-1000)}, strconv.FormatUint(3*epoch-1000, 10)},
		{"a write lock whose transaction lists no id", epoch + 150,
			[]string{writeLock("9/1", "RowExclusiveLock"), idLock("9/2", epoch+100)}, "0"},
		{"a lock of another mode, with no id", epoch + 150,
			[]string{writeLock("9/1", "ShareUpdateExclusiveLock")}, ""},
	} {
		locks := "(values " + strings.Join(row.locks, ", ") + ") as v(locktype, database, relation, transactionid, virtualtransaction, pid, mode, granted)"
		sql := "with" + eventWritersFrom(locks, fmt.Sprintf("'%d'::xid8", row.xmax)) + oldestEventWriterSelect
		var oldest *string
		if err := database.Pool.QueryRow(ctx, sql).Scan(&oldest); err != nil {
			t.Fatalf("%s: read the oldest event writer: %v", row.name, err)
		}
		got := ""
		if oldest != nil {
			got = *oldest
		}
		if got != row.want {
			t.Errorf("%s: the oldest event writer = %q, want %q", row.name, got, row.want)
		}
	}
}

// An observation compares the snapshot's xmin with the newest pending observer's id, a uint64,
// and reads the lock table when the xmin does not pass it. The comparison must be exact for every
// 64-bit id: an id at 2^63 or past it, sent as a bigint, would read as negative, every xmin would
// pass it, and the observation would answer the xmin instead of the lock table's bound. A
// transaction of another database holds the xmin below the observer's own id, so the two answers
// differ: the lock table's bound, with no event writer open, is the observer's own id.
func TestAnObservationComparesAPendingIDExactlyPastInt64(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	horizon := NewHorizon(database.Pool)
	storetest.HoldTransactionIDInThePostgresDatabase(t)

	if observation, writer, err := horizon.take(ctx, 0); err != nil || writer >= observation.xid {
		t.Fatalf("an observation with nothing pending = bound %d, own id %d, %v; want the xmin, below its own id, or this test proves nothing", writer, observation.xid, err)
	}
	for _, pending := range []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64} {
		observation, writer, err := horizon.take(ctx, pending)
		if err != nil {
			t.Fatalf("observe with %d pending: %v", pending, err)
		}
		if writer != observation.xid {
			t.Errorf("an observation with %d pending answered %d, not its own id %d: it compared %d as some other number and took the snapshot's xmin", pending, writer, observation.xid, pending)
		}
	}
}

// A mark counts only while the log it saw stands. A reset restarts the ids at 1, so a writer of the
// new log draws ids at or below an old mark's Newest after that mark: once the new log has climbed
// past it, the mark would vouch for ids still open. WriterFloor passes over a mark whose newest row
// is gone or carries another created_at.
func TestWriterFloorPassesOverAMarkFromBeforeAReset(t *testing.T) {
	database := storetest.Open(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first := NewHorizon(database.Pool)
	first.PersistProven()
	later := NewHorizon(database.Pool)

	newest := commitHorizonEvent(ctx, t, database)
	persisted := int64(0)
	for deadline := time.Now().Add(30 * time.Second); persisted < newest; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the persisted horizon = %d after %s, want at least %d", persisted, 30*time.Second, newest)
		}
		if _, err := first.Safe(ctx); err != nil {
			t.Fatalf("observe: %v", err)
		}
		var err error
		if persisted, err = later.Persisted(ctx); err != nil {
			t.Fatalf("read the persisted horizon: %v", err)
		}
	}
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
			return scriptedResults{err: errors.New("connection refused")}
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
