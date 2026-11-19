package testnats

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// HeldHandshakeAnswerWait bounds how long a row waits for an answer that must come while a
// HandshakeHold holds a bus client's reconnect handshake. A call that waits for nats.go's connection
// lock gives no answer until the held attempt ends at the bus client's connect timeout (5 s); one
// that does not wait for that lock answers at once, or at its own bound of at most a second. The
// wait lies between the two, with room for a loaded runner.
const HeldHandshakeAnswerWait = 2 * time.Second

// lockHeldWait is how long a read of a connection's status may take before RequireLockHeld and
// HoldWriteLock take nats.go's connection lock to be held: with the lock free the read takes
// microseconds.
const lockHeldWait = 200 * time.Millisecond

// HandshakeHold is a Proxy whose relay to the server can hold a reconnect's handshake: Hold drops
// every connection the proxy relays, so a client through it reconnects, and until Release it holds
// the server's answer to the CONNECT of each connection made meanwhile. The server's INFO reaches
// the client and the client's CONNECT reaches the server; nothing more the server sends does, as at
// a hub whose auth service has not answered the CONNECT. nats.go holds its connection lock through a
// reconnect attempt's dial and handshake (v1.53.1 doReconnect, nats.go:3458-3485), so while a
// handshake is held every read of the connection's state waits, until the client's connect timeout
// ends the attempt; the next attempt is held in turn.
//
// The proxy (Cut, Freeze, Restore) carries the client's connections, and the hold's own relay sits
// between it and the server. Restore ends a Cut or a Freeze and leaves a hold in force; Release
// ends the hold.
type HandshakeHold struct {
	*Proxy
	// target is the server's address, which the hold's relay dials.
	target   string
	listener net.Listener

	mu sync.Mutex
	// release is nil while connections are relayed as they are, and open from Hold until Release.
	release chan struct{}
	// held is closed once a CONNECT has reached the server under the hold Hold began.
	held     chan struct{}
	heldOnce *sync.Once
	// attempts counts the CONNECTs that have reached the server under a hold: each is one handshake
	// attempt whose answer the hold keeps.
	attempts atomic.Int64
}

// StartHandshakeHold relays to the NATS server at target, a client URL such as Start returns,
// through a proxy and the hold's own relay, on loopback ports of their own, until the test ends. URL
// is the same server through both.
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
	t.Cleanup(func() { _ = listener.Close() })
	h := &HandshakeHold{target: server.Host, listener: listener}
	go h.serve()
	relayed := *server
	relayed.Host = listener.Addr().String()
	h.Proxy = StartProxy(t, relayed.String())
	return h
}

// Hold drops every connection the proxy relays (Cut, then Restore) and holds the answer to the
// CONNECT of each connection made from now until Release. The channel it returns is closed once a
// client's CONNECT has reached the server through the hold, so that client's handshake is waiting
// for the answer the hold keeps.
func (h *HandshakeHold) Hold() <-chan struct{} {
	h.mu.Lock()
	if h.release == nil {
		h.release, h.held, h.heldOnce = make(chan struct{}), make(chan struct{}), new(sync.Once)
	}
	held := h.held
	h.mu.Unlock()
	h.Cut()
	h.Restore()
	return held
}

// HoldReconnect holds (Hold) and waits until conn's reconnect handshake is held: a client's
// CONNECT has reached the server through the hold, and nats.go keeps a read of conn's status
// waiting, so its connection lock is held for that handshake. It fails the test when either has
// not happened within 30 s. A bus client reconnects about a second after its connection drops
// (ReconnectWait).
func (h *HandshakeHold) HoldReconnect(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	select {
	case <-h.Hold():
	case <-time.After(30 * time.Second):
		t.Fatal("no reconnect's CONNECT reached the server through the handshake hold within 30 s")
	}
	AwaitLockHeld(t, conn)
}

// AwaitLockHeld waits until nats.go keeps a read of conn's status waiting, so its connection lock is
// held, failing the test when it is not within 30 s.
func AwaitLockHeld(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for lockFree(conn) {
		if time.Now().After(deadline) {
			t.Fatal("nats.go still answered a read of the connection's status after 30 s: no handshake or write of it holds its lock")
		}
	}
}

// Release ends a Hold: every answer it kept moves on, and connections made from now on are relayed
// as they are.
func (h *HandshakeHold) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.release != nil {
		close(h.release)
		h.release = nil
	}
}

// HoldWriteLock holds conn's nats.go connection lock with a write, on a connection that stays
// connected: it freezes the proxy (Freeze) and publishes to conn, a connection through the hold,
// until a read of conn's status is kept waiting past lockHeldWait. That happens once the socket's
// buffers are full and nats.go's write to it blocks under the lock. It returns the func that ends
// it, which thaws the proxy (Restore) and stops the publishes; the test's end calls it too.
func (h *HandshakeHold) HoldWriteLock(t testing.TB, conn *natsgo.Conn) (release func()) {
	t.Helper()
	h.Freeze()
	stop := make(chan struct{})
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop)
			h.Restore()
		})
	}
	t.Cleanup(release)
	go func() {
		payload := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := conn.Publish("testnats.write-lock", payload); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	for lockFree(conn) {
		if time.Now().After(deadline) {
			t.Fatal("nats.go still answered a read of the connection's status 30 s after the proxy froze and the publishes began: no write held its lock")
		}
	}
	return release
}

// RequireLockHeld fails the test when nats.go answers a read of conn's status within lockHeldWait:
// its connection lock is not held, so the handshake or the write the row holds it with is not. A row
// calls it once the calls it measures have answered, so their answers are known to have come while
// the lock was held. The read left waiting ends when the lock is let go.
func RequireLockHeld(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	if lockFree(conn) {
		t.Fatalf("nats.go answered a read of the connection's status within %s: its connection lock was not held when the calls answered", lockHeldWait)
	}
}

// lockFree reports whether nats.go answers a read of conn's status within lockHeldWait.
func lockFree(conn *natsgo.Conn) bool {
	read := make(chan struct{})
	go func() {
		conn.Status()
		close(read)
	}()
	select {
	case <-read:
		return true
	case <-time.After(lockHeldWait):
		return false
	}
}

// AnswersWhileHeld is AnswersWithin with HeldHandshakeAnswerWait.
func AnswersWhileHeld(t testing.TB, what string, calls ...func() error) []error {
	t.Helper()
	return AnswersWithin(t, HeldHandshakeAnswerWait, what, calls...)
}

// AnswersWithin runs each call at once, each on a goroutine of its own, and returns their answers in
// order. It fails the test, naming what, when any call has not answered within within of the start.
// A call that waits out a held handshake answers when the attempt ends, after the test has failed.
func AnswersWithin(t testing.TB, within time.Duration, what string, calls ...func() error) []error {
	t.Helper()
	answers := make([]chan error, len(calls))
	for i, call := range calls {
		answers[i] = make(chan error, 1)
		go func() { answers[i] <- call() }()
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	got := make([]error, len(calls))
	for i, answer := range answers {
		select {
		case got[i] = <-answer:
		case <-deadline.C:
			t.Fatalf("%s %d of %d gave no answer within %s while nats.go's lock was held: it waited for the lock", what, i+1, len(calls), within)
		}
	}
	return got
}

// AwaitLockFree waits until nats.go lets conn's connection lock go, failing the test after 30 s.
// With every reconnect's handshake held (HoldReconnect), that is the end of an attempt, when nats.go
// reads the connection as reconnecting and waits between attempts (ReconnectWait, 1 s for a bus
// client) with its lock free.
func AwaitLockFree(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	read := make(chan struct{})
	go func() {
		conn.Status()
		close(read)
	}()
	select {
	case <-read:
	case <-time.After(30 * time.Second):
		t.Fatal("nats.go still held its connection lock 30 s on: no attempt's handshake ended")
	}
}

// Attempts is how many handshake attempts have reached the server under a hold: each CONNECT the
// hold has kept the answer to.
func (h *HandshakeHold) Attempts() int64 { return h.attempts.Load() }

// AnswersWithinAttempt runs each call at once, each on a goroutine of its own, and returns their
// answers in order. It fails the test, naming what, unless every call answered within the handshake
// attempt the hold held when they began: no later attempt has reached the hold by the time all have
// answered, and nats.go's lock is still held then (RequireLockHeld). A call that waits for nats.go's
// lock answers only once that attempt has ended, at the client's connect timeout: in the gap after
// it, with the lock free, or once the next attempt has begun. The row decides by those events, and
// no wall-clock bound but a 30 s guard against a call that never answers.
func (h *HandshakeHold) AnswersWithinAttempt(t testing.TB, conn *natsgo.Conn, what string, calls ...func() error) []error {
	t.Helper()
	began := h.Attempts()
	answers := AnswersWithin(t, 30*time.Second, what, calls...)
	if ended := h.Attempts(); ended != began {
		t.Fatalf("%s answered only after %d more handshake attempt(s) had reached the hold: it waited out the attempt held when it began", what, ended-began)
	}
	RequireLockHeld(t, conn)
	return answers
}

// serve relays each connection the proxy makes to the hold until the hold's listener closes.
func (h *HandshakeHold) serve() {
	for {
		client, err := h.listener.Accept()
		if err != nil {
			return
		}
		go h.join(client)
	}
}

// join relays client, a connection the proxy made for one of its clients, to the server, under the
// hold when one is in force as client arrives, until either side ends; then it closes both. The
// client's stream is read as NATS protocol (protocolStream) to find its CONNECT, which may come in
// pieces.
func (h *HandshakeHold) join(client net.Conn) {
	defer client.Close()
	server, err := net.DialTimeout("tcp", h.target, dialTimeout)
	if err != nil {
		return
	}
	defer server.Close()
	h.mu.Lock()
	release, held, heldOnce := h.release, h.held, h.heldOnce
	h.mu.Unlock()
	gone := make(chan struct{})
	defer close(gone)
	go func() {
		defer client.Close()
		defer server.Close()
		answers := bufio.NewReader(server)
		if release != nil {
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
			}
		}
		_, _ = io.Copy(client, answers)
	}()
	var stream protocolStream
	buffer := make([]byte, 32<<10)
	for {
		read, err := client.Read(buffer)
		if read > 0 {
			if _, err := server.Write(buffer[:read]); err != nil {
				return
			}
			if release != nil && !stream.connected {
				stream.wrote(buffer[:read])
				if stream.connected {
					h.attempts.Add(1)
					heldOnce.Do(func() { close(held) })
				}
			}
		}
		if err != nil {
			return
		}
	}
}
