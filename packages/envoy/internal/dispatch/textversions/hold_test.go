package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/benchtime"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestOneFillHoldsTheLeaseAndAnotherTakesItOver runs the fill as two serving processes run it,
// each on a store of its own, and writes events while both are woken by each: the hold and the
// cursor row name the first process alone, which writes every event's rows, while the second
// stands by. Then the first dies with its hold held: its pool closes, so no renewal and no
// release of its reaches the database, and its context ends. The second takes the hold once it
// lapses and writes the next events, and the cursor row's holder, once it names the second, never
// names the first again. Each wait is on its fact, the take and then the rows, which wait on the
// event horizon (textversionstest.AwaitRows). How soon after the death the second takes the hold
// is a service level, which TestAStandbyFillTakesTheHoldWithinAMinuteOfTheHoldersDeath holds in
// benchtime's timing lane.
//
// Both fills take their hold with stagedHoldTTL (Config.holdTTL), so the first's hold cannot lapse
// while both run however long a loaded machine stalls its renewals, which on holdOptions.TTL would
// hand the standby the hold early; the row reads the first's expiry past holdOptions.TTL, which is
// what the seam holds. At the death the row lapses the dead holder's row itself, as the TTL passing
// with no renewal does. The timing lane's twin runs the production TTL and lapses nothing.
func TestOneFillHoldsTheLeaseAndAnotherTakesItOver(t *testing.T) {
	fillTakeOverAfterDeath(t, benchtime.EveryRun)
}

// A standby takes the hold of a holder that died within 60 s of the death: the hold's 15 s TTL
// and the standby's 10 s retry. The 60 s bounds the take-over alone; the rows the standby then
// writes wait on the event horizon, which waits on the whole Postgres instance, so no span bounds
// them. A reading of time on a shared runner is its load as much as the product's work, so the
// row runs only in benchtime's timing lane (benchtime.Timing); that the standby takes over at all
// is TestOneFillHoldsTheLeaseAndAnotherTakesItOver's, which the suite runs.
func TestAStandbyFillTakesTheHoldWithinAMinuteOfTheHoldersDeath(t *testing.T) {
	benchtime.Timing(t, os.Getenv(benchtime.TimingEnv))
	if took := fillTakeOverAfterDeath(t, benchtime.OnTheClock); took > time.Minute {
		t.Fatalf("the standby took the dead holder's hold %v after the death; want within 60 s of the death", took.Round(time.Millisecond))
	}
}

// stagedHoldTTL is the hold TTL of the every-run take-over row (Config.holdTTL): staged state for
// the database's clock, which judges the hold's expiry, and for the hold's own loss deadline, not a
// wait, longer than any `go test -timeout` CI gives this package.
const stagedHoldTTL = 24 * time.Hour

// fillTakeOverAfterDeath is TestOneFillHoldsTheLeaseAndAnotherTakesItOver's run, and answers how
// long after the holder's death, on the machine's clock, the standby's take of the hold was read.
// On benchtime.EveryRun both fills hold with stagedHoldTTL and the row lapses the dead holder's
// row at the death; on benchtime.OnTheClock they hold with holdOptions.TTL and the hold lapses by
// itself.
func fillTakeOverAfterDeath(t *testing.T, lane benchtime.Lane) time.Duration {
	t.Helper()
	var holdTTL time.Duration
	if lane == benchtime.EveryRun {
		holdTTL = stagedHoldTTL
	}
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFillHolding(t, storetest.SecondProcess(t, database), nil, nil, true, holdTTL)
	firstHolder := awaitLiveHolder(t, database)
	if lane == benchtime.EveryRun {
		var left time.Duration
		if err := database.Pool.QueryRow(context.Background(),
			`select expires_at - now() from duty_holds where name = $1`, fillHoldName).Scan(&left); err != nil {
			t.Fatalf("read the first fill's hold: %v", err)
		}
		if left <= holdOptions.TTL {
			t.Fatalf("the first fill's hold expires %v from the database's now(), want more than holdOptions.TTL %v: the fill took it with the staged TTL (Config.holdTTL)", left, holdOptions.TTL)
		}
	}
	second := startFillHolding(t, storetest.SecondProcess(t, database), nil, nil, true, holdTTL)
	second.AwaitLine(t, standingByMessage)

	var holders cursorHolders
	var ids []int64
	// The comments are written 300 ms apart while both fills run, and the hold is read at each: what
	// is checked is an absence (the standby never takes the hold while the holder lives), so each
	// pause is a span held for bothFillsWhy.
	for range 10 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
		first.Wake()
		second.Wake()
		holders.sample(t, database)
		if live := liveHolder(t, database); live != firstHolder {
			t.Fatalf("while both fills run, the hold names %q; want the first's, %s", live, firstHolder)
		}
		testwait.Span(t, 300*time.Millisecond, bothFillsWhy)
	}
	textversionstest.AwaitRows(t, database, ids, func() { holders.sample(t, database) })
	if got := holders.read; len(got) != 1 || got[0] != firstHolder {
		t.Fatalf("while both fills ran the cursor row named %v; want the first's holder %s alone", got, firstHolder)
	}
	if second.Logged(t, batchMessage) {
		t.Fatalf("the standby wrote a batch while the first held the hold:\n%s", second.Log())
	}

	died := time.Now()
	testwait.Returns(t, "the dead holder's pool to close once its fill lets its connections go", first.db.Pool.Close)
	first.Stop(t, "the dead holder's keyword fill to return after its stop")
	if lane == benchtime.EveryRun {
		// The dead holder's row lapses as stagedHoldTTL passing with no renewal would leave it: its
		// expiry at the database's now(), its holder kept.
		if _, err := database.Pool.Exec(context.Background(),
			`update duty_holds set expires_at = now() where name = $1 and holder::text = $2`, fillHoldName, firstHolder); err != nil {
			t.Fatalf("lapse the dead holder's hold: %v", err)
		}
	}
	var next []int64
	for range 3 {
		next = append(next, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	second.Wake()
	testwait.Until(t, "the standby to take the dead holder's hold", func() (string, bool) {
		holders.sample(t, database)
		second.Wake()
		if live := liveHolder(t, database); live != "" && live != firstHolder {
			return "the hold names " + live, true
		}
		return holdRow(t, database), false
	})
	took := time.Since(died)
	t.Logf("the standby took the hold %v after the holder died", took.Round(time.Millisecond))
	textversionstest.AwaitRows(t, database, next, func() {
		holders.sample(t, database)
		second.Wake()
	})
	t.Logf("the standby wrote the next events %v after the holder died", time.Since(died).Round(time.Millisecond))
	holders.sample(t, database)
	got := holders.read
	if len(got) != 2 || got[0] != firstHolder || got[1] == firstHolder {
		t.Fatalf("the cursor row named %v in turn; want the first's holder %s, then the second's, and never the first's again",
			got, firstHolder)
	}
	if live := liveHolder(t, database); live != got[1] {
		t.Fatalf("the hold names %q; want the second's holder %s, which wrote the cursor row", live, got[1])
	}
	return took
}

// TestAStandbyTakesTheReleasedHold stops the holder as a serving process stops, by its context:
// the batch in flight commits or rolls back and the hold is released, which notifies the standby
// through its listener (duty.HoldReleasedChannel on a pglisten.Hub, as the server registers it).
// The standby's retry is one the test never runs (Config.retry), and its released wake is
// signalled by a notification naming the fill's hold alone (releasedWake), so the notification is
// the one thing that can make it take the hold: it takes the hold, and a pass writes the next
// event. Its events are woken only once it is seen to hold the hold, so a standby that needs an
// event's wake to take it fails. Each wait is on its fact, bounded by the test binary's deadline
// alone: a fixed span from the stop would include the pass's wait on the event horizon, which
// waits on the database's open event writes (textversionstest.AwaitRows), not on the standby.
func TestAStandbyTakesTheReleasedHold(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database)
	standbyStore := storetest.SecondProcess(t, database)
	released := releasedWake(t, standbyStore)
	second := startFillRetrying(t, standbyStore, released, make(chan time.Time), true)
	second.AwaitLine(t, standingByMessage)
	// The holder writes an event before it stops, so what it releases is a hold it fills under.
	written := commitComment(t, database, broker, textversionstest.Issue).id
	first.Wake()
	textversionstest.AwaitRows(t, database, []int64{written}, nil)

	first.Stop(t, "the holder's keyword fill to return after its stop")
	stopped := time.Now()
	if !first.Logged(t, releasedHoldMessage) {
		t.Fatalf("the holder stopped without logging its release:\n%s", first.Log())
	}
	if live := liveHolder(t, database); live == firstHolder {
		t.Fatalf("the holder stopped and the hold still names it, %s", firstHolder)
	}
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing wakes the standby's events before its take: a standby that took the hold only once
	// an event woke it would never take it, and the row would fail here by name.
	var standby string
	testwait.Until(t, "the standby to take the released hold on the release's notification", func() (string, bool) {
		if standby = liveHolder(t, database); standby != "" {
			return "the hold names " + standby, true
		}
		return holdRow(t, database), false
	})
	took := time.Since(stopped)
	// The take can come before the event's commit, and its first pass with it, so the row wakes
	// the standby's events now, as the event's commit does in production; the take is already
	// seen, so the wake cannot have brought it.
	second.Wake()
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	t.Logf("the standby took the released hold %v after the holder stopped and wrote the next event %v after",
		took.Round(time.Millisecond), time.Since(stopped).Round(time.Millisecond))
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestAStandbyTakesAFreeHoldWithNoNotification gives the standby a released wake that is never
// signalled, as a standby whose listener is down has: the holder releases its hold and nothing
// tells the standby, which leaves the hold free until its next retry. The test runs those retries
// (Config.retry, where a serving process's ticker of standbyRetry ticks), and one takes the free
// hold. Nothing wakes the standby's events, so a standby that took the hold only once an event
// woke it would not pass; its first pass under the hold writes what was written while it stood
// by.
//
// Each wait is on its fact, bounded by the test binary's deadline alone: the hold coming free, the
// retry's take, and the row, which the pass writes only once the event horizon, which waits on the
// database's open event writes, has passed the event (textversionstest.AwaitRows). A fixed span
// from the holder's stop, the ticker's 10 s and 5 s more, read the instance's load as well as the
// standby (merge-group run 44963211024: `the events [1] had 0 rows within 14.996081355s; want 1`).
func TestAStandbyTakesAFreeHoldWithNoNotification(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database)
	never := make(chan struct{})
	retry := make(chan time.Time, 1)
	second := startFillRetrying(t, storetest.SecondProcess(t, database), never, retry, true)
	second.AwaitLine(t, standingByMessage)

	first.Stop(t, "the holder's keyword fill to return after its stop")
	// The holder released its hold as it stopped, or, where its release did not reach the
	// database, the hold lapses holdOptions.TTL after its last renewal: either way the stopped
	// holder's hold ends, and nothing tells the standby.
	testwait.Until(t, "the stopped holder's hold to end", func() (string, bool) {
		live := liveHolder(t, database)
		return live, live != firstHolder
	})
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing has told the standby that the hold is free, and nothing wakes its events, so it has
	// not taken it: only a retry, below, can.
	if live := liveHolder(t, database); live != "" || second.Logged(t, tookHoldMessage) {
		t.Fatalf("the standby took the hold before its retry, with no wake for the release: the hold names %q live:\n%s", live, second.Log())
	}

	// The test retries as the ticker does, again after a try that found no free hold or failed,
	// until a retry takes the hold.
	retried := time.Now()
	var standby string
	testwait.Until(t, "the standby's retry to take the free hold", func() (string, bool) {
		select {
		case retry <- time.Now():
		default:
		}
		if standby = liveHolder(t, database); standby != "" {
			return "the hold names " + standby, true
		}
		return holdRow(t, database), false
	})
	took := time.Since(retried)
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	t.Logf("the standby's retry took the hold %v after the test's first retry and wrote the next event %v after",
		took.Round(time.Millisecond), time.Since(retried).Round(time.Millisecond))
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestTheFillWritesNothingUntilTheServerListens holds Serving open for 5 s with events written and
// the fill woken: it writes no row, no cursor row and no hold. Once Serving closes, as it does once
// the server listens and is admitted to the registry of live server processes, the rows come.
func TestTheFillWritesNothingUntilTheServerListens(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	fill := startFill(t, storetest.SecondProcess(t, database), nil, false)
	var ids []int64
	for range 3 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	fill.Wake()

	testwait.Span(t, 5*time.Second, notServingWhy)
	if rows := countRows(t, database); rows != 0 {
		t.Fatalf("before the process serves the fill wrote %d rows", rows)
	}
	var cursors, holds int
	if err := database.Pool.QueryRow(t.Context(), `
		select (select count(*) from search_text_versions_fill), (select count(*) from duty_holds where name = $1)
	`, fillHoldName).Scan(&cursors, &holds); err != nil {
		t.Fatalf("read the cursor row and the hold: %v", err)
	}
	if cursors != 0 || holds != 0 {
		t.Fatalf("before the process serves the fill wrote %d cursor rows and %d holds; want none", cursors, holds)
	}

	close(fill.serving)
	textversionstest.AwaitRows(t, database, ids, nil)
}

// TestACaughtUpFillSendsNoStatement runs the fill on a store whose every statement a tracer sees,
// the hold's renewals counted apart from the rest. Once the fill has written the events and gone
// quiet, it sends no statement for 10 s with nothing written but its hold's renewal, which goes
// on.
func TestACaughtUpFillSendsNoStatement(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	tracer := &renewalsApart{}
	fill := startFill(t, storetest.TracedStore(t, database, tracer), nil, true)
	awaitLiveHolder(t, database)
	var ids []int64
	for range 3 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	fill.Wake()
	textversionstest.AwaitRows(t, database, ids, nil)

	// Longer than the horizon's longest re-read (rereadMax), so a fill still reading it again is
	// not taken for one at rest.
	storetest.AwaitQuiet(t, &tracer.others, rereadMax+time.Second, caughtUpFillWhy)
	renewed := tracer.renewals.Load()
	storetest.RequireNoStatements(t, &tracer.others, 10*time.Second, "a fill caught up with nothing written", caughtUpFillWhy)
	// The renewal goes on at rest: a real ticker paces it every holdOptions.Renew, so the row waits
	// for one past the count read before the quiet span rather than counting them inside a span.
	testwait.Until(t, "the hold's renewal after the fill went quiet", func() (string, bool) {
		n := tracer.renewals.Load()
		return fmt.Sprintf("%d renewals of the hold, %d of them before the quiet span", n, renewed), n > renewed
	})
}

// caughtUpFillWhy is why the caught-up fill's rest is held as a span: a fill at rest is the
// absence of its statements, which no state of the fill shows, its waits are on the wall clock so
// synctest.Wait does not reach them, and no later action of the test ends a rest.
const caughtUpFillWhy = "a fill at rest is the absence of its statements, which no state of the fill shows; its waits run on the wall clock, out of synctest's reach, and no later action of the test ends a rest"

// renewalsApart counts the statements the store it traces sends (storetest.TracedStore): the
// hold's renewals (duty's renewHold) in renewals, and every other statement in others.
type renewalsApart struct {
	others   storetest.StatementCounter
	renewals atomic.Int64
}

func (r *renewalsApart) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(data.SQL), "update duty_holds set expires_at = now() + make_interval") {
		r.renewals.Add(1)
		return ctx
	}
	return r.others.TraceQueryStart(ctx, conn, data)
}

func (r *renewalsApart) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// fillProcess is the fill one serving process runs (Run, through textversionstest.StartFill), on a
// store of the test's own, so two of them take turns on the hold through the database as two
// processes do. Its serving is the process's, open until the test closes it where startFill did
// not.
type fillProcess struct {
	*textversionstest.Fill
	db      *store.Store
	serving chan struct{}
}

// startFill runs the fill on db, woken for a released hold by released, with the process serving
// already when serving is set, and stops it once the test ends. A standby of it retries on its
// ticker of standbyRetry.
func startFill(t *testing.T, db *store.Store, released <-chan struct{}, serving bool) *fillProcess {
	t.Helper()
	return startFillRetrying(t, db, released, nil, serving)
}

// startFillRetrying is startFill with a standby that retries each time retry receives, the test
// sending it where the ticker of standbyRetry would tick (Config.retry); nil is the ticker.
func startFillRetrying(t *testing.T, db *store.Store, released <-chan struct{}, retry <-chan time.Time, serving bool) *fillProcess {
	t.Helper()
	return startFillHolding(t, db, released, retry, serving, 0)
}

// startFillHolding is startFillRetrying whose fill takes its hold with holdTTL (Config.holdTTL);
// zero is holdOptions.TTL.
func startFillHolding(t *testing.T, db *store.Store, released <-chan struct{}, retry <-chan time.Time, serving bool, holdTTL time.Duration) *fillProcess {
	t.Helper()
	process := &fillProcess{db: db, serving: make(chan struct{})}
	if serving {
		close(process.serving)
	}
	config := Config{
		Leases:   storetest.Leases(t, db.Pool),
		Horizon:  events.NewHorizon(db.Pool),
		Latency:  sessionanswers.NewLatency(time.Now),
		Serving:  process.serving,
		Released: released,
		retry:    retry,
		holdTTL:  holdTTL,
	}
	process.Fill = textversionstest.StartFill(t, func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error {
		config.Events, config.Logger = wake, logger
		return Run(ctx, config)
	})
	return process
}

// releasedWake runs a listener hub on db's listener pool, as the server runs its own, and answers
// a wake signalled by each notification on duty.HoldReleasedChannel that names the fill's hold,
// once the hub listens. The server's own wake (pglisten.Hub.Wake) is signalled after each LISTEN
// too, so that a standby catches up on a release sent while it had no connection; this one is
// signalled by a notification alone, so a standby that takes the hold on it took it on the
// release's notification.
func releasedWake(t *testing.T, db *store.Store) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "textversions test", Dial: db.Pool.Listener}
	wake := make(chan struct{}, 1)
	hub.Handle(duty.HoldReleasedChannel, func(payload string) {
		if payload != fillHoldName {
			return
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	listening := make(chan struct{})
	var once sync.Once
	hub.OnConnected(func() { once.Do(func() { close(listening) }) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		testwait.Receive(t, "the standby's listener hub to return", done)
	})
	testwait.Receive(t, "the standby's listener hub to listen", listening)
	return wake
}

// bothFillsWhy is why the two-fill row's pauses are spans: the absence of a takeover is no state
// the standby leaves, its retry ticker runs on the wall clock out of synctest's reach, and no later
// action of the row shows a takeover that did not happen.
const bothFillsWhy = "a standby that does not take a live holder's hold leaves no state, its retry ticker runs on the wall clock out of synctest's reach, and no later action shows a takeover that did not happen"

// notServingWhy is why the not-serving row's pause is a span: a fill that writes nothing before
// Serving closes leaves no state to read, it runs on the wall clock with real Postgres I/O, out of
// synctest's reach, and the later action, Serving's close, is what lets it write.
const notServingWhy = "a fill that writes nothing before Serving closes leaves no state, it does real Postgres I/O out of synctest's reach, and the only later action is the close that lets it write"

// liveHolder is the holder duty_holds names live for the fill's hold, as text, or "" for none.
func liveHolder(t *testing.T, database *store.Store) string {
	t.Helper()
	var holder string
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select holder::text from duty_holds where name = $1 and expires_at > now()), '')
	`, fillHoldName).Scan(&holder); err != nil {
		t.Fatalf("read the fill's hold: %v", err)
	}
	return holder
}

// awaitLiveHolder waits, through the shared wait, until the fill's hold names a live holder, and
// answers it. A try that finds none reads what duty_holds holds for the hold (holdRow).
func awaitLiveHolder(t *testing.T, database *store.Store) string {
	t.Helper()
	var holder string
	testwait.Until(t, "a fill to take the hold", func() (string, bool) {
		holder = liveHolder(t, database)
		if holder != "" {
			return "the hold names " + holder, true
		}
		return holdRow(t, database), false
	})
	return holder
}

// holdRow says what duty_holds holds for the fill's hold: the holder its row names and how far
// the row's expiry stands from the database's now() (below zero once it has lapsed), or that the
// hold has no row.
func holdRow(t *testing.T, database *store.Store) string {
	t.Helper()
	var row string
	if err := database.Pool.QueryRow(t.Context(), `
		select coalesce((select format('duty_holds names holder %s, expiring %s after now()', holder, expires_at - now())
			from duty_holds where name = $1), format('duty_holds has no row for %s', $1::text))
	`, fillHoldName).Scan(&row); err != nil {
		t.Fatalf("read the fill's hold row: %v", err)
	}
	return row
}

// cursorHolders is the cursor row's holder as the test read it.
type cursorHolders struct {
	// read is the holders read, each change once, in order: a holder that came back is read twice.
	read []string
}

// sample reads the cursor row's holder and records it when it differs from the last one read. A
// missing row, or one that names no holder, records nothing.
func (h *cursorHolders) sample(t *testing.T, database *store.Store) {
	t.Helper()
	var holder string
	err := database.Pool.QueryRow(t.Context(),
		`select coalesce(holder::text, '') from search_text_versions_fill where id = 1`).Scan(&holder)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return
	case err != nil:
		t.Fatalf("read the cursor row's holder: %v", err)
	}
	if holder != "" && (len(h.read) == 0 || h.read[len(h.read)-1] != holder) {
		h.read = append(h.read, holder)
	}
}
