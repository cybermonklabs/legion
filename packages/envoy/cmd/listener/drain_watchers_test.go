package main

import (
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// listenerKVWatchers names every KV watcher the listener of these rows runs, as kvwatch names it in
// its lines: the interest registry's two, the session registry's and the known-session store's.
var listenerKVWatchers = []string{"interest registry", "role claims", "session registry", "known session store"}

// The listener's shutdown ends its KV watchers itself, before the connection drains: main adds the
// drain hooks that do it (endListenerKVWatchers, and the known-session store's EndWatch). Left to
// the connection's drain, a watcher's consumer delete races the drain and is refused, logged as an
// ERROR, or cut off by the close and logged nowhere, so a missing hook shows only some of the time.
// What shows it every time is the line kvwatch.Watcher.End writes once every stop it made has
// returned, `<name> watcher ended`, which the listener reaches only from those hooks: every row
// requires it of every watcher, and no ERROR.
//
// The two rows differ in what End finds. With every consumer present it deletes them, and the row
// also requires the interest and session buckets to carry no consumer afterwards: nats.go replaces
// none of those consumers unless the listener reads none of their heartbeats for a whole check.
// The second row deletes those two watchers' consumers, as the server loses an ordered consumer in
// a NATS restart, and waits before the SIGTERM until nats.go has put a consumer of its own in each
// one's place, which it does once the lost one's idle heartbeats have been missing for a whole check
// (5 s heartbeats, checked every 10 s), so nominally 5 to 20 s after the loss. End then meets
// nats.go's replacement and warns of nothing: it deletes it, or, if the replacement's create is
// still in flight in the listener (the server has made it, the listener has not yet read the
// answer), it deletes nothing, since nats.go has not yet named it to the subscription, and the
// server removes it once its interest is gone. So that row requires no empty bucket, only that End
// met no consumer gone. A row that sent the SIGTERM at once after the delete would leave what End
// meets to how long the shutdown takes to reach the hooks: the consumer still gone, and its WARN,
// when that is quick, and a replacement when it is slow, as it was (18 s) on a loaded machine where
// such a row, requiring the WARN, failed. The WARN for a consumer End finds gone is tested where
// the end follows the delete at once (TestADrainEndsTheKVWatchersBeforeTheConnection).
func TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, test := range []struct {
		name string
		// replaced deletes the interest and session watchers' consumers, then waits, still before
		// the SIGTERM, until nats.go has put a consumer of its own in each one's place.
		replaced bool
	}{
		{name: "consumers present"},
		{name: "consumers replaced by nats.go", replaced: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			testnats.Connect(t, uri).Close()
			listener := startListenerProcess(t, binary, uri, "drain-watchers")
			listener.waitHealthy(t)

			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			js, err := admin.JetStream()
			if err != nil {
				t.Fatalf("admin JetStream: %v", err)
			}
			buckets := []string{bus.InterestBucket, bus.SessionBucket}
			for _, bucket := range buckets {
				// The listener turns healthy before the interest marker collector's first pass
				// (main.go starts it after the bind), and that pass's scan holds a consumer of its
				// own on the interest bucket until the scan ends, so the cache watcher's is the
				// bucket's only one only once that scan has.
				stream := "KV_" + bucket
				watcher := onlyConsumer(t, js, stream)
				if !test.replaced {
					continue
				}
				if err := js.DeleteConsumer(stream, watcher); err != nil {
					t.Fatalf("delete %s's watcher consumer %s: %v", bucket, watcher, err)
				}
			}
			// Each bucket's replacement is waited for once both are lost, so the waits overlap.
			if test.replaced {
				for _, bucket := range buckets {
					awaitReplacedConsumer(t, js, "KV_"+bucket)
				}
			}

			listener.Terminate(t)
			listener.WaitExit(t, "SIGTERM")
			output := listener.Output.String()
			if !strings.Contains(output, "envoy-listener shutdown complete") {
				t.Fatalf("the listener did not finish its ordered shutdown:\n%s", output)
			}
			for _, line := range strings.Split(output, "\n") {
				if errorLine.MatchString(line) {
					t.Fatalf("the shutdown logged an error: %s", line)
				}
			}
			for _, watcher := range listenerKVWatchers {
				if !strings.Contains(output, watcher+" watcher ended") {
					t.Fatalf("the shutdown did not end the %s watcher itself (no line from its end):\n%s", watcher, output)
				}
			}
			// The second row reads End against nats.go's replacements only if End met them, and so
			// found no consumer gone.
			if test.replaced && strings.Contains(output, "watcher's consumer was already gone at its end") {
				t.Fatalf("a watcher's consumer was still gone at its end, so nats.go had not replaced it:\n%s", output)
			}
			if !test.replaced {
				for _, bucket := range buckets {
					if consumers := consumerCount(t, js, "KV_"+bucket); consumers != 0 {
						t.Fatalf("%s carries %d consumers after the shutdown, want none", bucket, consumers)
					}
				}
			}
		})
	}
}

// awaitReplacedConsumer waits until nats.go has put a consumer of its own on stream in the place of
// the watcher consumer the test deleted from it. That one starts at the lost one's next sequence
// (DeliverByStartSequencePolicy), where a watcher's own consumer, and a scan's, start at each
// key's last record.
func awaitReplacedConsumer(t *testing.T, js natsgo.JetStreamContext, stream string) {
	t.Helper()
	replaced := func() bool {
		for info := range js.Consumers(stream) {
			if info.Config.DeliverPolicy == natsgo.DeliverByStartSequencePolicy {
				return true
			}
		}
		return false
	}
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, replaced) {
		t.Fatalf("nats.go did not replace the watcher consumer deleted from %s; it carries %v", stream, streamConsumers(js, stream))
	}
}

// consumerCount is how many consumers stream carries, as the server's answer to a stream info
// request says; a request that fails fails the test, so a listing that read nothing is never taken
// for a stream that carries nothing.
func consumerCount(t *testing.T, js natsgo.JetStreamContext, stream string) int {
	t.Helper()
	info, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read %s's consumers: %v", stream, err)
	}
	return info.State.Consumers
}

// streamConsumers lists the stream's consumers.
func streamConsumers(js natsgo.JetStreamContext, stream string) []string {
	var names []string
	for name := range js.ConsumerNames(stream) {
		names = append(names, name)
	}
	return names
}

// onlyConsumer waits, up to 30 s, until the stream carries exactly one consumer, its cache
// watcher's, and returns its name; it fails naming the consumers it last saw.
func onlyConsumer(t *testing.T, js natsgo.JetStreamContext, stream string) string {
	t.Helper()
	var consumers []string
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if consumers = streamConsumers(js, stream); len(consumers) == 1 {
			return consumers[0]
		}
	}
	t.Fatalf("%s carried consumers %v for 30 s after the listener turned healthy, want its cache watcher's one", stream, consumers)
	return ""
}
