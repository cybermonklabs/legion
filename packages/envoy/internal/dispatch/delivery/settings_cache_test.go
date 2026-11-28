package delivery

// The settings cache both intakes route by: read at start and when its wake is signalled, never
// on a timer and never per message.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// settingsWake runs a pglisten.Hub on pool's listener connection until the test ends, as the
// server does, and answers its wake on SettingsChannel once the hub has listened. The hub signals
// that wake after its LISTEN, just after it runs the callback this wait ends on, so a signal is
// on its way when the wake is returned.
func settingsWake(t *testing.T, pool *store.Pool) <-chan struct{} {
	t.Helper()
	hub := &pglisten.Hub{Owner: "delivery settings test", Dial: pool.Listener}
	wake := hub.Wake(SettingsChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		testwait.Receive(t, "the settings hub's Run to return once its context ended", done)
	})
	testwait.Receive(t, "the settings hub to listen", connected)
	return wake
}

// runSettingsCache runs cache until the test ends, and waits for Run to return before the test's
// own cleanups go on.
func runSettingsCache(t *testing.T, cache *SettingsCache) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.Run(ctx)
	}()
	t.Cleanup(func() {
		stop()
		testwait.Receive(t, "the settings cache's Run to return once its context ended", done)
	})
}

// settingsReadCounter counts the statements that read delivery_settings.
type settingsReadCounter struct{ reads atomic.Int64 }

func (c *settingsReadCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "from delivery_settings") {
		c.reads.Add(1)
	}
	return ctx
}

func (c *settingsReadCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedDeliveryPool is a pool of its own on pool's database whose delivery_settings reads are
// counted, on its lease and listener connections as well, which are opened from its configuration.
func countedDeliveryPool(t *testing.T, pool *store.Pool) (*store.Pool, *settingsReadCounter) {
	t.Helper()
	counter := &settingsReadCounter{}
	return storetest.Traced(t, pool, counter, nil), counter
}

// With both intakes running and bound, and nothing written, delivery_settings is read twice in
// all -- at the cache's start and on the hub's signal after its LISTEN -- and then the cache waits
// for its wake with no retry armed and no signal pending: nothing but a write's notification, or
// the hub's next LISTEN, can make it read again, and no timer reads it.
func TestTheIntakesReadNoSettingsWhileNothingChanges(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	counted, counter := countedDeliveryPool(t, pool)
	wake := settingsWake(t, counted)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	// settingsWait is one block of the cache in its wait (SettingsCache.Run, the settingsWaiting
	// hook): whether a retry was armed there, the reads made by then, and the wake's signals
	// already pending, each read as it blocked.
	type settingsWait struct {
		retrying bool
		reads    int64
		pending  int
	}
	var waitsMu sync.Mutex
	var waits []settingsWait
	waiting := func(retrying bool) {
		waitsMu.Lock()
		defer waitsMu.Unlock()
		waits = append(waits, settingsWait{retrying: retrying, reads: counter.reads.Load(), pending: len(wake)})
	}
	tuning := tuningFor(t)
	tuning.settingsWaiting.Store(&waiting)

	runCtx, cancel := context.WithCancel(ctx)
	// startIntakes rather than StartIntakes, whose cache runs production's tuning, which has no
	// hook: this test's tuning holds production's values (tuningFor) and the hook above, and the
	// intakes run on the real clock, as StartIntakes runs them.
	stop, err := startIntakes(runCtx, tuning, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, counted, deliveryTestIntakeLeases(t, counted), newFakeGitHub(t).newTestClient(), events.NewBroker(), wake, nil, unrenamed(), clock.Real())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitBoundDurable(t, publisher)
	waitFor(t, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchJobDurable)
		return err == nil && info.PushBound
	}, func() string { return "the job intake never bound its durable" })
	waitFor(t, func() bool { return counter.reads.Load() >= 2 }, func() string {
		return fmt.Sprintf("delivery_settings was read %d times, want the start's read and the hub signal's", counter.reads.Load())
	})

	// The state that would make a read happen: a retry armed in the cache's wait, or a signal
	// waiting on its wake. Once the cache blocks with neither, and has read nothing since it
	// blocked, only a notification on SettingsChannel, which nothing here writes, or a LISTEN of
	// the hub's on a new connection can end its wait, and the cache is the intakes' one reader of
	// delivery_settings.
	idle := testwait.Until(t, "the settings cache to block in its wait with no retry armed and no wake pending", func() (settingsWait, bool) {
		waitsMu.Lock()
		defer waitsMu.Unlock()
		if len(waits) == 0 {
			return settingsWait{}, false
		}
		last := waits[len(waits)-1]
		return last, !last.retrying && last.pending == 0 && last.reads == counter.reads.Load()
	})
	if read := counter.reads.Load(); idle.reads != 2 || read != 2 {
		t.Fatalf("delivery_settings read %d times as the cache went idle and %d times by now; want 2 and 2", idle.reads, read)
	}
}

// Ten job events of a tracked deploy run are checked against the cached settings: none of them
// reads delivery_settings, and they still schedule the run's read.
func TestJobEventsReadNoSettings(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedRunningDeployRun(t, ctx, pool)
	counted, counter := countedDeliveryPool(t, pool)
	clock := newFakeJobClock()
	jobs := newJobIntake(tuningFor(t), nil, counted, nil, newJobsFake(t).newTestClient(), clock, loadedSettingsCache(t, ctx, counted))

	before := counter.reads.Load()
	for range 10 {
		jobs.receive(ctx, jobPayload("acme/widgets", 500))
	}
	if read := counter.reads.Load() - before; read != 0 {
		t.Fatalf("10 job events read delivery_settings %d times, want none", read)
	}
	if armed := len(clock.Armed()); armed != 1 {
		t.Fatalf("10 job events of a tracked run armed %d reads, want 1: they did not pass the settings check", armed)
	}
}

// A read that finds no row leaves the cache empty; the first settings written are stored; a later
// read whose deploy repository is malformed keeps the settings before it.
func TestTheSettingsCacheKeepsTheLastValidSettings(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	cache := newSettingsCache(pool, nil, nil)
	if !cache.reload(ctx) || cache.Load() != nil {
		t.Fatalf("a read with no row = %+v, want nothing held", cache.Load())
	}

	seeded := seedDeliverySettings(t, ctx, pool)
	if !cache.reload(ctx) {
		t.Fatal("read the seeded settings")
	}
	if held := cache.Load(); held == nil || held.DeployRepo != seeded.DeployRepo {
		t.Fatalf("held settings = %+v, want deploy repository %s", held, seeded.DeployRepo)
	}

	if _, err := pool.Exec(ctx, `update delivery_settings set deploy_repo = 'no-slash' where singleton`); err != nil {
		t.Fatalf("write a malformed deploy repository: %v", err)
	}
	if !cache.reload(ctx) {
		t.Fatal("read the malformed settings")
	}
	if held := cache.Load(); held == nil || held.DeployRepo != seeded.DeployRepo {
		t.Fatalf("held settings after a malformed deploy repository = %+v, want %s kept", held, seeded.DeployRepo)
	}
}

// SettingsCache.Read's channel is closed by the first read that answers, whatever it found, and by
// no read that failed: a read on a cancelled context fails and leaves it open; a read that finds a
// deploy repository it refuses answers, holds nothing, and closes it; and a first read that finds
// no row closes it.
func TestTheSettingsCachesFirstReadIsTheFirstReadThatAnswers(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	closed := func(cache *SettingsCache) bool {
		select {
		case <-cache.Read():
			return true
		default:
			return false
		}
	}

	cache := newSettingsCache(pool, nil, nil)
	if closed(cache) {
		t.Fatal("a settings cache that has read nothing has its first read closed")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if cache.reload(cancelled) {
		t.Fatal("control: a read on a cancelled context answered")
	}
	if closed(cache) {
		t.Fatal("a read that failed closed the settings cache's first read")
	}

	seedDeliverySettings(t, ctx, pool)
	if _, err := pool.Exec(ctx, `update delivery_settings set deploy_repo = 'no-slash' where singleton`); err != nil {
		t.Fatalf("write a malformed deploy repository: %v", err)
	}
	if !cache.reload(ctx) {
		t.Fatal("read the malformed settings")
	}
	if held := cache.Load(); held != nil {
		t.Fatalf("held settings after a refused deploy repository = %+v, want nothing", held)
	}
	if !closed(cache) {
		t.Fatal("a read that refused the deploy repository left the settings cache's first read open")
	}

	if _, err := pool.Exec(ctx, `delete from delivery_settings`); err != nil {
		t.Fatalf("delete the delivery settings: %v", err)
	}
	empty := newSettingsCache(pool, nil, nil)
	if !empty.reload(ctx) || !closed(empty) {
		t.Fatal("a first read that found no row left the settings cache's first read open")
	}
}
