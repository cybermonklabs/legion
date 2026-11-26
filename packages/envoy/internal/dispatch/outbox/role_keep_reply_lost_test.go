package outbox

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// TestAKeptCopyWhoseFirstLaneReadReplyIsLostStillPrecedesTheIssuesNextEvent proves a role handover
// whose stored-message read loses one reply still moves the kept copy, and in order: the proxy
// discards the first stored-message reply after the server sends it, the read is sent again inside
// this batch's bound, and the holder reads the kept copy before the issue's next event.
func TestAKeptCopyWhoseFirstLaneReadReplyIsLostStillPrecedesTheIssuesNextEvent(t *testing.T) {
	t.Parallel()
	const role, holder, key = "kept-lost-lane-reply", "ses_kept_lost_lane_reply", "T-1"
	_, uri := testnats.Start(t)
	armed := make(chan struct{})
	proxy := testnats.StartProxy(t, uri, testnats.DropFirstReplyForRequests("$JS.API.STREAM.MSG.GET."+bus.Stream, armed))
	client, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithKeepInterval(keeperHeld))
	if err != nil {
		t.Fatalf("connect the outbox client through the reply-dropping proxy: %v", err)
	}
	t.Cleanup(client.Close)
	rig := newRoleRigOn(t, proxy.URL(), client, testnats.Connect(t, uri))
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder)
	kept := rig.routedEvent(t, key, role, model.Actor{Kind: "user", ID: "alice"})
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, holder, session.SessionEntry{SelfSubscribed: true})
	})
	outbox := holding(t, rig.publisherOn(publisher))
	close(armed)

	outbox.scan(context.Background(), unfenced{})
	if err := testwait.Receive(t, "the claim made under the keep", claimed); err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	if publishedAt(t, rig.database, kept.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, kept.ID)
		t.Fatalf("the kept event was not published before the later event: attempt_count %d, publish_error %v", attempts, failure)
	}
	if dropped := proxy.DroppedReplies(); dropped != 1 {
		t.Fatalf("the kept-lane reply drop fired %d times, want once", dropped)
	}

	keptKey := fmt.Sprintf("dispatch-%d", kept.ID)
	next := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "8f09091c-ae9b-47c3-a7a3-4375b4281dfe", IssueKey: new(key), Body: "the issue's next event"},
	})
	outbox.scan(context.Background(), unfenced{})
	nextKey := fmt.Sprintf("dispatch-%d", next.ID)
	waitFor(t, "the kept copy and the issue's next event at the role holder after its first lane-read reply was lost", func() bool {
		return len(recv.received()) == 2
	})
	received := recv.received()
	if received[0].EventID != keptKey || received[1].EventID != nextKey {
		t.Fatalf("the holder received forwards %v, want the kept copy %q before next event %q", eventIDs(received), keptKey, nextKey)
	}
}
