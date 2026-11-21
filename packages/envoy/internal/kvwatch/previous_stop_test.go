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
)

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
//
// Serial: its assertions are quiet windows: under a live run the Rewatch does not return for
// 100 ms while the replaced watcher's Stop is held, and, where the run stays live, for 300 ms more.
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
			eventually(t, "the first read's completion", w.Following)

			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			run, end := context.WithCancel(context.Background())
			t.Cleanup(end)
			rewatched := make(chan error, 1)
			go func() { rewatched <- w.Rewatch(run, conn) }()
			select {
			case <-held.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the Rewatch never stopped the watcher it replaced")
			}
			// While the run is live the watch waits for the stop.
			select {
			case err := <-rewatched:
				t.Fatalf("a Rewatch under a live run returned (%v) while the replaced watcher's Stop was still held", err)
			case <-time.After(100 * time.Millisecond):
			}

			if tc.ends {
				end()
				select {
				case err := <-rewatched:
					if err != nil {
						t.Fatalf("rewatch: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("the Rewatch did not return within 1s of its run ending: it waited out the replaced watcher's Stop")
				}
				select {
				case <-held.returned:
					t.Fatal("the replaced watcher's Stop returned before it was released; the test proves nothing")
				default:
				}
			} else {
				select {
				case err := <-rewatched:
					t.Fatalf("a Rewatch under a live run returned (%v) while the replaced watcher's Stop was still held", err)
				case <-time.After(300 * time.Millisecond):
				}
				release()
				select {
				case err := <-rewatched:
					if err != nil {
						t.Fatalf("rewatch: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the Rewatch did not return within 10s of the replaced watcher's Stop returning")
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
			eventually(t, "the replacement watcher's delivery", func() bool { return into.has("after-the-rewatch") })
		})
	}
}
