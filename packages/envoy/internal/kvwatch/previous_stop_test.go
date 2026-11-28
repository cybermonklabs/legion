package kvwatch_test

import (
	"context"
	"sync"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
func TestAWatchsRunDoesNotWaitOutThePreviousWatchersDelete(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ends bool
	}{
		{name: "a live run waits for the stop"},
		{name: "a run that ends does not", ends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			testwait.Until(t, "the first read's completion", func() (bool, bool) {
				following := w.Following()
				return following, following
			})

			conn := testnats.Connect(t, uri)
			t.Cleanup(conn.Close)
			run, end := context.WithCancel(context.Background())
			t.Cleanup(end)
			watching := make(chan uint64, 1)
			rewatched := make(chan error, 1)
			go func() {
				watching <- goroutinetest.ID()
				rewatched <- w.Rewatch(run, conn)
			}()
			id := testwait.Receive(t, "the Rewatch goroutine id", watching)
			testwait.Receive(t, "the Rewatch to stop the watcher it replaced", held.entered)
			// The live run must wait in watch's select until the stop returns or the run ends.
			type watchResult struct {
				returned bool
				err      error
			}
			result := testwait.Until(t, "Rewatch to wait on the held stop or return prematurely", func() (watchResult, bool) {
				select {
				case err := <-rewatched:
					return watchResult{returned: true, err: err}, true
				default:
					return watchResult{}, waitsInWatch(id)
				}
			})
			if result.returned {
				t.Fatalf("a Rewatch under a live run returned (%v) while the replaced watcher's Stop was still held", result.err)
			}

			if tc.ends {
				end()
				// Were the Rewatch to wait out the replaced watcher's Stop, which is held until the
				// test releases it, this wait would give up and name it.
				if err := testwait.Receive(t, "the Rewatch to return once its run ended", rewatched); err != nil {
					t.Fatalf("rewatch: %v", err)
				}
				select {
				case <-held.returned:
					t.Fatal("the replaced watcher's Stop returned before it was released; the test proves nothing")
				default:
				}
			} else {
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
			testwait.Until(t, "the replacement watcher's delivery", func() (bool, bool) {
				has := into.has("after-the-rewatch")
				return has, has
			})
		})
	}
}
