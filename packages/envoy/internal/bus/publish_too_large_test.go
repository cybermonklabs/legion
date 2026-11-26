package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// NATS takes a message only whole, and two limits decide what whole can be: nats.go refuses a
// message past the server's max payload before sending it, and the server closes the connection
// over a protocol line past its max control line (4 KiB by default), which a long enough subject
// makes. Both are ErrTooLarge, which names the size. A subject NATS does not accept, one holding
// whitespace or an empty token (which no stream's subjects match, so a JetStream publish waits out
// its deadline for an answer that never comes), is ErrInvalidSubject. Each is an ErrRefused, which
// a caller answers as a refusal rather than a failure to retry, and none may close the connection
// every subscription and watcher of the client runs on.
func TestPublishRefusesAnEnvelopeNATSCannotTakeWhole(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := func(topic, payload string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "event",
			Source:         "github",
			SourceEventID:  "delivery",
			Topic:          topic,
			DedupeKey:      "github.delivery",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "push to a branch",
			Payload:        payload,
			TraceID:        "trace",
		}
	}
	push := "notifications.github.acme.widgets.push.branch."
	for _, tc := range []struct {
		name     string
		envelope contracts.Envelope
		want     error
		says     string
	}{
		{"a subject past the server's protocol line", envelope(push+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5046 bytes"},
		{"a subject one byte past the bound", envelope(push+strings.Repeat("r", maxSubjectBytes-len(push)+1), "{}"), ErrTooLarge, "a subject of 4033 bytes, past 4032"},
		{"a payload past the server's max payload", envelope(push+"main", strings.Repeat("p", 1<<20)), ErrTooLarge, "max payload of 1048576 bytes"},
		{"a core subject past the server's protocol line", envelope(contracts.RoleTopicPrefix+strings.Repeat("r", 5000), "{}"), ErrTooLarge, "a subject of 5019 bytes"},
		{"a subject holding a space", envelope(push+"ci yml", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.ci yml"`},
		{"a subject holding an empty token", envelope(contracts.LegacyAgentSubject("sess..x"), "{}"), ErrInvalidSubject, `"notifications.agent.sess..x"`},
		{"a subject ending in an empty token", envelope(push+"main.", "{}"), ErrInvalidSubject, `"notifications.github.acme.widgets.push.branch.main."`},
		{"a core subject holding an empty token", envelope(contracts.RoleTopicPrefix+"a..b", "{}"), ErrInvalidSubject, `"notifications.role.a..b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := client.Publish(tc.envelope)
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrRefused) {
				t.Fatalf("publish error = %v, want %v, an ErrRefused", err, tc.want)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("publish error = %q, want it to say %q", err, tc.says)
			}
			if !client.Conn.IsConnected() {
				t.Fatalf("the refusal left the connection %v, want it connected", client.Conn.Status())
			}
			// The control: the same connection still publishes an envelope that fits.
			if err := client.Publish(envelope(push+"main", "{}")); err != nil {
				t.Fatalf("publish after the refusal: %v", err)
			}
		})
	}
	// The bound itself publishes: its line, reply inbox and sizes included, is within the server's.
	if err := client.Publish(envelope(push+strings.Repeat("r", maxSubjectBytes-len(push)), "{}")); err != nil {
		t.Fatalf("publish on a subject of %d bytes: %v", maxSubjectBytes, err)
	}
	if !client.Conn.IsConnected() {
		t.Fatalf("a subject at the bound left the connection %v, want it connected", client.Conn.Status())
	}
}

// TooLargeToPublish measures an envelope as a publish encodes it and refuses it past the server's
// max payload in the words of the publish's own refusal, sending nothing. The bound is exact: an
// envelope whose JSON is the max payload to the byte is not refused, and one byte more is. The
// JSON is what counts, not the payload's length: a payload of '<', which the encoding writes as
// six bytes each (\u003c), is refused though the payload alone is a quarter of the bound. A client
// that has not connected yet knows no max payload and refuses nothing, leaving the publish to
// answer for itself.
func TestTooLargeToPublishRefusesWhatThePublishWouldWithoutSendingIt(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := int(client.Conn.MaxPayload())
	envelope := func(payload string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "event",
			Source:         "agent",
			SourceEventID:  "source-event",
			Topic:          "notifications.team.too-large",
			DedupeKey:      "publish.too-large",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "a message measured against the max payload",
			Payload:        payload,
			TraceID:        "trace",
		}
	}
	encodedSize := func(item contracts.Envelope) int {
		encoded, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode the envelope: %v", err)
		}
		return len(encoded)
	}
	// Each byte of a payload of 'p' encodes as one, so the payload that brings the envelope to the
	// max payload exactly is the bound less what the rest of the envelope weighs.
	fill := maxPayload - (encodedSize(envelope("p")) - 1)
	atBound, pastBound := envelope(strings.Repeat("p", fill)), envelope(strings.Repeat("p", fill+1))
	if size := encodedSize(atBound); size != maxPayload {
		t.Fatalf("UNKNOWN: the envelope at the bound encodes to %d bytes, want the max payload, %d", size, maxPayload)
	}
	escaped := envelope(strings.Repeat("<", maxPayload/4))

	sentBefore := client.Conn.Stats().OutMsgs
	if err := client.TooLargeToPublish(atBound); err != nil {
		t.Fatalf("an envelope of exactly the max payload, %d bytes, was refused: %v", maxPayload, err)
	}
	for _, refused := range []struct {
		name string
		item contracts.Envelope
	}{
		{"an envelope one byte past the max payload", pastBound},
		{"a payload a quarter of the bound that encodes past it", escaped},
	} {
		err := client.TooLargeToPublish(refused.item)
		if !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v, want an ErrRefused naming ErrTooLarge", refused.name, err)
		}
		says := fmt.Sprintf("an envelope of %d bytes against the server's max payload of %d bytes", encodedSize(refused.item), maxPayload)
		if !strings.Contains(err.Error(), says) {
			t.Fatalf("%s: %q, want it to say %q", refused.name, err, says)
		}
	}
	if sent := client.Conn.Stats().OutMsgs - sentBefore; sent != 0 {
		t.Fatalf("the size checks sent %d messages, want none", sent)
	}

	// The publish of the same envelope is refused in the same words.
	checked := client.TooLargeToPublish(pastBound)
	if published := client.Publish(pastBound); published == nil || published.Error() != checked.Error() {
		t.Fatalf("the publish of the envelope one byte past the bound answered %v, want the check's own refusal %q", published, checked)
	}

	t.Run("a client that has not connected", func(t *testing.T) {
		unconnected, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(testnats.SeedOnly("")), WithConnectInBackground())
		if err != nil {
			t.Fatalf("connect in the background: %v", err)
		}
		t.Cleanup(unconnected.Close)
		if err := unconnected.TooLargeToPublish(pastBound); err != nil {
			t.Fatalf("a client that knows no max payload refused an envelope of %d bytes: %v", maxPayload+1, err)
		}
	})
}

// A publish whose body is past the server's max payload is refused for its size whatever is left
// of its context. nats.go refuses such bytes before sending them, but only after a JetStream
// publish has read its context, and a core publish is refused on an ended context first
// (publishConfirmed), so bytes the bus can never send were answered as a bus that did not answer
// whenever the caller's deadline had run out before the publish. Each path the bus publishes a
// body on is asked under a context that has already ended: a stored publish (a team topic), a core
// publish (a role topic) and a raw stored write (PublishCursorAt). The control is each path with a
// body within the bound under the same ended context: it is refused for that context and not for
// its size, so the check cannot pass by refusing every publish as too large.
func TestAPublishPastTheMaxPayloadIsRefusedForItsSizeWhateverIsLeftOfItsContext(t *testing.T) {
	t.Parallel()
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := int(client.Conn.MaxPayload())
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	envelope := func(topic, payload string) contracts.Envelope {
		return contracts.Envelope{
			EventID:        "event",
			Source:         "agent",
			SourceEventID:  "source-event",
			Topic:          topic,
			DedupeKey:      "publish.ended-context",
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: "a message under an ended context",
			Payload:        payload,
			TraceID:        "trace",
		}
	}
	stored, core := "notifications.team.ended-context", contracts.RoleTopicPrefix+"ended-context"
	for _, path := range []struct {
		name    string
		publish func(payload string) error
	}{
		{"a stored publish", func(payload string) error {
			_, err := client.PublishAckWithin(ended, envelope(stored, payload))
			return err
		}},
		{"a core publish", func(payload string) error {
			return client.PublishCoreContext(ended, core, envelope(core, payload))
		}},
		{"a raw stored write", func(payload string) error {
			_, err := client.PublishCursorAt(ended, stored+"-cursor", []byte(payload), 0)
			return err
		}},
	} {
		past := path.publish(strings.Repeat("p", maxPayload+1))
		if !errors.Is(past, ErrTooLarge) || !strings.Contains(past.Error(), fmt.Sprintf("max payload of %d bytes", maxPayload)) {
			t.Errorf("%s of a body past the max payload under an ended context: %v, want ErrTooLarge naming the max payload of %d bytes", path.name, past, maxPayload)
		}
		if within := path.publish("{}"); !errors.Is(within, context.Canceled) || errors.Is(within, ErrTooLarge) {
			t.Errorf("control: %s of a body within the max payload under an ended context: %v, want the context's own error", path.name, within)
		}
	}
}

// The size checks read the connection's max payload before anything else of a publish, so they
// must touch no connection that is not there. A nil client (a listener's deps built without one)
// and a zero Client know no max payload, so TooLargeToPublish refuses nothing on either, and a zero
// Client's core publish of a body past any server's max payload is answered as it was before the
// checks existed, by the connect that finds no connection (here under a context that has already
// ended, so nothing is dialled), never by a panic and never as too large.
func TestTheSizeChecksTouchNoConnectionThatIsNotThere(t *testing.T) {
	t.Parallel()
	big := contracts.Envelope{
		EventID:        "event",
		Source:         "agent",
		SourceEventID:  "source-event",
		Topic:          contracts.RoleTopicPrefix + "no-connection",
		DedupeKey:      "publish.no-connection",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "a body past any server's max payload",
		Payload:        strings.Repeat("p", 2<<20),
		TraceID:        "trace",
	}
	// answer runs call and reports a panic as this test's failure rather than the binary's.
	answer := func(name string, call func() error) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("%s panicked: %v", name, recovered)
				err = nil
			}
		}()
		return call()
	}
	var none *Client
	zero := &Client{}
	for name, client := range map[string]*Client{"a nil client": none, "a zero Client": zero} {
		if err := answer("TooLargeToPublish on "+name, func() error { return client.TooLargeToPublish(big) }); err != nil {
			t.Errorf("TooLargeToPublish on %s = %v, want nil: it knows no max payload", name, err)
		}
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	err := answer("a zero Client's core publish", func() error { return zero.PublishCoreContext(ended, big.Topic, big) })
	if err == nil || errors.Is(err, ErrTooLarge) {
		t.Errorf("a zero Client's core publish of a body past any max payload = %v, want the connect's own error", err)
	}
}
