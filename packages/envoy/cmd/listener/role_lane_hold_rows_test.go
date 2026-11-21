package main

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// These rows name no symbol this change added, so each builds against the forwarder before it and
// fails there for the reason it names.

// A forward still queued behind its holder's worker when the listener stops is not made: it parks
// with its slot, as a held message does, and is kept on its role's kept lane once the lane has
// drained (keepHeldNow). The worker's first forward blocks in its publish, so the other four queue
// behind it; the stop comes, the first forward ends, and the worker takes the four from its queue
// with the forwarder stopped. Before the keep is released the kept lane is empty and only the
// first was forwarded; after it, each of the four is kept under held_at_shutdown.
func TestForwardsQueuedBehindAHoldersWorkerAtShutdownAreKeptAfterTheDrain(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role     = "queued-at-shutdown"
		holder   = "ses_queued_at_shutdown"
		messages = 5
	)
	if err := harness.sessions.Put(holder, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register holder: %v", err)
	}
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	release := make(chan struct{})
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
		if forwards.Add(1) == 1 {
			<-release
		}
		return func() error { return nil }, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	handler := coreNATSDeliveryHandler(cfg)
	items := make([]contracts.Envelope, messages)
	for i := range items {
		items[i] = listenerTestEnvelope(contracts.RoleTopicPrefix+role, fmt.Sprintf("queued-at-shutdown-%d", i))
		items[i].Payload = `{"type":"worker-queued"}`
		handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, items[i])})
	}
	queued := func() int {
		forwarder.mu.Lock()
		defer forwarder.mu.Unlock()
		return len(forwarder.queues[holder])
	}
	if !testwait.Holds(20*time.Second, 10*time.Millisecond, func() bool { return forwards.Load() == 1 && queued() == messages-1 }) {
		t.Fatalf("want the first forward in its publish and %d queued behind it; forwards %d, queued %d", messages-1, forwards.Load(), queued())
	}

	forwarder.stopHolding()
	close(release)
	if !testwait.Holds(20*time.Second, 10*time.Millisecond, func() bool {
		forwarder.mu.Lock()
		defer forwarder.mu.Unlock()
		_, running := forwarder.queues[holder]
		return !running
	}) {
		t.Fatalf("the holder's worker did not end; %d still queued", queued())
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the worker made %d forwards, want 1: the %d it took from its queue after the stop must park, not go out", got, messages-1)
	}
	if outstanding := forwarder.outstanding(); outstanding != messages-1 {
		t.Fatalf("%d role messages outstanding before the keep is released, want the %d parked", outstanding, messages-1)
	}
	assertNothingKept(t, harness.client, role)

	forwarder.keepHeldNow()
	forwarder.settled(t)

	logs := harness.logs.String()
	if got := countLogged(logs, "listener role forwarded", items[0].EventID); got != 1 {
		t.Fatalf("the first message was forwarded %d times, want 1:\n%s", got, logs)
	}
	for i, item := range items[1:] {
		kept := linesIn(logs, "listener kept role message for the role's next holder", map[string]any{"event_id": item.EventID})
		if len(kept) != 1 || kept[0]["reason"] != string(roleKeptAtShutdown) {
			t.Fatalf("queued message %d %s: kept lines %v, want one naming %s:\n%s", i+1, item.EventID, kept, roleKeptAtShutdown, logs)
		}
	}
	if stored := storedOnSubjects(t, harness.client, bus.KeptRoleSubject(">"), role); stored != messages-1 {
		t.Fatalf("the role's kept lane holds %d copies, want %d", stored, messages-1)
	}
}

// forwardLosingTheFirstAcknowledgement is the listener's own forward (startRoleForward) to
// client's stream, except that the first forward's acknowledgement is lost: the stream stores that
// forward, then after runs, and the forward reports the error a forward whose acknowledgement never
// came reports, which leaves the stream's copy unknown (roleForwardEnded holds it).
func forwardLosingTheFirstAcknowledgement(client *bus.Client, forwards *atomic.Int32, after func()) func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
	real := startRoleForward(client)
	return func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
		wait, err := real(subject, item, msgID, timeout)
		if err != nil {
			return nil, err
		}
		if forwards.Add(1) > 1 {
			return wait, nil
		}
		return func() error {
			if err := wait(); err != nil {
				return err
			}
			after()
			return fmt.Errorf("%w: the acknowledgement was lost", bus.ErrForwardNotStored)
		}, nil
	}
}

// moveRole hands role from whoever holds it to session to, which a hard claim takes and registers
// as a live session.
func moveRole(t *testing.T, harness listenerDeliveryHarness, role, to string) {
	t.Helper()
	if err := harness.sessions.Put(to, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Errorf("register %s: %v", to, err)
	}
	if _, err := harness.registry.SetRole(to, "test-machine", role, false); err != nil {
		t.Errorf("move %s to %s: %v", role, to, err)
	}
}

// A message whose first forward the stream stored, but whose acknowledgement was lost, is held, and
// the role moves to another live session before the next attempt. The next attempt is that same
// forward again - to the first holder, under the same Nats-Msg-Id - so the stream drops it as a
// duplicate and holds one copy of the message, the first holder's. A delivery that resolved the
// holder again would forward it to the second holder under another subject and another id, and
// the stream would hold two copies of the one message.
func TestARoleMessageWhoseStoredForwardLostItsAcknowledgementIsStoredOnceWhenTheRoleMoves(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "moved-during-a-hold"
		first  = "ses_first_holder"
		second = "ses_second_holder"
	)
	if err := harness.sessions.Put(first, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the first holder: %v", err)
	}
	if _, err := harness.registry.SetRole(first, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = forwardLosingTheFirstAcknowledgement(harness.client, &forwards, func() { moveRole(t, harness, role, second) })
	cfg = wireTestRoleForwarder(t, cfg)
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "moved-during-a-hold")
	item.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	cfg.roleForwards.settled(t)

	if got := forwards.Load(); got != 2 {
		t.Fatalf("the lane made %d forwards, want 2: the one whose acknowledgement was lost, and one more", got)
	}
	if holder, err := harness.registry.RoleHolder(role); err != nil || holder != second {
		t.Fatalf("the role is held by %q (%v), want %s: the move did not land, so the row shows nothing", holder, err, second)
	}
	atFirst := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(first), first)
	atSecond := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(second), second)
	if atFirst+atSecond != 1 || atFirst != 1 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want the one first copy only:\n%s", atFirst, atSecond, harness.logs.String())
	}
}

// A message whose stored forward lost its acknowledgement, and which the listener's stop parks
// before its next attempt, ends with that same forward once more rather than a keep on the role's
// kept lane: the stream drops it as a duplicate, the kept lane stays empty, and the role, which
// moved to another session meanwhile, is handed no second copy by a handover of a kept one.
func TestAMessageWhoseForwardMayBeStoredEndsWithThatForwardAtTheStop(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const (
		role   = "stopped-during-a-hold"
		first  = "ses_first_at_the_stop"
		second = "ses_second_at_the_stop"
	)
	if err := harness.sessions.Put(first, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register the first holder: %v", err)
	}
	if _, err := harness.registry.SetRole(first, "test-machine", role, false); err != nil {
		t.Fatalf("claim role: %v", err)
	}
	var forwarder *roleForwarder
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = forwardLosingTheFirstAcknowledgement(harness.client, &forwards, func() {
		moveRole(t, harness, role, second)
		forwarder.stopHolding()
	})
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder = cfg.roleForwards
	handler := coreNATSDeliveryHandler(cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "stopped-during-a-hold")
	item.Payload = `{"type":"worker-queued"}`

	handler(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	if !testwait.Holds(20*time.Second, 10*time.Millisecond, func() bool { return forwards.Load() == 1 && forwarder.stopped() }) {
		t.Fatal("the first forward and the stop did not happen")
	}
	forwarder.keepHeldNow()
	forwarder.settled(t)

	assertNothingKept(t, harness.client, role)
	atFirst := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(first), first)
	atSecond := storedOnSubjects(t, harness.client, contracts.LegacyAgentSubject(second), second)
	if atFirst != 1 || atSecond != 0 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 1 and 0:\n%s", atFirst, atSecond, harness.logs.String())
	}
	if got := countLogged(harness.logs.String(), "listener role forwarded", item.EventID); got != 1 {
		t.Fatalf("the message was forwarded %d times by its line, want 1: the last forward is acknowledged as the duplicate it is:\n%s", got, harness.logs.String())
	}
}
