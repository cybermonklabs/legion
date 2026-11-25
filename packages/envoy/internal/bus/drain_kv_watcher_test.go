package bus_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A drain ends the KV watchers, deleting the ordered consumers nats.go created for them, before it
// drains the connection, as the listener's shutdown does (kvwatch.Watcher.End from a drain hook).
// Left to the connection's drain, nats.go removes a watcher's subscription and only then sends the
// delete (checkDrained), so the connection's drain, which waits only for its subscriptions, can
// reach its publishes first and refuse the delete as "connection draining", an ERROR, whether or
// not the consumer still exists. Ended first, the delete is a request the drain waits for. A
// consumer still there is deleted with no WARN or ERROR; one already gone (a NATS restart loses an
// ordered consumer, which nats.go keeps in memory) is a warning, since the delete was for that
// state. Either way nats.go reports nothing asynchronously and nothing is an ERROR, which
// TestListenerFollowsTheInterestRegistryAcrossNATSRestarts refuses (LEGION-278).
// The lost consumer is held lost (testnats.HoldConsumerLost) until the drain is over: left alone,
// nats.go puts a new one in its place once a heartbeat check passes in silence, and the end would
// delete that one and warn of nothing (docs/testing.md, "A test that deletes a KV watcher's
// consumer").
func TestADrainEndsTheKVWatchersBeforeTheConnection(t *testing.T) {
	for _, test := range []struct {
		name         string
		consumerGone bool
		want         string
	}{
		{name: "consumer present", want: ""},
		{name: "consumer already gone", consumerGone: true, want: "WARN drain-test watcher's consumer was already gone at its end"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, uri := testnats.Start(t)
			logs := captureBusLogs(t)

			name := t.Name()
			client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(name))
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			kv, err := bus.EnsureKeyValue(t.Context(), client.Conn, client.JS(), &natsgo.KeyValueConfig{Bucket: "drain_kv_watcher"})
			if err != nil {
				t.Fatalf("create bucket: %v", err)
			}
			watcher := kvwatch.New("drain-test", kv, discardingCache{})
			watcher.Start()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := watcher.WaitReady(ctx); err != nil {
				t.Fatalf("wait for the watcher: %v", err)
			}
			client.AddDrainHook(watcher.End)

			admin := testnats.Connect(t, uri)
			var hold *testnats.ConsumerHold
			if consumers := consumerNames(t, admin, "KV_drain_kv_watcher"); len(consumers) != 1 {
				t.Fatalf("consumers before the drain = %v, want the watcher's one", consumers)
			} else if test.consumerGone {
				// Delete the watcher's consumer from a second connection, as the server does when it
				// loses it, and keep nats.go from replacing it until the test ends.
				hold = testnats.HoldConsumerLost(t, admin, "KV_drain_kv_watcher", consumers[0])
			}

			watcher.Stop()
			if err := client.Drain(10 * time.Second); err != nil {
				t.Fatalf("drain: %v", err)
			}
			// nats.go runs an async error report after the drain's close only if it queued it before
			// closing its callback queue; the wait gives one queued in time the moment to log.
			time.Sleep(200 * time.Millisecond)
			want, replaced := test.want, hold != nil && hold.Replaced(t)
			if replaced {
				// A stall past a whole heartbeat check let nats.go replace the lost consumer, which
				// the hold's own record of the server's creates says, so the end met the replacement
				// and has no loss to warn of; and a replacement still in flight when the end ran is
				// left for the server to remove.
				t.Logf("nats.go replaced the held consumer: the row reads the replacement's end")
				want = ""
			}
			if got := strings.Join(warnOrErrorRecords(t, logs, name), ", "); got != want {
				t.Fatalf("the drain logged %q, want %q", got, want)
			}
			// The end's own record that it ended the watcher, in every row: without it, a drain whose
			// hook never ran the end, or whose end sent no delete, passes wherever nothing logs a WARN
			// or an ERROR, which is every row once nats.go has replaced the held consumer. The record
			// names no client, and no other test runs beside this one, so it is this watcher's.
			if got := logs.count("", "drain-test watcher ended"); got != 1 {
				t.Fatalf("the drain logged %d %q records, want 1: the drain hook did not end the watcher", got, "drain-test watcher ended")
			}
			if consumers := consumerNames(t, admin, "KV_drain_kv_watcher"); len(consumers) != 0 && !replaced {
				t.Fatalf("consumers after the drain = %v, want none", consumers)
			}
		})
	}
}

// A drain runs its hooks once the delivery subscriptions have drained and before the connection's
// own drain, within the drain's deadline, so a hook's requests still reach the server.
func TestDrainRunsItsHooksWhileTheConnectionCarriesRequests(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	started := make(chan struct{})
	var delivered atomic.Bool
	if _, err := client.SubscribeCore("notifications.role.drain-hook", func(*natsgo.Msg) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		delivered.Store(true)
	}, "drain-hook"); err != nil {
		t.Fatalf("subscribe the role lane: %v", err)
	}
	if err := client.Conn.Publish("notifications.role.drain-hook", nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the delivery never reached its handler")
	}
	type observed struct {
		delivered, draining, deadline bool
		flush                         error
	}
	ran := make(chan observed, 2)
	client.AddDrainHook(func(ctx context.Context) {
		_, hasDeadline := ctx.Deadline()
		ran <- observed{
			delivered: delivered.Load(),
			draining:  client.Conn.IsDraining(),
			deadline:  hasDeadline,
			flush:     client.Conn.FlushWithContext(ctx),
		}
	})
	if err := client.Drain(5 * time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	close(ran)
	var runs []observed
	for run := range ran {
		runs = append(runs, run)
	}
	if len(runs) != 1 {
		t.Fatalf("the drain ran its hook %d times, want once", len(runs))
	}
	run := runs[0]
	if !run.delivered || run.draining || run.flush != nil || !run.deadline {
		t.Fatalf("the hook ran with the delivery finished %v, the connection draining %v, a flush answering %v and a deadline %v; want the delivery finished, the connection not draining, the flush answered and the drain's deadline",
			run.delivered, run.draining, run.flush, run.deadline)
	}
}

// warnOrErrorRecords takes the client under test from the moment it is connected, though that
// client may have logged nothing under its name yet: a healthy client of these tests first names
// itself in its close's records (`envoy nats disconnected`, `envoy nats connection closed`, `envoy
// nats recovery cancelled`), which nats.go reports from its own callbacks after the drain has
// returned. In 20 measured drains of
// TestAStuckDeliveryStillEndsTheKVWatchers' shape, 4 had no such record when Drain returned, the
// first landing 1.6 to 2.3 ms later, and that test reads the records with no wait of its own, so
// a helper that refuses its caller until such a record lands makes the test race nats.go's
// callback.
func TestWarnOrErrorRecordsTakesAClientThatHasLoggedNothing(t *testing.T) {
	_, uri := testnats.Start(t)
	logs := captureBusLogs(t)

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	if got := warnOrErrorRecords(t, logs, t.Name()); len(got) != 0 {
		t.Fatalf("the records of a client that has logged nothing = %q, want none", got)
	}
}

// consumerNames lists the stream's consumers through admin. A listing that fails fails the test
// (jetstream's lister reports its error, where nats.go's older one closes its channel and says
// nothing), so a listing that read nothing is never taken for a stream that carries nothing.
func consumerNames(t *testing.T, admin *natsgo.Conn, stream string) []string {
	t.Helper()
	js, err := jetstream.New(admin)
	if err != nil {
		t.Fatalf("admin JetStream: %v", err)
	}
	found, err := js.Stream(t.Context(), stream)
	if err != nil {
		t.Fatalf("read stream %s: %v", stream, err)
	}
	lister := found.ConsumerNames(t.Context())
	var names []string
	for name := range lister.Name() {
		names = append(names, name)
	}
	if err := lister.Err(); err != nil {
		t.Fatalf("list %s's consumers: %v", stream, err)
	}
	return names
}

// warnOrErrorRecords is every WARN or ERROR record the client named name logged so far, each as
// its level and message, with the error for an async error report. name is the name that client
// was connected with (bus.WithName). The caller passes it, rather than the helper waiting for a
// record that names the client under test, which a healthy client of these tests may not have
// logged yet when the assertion reads (TestWarnOrErrorRecordsTakesAClientThatHasLoggedNothing).
// The capture (captureBusLogs) is the process's default logger, so every client of every test
// running in this binary writes into it, and a record naming another client is dropped: without
// that, a client an earlier test left connected to a cluster it has stopped fails this test with
// its reconnect warning. A caller that names no client is refused, since the filter would then
// drop every record that names a client, the client under test's among them. A record naming no
// client is kept, since the records these tests require are a watcher's and a reconcile's, which
// name none.
func warnOrErrorRecords(t *testing.T, logs *busLogs, name string) []string {
	t.Helper()
	if name == "" {
		t.Fatal("warnOrErrorRecords needs the name the client under test was connected with (bus.WithName)")
	}
	var records []string
	for _, line := range logs.lines() {
		if line == "" {
			continue
		}
		var record struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode the log record %q: %v", line, err)
		}
		if record.Level != "WARN" && record.Level != "ERROR" {
			continue
		}
		if record.Name != "" && record.Name != name {
			continue
		}
		if record.Msg == "envoy nats async error" {
			records = append(records, record.Level+" "+record.Msg+": "+record.Error)
			continue
		}
		records = append(records, record.Level+" "+record.Msg)
	}
	return records
}
