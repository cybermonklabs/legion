package textversions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
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
// registers it). The standby's retry is one the test never runs (Config.retry), and its released
// wake is signalled by a notification naming the fill's hold alone (releasedWake), so the
// notification is the one thing that can make it take the hold: it takes the hold, and a pass
// writes the next event. Its events are woken only once it is seen to hold the hold, so a standby
// that needs an event's wake to take it fails. Each wait is on its fact, bounded by the test
// binary's deadline alone: a fixed span from the stop would include the pass's wait on the event
// horizon, which waits on the database's open event writes (textversionstest.AwaitRowsUntil), not
// on the standby.
func TestAStandbyTakesTheReleasedHoldAtOnce(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
	standbyStore := storetest.SecondProcess(t, database)
	released := releasedWake(t, standbyStore)
	second := startFillRetrying(t, standbyStore, released, make(chan time.Time), true)
	second.AwaitLine(t, standingByMessage)
	// The holder writes an event before it stops, so what it releases is a hold it fills under.
	written := commitComment(t, database, broker, textversionstest.Issue).id
	first.Wake()
	textversionstest.AwaitRowsUntil(t, database, []int64{written}, nil)

	first.Stop(t)
	stopped := time.Now()
	if !first.Logged(t, releasedHoldMessage) {
		t.Fatalf("the holder stopped without logging its release:\n%s", first.Log())
	}
	if live := liveHolder(t, database); live == firstHolder {
		t.Fatalf("the holder stopped and the hold still names it, %s", firstHolder)
	}
	id := commitComment(t, database, broker, textversionstest.Issue).id
	standby := testwait.Until(t, "the standby to take the released hold on the release's notification", func() (string, bool) {
		live := liveHolder(t, database)
		return live, live != ""
	})
	took := time.Since(stopped)
	// The standby took the hold, and may have run its first pass under it, before the event above
	// was written.
	second.Wake()
	textversionstest.AwaitRowsUntil(t, database, []int64{id}, nil)
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
// hold; the standby's first pass writes the next event. Its events are woken only once it is seen
// to hold the hold, so a standby whose retry needs an event's wake as well fails.
//
// Each wait is on its fact, bounded by the test binary's deadline alone: the hold coming free, the
// retry's take, and the row, which the pass writes only once the event horizon, which waits on the
// database's open event writes, has passed the event (textversionstest.AwaitRowsUntil). A fixed span
// from the holder's stop, the ticker's 10 s and 5 s more, read the instance's load as well as the
// standby (merge-group run 44963211024: `the events [1] had 0 rows within 14.996081355s; want 1`).
func TestAStandbyTakesAFreeHoldWithNoNotification(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
	never := make(chan struct{})
	retry := make(chan time.Time, 1)
	second := startFillRetrying(t, storetest.SecondProcess(t, database), never, retry, true)
	second.AwaitLine(t, standingByMessage)

	first.Stop(t)
	// The holder released its hold as it stopped, or, where its release did not reach the
	// database, the hold lapses holdOptions.TTL after its last renewal: either way the stopped
	// holder's hold ends, and nothing tells the standby.
	testwait.Until(t, "the stopped holder's hold to end", func() (string, bool) {
		live := liveHolder(t, database)
		return live, live != firstHolder
	})
	id := commitComment(t, database, broker, textversionstest.Issue).id
	// Nothing has told the standby that the hold is free, and nothing has woken its events, so it
	// has not taken it: only a retry, below, can.
	if live := liveHolder(t, database); live != "" || second.Logged(t, tookHoldMessage) {
		t.Fatalf("the standby took the hold before its retry, with no wake for the release: the hold names %q live:\n%s", live, second.Log())
	}

	// The test retries as the ticker does, again after a try that found no free hold or failed,
	// until a retry takes the hold.
	retried := time.Now()
	standby := testwait.UntilPolling(t, 100*time.Millisecond, "the standby's retry to take the free hold", func() (string, bool) {
		select {
		case retry <- time.Now():
		default:
		}
		live := liveHolder(t, database)
		return live, live != ""
	})
	took := time.Since(retried)
	// Its events are woken only now, so no event's wake can have brought the take.
	second.Wake()
	textversionstest.AwaitRowsUntil(t, database, []int64{id}, nil)
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

	time.Sleep(5 * time.Second)
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
	textversionstest.AwaitRowsUntil(t, database, ids, nil)
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
	awaitLiveHolder(t, database, 10*time.Second)
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
// ticker of standbyRetry.
func startFill(t *testing.T, db *store.Store, released <-chan struct{}, serving bool) *fillProcess {
	t.Helper()
	return startFillRetrying(t, db, released, nil, serving)
}

// startFillRetrying is startFill with a standby that retries each time retry receives, the test
// sending it where the ticker of standbyRetry would tick (Config.retry); nil is the ticker.
func startFillRetrying(t *testing.T, db *store.Store, released <-chan struct{}, retry <-chan time.Time, serving bool) *fillProcess {
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
		<-done
	})
	select {
	case <-listening:
	case <-time.After(10 * time.Second):
		t.Fatal("the standby's listener hub did not listen within 10 s")
	}
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

// awaitLiveHolder waits until the fill's hold names a live holder, and answers it.
func awaitLiveHolder(t *testing.T, database *store.Store, within time.Duration) string {
	t.Helper()
	var holder string
	testwait.WithinPolling(t, within, 20*time.Millisecond, fmt.Sprintf("a fill to take the hold within %v", within), func() bool {
		holder = liveHolder(t, database)
		return holder != ""
	})
	return holder
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
