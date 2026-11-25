package bus_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
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
func TestAStoredPublishSaysWhenTheStreamMayHoldWhatItFailedToSend(t *testing.T) {
	t.Parallel()
	server, uri := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { bustest.Close(t, client) })
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
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
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

	// The server's replies are held (FreezeReplies), so the publish reaches the stream and its
	// acknowledgement does not reach the client. The publish's context is cancelled once the stream
	// holds the publish's own message, read beside the relay (StoredOn): a fact no other message the
	// client sends makes, so the context ends during the acknowledgement wait, after the send, however
	// long the send took to go out. A deadline of the row's own could pass before the send on a loaded
	// machine, and the error would then be the context's alone. The acknowledgement wait ends on the
	// cancel, or on the publish's own bound (publishBound) should that come first; either is the
	// context's end.
	t.Run("a publish whose acknowledgement is lost after its send is ErrUnacknowledged and is stored", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_lost")
		thaw := server.FreezeReplies(t)
		t.Cleanup(thaw)
		ctx, cancel := context.WithCancel(testwait.Context(t))
		defer cancel()
		published := make(chan error, 1)
		go func() {
			_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			published <- err
		}()
		reads := testwait.Context(t)
		testwait.Until(t, "the stream to store the publish its server's answer is held from", func() (string, bool) {
			stored := server.StoredOn(t, reads, bus.Stream, topic)
			return fmt.Sprintf("the stream holds %d messages on %s", stored, topic), stored == 1
		})
		cancel()
		err := testwait.Receive(t, "the publish whose context ended after its send to return", published)
		thaw()
		if !errors.Is(err, bus.ErrUnacknowledged) || !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a publish to a server whose answer was held = %v, want ErrUnacknowledged around the context's error", err)
		}
		if stored := storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one the unacknowledged publish stored", stored, topic)
		}
	})

	t.Run("a publish whose connection closed under it is sent again and its acknowledgement says so", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_resent")
		thaw := server.FreezeReplies(t)
		t.Cleanup(thaw)
		closing := client.Connection()
		type answer struct {
			ack bus.StoredAck
			err error
		}
		answered := make(chan answer, 1)
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		go func() {
			ack, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			answered <- answer{ack, err}
		}()
		// The send has reached the stream, its request waiting for an answer the relay holds, when
		// the connection closes: the request fails with nats.ErrConnectionClosed and the bus sends it
		// again on the connection it dials in its place once the server's answers pass again. The
		// stream's count on the publish's own subject, read beside the relay, is what shows the send
		// went out: no other message the client sends moves it.
		reads := testwait.Context(t)
		testwait.Until(t, "the stream to store the publish before its connection closes", func() (string, bool) {
			stored := server.StoredOn(t, reads, bus.Stream, topic)
			return fmt.Sprintf("the stream holds %d messages on %s", stored, topic), stored == 1
		})
		closing.Close()
		thaw()
		got := testwait.Receive(t, "the publish whose connection closed under it to return", answered)
		if got.err != nil {
			t.Fatalf("a publish sent again after its connection closed = %v, want it acknowledged", got.err)
		}
		if !got.ack.Resent {
			t.Fatalf("a publish sent again after its connection closed answered %+v, want Resent", got.ack)
		}
		// The first send was stored before the close, so the resend is a repeat of it under the same
		// Nats-Msg-Id, and the stream holds the one copy.
		t.Logf("the resend's acknowledgement: %+v", got.ack)
		if stored := storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one copy of the publish sent twice", stored, topic)
		}
	})

	t.Run("a publish interrupted by reconnect is sent again after its first copy is stored", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_reconnect")
		proxy := testnats.StartProxy(t, uri)
		reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect through the reply gate: %v", err)
		}
		t.Cleanup(func() { bustest.Close(t, reconnecting) })
		proxy.FreezeServerReplies()
		type answer struct {
			ack bus.StoredAck
			err error
		}
		answered := make(chan answer, 1)
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		go func() {
			ack, err := reconnecting.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			answered <- answer{ack, err}
		}()
		testwait.Until(t, "the publish to reach the server before reconnecting", func() (string, bool) {
			stored := storedOn(t, topic)
			return fmt.Sprintf("the stream holds %d messages on %s", stored, topic), stored == 1
		})
		first, err := client.JS().GetLastMsg(bus.Stream, topic)
		if err != nil {
			t.Fatalf("read the first stored copy: %v", err)
		}
		if err := reconnecting.Connection().ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
		proxy.Restore()
		got := testwait.Receive(t, "the publish interrupted by reconnect to return", answered)
		if got.err != nil {
			t.Fatalf("a publish sent again after reconnect = %v, want it acknowledged", got.err)
		}
		if !got.ack.Resent || !got.ack.Duplicate || got.ack.Sequence != first.Sequence {
			t.Fatalf("a publish sent again after reconnect answered %+v, want resent duplicate at sequence %d", got.ack, first.Sequence)
		}
		if stored := storedOn(t, topic); stored != 1 {
			t.Fatalf("the stream holds %d messages on %s, want the one copy of the publish sent twice", stored, topic)
		}
	})

	// A cursor write under a precondition is sent again after a reconnect cuts its answer, and the
	// stream refuses the resend because the first send already moved the cursor. That refusal is
	// no ErrCursorMoved, which says the write stored nothing: the error wraps ErrUnacknowledged, so
	// the listener reads the cursor back (roleKeepLaneCursor.confirm) and finds this write.
	t.Run("a cursor write stored before a reconnect cut its answer is no ErrCursorMoved", func(t *testing.T) {
		subject := bus.KeptRoleCursorSubject("unacknowledged-cursor-reconnect")
		proxy := testnats.StartProxy(t, uri)
		reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect through the reply gate: %v", err)
		}
		t.Cleanup(func() { bustest.Close(t, reconnecting) })
		proxy.FreezeServerReplies()
		body := []byte(`{"kept":7}`)
		answered := make(chan error, 1)
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		go func() {
			_, err := reconnecting.PublishCursorAt(ctx, subject, body, 0)
			answered <- err
		}()
		testwait.Eventually(t, "the cursor write to be stored before reconnecting",
			func() bool { return storedOn(t, subject) == 1 })
		if err := reconnecting.Connection().ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
		proxy.Restore()
		got := testwait.Receive(t, "the cursor write interrupted by reconnect to return", answered)
		if errors.Is(got, bus.ErrCursorMoved) {
			t.Fatalf("a cursor write the stream stored before the reconnect = %v, want no ErrCursorMoved: the cursor holds this write", got)
		}
		if !errors.Is(got, bus.ErrUnacknowledged) {
			t.Fatalf("a cursor write whose answer the reconnect cut = %v, want ErrUnacknowledged", got)
		}
		stored, err := client.JS().GetLastMsg(bus.Stream, subject)
		if err != nil {
			t.Fatalf("read the cursor back: %v", err)
		}
		if string(stored.Data) != string(body) {
			t.Fatalf("the cursor reads back %q, want this write's %q", stored.Data, body)
		}
		if n := storedOn(t, subject); n != 1 {
			t.Fatalf("the stream holds %d cursor writes on %s, want the one this call stored", n, subject)
		}
	})

	t.Run("a publish that waits out its deadline for a reconnect sends nothing and is no ErrUnacknowledged", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_reconnecting")
		server.Stop(t)
		testwait.Until(t, "the client to reconnect after its server stopped", func() (string, bool) {
			connected, reconnecting := client.Connected(), client.Connection().IsReconnecting()
			return fmt.Sprintf("the client reads connected %t, its connection reconnecting %t", connected, reconnecting), !connected && reconnecting
		})
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, bus.ErrUnacknowledged) {
			t.Fatalf("a publish while the client reconnects = %v, want the context's error and no ErrUnacknowledged", err)
		}
		server.Start(t)
		testwait.Until(t, "the stream to answer again after its server started", func() (string, bool) {
			_, err := client.JS().StreamInfo(bus.Stream)
			connected := client.Connected()
			return fmt.Sprintf("the client reads connected %t, the stream's info answered %v", connected, err), connected && err == nil
		})
		if stored := storedOn(t, topic); stored != 0 {
			t.Fatalf("the stream holds %d messages on %s, want none", stored, topic)
		}
	})
}
