// Package measurebounds times, under the box's load, the four NATS requests the listener and
// Dispatch send with no bound of the request helper's (bus.Request): a KV watch's create, the
// durable consumer's bind, the consumer-name lister and Dispatch's agent-stream replay; and it
// checks that each ends at a bound of its own when its reply never comes (withheld_test.go). The
// start's stream ensure is not among them: a start's wait ends on its caller's context and on the
// fact it waits for, not on a fixed bound. The timings are a measurement, not a test of the
// product: they run only with MEASUREBOUNDS_MEASURE set, and a run logs them and fails on none.
package measurebounds

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/agentstream"
	"github.com/sjawhar/envoy/internal/testnats"
)

const runs = 30

func loadavg(t *testing.T) string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		t.Fatalf("read /proc/loadavg: %v", err)
	}
	return strings.Join(strings.Fields(string(b))[:3], " ")
}

func report(t *testing.T, name string, times []time.Duration) {
	slices.Sort(times)
	p99 := times[int(math.Ceil(0.99*float64(len(times))))-1]
	t.Logf("MEASURE %s: n=%d p50=%s p99=%s max=%s", name, len(times), times[len(times)/2], p99, times[len(times)-1])
}

func TestMeasureTheFourRequestsWithNoBoundOfTheHelpers(t *testing.T) {
	if os.Getenv("MEASUREBOUNDS_MEASURE") == "" {
		t.Skip("the measurement runs only with MEASUREBOUNDS_MEASURE set")
	}
	uri := testnats.URL(t)
	t.Logf("MEASURE load at start: %s", loadavg(t))
	defer func() { t.Logf("MEASURE load at end: %s", loadavg(t)) }()

	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := client.JS()

	// A KV watch's create (bus.KeyValue.WatchKeys), on a bucket holding a few keys.
	kv, err := bus.EnsureKeyValue(t.Context(), client.Connection(), js, &nats.KeyValueConfig{Bucket: "measure_watch", Storage: nats.MemoryStorage})
	if err != nil {
		t.Fatalf("ensure the bucket: %v", err)
	}
	for i := range 8 {
		if _, err := kv.Put(t.Context(), fmt.Sprintf("k%d", i), []byte("v")); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	watch := make([]time.Duration, 0, runs)
	for range runs {
		started := time.Now()
		w, err := kv.WatchKeys(context.Background(), ">")
		watch = append(watch, time.Since(started))
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
		_ = w.Stop()
	}
	report(t, "a KV watch's create (WatchKeys)", watch)

	// The durable consumer's bind (Subscribe with nats.Bind), on a durable that exists.
	bind := make([]time.Duration, 0, runs)
	for i := range runs {
		name := fmt.Sprintf("measure-bind-%d", i)
		if _, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: name, DeliverSubject: nats.NewInbox(), AckPolicy: nats.AckExplicitPolicy}); err != nil {
			t.Fatalf("add the durable: %v", err)
		}
		started := time.Now()
		sub, err := client.Subscribe("", func(*nats.Msg) {}, nats.Bind(bus.Stream, name), nats.ManualAck())
		bind = append(bind, time.Since(started))
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		_ = sub.Unsubscribe()
		_ = js.DeleteConsumer(bus.Stream, name)
	}
	report(t, "the durable consumer's bind (Subscribe, nats.Bind)", bind)

	// The consumer-name lister, over a stream holding 50 consumers.
	for i := range 50 {
		if _, err := js.AddConsumer(bus.Stream, &nats.ConsumerConfig{Durable: fmt.Sprintf("measure-list-%d", i), AckPolicy: nats.AckExplicitPolicy}); err != nil {
			t.Fatalf("add a listed consumer: %v", err)
		}
	}
	list := make([]time.Duration, 0, runs)
	for range runs {
		started := time.Now()
		names := 0
		for range js.ConsumerNames(bus.Stream) {
			names++
		}
		list = append(list, time.Since(started))
		if names < 50 {
			t.Fatalf("the lister named %d consumers, want at least 50", names)
		}
	}
	report(t, "the consumer-name lister (ConsumerNames, 50 consumers)", list)

	// Dispatch's agent-stream replay, answered by a session on its control subject.
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	const sessionID = "ses_measure_replay"
	responder, err := conn.Subscribe(contracts.AgentStreamControlSubject(sessionID), func(m *nats.Msg) { _ = m.Respond([]byte("frame")) })
	if err != nil {
		t.Fatalf("subscribe the session: %v", err)
	}
	t.Cleanup(func() { _ = responder.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	replayConn := testnats.Connect(t, uri)
	t.Cleanup(replayConn.Close)
	source := agentstream.NewNATS(replayConn)
	replay := make([]time.Duration, 0, runs)
	for range runs {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		started := time.Now()
		frame, err := source.Replay(ctx, sessionID)
		replay = append(replay, time.Since(started))
		cancel()
		if err != nil || string(frame) != "frame" {
			t.Fatalf("replay answered %q, %v", frame, err)
		}
	}
	report(t, "Dispatch's agent-stream replay (Replay)", replay)
}
