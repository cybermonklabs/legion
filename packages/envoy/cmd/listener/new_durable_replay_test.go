package main

// Check l24 of dispatch://INFRA-3589: a listener whose durable is created new does not replay the
// stream. It reports no wake and forwards no role message for a message stored before it started,
// and a listener whose durable exists resumes from that durable's cursor.
//
// This file uses only what the listener package had at the commit before that change, and names
// each log line by its text, so it compiles against that commit and fails there on its
// assertions.

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	// replayMachine is the machine id the listener runs under. The rig's NATS server is the
	// test's own, so no durable of that machine exists before the first start.
	replayMachine = "l24-new"
	// replayRole is the role the rig's holder claims, and replayHolder that holder.
	replayRole   = "l24-role"
	replayHolder = "ses_l24_holder"
	// replayWatch is how long after a listener is healthy a row watches for a wake or a forward
	// that must not come: the wait, the redelivery and the report's bound (aloneLatest), and a
	// margin.
	replayWatch = aloneLatest + 2*time.Second
)

// replayRig is a NATS server of the test's own, a listener binary built from this tree, a bus
// client of the test's own, and a reader of every agent wake report the stream stores.
type replayRig struct {
	uri       string
	binary    string
	publisher *bus.Client

	mu    sync.Mutex
	wakes []aloneWake
}

// startReplayRig starts the rig with no listener running. The publisher owns the stream, so a
// message can be stored before the first listener starts.
func startReplayRig(t *testing.T) *replayRig {
	t.Helper()
	_, uri := testnats.Start(t)
	publisher, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the publisher and create the stream: %v", err)
	}
	t.Cleanup(publisher.Close)
	rig := &replayRig{uri: uri, binary: cmdtest.Build(t, "envoy-listener"), publisher: publisher}
	wakes, err := publisher.JS().Subscribe(bus.LegionWakeTopic, func(msg *natsgo.Msg) {
		meta, err := msg.Metadata()
		if err != nil {
			return
		}
		var item contracts.Envelope
		var report wakeReport
		if json.Unmarshal(msg.Data, &item) != nil || json.Unmarshal([]byte(item.Payload), &report) != nil || report.Kind != wakeKindAgent {
			return
		}
		rig.mu.Lock()
		rig.wakes = append(rig.wakes, aloneWake{session: report.Session, stored: meta.Timestamp})
		rig.mu.Unlock()
	}, natsgo.OrderedConsumer(), natsgo.DeliverAll())
	if err != nil {
		t.Fatalf("read the wake reports the stream stores: %v", err)
	}
	t.Cleanup(func() { _ = wakes.Unsubscribe() })
	if err := publisher.Conn.Flush(); err != nil {
		t.Fatalf("flush the wake reader: %v", err)
	}
	return rig
}

// start starts a listener under replayMachine with a wait of aloneWindow and waits until it is
// healthy, which it is only once its durable is bound. It returns the listener and the time it
// became healthy.
func (r *replayRig) start(t *testing.T, name string) (*listenerProcess, time.Time) {
	t.Helper()
	listener := startListenerProcess(t, r.binary, r.uri, replayMachine, "ENVOY_UNPLACEABLE_RETRY_WINDOW=2s")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("%s's output:\n%s", name, listener.Output.String())
		}
	})
	listener.waitHealthy(t)
	return listener, time.Now()
}

// storeAgent stores a Dispatch envelope on sessionID's agent subject and returns it with its
// stream sequence and the time the stream stored it.
func (r *replayRig) storeAgent(t *testing.T, sessionID string) (contracts.Envelope, uint64, time.Time) {
	t.Helper()
	item := newEnvelope("dispatch", contracts.LegacyAgentSubject(sessionID), "a comment for "+sessionID)
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("encode the message for %s: %v", sessionID, err)
	}
	ack, err := r.publisher.JS().Publish(item.Topic, data)
	if err != nil {
		t.Fatalf("store the message for %s: %v", sessionID, err)
	}
	stored, err := r.publisher.JS().GetMsg(bus.Stream, ack.Sequence)
	if err != nil {
		t.Fatalf("read back the stored message for %s: %v", sessionID, err)
	}
	return item, ack.Sequence, stored.Time
}

// replayRoleMessage is a message to replayRole from another session.
func replayRoleMessage(name string) contracts.Envelope {
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+replayRole, "publish.l24-"+name+"-"+id.New())
	item.SourceSession = "ses_l24_sender"
	return item
}

// storeRoleForward stores item as the stream holds a role message: the copy a forward to
// replayHolder left on the holder's agent subject, under the forward's own key and message id. No
// stream subject captures a role lane, and a start moves any subject that does off the stream, so
// this copy is the only form in which the stream holds a role message.
func (r *replayRig) storeRoleForward(t *testing.T, item contracts.Envelope) {
	t.Helper()
	forward := roleForward{item: item, sessionID: replayHolder}
	subject := contracts.LegacyAgentSubject(replayHolder)
	data, err := json.Marshal(forward.forwarded())
	if err != nil {
		t.Fatalf("encode the forward of %s: %v", item.EventID, err)
	}
	if _, err := r.publisher.JS().Publish(subject, data, natsgo.MsgId(forward.msgID(subject))); err != nil {
		t.Fatalf("store the forward of %s on %s: %v", item.EventID, subject, err)
	}
}

// wakesFor is every agent wake report the stream stored for sessionID so far.
func (r *replayRig) wakesFor(sessionID string) []aloneWake {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found []aloneWake
	for _, wake := range r.wakes {
		if wake.session == sessionID {
			found = append(found, wake)
		}
	}
	return found
}

// awaitWakes waits until the stream holds n wake reports for sessionID or deadline passes, and
// then half a second more, so a report past the n it waited for is counted too.
func (r *replayRig) awaitWakes(sessionID string, n int, deadline time.Time) []aloneWake {
	for len(r.wakesFor(sessionID)) < n && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	return r.wakesFor(sessionID)
}

// roleForwards counts the lines in which listener says it forwarded the role message eventID.
func roleForwards(listener *listenerProcess, eventID string) int {
	return countRecords(listener, "listener role forwarded", func(record map[string]any) bool {
		return record["event_id"] == eventID
	})
}

// keptForwards counts the lines of roleForwards that forward a kept copy of eventID: the kept
// lane's walk writes kept_at on its line (finishRoleForward), and no other forward does.
func keptForwards(listener *listenerProcess, eventID string) int {
	return countRecords(listener, "listener role forwarded", func(record map[string]any) bool {
		_, kept := record["kept_at"]
		return record["event_id"] == eventID && kept
	})
}

// relayHandovers counts the lines in which listener says its session relay copied the role
// message eventID, a frame of sessionID's own consumer, to the role's kept lane, because the
// role's claim named another session, or none, when the relay read the frame (takeOverFrame).
func relayHandovers(listener *listenerProcess, sessionID, eventID string) int {
	return countRecords(listener, "listener session relay copied a role message to the role's kept lane: another session holds the role, or none does", func(record map[string]any) bool {
		return record["session_id"] == sessionID && record["event_id"] == eventID
	})
}

// deliveriesOf counts the frames carrying eventID that standIn has received so far, reading until
// no frame arrives for quiet.
func deliveriesOf(standIn *sessionStandIn, eventID string, quiet time.Duration) int {
	delivered := 0
	for {
		select {
		case frame := <-standIn.frames:
			if frame.item.EventID == eventID {
				delivered++
			}
		case <-time.After(quiet):
			return delivered
		}
	}
}

// durableInfo reads the machine's durable.
func (r *replayRig) durableInfo(t *testing.T) *natsgo.ConsumerInfo {
	t.Helper()
	info, err := r.publisher.JS().ConsumerInfo(bus.Stream, bus.ListenerDurable(replayMachine))
	if err != nil {
		t.Fatalf("read the durable of %s: %v", replayMachine, err)
	}
	return info
}

// deliverPolicyName is policy as the server's API writes it, such as "new" or "all".
func deliverPolicyName(policy natsgo.DeliverPolicy) string {
	name, err := policy.MarshalJSON()
	if err != nil {
		return err.Error()
	}
	return string(name)
}

// TestANewListenerDurableDoesNotReplayTheStream is check l24. Its rows run in order on one rig,
// each on what the row before it left.
//
// Row a stores an agent message for a session with no record and a role message, and only then
// starts a listener under a machine id with no durable. The listener creates its durable, and the
// durable, read as soon as the listener is healthy, has delivered or holds no more messages than
// the stream stored after the two, though its filter takes both. Within the wait and a margin the
// listener reports no wake for the agent message and its handler never takes it. The role
// message is stored on its holder's own subject, so the holder receives it through its own
// consumer, by one of two paths.
// The relay sends the frame when the claim it reads names the holder. When the claim names no
// session, the relay copies the frame to the role's kept lane in place of sending it, and the kept
// lane's walk forwards that copy to the holder (takeOverFrame). The row takes either path, and
// fails on two deliveries or on none within its window: it counts what the holder received until
// a second has passed with no frame (deliveriesOf). That is one delivery in the window, not
// exactly once: the hand-over is at least once. The relay copies the frame and then acknowledges
// it, two steps, so a listener that stops between them, or an acknowledgement that fails, leaves
// the frame to be copied again, and the holder then receives a second frame of the same event.
// A durable that took the stored role message would write no line for it: its handler
// acknowledges a stored forward copy before it logs anything (listenerDeliveryHandler), so the
// durable's own count is what shows it took neither message.
// Row b stores an agent message for another session with no record and sends a role message to
// the role's holder after the start: the listener reports the wake after the wait and forwards the
// role message, as any listener does, and its handler's "listener received" line for the agent
// message is there, which is row a's control. Row c stops the listener, stores an agent message
// while no listener runs, and starts a listener under the same machine id: it binds the durable it
// finds, which keeps its cursor and its start, and reports the wake of that message, whose
// "listener received" line is there, and takes no agent message its durable had already
// acknowledged.
func TestANewListenerDurableDoesNotReplayTheStream(t *testing.T) {
	// Not parallel: it holds a wall-clock bound within a second or two of what it waits for: a wake
	// stored inside 2 s past the latest time the agent wait allows (aloneLatest).
	rig := startReplayRig(t)
	stale := "ses_l24_stale_" + id.New()
	staleItem, _, _ := rig.storeAgent(t, stale)
	staleRole := replayRoleMessage("before")
	rig.storeRoleForward(t, staleRole)
	before, err := rig.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream before the listener starts: %v", err)
	}

	listener, healthy := rig.start(t, "the new listener")
	// The durable before the holder registers. One that replayed the stream has delivered the two
	// messages stored before the start, or holds them to deliver, beside what was stored since. One
	// created at deliver policy new has delivered or holds at most what was stored since, such as
	// the wake report of kind listener_ready the listener publishes once it is ready. The stream
	// is read after the durable, so it holds every message the durable could have taken.
	info := rig.durableInfo(t)
	after, err := rig.publisher.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream once the listener is healthy: %v", err)
	}
	if taken, since := info.Delivered.Consumer+info.NumPending, after.State.LastSeq-before.State.LastSeq; taken > since {
		t.Errorf("the durable the new listener created has delivered %d messages and holds %d to deliver, %d in all, where the stream stored %d after the two stored before its start; want no more than those",
			info.Delivered.Consumer, info.NumPending, taken, since)
	}
	// The holder answers each frame done, as a plugin in consumer delivery does, so its relay
	// sends a frame once and the row counts what the holder received.
	holder := startSessionStandIn(t, rig.uri, replayHolder, answerDone)
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(replayHolder, "epoch-1"))
	postListener(t, listener.port, "/v1/roles/set", `{"session_id":"`+replayHolder+`","role":"`+replayRole+`"}`)

	t.Run("a: a listener under a new machine id takes nothing stored before its start", func(t *testing.T) {
		time.Sleep(time.Until(healthy.Add(replayWatch)))
		if got := countRecords(listener, "listener agent wake reported", func(record map[string]any) bool {
			return record["session_id"] == stale
		}); got != 0 {
			t.Errorf("the new listener logged listener agent wake reported %d times for %s, whose message was stored before it started, want never", got, stale)
		}
		if wakes := rig.wakesFor(stale); len(wakes) != 0 {
			t.Errorf("the stream holds %d wake reports for %s, whose message was stored before the listener started, want none", len(wakes), stale)
		}
		if got := aloneReceived(listener, staleItem.EventID); got != 0 {
			t.Errorf("the new listener's durable handed it the message stored before its start %d times, want never", got)
		}
		// The holder's own consumer starts at the start of the holder's subject, so its relay
		// reads the forward stored there before this listener started, as it reads every message
		// stored for the session. A relay that reads that frame after the claim below lands sends
		// it to the holder. A relay that reads it before the claim lands copies it to the role's
		// kept lane in place of sending it, and logs the hand-over (takeOverFrame). The kept lane's
		// walk then forwards the copy to the role's holder, which is the same session, and the
		// holder's relay sends that forward. That is the session's own consumer at work: the kept
		// lane forwards the message at most once, and only a copy the holder's relay made.
		delivered := deliveriesOf(holder, staleRole.EventID, time.Second)
		kept := keptForwards(listener, staleRole.EventID)
		handed := relayHandovers(listener, replayHolder, staleRole.EventID)
		if kept > 1 || (kept == 0) != (handed == 0) {
			t.Errorf("the new listener forwarded the role message stored before its start %d times from the kept lane, where the holder's relay copied it %d times; want once for a copy the relay made, else never", kept, handed)
		}
		if delivered != 1 {
			t.Errorf("the holder received the role message stored before the listener's start %d times within the row's window (the relay copied it to the kept lane %d times, the kept lane forwarded it %d times); want once", delivered, handed, kept)
		}
		t.Logf("the holder received the role message stored before the listener's start %d time(s): the relay copied it to the kept lane %d time(s) and the kept lane forwarded it %d time(s)", delivered, handed, kept)
		if info := rig.durableInfo(t); info.Config.DeliverPolicy != natsgo.DeliverNewPolicy {
			t.Errorf("the durable the new listener created has deliver policy %s, want \"new\"", deliverPolicyName(info.Config.DeliverPolicy))
		}
	})

	fresh := "ses_l24_fresh_" + id.New()
	freshRole := replayRoleMessage("after")
	var freshItem contracts.Envelope
	t.Run("b: a message stored after the start is woken or forwarded as today", func(t *testing.T) {
		item, seq, stored := rig.storeAgent(t, fresh)
		freshItem = item
		requireWakes(t, rig.awaitWakes(fresh, 1, stored.Add(aloneLatest+2*time.Second)), 1, stored)
		requireOneWakeLine(t, "the new listener", listener, item, fresh)
		if got := aloneReceived(listener, item.EventID); got < 1 {
			t.Errorf("control: the new listener's handler took the message stored after its start %d times, want at least once", got)
		}
		if info := rig.durableInfo(t); info.Delivered.Consumer == 0 {
			t.Errorf("control: the durable has delivered no message, though the listener reported the wake of one stored after its start")
		}

		publish(t, rig.publisher, freshRole)
		waitFor(t, "the new listener to forward the role message sent after its start", func() bool {
			return roleForwards(listener, freshRole.EventID) == 1
		})
		copied, err := rig.publisher.JS().GetLastMsg(bus.Stream, contracts.LegacyAgentSubject(replayHolder))
		if err != nil {
			t.Fatalf("read the forward stored for %s: %v", replayHolder, err)
		}
		var forwarded contracts.Envelope
		if err := json.Unmarshal(copied.Data, &forwarded); err != nil || forwarded.EventID != freshRole.EventID {
			t.Errorf("the stream's last message for %s is %s, want the forward of %s", replayHolder, copied.Data, freshRole.EventID)
		}
		awaitDurableState(t, rig.publisher, bus.ListenerDurable(replayMachine), 0, seq)
	})

	t.Run("c: a listener whose durable exists resumes from its cursor", func(t *testing.T) {
		before := rig.durableInfo(t)
		listener.Terminate(t)
		listener.WaitExit(t, "SIGTERM")
		resumed := "ses_l24_resumed_" + id.New()
		item, seq, _ := rig.storeAgent(t, resumed)

		restarted, healthy := rig.start(t, "the restarted listener")
		if wakes := rig.awaitWakes(resumed, 1, healthy.Add(replayWatch)); len(wakes) != 1 {
			t.Errorf("the stream holds %d wake reports for %s, whose message was stored while the durable's listener was stopped, want one", len(wakes), resumed)
		}
		requireOneWakeLine(t, "the restarted listener", restarted, item, resumed)
		if got := aloneReceived(restarted, item.EventID); got < 1 {
			t.Errorf("control: the restarted listener's handler took the message stored while it was stopped %d times, want at least once", got)
		}
		awaitDurableState(t, rig.publisher, bus.ListenerDurable(replayMachine), 0, seq)

		after := rig.durableInfo(t)
		if !after.Created.Equal(before.Created) {
			t.Errorf("the durable was created again at %s, want the one created at %s", after.Created, before.Created)
		}
		if after.Config.DeliverPolicy != before.Config.DeliverPolicy {
			t.Errorf("the restart moved the durable's deliver policy from %s to %s, want it kept", deliverPolicyName(before.Config.DeliverPolicy), deliverPolicyName(after.Config.DeliverPolicy))
		}
		for _, acknowledged := range []string{staleItem.EventID, freshItem.EventID} {
			if got := aloneReceived(restarted, acknowledged); got != 0 {
				t.Errorf("the restarted listener took event %s %d times, which its durable had passed already, want never", acknowledged, got)
			}
		}
		if got := countRecords(restarted, "listener agent wake reported", func(record map[string]any) bool {
			return record["session_id"] == fresh || record["session_id"] == stale
		}); got != 0 {
			t.Errorf("the restarted listener reported %d wakes for messages its durable had passed already, want none", got)
		}
	})
}
