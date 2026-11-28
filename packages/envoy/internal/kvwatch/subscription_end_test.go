package kvwatch_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A kvwatch subscription can end in ways kvwatch does not choose: the connection closes under it,
// or the listener shuts down mid-scan. None of them may panic the process. INFRA-261 was such a
// panic: nats.go's legacy KV watcher arms an init timer while a scan is pending, and that timer's
// callback could send on a channel the subscription's end had closed (nats.go v1.55.0
// kv.go:1210-1236). That timer is not on kvwatch's path: bus.KeyValue forwards no legacy watch,
// and WatchKeys goes to the jetstream handle (internal/bus/kv.go). The jetstream watcher has no
// init timer, and its update callback and its closed handler run on the one delivery goroutine
// (nats.go v1.55.0 jetstream/kv.go:1276-1388, nats.go:3798-3890), so nothing of its own sends
// after the close.
//
// What can still race is kvwatch's own handling of that end against a Stop. So the connection is
// closed while the scan is under way and nats.go's delivery is parked on the full 256-entry
// updates buffer, and Stop comes on either side of the moment the watcher's consumer reads the
// end. Neither order may panic. A Stop before the consumer reads the end leaves no error, no
// "watcher stopped" WARN and no repair wait. A Stop after it disarms the repair wait the end
// armed.
func TestASubscriptionEndingMidScanNeverPanics(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		stopFirst bool
	}{
		{name: "stopped before its end is read", stopFirst: true},
		{name: "stopped after its end is read"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			uri := testnats.URL(t)
			conn, kv := bucket(t, uri)
			for i := range 300 {
				if _, err := kv.Put(fmt.Sprintf("k%04d", i), []byte("v")); err != nil {
					t.Fatalf("put: %v", err)
				}
			}

			held, released := make(chan struct{}), make(chan struct{})
			var holdOnce, releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(released) }) }
			into := withApply{seen: newSeen(), before: func(natsgo.KeyValueEntry) {
				holdOnce.Do(func() {
					close(held)
					<-released
				})
			}}
			creator, consuming := make(chan uint64, 1), make(chan uint64, 1)
			timers := &pending{}
			logger, logged := jsonLogger(t)
			w := kvwatch.New("test cache", scripted(kv, reader(subscribing(creator), consuming)).handle(), into,
				kvwatch.WithStreamProbeInterval(0), kvwatch.WithAfterFunc(timers.afterFunc()), kvwatch.WithLogger(logger))
			t.Cleanup(w.Stop)
			t.Cleanup(release)
			w.Start()
			createdBy := testwait.Receive(t, "the watch to subscribe", creator)
			consumeID := testwait.Receive(t, "the watch to start consuming", consuming)
			testwait.Receive(t, "the scan's first record to reach the cache", held)
			testwait.Until(t, "this subscription's delivery to park on its full updates buffer", func() ([]string, bool) {
				return deliveryBlocked(createdBy, "github.com/nats-io/nats%2ego.(*Conn).subscribeLocked")
			})
			conn.Close()

			if row.stopFirst {
				w.Stop()
				release()
				returned(t, "the stopped watch's consumer to return", consumeID)
				if err := w.Err(); err != nil {
					t.Errorf("Err after Stop = %v, want nil", err)
				}
				if warns := matching(logged(), "WARN", stoppedWarn); len(warns) != 0 {
					t.Errorf("the stopped watch logged %d stop warnings, want none", len(warns))
				}
				if waits := timers.armed(time.Minute); len(waits) != 0 {
					t.Errorf("the stopped watch armed %d repair waits, want none", len(waits))
				}
			} else {
				release()
				err := testwait.Until(t, "the watch to record the end of its subscription", func() (error, bool) {
					err := w.Err()
					return err, err != nil
				})
				if err.Error() != stoppedWarn {
					t.Fatalf("Err after the subscription closed = %v, want %q", err, stoppedWarn)
				}
				repair := timers.last(t, time.Minute)
				w.Stop()
				if !repair.stopped.Load() {
					t.Error("Stop left the ended watch's repair wait armed")
				}
				returned(t, "the ended watch's consumer to return", consumeID)
			}
		})
	}
}
