package bus

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// An envelope past the server's max payload is refused as the ErrTooLarge it is before its publish
// reads its context, on each path that encodes an envelope: the stored path and the core path
// (PublishAckWithin, which the listener's send and publish routes take) and a role forward
// (StartRequestStoredTo). nats.go refuses such bytes before it sends anything, but its JetStream
// request reads the context first (nats.go v1.55.0 context.go:47-51), and publishConfirmed reads
// it first on the core path and a forward's, so a publish whose deadline its caller had already
// spent was answered with that deadline's error. The listener answers that error 503 "the message
// bus did not answer" with Retry-After: 1, and the sender sends the same bytes again, to the same
// refusal: cmd/listener's TestSendAnswersServiceUnavailableWhenTheBusRefusesThePublish met it in
// CI, on a send whose budget its own reads had spent.
//
// The rows, on each path: an envelope of exactly the max payload is accepted (the stored and core
// paths send it with no header; a forward carries a Nats-Msg-Id, which nats.go counts, so it has no
// such row); one byte past it is ErrTooLarge naming its size and the bound under a live context, a
// deadline already past and a cancelled context (a forward's window of 0 for the last two), and
// nothing of it reaches the subscriber or the stream. The control: an envelope under the bound is
// still refused with the context's own error under the same ended contexts, so the size check
// takes nothing from the answer to a publish whose deadline ended.
func TestAnEnvelopePastTheMaxPayloadIsRefusedBeforeItsContextIsRead(t *testing.T) {
	t.Parallel()
	const issuedAt = 1_800_000_000_000
	for _, tc := range []struct {
		name  string
		topic string
		core  bool
	}{
		{"a topic the stream captures", contracts.LegacyAgentSubject("ses_too_large"), false},
		{"a role lane", contracts.RoleTopicPrefix + "too-large", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			client, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(client.Close)
			if got := client.UsesCoreTransport(tc.topic); got != tc.core {
				t.Fatalf("UsesCoreTransport(%q) = %t, want %t: the case would not exercise the transport it names", tc.topic, got, tc.core)
			}
			subscriber := testnats.Connect(t, uri)
			t.Cleanup(subscriber.Close)
			received, err := subscriber.SubscribeSync(tc.topic)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if err := subscriber.Flush(); err != nil {
				t.Fatalf("flush the subscription: %v", err)
			}
			maxPayload := int(client.Conn.MaxPayload())
			if maxPayload <= 0 {
				t.Fatalf("the server announced a max payload of %d bytes", maxPayload)
			}
			// sized is an envelope whose encoding is exactly size bytes: an envelope with a payload of
			// one byte, its payload grown by the bytes it lacks, each 'a' encoding as one byte.
			sized := func(size int) (contracts.Envelope, []byte) {
				item := contracts.Envelope{
					EventID:        "evt-too-large",
					Source:         "agent",
					SourceEventID:  "source-too-large",
					Topic:          tc.topic,
					DedupeKey:      "publish.too-large",
					IssuedAt:       issuedAt,
					PayloadSummary: "the max payload's edge",
					Payload:        "a",
					TraceID:        "trace-too-large",
				}
				base, err := json.Marshal(item)
				if err != nil {
					t.Fatalf("encode: %v", err)
				}
				item.Payload = strings.Repeat("a", size-len(base)+1)
				data, err := json.Marshal(item)
				if err != nil || len(data) != size {
					t.Fatalf("an envelope sized %d bytes encodes to %d bytes (%v)", size, len(data), err)
				}
				return item, data
			}

			atBound, atBoundData := sized(maxPayload)
			if contracts.DedupeKeyNamesTheUpstreamEvent(atBound) {
				t.Fatalf("the envelope earns a Nats-Msg-Id, whose header would carry it past the bound")
			}
			if _, err := client.PublishAckWithin(context.Background(), atBound); err != nil {
				t.Fatalf("an envelope of exactly the max payload, %d bytes: %v, want it published", maxPayload, err)
			}
			message, err := received.NextMsg(10 * time.Second)
			if err != nil || len(message.Data) != len(atBoundData) {
				t.Fatalf("the subscriber did not receive the envelope of exactly the max payload (%v)", err)
			}

			oversized, oversizedData := sized(maxPayload + 1)
			wantTooLarge := func(t *testing.T, what string, err error) {
				t.Helper()
				if !errors.Is(err, ErrTooLarge) || !errors.Is(err, ErrRefused) {
					t.Fatalf("%s = %v, want an ErrRefused naming ErrTooLarge", what, err)
				}
				if text := err.Error(); !strings.Contains(text, strconv.Itoa(len(oversizedData))) || !strings.Contains(text, strconv.Itoa(maxPayload)) {
					t.Fatalf("%s's refusal %q names neither the envelope's %d bytes nor the max payload's %d", what, text, len(oversizedData), maxPayload)
				}
			}
			past, cancelPast := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancelPast()
			cancelled, cancel := context.WithTimeout(context.Background(), time.Minute)
			cancel()
			small, _ := sized(len(atBoundData) / 2)
			for _, asked := range []struct {
				name string
				ctx  context.Context
				// err is what an envelope under the bound is answered under the same context: nil for
				// the live one.
				err error
			}{
				{"a live context", context.Background(), nil},
				{"a deadline already past", past, context.DeadlineExceeded},
				{"a cancelled context", cancelled, context.Canceled},
			} {
				_, err := client.PublishAckWithin(asked.ctx, oversized)
				wantTooLarge(t, "an envelope one byte past the max payload under "+asked.name, err)
				if asked.err == nil {
					continue
				}
				if _, err := client.PublishAckWithin(asked.ctx, small); !errors.Is(err, asked.err) || errors.Is(err, ErrTooLarge) {
					t.Fatalf("an envelope under the max payload under %s = %v, want the context's own %v", asked.name, err, asked.err)
				}
			}
			agentSubject := contracts.LegacyAgentSubject("ses_too_large_forward")
			for _, window := range []time.Duration{time.Minute, 0} {
				forward, err := client.StartRequestStoredTo(agentSubject, oversized, ForwardMsgID(oversized.DedupeKey, agentSubject), window)
				if forward != nil {
					_, _ = forward.Wait()
				}
				wantTooLarge(t, "a forward one byte past the max payload in a window of "+window.String(), err)
			}

			if err := client.Conn.Flush(); err != nil {
				t.Fatalf("flush the publisher's connection: %v", err)
			}
			if message, err := received.NextMsg(500 * time.Millisecond); err == nil {
				t.Fatalf("the subscriber received %d bytes from a publish that was refused", len(message.Data))
			}
			if !tc.core {
				last, err := client.JS().GetLastMsg(Stream, tc.topic)
				if err != nil || len(last.Data) != len(atBoundData) {
					t.Fatalf("the stream's last message on %s after the refused publishes: %v, want the envelope of exactly the max payload", tc.topic, err)
				}
			}
			if _, err := client.JS().GetLastMsg(Stream, agentSubject); !errors.Is(err, nats.ErrMsgNotFound) {
				t.Fatalf("the stream's last message on %s after the refused forwards: %v, want none", agentSubject, err)
			}
		})
	}
}

// A connection that has never connected has been told no max payload (nats.go v1.55.0 reads it
// from the info only a server's INFO writes: nats.go:4317, :6619-6623), so nothing is refused for
// its size before the publish, which then waits for the connection as it always has: under an
// ended context that wait's error is the answer, and not ErrTooLarge. nats.go's own check answers
// the bytes once the connection has connected.
func TestABoundOfZeroRefusesNothingForItsSize(t *testing.T) {
	t.Parallel()
	client, err := Connect([]string{"nats://127.0.0.1:1"}, WithEnvironment(testnats.SeedOnly("")), WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect in the background: %v", err)
	}
	t.Cleanup(client.Close)
	if bound := client.Connection().MaxPayload(); bound != 0 {
		t.Fatalf("a connection that has never connected reads a max payload of %d bytes, want 0", bound)
	}
	huge := strings.Repeat("a", 64<<20)
	if err := client.payloadRefusal(len(huge)); err != nil {
		t.Fatalf("payloadRefusal of %d bytes at a bound of 0 = %v, want nil", len(huge), err)
	}
	item := contracts.Envelope{
		EventID: "evt-no-bound", Source: "agent", SourceEventID: "source-no-bound", Topic: contracts.LegacyAgentSubject("ses_no_bound"),
		DedupeKey: "publish.no-bound", IssuedAt: contracts.NowMillis(), PayloadSummary: "no bound yet", Payload: huge, TraceID: "trace-no-bound",
	}
	if err := client.EnvelopeRefusal(item); err != nil {
		t.Fatalf("EnvelopeRefusal of a %d-byte payload at a bound of 0 = %v, want nil", len(huge), err)
	}
	past, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := client.PublishAckWithin(past, item); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTooLarge) {
		t.Fatalf("a publish at a bound of 0 under a deadline already past = %v, want the wait's own %v", err, context.DeadlineExceeded)
	}
	var none *Client
	if err := none.EnvelopeRefusal(item); err != nil {
		t.Fatalf("EnvelopeRefusal of a nil client = %v, want nil", err)
	}
}

// EnvelopeRefusal refuses a payload past the server's max payload, naming its size and the bound,
// and nothing at or under it: the listener's routes ask it before anything waits, and an envelope
// whose payload is at the bound is past it only by its other fields, which its publish refuses.
func TestEnvelopeRefusalJudgesThePayloadAgainstTheMaxPayload(t *testing.T) {
	t.Parallel()
	client, err := Connect([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := int(client.Conn.MaxPayload())
	for _, size := range []int{maxPayload - 1, maxPayload} {
		if err := client.EnvelopeRefusal(contracts.Envelope{Payload: strings.Repeat("a", size)}); err != nil {
			t.Fatalf("EnvelopeRefusal of a %d-byte payload against a max payload of %d = %v, want nil", size, maxPayload, err)
		}
	}
	err = client.EnvelopeRefusal(contracts.Envelope{Payload: strings.Repeat("a", maxPayload+1)})
	if !errors.Is(err, ErrTooLarge) || !strings.Contains(err.Error(), strconv.Itoa(maxPayload+1)) || !strings.Contains(err.Error(), strconv.Itoa(maxPayload)) {
		t.Fatalf("EnvelopeRefusal of a %d-byte payload = %v, want ErrTooLarge naming its size and the bound", maxPayload+1, err)
	}
}
