package testnats

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// HeldHandshakeAnswerWait bounds how long a row waits for an answer that must come while a
// HandshakeHold holds a bus client's reconnect handshake. A read of the client's connection that
// waits for nats.go's connection lock gives no answer until the held attempt ends at the bus
// client's connect timeout (5 s); one that waits for that lock at most the bus's status read bound
// answers within about a tenth of a second. The wait lies between the two, with room for a loaded
// runner.
const HeldHandshakeAnswerWait = 2 * time.Second

// HandshakeHold relays a client's TCP connections to a NATS server, and holds the handshake of a
// reconnect: Hold drops every connection it relays, so a client through it reconnects, and until
// Release it holds the server's answer to the CONNECT of each connection it accepts. The server's
// INFO reaches the client and the client's CONNECT reaches the server; nothing more the server
// sends does, as at a hub whose auth service has not answered the CONNECT. nats.go holds its
// connection lock through a reconnect attempt's dial and handshake (v1.53.1 doReconnect,
// nats.go:3458-3485), so while a handshake is held every read of the connection's state waits, until
// the client's connect timeout ends the attempt; the next attempt is held in turn.
type HandshakeHold struct {
	listener net.Listener
	target   string
	url      string
	done     chan struct{}
	closing  sync.Once

	mu sync.Mutex
	// conns are both ends of every connection the hold is relaying.
	conns map[net.Conn]struct{}
	// release is nil while connections are relayed as they are, and open from Hold until Release.
	release chan struct{}
	// held is closed once a CONNECT has reached the server under the hold Hold began.
	held     chan struct{}
	heldOnce *sync.Once
}

// StartHandshakeHold relays to the NATS server at target, a client URL such as Start returns, on a
// loopback port of its own, until the test ends. URL is the same server through the hold.
func StartHandshakeHold(t testing.TB, target string) *HandshakeHold {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the handshake hold relays to: %v", target, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the handshake hold: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	h := &HandshakeHold{
		listener: listener,
		target:   server.Host,
		url:      relayed.String(),
		done:     make(chan struct{}),
		conns:    map[net.Conn]struct{}{},
	}
	go h.serve()
	t.Cleanup(h.close)
	return h
}

// URL is the NATS URL of the hold's address: the target's, credentials included, with the host and
// port the hold listens on.
func (h *HandshakeHold) URL() string { return h.url }

// Hold drops every connection the hold relays and holds the answer to the CONNECT of each one it
// accepts from now until Release. The channel it returns is closed once a client's CONNECT has
// reached the server through the hold, so that client's handshake is waiting for the answer the
// hold keeps.
func (h *HandshakeHold) Hold() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release == nil {
		h.release, h.held, h.heldOnce = make(chan struct{}), make(chan struct{}), new(sync.Once)
	}
	for conn := range h.conns {
		_ = conn.Close()
	}
	clear(h.conns)
	return h.held
}

// Release ends a Hold: every answer it kept moves on, and connections accepted from now on are
// relayed as they are.
func (h *HandshakeHold) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release != nil {
		close(h.release)
		h.release = nil
	}
}

// HoldReconnect holds (Hold) and waits until a client's reconnect handshake is held through it,
// failing the test when none is within 30 s. A bus client reconnects about a second after its
// connection drops (ReconnectWait).
func (h *HandshakeHold) HoldReconnect(t testing.TB) {
	t.Helper()
	select {
	case <-h.Hold():
	case <-time.After(30 * time.Second):
		t.Fatal("no reconnect's CONNECT reached the server through the handshake hold within 30 s")
	}
}

// AnswersWhileHeld runs each call at once, each on a goroutine of its own, and returns their
// answers in order. It fails the test, naming what, when any call has not answered within
// HeldHandshakeAnswerWait of the start. A call that waits out a held handshake answers when the
// attempt ends, after the test has failed.
func AnswersWhileHeld(t testing.TB, what string, calls ...func() error) []error {
	t.Helper()
	answers := make([]chan error, len(calls))
	for i, call := range calls {
		answers[i] = make(chan error, 1)
		go func() { answers[i] <- call() }()
	}
	deadline := time.NewTimer(HeldHandshakeAnswerWait)
	defer deadline.Stop()
	got := make([]error, len(calls))
	for i, answer := range answers {
		select {
		case got[i] = <-answer:
		case <-deadline.C:
			t.Fatalf("%s %d of %d gave no answer within %s while the handshake was held: it waited for the handshake", what, i+1, len(calls), HeldHandshakeAnswerWait)
		}
	}
	return got
}

// serve relays each connection the hold accepts until it closes.
func (h *HandshakeHold) serve() {
	for {
		client, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.join(client)
	}
}

// join dials the server for client and relays the two into each other, under the hold when one
// is in force as client is accepted.
func (h *HandshakeHold) join(client net.Conn) {
	if !h.track(client) {
		return
	}
	server, err := net.DialTimeout("tcp", h.target, dialTimeout)
	if err != nil || !h.track(server) {
		if err == nil {
			_ = server.Close()
		}
		h.untrack(client)
		return
	}
	defer h.untrack(client, server)
	h.mu.Lock()
	release, held, heldOnce := h.release, h.held, h.heldOnce
	h.mu.Unlock()
	if release == nil {
		go func() {
			_, _ = io.Copy(client, server)
			h.untrack(client, server)
		}()
		_, _ = io.Copy(server, client)
		return
	}
	gone := make(chan struct{})
	defer close(gone)
	go h.answerOnRelease(client, server, release, gone)
	buffer := make([]byte, 32<<10)
	for {
		read, err := client.Read(buffer)
		if read > 0 {
			if _, err := server.Write(buffer[:read]); err != nil {
				return
			}
			if bytes.Contains(buffer[:read], []byte("CONNECT ")) {
				heldOnce.Do(func() { close(held) })
			}
		}
		if err != nil {
			return
		}
	}
}

// answerOnRelease passes the server's INFO line to client and the rest of what the server sends
// once release is closed, or ends once the connection is gone or the hold closes.
func (h *HandshakeHold) answerOnRelease(client, server net.Conn, release, gone <-chan struct{}) {
	defer h.untrack(client, server)
	answers := bufio.NewReader(server)
	info, err := answers.ReadBytes('\n')
	if err != nil {
		return
	}
	if _, err := client.Write(info); err != nil {
		return
	}
	select {
	case <-release:
	case <-gone:
		return
	case <-h.done:
		return
	}
	_, _ = io.Copy(client, answers)
}

// track records conn as relayed and reports true, or closes it and reports false once the hold has
// closed.
func (h *HandshakeHold) track(conn net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done:
		_ = conn.Close()
		return false
	default:
	}
	h.conns[conn] = struct{}{}
	return true
}

func (h *HandshakeHold) untrack(conns ...net.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
		delete(h.conns, conn)
	}
}

// close stops listening, closes every relayed connection and ends every hold.
func (h *HandshakeHold) close() {
	h.closing.Do(func() {
		close(h.done)
		_ = h.listener.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		for conn := range h.conns {
			_ = conn.Close()
		}
		clear(h.conns)
	})
}
