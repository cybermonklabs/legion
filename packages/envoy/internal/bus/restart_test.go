package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logtest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The listener opens its interest registry, session registry and CI store from client.Conn once,
// at start. A NATS server restart must leave that state working: the connection reconnects in
// place rather than closing, so a JetStream handle taken from it before the restart is still bound
// to a live connection afterwards.
func TestAServerRestartKeepsJetStreamStateTakenFromTheConnection(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js, err := client.Conn.JetStream(testnats.MaxWait(t))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	kv, err := js.CreateKeyValue(&natsgo.KeyValueConfig{Bucket: "server-restart"})
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	srv.Stop(t)
	srv.Start(t)
	testwait.Eventually(t, "the client to reconnect", client.Connected)
	testwait.Until(t, "a put through the KV handle taken before the restart", func() (error, bool) {
		_, err := kv.Put("key", []byte("value"))
		return err, err == nil
	})
}

// A publish while the server is away fails, and a publish that failed is never sent later: the
// callers treat an error as not delivered (a role-lane forward as delivery_failed, a webhook as a
// 500 for the sender to retry), so a copy held and sent after reconnecting would arrive twice.
func TestAPublishThatFailedWhileReconnectingIsNeverSent(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	srv.Stop(t)
	testwait.Eventually(t, "the client to notice the server is gone", func() bool { return !client.Connected() })
	item := contracts.Envelope{
		EventID:       "evt-publish-while-reconnecting",
		Source:        "agent",
		SourceEventID: "source-publish-while-reconnecting",
		Topic:         "notifications.role.publish-while-reconnecting",
		DedupeKey:     "publish-while-reconnecting",
		IssuedAt:      contracts.NowMillis(),
	}
	if err := client.Publish(item); err == nil {
		t.Fatal("a core publish while the server was away reported success")
	}

	srv.Start(t)
	observer := testnats.Connect(t, uri)
	t.Cleanup(observer.Close)
	received := make(chan *natsgo.Msg, 1)
	if _, err := observer.ChanSubscribe(item.Topic, received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := observer.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}
	testwait.Eventually(t, "the client to reconnect", client.Connected)
	// A marker the client publishes on the same subject once it is connected again reaches the
	// observer behind anything the client sent before it, so the marker arriving first shows the
	// failed publish was never sent.
	if err := client.Conn.Publish(item.Topic, []byte("marker")); err != nil {
		t.Fatalf("publish the marker: %v", err)
	}
	if message := testwait.Receive(t, "the marker published after the reconnect", received); string(message.Data) != "marker" {
		t.Fatalf("the publish that failed was sent after the reconnect: %s", message.Data)
	}
}

// A publish while the connection is reconnecting waits for the reconnect, within its own deadline,
// rather than failing at once: a webhook that gets an error answers 503, and the sender does not
// redeliver, so a failure fast enough to beat a NATS restart loses the event. The publish's bounds
// are on the client's clock, which stands still, so the server starts again only once the publish
// waits for the reconnect (its recheck armed), and the publish can only succeed by waiting for it.
func TestAPublishWhileReconnectingWaitsForTheReconnect(t *testing.T) {
	srv, uri := testnats.Start(t)
	still := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithClock(still), bus.WithPublishAcknowledgementClock(still))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	srv.Stop(t)
	testwait.Eventually(t, "the client to start reconnecting", func() bool { return client.Conn.IsReconnecting() })
	item := contracts.Envelope{
		EventID:        "evt-publish-across-restart",
		Source:         "github",
		SourceEventID:  "source-publish-across-restart",
		Topic:          "notifications.github.acme.widgets.push.branch.main",
		DedupeKey:      "publish-across-restart",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "publish across a NATS restart",
		TraceID:        "trace-publish-across-restart",
	}
	published := make(chan error, 1)
	go func() { published <- client.Publish(item) }()
	awaitArmedAt(t, still, "the publish's reconnect wait to arm its recheck", still.Now().Add(bus.ReconnectRecheck).Equal)
	srv.Start(t)
	if err := testwait.Receive(t, "the publish waiting through the restart", published); err != nil {
		t.Fatalf("a JetStream publish during a NATS restart failed: %v", err)
	}
}

// A connection that reconnects while Drain is still draining belongs to a process that is shutting
// down: the reconnect closes it, quietly, instead of re-subscribing or logging a failure. Drain's
// deadlines are on the client's clock, which stands still, so the drain ending at all says the
// reconnect ended it.
func TestAReconnectDuringDrainClosesQuietly(t *testing.T) {
	srv, uri := testnats.Start(t)
	still := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithClock(still))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(sync.OnceFunc(func() { close(release) }))
	held, err := client.SubscribeCore("notifications.role.reconnect-during-drain", func(*natsgo.Msg) {
		started <- struct{}{}
		<-release
	}, "reconnect-during-drain")
	if err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.reconnect-during-drain", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	testwait.Receive(t, "the delivery to reach its handler", started)

	logs := captureBusLogs(t)
	drained := make(chan error, 1)
	go func() { drained <- client.Drain(15 * time.Second) }()
	testwait.Eventually(t, "Drain to drain the delivery subscription", held.IsDraining)
	srv.Stop(t)
	srv.Start(t)
	testwait.Receive(t, "the reconnect during the drain to end it, with its deadline not reached", drained)
	if !client.Conn.IsClosed() {
		t.Fatal("the drain ended with the reconnected connection still open")
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a reconnect during the drain logged an error: %s", line)
	}
}

// busLogs collects the default slog logger's JSON records for one test.
type busLogs struct {
	logtest.Buffer
}

func (l *busLogs) errorLine() string {
	return l.lineAt("ERROR", "")
}

// lines returns the records logged so far, one per line.
func (l *busLogs) lines() []string {
	return strings.Split(l.String(), "\n")
}

// lineAt returns the first record at level whose text holds fragment, or "".
func (l *busLogs) lineAt(level, fragment string) string {
	for _, line := range l.lines() {
		if strings.Contains(line, `"level":"`+level+`"`) && strings.Contains(line, fragment) {
			return line
		}
	}
	return ""
}

// attempts decodes, in the order they were logged, the attempt numbers of the records at level
// with message msg, so a test reads the number from the record rather than from where slog writes
// it.
func (l *busLogs) attempts(level, msg string) []int {
	var found []int
	for _, line := range l.lines() {
		if line == "" {
			continue
		}
		var record struct {
			Level   string
			Msg     string
			Attempt int
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			panic(fmt.Sprintf("decode the bus log record %q: %v", line, err))
		}
		if record.Level == level && record.Msg == msg && record.Attempt > 0 {
			found = append(found, record.Attempt)
		}
	}
	return found
}

// count is how many records with message msg the client named name has logged so far.
func (l *busLogs) count(name, msg string) int {
	found := 0
	for _, line := range l.lines() {
		if line == "" {
			continue
		}
		var record struct {
			Msg  string
			Name string
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			panic(fmt.Sprintf("decode the bus log record %q: %v", line, err))
		}
		if record.Name == name && record.Msg == msg {
			found++
		}
	}
	return found
}

func captureBusLogs(t *testing.T) *busLogs {
	t.Helper()
	logs := &busLogs{}
	logtest.SetDefault(t, slog.NewJSONHandler(logs, nil))
	return logs
}

// A reconnect in place keeps every subscription nats.go restored. After a server restart, nats.go
// re-sends each subscription's SUB on the connection it reconnected, so the subscription Subscribe
// returned still delivers, the durable's from the consumer the server kept on its file store;
// tearing it down to bind the consumer again races the server's own release of the consumer's
// push binding, which refuses the new bind with "consumer is already bound to a subscription" and
// logs a resubscribe failure at ERROR (LEGION-278).
func TestAReconnectInPlaceKeepsTheSubscriptionsNATSRestored(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	var durableDeliveries, roleDeliveries atomic.Int32
	durable, err := subscribeAllNotifications(client, func(message *natsgo.Msg) {
		durableDeliveries.Add(1)
		_ = message.Ack()
	}, natsgo.Durable("in-place-reconnect"), natsgo.DeliverNew(), natsgo.AckExplicit(), natsgo.ManualAck())
	if err != nil {
		t.Fatalf("durable subscribe: %v", err)
	}
	const roleSubject = "notifications.role.in-place-reconnect"
	role, err := client.SubscribeCore(roleSubject, func(*natsgo.Msg) { roleDeliveries.Add(1) }, "in-place-reconnect")
	if err != nil {
		t.Fatalf("role subscribe: %v", err)
	}
	reconnected := make(chan struct{}, 1)
	client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
		reconnected <- struct{}{}
		return nil
	})

	logs := captureBusLogs(t)
	srv.Stop(t)
	srv.Start(t)
	testwait.Receive(t, "the reconnect to finish restoring the subscriptions", reconnected)
	if !durable.IsValid() || !role.IsValid() {
		t.Fatalf("a reconnect in place tore down the subscriptions Subscribe returned (durable valid %t, role valid %t)", durable.IsValid(), role.IsValid())
	}

	// The restarted server answers core NATS before JetStream is ready, so the publish is retried
	// until the stream acknowledges it. The message id makes a retry after a late acknowledgement
	// a duplicate the stream drops, so the stream holds the message once either way.
	testwait.Until(t, "the stream to acknowledge the publish to the durable after the restart", func() (error, bool) {
		_, err := client.JS().Publish("notifications.github.test.in-place-reconnect", []byte(`{}`), natsgo.MsgId("in-place-reconnect"))
		return err, err == nil
	})
	if err := client.Conn.Publish(roleSubject, []byte(`{}`)); err != nil {
		t.Fatalf("publish to the role lane: %v", err)
	}
	testwait.Eventually(t, "both deliveries after the restart", func() bool {
		return durableDeliveries.Load() >= 1 && roleDeliveries.Load() >= 1
	})
	// A second copy of either delivery would come from a second subscription of its subject; the
	// server holds one each, so no copy is still to come.
	info, err := client.JS().ConsumerInfo(bus.Stream, "in-place-reconnect")
	if err != nil {
		t.Fatalf("read the durable: %v", err)
	}
	for _, subject := range []string{info.Config.DeliverSubject, roleSubject} {
		if n := subscriptionsOn(t, srv, subject); n != 1 {
			t.Fatalf("the server holds %d subscriptions on %s after the restart, want 1", n, subject)
		}
	}
	if durable, role := durableDeliveries.Load(), roleDeliveries.Load(); durable != 1 || role != 1 {
		t.Fatalf("deliveries after the restart: durable %d, role %d, want one each", durable, role)
	}
	if line := logs.errorLine(); line != "" {
		t.Fatalf("a reconnect in place logged an error: %s", line)
	}
}

// A reconnect hook that fails after the client was stopped fails because of the stop: a SIGTERM
// that lands while the listener's rewatch hook runs closes the connection the hook is reading
// through. recover already reports that case as the stop; the reconnect path must too, at INFO
// with the error kept on the line. A hook that fails with the client still running is reported by
// how long the hooks have been uncovered, not by the single run that failed: a recovery runs them
// again within seconds (recoverUntilHealthy), so a failure it repairs is a WARN an operator need
// not act on, and the ERROR now means what it says, that the hooks have still not covered their
// connection event a whole bus.RepairGrace later and nothing is repairing the state behind them.
// The INFO line of the stop can hide neither.
func TestAReconnectHookFailureIsReportedByItsStallUnlessTheClientStopped(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop bool
	}{
		{name: "after the stop", stop: true},
		{name: "without a stop", stop: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			// The stall is measured on this clock, so the test moves the grace itself rather than
			// waiting out a minute of it.
			clock := newReconnectErrClock()
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReconnectErrClock(clock.Now))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			hooked := make(chan struct{}, 1)
			client.AddReconnectHook(func(context.Context, *natsgo.Conn) error {
				if tc.stop {
					// The shutdown begins while the hook is still running, and the hook's next
					// server request fails on the closed connection.
					client.Close()
				}
				// The recovery runs the failing hook again and again, and the test reads this
				// signal once, so a run that finds it unread goes on rather than blocking: a hook
				// stuck here would report no further failure, and the stall would never be seen.
				select {
				case hooked <- struct{}{}:
				default:
				}
				return errors.New("rewatch interest cache: nats: connection closed")
			})

			logs := captureBusLogs(t)
			srv.Stop(t)
			srv.Start(t)
			testwait.Receive(t, "the reconnect to run its hook", hooked)
			if !tc.stop {
				const retried = "envoy nats reconnect hook failed; it is retried"
				const uncovered = "envoy nats reconnect hooks have not succeeded"
				testwait.Eventually(t, "the failed run reported at WARN", func() bool {
					return logs.lineAt("WARN", retried) != ""
				})
				if line := logs.lineAt("WARN", retried); !strings.Contains(line, "connection closed") {
					t.Fatalf("a hook failing under the grace was not logged at WARN with its error: %q\n%s", line, logs.String())
				}
				if line := logs.errorLine(); line != "" {
					t.Fatalf("a hook failing under bus.RepairGrace logged an error: %s", line)
				}
				// Past the grace the hooks have still not covered the reconnect. The recovery runs
				// them again with backoff, up to 30 s between attempts, so the ERROR comes with the
				// next run rather than at once.
				clock.Advance(bus.RepairGrace)
				testwait.Eventually(t, "the uncovered hooks reported at ERROR past bus.RepairGrace", func() bool {
					return logs.lineAt("ERROR", uncovered) != ""
				})
				if line := logs.lineAt("ERROR", uncovered); !strings.Contains(line, "connection closed") {
					t.Fatalf("the hooks uncovered past bus.RepairGrace were not logged at ERROR with the hook's error: %q\n%s", line, logs.String())
				}
				return
			}
			testwait.Eventually(t, "the stop's report of the failed hook, or an error", func() bool {
				return strings.Contains(logs.String(), "reconnect hooks cancelled") || logs.errorLine() != ""
			})
			if line := logs.errorLine(); line != "" {
				t.Fatalf("a hook failing after the stop logged an error: %s", line)
			}
			if !strings.Contains(logs.String(), "envoy nats reconnect hooks cancelled") || !strings.Contains(logs.String(), "connection closed") {
				t.Fatalf("the stop was not reported with its error at INFO: %s", logs.String())
			}
		})
	}
}
