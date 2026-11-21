package testnats

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

// HandshakeHold is a Proxy whose relay to the server can hold a reconnect's handshake: Hold drops
// every connection the proxy relays, so a client through it reconnects, and until Release it holds
// the server's answer to the CONNECT of each connection made meanwhile. The server's INFO reaches
// the client and the client's CONNECT reaches the server; nothing more the server sends does, as at
// a hub whose auth service has not answered the CONNECT. nats.go holds its connection lock through a
// reconnect attempt's dial and handshake: doReconnect takes it before the dial and lets it go only
// once the attempt has failed (v1.55.0 nats.go:3554-3588, then 3491), and processConnectInit writes
// the CONNECT under it and waits for the answer until the client's connect timeout (nats.go:2944,
// 2966). So while a handshake is held every read of the connection's state waits, until that timeout
// ends the attempt; the next attempt is held in turn. nats.go does not close a failed attempt's
// connection, so the hold cannot see an attempt end; it counts the CONNECTs (Attempts), and the rows
// read the end of an attempt from the lock's own state (lockTaken, lockLetGo).
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

// HoldReconnect holds (Hold) and waits until conn's reconnect handshake is held (AwaitAttempt): a
// CONNECT made after the call has reached the server through the hold, and conn's connection lock
// is taken. A bus client reconnects about a second after its connection drops (ReconnectWait).
func (h *HandshakeHold) HoldReconnect(t *testing.T, conn *natsgo.Conn) {
	t.Helper()
	before := h.Attempts()
	h.Hold()
	h.AwaitAttempt(t, conn, before)
}

// AwaitAttempt waits until a handshake attempt after the first after has reached the hold and conn's
// connection lock is taken (lockTaken), both read at once. nats.go writes an attempt's CONNECT under
// that lock and keeps it until the attempt fails at the client's connect timeout, so from the
// CONNECT on the lock is that attempt's. Two facts decide, the hold's count of CONNECTs and the
// lock's own state, and no reading of how long a call takes. Its one bound is the test binary's
// deadline less testwait's margin (testwait.Until), a guard against an attempt that never comes.
func (h *HandshakeHold) AwaitAttempt(t *testing.T, conn *natsgo.Conn, after int64) {
	t.Helper()
	lock, found := connLock(t, conn)
	if !found {
		t.FailNow()
	}
	what := fmt.Sprintf("a handshake attempt after the first %d to reach the hold, with nats.go's connection lock taken", after)
	testwait.Until(t, what, func() (string, bool) {
		attempts := h.Attempts()
		taken := lockTaken(lock)
		return fmt.Sprintf("%d attempt(s) at the hold, lock taken %t", attempts, taken), attempts > after && taken
	})
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

// HoldWriteLock holds conn's nats.go connection lock for writing on a connection through the hold
// that stays connected, as a write to a socket whose peer has stopped reading holds it: it freezes
// the proxy (Freeze), so nothing either side sends reaches the other, and takes the lock itself
// (takeConnLock). It returns once it holds the lock, which no reading of how long nats.go takes to
// answer decides: from then every nats.go call on conn that needs the lock - a publish, a flush, a
// close, a read of the status - waits until the func it returns thaws the proxy (Restore) and lets
// the lock go. The test's end calls that func too. When it cannot take the lock it fails the test by
// name without ending its goroutine, which may be a hook's rather than the test's (takeConnLock),
// and returns a func that only thaws the proxy.
func (h *HandshakeHold) HoldWriteLock(t *testing.T, conn *natsgo.Conn) (release func()) {
	t.Helper()
	h.Freeze()
	t.Cleanup(h.Restore)
	lock, taken := takeConnLock(t, conn)
	if !taken {
		return h.Restore
	}
	release = sync.OnceFunc(func() {
		h.Restore()
		lock.Unlock()
	})
	t.Cleanup(release)
	return release
}

// RequireLockHeld fails the test unless conn's connection lock is taken now (lockTaken): the
// handshake or the HoldWriteLock the row holds it with still holds it. A row calls it once the calls
// it measures have answered, so their answers are known to have come while the lock was held. It
// reads the lock's own state and waits for nothing.
func RequireLockHeld(t testing.TB, conn *natsgo.Conn) {
	t.Helper()
	lock, found := connLock(t, conn)
	if !found {
		t.FailNow()
	}
	if !lockTaken(lock) {
		t.Fatal("nats.go's connection lock was free once the calls had answered: neither a handshake nor a write held it then")
	}
}

// lockTaken reports whether lock, a connection's nats.go lock (connLock), is held by a writer or
// waited for by one now: whether a read of the connection's status would wait. A held handshake and
// HoldWriteLock each hold it for writing. It reads the lock's own state (TryRLock) and waits for
// nothing, so a reader descheduled before or after the read cannot change the answer.
func lockTaken(lock *sync.RWMutex) bool {
	if lock.TryRLock() {
		lock.RUnlock()
		return false
	}
	return true
}

// lockLetGo returns a channel closed once nats.go has let conn's connection lock go from now on: a
// read of conn's status begun now waits while a writer holds the lock, and sync.RWMutex hands a
// waiting read the lock at the writer's Unlock, ahead of the next writer. So the channel closes when
// the attempt holding the lock ends, even when the next attempt takes the lock again at once; a read
// begun with the lock free closes it at once.
func lockLetGo(conn *natsgo.Conn) <-chan struct{} {
	letGo := make(chan struct{})
	go func() {
		conn.Status()
		close(letGo)
	}()
	return letGo
}

// AnswersWithin runs each call at once, each on a goroutine of its own, and returns their answers in
// order. It fails the test, naming what, when any call has not answered within within of the start.
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
// attempt the hold held when they began: by the time all have answered, no later attempt has reached
// the hold, nats.go has not let its lock go since they began (lockLetGo), and the lock is taken then
// (RequireLockHeld). A call that waits for nats.go's lock answers only once that attempt has ended,
// at the client's connect timeout: in the gap after it, with the lock free, or once the next attempt
// has begun; nats.go let the lock go either way. The row decides by those events and the lock's own
// state, and no wall-clock bound but a 30 s guard against a call that never answers.
func (h *HandshakeHold) AnswersWithinAttempt(t testing.TB, conn *natsgo.Conn, what string, calls ...func() error) []error {
	t.Helper()
	began := h.Attempts()
	letGo := lockLetGo(conn)
	answers := AnswersWithin(t, 30*time.Second, what, calls...)
	if ended := h.Attempts(); ended != began {
		t.Fatalf("%s answered only after %d more handshake attempt(s) had reached the hold: it waited out the attempt held when it began", what, ended-began)
	}
	select {
	case <-letGo:
		t.Fatalf("%s answered only after nats.go had let its connection lock go: it waited out the attempt held when it began", what)
	default:
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
