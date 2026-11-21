package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A listener whose machine id is a fully qualified host name starts and delivers. NATS refuses a
// dot in a consumer name, so the listener's durable and queue group spell each dot as an escape
// (bus.ListenerDurable, bus.ListenerQueue): the listener reaches ready on a real server, binds its
// durable under that name, and a session registered through it receives a stored message on its
// agent subject from its consumer, once. Without the escape the listener never leaves starting,
// and its bind retries "nats: invalid consumer name". A machine id that spells the escape itself
// gives the dotted id's durable and queue group, so a second listener started with it would take
// the first one's durable: it is refused at start, naming ENVOY_MACHINE_ID.
func TestADottedMachineIDStartsAndDelivers(t *testing.T) {
	const machineID = "host.example.internal"
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)

	t.Run("a dotted machine id reaches ready and delivers one message to a session", func(t *testing.T) {
		listener := startListenerProcess(t, binary, uri, machineID)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("the listener's output:\n%s", listener.Output.String())
			}
		})
		listener.waitHealthy(t)

		publisher := connectPublisher(t, uri)
		durable := bus.ListenerDurable(machineID)
		info, err := publisher.JS().ConsumerInfo(bus.Stream, durable)
		if err != nil {
			t.Fatalf("read the durable %q of the listener %s: %v", durable, machineID, err)
		}
		if !info.PushBound {
			t.Fatalf("the durable %q is not bound to the listener's subscription", durable)
		}

		sessionID := "ses_dotted_machine_" + id.New()
		standIn := startSessionStandIn(t, uri, sessionID, answerDone)
		postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistrationIn(t, sessionID, "epoch-1", "/work/dotted-machine"))
		// The session's entry keeps the machine id as the setting spells it; only the NATS names
		// escape it.
		listener.awaitSessionMachine(t, sessionID, machineID)

		item := newEnvelope("agent", contracts.LegacyAgentSubject(sessionID), "a message for a session on a machine with a dotted id")
		publish(t, publisher, item)
		requireFrames(t, standIn.collect(t, 1, 15*time.Second), item)
		standIn.none(t, 2*time.Second)
	})

	t.Run("a machine id that would encode as the dotted one is refused at start", func(t *testing.T) {
		// twin is the dotted id's escape, written out: the id whose plain spelling is the dotted
		// id's durable.
		twin := strings.TrimPrefix(bus.ListenerDurable(machineID), "listener-")
		if twin == machineID {
			t.Fatalf("the durable of %s is listener-%s: the dots are not encoded, so NATS refuses the name", machineID, twin)
		}
		if bus.ListenerDurable(twin) != bus.ListenerDurable(machineID) || bus.ListenerQueue(twin) != bus.ListenerQueue(machineID) {
			t.Fatalf("%q and %q no longer encode alike (durables %q and %q): choose another pair",
				twin, machineID, bus.ListenerDurable(twin), bus.ListenerDurable(machineID))
		}
		listener := startListenerProcess(t, binary, uri, twin)
		listener.WaitExit(t, "a start as "+twin)
		output := listener.Output.String()
		if code := listener.Cmd.ProcessState.ExitCode(); code != 1 {
			t.Fatalf("a listener started as %q exited %d, want 1:\n%s", twin, code, output)
		}
		if !strings.Contains(output, "ENVOY_MACHINE_ID") {
			t.Fatalf("the refusal of %q does not name ENVOY_MACHINE_ID:\n%s", twin, output)
		}
		if strings.Contains(output, "envoy-listener listening") {
			t.Fatalf("a listener started as %q served before it refused the id:\n%s", twin, output)
		}
	})
}
