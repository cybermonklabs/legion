package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// Every stored send carries headers, at least the Nats-Expected-Stream that names the
// notifications stream, and nats.go counts them with the envelope against the server's max
// payload. So the largest envelope a stored send takes is the max payload less the bytes of the
// headers that send carries, and a refusal of one byte more names that limit: the envelope, the
// header bytes, their sum past the max payload, and the envelope the headers leave room for. A
// refusal that named only the envelope would read as an envelope that fits.
//
// Each kind of stored send is a row, with the headers it carries written out here rather than
// read from the bus: an envelope published on its own topic under no message id, one published
// under a message id the caller names, a cursor write's raw bytes under its expected sequence, a
// window's two sends and its request, and a forward. Each row sends an envelope that fills the
// max payload exactly with those headers, which the stream stores, and reads the stored copy's
// headers back to show that the row counted what nats.go sent; then one byte more, which nats.go
// refuses before sending it, and whose error must name the limit the first fitted.
func TestAStoredSendThatFillsTheMaxPayloadWithItsHeadersIsStoredAndOneByteMoreNamesThatLimit(t *testing.T) {
	uri := testnats.URL(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithReplicas(1))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	maxPayload := int(client.Connection().MaxPayload())
	// headerLine is one header line as nats.go writes it, and expectedStream the one every stored
	// send carries; headerBlock is the whole block of the lines given, its first line and the
	// blank line that ends it included.
	headerLine := func(key, value string) int { return len(key) + len(": ") + len(value) + len("\r\n") }
	expectedStream := headerLine(nats.ExpectedStreamHdr, bus.Stream)
	headerBlock := func(lines ...int) int {
		size := len("NATS/1.0\r\n") + len("\r\n")
		for _, line := range lines {
			size += line
		}
		return size
	}
	// sized is an envelope on subject whose encoding, the data a send puts on the wire, is size
	// bytes long. Its source is an agent and its key one no upstream event names, so a publish on
	// its own topic carries no message id (contracts.DedupeKeyNamesTheUpstreamEvent).
	sized := func(t *testing.T, subject string, size int) contracts.Envelope {
		t.Helper()
		item := contracts.Envelope{EventID: "evt-" + id.New(), Source: "agent", Topic: subject, DedupeKey: "max-payload-" + id.New(), IssuedAt: contracts.NowMillis()}
		bare, err := json.Marshal(item)
		if err != nil {
			t.Fatalf("encode the envelope: %v", err)
		}
		item.Payload = strings.Repeat("p", size-len(bare)-len(`,"payload":""`))
		if encoded, err := json.Marshal(item); err != nil || len(encoded) != size {
			t.Fatalf("the envelope encodes to %d bytes (%v), want %d", len(encoded), err, size)
		}
		return item
	}
	msgID := func(prefix string) string { return prefix + "-" + id.New() }
	type send struct {
		name string
		// headers is the size of the header block the send carries.
		headers func(msgID string) int
		send    func(t *testing.T, c *bus.Client, subject, msgID string, size int) error
	}
	withMsgID := func(m string) int { return headerBlock(expectedStream, headerLine(nats.MsgIdHdr, m)) }
	withoutMsgID := func(string) int { return headerBlock(expectedStream) }
	sends := []send{
		{"Publish, under no message id", withoutMsgID, func(t *testing.T, c *bus.Client, subject, _ string, size int) error {
			return c.Publish(sized(t, subject, size))
		}},
		{"PublishStoredAck, under a message id", withMsgID, func(t *testing.T, c *bus.Client, subject, m string, size int) error {
			_, err := c.PublishStoredAck(context.Background(), subject, sized(t, subject, size), m)
			return err
		}},
		{"PublishCursorAt, under its expected sequence", func(string) int {
			return headerBlock(expectedStream, headerLine(nats.ExpectedLastSubjSeqHdr, "0"))
		}, func(_ *testing.T, c *bus.Client, subject, _ string, size int) error {
			_, err := c.PublishCursorAt(context.Background(), subject, []byte(strings.Repeat("c", size)), 0)
			return err
		}},
		{"a window's PublishAsync, under no message id", withoutMsgID, func(t *testing.T, c *bus.Client, subject, _ string, size int) error {
			return <-c.WindowUntil(nil).PublishAsync(sized(t, subject, size))
		}},
		{"a window's PublishStoredAsync, under a message id", withMsgID, func(t *testing.T, c *bus.Client, subject, m string, size int) error {
			return <-c.WindowUntil(nil).PublishStoredAsync(subject, sized(t, subject, size), m)
		}},
		{"a window's Request", withMsgID, func(t *testing.T, c *bus.Client, subject, m string, size int) error {
			return <-c.WindowUntil(nil).Request(subject, sized(t, subject, size), bus.RequestOptions{MsgID: m, Timeout: 5 * time.Second})
		}},
		{"a forward", withMsgID, func(t *testing.T, c *bus.Client, subject, m string, size int) error {
			forward, err := c.StartRequestStoredTo(subject, sized(t, subject, size), m, 5*time.Second)
			if err != nil {
				return err
			}
			return forward.Wait()
		}},
	}
	for i, s := range sends {
		t.Run(s.name, func(t *testing.T) {
			subject := fmt.Sprintf("%sses_max_payload_%d", contracts.AgentTopicPrefix, i)
			fits := msgID("fits")
			headers := s.headers(fits)
			limit := maxPayload - headers
			if err := s.send(t, client, subject, fits, limit); err != nil {
				t.Fatalf("a send of %d bytes with %d bytes of headers, the max payload of %d exactly: %v, want it stored", limit, headers, maxPayload, err)
			}
			stored, err := client.JS().GetLastMsg(bus.Stream, subject)
			if err != nil {
				t.Fatalf("read the copy the send stored on %s: %v", subject, err)
			}
			var lines []int
			for key, values := range stored.Header {
				for _, value := range values {
					lines = append(lines, headerLine(key, value))
				}
			}
			if got := headerBlock(lines...); got != headers || len(stored.Data) != limit {
				t.Fatalf("the stored copy has %d bytes and headers %v, %d bytes of them; the row counted %d bytes and %d bytes of headers", len(stored.Data), stored.Header, got, limit, headers)
			}

			over := msgID("over")
			headers = s.headers(over)
			limit = maxPayload - headers
			err = s.send(t, client, subject, over, limit+1)
			t.Logf("one byte past the limit of %d bytes, with %d bytes of headers: %v", limit, headers, err)
			if !errors.Is(err, bus.ErrTooLarge) {
				t.Fatalf("a send of %d bytes with %d bytes of headers = %v, want bus.ErrTooLarge", limit+1, headers, err)
			}
			for _, want := range []string{
				fmt.Sprintf("an envelope of %d bytes with %d bytes of headers", limit+1, headers),
				fmt.Sprintf("%d bytes in all", maxPayload+1),
				fmt.Sprintf("the server's max payload of %d bytes", maxPayload),
				fmt.Sprintf("leaves %d bytes for the envelope", limit),
			} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("the refusal reads %q, want it to say %q", err, want)
				}
			}
			if copies := storedOn(t, client, subject); copies != 1 {
				t.Fatalf("the stream holds %d copies on %s, want only the one that fitted", copies, subject)
			}
		})
	}
}
