package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
)

// tooLargeWithHeaders reads the numbers out of the bus's refusal of a stored send past the max
// payload (bus.ErrTooLarge): the envelope, its header bytes, their sum, the max payload and the
// envelope those headers leave room for.
var tooLargeWithHeaders = regexp.MustCompile(`an envelope of (\d+) bytes with (\d+) bytes of headers, (\d+) bytes in all, against the server's max payload of (\d+) bytes, which leaves (\d+) bytes for the envelope`)

// The largest message the send route stores is the one whose stored copy fills the server's max
// payload with the headers the copy carries, its Nats-Msg-Id and the Nats-Expected-Stream that
// names the notifications stream, since nats.go counts headers against that payload too. One
// character more is the 413, and its error names that same limit: the envelope it refused, the
// header bytes beside it, their sum past the max payload, and the envelope those headers leave,
// which is the stored copy's own size. A refusal that named only the envelope would name a size
// under the max payload and read as a message that fits.
func TestTheLargestSendTheRouteStoresIsTheLimitItsRefusalNames(t *testing.T) {
	const target = "ses_largest_send"
	subject := contracts.LegacyAgentSubject(target)
	rig := newKnownSessionRig(t)
	if err := rig.deps.sessions.Put(target, session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine", Delivery: session.DeliveryConsumer}); err != nil {
		t.Fatalf("write the session entry of %s: %v", target, err)
	}
	maxPayload := int(rig.harness.client.Connection().MaxPayload())
	send := func(length int) sendAnswer {
		return rig.send(t, target, map[string]any{"message": strings.Repeat("a", length)})
	}
	lo, hi := maxPayload-8192, maxPayload
	if a := send(lo); a.status != http.StatusOK {
		t.Fatalf("a message of %d characters: status %d, want 200: %s", lo, a.status, a.raw)
	}
	if a := send(hi); a.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a message of %d characters: status %d, want 413", hi, a.status)
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		switch a := send(mid); a.status {
		case http.StatusOK:
			lo = mid
		case http.StatusRequestEntityTooLarge:
			hi = mid
		default:
			t.Fatalf("a message of %d characters: status %d: %s", mid, a.status, a.raw)
		}
	}
	stored, err := rig.harness.client.JS().GetLastMsg(bus.Stream, subject)
	if err != nil {
		t.Fatalf("read the last copy on %s: %v", subject, err)
	}
	headers := len("NATS/1.0\r\n") + len("\r\n")
	for key, values := range stored.Header {
		for _, value := range values {
			headers += len(key) + len(": ") + len(value) + len("\r\n")
		}
	}
	refused := send(hi)
	t.Logf("max payload %d; the largest message stored is %d characters, an envelope of %d bytes with headers %v of %d bytes; one more: status %d, %q",
		maxPayload, lo, len(stored.Data), stored.Header, headers, refused.status, refused.Error)
	if len(stored.Data)+headers != maxPayload {
		t.Fatalf("the largest stored copy is %d bytes with %d bytes of headers, %d in all, not the max payload of %d", len(stored.Data), headers, len(stored.Data)+headers, maxPayload)
	}
	if refused.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a message of %d characters: status %d, want 413: %s", hi, refused.status, refused.raw)
	}
	named := tooLargeWithHeaders.FindStringSubmatch(refused.Error)
	if named == nil {
		t.Fatalf("the 413 reads %q, want it to name the envelope, its headers, their sum, the max payload and the envelope the headers leave", refused.Error)
	}
	numbers := make([]int, len(named)-1)
	for i, digits := range named[1:] {
		numbers[i], _ = strconv.Atoi(digits)
	}
	envelope, headerBytes, inAll, limit, leaves := numbers[0], numbers[1], numbers[2], numbers[3], numbers[4]
	if envelope != len(stored.Data)+1 || headerBytes != headers || inAll != maxPayload+1 || limit != maxPayload || leaves != len(stored.Data) {
		t.Fatalf("the 413 names an envelope of %d bytes with %d bytes of headers, %d in all, against %d, leaving %d; want %d with %d, %d in all, against %d, leaving %d, the largest stored copy's size",
			envelope, headerBytes, inAll, limit, leaves, len(stored.Data)+1, headers, maxPayload+1, maxPayload, len(stored.Data))
	}
}
