package delivery

// The settings cache both intakes route by: read at start and when one of its wakes is signalled,
// never on a timer and never per message.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
	"github.com/sjawhar/envoy/internal/testnats"
)

// settingsWake runs a pglisten.Hub on pool's listener connection until the test ends, as the
// server does, and answers its wake on SettingsChannel once the hub has listened. The hub signals
// that wake after its LISTEN, so the channel already holds a signal when it is returned.
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
		<-done
	})
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the settings hub did not listen within 10s")
	}
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
		<-done
	})
}

// settingsReadCounter counts the statements that read the projects' delivery settings (the cache's
// own read, ListProjectSettings; its second statement reads the sources).
type settingsReadCounter struct{ reads atomic.Int64 }

func (c *settingsReadCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "from project_delivery_settings") {
		c.reads.Add(1)
	}
	return ctx
}

func (c *settingsReadCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// countedDeliveryPool is a pool of its own on pool's database whose reads of the projects'
// delivery settings are counted, on its lease and listener connections as well, which are opened
// from its configuration.
func countedDeliveryPool(t *testing.T, pool *store.Pool) (*store.Pool, *settingsReadCounter) {
	t.Helper()
	counter := &settingsReadCounter{}
	return storetest.Traced(t, pool, counter, nil), counter
}

// With both intakes running and bound, and nothing written, the projects' delivery settings are
// read twice in all -- at the cache's start and on the hub's signal after its LISTEN -- and then not
// once over three seconds: no timer reads them.
func TestTheIntakesReadNoSettingsWhileNothingChanges(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	counted, counter := countedDeliveryPool(t, pool)
	wake := settingsWake(t, counted)
	natsURL := testnats.URL(t)
	publisher := intakeTestClient(t)

	runCtx, cancel := context.WithCancel(ctx)
	stop, err := StartIntakes(runCtx, func(name string) (*bus.Client, error) {
		return bus.Connect([]string{natsURL}, bus.WithName(name))
	}, counted, deliveryTestIntakeLeases(t, counted), newFakeGitHub(t).newTestClient(), events.NewBroker(), wake, nil, nil, unrenamed())
	if err != nil {
		cancel()
		t.Fatalf("start the intakes: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stop()
	})
	awaitBoundDurable(t, publisher)
	waitFor(t, 20*time.Second, func() bool {
		info, err := publisher.JS().ConsumerInfo(bus.Stream, bus.DispatchJobDurable)
		return err == nil && info.PushBound
	}, func() string { return "the job intake never bound its durable" })
	waitFor(t, 5*time.Second, func() bool { return counter.reads.Load() >= 2 }, func() string {
		return fmt.Sprintf("the delivery settings were read %d times, want the start's read and the hub signal's", counter.reads.Load())
	})

	before := counter.reads.Load()
	time.Sleep(3 * time.Second)
	if read := counter.reads.Load() - before; read != 0 || before != 2 {
		t.Fatalf("the delivery settings were read %d times before the idle window and %d times over 3s of it; want 2 and none", before, read)
	}
}

// Ten job events of a tracked deploy run are checked against the cached rules: none of them reads
// the delivery settings, and they still schedule the run's read.
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
		t.Fatalf("10 job events read the delivery settings %d times, want none", read)
	}
	if armed := clock.armed(); armed != 1 {
		t.Fatalf("10 job events of a tracked run armed %d reads, want 1: they did not pass the settings check", armed)
	}
}

// A read that finds no delivery-on project holds no rule, so an intake does not bind (routes); the
// first settings saved are stored and close the channel Changed handed out before them; and a
// project whose delivery is turned off gives no rule again.
func TestTheSettingsCacheHoldsTheRulesOfDeliveryOnProjects(t *testing.T) {
	t.Parallel()
	pool, ctx := deliveryTestPool(t)
	cache := newSettingsCache(pool, nil, nil, nil)
	if cache.Load() != nil || cache.routes() {
		t.Fatalf("a cache that has read nothing holds %+v, want nothing", cache.Load())
	}
	if !cache.reload(ctx) || cache.Load() == nil || cache.routes() {
		t.Fatalf("a read with no delivery settings holds %+v, want rules with no repository", cache.Load().All())
	}

	changed := cache.Changed()
	seeded := seedDeliverySettings(t, ctx, pool)
	if !cache.reload(ctx) {
		t.Fatal("read the seeded settings")
	}
	select {
	case <-changed:
	default:
		t.Fatal("storing the first rules did not close the channel Changed handed out before them")
	}
	if held, ok := cache.Load().For(seeded.DeployRepo); !ok || held.DeployWorkflowPath != seeded.DeployWorkflowPath || !cache.routes() {
		t.Fatalf("held rule of %s = %+v (%t), want the seeded rule", seeded.DeployRepo, held, ok)
	}

	if _, err := pool.Exec(ctx, `update project_delivery_settings set delivery = false where project_key = $1`, deliveryProject); err != nil {
		t.Fatalf("turn the project's delivery off: %v", err)
	}
	if !cache.reload(ctx) {
		t.Fatal("read the settings with delivery off")
	}
	if cache.routes() {
		t.Fatalf("held rules with the project's delivery off = %+v, want none", cache.Load().All())
	}
}

// The cache reads every project's delivery settings again when a save announces itself on
// SettingsChannel, and when its projects wake (in the server, the commit of an event: a
// project.updated is how a write of a default repository shows) finds that a default repository a
// source reads has moved, since a source that names no repository moves with its project's
// default. A projects wake that finds no default moved reads the settings no more, and a move of a
// default alone announces nothing on SettingsChannel, so the save wake would not have caught it.
func TestTheCacheReloadsOnASaveAndOnADefaultRepositoryMove(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seeded := seedDeliverySettings(t, ctx, pool)
	counted, counter := countedDeliveryPool(t, pool)
	projectsWake := make(chan struct{}, 1)
	cache := newSettingsCache(counted, settingsWake(t, counted), projectsWake, tuningFor(t).log)
	runSettingsCache(t, cache)
	waitFor(t, 5*time.Second, func() bool {
		_, ok := cache.Load().For(deliveryRepo)
		return ok
	}, func() string {
		return fmt.Sprintf("the cache never held the rule of %s: %+v", deliveryRepo, cache.Load())
	})

	const release = ".github/workflows/release.yml"
	seeded.DeployWorkflowPath = release
	if _, err := putTestRule(ctx, pool, seeded, model.Actor{Kind: "system", ID: "delivery-test"}); err != nil {
		t.Fatalf("save a new deploy workflow path: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		rule, ok := cache.Load().For(deliveryRepo)
		return ok && rule.DeployWorkflowPath == release
	}, func() string {
		return fmt.Sprintf("the cache holds %+v two seconds after the save, want deploy workflow path %s", cache.Load().All(), release)
	})

	// The save's notifications are read by now; a projects wake with no default moved reads the
	// projects' defaults and not the settings.
	time.Sleep(500 * time.Millisecond)
	before := counter.reads.Load()
	projectsWake <- struct{}{}
	time.Sleep(500 * time.Millisecond)
	if read := counter.reads.Load() - before; read != 0 {
		t.Fatalf("a projects wake with no default repository moved read the delivery settings %d times, want none", read)
	}

	const moved = "acme/moved"
	if _, err := pool.Exec(ctx, `update projects set default_repo = $2 where key = $1`, deliveryProject, moved); err != nil {
		t.Fatalf("move the project's default repository: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, ok := cache.Load().For(moved); ok {
		t.Fatalf("the cache read the moved default repository with no projects wake: %+v", cache.Load().All())
	}
	projectsWake <- struct{}{}
	waitFor(t, 2*time.Second, func() bool {
		rule, ok := cache.Load().For(moved)
		_, stale := cache.Load().For(deliveryRepo)
		return ok && rule.DeployWorkflowPath == release && rule.Project == deliveryProject && !stale
	}, func() string {
		return fmt.Sprintf("the cache holds %+v two seconds after the projects wake, want the source's rule under %s alone", cache.Load().All(), moved)
	})
}
