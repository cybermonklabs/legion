package main

import (
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testwait"
)

// These rows name no symbol this change added, so each builds against the head before it
// (cdcd3a8d77) and fails there for the reason it names.

// pinRig is one listener's role lane over a delivery harness whose role is claimed by first, a
// live session.
type pinRig struct {
	harness listenerDeliveryHarness
	role    string
	first   string
}

func newPinRig(t *testing.T, role, first string) pinRig {
	t.Helper()
	harness := newListenerDeliveryHarness(t)
	if err := harness.sessions.Put(first, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true}); err != nil {
		t.Fatalf("register %s: %v", first, err)
	}
	if _, err := harness.registry.SetRole(first, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, first, err)
	}
	return pinRig{harness: harness, role: role, first: first}
}

// deleteSession removes sessionID's record from the session bucket, as its unregistration does,
// and waits until this listener's cache no longer reads it live, so the next resolution sees it
// gone as the bucket does.
func (r pinRig) deleteSession(t *testing.T, sessionID string) {
	t.Helper()
	sessions, err := r.harness.client.JS().KeyValue(bus.SessionBucket)
	if err != nil {
		t.Errorf("open the session bucket: %v", err)
		return
	}
	if err := sessions.Delete(sessionID); err != nil {
		t.Errorf("delete %s's session record: %v", sessionID, err)
		return
	}
	if !testwait.Holds(10*time.Second, 10*time.Millisecond, func() bool {
		_, state := r.harness.sessions.Lookup(sessionID)
		return state != session.Live
	}) {
		t.Errorf("the session cache still reads %s live after its record was deleted", sessionID)
	}
}

// deliver hands one role message to the lane through forward and waits until the lane holds
// nothing, returning the message.
func (r pinRig) deliver(t *testing.T, eventID string, forward func(string, contracts.Envelope, string, time.Duration) (func() error, error)) contracts.Envelope {
	t.Helper()
	cfg := r.harness.config
	cfg.forwardRole = forward
	cfg = wireTestRoleForwarder(t, cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+r.role, eventID)
	item.Payload = `{"type":"worker-queued"}`
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	cfg.roleForwards.settled(t)
	return item
}

// storedFor is how many copies the stream holds on sessionID's agent subject.
func (r pinRig) storedFor(t *testing.T, sessionID string) uint64 {
	t.Helper()
	return storedOnSubjects(t, r.harness.client, contracts.LegacyAgentSubject(sessionID), sessionID)
}

// forwardNotSentFirst is the listener's own forward (startRoleForward), except that the first
// forward is made with its window already spent, so it leaves nothing on the wire and fails with
// the window error a forward the process stalled before its publish meets; before runs first.
func forwardNotSentFirst(client *bus.Client, forwards *atomic.Int32, before func()) func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
	real := startRoleForward(client)
	return func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
		if forwards.Add(1) == 1 {
			before()
			return real(subject, item, msgID, 0)
		}
		return real(subject, item, msgID, timeout)
	}
}

// A forward whose window was spent before its publish left nothing on the wire, so the next
// attempt resolves the holder again: the role moved meanwhile to a second session, though the
// first keeps its session record, and the second gets the message. The stream holds no copy for
// the first and one for the second. A lane that pinned the message to the first holder on that
// error forwards it there, where the first holder, which no longer holds the role, gets it.
func TestAForwardThatLeftNothingOnTheWireIsResolvedAgainWhenTheRoleMoves(t *testing.T) {
	const second = "ses_pin_second_alive"
	r := newPinRig(t, "pin-not-sent-moved", "ses_pin_first_alive")
	var forwards atomic.Int32
	r.deliver(t, "pin-not-sent-moved", forwardNotSentFirst(r.harness.client, &forwards, func() {
		moveRole(t, r.harness, r.role, second)
	}))

	if atFirst, atSecond := r.storedFor(t, r.first), r.storedFor(t, second); atFirst != 0 || atSecond != 1 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 0 and 1:\n%s", atFirst, atSecond, r.harness.logs.String())
	}
}

// The same forward, its window spent before the publish, while the first holder's session record
// is deleted and a second session claims the role: the next attempt reaches the second holder.
func TestAForwardThatLeftNothingOnTheWireReachesTheNewHolderWhenTheFirstIsGone(t *testing.T) {
	const second = "ses_pin_second_after_gone"
	r := newPinRig(t, "pin-not-sent-gone", "ses_pin_first_gone")
	var forwards atomic.Int32
	r.deliver(t, "pin-not-sent-gone", forwardNotSentFirst(r.harness.client, &forwards, func() {
		r.deleteSession(t, r.first)
		moveRole(t, r.harness, r.role, second)
	}))

	if atFirst, atSecond := r.storedFor(t, r.first), r.storedFor(t, second); atFirst != 0 || atSecond != 1 {
		t.Fatalf("the stream holds %d copies for the first holder and %d for the second, want 0 and 1:\n%s", atFirst, atSecond, r.harness.logs.String())
	}
}

// A forward the stream stored while its acknowledgement was lost may have been stored, so the
// message is pinned to that holder; but the holder's session record is deleted and a second
// session claims the role before the next attempt. A session with no record has no consumer, so
// the copy on its subject reaches nobody: the pin is dropped, the holder resolved again, and the
// second holder gets the message. A lane that kept the pin forwards to the first holder again,
// where the stream drops it as the duplicate it is, and the message is never read.
func TestAPinnedMessageWhoseHolderIsGoneReachesTheNewHolder(t *testing.T) {
	const second = "ses_pin_second_after_stored"
	r := newPinRig(t, "pin-stored-gone", "ses_pin_first_stored")
	var forwards atomic.Int32
	r.deliver(t, "pin-stored-gone", forwardLosingTheFirstAcknowledgement(r.harness.client, &forwards, func() {
		r.deleteSession(t, r.first)
		moveRole(t, r.harness, r.role, second)
	}))

	if atSecond := r.storedFor(t, second); atSecond != 1 {
		t.Fatalf("the stream holds %d copies for the second holder, want 1: the message reached no session that reads it:\n%s", atSecond, r.harness.logs.String())
	}
	if got := forwards.Load(); got != 2 {
		t.Fatalf("the lane made %d forwards, want 2: the stored one, and one to the second holder", got)
	}
}

// A message pinned to a holder whose session record is then deleted, with no session claiming the
// role after it, is kept on the role's kept lane once, for the role's next live holder, under the
// reason holder_lapsed: the claim still names the first holder, which is not live. A lane that kept
// the pin forwards it to the first holder's subject again and keeps nothing, so no later holder
// ever gets it.
func TestAPinnedMessageWhoseHolderIsGoneWithNoSuccessorIsKept(t *testing.T) {
	r := newPinRig(t, "pin-stored-gone-alone", "ses_pin_first_alone")
	var forwards atomic.Int32
	item := r.deliver(t, "pin-stored-gone-alone", forwardLosingTheFirstAcknowledgement(r.harness.client, &forwards, func() {
		r.deleteSession(t, r.first)
	}))

	kept := linesIn(r.harness.logs.String(), "listener kept role message for the role's next holder", map[string]any{"event_id": item.EventID})
	if len(kept) != 1 || kept[0]["reason"] != string(roleKeptLapsed) {
		t.Fatalf("kept lines %v, want one naming %s:\n%s", kept, roleKeptLapsed, r.harness.logs.String())
	}
	if stored := storedOnSubjects(t, r.harness.client, bus.KeptRoleSubject(">"), r.role); stored != 1 {
		t.Fatalf("the role's kept lane holds %d copies, want 1", stored)
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the first: none to a holder with no session record", got)
	}
}

// The stop's end of a pinned message reads the pinned holder's session record too: the stored
// forward's acknowledgement is lost, the holder's session record is deleted, and the listener
// stops before the next attempt. The message makes no forward to the holder with no record, whose
// copy would reach nobody, and is kept once on the role's kept lane, under held_at_shutdown, for
// the role's next live holder. A lane that kept the pin forwards it to the first holder at the
// stop and keeps nothing.
func TestAPinnedMessageWhoseHolderIsGoneIsKeptAtTheStop(t *testing.T) {
	r := newPinRig(t, "pin-stored-gone-stop", "ses_pin_first_stop")
	var forwards atomic.Int32
	var forwarder *roleForwarder
	cfg := r.harness.config
	cfg.forwardRole = forwardLosingTheFirstAcknowledgement(r.harness.client, &forwards, func() {
		r.deleteSession(t, r.first)
		forwarder.stopHolding()
	})
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder = cfg.roleForwards
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+r.role, "pin-stored-gone-stop")
	item.Payload = `{"type":"worker-queued"}`
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	if !testwait.Holds(20*time.Second, 10*time.Millisecond, func() bool { return forwards.Load() == 1 && forwarder.stopped() }) {
		t.Fatal("the first forward and the stop did not happen")
	}
	forwarder.keepHeldNow()
	forwarder.settled(t)

	kept := linesIn(r.harness.logs.String(), "listener kept role message for the role's next holder", map[string]any{"event_id": item.EventID})
	if len(kept) != 1 || kept[0]["reason"] != string(roleKeptAtShutdown) {
		t.Fatalf("kept lines %v, want one naming %s:\n%s", kept, roleKeptAtShutdown, r.harness.logs.String())
	}
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the first: none to a holder with no session record", got)
	}
}
