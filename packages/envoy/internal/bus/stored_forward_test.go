package bus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A forward counts once the stream stores it: no session subscribes to its agent subject any more
// (INFRA-771), so StartRequestStoredTo's wait returns nil on the stream's acknowledgement, well
// inside the window, and stores the forward once however often it is sent (its Nats-Msg-Id). On a
// subject no stream captures and nobody subscribes it returns ErrForwardNotStored.
func TestStartRequestStoredToCountsTheStreamsAcknowledgementAlone(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
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
	var acks []bus.StoredAck
	for range 2 {
		started := time.Now()
		forward, err := client.StartRequestStoredTo(context.Background(), subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
		if err != nil {
			t.Fatalf("StartRequestStoredTo: %v", err)
		}
		ack, err := forward.Wait()
		if err != nil {
			t.Fatalf("the stored forward's wait = %v, want nil on the stream's acknowledgement", err)
		}
		if elapsed := time.Since(started); elapsed >= window {
			t.Fatalf("the stored forward's wait took %s, the whole window; want it to end on the acknowledgement", elapsed)
		}
		acks = append(acks, ack)
	}
	// The second forward under the same Nats-Msg-Id is the stream's duplicate of the first, and its
	// acknowledgement names the first's copy, which a role send answers (roleSend.deliveredHeld).
	if acks[0].Duplicate || acks[0].Sequence == 0 || !acks[1].Duplicate || acks[1].Sequence != acks[0].Sequence {
		t.Fatalf("the two forwards' acknowledgements are %+v, want the first new at a sequence and the second a duplicate naming it", acks)
	}
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: subject})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stored := info.State.Subjects[subject]; stored != 1 {
		t.Fatalf("the stream stores %d copies of the forward sent twice, want 1", stored)
	}

	const uncaptured = "test.uncaptured.serving-port"
	forward, err := client.StartRequestStoredTo(context.Background(), uncaptured, item, bus.ForwardMsgID(item.DedupeKey, uncaptured), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("StartRequestStoredTo on %s: %v", uncaptured, err)
	}
	if _, err := forward.Wait(); !errors.Is(err, bus.ErrForwardNotStored) {
		t.Fatalf("the wait of a forward no stream stores = %v, want ErrForwardNotStored", err)
	}
}

// A forward with no Nats-Msg-Id is refused before anything is sent: the stored copy is the whole
// of the delivery, and an unnamed one is stored again by every attempt, so the name is required
// rather than optional. The refusal is an ErrRefused, which the listener answers at once instead
// of retrying it for the rest of the send's window (cmd/listener's busRefusesForGood).
func TestStartRequestStoredToRefusesAForwardWithoutAMsgID(t *testing.T) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subject := contracts.LegacyAgentSubject("ses_unnamed_forward")

	forward, err := client.StartRequestStoredTo(context.Background(), subject, contracts.Envelope{
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
	select {
	case err := <-client.WindowUntil(nil).Request(subject, item, bus.RequestOptions{Timeout: 2 * time.Second}):
		if !errors.Is(err, bus.ErrNoForwardMsgID) || !errors.Is(err, bus.ErrRefused) {
			t.Fatalf("an unnamed request answered %v, want ErrNoForwardMsgID and ErrRefused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unnamed request was never answered")
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
// and its flush waits for a PONG that cannot come until the window runs out.
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
	forward, err := client.StartRequestStoredTo(context.Background(), subject, item, bus.ForwardMsgID(item.DedupeKey, subject), 2*time.Second)
	if err != nil {
		t.Fatalf("StartRequestStoredTo against a healthy server: %v", err)
	}
	if _, err := forward.Wait(); err != nil {
		t.Fatalf("the stored forward's wait against a healthy server = %v, want nil", err)
	}

	// The server's traffic stops in both directions while its connection stays up.
	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes startNATS's shutdown.
	t.Cleanup(thaw)

	const window = 500 * time.Millisecond
	started := time.Now()
	_, err = client.StartRequestStoredTo(context.Background(), subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("StartRequestStoredTo against a frozen server returned no error; the forward cannot have been flushed")
	}
	if !errors.Is(err, natsgo.ErrTimeout) {
		t.Fatalf("StartRequestStoredTo error = %v, want a wrapped nats.ErrTimeout from the flush", err)
	}
	// The publish was made and buffered; only its flush ran out of the window, so the error is not
	// marked ErrNotSent, which tells the listener the forward never left this process: the server
	// may have stored it, and the listener sends it again to the same holder. Nor is it the one for
	// a window that ended before the publish (bus.ErrWindowEndedBeforePublish), which the listener
	// marks on the line of the message it holds.
	if errors.Is(err, bus.ErrNotSent) || errors.Is(err, bus.ErrWindowEndedBeforePublish) {
		t.Fatalf("StartRequestStoredTo error = %v, which is ErrNotSent or ErrWindowEndedBeforePublish, but the publish was made and only its flush timed out", err)
	}
	if elapsed < window || elapsed > 2*time.Second {
		t.Fatalf("StartRequestStoredTo returned after %s; the flush must be bounded by the %s window, not the client's 10s default", elapsed, window)
	}
}

// A forward's wait ends at once when its caller's context is cancelled, however much of its window
// is left, with ErrForwardNotStored wrapping the cancel: the forward was published, so the stream
// may have stored it, and the error is not ErrNotSent, so a caller that stops holds the message
// pinned and sends it again under the same Nats-Msg-Id (cmd/listener's role lane). A plain
// subscriber takes the subject and never answers, so neither an acknowledgement nor a no-responders
// answer can end the wait first. The control is the same forward with no cancel, which waits out its
// window and ends without the cancel's error.
func TestAForwardsWaitEndsAtItsContextsCancel(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	silent := testnats.Connect(t, uri)
	t.Cleanup(silent.Close)
	const subject = "test.silent.forward-wait"
	if _, err := silent.SubscribeSync(subject); err != nil {
		t.Fatalf("subscribe the silent taker: %v", err)
	}
	if err := silent.Flush(); err != nil {
		t.Fatalf("flush the silent taker: %v", err)
	}
	item := contracts.Envelope{EventID: "evt_cut_wait", Topic: "notifications.role.cut-wait", DedupeKey: "publish.cut-wait"}
	const window = 30 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	forward, err := client.StartRequestStoredTo(ctx, subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
	if err != nil {
		t.Fatalf("StartRequestStoredTo: %v", err)
	}
	cancel()
	started := time.Now()
	_, err = forward.Wait()
	elapsed := time.Since(started)
	if !errors.Is(err, bus.ErrForwardNotStored) || !errors.Is(err, context.Canceled) || errors.Is(err, bus.ErrNotSent) {
		t.Fatalf("the cancelled forward's wait = %v, want ErrForwardNotStored wrapping context.Canceled and not ErrNotSent", err)
	}
	if elapsed >= window/2 {
		t.Fatalf("the cancelled forward's wait took %s, its window's; want it to end at the cancel", elapsed)
	}

	control := contracts.Envelope{EventID: "evt_uncut_wait", Topic: "notifications.role.cut-wait", DedupeKey: "publish.uncut-wait"}
	forward, err = client.StartRequestStoredTo(context.Background(), subject, control, bus.ForwardMsgID(control.DedupeKey, subject), 300*time.Millisecond)
	if err != nil {
		t.Fatalf("StartRequestStoredTo, control: %v", err)
	}
	if _, err := forward.Wait(); !errors.Is(err, bus.ErrForwardNotStored) || errors.Is(err, context.Canceled) {
		t.Fatalf("the uncancelled forward's wait = %v, want ErrForwardNotStored at its window, without a cancel", err)
	}
}

// A forward's publish whose flush waits on a server that stopped answering ends at once when its
// caller's context is cancelled, not at its window: the publish was made, so the error is the
// flush's, wrapping the cancel, and not ErrNotSent. The server's replies are frozen through a proxy
// while the client's writes still reach the server, so a subscriber on a direct connection sees the
// forward arrive, and the row cancels then. The control is a context cancelled before the call,
// whose forward is never published and is ErrNotSent.
func TestAForwardsPublishEndsAtItsContextsCancel(t *testing.T) {
	uri := testnats.URL(t)
	proxy := testnats.StartProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.URL()})
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(client.Close)
	watcher := testnats.Connect(t, uri)
	t.Cleanup(watcher.Close)
	const subject = "test.held.forward-publish"
	arrived, err := watcher.SubscribeSync(subject)
	if err != nil {
		t.Fatalf("subscribe the watcher: %v", err)
	}
	if err := watcher.Flush(); err != nil {
		t.Fatalf("flush the watcher: %v", err)
	}
	item := contracts.Envelope{EventID: "evt_cut_publish", Topic: "notifications.role.cut-publish", DedupeKey: "publish.cut-publish"}
	const window = 30 * time.Second
	proxy.FreezeServerReplies()
	t.Cleanup(proxy.Restore)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	published := make(chan error, 1)
	go func() {
		_, err := client.StartRequestStoredTo(ctx, subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window)
		published <- err
	}()
	if _, err := arrived.NextMsgWithContext(t.Context()); err != nil {
		t.Fatalf("the forward never reached the server: %v", err)
	}
	started := time.Now()
	cancel()
	err = testwait.Receive(t, "the held publish's answer once cancelled", published)
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, context.Canceled) || errors.Is(err, bus.ErrNotSent) {
		t.Fatalf("the cancelled publish = %v, want the flush's error wrapping context.Canceled and not ErrNotSent", err)
	}
	if elapsed >= window/2 {
		t.Fatalf("the cancelled publish answered %s after the cancel, its window's; want it to end at the cancel", elapsed)
	}

	proxy.Restore()
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := client.StartRequestStoredTo(done, subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window); !errors.Is(err, bus.ErrNotSent) {
		t.Fatalf("a forward whose context was cancelled before the call = %v, want ErrNotSent", err)
	}
}
