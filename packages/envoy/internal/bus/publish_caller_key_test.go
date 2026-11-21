package bus_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A publish under a dedupe key its caller chose, which names no event, whose connection closed
// under it after the stream stored its first send, is sent again on the connection the client dials
// in its place, and the stream keeps one copy: both sends carry one Nats-Msg-Id, the envelope's own
// event id and its topic (envelopeMsgID). The hold withholds the first send's acknowledgement, so
// that send is stored, and then holds nothing more (a Hold on a prefix no publish names), so the
// second send is answered; the row closes the client's connection between the two.
//
// The control is this row at main 6c21aa945d, whose bus gave such a key no message id: its send
// again after the closed connection stores a second copy.
func TestAPublishUnderACallersKeyWhoseConnectionClosedIsStoredOnce(t *testing.T) {
	hold := testnats.StartReplyHold(t, testnats.URL(t))
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect through the hold: %v", err)
	}
	t.Cleanup(client.Close)
	topic := contracts.AgentSubject(contracts.NoProject, "ses_caller_key_"+id.New())
	item := contracts.Envelope{
		EventID:        id.New(),
		Source:         "agent",
		SourceEventID:  id.New(),
		Topic:          topic,
		DedupeKey:      "caller.closed-" + id.New(),
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a publish under a key its caller chose",
	}
	if contracts.DedupeKeyNamesTheUpstreamEvent(item) {
		t.Fatalf("the row's key %q names its event, want one its caller chose", item.DedupeKey)
	}
	held := hold.Hold(topic)
	answered := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t)-5*time.Second)
		defer cancel()
		answered <- client.PublishWithin(ctx, item)
	}()
	select {
	case <-held.Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the hold withheld no acknowledgement of the publish by the test's deadline")
	}
	hold.Hold("envoy.held.nothing.")
	client.Connection().Close()
	select {
	case err = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the publish whose connection closed under it was still waiting at the test's deadline")
	}
	if err != nil {
		t.Fatalf("the publish whose connection closed under it answered %v, want it sent again and acknowledged", err)
	}
	observer, err := testnats.Connect(t, testnats.URL(t)).JetStream()
	if err != nil {
		t.Fatalf("open the observer's JetStream context: %v", err)
	}
	info, err := observer.StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: topic})
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", topic, err)
	}
	if stored := info.State.Subjects[topic]; stored != 1 {
		t.Fatalf("the stream holds %d copies of the publish on %s, want 1: its second send stored a second copy", stored, topic)
	}
}
