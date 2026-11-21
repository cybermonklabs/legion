package testnats

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// ReplyHold relays a client's TCP connections to a NATS server and can hold the replies to the
// requests the client sends on a subject: the request reaches the server, which answers it, and the
// hold keeps that answer from the client, so the client waits on a reply that never comes, which
// is what it sees of a request whose reply the connection's drop lost. Drop then ends every
// connection the hold is relaying, as a network that ends a connection does, and keeps accepting,
// so the client reconnects through it at its next attempt; Cut ends them too and refuses every
// reconnect until Restore, as Proxy.Cut does, so a test can see what the client does while it is
// disconnected, however long the box takes. It can hold a flush's PONG the same way (HoldFlush), so
// a flush waits on a PONG that never comes, which is what it sees of a flush whose PONG the drop
// lost. A new connection's replies and flushes are not held unless Hold or HoldFlush names them
// again. Every other protocol line is relayed as it is, so the connection is a real one on a real
// server.
type ReplyHold struct {
	listener net.Listener
	target   string
	url      string

	mu     sync.Mutex
	prefix string
	// replies are the reply subjects of the held requests, whose answers the hold drops.
	replies map[string]struct{}
	// held is what the hold reports of the replies it holds now; nil while it holds none.
	held *HeldReplies
	// flushPrefix and flush are HoldFlush's: the subject prefix of the publishes whose flush's
	// PONG the hold keeps, and what it reports of it; flush is nil while it holds none.
	flushPrefix string
	flush       *HeldFlush
	conns       map[net.Conn]struct{}
	// cut is set from Cut until Restore: every connection the hold accepts is closed at once.
	cut bool
}

// HeldFlush is what a flush hold (ReplyHold.HoldFlush) reports: the PONG of a publish's flush
// withheld.
type HeldFlush struct {
	withheld chan struct{}
}

// Withheld is closed once the hold has kept from the client the PONG answering the first PING the
// client sent after a publish on the hold's prefix: that publish's own flush, for a publish that
// flushes behind it (bus.Client's publishConfirmed), is waiting for that PONG from then on, and
// waits until the connection is dropped, which nats.go answers it with as a close (processOpErr
// clears the pending flushes).
func (f *HeldFlush) Withheld() <-chan struct{} { return f.withheld }

// HeldReplies is what a hold (ReplyHold.Hold) reports, as two events: a reply withheld, and the
// PONG of the client's first flush after a held request, forwarded.
type HeldReplies struct {
	withheld, flushed chan struct{}
}

// Withheld is closed once the hold has kept the first reply to a request on its prefix from the
// client. A request that sends no flush behind its publish (nats.go's own requests, a legacy
// JetStream call or a KV read or write) is waiting for that reply from then on: its publish has
// reached the server, which answered it.
func (r *HeldReplies) Withheld() <-chan struct{} { return r.withheld }

// Flushed is closed once the PONG answering the first PING the client sent after a request on the
// hold's prefix has been forwarded to the client: that request's own flush, for a request that
// flushes behind its publish (bus.Client's publishConfirmed). A PONG answers the PINGs of its
// connection in order, so the hold counts them on each connection and knows which one it is,
// whether it comes before the reply or after it: nats-server answers a PING on the connection's
// own goroutine and a JetStream API request on another, so the PONG often comes first. Nothing
// closes it before that PONG is forwarded. A request that flushes is past its flush and waiting
// for its reply once both events are in.
func (r *HeldReplies) Flushed() <-chan struct{} { return r.flushed }

// StartReplyHold relays to the NATS server at target, a client URL such as URL returns, on a
// loopback port and address of its own (testport.LoopbackIP) until the test ends. URL is the same
// server through the hold.
func StartReplyHold(t testing.TB, target string) *ReplyHold {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the hold relays to: %v", target, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the reply hold: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	h := &ReplyHold{listener: listener, target: server.Host, url: relayed.String(), conns: map[net.Conn]struct{}{}}
	t.Cleanup(h.close)
	go h.serve()
	return h
}

// URL is the NATS URL of the hold's address: the target's, credentials included, with the host and
// port the hold listens on.
func (h *ReplyHold) URL() string { return h.url }

// Hold holds the reply to every request the client sends from now on whose subject starts with
// prefix, until Drop, and reports when it has withheld the first of them and when the first PONG
// after it reached the client (HeldReplies).
func (h *ReplyHold) Hold(prefix string) *HeldReplies {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prefix = prefix
	h.replies = map[string]struct{}{}
	h.held = &HeldReplies{withheld: make(chan struct{}), flushed: make(chan struct{})}
	return h.held
}

// HoldFlush holds the PONG of the flush of every publish the client sends from now on whose subject
// starts with prefix - the PONG answering the first PING the client sends on the connection after
// such a publish - until Drop, and reports when it has withheld the first of them (HeldFlush). A
// test that drops the connection then drops it while that flush is still waiting.
func (h *ReplyHold) HoldFlush(prefix string) *HeldFlush {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushPrefix = prefix
	h.flush = &HeldFlush{withheld: make(chan struct{})}
	return h.flush
}

// Drop holds nothing more and closes every connection the hold is relaying: each client through it
// is disconnected at once, and reconnects through it.
func (h *ReplyHold) Drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked()
}

// Cut is Drop that also refuses every connection the hold accepts until Restore, as a dial the
// network refuses ends: each client through it is disconnected at once and stays disconnected, its
// reconnects failing, until Restore.
func (h *ReplyHold) Cut() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cut = true
	h.dropLocked()
}

// Restore ends a Cut: the hold accepts and relays connections again, so a client through it
// reconnects at its next attempt.
func (h *ReplyHold) Restore() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cut = false
}

// dropLocked holds nothing more and closes every connection the hold is relaying; the caller holds
// h.mu.
func (h *ReplyHold) dropLocked() {
	h.prefix, h.replies, h.held = "", nil, nil
	h.flushPrefix, h.flush = "", nil
	for conn := range h.conns {
		_ = conn.Close()
		delete(h.conns, conn)
	}
}

// flushes is one relayed connection's count of the PINGs the client sent and the PONGs forwarded
// to it, under ReplyHold.mu, the PONG a held request's flush waits for, and the PONG a flush hold
// keeps from the client.
type flushes struct {
	pings, pongs int
	// due is the PONG, by its count on this connection, that answers the first PING the client
	// sent after a held request, and 0 while none is due; held is the hold it belongs to.
	due  int
	held *HeldReplies
	// kept is the PONG, by its count on this connection, that answers the first PING the client
	// sent after a publish a flush hold names, which the hold keeps from the client, and 0 while
	// none is; keptFor is the flush hold it belongs to.
	kept    int
	keptFor *HeldFlush
}

// request notes the reply subject of a request the client sent on subject when its reply is to be
// held, and that the next PING the client sends on the connection is that request's flush.
func (h *ReplyHold) request(conn *flushes, subject, reply string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.replies != nil && reply != "" && strings.HasPrefix(subject, h.prefix) {
		h.replies[reply] = struct{}{}
		if conn.due == 0 {
			conn.due, conn.held = conn.pings+1, h.held
		}
	}
}

// published notes that the next PING the client sends on the connection is the flush of a publish
// it sent on subject, when a flush hold names that subject.
func (h *ReplyHold) published(conn *flushes, subject string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.flush != nil && strings.HasPrefix(subject, h.flushPrefix) && conn.kept == 0 {
		conn.kept, conn.keptFor = conn.pings+1, h.flush
	}
}

// pinged counts a PING the client sent on conn.
func (h *ReplyHold) pinged(conn *flushes) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn.pings++
}

// withhold reports whether the server's message on subject is a reply the hold drops, and when it
// is, reports it withheld.
func (h *ReplyHold) withhold(subject string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.replies[subject]; !ok || h.held == nil {
		return false
	}
	closeOnce(h.held.withheld)
	return true
}

// keepPong reports whether the PONG the server sent next on conn is the one a flush hold keeps
// from the client, and when it is, counts it and reports it withheld. A PONG answers the PINGs of
// its connection in order, so its count says which PING it answers.
func (h *ReplyHold) keepPong(conn *flushes) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if conn.kept == 0 || conn.pongs+1 != conn.kept {
		return false
	}
	conn.pongs++
	closeOnce(conn.keptFor.withheld)
	conn.kept, conn.keptFor = 0, nil
	return true
}

// ponged counts a PONG forwarded to the client on conn, and reports the held request's flush
// answered when it is the PONG that flush waits for.
func (h *ReplyHold) ponged(conn *flushes) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conn.pongs++
	if conn.due != 0 && conn.pongs >= conn.due {
		closeOnce(conn.held.flushed)
		conn.due, conn.held = 0, nil
	}
}

// closeOnce closes ch unless it is closed already; the caller holds h.mu.
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (h *ReplyHold) serve() {
	for {
		client, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.relay(client)
	}
}

// relay joins client to the server, a protocol line at a time each way: what the client sends is
// relayed as it is, noting the replies of the requests to hold, the publishes whose flush to hold
// and counting its PINGs, and what the server sends is relayed but for each held reply and its
// payload and each held flush's PONG. Both ends are out of the hold's map once the relay ends.
func (h *ReplyHold) relay(client net.Conn) {
	h.mu.Lock()
	cut := h.cut
	h.mu.Unlock()
	if cut {
		_ = client.Close()
		return
	}
	server, err := net.Dial("tcp", h.target)
	if err != nil {
		_ = client.Close()
		return
	}
	h.mu.Lock()
	if h.cut {
		// Cut while this connection was being dialled: it is refused as one accepted after the Cut.
		h.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	h.conns[client], h.conns[server] = struct{}{}, struct{}{}
	h.mu.Unlock()
	defer func() {
		_ = client.Close()
		_ = server.Close()
		h.mu.Lock()
		delete(h.conns, client)
		delete(h.conns, server)
		h.mu.Unlock()
	}()
	conn := &flushes{}
	go h.down(conn, server, client)
	lines := bufio.NewReader(client)
	for {
		line, payload, fields, err := readMessage(lines, "PUB", "HPUB")
		if err != nil {
			return
		}
		isPub := strings.EqualFold(fields[0], "PUB") || strings.EqualFold(fields[0], "HPUB")
		if isPub && len(fields) >= 3 {
			// `PUB <subject> [reply] <bytes>` and `HPUB <subject> [reply] <header bytes> <bytes>`.
			h.published(conn, fields[1])
		}
		switch {
		// `PUB <subject> <reply> <bytes>` and `HPUB <subject> <reply> <header bytes> <bytes>`.
		case (strings.EqualFold(fields[0], "PUB") && len(fields) == 4) || (strings.EqualFold(fields[0], "HPUB") && len(fields) == 5):
			h.request(conn, fields[1], fields[2])
		case len(fields) == 1 && strings.EqualFold(fields[0], "PING"):
			h.pinged(conn)
		}
		if _, err := server.Write(append(line, payload...)); err != nil {
			return
		}
	}
}

// down relays what the server sends to client, leaving out each held reply and each PONG a flush
// hold keeps (keepPong), and counts the PONGs it forwards (HeldReplies.Flushed).
func (h *ReplyHold) down(conn *flushes, server, client net.Conn) {
	defer func() {
		_ = client.Close()
		_ = server.Close()
	}()
	lines := bufio.NewReader(server)
	for {
		line, payload, fields, err := readMessage(lines, "MSG", "HMSG")
		if err != nil {
			return
		}
		if len(fields) >= 4 && (strings.EqualFold(fields[0], "MSG") || strings.EqualFold(fields[0], "HMSG")) && h.withhold(fields[1]) {
			continue
		}
		pong := len(fields) == 1 && strings.EqualFold(fields[0], "PONG")
		if pong && h.keepPong(conn) {
			continue
		}
		if _, err := client.Write(append(line, payload...)); err != nil {
			return
		}
		if pong {
			h.ponged(conn)
		}
	}
}

// readMessage reads one protocol line from lines and, when its verb is one of withPayload, the
// payload its last field counts and the CRLF after it.
func readMessage(lines *bufio.Reader, withPayload ...string) (line, payload []byte, fields []string, err error) {
	for {
		if line, err = lines.ReadBytes('\n'); err != nil {
			return nil, nil, nil, err
		}
		if fields = strings.Fields(string(line)); len(fields) > 0 {
			break
		}
	}
	for _, verb := range withPayload {
		if !strings.EqualFold(fields[0], verb) || len(fields) < 3 {
			continue
		}
		size, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return nil, nil, nil, err
		}
		payload = make([]byte, size+len("\r\n"))
		if _, err := io.ReadFull(lines, payload); err != nil {
			return nil, nil, nil, err
		}
		break
	}
	return line, payload, fields, nil
}

func (h *ReplyHold) close() {
	_ = h.listener.Close()
	h.Drop()
}
