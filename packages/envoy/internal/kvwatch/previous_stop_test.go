package kvwatch_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// heldStopQuiet is why a Rewatch under a live run is held quiet for a span while the replaced
// watcher's Stop is held: the Rewatch's wait for that Stop leaves no state this test can read, the
// watch does real network I/O to the test server, so synctest does not reach it, and the only later
// action whose effect would show it is the Stop's release, the event the Rewatch is to wait for.
const heldStopQuiet = "no state shows the Rewatch waiting on the held Stop, it does real network I/O so synctest cannot reach it, and the one later action is the Stop's release it must wait for"

// heldStopKV hands out watchers whose Stop is held until release closes, standing in for the
// delete of the consumer nats.go created for a watcher, a request that takes no context and goes
// unanswered on a connection that dropped. It serves one watch: entered closes when that watcher's
// Stop is called, returned once its Stop has returned.
type heldStopKV struct {
	jetstream.KeyValue
	entered, release, returned chan struct{}
	entering, returning        sync.Once
}

func newHeldStopKV(kv jetstream.KeyValue) *heldStopKV {
	return &heldStopKV{KeyValue: kv, entered: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
}

func (k *heldStopKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	return &heldStopWatcher{KeyWatcher: watcher, kv: k}, nil
}

type heldStopWatcher struct {
	jetstream.KeyWatcher
	kv *heldStopKV
}

func (w *heldStopWatcher) Stop() error {
	w.kv.entering.Do(func() { close(w.kv.entered) })
	<-w.kv.release
	err := w.KeyWatcher.Stop()
	w.kv.returning.Do(func() { close(w.kv.returned) })
	return err
}

// A watch stops the watcher it replaces, and nats.go's Stop deletes that watcher's consumer with a
// request that takes no context. A reconnect hook's Rewatch passes its run, whose connection may
// drop while that delete is unanswered: the watch waits for the stop only until the run ends, so
// the run ends at the drop rather than at the request's own bound, and the stop goes on by itself.
// A run that does not end waits for the stop, so a live Rewatch that returned has the previous
// watcher stopped. Either way the replacement is the cache's watcher.
func TestAWatchsRunDoesNotWaitOutThePreviousWatchersDelete(t *testing.T) {
	for _, tc := range []struct {
		name string
		ends bool
	}{
		{name: "a live run waits for the stop"},
		{name: "a run that ends does not", ends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			_, kv := bucket(t, uri)
			held := newHeldStopKV(kv.WatchHandle())
			var releasing sync.Once
			release := func() { releasing.Do(func() { close(held.release) }) }
			into := newSeen()
			w := kvwatch.New("test cache", bus.NewKeyValue(kv.Legacy(), held, kv.Streams()), into, kvwatch.WithStreamProbeInterval(0))
			t.Cleanup(w.Stop)
			// Cleanups run last-in first-out: the held Stop is released before the watcher stops.
			t.Cleanup(release)
			w.Start()
			testwait.Eventually(t, "the first read's completion", w.Following)

			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			run, end := context.WithCancel(context.Background())
			t.Cleanup(end)
			rewatched := make(chan error, 1)
			go func() { rewatched <- w.Rewatch(run, conn) }()
			testwait.Receive(t, "the Rewatch to stop the watcher it replaced", held.entered)
			// While the run is live the watch waits for the stop.
			testwait.Quiet(t, "a Rewatch under a live run, while the replaced watcher's Stop is held", rewatched, 100*time.Millisecond, heldStopQuiet)

			if tc.ends {
				end()
				// The replaced watcher's Stop stays held until the test's cleanup, so a Rewatch that
				// waited it out would not return before then, and this wait would give up naming it.
				if err := testwait.Receive(t, "the Rewatch to return once its run ended, with the replaced watcher's Stop still held", rewatched); err != nil {
					t.Fatalf("rewatch: %v", err)
				}
				select {
				case <-held.returned:
					t.Fatal("the replaced watcher's Stop returned before it was released; the test proves nothing")
				default:
				}
			} else {
				testwait.Quiet(t, "a Rewatch under a live run, while the replaced watcher's Stop is held", rewatched, 300*time.Millisecond, heldStopQuiet)
				release()
				if err := testwait.Receive(t, "the Rewatch to return once the replaced watcher's Stop returned", rewatched); err != nil {
					t.Fatalf("rewatch: %v", err)
				}
				select {
				case <-held.returned:
				default:
					t.Fatal("a Rewatch under a live run returned before the replaced watcher's Stop did")
				}
			}

			if err := w.Err(); err != nil {
				t.Fatalf("the replacement watcher ended: %v", err)
			}
			if _, err := w.KV().Put("after-the-rewatch", []byte("1")); err != nil {
				t.Fatalf("put: %v", err)
			}
			testwait.Eventually(t, "the replacement watcher's delivery", func() bool { return into.has("after-the-rewatch") })
		})
	}
}
