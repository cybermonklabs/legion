package textversions

import (
	"context"
	"errors"
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
)

// TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds runs the fill as two serving
// processes run it, each on a store of its own, and writes events while both are woken by each:
// the hold and the cursor row name the first process alone, which writes every event's rows,
// while the second stands by. Then the first dies with its hold held: its pool closes, so no
// renewal and no release of its reaches the database, and its context ends. The second takes the
// hold once it lapses and writes the next events within 60 s of the death, and the cursor row's
// holder, once it names the second, never names the first again.
func TestOneFillHoldsTheLeaseAndAnotherTakesOverWithinSixtySeconds(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
	second := startFill(t, storetest.SecondProcess(t, database), nil, true)
	second.AwaitLine(t, standingByMessage, 10*time.Second)

	var holders cursorHolders
	var ids []int64
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
	textversionstest.AwaitRows(t, database, ids, 30*time.Second, func() { holders.sample(t, database) })
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
	textversionstest.AwaitRows(t, database, next, time.Minute-time.Since(died), func() {
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
// registers it), so the standby takes the hold and writes the next event within 2 s, far inside
// its 10 s retry.
func TestAStandbyTakesTheReleasedHoldAtOnce(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
	standbyStore := storetest.SecondProcess(t, database)
	released := releasedWake(t, standbyStore)
	second := startFill(t, standbyStore, released, true)
	second.AwaitLine(t, standingByMessage, 10*time.Second)
	// The holder writes an event before it stops, so what it releases is a hold it fills under.
	written := commitComment(t, database, broker, textversionstest.Issue).id
	first.Wake()
	textversionstest.AwaitRows(t, database, []int64{written}, 30*time.Second, nil)

	first.Stop(t)
	stopped := time.Now()
	if !first.Logged(t, releasedHoldMessage) {
		t.Fatalf("the holder stopped without logging its release:\n%s", first.Log())
	}
	if live := liveHolder(t, database); live == firstHolder {
		t.Fatalf("the holder stopped and the hold still names it, %s", firstHolder)
	}
	id := commitComment(t, database, broker, textversionstest.Issue).id
	second.Wake()
	textversionstest.AwaitRows(t, database, []int64{id}, 2*time.Second-time.Since(stopped), nil)
	t.Logf("the standby wrote the next event %v after the holder stopped", time.Since(stopped).Round(time.Millisecond))
	if got := readCursorRow(t, database).holder; got == firstHolder || got == "" {
		t.Fatalf("the cursor row names holder %q; want the standby's", got)
	}
}

// TestAStandbyTakesAFreeHoldWithNoNotification gives the standby a released wake that is never
// signalled, as a standby whose listener is down has: the holder releases its hold and nothing
// tells the standby, whose retry takes the free hold within its 10 s and writes the next event.
func TestAStandbyTakesAFreeHoldWithNoNotification(t *testing.T) {
	database := storetest.Open(t)
	textversionstest.SeedIssue(t, database)
	broker := events.NewBroker()
	first := startFill(t, storetest.SecondProcess(t, database), nil, true)
	firstHolder := awaitLiveHolder(t, database, 10*time.Second)
	never := make(chan struct{})
	second := startFill(t, storetest.SecondProcess(t, database), never, true)
	second.AwaitLine(t, standingByMessage, 10*time.Second)

	first.Stop(t)
	stopped := time.Now()
	id := commitComment(t, database, broker, textversionstest.Issue).id
	second.Wake()
	textversionstest.AwaitRows(t, database, []int64{id}, standbyRetry+5*time.Second-time.Since(stopped), nil)
	t.Logf("the standby wrote the next event %v after the holder released its hold", time.Since(stopped).Round(time.Millisecond))
	if live := liveHolder(t, database); live == "" || live == firstHolder {
		t.Fatalf("the hold names %q; want the standby's holder", live)
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
	textversionstest.AwaitRows(t, database, ids, 30*time.Second, nil)
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
	textversionstest.AwaitRows(t, database, ids, 30*time.Second, nil)

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
// already when serving is set, and stops it once the test ends.
func startFill(t *testing.T, db *store.Store, released <-chan struct{}, serving bool) *fillProcess {
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
	process.Fill = textversionstest.StartFill(t, func(ctx context.Context, wake <-chan struct{}, logger *slog.Logger) error {
		config.Events, config.Logger = wake, logger
		return Run(ctx, config)
	})
	return process
}

// releasedWake runs a listener hub on db's listener pool, as the server runs its own, with a wake
// on duty.HoldReleasedChannel, and answers that wake once the hub listens.
func releasedWake(t *testing.T, db *store.Store) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "textversions test", Dial: db.Pool.Listener}
	wake := hub.Wake(duty.HoldReleasedChannel)
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
	deadline := time.Now().Add(within)
	for {
		if holder := liveHolder(t, database); holder != "" {
			return holder
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fill took the hold within %v", within)
		}
		time.Sleep(20 * time.Millisecond)
	}
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
