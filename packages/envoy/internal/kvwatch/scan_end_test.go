package kvwatch_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// natsContextWaits counts the subscribing goroutine's children waiting on the watch context.
func natsContextWaits(creator uint64) int {
	n := 0
	for _, trace := range startedIn(creator, "") {
		if strings.Contains(trace, ".(*js).subscribe.func") && strings.Contains(trace, "/js.go:") {
			n++
		}
	}
	return n
}

// deliveryBlocked reports whether this watch's delivery is parked on its full updates buffer.
func deliveryBlocked(creator uint64) bool {
	for _, trace := range startedIn(creator, "") {
		header, _, _ := strings.Cut(trace, "\n")
		if strings.Contains(header, "[chan send") && strings.Contains(trace, "jetstream.(*kvs).WatchFiltered.func") {
			return true
		}
	}
	return false
}

// A watcher's subscription can end while its scan is under way and its idle wait armed: a watch
// that replaces it stops it, the connection it reads through closes, or End stops it at shutdown.
// Each is done here at the worst moment for nats.go's legacy watcher, with the cache holding the
// scan's first record, so the 256-entry updates buffer is full and nats.go's delivery is parked on
// it holding the watcher's lock. None may panic the process; the idle wait of that scan is
// disarmed, and its callback, run late as an already-started time.AfterFunc callback can be, ends
// nothing: no timed-out warm-up, no re-read; and the goroutine nats.go parks on the watch's context
// is released once the watcher has been stopped or has ended.
func TestASubscriptionEndingMidScanDisarmsTheIdleWaitAndReleasesItsContext(t *testing.T) {
	t.Parallel()
	const records = 600
	for _, test := range []struct {
		name string
		// end ends the subscription of w, whose scan is held, and returns once it has asked for
		// that end. release lets the held record go.
		end func(t *testing.T, w *kvwatch.Watcher, conn *natsgo.Conn, uri string, release func())
		// stopped is the terminal error the end leaves, "" for none.
		stopped string
	}{
		{name: "a replacing watch stops it", end: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, uri string, release func()) {
			_, other := bucket(t, uri)
			done := make(chan error, 1)
			watching := make(chan uint64, 1)
			// The switch waits for the held apply (applyMu), so the release comes once the watch is
			// parked on it, and the watch stops the replaced watcher while its scan is under way.
			go func() {
				watching <- goroutinetest.ID()
				done <- kvwatch.WatchOn(w, other)
			}()
			id := testwait.Receive(t, "the replacing watch's goroutine id", watching)
			testwait.Until(t, "the replacing watch waiting for the held apply", func() (bool, bool) {
				parked := parkedOnTheApplyLock(id)
				return parked, parked
			})
			release()
			if err := testwait.Receive(t, "the replacing watch to return", done); err != nil {
				t.Fatalf("the replacing watch: %v", err)
			}
		}},
		{name: "its connection closes", stopped: "test cache watcher stopped", end: func(t *testing.T, _ *kvwatch.Watcher, conn *natsgo.Conn, _ string, release func()) {
			conn.Close()
			release()
		}},
		{name: "End stops it at shutdown", end: func(t *testing.T, w *kvwatch.Watcher, _ *natsgo.Conn, _ string, release func()) {
			w.End(testwait.Context(t))
			release()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			uri := testnats.URL(t)
			_, seed := bucket(t, uri)
			for i := range records {
				if _, err := seed.Put(fmt.Sprintf("k%04d", i), []byte("v")); err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			conn, kv := bucket(t, uri)
			subscribing := make(chan uint64, 1)
			step := func(ctx context.Context, handle jetstream.KeyValue) (jetstream.KeyWatcher, error) {
				subscribing <- goroutinetest.ID()
				return handle.WatchAll(ctx)
			}
			consume := make(chan uint64, 1)
			holdCtx := testwait.Context(t)

			released := make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			into := withApply{seen: newSeen(), before: func(natsgo.KeyValueEntry) {
				holdOnce.Do(func() {
					consume <- goroutinetest.ID()
					select {
					case <-released:
					case <-holdCtx.Done():
					}
				})
			}}
			timers := &pending{}
			now := newClock()
			logger, logged := jsonLogger(t)
			w := kvwatch.New("test cache", scripted(kv, step).handle(), into, kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithNow(now.Now),
				kvwatch.WithStreamProbeInterval(0), kvwatch.WithLogger(logger))
			t.Cleanup(w.Stop)
			t.Cleanup(release)
			w.Start()
			creator := testwait.Receive(t, "the subscribing goroutine id", subscribing)
			consumeID := testwait.Receive(t, "the scan's first record", consume)
			idle := timers.last(t, kvwatch.ScanIdleTimeout)
			testwait.Until(t, "nats.go's delivery to park on the full updates buffer", func() (bool, bool) {
				blocked := deliveryBlocked(creator)
				return blocked, blocked
			})
			if natsContextWaits(creator) == 0 {
				t.Fatal("no goroutine of nats.go's waits on the running watch's context: the release checked below would prove nothing")
			}

			test.end(t, w, conn, uri, release)

			testwait.Until(t, "nats.go's goroutine on the ended watch's context to be released", func() (int, bool) {
				n := natsContextWaits(creator)
				return n, n == 0
			})
			if test.stopped != "" {
				testwait.Until(t, "the ended watcher's error", func() (error, bool) {
					err := w.Err()
					return err, err != nil
				})
			}
			if !idle.stopped.Load() {
				t.Fatal("the ended scan's idle wait was left armed")
			}

			// The idle wait's callback runs late, past the timeout, as a time.AfterFunc callback
			// already started when its Stop came would.
			rereads := len(timers.armed(5 * time.Second))
			now.advance(2 * kvwatch.ScanIdleTimeout)
			idle.fire()
			returned(t, "the ended watcher to finish consuming", consumeID)
			if timedOut := matching(logged(), "WARN", "test cache cache warm-up"); len(timedOut) != 0 {
				t.Fatalf("the late idle callback timed the ended scan out: %v", timedOut)
			}
			if got := len(timers.armed(5 * time.Second)); got != rereads {
				t.Fatalf("the late idle callback armed %d re-reads, want none", got-rereads)
			}
			var recorded string
			if err := w.Err(); err != nil {
				recorded = err.Error()
			}
			if recorded != test.stopped {
				t.Fatalf("Err after the end = %q, want %q", recorded, test.stopped)
			}
		})
	}
}
