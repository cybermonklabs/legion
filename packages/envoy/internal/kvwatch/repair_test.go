package kvwatch_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// armed stands in for time.AfterFunc and records what the watcher asked of it: how long it wanted
// to wait for a repair, and every callback, in the order the watcher armed them, which the test
// fires itself. The production wait is a minute, so a test that slept for it could not run at
// all. Each callback is kept, not just the last, because a real one can still run after a later
// stop has armed its own: time.Timer.Stop does not wait for a callback already started. A test
// that counts or fires its waits turns the stream probe off (kvwatch.WithStreamProbeInterval(0)),
// whose waits it would otherwise record among them. Scan idle waits are kept apart (idles) and
// discarded-watch drains are not recorded: waits, asked and fireIt track only the other waits,
// and fireIdle fires the last idle wait.
type armed struct {
	mu     sync.Mutex
	delay  time.Duration
	delays []time.Duration
	fires  []func()
	idles  []func()
}

// afterFunc is the AfterFunc WithAfterFunc takes.
func (a *armed) afterFunc() kvwatch.AfterFunc {
	return func(d time.Duration, f func()) kvwatch.Timer {
		a.mu.Lock()
		defer a.mu.Unlock()
		if d == kvwatch.ScanIdleTimeout {
			a.idles = append(a.idles, f)
			return (*armedTimer)(a)
		}
		if d == kvwatch.AbandonedDrain {
			return (*armedTimer)(a)
		}
		a.delay = d
		a.delays = append(a.delays, d)
		a.fires = append(a.fires, f)
		return (*armedTimer)(a)
	}
}

// waits reports how many waits the watcher armed, excluding scan idle waits and discard bounds.
func (a *armed) waits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.fires)
}

// asked is the last delay the watcher asked for, excluding scan idle waits and discard bounds.
func (a *armed) asked() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.delay
}

// fireIdle runs the last scan idle wait's callback, and fails when none was armed.
func (a *armed) fireIdle(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	if len(a.idles) == 0 {
		a.mu.Unlock()
		t.Fatal("the watcher armed no scan idle wait")
	}
	fire := a.idles[len(a.idles)-1]
	a.mu.Unlock()
	fire()
}

// fireIt runs the last armed wait's callback, as the real timer would once its delay passed, and
// fails when nothing was armed.
func (a *armed) fireIt(t *testing.T) {
	t.Helper()
	a.fireAt(t, a.waits()-1)
}

// fireAt runs the callback of the wait the watcher armed i-th, counting from 0, and fails when it
// armed no such wait.
func (a *armed) fireAt(t *testing.T, i int) {
	t.Helper()
	a.mu.Lock()
	if i < 0 || i >= len(a.fires) {
		count := len(a.fires)
		a.mu.Unlock()
		t.Fatalf("the watcher armed %d waits, so no wait %d to fire", count, i)
	}
	fire := a.fires[i]
	a.mu.Unlock()
	fire()
}

// armedTimer is the Timer an armed hands back. Its Stop does nothing: the fake fires a wait only
// when the test asks, which is what lets a test fire one a repair has already stopped.
type armedTimer armed

func (t *armedTimer) Stop() bool { return true }

// matching is every record whose message and level are both these. A case is matched by the two
// together, so no other line the watcher writes - its warm-up, a failed first start, a recreated
// bucket - can satisfy it.
func matching(records []map[string]any, level, msg string) []map[string]any {
	var found []map[string]any
	for _, record := range records {
		if haveLevel, _ := record["level"].(string); haveLevel != level {
			continue
		}
		if haveMsg, _ := record["msg"].(string); haveMsg == msg {
			found = append(found, record)
		}
	}
	return found
}

const (
	stoppedWarn    = "test cache watcher stopped"
	unrepairedErr  = "test cache watcher was not repaired"
	testBucketName = "kvwatch-test"
)

// jetStream is the JetStream context of conn.
func jetStream(t *testing.T, conn *natsgo.Conn) natsgo.JetStreamContext {
	t.Helper()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return js
}

// ending lets a test end a real subscription without waiting for nats.go to notice a lost stream.
// Each step sends the cancel function of the subscription it starts.
type ending chan context.CancelFunc

func (e ending) step() watchStep {
	return func(ctx context.Context, kv jetstream.KeyValue) (jetstream.KeyWatcher, error) {
		ctx, cancel := context.WithCancel(ctx)
		watcher, err := kv.WatchAll(ctx)
		if err != nil {
			cancel()
			return nil, err
		}
		e <- cancel
		return watcher, nil
	}
}

// end cancels the context nats.go's subscription lives in. nats.go v1.55.0 (js.go:2072-2077)
// starts a goroutine that unsubscribes on ctx.Done; the watcher's closed handler closes updates.
func (e ending) end(t *testing.T) {
	t.Helper()
	testwait.Receive(t, "the subscription to start", e)()
}

// deleteTestBucket deletes the test bucket and the stream its ordered consumer reads. It does not
// close the subscription; a test that needs it to end does that separately.
func deleteTestBucket(t *testing.T, js natsgo.JetStreamContext) {
	t.Helper()
	if err := js.DeleteKeyValue(testBucketName); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
}

// waitForStopWarns waits for want WARN records from the watcher's own subscription-close path.
func waitForStopWarns(t *testing.T, records func() []map[string]any, want int) {
	t.Helper()
	testwait.Until(t, "the stopped watchers' WARNs", func() (int, bool) {
		got := len(matching(records(), "WARN", stoppedWarn))
		return got, got == want
	})
}

// A stop a Rewatch repairs a moment later is a WARN and nothing more. The repair disarms the wait,
// so even firing it afterwards writes no ERROR. Without that, every reconnect and every
// self-health rebuild left an ERROR an operator had to learn to ignore (INFRA-182).
func TestAWatcherRepairedAfterItsConsumerWentLogsOneWarn(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	into := newSeen()
	arm := &armed{}
	logger, records := jsonLogger(t)
	end := make(ending, 1)
	w := kvwatch.New("test cache", scripted(kv, end.step()).handle(), into,
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("the first scan: %v", err)
	}

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	end.end(t)
	waitForStopWarns(t, records, 1)

	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	if err := w.Rewatch(testwait.Context(t), conn); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	arm.fireIt(t)

	if warns := matching(records(), "WARN", stoppedWarn); len(warns) != 1 {
		t.Fatalf("WARN %q logged %d times, want once", stoppedWarn, len(warns))
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("a repaired watcher logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}
}

// A repair that fails is no report of its own: it is not the end of the attempt, because the bus
// arms another repair within seconds (internal/bus/recovery.go, recoverUntilHealthy), so one
// Rewatch that failed says only that the outage is still running. The wait the stop armed stays
// armed and makes the one report when its minute passes, and a second failed Rewatch adds no
// second ERROR.
func TestAWatcherWhoseBucketIsGoneReportsOnlyWhenItsWaitPasses(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	end := make(ending, 1)
	w := kvwatch.New("test cache", scripted(kv, end.step()).handle(), newSeen(),
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("the first scan: %v", err)
	}

	deleteTestBucket(t, jetStream(t, conn))
	end.end(t)
	waitForStopWarns(t, records, 1)

	if err := w.Rewatch(testwait.Context(t), conn); err == nil {
		t.Fatal("a rewatch onto a deleted bucket succeeded")
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("a failed repair logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}
	if got := arm.waits(); got != 1 {
		t.Fatalf("the stop armed %d repair waits after a failed repair, want one", got)
	}

	if err := w.Rewatch(testwait.Context(t), conn); err == nil {
		t.Fatal("a second rewatch onto a deleted bucket succeeded")
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("a second failed repair logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}

	arm.fireIt(t)
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("the passed wait logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
	}
	want := "no repair within " + time.Minute.String()
	if reason, _ := errs[0]["reason"].(string); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// A stop nothing repairs is the outage an operator has to see, so it becomes an ERROR once the
// minute the watcher waits for a repair has passed, and not before: an ERROR at the stop itself
// could not tell this apart from a stop a rebuild repairs a moment later (INFRA-182).
func TestAWatcherNothingRepairsLogsWhenItsMinutePasses(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	end := make(ending, 1)
	w := kvwatch.New("test cache", scripted(kv, end.step()).handle(), newSeen(),
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("the first scan: %v", err)
	}

	deleteTestBucket(t, jetStream(t, conn))
	end.end(t)
	waitForStopWarns(t, records, 1)

	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("the stop logged %d %q errors before its wait passed, want none: %v", len(errs), unrepairedErr, errs)
	}
	if got := arm.asked(); got != time.Minute {
		t.Fatalf("the watcher waited %v for a repair, want %v", got, time.Minute)
	}

	arm.fireIt(t)
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("the passed wait logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
	}
	want := "no repair within " + time.Minute.String()
	if reason, _ := errs[0]["reason"].(string); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// A wait reports only the stop that armed it. A repair clears the wait, but time.Timer.Stop does
// not wait for a callback already started, so the callback can still run once the replacement
// has stopped too and armed a wait of its own. Without knowing its stop, that late callback took
// the second stop's report: an ERROR saying no repair came within a minute of a stop moments
// old, and the second wait, finding the stop reported, then said nothing when its minute passed.
func TestAWaitARepairClearedReportsNotTheNextStop(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	end := make(ending, 1)
	w := kvwatch.New("test cache", scripted(kv, end.step()).handle(), newSeen(),
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	if err := w.WaitReady(testwait.Context(t)); err != nil {
		t.Fatalf("the first scan: %v", err)
	}

	js := jetStream(t, conn)
	deleteTestBucket(t, js)
	end.end(t)
	waitForStopWarns(t, records, 1)
	testnats.RecreateKeyValue(t, js, &natsgo.KeyValueConfig{Bucket: testBucketName})
	live := testnats.Connect(t, uri)
	if err := w.Rewatch(testwait.Context(t), live); err != nil {
		t.Fatalf("rewatch: %v", err)
	}
	// The replacement's read completes, its stream confirmed, while the bucket still exists: a
	// delete during that confirmation would make it arm a re-read of its own (rereadDelay), a wait
	// neither stop armed. WaitComplete would not do here: the copy stays servable through its
	// stale episode, so it can answer before the replacement's read has completed.
	testwait.Until(t, "the replacement's read to complete", func() (bool, bool) {
		following := w.Following()
		return following, following
	})
	deleteTestBucket(t, js)
	// Rewatch opens its own handle, so end the replacement's subscription by closing its connection.
	live.Close()
	waitForStopWarns(t, records, 2)
	if got := arm.waits(); got != 2 {
		arm.mu.Lock()
		delays := slices.Clone(arm.delays)
		arm.mu.Unlock()
		t.Fatalf("two stops armed %d waits (%v), want two repair waits", got, delays)
	}

	// The repair cleared the first wait; its callback runs now, as one already started would.
	arm.fireAt(t, 0)
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("the repaired stop's wait logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}

	arm.fireAt(t, 1)
	errs := matching(records(), "ERROR", unrepairedErr)
	if len(errs) != 1 {
		t.Fatalf("the second stop's passed wait logged %d %q errors, want one: %v", len(errs), unrepairedErr, errs)
	}
	want := "no repair within " + time.Minute.String()
	if reason, _ := errs[0]["reason"].(string); reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// A shutdown's own stop is neither a stop to report nor a repair to wait for: End retires the
// watcher first, so it writes no WARN, arms no wait and can leave no ERROR behind. The listener's
// shutdown test greps its whole output for ERROR across ten runs
// (cmd/listener/shutdown_test.go), so an arm left by an ordered stop would fail it.
func TestAWatcherStoppedOnPurposeLogsNeither(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	arm := &armed{}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen(),
		kvwatch.WithLogger(logger), kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithStreamProbeInterval(0))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	w.End(testContext(t))

	if warns := matching(records(), "WARN", stoppedWarn); len(warns) != 0 {
		t.Fatalf("an ordered stop logged %d %q warnings, want none: %v", len(warns), stoppedWarn, warns)
	}
	if errs := matching(records(), "ERROR", unrepairedErr); len(errs) != 0 {
		t.Fatalf("an ordered stop logged %d %q errors, want none: %v", len(errs), unrepairedErr, errs)
	}
	if arm.waits() != 0 {
		t.Fatalf("an ordered stop armed %d repair waits, want none", arm.waits())
	}
}
