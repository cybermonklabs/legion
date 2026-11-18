package bus_test

import (
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A forward counts once the stream stores it: no session subscribes to its agent subject any more
// (INFRA-771), so StartRequestStoredTo's wait returns nil on the stream's acknowledgement, and
// stores the forward once however often it is sent (its Nats-Msg-Id). The window is a deadline on
// the client's clock, which stands still here, so a wait that ran out its window instead would
// never return. On a subject no stream captures and nobody subscribes, the server's no-responders
// answer ends the wait with ErrForwardNotStored, with that clock still standing (PendingForward.Wait).
func TestStartRequestStoredToCountsTheStreamsAcknowledgementAlone(t *testing.T) {
	clock := clocktest.New(fakeStart)
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)}, bus.WithClock(clock))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	item := contracts.Envelope{
		EventID:        "evt_stored_only",
		Source:         "agent",
		SourceEventID:  "src_stored_only",
		Topic:          "notifications.role.stored-only",
		DedupeKey:      "publish.stored-only",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a stored forward to a session's agent subject",
		TraceID:        "trace_stored_only",
	}
	const window = 2 * time.Second
	subject := contracts.LegacyAgentSubject("ses_serving_port")
	for range 2 {
		forward, err := client.StartRequestStoredTo(subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
		if err != nil {
			t.Fatalf("StartRequestStoredTo: %v", err)
		}
		waited := make(chan error, 1)
		go func() { waited <- forward.Wait() }()
		if err := testwait.Receive(t, "the stored forward's wait with its window's clock standing still", waited); err != nil {
			t.Fatalf("the stored forward's wait = %v, want nil on the stream's acknowledgement", err)
		}
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stored := info.State.Subjects[subject]; stored != 1 {
		t.Fatalf("the stream stores %d copies of the forward sent twice, want 1", stored)
	}

	const uncaptured = "test.uncaptured.serving-port"
	forward, err := client.StartRequestStoredTo(uncaptured, item, bus.ForwardMsgID(item.DedupeKey, uncaptured), window)
	if err != nil {
		t.Fatalf("StartRequestStoredTo on %s: %v", uncaptured, err)
	}
	unstored := make(chan error, 1)
	go func() { unstored <- forward.Wait() }()
	if err := testwait.Receive(t, "the wait of a forward no stream stores to end at the server's no-responders answer, with the clock standing still", unstored); !errors.Is(err, bus.ErrForwardNotStored) || !errors.Is(err, natsgo.ErrNoResponders) {
		t.Fatalf("the wait of a forward no stream stores = %v, want ErrForwardNotStored wrapping the server's no-responders answer", err)
	}
}

// A forward with no Nats-Msg-Id is refused before anything is sent: the stored copy is the whole
// of the delivery, and an unnamed one is stored again by every attempt, so the name is required
// rather than optional. The refusal is an ErrRefused, which the listener answers at once instead
// of retrying it for the rest of the send's window (forwardRefused).
func TestStartRequestStoredToRefusesAForwardWithoutAMsgID(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subject := contracts.LegacyAgentSubject("ses_unnamed_forward")

	forward, err := client.StartRequestStoredTo(subject, contracts.Envelope{
		EventID:        "evt_unnamed",
		Source:         "agent",
		SourceEventID:  "src_unnamed",
		Topic:          "notifications.role.unnamed",
		DedupeKey:      "publish.unnamed",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a forward with no Nats-Msg-Id",
	}, "", 2*time.Second)
	if forward != nil {
		t.Fatalf("an unnamed forward returned a pending forward; want none, since nothing was sent")
	}
	if !errors.Is(err, bus.ErrNoForwardMsgID) || !errors.Is(err, bus.ErrRefused) {
		t.Fatalf("an unnamed forward returned %v, want ErrNoForwardMsgID and ErrRefused", err)
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stored := info.State.Subjects[subject]; stored != 0 {
		t.Fatalf("the stream stores %d messages on %s after a refused forward, want none", stored, subject)
	}
}

// A window's Request is the outbox's forward of one envelope to one holder, so it is named by the
// same rule and refused the same way: an unnamed request would be stored again by every attempt
// of the outbox's retry, which is what the Nats-Msg-Id exists to stop. The refusal reaches the
// caller in the request's own channel, before anything is sent.
func TestAWindowRequestRefusesAForwardWithoutAMsgID(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subject := contracts.LegacyAgentSubject("ses_unnamed_request")

	item := contracts.Envelope{
		EventID:        "evt_unnamed_request",
		Source:         "agent",
		SourceEventID:  "src_unnamed_request",
		Topic:          "notifications.role.unnamed-request",
		DedupeKey:      "publish.unnamed-request",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a request with no Nats-Msg-Id",
	}
	err = testwait.Receive(t, "the answer to the unnamed request", client.WindowUntil(nil).Request(subject, item, bus.RequestOptions{}))
	if !errors.Is(err, bus.ErrNoForwardMsgID) || !errors.Is(err, bus.ErrRefused) {
		t.Fatalf("an unnamed request answered %v, want ErrNoForwardMsgID and ErrRefused", err)
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stored := info.State.Subjects[subject]; stored != 0 {
		t.Fatalf("the stream holds %d copies on %s after a refused request, want 0", stored, subject)
	}
}

// A forward whose flush times out is refused by StartRequestStoredTo itself, before any wait, and
// within the window the caller gave rather than the client's default 10 s flush: the forward is
// still buffered on this side of a stalled connection and is not known to have reached anyone, so
// the listener reports it delivery_failed at once rather than holding the role send open. Driven
// for real: the server's traffic is frozen, so the client stays CONNECTED, buffers the publish,
// and its flush waits for a PONG that cannot come until the window runs out. nats.go times that
// flush itself, on a clock no test reaches, so which window bounds it is read off the order of two
// forwards: a long-windowed one started first is still waiting when a short-windowed one started
// after it has been refused, which a flush bounded by anything but its own window could not do.
func TestStartRequestStoredToIsBoundedByItsWindowWhenTheFlushTimesOut(t *testing.T) {
	srv, uri := startNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	subject := contracts.LegacyAgentSubject("ses_stalled")
	item := contracts.Envelope{
		EventID:        "evt_stalled",
		Source:         "agent",
		SourceEventID:  "src_stalled",
		Topic:          "notifications.role.stalled-server",
		DedupeKey:      "publish.stalled",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "the server's traffic is frozen under a live connection",
		TraceID:        "trace_stalled",
	}
	// Control: the same subject and envelope are stored while the server runs.
	forward, err := client.StartRequestStoredTo(subject, item, bus.ForwardMsgID(item.DedupeKey, subject), 2*time.Second)
	if err != nil {
		t.Fatalf("StartRequestStoredTo against a healthy server: %v", err)
	}
	if err := forward.Wait(); err != nil {
		t.Fatalf("the stored forward's wait against a healthy server = %v, want nil", err)
	}

	// The server's traffic stops in both directions while its connection stays up.
	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes startNATS's shutdown.
	t.Cleanup(thaw)

	const (
		shortWindow = 500 * time.Millisecond
		longWindow  = 30 * time.Second
	)
	longer := make(chan error, 1)
	go func() {
		_, err := client.StartRequestStoredTo(contracts.LegacyAgentSubject("ses_stalled_long"), item, bus.ForwardMsgID(item.DedupeKey, "long"), longWindow)
		longer <- err
	}()
	_, err = client.StartRequestStoredTo(subject, item, bus.ForwardMsgID(item.DedupeKey, subject), shortWindow)
	if err == nil {
		t.Fatal("StartRequestStoredTo against a frozen server returned no error; the forward cannot have been flushed")
	}
	if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("StartRequestStoredTo error = %v, want a wrapped nats.ErrTimeout from the flush", err)
	}
	if len(longer) != 0 {
		t.Fatalf("the forward with the %s window was answered (%v) by the time the %s one was: the flush is not bounded by its own window", longWindow, <-longer, shortWindow)
	}
}
