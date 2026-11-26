package bus_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

const (
	orderedStreamTopic = "notifications.dispatch.issue.T-1.message.created"
	uncapturedTopic    = "notifications.role.uncaptured"
)

// numberedEnvelope is the index'th of a run of dispatch envelopes on topic, carrying index as its
// source event id.
func numberedEnvelope(topic string, index int) contracts.Envelope {
	return contracts.Envelope{
		EventID:       fmt.Sprintf("dispatch-%d", index),
		Source:        "dispatch",
		SourceEventID: fmt.Sprint(index),
		Topic:         topic,
		DedupeKey:     fmt.Sprintf("dispatch-%d", index),
		IssuedAt:      contracts.NowMillis(),
	}
}

func sourceEventID(t *testing.T, data []byte) string {
	t.Helper()
	var envelope contracts.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode an envelope: %v", err)
	}
	return envelope.SourceEventID
}

// PublishAsync sends each envelope without waiting for the answer to the one before, every one is
// answered accepted, and the stream stores a run sent back to back on one window's connection in
// send order.
func TestPublishAsyncKeepsSendOrder(t *testing.T) {
	t.Parallel()
	const count = 500
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(reader.Close)

	window := client.WindowUntil(nil)
	answers := make([]<-chan error, 0, count)
	for index := range count {
		answers = append(answers, window.PublishAsync(numberedEnvelope(orderedStreamTopic, index)))
	}
	for index, answer := range answers {
		if err := testwait.Receive(t, fmt.Sprintf("publish %d to be answered", index), answer); err != nil {
			t.Fatalf("publish %d was answered %v, want accepted", index, err)
		}
	}

	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if info.State.Msgs != count {
		t.Fatalf("the stream holds %d messages, want %d", info.State.Msgs, count)
	}
	for index := range count {
		message, err := js.GetMsg(bus.Stream, info.State.FirstSeq+uint64(index))
		if err != nil {
			t.Fatalf("read stream message %d: %v", index, err)
		}
		if got := sourceEventID(t, message.Data); got != fmt.Sprint(index) {
			t.Fatalf("stream message %d carries envelope %s, want %d", index, got, index)
		}
	}
}

// A topic the stream does not capture is answered at once, before anything is sent, with
// ErrNotCaptured: neither a refusal, which a caller counts as reached, nor ErrUnavailable, which it
// resends with no attempt counted, so the caller backs the envelope off. Nothing reaches a core
// subscriber of the topic.
func TestAWindowAnswersATopicTheStreamDoesNotCaptureAsNotCaptured(t *testing.T) {
	t.Parallel()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(reader.Close)
	received, err := reader.SubscribeSync(uncapturedTopic)
	if err != nil {
		t.Fatalf("subscribe to the uncaptured topic: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	select {
	case err := <-client.WindowUntil(nil).PublishAsync(numberedEnvelope(uncapturedTopic, 0)):
		if !errors.Is(err, bus.ErrNotCaptured) || errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("a publish to %s answered %v, want ErrNotCaptured, neither ErrRefused nor ErrUnavailable", uncapturedTopic, err)
		}
	default:
		t.Fatalf("a publish to %s returned with no answer, as if it had sent something", uncapturedTopic)
	}
	// Nothing was sent, which two flushes prove with no span. The server takes a connection's
	// messages in order, so once the client's flush is answered it has routed anything the client
	// sent, into the reader's connection ahead of anything later; once the reader's own flush is
	// answered, that message is in the subscription's queue. A window of 300 ms (before) could pass
	// with a wrongly sent message still on its way on a loaded machine.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the client: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the reader: %v", err)
	}
	if queued, _, err := received.Pending(); err != nil || queued != 0 {
		t.Fatalf("the uncaptured topic's subscriber holds %d message(s) (%v); want nothing sent", queued, err)
	}
}

// A window whose connection is lost sends nothing more, and says so before it returns: its answer
// is in the channel at once, ErrUnavailable, which a caller tells apart from a failure of the
// envelope itself.
func TestAWindowWhoseConnectionIsLostAnswersUnavailableBeforeSending(t *testing.T) {
	t.Parallel()
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	window := client.WindowUntil(nil)
	if err := <-window.PublishAsync(numberedEnvelope(orderedStreamTopic, 0)); err != nil {
		t.Fatalf("first publish answered %v", err)
	}
	server.Stop(t)
	deadline := time.Now().Add(5 * time.Second)
	for client.Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	answer := window.PublishAsync(numberedEnvelope(orderedStreamTopic, 1))
	select {
	case err := <-answer:
		if !errors.Is(err, bus.ErrUnavailable) {
			t.Fatalf("a publish on a lost connection answered %v, want bus.ErrUnavailable", err)
		}
	default:
		t.Fatal("a publish on a lost connection returned with no answer, as if it had sent something")
	}
}

// A JetStream publish NATS answers with no responders while the window's connection stays
// connected is not a lost connection, so it is not ErrUnavailable, which a caller resends on the
// next connection with no attempt counted: it is ErrNotAcknowledged, which the caller backs off.
// Deleting the stream makes the server answer the stream's subject with no responders.
func TestAWindowAnswersNoRespondersOnACurrentConnectionAsNotAcknowledged(t *testing.T) {
	t.Parallel()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	if err := client.JS().DeleteStream(bus.Stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}

	answered := client.WindowUntil(nil).PublishAsync(numberedEnvelope(orderedStreamTopic, 0))
	if err := testwait.Receive(t, "the publish to be answered", answered); !errors.Is(err, bus.ErrNotAcknowledged) || !errors.Is(err, natsgo.ErrNoResponders) || errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a publish answered with no responders on a current connection answered %v, want ErrNotAcknowledged wrapping nats.ErrNoResponders, not ErrUnavailable", err)
	}
	if !client.Connected() {
		t.Fatal("the client lost its connection; the test proves nothing about a current one")
	}
}

// A JetStream publish NATS never acknowledges while the window's connection stays connected, as a
// server that has gone silent leaves it, is ErrNotAcknowledged once the acknowledgement's 5 s run
// out, not ErrUnavailable: the connection never read as lost.
func TestAWindowAnswersAnUnacknowledgedPublishOnACurrentConnectionAsNotAcknowledged(t *testing.T) {
	t.Parallel()
	server, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	window := client.WindowUntil(nil)
	if err := <-window.PublishAsync(numberedEnvelope(orderedStreamTopic, 0)); err != nil {
		t.Fatalf("first publish answered %v", err)
	}

	thaw := server.Freeze(t)
	defer thaw()
	answered := window.PublishAsync(numberedEnvelope(orderedStreamTopic, 1))
	if err := testwait.Receive(t, "the publish to be answered", answered); !errors.Is(err, bus.ErrNotAcknowledged) || !errors.Is(err, natsgo.ErrAsyncPublishTimeout) || errors.Is(err, bus.ErrUnavailable) {
		t.Fatalf("a publish never acknowledged on a current connection answered %v, want ErrNotAcknowledged wrapping nats.ErrAsyncPublishTimeout, not ErrUnavailable", err)
	}
	if !client.Connected() {
		t.Fatal("the client lost its connection while frozen; the test proves nothing about a current one")
	}
}
