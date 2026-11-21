package kvwatch_test

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// abandonKV makes each watch for real, so the server has answered the consumer's create, and
// hands back a watcher whose updates nothing closes. With end set it ends the run before it hands
// the watcher back: the window where a watch's run ends after the create was answered and before
// nats.go's WatchFiltered sets the handler that closes them (jetstream/kv.go 1356-1368). Its Stop
// closes stopping as it begins and is then held until release closes, standing in for the consumer
// delete it sends, a request that takes no context and goes unanswered on a connection that
// dropped.
type abandonKV struct {
	jetstream.KeyValue
	end      func()
	stopping chan struct{}
	release  chan struct{}
	stopped  chan struct{}
	begin    sync.Once
	once     sync.Once
}

func (k *abandonKV) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(ctx, keys, opts...)
	if err != nil {
		return nil, err
	}
	if k.end != nil {
		k.end()
	}
	return &unclosedWatcher{KeyWatcher: watcher, updates: make(chan jetstream.KeyValueEntry), kv: k}, nil
}

type unclosedWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
	kv      *abandonKV
}

func (w *unclosedWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *unclosedWatcher) Stop() error {
	w.kv.begin.Do(func() { close(w.kv.stopping) })
	<-w.kv.release
	err := w.KeyWatcher.Stop()
	w.kv.once.Do(func() { close(w.kv.stopped) })
	return err
}

// subscriptionGoroutines counts the goroutines running one of kvwatch's subscription methods.
func subscriptionGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	count := 0
	for _, stack := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(stack, "kvwatch.(*subscription).") {
			count++
		}
	}
	return count
}

// A watch discards a watcher it made and never installed: one whose run ended while its consumer's
// create was in flight, and one a newer generation overtook, whose run ends while its stop's
// delete is unanswered. Nothing of it outlives the discard's bound: the watch returns at once,
// without waiting out the watcher's stop, whose delete may never be answered; and once the stop
// returns, the goroutine that reads and drops the watcher's entries ends within the bound, even
// when nothing ever closes them, and the watcher's consumer is gone from the server. The cache's
// own watcher is untouched. A watch under a live run that overtakes still returns with its watcher
// stopped (TestADiscardedWatcherIsStoppedAtOnceAndReadUntilItEnds).
func TestAnAbandonedWatchersDiscardEnds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ends    bool
		install func(w *kvwatch.Watcher, run context.Context, kv bus.KeyValue) error
	}{
		{
			name: "its run ended while the create was in flight", ends: true,
			install: func(w *kvwatch.Watcher, run context.Context, kv bus.KeyValue) error {
				return kvwatch.WatchUnder(w, run, kv)
			},
		},
		{
			name: "a newer generation overtook it",
			install: func(w *kvwatch.Watcher, run context.Context, kv bus.KeyValue) error {
				return kvwatch.RereadUnder(w, run, kv, kvwatch.Generation(w)+1)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			into := newSeen()
			w := kvwatch.New("test cache", kv, into, kvwatch.WithStreamProbeInterval(0))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the first read's completion", w.Following)

			js, err := jetstream.New(conn)
			if err != nil {
				t.Fatalf("jetstream: %v", err)
			}
			consumers := func() int {
				stream, err := js.Stream(context.Background(), "KV_"+kv.Bucket())
				if err != nil {
					t.Fatalf("read the bucket's stream: %v", err)
				}
				info, err := stream.Info(context.Background())
				if err != nil {
					t.Fatalf("read the bucket's stream: %v", err)
				}
				return info.State.Consumers
			}
			goroutines, held := subscriptionGoroutines(), consumers()

			run, end := context.WithCancel(context.Background())
			t.Cleanup(end)
			abandon := &abandonKV{KeyValue: kv.WatchHandle(), stopping: make(chan struct{}), release: make(chan struct{}), stopped: make(chan struct{})}
			if tc.ends {
				abandon.end = end
			} else {
				go func() {
					select {
					case <-abandon.stopping:
						end()
					case <-run.Done():
					}
				}()
			}
			var releasing sync.Once
			release := func() { releasing.Do(func() { close(abandon.release) }) }
			t.Cleanup(release)
			watched := make(chan error, 1)
			go func() { watched <- tc.install(w, run, bus.NewKeyValue(kv.Legacy(), abandon, kv.Streams())) }()
			select {
			case err := <-watched:
				if tc.ends && err == nil {
					t.Fatal("a watch whose run ended while its create was in flight answered nil, want the run's end")
				}
				if !tc.ends && err != nil {
					t.Fatalf("an overtaken watch answered %v, want nil", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the watch did not return within 2s: it waited out the discarded watcher's stop")
			}

			release()
			select {
			case <-abandon.stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("the discarded watcher was never stopped")
			}
			eventually(t, "the discard's goroutine to end", func() bool { return subscriptionGoroutines() <= goroutines })
			eventually(t, "the discarded watcher's consumer to be gone", func() bool { return consumers() <= held })

			if err := w.Err(); err != nil {
				t.Fatalf("the cache's watcher ended: %v", err)
			}
			if _, err := kv.Put(t.Context(), "after-the-discarded-watch", []byte("1")); err != nil {
				t.Fatalf("put: %v", err)
			}
			eventually(t, "the cache's own watcher's delivery", func() bool { return into.has("after-the-discarded-watch") })
		})
	}
}
