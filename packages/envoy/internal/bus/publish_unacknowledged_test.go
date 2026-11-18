package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
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
// never free and one it cannot hold never costs. A publish whose caller reads the duplicate verdict
// and whose connection closed under it is not sent again, since a second send would answer the
// first send's copy as a duplicate, and its error says the stream may hold it, so the listener
// charges it. Each case reads the stream to show what the error claims: the stream stores what an
// unacknowledged publish sent once the server answers again, and holds nothing of a publish that
// waited for a reconnect.
func TestAStoredPublishSaysWhenTheStreamMayHoldWhatItFailedToSend(t *testing.T) {
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
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
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
		thaw()
		if !errors.Is(err, bus.ErrUnacknowledged) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a publish to a server that stopped answering = %v, want ErrUnacknowledged around the context's error", err)
		}
		testwait.WithinPolling(t, 10*time.Second, 50*time.Millisecond, "the stream to store what the frozen server held",
			func() bool { return storedOn(t, topic) == 1 })
	})

	t.Run("a publish whose connection closed under it is sent once and is ErrUnacknowledged", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_closed")
		thaw := server.Freeze(t)
		t.Cleanup(thaw)
		closing := client.Connection()
		before := closing.Stats().OutMsgs
		answered := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			answered <- err
		}()
		// The send is out on the frozen connection, its request waiting for an answer, when the
		// connection closes: the request answers bus.ErrConnectionLost, since the client is still
		// running, whichever of nats.go's answer and the end of the connection's life comes first
		// (closedUnder), and the bus sends it no second time, since the caller reads the duplicate
		// verdict a second send would answer for the first send's copy.
		testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the publish to go out on the frozen connection",
			func() bool { return closing.Stats().OutMsgs > before })
		closing.Close()
		thaw()
		var err error
		select {
		case err = <-answered:
		case <-time.After(15 * time.Second):
			t.Fatal("the publish whose connection closed under it did not return")
		}
		// An error, and no acknowledgement, is what shows the publish was not sent again: a second
		// send would have been acknowledged, a duplicate of the first send's copy where the frozen
		// server read that send before the close. The frozen server may or may not have read it, so
		// the stream may hold its copy, and the error says so.
		if !errors.Is(err, bus.ErrUnacknowledged) || !errors.Is(err, bus.ErrConnectionLost) {
			t.Errorf("a publish whose connection closed under it = %v, want ErrUnacknowledged around ErrConnectionLost", err)
		}
		testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "the client to answer again after the close",
			func() bool {
				_, err := client.JS().StreamInfo(bus.Stream)
				return client.Connected() && err == nil
			})
	})

	t.Run("a publish that waits out its deadline for a reconnect sends nothing and is no ErrUnacknowledged", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_reconnecting")
		server.Stop(t)
		testwait.WithinPolling(t, 10*time.Second, 10*time.Millisecond, "the client to reconnect after its server stopped",
			func() bool { return !client.Connected() && client.Connection().IsReconnecting() })
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, bus.ErrUnacknowledged) {
			t.Fatalf("a publish while the client reconnects = %v, want the context's error and no ErrUnacknowledged", err)
		}
		server.Start(t)
		testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "the stream to answer again after its server started",
			func() bool {
				_, err := client.JS().StreamInfo(bus.Stream)
				return client.Connected() && err == nil
			})
		if stored := storedOn(t, topic); stored != 0 {
			t.Fatalf("the stream holds %d messages on %s, want none", stored, topic)
		}
	})
}
