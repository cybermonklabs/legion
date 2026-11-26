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
// still lost at the drain: a disconnect carrying the error that ended it came after its last
// reconnect (mainConnection). A NATS that falls silent for a few seconds near the SIGTERM
// loses that connection in any row. Still lost at the drain, the ends can delete nothing, so such a
// run reads only that each said so. Made again before the drain, the connection makes every
// watcher again with it (the reconnect hook's rewatch): each end then must say `watcher ended`, and
// the buckets must carry none of the listener's consumers, as in any row, but the lost row's WARN,
// whose consumer the rewatch replaced, is not required.
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
//     the subscription. nats.go deletes that one once the answer comes and it finds the
//     subscription closed (nats.go v1.55.0 js.go, resetOrderedConsumer), but in a goroutine nothing
//     waits for, so a connection closed first leaves it for the server to remove once its interest
//     is gone. So that row requires no empty bucket, only that End met no consumer gone.
//   - Connection reconnecting at the drain: the server stops answering until the listener's
//     connection reads as lost, so the drain runs its hooks on a context already ended, and each
//     end says its consumer was left to the server.
//
// A bucket "carries none" counts only consumers nats.go did not make itself in a lost one's place
// (consumersLeft): such a replacement, in flight when its watcher's end ran, may be left to the
// server to remove.
func TestTheListenerShutdownEndsItsKVWatchersBeforeTheDrain(t *testing.T) {
	t.Parallel()
	// instance is the listener's instance, which names its main client (mainConnection).
	const instance = "drain-watchers-pod"
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
			listener := startListenerProcess(t, binary, uri, "drain-watchers", "ENVOY_INSTANCE_ID="+instance)
			listener.waitHealthy(t)

			admin := testnats.Connect(t, uri)
			t.Cleanup(admin.Close)
			js, err := jetstream.New(admin)
			if err != nil {
				t.Fatalf("admin JetStream: %v", err)
			}
			buckets := []string{bus.InterestBucket, bus.SessionBucket}
			// The cache watcher of each bucket, named as its end names it.
			cacheWatcher := map[string]string{bus.InterestBucket: "interest registry", bus.SessionBucket: "session registry"}
			// holds is each held consumer's hold, by the watcher whose consumer it is.
			holds := map[string]*testnats.ConsumerHold{}
			for _, bucket := range buckets {
				stream := "KV_" + bucket
				switch {
				case test.lost:
					holds[cacheWatcher[bucket]] = testnats.HoldConsumerLost(t, admin, stream, cacheWatcherConsumer(t, js, stream))
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
				lost := func() bool { lostAtDrain, _ := mainConnection(listener.Output.String(), instance); return lostAtDrain }
				if !testwait.Holds(listenerWaitBound(t), 100*time.Millisecond, lost) {
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
			lostAtDrain, remade := mainConnection(output, instance)
			leftToServer := 0
			for _, watcher := range listenerKVWatchers {
				ended := countRecords(listener, watcher+" watcher ended", everyRecord) > 0
				left := countRecords(listener, watcher+" watcher's consumer was left to the server: the connection was lost before its delete", everyRecord) > 0
				switch {
				case left && lostAtDrain:
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
			if lostAtDrain {
				t.Logf("the listener's main connection was still lost at the drain, so the row reads only that each watcher's end said its consumer was left to the server")
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
			if test.lost && remade {
				t.Logf("the listener's main connection was lost and made again before the drain, which made every watcher again: the row does not require its consumers found gone")
			} else if test.lost {
				for _, bucket := range buckets {
					watcher := cacheWatcher[bucket]
					if holds[watcher].Replaced(t) {
						t.Logf("nats.go replaced the %s watcher's held consumer: the row reads that replacement's end", watcher)
						continue
					}
					if !gone(watcher) {
						t.Fatalf("the %s watcher's end did not find its consumer gone:\n%s", watcher, output)
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

// busRecords is the fields of every line internal/bus wrote in output whose message is one of msgs,
// in the order the lines were written, each with its message under "msg". A line is read as slog's
// default handler writes it: the message, then a space and key=value for each field, a value slog
// quoted read whole and unquoted (strconv.QuotedPrefix), an unquoted one up to the next space. A
// line whose text after the message is no such sequence is not one of them.
func busRecords(output string, msgs ...string) []map[string]string {
	var records []map[string]string
	for line := range strings.SplitSeq(output, "\n") {
		match := busTextLine.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		for _, msg := range msgs {
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
				fields["msg"] = msg
				records = append(records, fields)
			}
			break
		}
	}
	return records
}

// mainConnection reads the connection of the main client of the listener running as instance from
// internal/bus's own records in output, in the order it wrote them. That client is named
// `envoy-listener <instance>` (connectListenerBus), and the bus writes in a line's name field the
// name its connection presents (bus.WithName): that name, followed by a space and the binary's
// build when the binary records one (connectionName). cmdtest builds the listener with none
// (-buildvcs=false, no stamped commit), so the field is `envoy-listener <instance>` alone, which
// slog quotes for its space. It is compared whole, since the listener's other clients write the
// same lines under names of their own: the delivery lease's `envoy-listener-lease <instance>`, the
// role lane's `envoy-role-lane`, the session consumers' `envoy-session-consumers` and the relay
// sockets' `envoy-relay-sockets`. lostAtDrain: a disconnect carrying the error that ended the
// connection came after its last reconnect, so the connection was still lost when the shutdown
// drained it. A disconnect with no error is a close, which leaves a lost connection lost:
// nats.go's close reports one whenever the connection has a socket (nats.go v1.55.0 nats.go,
// close), and a reconnect to a frozen server has one, since the kernel still accepts the dial, so
// the drain's close of a connection still reconnecting can log one after the loss. remade: it
// reconnected at least once, which makes every KV watcher again (the reconnect hook's rewatch).
func mainConnection(output, instance string) (lostAtDrain, remade bool) {
	name := "envoy-listener " + instance
	for _, fields := range busRecords(output, "envoy nats disconnected", "envoy nats reconnected") {
		switch {
		case fields["name"] != name:
		case fields["msg"] == "envoy nats reconnected":
			lostAtDrain, remade = false, true
		case fields["error"] != "":
			lostAtDrain = true
		}
	}
	return lostAtDrain, remade
}

// mainConnection reads the main connection's state from the bus's records alone: the error a
// disconnect carries, the order of the lines and the client's name, not a line's text elsewhere.
// The name it reads is the listener's main client's whole name field, `envoy-listener <instance>`
// (mainConnection says why), which slog's text handler quotes for its space; the role lane's line,
// under a name of its own, is not read.
func TestTheShutdownTestReadsTheMainConnectionAtTheDrain(t *testing.T) {
	t.Parallel()
	const (
		instance   = "pod-a"
		mainLost   = `2026/11/18 07:00:01 INFO envoy nats disconnected name="envoy-listener pod-a" error="nats: stale connection"`
		mainClosed = `2026/11/18 07:00:02 INFO envoy nats disconnected name="envoy-listener pod-a"`
		mainBack   = `2026/11/18 07:00:03 INFO envoy nats reconnected name="envoy-listener pod-a" url=nats://127.0.0.1:4222`
		laneLost   = `2026/11/18 07:00:01 INFO envoy nats disconnected name=envoy-role-lane error="nats: stale connection"`
		jsonQuote  = `{"time":"2026-11-18T07:00:01Z","level":"INFO","msg":"envoy nats disconnected name=\"envoy-listener pod-a\" error=x"}`
	)
	for _, row := range []struct {
		name                string
		lines               []string
		lostAtDrain, remade bool
	}{
		{name: "closed by a drain", lines: []string{mainClosed}},
		{name: "only another client's connection lost", lines: []string{laneLost, mainClosed}},
		{name: "a JSON line quoting the text", lines: []string{jsonQuote}},
		{name: "lost, and the drain found it reconnecting", lines: []string{mainLost}, lostAtDrain: true},
		{name: "lost, then closed by the drain while still reconnecting", lines: []string{mainLost, mainClosed}, lostAtDrain: true},
		{name: "lost and made again before the drain", lines: []string{mainLost, mainBack, mainClosed}, remade: true},
		{name: "made again, then lost again at the drain", lines: []string{mainLost, mainBack, mainLost, mainClosed}, lostAtDrain: true, remade: true},
	} {
		lostAtDrain, remade := mainConnection(strings.Join(row.lines, "\n"), instance)
		if lostAtDrain != row.lostAtDrain || remade != row.remade {
			t.Errorf("%s: mainConnection = lost at the drain %t, remade %t; want %t, %t", row.name, lostAtDrain, remade, row.lostAtDrain, row.remade)
		}
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
	if !testwait.Holds(listenerWaitBound(t), 100*time.Millisecond, replaced) {
		t.Fatalf("nats.go did not replace the watcher consumer deleted from %s", stream)
	}
}

// consumersLeft names the consumers stream carries other than those nats.go made itself in the
// place of a lost one (replacedByNATS). Such a replacement can be in flight when the shutdown
// ends its watcher, which then deletes nothing, and nats.go's own delete of it once the create
// answers can come after the connection's close, which leaves it to the server; any other consumer
// left is one the shutdown did not end.
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
