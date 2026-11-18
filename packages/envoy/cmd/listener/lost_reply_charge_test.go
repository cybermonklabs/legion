package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// heldListenerClient is a listener's client on the package's shared server through a hold of its
// own (testnats.ReplyHold), owning the stream as the listener does.
func heldListenerClient(t *testing.T) (*bus.Client, *testnats.ReplyHold) {
	t.Helper()
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect the listener through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	return client, hold
}

// dropWhenWithheld drops every connection through hold once it has withheld the first reply held
// names (testnats.ReplyHold.Drop), and reports on the returned channel whether it did, by the
// test's deadline. It runs off the test's goroutine, which makes the request the hold withholds.
func dropWhenWithheld(t *testing.T, hold *testnats.ReplyHold, held *testnats.HeldReplies) <-chan bool {
	dropped := make(chan bool, 1)
	go func() {
		select {
		case <-held.Withheld():
			hold.Drop()
			dropped <- true
		case <-time.After(testnats.Remaining(t)):
			dropped <- false
		}
	}()
	return dropped
}

// A topic publish whose acknowledgement the connection's drop lost costs its sender one, though
// the route answers 503: the bus sends a publish whose caller reads the duplicate verdict once
// (bus.Client.PublishAckWithin), so the loss is the route's answer, and its error wraps
// bus.ErrUnacknowledged, since the stream may hold the copy (sendUnanswered counts
// bus.ErrConnectionLost), which senderCharge.published charges. The hold withholds the stream's
// acknowledgement, so the publish reached the stream, and then drops the connection
// (testnats.ReplyHold.Drop): the stream holds the message the route answered 503 for, which an
// observer of its own reads, the listener's connection being down until it reconnects.
//
// The control is this row with bus.ErrConnectionLost taken out of sendUnanswered: the 503 costs
// nothing, though the stream holds the message, and the sender's next message is answered 200.
func TestATopicPublishWhoseAckTheDropLostCostsOne(t *testing.T) {
	client, hold := heldListenerClient(t)
	rig := newSenderLimitRigOn(t, client, testnats.URL(t))
	rig.limit(4, 1)
	const sender = "ses_cost_publish_lost"
	topic := "notifications.team.sender-limit-lost-" + id.New()

	dropped := dropWhenWithheld(t, hold, hold.Hold(topic))
	lost := rig.publishFrom(t, sender, topic, nil)
	if !<-dropped {
		t.Fatalf("the hold withheld no acknowledgement of the publish, so no drop took it; body %s", lost.raw)
	}
	lost.requireStatus(t, "the publish whose acknowledgement the drop lost", http.StatusServiceUnavailable)
	lost.requireOutcome(t, "the publish whose acknowledgement the drop lost", outcomeBusUnavailable)
	lost.requireNamed(t, "the publish whose acknowledgement the drop lost", sender)
	observer, err := testnats.Connect(t, testnats.URL(t)).JetStream()
	if err != nil {
		t.Fatalf("open the observer's JetStream context: %v", err)
	}
	waitFor(t, testnats.Remaining(t), "the stream to hold the publish whose acknowledgement the drop lost", func() bool {
		info, err := observer.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: topic})
		return err == nil && info.State.Subjects[topic] == 1
	})
	waitFor(t, testnats.Remaining(t), "the listener's client to reconnect through the hold", client.Connected)
	rig.requireCount(t, sender, false)
}

// A topic publish no sender limit counts, whose acknowledgement the connection's drop lost,
// answers 200 naming the stored copy's event, as the route answered it before this change: the
// route reads no duplicate verdict for it, so it publishes it through PublishWithin, which the bus
// sends again on the new connection after the loss (sendStored), and the stream, which holds the
// first send's copy under the message's id, answers the second as that copy's duplicate. So the
// stream holds one copy, and the answer's event_id is that copy's. That holds for each id form: a
// key the route mints for the request (publish.<id>), which names its event and is the id, and a
// dedupe_key the caller chose that names no event (caller.<name>), whose id is the envelope's own
// event id (bus.envelopeMsgID). The hold withholds the first send's acknowledgement, so that send
// reached the stream, and then drops the connection, which the client reconnects through
// (testnats.ReplyHold.Drop): the second send is answered inside the route's own budget
// (sendRequestBudget) once the client has reconnected, about a second after the drop.
//
// The controls: this row with the route publishing every message through PublishAckWithin, as it
// publishes one its sender limit counts, where the publish is sent once and answers 503; and the
// caller-key row with envelopeMsgID giving such a key no id, where the bus sends the publish once,
// as it sends every write with no id (sendStored), so the route answers 503 and the stream holds
// the one copy the first send stored.
func TestATopicPublishNoLimitCountsAnswersItsStoredCopyAcrossALostAck(t *testing.T) {
	for _, row := range []struct {
		name   string
		fields map[string]any
	}{
		{"under the key the route mints", nil},
		{"under a dedupe_key that names no event", map[string]any{"dedupe_key": "caller.uncounted-" + id.New()}},
	} {
		t.Run(row.name, func(t *testing.T) {
			client, hold := heldListenerClient(t)
			rig := newSenderLimitRigOn(t, client, testnats.URL(t))
			if rig.deps.senderLimit != nil {
				t.Fatal("the rig counts senders, want no sender limit")
			}
			const sender = "ses_uncounted_publish_lost"
			topic := "notifications.team.uncounted-lost-" + id.New()

			dropped := dropWhenWithheld(t, hold, hold.Hold(topic))
			answer := rig.publishFrom(t, sender, topic, row.fields)
			if !<-dropped {
				t.Fatalf("the hold withheld no acknowledgement of the publish, so no drop took it; body %s", answer.raw)
			}
			answer.requireStatus(t, "the uncounted publish whose acknowledgement the drop lost", http.StatusOK)
			var published contracts.Envelope
			if err := json.Unmarshal([]byte(answer.raw), &published); err != nil || published.EventID == "" {
				t.Fatalf("the publish's answer %s names no event (%v)", answer.raw, err)
			}
			observer, err := testnats.Connect(t, testnats.URL(t)).JetStream()
			if err != nil {
				t.Fatalf("open the observer's JetStream context: %v", err)
			}
			info, err := observer.StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: topic})
			if err != nil {
				t.Fatalf("read what the stream holds on %s: %v", topic, err)
			}
			if held := info.State.Subjects[topic]; held != 1 {
				t.Fatalf("the stream holds %d messages on %s, want the one copy of the publish sent twice", held, topic)
			}
			last, err := observer.GetLastMsg(bus.Stream, topic)
			if err != nil {
				t.Fatalf("read the stored copy on %s: %v", topic, err)
			}
			var stored contracts.Envelope
			if err := json.Unmarshal(last.Data, &stored); err != nil {
				t.Fatalf("decode the stored copy: %v", err)
			}
			if stored.EventID != published.EventID {
				t.Fatalf("the 200 names event %s, and the stream's one copy is event %s", published.EventID, stored.EventID)
			}
		})
	}
}

// A keep whose acknowledgement the connection's drop lost takes the copy its own first send stored
// as kept, and not as a repeat (storeKeptRoleCopy's ownSend): the bus sends the keep's publish
// once and answers bus.ErrConnectionLost wrapped in bus.ErrUnacknowledged (PublishStoredAck), the
// keep publishes again under the same id once the connection is back (bus.Reconnected), and the
// stream's duplicate answer names the first send's copy, which carries the keep's own event id. A
// repeat is what the publish route's sender limit charges nothing for (roleSend.kept), so a repeat
// here would let a message the keep itself stored cost its sender nothing.
//
// The control is this row with ownSend left unmarked by bus.ErrUnacknowledged: the keep answers
// its own copy as a repeat.
func TestAKeepWhoseAckTheDropLostTakesItsOwnCopyAsNoRepeat(t *testing.T) {
	client, hold := heldListenerClient(t)
	const role = "lost-keep-own-role"
	cfg := listenerDeliveryHandlerConfig{client: client, logger: logging.NewWithWriter("test", &cmdtest.LockedBuffer{}), now: time.Now}
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "lost-keep-own-00")

	dropped := dropWhenWithheld(t, hold, hold.Hold(bus.KeptRoleSubject(role)))
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t)-5*time.Second)
	defer cancel()
	kept, err := storeKeptRoleCopy(ctx, cfg, role, item, client.PublishStoredAck)
	if !<-dropped {
		t.Fatalf("the hold withheld no acknowledgement of the keep's publish, so no drop took it (keep: %+v, %v)", kept, err)
	}
	if err != nil {
		t.Fatalf("the keep whose acknowledgement the drop lost answered %v, want its own stored copy kept", err)
	}
	if kept.item.EventID != item.EventID {
		t.Fatalf("the keep answered the copy of event %s, want its own first send's copy of event %s", kept.item.EventID, item.EventID)
	}
	if kept.repeat {
		t.Fatalf("the keep answered its own first send's copy at sequence %d as a repeat, which the sender limit charges nothing for", kept.sequence)
	}
}

// A keep whose acknowledgement the connection's drop lost, its send a duplicate of a copy another
// send of the same dedupe key stored before it, takes that copy as a repeat, as it does where no
// drop came: its own send may have stored a copy (bus.ErrUnacknowledged), but the copy the stream
// names carries another event id, so its own send was dropped as that copy's duplicate and stored
// nothing (storeKeptRoleCopy). The sender limit charges nothing for such a repeat (roleSend.kept).
//
// The control is this row with the event id left out of the judgement: the keep takes the other
// send's copy as its own, and its sender is charged for a message the lane already held.
func TestAKeepWhoseAckTheDropLostTakesAnotherSendsCopyAsARepeat(t *testing.T) {
	client, hold := heldListenerClient(t)
	const role = "lost-keep-other-role"
	cfg := listenerDeliveryHandlerConfig{client: client, logger: logging.NewWithWriter("test", &cmdtest.LockedBuffer{}), now: time.Now}
	earlier := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "lost-keep-other-00")
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "lost-keep-other-00")
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t)-5*time.Second)
	defer cancel()
	stored, err := storeKeptRoleCopy(ctx, cfg, role, earlier, client.PublishStoredAck)
	if err != nil || stored.repeat {
		t.Fatalf("the earlier send's keep answered %+v, %v, want a new copy", stored, err)
	}

	dropped := dropWhenWithheld(t, hold, hold.Hold(bus.KeptRoleSubject(role)))
	kept, err := storeKeptRoleCopy(ctx, cfg, role, item, client.PublishStoredAck)
	if !<-dropped {
		t.Fatalf("the hold withheld no acknowledgement of the keep's publish, so no drop took it (keep: %+v, %v)", kept, err)
	}
	if err != nil {
		t.Fatalf("the keep whose acknowledgement the drop lost answered %v, want the earlier copy answered as a repeat", err)
	}
	if kept.item.EventID != earlier.EventID || kept.sequence != stored.sequence {
		t.Fatalf("the keep answered the copy of event %s at %d, want the earlier send's copy of event %s at %d",
			kept.item.EventID, kept.sequence, earlier.EventID, stored.sequence)
	}
	if !kept.repeat {
		t.Fatalf("the keep answered the earlier send's copy at sequence %d as its own, which the sender limit charges for", kept.sequence)
	}
}
