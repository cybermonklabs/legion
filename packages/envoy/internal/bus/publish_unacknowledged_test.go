package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// unacknowledgedEnvelope is a Dispatch envelope on topic under a key of its own, so it is published
// under a Nats-Msg-Id (contracts.DedupeKeyNamesTheUpstreamEvent) and a send of it twice is stored
// once.
func unacknowledgedEnvelope(topic string) contracts.Envelope {
	key := "dispatch-" + id.New()
	return contracts.Envelope{
		EventID:        "evt-" + id.New(),
		Source:         "dispatch",
		SourceEventID:  key,
		Topic:          topic,
		DedupeKey:      key,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a publish whose answer may not come",
		TraceID:        "trace-" + id.New(),
	}
}

// A stored publish that failed after its send went out unanswered may still have put its message
// in the stream, and its error says so (bus.ErrUnacknowledged) beside the error it wraps; one that
// sent nothing, or that the server refused, does not. The listener's sender limit charges a
// sender for the first and not for the others (cmd/listener), so a message the stream may hold is
// never free and one it cannot hold never costs. A publish whose connection closed under it is sent
// again, and its acknowledgement says so (StoredAck.Resent), since the copy the stream names may
// be the first send's. Each case reads the stream to show what the error claims: the stream stores
// what an unacknowledged publish sent once the server answers again, and holds nothing of a
// publish that waited for a reconnect.
//
// A publish's deadline is armed on a clocktest.Fake (bus.WithPublishAcknowledgementClock), which
// only a case whose verdict is that deadline's end moves, to the deadline the publish armed, once
// the publish has reached the wait the case names: the acknowledgement wait after a send to the
// frozen server, and the wait for a reconnect. No publish here ends on the machine's clock.
func TestAStoredPublishSaysWhenTheStreamMayHoldWhatItFailedToSend(t *testing.T) {
	server, uri := testnats.Start(t)
	acknowledgements := clocktest.New(time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC))
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1), bus.WithPublishAcknowledgementClock(acknowledgements))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	// storedOn is how many messages the stream holds on topic.
	storedOn := func(t *testing.T, topic string) uint64 {
		t.Helper()
		info, err := client.JS().StreamInfo(bus.Stream, &nats.StreamInfoRequest{SubjectsFilter: topic})
		if err != nil {
			t.Fatalf("read what the stream holds on %s: %v", topic, err)
		}
		return info.State.Subjects[topic]
	}

	t.Run("a publish under a context that has already ended sends nothing and is no ErrUnacknowledged", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_ended")
		ended, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		_, err := client.PublishAckWithin(ended, unacknowledgedEnvelope(topic))
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, bus.ErrUnacknowledged) {
			t.Fatalf("a publish under an ended context = %v, want the context's error and no ErrUnacknowledged", err)
		}
		if stored := storedOn(t, topic); stored != 0 {
			t.Fatalf("the stream holds %d messages on %s, want none", stored, topic)
		}
	})

	t.Run("a write the stream refused is no ErrUnacknowledged", func(t *testing.T) {
		ctx := testwait.Context(t)
		subject := bus.KeptRoleCursorSubject("unacknowledged-refused")
		if _, err := client.PublishCursorAt(ctx, subject, []byte(`{"kept":1}`), 0); err != nil {
			t.Fatalf("the first cursor write: %v", err)
		}
		_, err := client.PublishCursorAt(ctx, subject, []byte(`{"kept":2}`), 0)
		if !errors.Is(err, bus.ErrCursorMoved) || errors.Is(err, bus.ErrUnacknowledged) {
			t.Fatalf("a write at a stale expectation = %v, want ErrCursorMoved and no ErrUnacknowledged", err)
		}
	})

	t.Run("a publish whose acknowledgement is lost after its send is ErrUnacknowledged and is stored", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_lost")
		thaw := server.Freeze(t)
		t.Cleanup(thaw)
		frozen := client.Connection()
		before := frozen.Stats().OutMsgs
		ctx := testwait.Context(t)
		published := make(chan error, 1)
		go func() {
			_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			published <- err
		}()
		// The publish arms its deadline before it sends, so the fake moves to it only once the send
		// is out on the frozen connection: what the deadline then ends is the acknowledgement wait.
		deadline := acknowledgements.AwaitArmed(t, "the deadline of the publish to the frozen server", 1)[0]
		testwait.Eventually(t, "the publish to go out on the frozen connection",
			func() bool { return frozen.Stats().OutMsgs > before })
		acknowledgements.Set(deadline)
		err := testwait.Receive(t, "the publish to the frozen server to end at its deadline", published)
		thaw()
		if !errors.Is(err, bus.ErrUnacknowledged) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a publish to a server that stopped answering = %v, want ErrUnacknowledged around the context's error", err)
		}
		testwait.Eventually(t, "the stream to store what the frozen server held",
			func() bool { return storedOn(t, topic) == 1 })
	})

	t.Run("a publish whose connection closed under it is sent again and its acknowledgement says so", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_resent")
		thaw := server.Freeze(t)
		t.Cleanup(thaw)
		closing := client.Connection()
		before := closing.Stats().OutMsgs
		type answer struct {
			ack bus.StoredAck
			err error
		}
		ctx := testwait.Context(t)
		answered := make(chan answer, 1)
		go func() {
			ack, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			answered <- answer{ack, err}
		}()
		// The send is out on the frozen connection, its request waiting for an answer, when the
		// connection closes: the request fails with nats.ErrConnectionClosed and the bus sends it
		// again on the connection it dials in its place once the server answers again.
		testwait.Eventually(t, "the publish to go out on the frozen connection",
			func() bool { return closing.Stats().OutMsgs > before })
		closing.Close()
		thaw()
		got := testwait.Receive(t, "the publish whose connection closed under it to return", answered)
		if got.err != nil {
			t.Fatalf("a publish sent again after its connection closed = %v, want it acknowledged", got.err)
		}
		if !got.ack.Resent {
			t.Fatalf("a publish sent again after its connection closed answered %+v, want Resent", got.ack)
		}
		// The first send reached the stream after the thaw too, or the stream stored the second
		// alone; under one Nats-Msg-Id it holds one copy either way, and the second's verdict says
		// which (Duplicate).
		t.Logf("the resend's acknowledgement: %+v", got.ack)
		if stored := storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one copy of the publish sent twice", stored, topic)
		}
	})

	t.Run("a publish that waits out its deadline for a reconnect sends nothing and is no ErrUnacknowledged", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_reconnecting")
		server.Stop(t)
		testwait.Eventually(t, "the client to reconnect after its server stopped",
			func() bool { return !client.Connected() && client.Connection().IsReconnecting() })
		ctx := testwait.Context(t)
		published := make(chan error, 1)
		go func() {
			_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			published <- err
		}()
		deadline := acknowledgements.AwaitArmed(t, "the deadline of the publish while the client reconnects", 1)[0]
		if len(published) != 0 {
			t.Fatalf("a publish while the client reconnects returned %v before the clock reached its deadline", <-published)
		}
		acknowledgements.Set(deadline)
		err := testwait.Receive(t, "the publish while the client reconnects to end at its deadline", published)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, bus.ErrUnacknowledged) {
			t.Fatalf("a publish while the client reconnects = %v, want the context's error and no ErrUnacknowledged", err)
		}
		server.Start(t)
		testwait.Eventually(t, "the stream to answer again after its server started",
			func() bool {
				_, err := client.JS().StreamInfo(bus.Stream)
				return client.Connected() && err == nil
			})
		// A later publish on the same topic, acknowledged: the stream stores what one connection
		// sends in the order it was sent, so a copy of the publish that waited for the reconnect,
		// sent before this one, would be stored before it.
		if _, err := client.PublishAckWithin(testwait.Context(t), unacknowledgedEnvelope(topic)); err != nil {
			t.Fatalf("a publish on %s after the server started: %v", topic, err)
		}
		if stored := storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the later publish's one and none of the publish that waited for a reconnect", stored, topic)
		}
	})
}
