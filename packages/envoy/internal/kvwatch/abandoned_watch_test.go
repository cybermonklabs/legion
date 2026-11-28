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
	reading  chan uint64
	updates  chan jetstream.KeyValueEntry
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
	return &unclosedWatcher{KeyWatcher: watcher, kv: k}, nil
}

type unclosedWatcher struct {
	jetstream.KeyWatcher
	kv *abandonKV
}

func (w *unclosedWatcher) Updates() <-chan jetstream.KeyValueEntry {
	w.kv.reading <- goroutinetest.ID()
	return w.kv.updates
}

func (w *unclosedWatcher) Stop() error {
	w.kv.begin.Do(func() { close(w.kv.stopping) })
	<-w.kv.release
	err := w.KeyWatcher.Stop()
	w.kv.once.Do(func() { close(w.kv.stopped) })
	return err
}

// A watch discards a watcher it made and never installed: one whose run ended while its consumer's
// create was in flight, and one a newer generation overtook, whose run ends while its stop's
// delete is unanswered. Nothing of it outlives the discard: the watch returns without waiting out
// the watcher's stop, which the test holds and whose delete may never be answered; and once the
// stop returns, the goroutine that reads and drops the watcher's entries ends, even when nothing
// ever closes them, and the watcher's consumer is gone from the server. The cache's
// own watcher is untouched. A watch under a live run that overtakes still returns with its watcher
// stopped (TestADiscardedWatcherIsStoppedAtOnceAndReadUntilItEnds).
func TestAnAbandonedWatchersDiscardEnds(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			uri := testnats.URL(t)
			_, kv := bucket(t, uri)
			into := newSeen()
			timers := &pending{}
			w := kvwatch.New("test cache", kv, into, kvwatch.WithStreamProbeInterval(0),
				kvwatch.WithAfterFunc(timers.afterFunc()))
			t.Cleanup(w.Stop)
			w.Start()
			eventually(t, "the first read's completion", w.Following)

			ctx := testwait.Context(t)
			held := len(testConsumers(t, ctx, kv))

			run, end := context.WithCancel(ctx)
			t.Cleanup(end)
			abandon := &abandonKV{
				KeyValue: kv.WatchHandle(),
				stopping: make(chan struct{}),
				release:  make(chan struct{}),
				stopped:  make(chan struct{}),
				reading:  make(chan uint64, 1),
				updates:  make(chan jetstream.KeyValueEntry),
			}
			t.Cleanup(func() { close(abandon.updates) })
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
			err := testwait.Receive(t, "the watch to return while the discarded watcher's stop is held", watched)
			if tc.ends && err == nil {
				t.Fatal("a watch whose run ended while its create was in flight answered nil, want the run's end")
			}
			if !tc.ends && err != nil {
				t.Fatalf("an overtaken watch answered %v, want nil", err)
			}

			drainID := testwait.Receive(t, "the discarded watcher's reader to start", abandon.reading)
			wait(t, abandon.stopping, "the discarded watcher's stop to start")
			if waits := timers.armed(kvwatch.AbandonedDrain); len(waits) != 0 {
				t.Fatalf("the drain armed %d waits while the stop was still held, want none", len(waits))
			}
			release()
			testwait.Receive(t, "the discarded watcher to be stopped", abandon.stopped)
			drain := testwait.Until(t, "the discarded watcher's drain to arm its bound", func() (*pendingWait, bool) {
				live := goroutinetest.Live(drainID)
				waits := timers.armed(kvwatch.AbandonedDrain)
				if len(waits) > 1 {
					t.Fatalf("the drain armed %d bounds, want one", len(waits))
				}
				if len(waits) == 1 {
					return waits[0], true
				}
				if !live {
					t.Fatal("the discarded watcher's drain returned without arming its bound on after")
				}
				return nil, false
			})
			testwait.Until(t, "the stopped watcher's drain to keep reading until its bound passes", func() (string, bool) {
				select {
				case abandon.updates <- fakeEntry{key: "discarded"}:
					return "", true
				default:
					trace := traceOf(drainID)
					if trace == "" {
						t.Fatal("the drain returned before its bound passed")
					}
					return trace, false
				}
			})
			drain.fire()
			returned(t, "the discard's drain goroutine to return", drainID)
			testwait.Until(t, "the goroutines the discard's drain started to return", func() ([]string, bool) {
				children := startedIn(drainID, "")
				return children, len(children) == 0
			})
			if !drain.stopped.Load() {
				t.Error("the drain returned without stopping its timer")
			}
			testwait.Until(t, "the discarded watcher's consumer to be gone", func() (int, bool) {
				got := len(testConsumers(t, ctx, kv))
				return got, got <= held
			})

			if err := w.Err(); err != nil {
				t.Fatalf("the cache's watcher ended: %v", err)
			}
			if _, err := kv.Put("after-the-discarded-watch", []byte("1")); err != nil {
				t.Fatalf("put: %v", err)
			}
			eventually(t, "the cache's own watcher to deliver a new record", func() bool { return into.has("after-the-discarded-watch") })
		})
	}
}
