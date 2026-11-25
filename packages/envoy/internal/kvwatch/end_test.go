package kvwatch_test

import (
	"context"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// warnings is every WARN or ERROR record, each as its level, message and error.
func warnings(records []map[string]any) []string {
	var found []string
	for _, record := range records {
		level, _ := record["level"].(string)
		if level != "WARN" && level != "ERROR" {
			continue
		}
		message, _ := record["msg"].(string)
		errText, _ := record["error"].(string)
		found = append(found, level+" "+message+": "+errText)
	}
	return found
}

// testConsumers lists the test bucket's consumers.
func testConsumers(t *testing.T, conn *natsgo.Conn) []string {
	t.Helper()
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	var names []string
	for name := range js.ConsumerNames("KV_kvwatch-test") {
		names = append(names, name)
	}
	return names
}

// A watch still starting when the shutdown stops the watcher arms a watcher after the stop. End
// waits for that watch and ends its watcher too, deleting its consumer, so nothing is left for the
// connection's drain to delete.
func TestEndEndsAWatcherArmedAfterTheStop(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	held := holdWatch(kv)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", held.handle(), newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	armedWatch(t, held, "the watch to arm its watcher")
	w.Stop()
	ended := make(chan struct{})
	go func() {
		// Under the test's own context, which has no deadline: End's return is the fact the row
		// waits for, and only the wait below ends that wait.
		w.End(t.Context())
		close(ended)
	}()
	close(held.release)
	testwait.Receive(t, "End to return once the watch it waited for armed and ended its watcher", ended)
	if consumers := testConsumers(t, conn); len(consumers) != 0 {
		t.Fatalf("consumers after End = %v, want none", consumers)
	}
	if got := warnings(records()); len(got) != 0 {
		t.Fatalf("End logged %q, want nothing", got)
	}
}

// A watch still starting when End's deadline passes is an ERROR, since End cannot say its watcher
// was ended within it. The watch ends its watcher itself once it arms it, so its consumer is still
// deleted.
func TestAWatchStillStartingAtEndsDeadlineIsAnError(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	held := holdWatch(kv)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", held.handle(), newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	armedWatch(t, held, "the watch to arm its watcher")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.End(ctx)
	close(held.release)
	eventually(t, "the late watch to delete its consumer", func() bool { return len(testConsumers(t, conn)) == 0 })
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end found a watch still starting: context deadline exceeded"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}

// failingStopKV is heldWatchKV whose watcher's Stop stops the real watcher, so the test leaves no
// consumer behind, and then answers stopErr: nats.go's ErrConnectionDraining, as its Unsubscribe
// answers once the connection's drain has begun (nats.go v1.55.0, nats.go:5492-5494), or its
// ErrDisconnected, as a consumer delete answers when a token renewal's ForceReconnect cuts the
// request (nats.go v1.55.0, nats.go:2667, context.go:68-74).
type failingStopKV struct {
	*heldWatchKV
	stopErr error
}

func (k failingStopKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.heldWatchKV.Watch(ctx, keys, opts...)
	if err != nil {
		return watcher, err
	}
	return failingStop{KeyWatcher: watcher, err: k.stopErr}, nil
}

// handle is k as the bus.KeyValue a watcher takes.
func (k failingStopKV) handle() bus.KeyValue { return bus.NewKeyValue(k.legacy, k, k.streams) }

type failingStop struct {
	jetstream.KeyWatcher
	err error
}

func (w failingStop) Stop() error {
	_ = w.KeyWatcher.Stop()
	return w.err
}

// A watch still starting when End gives up on it because the connection was lost arms its watcher
// after End and stops it itself. That stop runs on a connection the drain has gone on without,
// which nats.go refuses as "connection draining" once the connection's drain begins: the consumer
// is left to the server, as End's own are, so the stop is a warning, never an error.
func TestALateWatchAfterALostConnectionIsAWarning(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	held := failingStopKV{heldWatchKV: holdWatch(kv), stopErr: natsgo.ErrConnectionDraining}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", held.handle(), newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	armedWatch(t, held.heldWatchKV, "the watch to arm its watcher")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(bus.ErrConnectionLost)
	w.End(ctx)
	close(held.release)
	eventually(t, "the late watch's stop to be logged", func() bool { return len(warnings(records())) == 2 })
	lost := "WARN test cache watcher end found a watch still starting: " + bus.ErrConnectionLost.Error()
	late := "WARN test cache watcher's consumer was left to the server: the connection was lost before its delete: " +
		bus.ErrConnectionLost.Error() + ": " + natsgo.ErrConnectionDraining.Error()
	if got, want := strings.Join(warnings(records()), ", "), lost+", "+late; got != want {
		t.Fatalf("End and the late watch logged %q, want %q", got, want)
	}
}

// A DeleteConsumer answered ErrDisconnected during a drain (a token renewal's ForceReconnect cut
// the request) is a lost connection, logged at WARN: the server deletes a consumer whose interest
// is gone on its own. Without ErrDisconnected in lostConnection, it falls to ERROR.
func TestADeleteConsumerAnsweredErrDisconnectedDuringEndIsAWarning(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	held := failingStopKV{heldWatchKV: holdWatch(kv), stopErr: natsgo.ErrDisconnected}
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", held.handle(), newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	armedWatch(t, held.heldWatchKV, "the watch to arm its watcher")
	close(held.release)
	eventually(t, "the first read to follow", w.Following)
	w.End(context.Background())
	got := warnings(records())
	for _, line := range got {
		if strings.HasPrefix(line, "ERROR") {
			t.Fatalf("End logged an error for a disconnected delete: %s", line)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "WARN") || !strings.Contains(got[0], natsgo.ErrDisconnected.Error()) {
		t.Fatalf("End logged %v, want one WARN naming ErrDisconnected", got)
	}
}

// endsWaiting counts the goroutines parked inside End: waiting for a watch, or for another End.
func endsWaiting() int {
	n := 0
	for _, goroutine := range goroutinetest.Traces() {
		header, _, _ := strings.Cut(goroutine, "\n")
		parked := strings.Contains(header, "[select") || strings.Contains(header, "[sync.Mutex.Lock")
		if parked && strings.Contains(goroutine, "kvwatch.(*Watcher).End(") {
			n++
		}
	}
	return n
}

// End may be called more than once, and by two callers at once: the first does the work and every
// caller returns once it has, so a second End beside a first still waiting on a watch never takes
// the first's wake-up.
func TestEndCalledTwiceAtOnceReturnsForBoth(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	held := holdWatch(kv)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", held.handle(), newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	armedWatch(t, held, "the watch to arm its watcher")
	ended := make(chan struct{}, 2)
	for range 2 {
		go func() {
			// Under the test's own context, which has no deadline: each End's return is the fact
			// the row waits for, and only the wait below ends that wait.
			w.End(t.Context())
			ended <- struct{}{}
		}()
	}
	eventually(t, "both Ends to wait for the watch", func() bool { return endsWaiting() == 2 })
	close(held.release)
	for range 2 {
		testwait.Receive(t, "both Ends to return once the watch they waited for had finished", ended)
	}
	if consumers := testConsumers(t, conn); len(consumers) != 0 {
		t.Fatalf("consumers after both Ends = %v, want none", consumers)
	}
	if got := warnings(records()); len(got) != 0 {
		t.Fatalf("End logged %q, want nothing", got)
	}
}

// A delete that fails for any reason but its consumer being gone stays an ERROR: here the bucket's
// stream was deleted under the watcher, so the server answers "stream not found".
func TestAFailedConsumerDeleteAtTheEndIsAnError(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	eventually(t, "the first scan", w.Ready)
	js, err := conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue("kvwatch-test"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	w.End(testContext(t))
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end failed: nats: stream not found"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}

// End is bounded by its context: against a server that does not answer, it returns at the
// deadline and logs the unanswered delete as an ERROR rather than holding the shutdown.
func TestEndIsBoundedByItsContext(t *testing.T) {
	srv, uri := testnats.Start(t)
	_, kv := bucket(t, uri)
	logger, records := jsonLogger(t)
	w := kvwatch.New("test cache", kv, newSeen(), kvwatch.WithLogger(logger))
	w.Start()
	eventually(t, "the first scan", w.Ready)

	thaw := srv.Freeze(t)
	t.Cleanup(thaw)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	w.End(ctx)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("End took %s against an unresponsive server, want it to return at its deadline", elapsed)
	}
	if got, want := strings.Join(warnings(records()), ", "), "ERROR test cache watcher end failed: delete its consumer: context deadline exceeded"; got != want {
		t.Fatalf("End logged %q, want %q", got, want)
	}
}
