package testnats

import (
	"bufio"
	"maps"
	"net"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testwait"
)

// A hold lets a client's handshake PING, the first operation after its CONNECT, through and waits
// for it, so the client gets its PONG while the hold is in force, and the hold returns only once
// that PING has reached the server. A client held between the two would have its CONNECT processed
// and its first PONG unsent, and nats-server's first-PING timer sends such a client a PING of its
// own 2 to 2.4 s after its CONNECT (relay.hold), which its handshake reads where it waits for the
// PONG. The row writes the PING only once the hold is in force.
func TestAHoldLetsAClientsHandshakePingThrough(t *testing.T) {
	server, uri := Start(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(uri, "nats://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "INFO ") {
		t.Fatalf("the server's first line was %q (%v), want its INFO", line, err)
	}
	if _, err := conn.Write([]byte("CONNECT {\"verbose\":false,\"lang\":\"go\"}\r\n")); err != nil {
		t.Fatal(err)
	}
	testwait.Until(t, "the relay to write the client's CONNECT", func() (bool, bool) {
		server.relay.mu.Lock()
		defer server.relay.mu.Unlock()
		for j := range server.relay.joins {
			if j.sent.connected {
				return true, true
			}
		}
		return false, false
	})
	type held struct {
		release func()
		err     error
		// pings is how many PINGs of the client the relay had written when the hold returned.
		pings int64
	}
	holding := make(chan held, 1)
	go func() {
		release, err := server.relay.hold()
		server.relay.mu.Lock()
		var pings int64
		for j := range server.relay.joins {
			pings += j.sent.pings
		}
		server.relay.mu.Unlock()
		holding <- held{release, err, pings}
	}()
	testwait.Until(t, "the hold to be in force", func() (bool, bool) {
		server.relay.mu.Lock()
		defer server.relay.mu.Unlock()
		return server.relay.holds > 0, server.relay.holds > 0
	})
	if _, err := conn.Write([]byte("PING\r\n")); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		lines <- line
	}()
	if line := testwait.Receive(t, "the PONG to the client's handshake PING, sent during a hold", lines); line != "PONG\r\n" {
		t.Fatalf("the server answered the handshake's PING with %q, want PONG", line)
	}
	h := testwait.Receive(t, "the hold to stop the client past its handshake's PING", holding)
	defer h.release()
	if h.err != nil {
		t.Fatalf("hold: %v", h.err)
	}
	if h.pings != 1 {
		t.Fatalf("the hold returned with %d of the client's PINGs written to the server, want 1: it did not wait for the handshake's PING", h.pings)
	}
}

// The relay keeps the address of a connection it holds, and of one it has let go while the server
// still lists it, and forgets one it has let go that the server no longer lists: a client that
// later reaches the server around the relay from that local port is then not taken for one the
// relay held (Server.ordered).
func TestTheRelayForgetsALetGoConnectionTheServerNoLongerLists(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	joinedNow := conn.LocalAddr().String()
	const listedLetGo, unlistedLetGo = "127.0.0.1:1", "127.0.0.1:2"
	r := &relay{
		joins:         map[*joined]struct{}{{upstream: conn}: {}},
		upstreamsHeld: map[string]bool{joinedNow: true, listedLetGo: true, unlistedLetGo: true},
	}
	if got, want := r.upstreams(map[string]bool{listedLetGo: true}), map[string]bool{joinedNow: true, listedLetGo: true}; !maps.Equal(got, want) {
		t.Fatalf("with the server listing %s: the relay holds %v, want %v", listedLetGo, got, want)
	}
	if got, want := r.upstreams(map[string]bool{}), map[string]bool{joinedNow: true}; !maps.Equal(got, want) {
		t.Fatalf("with the server listing none: the relay holds %v, want %v, the connection it still holds", got, want)
	}
}

// clientStream is a client's stream to the server with the operations the relay must see in it:
// its CONNECT, two PINGs, and payloads that hold a whole PUB line, a PING line and a CRLF, which
// are bytes of a message and none of those operations. It ends at the end of an operation.
const clientStream = "CONNECT {\"verbose\":false,\"headers\":true}\r\nPING\r\n" +
	"SUB _INBOX.reply 1\r\n" +
	"PUB $JS.API.INFO _INBOX.reply.1 0\r\n\r\n" +
	"PUB notes.one 21\r\nPUB notes.two 2\r\nab\r\n\r\n" +
	"HPUB notes.headed _INBOX.reply.2 22 27\r\nNATS/1.0\r\nKey: one\r\n\r\nhello\r\n" +
	"hpub notes.lower 12 12\r\nNATS/1.0\r\n\r\n\r\n" +
	"pub notes.ping 6\r\nPING\r\n\r\n" +
	"ping\r\nUNSUB 1\r\nPONG\r\n"

// The relay's record of a client's stream finds its CONNECT, its PINGs and the operations after
// its CONNECT, and where each operation ends, however the stream is cut: a hold stops a client only
// between two operations, and the relay's own PING goes in there, so a boundary inside a payload
// would put the PING inside a message, and a PING counted from a payload would leave the relay
// waiting for a PONG the server never sends. A stream of its CONNECT alone is not where a hold may
// stop it (settled): the next bytes, if they begin a PING, are its handshake's PING, which a hold
// lets through (relay.hold).
func TestTheRelaysRecordOfAClientsStreamFindsItsOperationsWhereverTheStreamIsCut(t *testing.T) {
	for _, cut := range []int{1, 2, 3, 7, 64, len(clientStream)} {
		var stream protocolStream
		for start := 0; start < len(clientStream); start += cut {
			stream.wrote([]byte(clientStream[start:min(start+cut, len(clientStream))]))
		}
		if stream.broken != nil || !stream.connected || stream.pings != 2 || stream.afterConnect != 10 || !stream.settled() {
			t.Errorf("pieces of %d bytes: broken %v, connected %v, pings %d, after the CONNECT %d, settled %v; want nil, true, 2, 10, true",
				cut, stream.broken, stream.connected, stream.pings, stream.afterConnect, stream.settled())
		}
	}
	var connectOnly protocolStream
	connectOnly.wrote([]byte("CONNECT {\"verbose\":false}\r\n"))
	if !connectOnly.connected || connectOnly.afterConnect != 0 || !connectOnly.atBoundary() || connectOnly.settled() {
		t.Errorf("a stream of its CONNECT alone: connected %v, after the CONNECT %d, at a boundary %v, settled %v; want true, 0, true, false",
			connectOnly.connected, connectOnly.afterConnect, connectOnly.atBoundary(), connectOnly.settled())
	}
	for next, want := range map[string]bool{"PING\r\n": true, "ping\r\nSUB a 1\r\n": true, "PI": true, "PUB a 0\r\n\r\n": false, "SUB a 1\r\n": false} {
		if got := connectOnly.handshakePing([]byte(next)); got != want {
			t.Errorf("after a CONNECT alone, %q is the handshake's PING: %v, want %v", next, got, want)
		}
	}
	var pinged protocolStream
	pinged.wrote([]byte("CONNECT {\"verbose\":false}\r\nPING\r\n"))
	if pinged.handshakePing([]byte("PING\r\n")) || !pinged.settled() {
		t.Errorf("after a CONNECT and its PING: a second PING is the handshake's %v, settled %v; want false, true", pinged.handshakePing([]byte("PING\r\n")), pinged.settled())
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
