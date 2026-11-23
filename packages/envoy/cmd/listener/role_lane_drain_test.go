package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// laneOnHarness opens a role lane over harness's server whose handler is handler, as main.go opens
// the listener's (bus.ConnectRoleLane), and returns a roleLane over it and forwards, for its drain.
func laneOnHarness(t *testing.T, harness listenerDeliveryHarness, handler natsgo.MsgHandler, forwards *roleForwarder) roleLane {
	t.Helper()
	client, _, err := bus.ConnectRoleLane([]string{harness.client.Conn.ConnectedUrl()}, "test-machine", handler)
	if err != nil {
		t.Fatalf("open the role lane: %v", err)
	}
	t.Cleanup(client.Close)
	return roleLane{client: client, forwards: forwards, stopWatch: func() {}}
}

// The listener's stop keeps every role message its lane took or still holds while every forward
// slot is held. NATS refuses every forward, so the slots fill with held messages and the handler
// waits for a slot with the next, while the lane's subscription holds the rest. The drain stops the
// forwarder before it drains the lane, and releases the held messages to keep only once the lane's
// client has drained: the handler's wait ends, it keeps each message it takes, the drain takes
// every message the subscription held, and every message, held or not, is kept on the role's kept
// lane under held_at_shutdown. No held message's keep goes out before the handler's last keep: the
// stream's own sequence on the kept lane orders the keeps, so the order is read off the store and
// not off a clock (TestAHeldRoleMessageIsKeptOnlyOnceTheLaneHasDrained holds that order on its
// own).
func TestRoleLaneDrainKeepsEveryMessageWhileEverySlotIsHeld(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role     = "drain-every-slot-held"
		limit    = 8
		messages = 40
	)
	if err := harness.sessions.Put("ses_drain_every_slot", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole("ses_drain_every_slot", "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		return errors.New("nats: connection closed")
	})
	forwarder := newRoleForwarderLimited(cfg, limit)
	cfg.roleForwards = forwarder
	lane := laneOnHarness(t, harness, coreNATSDeliveryHandler(cfg), forwarder)
	publisher := testnats.Connect(t, harness.client.Conn.ConnectedUrl())
	t.Cleanup(publisher.Close)
	published := make([]contracts.Envelope, messages)
	for i := range published {
		published[i] = publishRoleMessage(t, publisher, role, fmt.Sprintf("drained %d", i))
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the role messages: %v", err)
	}
	waitFor(t, 20*time.Second, fmt.Sprintf("%d messages held and the handler waiting with the next", limit), func() bool {
		logs := harness.logs.String()
		return strings.Count(logs, `"msg":"`+roleHeldLine+`"`) >= limit &&
			countLogged(logs, "listener received", published[limit].EventID) == 1
	})
	if taken := len(linesIn(harness.logs.String(), "listener received", nil)); taken != limit+1 {
		t.Fatalf("the handler took %d messages before the stop, want %d: the rest must still wait in the lane's subscription", taken, limit+1)
	}

	lane.drain(harness.config.logger, 3*time.Second)

	logs := harness.logs.String()
	if strings.Contains(logs, roleLaneUndrainedLine) || strings.Contains(logs, roleHeldUnkeptLine) {
		t.Fatalf("the drain lost role messages:\n%s", logs)
	}
	// The handler keeps every message it takes after the stop - the one it was waiting for a slot
	// for, and each the drain hands it - and the held ones are kept after the last of those: the
	// stream stores the keeps on the kept lane in the order they were made, and its sequence says
	// so to the message.
	keptAt := func(item contracts.Envelope) uint64 {
		kept := linesIn(logs, "listener kept role message for the role's next holder", map[string]any{"event_id": item.EventID})
		if len(kept) != 1 {
			t.Fatalf("message %s has kept lines %v, want one:\n%s", item.EventID, kept, logs)
		}
		sequence, ok := kept[0]["sequence"].(float64)
		if !ok {
			t.Fatalf("message %s's kept line names no sequence: %v", item.EventID, kept[0])
		}
		return uint64(sequence)
	}
	var lastHandlerKeep uint64
	for _, item := range published[limit:] {
		lastHandlerKeep = max(lastHandlerKeep, keptAt(item))
	}
	for i, item := range published[:limit] {
		if sequence := keptAt(item); sequence < lastHandlerKeep {
			t.Fatalf("held message %d %s was kept at sequence %d, before the handler's last keep at %d:\n%s", i, item.EventID, sequence, lastHandlerKeep, logs)
		}
	}
	for i, item := range published {
		kept := linesIn(logs, "listener kept role message for the role's next holder", map[string]any{"role": role, "event_id": item.EventID})
		if len(kept) != 1 || kept[0]["reason"] != string(roleKeptAtShutdown) {
			t.Fatalf("message %d %s: kept lines %v, want one naming %s:\n%s", i, item.EventID, kept, roleKeptAtShutdown, logs)
		}
	}
	if stored := storedOnSubjects(t, harness.client, bus.KeptRoleSubject(">"), role); stored != messages {
		t.Fatalf("the role's kept lane holds %d copies, want %d", stored, messages)
	}
}

// A drain that runs out with role messages still in the lane's subscription logs at ERROR how many
// its handler had not finished, the one it is on included (nats.go's Subscription.Pending): the
// close drops each one it had not taken, and the lane held the only copy.
func TestRoleLaneDrainLogsTheMessagesItLeavesUntaken(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var taken atomic.Int32
	lane := laneOnHarness(t, harness, func(*natsgo.Msg) {
		taken.Add(1)
		<-release
	}, newRoleForwarderLimited(harness.config, 1))
	publisher := testnats.Connect(t, harness.client.Conn.ConnectedUrl())
	t.Cleanup(publisher.Close)
	const messages = 5
	for i := range messages {
		publishRoleMessage(t, publisher, "drain-untaken", fmt.Sprintf("untaken %d", i))
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the role messages: %v", err)
	}
	waitFor(t, 20*time.Second, "the handler to block on the first message", func() bool { return taken.Load() == 1 })
	waitFor(t, 20*time.Second, "the lane's subscription to hold the rest", func() bool {
		return lane.client.Conn.Stats().InMsgs >= messages
	})

	lane.drain(harness.config.logger, time.Second)

	undrained := linesIn(harness.logs.String(), roleLaneUndrainedLine, nil)
	if len(undrained) != 1 || undrained[0]["pending"] != float64(messages) {
		t.Fatalf("the drain's ERROR lines are %v, want one naming %d pending:\n%s", undrained, messages, harness.logs.String())
	}
}

// A message the lane holds when the listener stops makes no keep at the stop: it parks, and keeps
// only once the lane's own client has drained (keepHeldNow). That is what leaves the lane's
// handler the window to keep the messages its subscription still holds, which go over the same
// main connection: a listener holding its bound has up to 4,096 held messages against them.
func TestAHeldRoleMessageIsKeptOnlyOnceTheLaneHasDrained(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "parked-until-drained"
	if err := harness.sessions.Put("ses_parked", session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole("ses_parked", "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	cfg := harness.config
	cfg.forwardRole = inlineForward(func(string, contracts.Envelope, string, time.Duration) error {
		return errors.New("nats: connection closed")
	})
	forwarder := newRoleForwarderLimited(cfg, 1)
	cfg.roleForwards = forwarder
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "parked-until-drained")
	item.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	waitFor(t, 20*time.Second, "the message to be held", func() bool {
		return countLogged(harness.logs.String(), roleHeldLine, item.EventID) >= 1
	})
	forwarder.stopHolding()

	// The stop alone keeps nothing: the message parks with its slot, and says so, and the kept lane
	// stays empty while the lane's handler would be keeping what its subscription holds.
	waitFor(t, 20*time.Second, "the held message to park", func() bool {
		return countLogged(harness.logs.String(), roleParkedLine, item.EventID) == 1
	})
	for i := range 20 {
		if kept := countLogged(harness.logs.String(), "listener kept role message for the role's next holder", item.EventID); kept != 0 {
			t.Fatalf("the held message was kept %d times %d ms after the stop, before the lane drained:\n%s", kept, i*10, harness.logs.String())
		}
		if outstanding := forwarder.outstanding(); outstanding != 1 {
			t.Fatalf("%d role messages outstanding after the stop, want the 1 parked", outstanding)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertNothingKept(t, harness.client, role)

	forwarder.keepHeldNow()
	forwarder.settled(t)

	kept := linesIn(harness.logs.String(), "listener kept role message for the role's next holder", map[string]any{"event_id": item.EventID})
	if len(kept) != 1 || kept[0]["reason"] != string(roleKeptAtShutdown) {
		t.Fatalf("the kept lines for %s are %v, want one naming %s:\n%s", item.EventID, kept, roleKeptAtShutdown, harness.logs.String())
	}
	if stored := storedOnSubjects(t, harness.client, bus.KeptRoleSubject(">"), role); stored != 1 {
		t.Fatalf("the role's kept lane holds %d copies, want 1", stored)
	}
}
