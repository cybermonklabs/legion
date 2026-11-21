package testnats

import (
	"strings"
	"testing"
)

// clientStream is a client's stream to the server with the operations the relay must see in it:
// its CONNECT, two PINGs, two direct gets (a PUB and an hpub), and payloads that hold a whole PUB
// line, one to a direct-get subject among them, a PING line and a CRLF, which are bytes of a
// message and none of those operations. It ends at the end of an operation.
const clientStream = "CONNECT {\"verbose\":false,\"headers\":true}\r\nPING\r\n" +
	"SUB _INBOX.reply 1\r\n" +
	"PUB $JS.API.INFO _INBOX.reply.1 0\r\n\r\n" +
	"PUB notes.one 21\r\nPUB notes.two 2\r\nab\r\n\r\n" +
	"HPUB notes.headed _INBOX.reply.2 22 27\r\nNATS/1.0\r\nKey: one\r\n\r\nhello\r\n" +
	"hpub notes.lower 12 12\r\nNATS/1.0\r\n\r\n\r\n" +
	"pub notes.ping 6\r\nPING\r\n\r\n" +
	"PUB $JS.API.DIRECT.GET.KV_b.$KV.b.k _INBOX.reply.3 0\r\n\r\n" +
	"hpub $JS.API.DIRECT.GET.KV_b _INBOX.reply.4 12 21\r\nNATS/1.0\r\n\r\n{\"seq\":1}\r\n" +
	"PUB notes.decoy 35\r\nPUB $JS.API.DIRECT.GET.KV_b.x 0\r\n\r\n\r\n" +
	"ping\r\nUNSUB 1\r\nPONG\r\n"

// The relay's record of a client's stream finds its CONNECT, its PINGs and its direct gets, and
// where each operation ends, however the stream is cut: a hold stops a client only between two
// operations, and the relay's own PING goes in there, so a boundary inside a payload would put the
// PING inside a message, and a PING counted from a payload would leave the relay waiting for a
// PONG the server never sends. A direct get counted from a payload would refuse a Reload the
// server is safe to take.
func TestTheRelaysRecordOfAClientsStreamFindsItsOperationsWhereverTheStreamIsCut(t *testing.T) {
	for _, cut := range []int{1, 2, 3, 7, 64, len(clientStream)} {
		var stream protocolStream
		for start := 0; start < len(clientStream); start += cut {
			stream.wrote([]byte(clientStream[start:min(start+cut, len(clientStream))]))
		}
		if stream.broken != nil || !stream.connected || stream.pings != 2 || stream.directGets != 2 || !stream.atBoundary() {
			t.Errorf("pieces of %d bytes: broken %v, connected %v, pings %d, direct gets %d, at a boundary %v; want nil, true, 2, 2, true",
				cut, stream.broken, stream.connected, stream.pings, stream.directGets, stream.atBoundary())
		}
	}
	// A message is whole at the LF after its payload, so neither the payload's last byte nor the CR
	// after it ends the operation; toBoundary says how far the next bytes take it.
	var stream protocolStream
	stream.wrote([]byte("PUB notes.cut 10\r\n01234"))
	for _, step := range []struct {
		piece      string
		toBoundary int
		at         bool
	}{{"56789", 5, false}, {"\r", 1, false}, {"\nPING\r\n", 1, true}} {
		if got := stream.toBoundary([]byte(step.piece)); got != step.toBoundary {
			t.Errorf("toBoundary(%q) = %d, want %d", step.piece, got, step.toBoundary)
		}
		stream.wrote([]byte(step.piece[:stream.toBoundary([]byte(step.piece))]))
		if stream.atBoundary() != step.at {
			t.Errorf("after %q at a boundary %v, want %v", step.piece, stream.atBoundary(), step.at)
		}
	}
	// From a boundary, the next operation runs to the end of its payload.
	if got := stream.toBoundary([]byte("PUB notes.next 3\r\nabc\r\nPING\r\n")); got != len("PUB notes.next 3\r\nabc\r\n") {
		t.Errorf("toBoundary from a boundary = %d, want the PUB and its payload, %d", got, len("PUB notes.next 3\r\nabc\r\n"))
	}
}

// What the server sends the client reaches it whole but for the PONGs the relay drops, each one
// asked about in order however the stream is cut, and a PONG in a message's payload is the
// message's, not a PONG.
func TestTheRelayDropsOnlyThePongsItIsToldToWhereverTheServersStreamIsCut(t *testing.T) {
	const info = "INFO {\"server_id\":\"a\",\"headers\":true}\r\n"
	const served = info +
		"PONG\r\n" +
		"MSG notes.one 1 _INBOX.reply 6\r\nPONG\r\n\r\n" +
		"HMSG notes.two 2 12 18\r\nNATS/1.0\r\n\r\nPONG\r\n\r\n" +
		"PONG\r\n" +
		"PING\r\n+OK\r\n-ERR 'Permissions Violation'\r\n" +
		"pong\r\n"
	// The second and the third of the three PONGs are the relay's.
	const want = info +
		"PONG\r\n" +
		"MSG notes.one 1 _INBOX.reply 6\r\nPONG\r\n\r\n" +
		"HMSG notes.two 2 12 18\r\nNATS/1.0\r\n\r\nPONG\r\n\r\n" +
		"PING\r\n+OK\r\n-ERR 'Permissions Violation'\r\n"
	for _, cut := range []int{1, 2, 3, 7, 64, len(served)} {
		stream := protocolStream{fromServer: true}
		pongs := 0
		var got strings.Builder
		for start := 0; start < len(served); start += cut {
			got.Write(stream.filter([]byte(served[start:min(start+cut, len(served))]), func() bool {
				pongs++
				return pongs > 1
			}))
		}
		if got.String() != want || pongs != 3 || stream.broken != nil {
			t.Errorf("pieces of %d bytes passed on %q with %d PONGs asked about (broken %v), want %q and 3", cut, got.String(), pongs, stream.broken, want)
		}
	}
}

// A stream that stops reading as NATS protocol says so, and nothing after it is read as
// operations: a client's stays off every boundary, so a hold fails on it rather than put a PING
// inside it, and the server's is passed on as it comes.
func TestTheRelayRefusesAStreamItCannotRead(t *testing.T) {
	for _, row := range []struct {
		name, stream, says string
		fromServer         bool
	}{
		{"a PUB with no size", "PUB notes.one\r\n", "a PUB line has 1 arguments", false},
		{"a PUB with too many arguments", "PUB a b c 3\r\nabc\r\n", "a PUB line has 4 arguments", false},
		{"an HPUB with one size", "HPUB notes.one 3\r\nabc\r\n", "an HPUB line has 2 arguments", false},
		{"a size that is not a number", "PUB notes.one three\r\nabc\r\n", "is not a count of bytes", false},
		{"a negative size", "PUB notes.one -1\r\n", "is not a count of bytes", false},
		{"an endless control line", "PUB " + strings.Repeat("a", maxControlLine), "runs past", false},
		{"a MSG with no sid", "MSG notes.one 3\r\nabc\r\n", "a MSG line has 2 arguments", true},
		{"an HMSG with one size", "HMSG notes.one 1 3\r\nabc\r\n", "an HMSG line has 3 arguments", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			stream := protocolStream{fromServer: row.fromServer}
			if row.fromServer {
				passed := stream.filter([]byte(row.stream), func() bool { return true })
				after := stream.filter([]byte("PONG\r\n"), func() bool { return true })
				if string(passed) != row.stream || string(after) != "PONG\r\n" {
					t.Errorf("passed on %q then %q, want everything as it came", passed, after)
				}
			} else {
				stream.wrote([]byte(row.stream))
				stream.wrote([]byte("PING\r\n"))
				if stream.atBoundary() || stream.pings != 0 {
					t.Errorf("at a boundary %v with %d PINGs after the refusal, want neither", stream.atBoundary(), stream.pings)
				}
			}
			if stream.broken == nil || !strings.Contains(stream.broken.Error(), row.says) {
				t.Fatalf("broken is %v, want an error saying %q", stream.broken, row.says)
			}
		})
	}
}
