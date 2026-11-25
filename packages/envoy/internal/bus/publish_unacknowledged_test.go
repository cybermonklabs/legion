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
		answered := make(chan answer, 1)
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		go func() {
			ack, err := client.PublishAckWithin(ctx, unacknowledgedEnvelope(topic))
			answered <- answer{ack, err}
		}()
		// The send is out on the frozen connection, its request waiting for an answer, when the
		// connection closes: the request fails with nats.ErrConnectionClosed and the bus sends it
		// again on the connection it dials in its place once the server answers again.
		testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the publish to go out on the frozen connection",
			func() bool { return closing.Stats().OutMsgs > before })
		closing.Close()
		thaw()
		var got answer
		select {
		case got = <-answered:
		case <-testwait.Context(t).Done():
			t.Fatal("the publish whose connection closed under it did not return by the test's deadline")
		}
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

	t.Run("a publish interrupted by reconnect is sent again after its first copy is stored", func(t *testing.T) {
		topic := contracts.LegacyAgentSubject("ses_unacknowledged_reconnect")
		proxy := testnats.StartProxy(t, uri)
		reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect through the reply gate: %v", err)
		}
		t.Cleanup(reconnecting.Close)
		before := reconnecting.Connection().Stats().OutMsgs
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
		testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the publish to reach the server before reconnecting",
			func() bool { return reconnecting.Connection().Stats().OutMsgs > before && storedOn(t, topic) == 1 })
		first, err := client.JS().GetLastMsg(bus.Stream, topic)
		if err != nil {
			t.Fatalf("read the first stored copy: %v", err)
		}
		if err := reconnecting.Connection().ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
		proxy.Restore()
		var got answer
		select {
		case got = <-answered:
		case <-testwait.Context(t).Done():
			t.Fatal("the publish interrupted by reconnect did not return by the test's deadline")
		}
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
	// the listener reads the cursor back (roleKeepLaneCursor.confirm) and finds this write. The
	// refusal is read by the one test both conditional writes share (preconditionLost), so this row
	// holds it for Client.WriteTakeoverRecordAt too.
	t.Run("a cursor write stored before a reconnect cut its answer is no ErrCursorMoved", func(t *testing.T) {
		subject := bus.KeptRoleCursorSubject("unacknowledged-cursor-reconnect")
		proxy := testnats.StartProxy(t, uri)
		reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect through the reply gate: %v", err)
		}
		t.Cleanup(reconnecting.Close)
		proxy.FreezeServerReplies()
		body := []byte(`{"kept":7}`)
		answered := make(chan error, 1)
		// No span of its own: an answer can come late on a loaded machine with nothing wrong.
		ctx := testwait.Context(t)
		go func() {
			_, err := reconnecting.PublishCursorAt(ctx, subject, body, 0)
			answered <- err
		}()
		testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the cursor write to be stored before reconnecting",
			func() bool { return storedOn(t, subject) == 1 })
		if err := reconnecting.Connection().ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
		proxy.Restore()
		var got error
		select {
		case got = <-answered:
		case <-testwait.Context(t).Done():
			t.Fatal("the cursor write interrupted by reconnect did not return by the test's deadline")
		}
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

	// The claim route's write of a takeover record takes no precondition (WriteTakeoverRecord): it
	// writes a subject of its own, a fresh attempt id's, which no other writer has. So the resend
	// after a reconnect cut the first send's answer is stored too, and the write returns nil, as it
	// must for a claim whose record stored. Under a precondition the stream would refuse that
	// resend, which meets the first send's own copy, and the claim route would answer the claim as
	// one whose record did not store.
	t.Run("a takeover record stored before a reconnect cut its answer is written", func(t *testing.T) {
		const role, previous = "unacknowledged-claim", "ses_unacknowledged_previous"
		subject := bus.TakeoverSubject(role, previous, id.New())
		record := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: time.Now().UnixMilli()}
		proxy := testnats.StartProxy(t, uri)
		reconnecting, err := bus.ConnectOwningStream([]string{proxy.URL()}, bus.WithReplicas(1))
		if err != nil {
			t.Fatalf("connect through the reply gate: %v", err)
		}
		t.Cleanup(reconnecting.Close)
		proxy.FreezeServerReplies()
		answered := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			answered <- reconnecting.WriteTakeoverRecord(ctx, subject, record)
		}()
		testwait.WithinPolling(t, 5*time.Second, time.Millisecond, "the takeover record to be stored before reconnecting",
			func() bool { return storedOn(t, subject) == 1 })
		if err := reconnecting.Connection().ForceReconnect(); err != nil {
			t.Fatalf("force reconnect: %v", err)
		}
		proxy.Restore()
		var got error
		select {
		case got = <-answered:
		case <-time.After(15 * time.Second):
			t.Fatal("the takeover record write interrupted by reconnect did not return")
		}
		if got != nil {
			t.Fatalf("a takeover record write the stream stored before the reconnect = %v, want nil: the subject holds the record", got)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		records, err := client.TakeoverRecords(ctx, role)
		if err != nil {
			t.Fatalf("read %s's takeover records: %v", role, err)
		}
		if len(records) != 1 || records[0].Subject != subject || records[0].Record != record {
			t.Fatalf("%s's takeover records read %+v, want this write's %+v on %s", role, records, record, subject)
		}
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
