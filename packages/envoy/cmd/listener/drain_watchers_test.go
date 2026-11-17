package main

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
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
// The rows differ in what End finds:
//   - With every consumer present it deletes them, and the buckets carry none afterwards.
//   - Consumers already gone: the interest and session watchers' consumers are deleted, as the
//     server loses an ordered consumer in a NATS restart, and held lost (testnats.HoldConsumerLost)
//     until the listener has exited, so however long the shutdown takes to reach the hooks, End
//     finds each gone and says so at WARN. Left alone, nats.go puts a new consumer in a lost one's
//     place once a heartbeat check passes in silence (5 s heartbeats, checked every 10 s, so
//     nominally 5 to 20 s on); the shutdown on a loaded machine took 18 s to reach the hooks, and a
//     row that did not hold the loss failed there for want of the WARN.
//   - Consumers replaced by nats.go: the same consumers are deleted and the row waits for nats.go's
//     replacements before the SIGTERM, so End meets them and warns of nothing. It deletes each, or,
//     if a replacement's create is still in flight in the listener (the server has made it, the
//     listener has not yet read the answer), deletes nothing, since nats.go has not yet named it to
//     the subscription, and the server removes it once its interest is gone. So that row requires
//     no empty bucket, only that End met no consumer gone.
//   - Connection reconnecting at the drain: the server stops answering until the listener's
//     connection reads as lost, so the drain runs its hooks on a context already ended, and each
//     end says its consumer was left to the server.
func TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain(t *testing.T) {
	binary := cmdtest.Build(t, "envoy-listener")
	for _, test := range []struct {
		name string
		// lost deletes the interest and session watchers' consumers and holds them lost.
		lost bool
		// replaced deletes the interest and session watchers' consumers, then waits, still before
		// the SIGTERM, until nats.go has put a consumer of its own in each one's place.
		replaced bool
		// frozen stops the server answering, then waits, still before the SIGTERM, until the
		// listener's connection has gone stale, so the drain finds it reconnecting.
		frozen bool
	}{
		{name: "consumers present"},
		{name: "consumers already gone", lost: true},
		{name: "consumers replaced by nats.go", replaced: true},
		{name: "connection reconnecting at the drain", frozen: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			testnats.Connect(t, uri).Close()
			listener := startListenerProcess(t, binary, uri, "drain-watchers")
			listener.waitHealthy(t)

			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatalf("admin JetStream: %v", err)
			}
			buckets := []string{bus.InterestBucket, bus.SessionBucket}
			for _, bucket := range buckets {
				stream := "KV_" + bucket
				switch {
				case test.lost:
					testnats.HoldConsumerLost(t, admin, stream, cacheWatcherConsumer(t, js, stream))
				case test.replaced:
					watcher := cacheWatcherConsumer(t, js, stream)
					if err := js.DeleteConsumer(t.Context(), stream, watcher); err != nil {
						t.Fatalf("delete %s's watcher consumer %s: %v", bucket, watcher, err)
					}
				}
			}
			// Each bucket's replacement is waited for once both are lost, so the waits overlap.
			if test.replaced {
				for _, bucket := range buckets {
					awaitReplacedConsumer(t, js, "KV_"+bucket)
				}
			}
			if test.frozen {
				thaw := srv.Freeze(t)
				t.Cleanup(thaw)
				// The client pings every second and calls the connection stale at the second
				// ping unanswered (bus/connect.go), and then reconnects, which a frozen server
				// never completes.
				stale := func() bool {
					return strings.Contains(listener.Output.String(), "envoy nats disconnected name=envoy error=")
				}
				if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, stale) {
					t.Fatalf("the listener's connection never read as lost under a frozen server:\n%s", listener.Output.String())
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
			if test.frozen {
				// The row reads the drain of a reconnecting connection only if the drain met one.
				if !strings.Contains(output, "drain NATS: nats: connection reconnecting") {
					t.Fatalf("the drain did not find the connection reconnecting:\n%s", output)
				}
				// Nothing reaches the server, so each watcher's end says that its consumer was
				// left to the server.
				for _, watcher := range listenerKVWatchers {
					if !strings.Contains(output, watcher+" watcher's consumer was left to the server") {
						t.Fatalf("the shutdown did not end the %s watcher itself (no line from its end):\n%s", watcher, output)
					}
				}
				return
			}
			for _, watcher := range listenerKVWatchers {
				if !strings.Contains(output, watcher+" watcher ended") {
					t.Fatalf("the shutdown did not end the %s watcher itself (no line from its end):\n%s", watcher, output)
				}
			}
			if test.replaced {
				// The row reads End against nats.go's replacements only if End met them, and so
				// found no consumer gone.
				if strings.Contains(output, "watcher's consumer was already gone at its end") {
					t.Fatalf("a watcher's consumer was still gone at its end, so nats.go had not replaced it:\n%s", output)
				}
				return
			}
			if test.lost {
				for _, watcher := range []string{"interest registry", "session registry"} {
					if !strings.Contains(output, watcher+" watcher's consumer was already gone at its end") {
						t.Fatalf("the %s watcher's end did not find its consumer gone:\n%s", watcher, output)
					}
				}
			}
			for _, bucket := range buckets {
				if left := streamConsumerInfos(t, js, "KV_"+bucket); len(left) != 0 {
					t.Fatalf("%s carries %d consumers after the shutdown, want none", bucket, len(left))
				}
			}
		})
	}
}

// streamConsumerInfos is every consumer stream carries, as the server reports them. A listing that
// fails fails the test (jetstream's lister reports its error, where nats.go's older one closes its
// channel and says nothing), so a listing that read nothing is never taken for an empty stream.
func streamConsumerInfos(t *testing.T, js jetstream.JetStream, name string) []*jetstream.ConsumerInfo {
	t.Helper()
	stream, err := js.Stream(t.Context(), name)
	if err != nil {
		t.Fatalf("read stream %s: %v", name, err)
	}
	lister := stream.ListConsumers(t.Context())
	var infos []*jetstream.ConsumerInfo
	for info := range lister.Info() {
		infos = append(infos, info)
	}
	if err := lister.Err(); err != nil {
		t.Fatalf("list %s's consumers: %v", name, err)
	}
	return infos
}

// replacedByNATS says whether consumer is one nats.go's ordered consumer made in the place of one
// the server lost: such a one starts at the lost one's next sequence
// (DeliverByStartSequencePolicy), where a watcher's own consumer, and a scan's, start at each
// key's last record.
func replacedByNATS(consumer *jetstream.ConsumerInfo) bool {
	return consumer.Config.DeliverPolicy == jetstream.DeliverByStartSequencePolicy
}

// cacheWatcherConsumer is the name of the consumer of the cache's own watcher on stream, read from
// the consumers' configurations rather than by waiting for the others to end: the watcher reads
// values, from each key's last record, where the interest marker collector's scan, which holds a
// consumer of its own on the interest bucket while it runs, reads metadata only
// (store/interest_markers.go, MetaOnly). It fails unless exactly one consumer reads that way.
func cacheWatcherConsumer(t *testing.T, js jetstream.JetStream, stream string) string {
	t.Helper()
	var watchers []string
	for _, consumer := range streamConsumerInfos(t, js, stream) {
		if !consumer.Config.HeadersOnly && consumer.Config.DeliverPolicy == jetstream.DeliverLastPerSubjectPolicy {
			watchers = append(watchers, consumer.Name)
		}
	}
	if len(watchers) != 1 {
		t.Fatalf("%s carries %d consumers that read values from each key's last record (%v), want its cache watcher's one", stream, len(watchers), watchers)
	}
	return watchers[0]
}

// awaitReplacedConsumer waits until nats.go has put a consumer of its own on stream in the place of
// the watcher consumer the test deleted from it (replacedByNATS).
func awaitReplacedConsumer(t *testing.T, js jetstream.JetStream, stream string) {
	t.Helper()
	replaced := func() bool {
		for _, consumer := range streamConsumerInfos(t, js, stream) {
			if replacedByNATS(consumer) {
				return true
			}
		}
		return false
	}
	if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, replaced) {
		t.Fatalf("nats.go did not replace the watcher consumer deleted from %s", stream)
	}
}
