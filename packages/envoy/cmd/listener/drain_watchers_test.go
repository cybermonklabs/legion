package main

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
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
// What shows it every time is the record kvwatch.Watcher.End writes of each watcher, which the
// listener reaches only from those hooks: `<name> watcher ended` once every stop it made has
// returned, or, on a connection lost before or during the end, `<name> watcher's consumer was left
// to the server`. Every row requires one of the two of every watcher, and no ERROR. The second is
// taken only where internal/bus, independently of the ends, recorded the main client's connection
// lost (`envoy nats disconnected` with its error). A NATS that falls silent for a few seconds at the
// SIGTERM loses that connection in any row: the drain then finds it reconnecting, or it is made
// again first and every watcher with it (the reconnect hook's rewatch). Either way the ends meet
// none of the row's consumers, so such a run reads only that each end said what it did.
//
// The rows differ in what End finds:
//   - With every consumer present it deletes them, and the buckets carry none afterwards.
//   - Consumers already gone: the interest and session watchers' consumers are deleted, as the
//     server loses an ordered consumer in a NATS restart, and held lost (testnats.HoldConsumerLost)
//     until the listener has exited, so however long the shutdown takes to reach the hooks, End
//     finds each gone and says so at WARN. Left alone, nats.go puts a new consumer in a lost one's
//     place once a heartbeat check passes in silence (5 s heartbeats, checked every 10 s, so
//     nominally 5 to 20 s on); the shutdown on a loaded machine took 18 s to reach the hooks, and a
//     row that did not hold the loss failed there for want of the WARN. The hold's heartbeats are
//     the test's own timers, so a stall of a process past a whole check can still let nats.go
//     replace a consumer; the hold's record of the server's creates (ConsumerHold.Replaced) says
//     whether it did, and the row requires the WARN only where it did not.
//   - Consumers replaced by nats.go: the same consumers are deleted and the row waits for nats.go's
//     replacements before the SIGTERM, so End meets them and warns of nothing. It deletes each, or,
//     if a replacement's create is still in flight in the listener (the server has made it, the
//     listener has not yet read the answer), deletes nothing, since nats.go has not yet named it to
//     the subscription, and the server removes it once its interest is gone. So that row requires
//     no empty bucket, only that End met no consumer gone.
//   - Connection reconnecting at the drain: the server stops answering until the listener's
//     connection reads as lost, so the drain runs its hooks on a context already ended, and each
//     end says its consumer was left to the server.
//
// A bucket "carries none" counts only consumers nats.go did not make itself in a lost one's place
// (consumersLeft): such a replacement, in flight when its watcher's end ran, is the server's to
// remove.
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
			var holds []*testnats.ConsumerHold
			for _, bucket := range buckets {
				stream := "KV_" + bucket
				switch {
				case test.lost:
					holds = append(holds, testnats.HoldConsumerLost(t, admin, stream, cacheWatcherConsumer(t, js, stream)))
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
				lost := func() bool { return mainConnectionLost(listener.Output.String()) }
				if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, lost) {
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
			everyRecord := func(map[string]any) bool { return true }
			connectionLost := mainConnectionLost(output)
			leftToServer := 0
			for _, watcher := range listenerKVWatchers {
				ended := countRecords(listener, watcher+" watcher ended", everyRecord) > 0
				left := countRecords(listener, watcher+" watcher's consumer was left to the server: the connection was lost before its delete", everyRecord) > 0
				switch {
				case left && connectionLost:
					leftToServer++
				case ended && !left:
				default:
					t.Fatalf("the shutdown did not end the %s watcher itself (no line from its end):\n%s", watcher, output)
				}
			}
			if test.frozen {
				// The row reads the drain of a reconnecting connection only if the drain met one.
				reconnecting := countRecords(listener, "nats drain error", func(record map[string]any) bool {
					text, _ := record["error"].(string)
					return strings.HasSuffix(text, natsgo.ErrConnectionReconnecting.Error())
				})
				if reconnecting == 0 || leftToServer != len(listenerKVWatchers) {
					t.Fatalf("the drain did not find the connection reconnecting, or a watcher's end did not say its consumer was left to the server:\n%s", output)
				}
				return
			}
			if connectionLost {
				// The connection was lost, and perhaps made again, which re-makes every watcher
				// (the reconnect hook's rewatch): what the ends met is no longer the row's consumers.
				t.Logf("the listener's main connection was lost during the row, so the row reads only that each watcher's end said so")
				return
			}
			gone := func(watcher string) bool {
				return countRecords(listener, watcher+" watcher's consumer was already gone at its end", everyRecord) > 0
			}
			if test.replaced {
				// The row reads End against nats.go's replacements only if End met them, and so
				// found no consumer gone.
				if slices.ContainsFunc(listenerKVWatchers, gone) {
					t.Fatalf("a watcher's consumer was still gone at its end, so nats.go had not replaced it:\n%s", output)
				}
				return
			}
			if test.lost {
				if slices.ContainsFunc(holds, func(hold *testnats.ConsumerHold) bool { return hold.Replaced(t) }) {
					t.Logf("nats.go replaced a held consumer: the row reads the replacement's end")
				} else {
					for _, watcher := range []string{"interest registry", "session registry"} {
						if !gone(watcher) {
							t.Fatalf("the %s watcher's end did not find its consumer gone:\n%s", watcher, output)
						}
					}
				}
			}
			for _, bucket := range buckets {
				if left := consumersLeft(t, js, "KV_"+bucket); len(left) != 0 {
					t.Fatalf("%s carries consumers %v after the shutdown, want none", bucket, left)
				}
			}
		})
	}
}

// busTextLine is one line internal/bus writes through Go's default logger, in the format
// TestListenerLogsFromOtherPackagesKeepTheirHandlers pins: the date and time, the level, then the
// message and its fields.
var busTextLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (?:DEBUG|INFO|WARN|ERROR) (.*)$`)

// busRecords is the fields of every line internal/bus wrote in output whose message is msg. A line
// is read as slog's default handler writes it: the message, then a space and key=value for each
// field, a value slog quoted read whole and unquoted (strconv.QuotedPrefix), an unquoted one up to
// the next space. A line whose text after the message is no such sequence is not one of them.
func busRecords(output, msg string) []map[string]string {
	var records []map[string]string
	for line := range strings.SplitSeq(output, "\n") {
		match := busTextLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		rest, found := strings.CutPrefix(match[1], msg)
		if !found || (rest != "" && !strings.HasPrefix(rest, " ")) {
			continue
		}
		fields := map[string]string{}
		for rest = strings.TrimPrefix(rest, " "); rest != ""; {
			key, after, found := strings.Cut(rest, "=")
			if !found || key == "" || strings.ContainsAny(key, ` "`) {
				fields = nil
				break
			}
			value, next, _ := strings.Cut(after, " ")
			if strings.HasPrefix(after, `"`) {
				literal, err := strconv.QuotedPrefix(after)
				if err != nil {
					fields = nil
					break
				}
				value, _ = strconv.Unquote(literal)
				next = strings.TrimPrefix(after[len(literal):], " ")
			}
			fields[key], rest = value, next
		}
		if fields != nil {
			records = append(records, fields)
		}
	}
	return records
}

// mainConnectionLost says whether internal/bus recorded, in output, the listener's main client's
// connection lost: a disconnect of the client named envoy that carries the error ending it. The
// close of a drain that went as it should carries none.
func mainConnectionLost(output string) bool {
	return slices.ContainsFunc(busRecords(output, "envoy nats disconnected"), func(fields map[string]string) bool {
		return fields["name"] == "envoy" && fields["error"] != ""
	})
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

// consumersLeft names the consumers stream carries other than those nats.go made itself in the
// place of a lost one (replacedByNATS). Such a replacement can be in flight when the shutdown
// ends its watcher, which then deletes nothing, and the server removes it once its interest is
// gone; any other consumer left is one the shutdown did not end.
func consumersLeft(t *testing.T, js jetstream.JetStream, stream string) []string {
	t.Helper()
	var left []string
	for _, consumer := range streamConsumerInfos(t, js, stream) {
		if !replacedByNATS(consumer) {
			left = append(left, consumer.Name)
		}
	}
	return left
}
