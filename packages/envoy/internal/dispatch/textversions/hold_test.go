package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/sessionanswers"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/textversions/textversionstest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds runs the fill as two serving
// processes run it, each on a store of its own, and writes events while both are woken by each:
// the hold and the cursor row name the first process alone, which writes every event's rows,
// while the second stands by. Then the first dies with its hold held: its pool closes, so no
// renewal and no release of its reaches the database, and its context ends. The second takes the
// hold once it lapses, within 60 s of the death, and writes the next events, and the cursor row's
// holder, once it names the second, never names the first again. The 60 s bounds the take-over
// alone, the hold's lapse and the standby's retry; the rows wait on the event horizon, bounded by
// the test binary's deadline (textversionstest.AwaitRowsUntil).
func TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database)
	second := startFill(t, storetest.SecondProcess(t, database), nil, true)
	second.AwaitLine(t, standingByMessage)

	var holders cursorHolders
	var ids []int64
	// The comments are written 300 ms apart while both fills run, and the hold is read at each: the
	// loop writes on a tick rather than waiting for something to hold, so it keeps its own sleep.
	for range 10 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
		first.Wake()
		second.Wake()
		holders.sample(t, database)
		if live := liveHolder(t, database); live != firstHolder {
			t.Fatalf("while both fills run, the hold names %q; want the first's, %s", live, firstHolder)
		}
		time.Sleep(300 * time.Millisecond)
	}
	textversionstest.AwaitRowsUntil(t, database, ids, func() { holders.sample(t, database) })
	if got := holders.read; len(got) != 1 || got[0] != firstHolder {
		t.Fatalf("while both fills ran the cursor row named %v; want the first's holder %s alone", got, firstHolder)
	}
	if second.Logged(t, batchMessage) {
		t.Fatalf("the standby wrote a batch while the first held the hold:\n%s", second.Log())
	}

	died := time.Now()
	first.db.Pool.Close()
	first.Stop(t)
	var next []int64
	for range 3 {
		next = append(next, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	second.Wake()
	testwait.WithinPolling(t, time.Minute-time.Since(died), 50*time.Millisecond, "the standby to take the dead holder's hold within 60 s of the death", func() bool {
		holders.sample(t, database)
		second.Wake()
		live := liveHolder(t, database)
		return live != "" && live != firstHolder
	})
	t.Logf("the standby took the hold %v after the holder died", time.Since(died).Round(time.Millisecond))
	textversionstest.AwaitRowsUntil(t, database, next, func() {
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
}

// TestAStandbyTakesTheReleasedHoldAtOnce stops the holder as a serving process stops, by its
// context: the batch in flight commits or rolls back and the hold is released, which notifies
// the standby through its listener (duty.HoldReleasedChannel on a pglisten.Hub, as the server
// registers it). The standby runs on a fake clock the test never moves (Config.clock), so its
// ticker of standbyRetry never ticks, and its released wake is signalled by a notification naming
// the fill's hold alone (releasedWake), so the notification is the one thing that can make it take
// the hold: it takes the hold, and a pass writes the next event. Its events are woken only once it
// is seen to hold the hold, so a standby that needs an event's wake to take it fails. Each wait is
// on its fact, bounded by the test binary's deadline alone: a fixed span from the stop would
// include the pass's wait on the event horizon, which waits on the database's open event writes
// (textversionstest.AwaitRows), not on the standby.
func TestAStandbyTakesTheReleasedHoldAtOnce(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database)
	standbyStore := storetest.SecondProcess(t, database)
	released := releasedWake(t, standbyStore)
	standbyClock := clocktest.New(fakeStart)
	second := startFillWith(t, standbyStore, released, true, func(config *Config) { config.clock = standbyClock })
	second.AwaitLine(t, standingByMessage)
	// The holder writes an event before it stops, so what it releases is a hold it fills under.
	written := commitComment(t, database, broker, textversionstest.Issue).id
	first.Wake()
	textversionstest.AwaitRows(t, database, []int64{written}, nil)

	first.Stop(t)
	if !first.Logged(t, releasedHoldMessage) {
		t.Fatalf("the holder stopped without logging its release:\n%s", first.Log())
	}
	if live := liveHolder(t, database); live == firstHolder {
		t.Fatalf("the holder stopped and the hold still names it, %s", firstHolder)
	}
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing wakes the standby's events before its take, and its clock stands still: only the
	// release's notification can bring the take.
	var standby string
	testwait.Until(t, "the standby to take the released hold on the release's notification", func() (string, bool) {
		if standby = liveHolder(t, database); standby != "" {
			return "the hold names " + standby, true
		}
		return holdRow(t, database), false
	})
	// The standby took the hold, and may have run its first pass under it, before the event above
	// was written.
	second.Wake()
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestAStandbyTakesAFreeHoldWithNoNotification gives the standby a released wake that is never
// signalled, as a standby whose listener is down has: the holder releases its hold and nothing
// tells the standby, which leaves the hold free until its next retry. The standby's ticker of
// standbyRetry runs on a fake clock (Config.clock), which the test moves to each tick once the
// ticker has armed it, as standbyRetry passing does, until a try at a tick takes the free hold.
// Nothing wakes the standby's events, so a standby that took the hold only once an event woke it
// would not pass; its first pass under the hold writes what was written while it stood by.
//
// Each wait is on its fact, bounded by the test binary's deadline alone: the hold coming free, the
// tick's try, and the row, which the pass writes only once the event horizon, which waits on the
// database's open event writes, has passed the event (textversionstest.AwaitRows). A fixed span
// from the holder's stop, the ticker's 10 s and 5 s more, read the instance's load as well as the
// standby (merge-group run 44963211024: `the events [1] had 0 rows within 14.996081355s; want 1`).
func TestAStandbyTakesAFreeHoldWithNoNotification(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database)
	standbyClock := clocktest.New(fakeStart)
	second := startFillWith(t, storetest.SecondProcess(t, database), make(chan struct{}), true, func(config *Config) { config.clock = standbyClock })
	second.AwaitLine(t, standingByMessage)
	tick := fakeStart.Add(standbyRetry)
	standbyClock.AwaitArmedAt(t, "the standby's first tick, standbyRetry after it stood by", tick)

	first.Stop(t)
	// The holder released its hold as it stopped, or, where its release did not reach the
	// database, the hold lapses holdOptions.TTL after its last renewal: either way the stopped
	// holder's hold ends, and nothing tells the standby.
	testwait.Until(t, "the stopped holder's hold to end", func() (string, bool) {
		live := liveHolder(t, database)
		return live, live != firstHolder
	})
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing has told the standby that the hold is free, nothing wakes its events, and its clock
	// has not reached its tick, so it has not taken it: only a try at a tick, below, can.
	if live := liveHolder(t, database); live != "" || second.Logged(t, tookHoldMessage) {
		t.Fatalf("the standby took the hold before its tick, with no wake for the release: the hold names %q live:\n%s", live, second.Log())
	}

	// A try at a tick that found no free hold, or failed, arms the next tick standbyRetry later.
	var standby string
	for standby == "" {
		standbyClock.Set(tick)
		next := tick.Add(standbyRetry)
		testwait.Until(t, fmt.Sprintf("the standby's try at its tick %s to take the free hold, or to arm its next tick", tick.Format(time.RFC3339)),
			func() (string, bool) {
				if standby = liveHolder(t, database); standby != "" {
					return "the hold names " + standby, true
				}
				armed := standbyClock.Armed()
				return fmt.Sprintf("%s; the standby's clock holds %v", holdRow(t, database), armed), slices.Contains(armed, next)
			})
		tick = next
	}
	textversionstest.AwaitRows(t, database, []int64{id}, nil)
	if standby == firstHolder {
		t.Fatalf("the hold names the holder that released it, %s; want the standby's", firstHolder)
	}
	if got := readCursorRow(t, database).holder; got != standby {
		t.Fatalf("the cursor row names holder %q; want the standby's, %s, which holds the hold", got, standby)
	}
}

// TestTheFillWritesNothingUntilTheServerListens holds Serving open with events written and the
// fill woken: Run's goroutine waits at its gate on Serving (Config.awaitingServing), and the fill
// has written no row, no cursor row and no hold, then and after a span of 5 s (notServingWhy).
// Once Serving closes, as it does once the server listens and is admitted to the registry of live
// server processes, the rows come.
func TestTheFillWritesNothingUntilTheServerListens(t *testing.T) {
	// Not parallel: it holds a span of 5 s of real time with Serving open (notServingWhy), in which
	// a write the parallel tests' load delayed past the span would pass.
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	atGate := make(chan struct{})
	fill := startFillWith(t, storetest.SecondProcess(t, database), nil, false, func(config *Config) {
		config.awaitingServing = func() { close(atGate) }
	})
	var ids []int64
	for range 3 {
		ids = append(ids, commitComment(t, database, broker, textversionstest.Issue).id)
	}
	fill.Wake()

	testwait.Receive(t, "the fill's Run to reach its gate on Serving", atGate)
	requireNothingWritten(t, database, "at the fill's gate on Serving")
	testwait.Span(t, 5*time.Second, notServingWhy)
	requireNothingWritten(t, database, "5 s after the fill reached its gate on Serving")

	close(fill.serving)
	textversionstest.AwaitRows(t, database, ids, nil)
}

// notServingWhy is why TestTheFillWritesNothingUntilTheServerListens holds a span with Serving
// open, after the state that proves what it can.
const notServingWhy = "the gate hook and the reads show Run's one goroutine waiting on Serving with nothing written, " +
	"but no state reports, no clock of the test drives and no later action is ordered against a write from a goroutine " +
	"Run starts before its gate, and synctest.Wait cannot hold the fill's real Postgres connections; " +
	"5 s is main's window, so a write within it fails here as it failed there"

// requireNothingWritten holds the fill to no keyword row, no cursor row and no hold, when names
// the moment.
func requireNothingWritten(t *testing.T, database *store.Store, when string) {
	t.Helper()
	var rows, cursors, holds int
	if err := database.Pool.QueryRow(t.Context(), `
		select (select count(*) from search_text_versions), (select count(*) from search_text_versions_fill),
		       (select count(*) from duty_holds where name = $1)
	`, fillHoldName).Scan(&rows, &cursors, &holds); err != nil {
		t.Fatalf("read the keyword rows, the cursor row and the hold: %v", err)
	}
	if rows != 0 || cursors != 0 || holds != 0 {
		t.Fatalf("%s, before the process serves, the fill wrote %d rows, %d cursor rows and %d holds; want none", when, rows, cursors, holds)
	}
}

// TestACaughtUpFillSendsNoStatement runs the fill on a store whose every statement a tracer sees,
// the hold's renewals counted apart from the rest. Once the fill has written the events and gone
// quiet, it sends no statement for 10 s with nothing written but its hold's renewal, which goes
// on.
func TestACaughtUpFillSendsNoStatement(t *testing.T) {
	// Not parallel: it holds quiet windows of real time, 9 s and then 10 s with no statement
	// (storetest.AwaitQuiet, storetest.RequireNoStatements), which the parallel tests' load could
	// make it miss, or pass with a statement it delayed past them.
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
	textversionstest.AwaitRowsUntil(t, database, ids, nil)

	// Longer than the horizon's longest re-read (rereadMax), so a fill still reading it again is
	// not taken for one at rest.
	storetest.AwaitQuiet(t, &tracer.others, rereadMax+time.Second, time.Minute)
	renewed := tracer.renewals.Load()
	storetest.RequireNoStatements(t, &tracer.others, 10*time.Second, "a fill caught up with nothing written")
	if tracer.renewals.Load() == renewed {
		t.Errorf("the hold was not renewed over 10 s; want its renewal every %v", holdOptions.Renew)
	}
}

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
// ticker of standbyRetry, on the wall clock.
func startFill(t *testing.T, db *store.Store, released <-chan struct{}, serving bool) *fillProcess {
	t.Helper()
	return startFillWith(t, db, released, serving, func(*Config) {})
}

// startFillWith is startFill with configure's change to the Config the fill runs with: the clock
// its standby's ticker and its waits run on (Config.clock), or a hook that reports its state to the
// test.
func startFillWith(t *testing.T, db *store.Store, released <-chan struct{}, serving bool, configure func(*Config)) *fillProcess {
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
	}
	configure(&config)
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
// answers it. A read that finds none reads what duty_holds holds for the hold (holdRow).
func awaitLiveHolder(t *testing.T, database *store.Store) string {
	t.Helper()
	var holder string
	testwait.Until(t, "a fill to take the hold", func() (string, bool) {
		if holder = liveHolder(t, database); holder != "" {
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
