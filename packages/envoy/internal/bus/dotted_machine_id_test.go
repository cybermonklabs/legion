package bus_test

import (
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The listener's durable and queue group keep the names of every machine id NATS takes, so no
// deployed durable is renamed, and give a machine id with a dot names NATS takes: a real server
// creates the durable of host.example.internal, and its queue group is one subject token.
// cmd/listener's test of this name runs a listener with that id.
func TestADottedMachineIDStartsAndDelivers(t *testing.T) {
	t.Parallel()
	t.Run("a plain machine id keeps its names", func(t *testing.T) {
		for _, row := range []struct{ machineID, durable, queue string }{
			{"gke", "listener-gke", "envoy-listener-gke"},
			{"example-host-a", "listener-example-host-a", "envoy-listener-example-host-a"},
			{"dispatch", "listener-dispatch", "envoy-listener-dispatch"},
			{"example_host:1@a", "listener-example_host:1@a", "envoy-listener-example_host:1@a"},
		} {
			if got := bus.ListenerDurable(row.machineID); got != row.durable {
				t.Errorf("ListenerDurable(%q) = %q, want %q", row.machineID, got, row.durable)
			}
			if got := bus.ListenerQueue(row.machineID); got != row.queue {
				t.Errorf("ListenerQueue(%q) = %q, want %q", row.machineID, got, row.queue)
			}
		}
	})

	t.Run("two ids that differ only in an escaped character give two durables", func(t *testing.T) {
		ids := []string{"host.example", "host*example", "host>example", "host/example", "host\\example", "host\texample"}
		seen := make(map[string]string, len(ids))
		for _, machineID := range ids {
			durable := bus.ListenerDurable(machineID)
			if other, ok := seen[durable]; ok {
				t.Errorf("ListenerDurable(%q) = ListenerDurable(%q) = %q, want two durables", machineID, other, durable)
			}
			seen[durable] = machineID
		}
	})

	t.Run("a dotted machine id names a durable NATS creates", func(t *testing.T) {
		const machineID = "host.example.internal"
		durable, queue := bus.ListenerDurable(machineID), bus.ListenerQueue(machineID)
		// nats-server refuses these in a consumer name (server/jetstream.go isValidAssetName),
		// and nats.go refuses a dot (jsm.go checkConsumerName).
		if strings.ContainsAny(durable, " \t\r\n\f.*>\\/") || len(durable) > 255 {
			t.Errorf("ListenerDurable(%q) = %q, which is no consumer name NATS takes", machineID, durable)
		}
		if strings.ContainsAny(queue, " \t\r\n\f.*>") {
			t.Errorf("ListenerQueue(%q) = %q, which is not one subject token", machineID, queue)
		}

		conn := testnats.Connect(t, testnats.URL(t))
		t.Cleanup(conn.Close)
		js, err := conn.JetStream()
		if err != nil {
			t.Fatalf("open JetStream: %v", err)
		}
		run := id.New()
		stream := "DOTTED_MACHINE_" + run
		if _, err := js.AddStream(&natsgo.StreamConfig{Name: stream, Subjects: []string{"dotted-machine." + run + ".>"}, Storage: natsgo.MemoryStorage}); err != nil {
			t.Fatalf("create the stream %s: %v", stream, err)
		}
		t.Cleanup(func() { _ = js.DeleteStream(stream) })
		info, err := js.AddConsumer(stream, &natsgo.ConsumerConfig{Durable: durable, AckPolicy: natsgo.AckExplicitPolicy})
		if err != nil {
			t.Fatalf("NATS refused the durable %q of %s: %v", durable, machineID, err)
		}
		if info.Name != durable {
			t.Fatalf("NATS created the durable %q, want %q", info.Name, durable)
		}
		subscription, err := conn.QueueSubscribeSync("dotted-machine."+run+".role", queue)
		if err != nil {
			t.Fatalf("subscribe in the queue group %q: %v", queue, err)
		}
		t.Cleanup(func() { _ = subscription.Unsubscribe() })
		if err := conn.Flush(); err != nil {
			t.Fatalf("flush the subscription in %q: %v", queue, err)
		}
		if err := conn.LastError(); err != nil {
			t.Fatalf("NATS refused the subscription in %q: %v", queue, err)
		}
	})
}
