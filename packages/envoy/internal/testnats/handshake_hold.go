package testnats

import (
	"bufio"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// lockHeldWait is how long a read of a connection's status may take before RequireLockHeld and
// AwaitLockHeld take nats.go's connection lock to be held: with the lock free the read takes
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
// is the same server through both. The proxy gives each client connection a receive buffer of
// holdSocketBuffer (ProxyReceiveBuffer), so a frozen proxy takes little of a client's stream
// (HoldWriteLock).
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
	h.Proxy = StartProxy(t, relayed.String(), ProxyReceiveBuffer(holdSocketBuffer))
	return h
}

// holdSocketBuffer is the receive buffer of each client connection the hold's proxy accepts, and
// the send buffer HoldWriteLock gives the client's own socket.
const holdSocketBuffer = 16 << 10

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
// connected: it makes one write that the frozen proxy's network stack cannot take whole, so the
// write, and with it the lock, lasts until Restore.
//
// nats.go v1.55.0 holds its connection lock from the Lock in (*Conn).publish to its Unlock. A
// publish whose message passes its buffer's limit writes it to the socket inside that, in
// (*natsWriter).flush, which returns only once the socket has taken every byte. A frozen proxy
// (Freeze) reads the client's socket at most once more, the read in flight as it froze, of up to
// proxyReadBuffer: Proxy.relay calls pass before each read. So the network stack can take no more
// of a write than the client socket's send buffer, the proxy's receive buffer for the connection
// and that one read. A write seen parked is not proof by itself: on gVisor on a loaded host the
// stack handles the segments it was sent late, so a write parks before those buffers are full and
// goes on once the stack has handled them, after the proxy has stopped reading. A loop of 64 KiB
// publishes, with the stack's own buffer sizes, seen parked in its write at its 19th publish had
// made 56 a second later: its publishes ended, and the lock was let go between two of them.
//
// So both buffers are small and the one write is far bigger than both. The hold's proxy gives each
// client connection a receive buffer of holdSocketBuffer from its accept (ProxyReceiveBuffer).
// Before it freezes the proxy, HoldWriteLock gives conn's own socket a send buffer of
// holdSocketBuffer (clientSocket finds that socket: testnats does not hold nats.go's net.Conn). It
// reads both sizes back, and fails the test, naming the three sizes, unless the payload, conn's
// MaxPayload less 4 KiB, is more than twice what the stack can take. It then freezes the proxy and
// makes the one publish, on a goroutine of its own, and returns once the runtime's record of that
// goroutine (runtime.Stack) shows it inside (*natsWriter).flush under (*Conn).publish, in any state:
// from then on the lock is held until Restore, and no read of the connection's status decides it.
// It reads that record about every 10 ms, and fails the test, with the goroutine's stack, when the
// publish has not reached its flush within 30 s. nats.go's write deadline (FlusherTimeout, a
// minute) is longer than the 30 s guard of any row. It returns the func that ends the hold, which
// thaws the proxy (Restore); the test's end calls it too.
func (h *HandshakeHold) HoldWriteLock(t testing.TB, conn *natsgo.Conn) (release func()) {
	t.Helper()
	local := conn.LocalAddr()
	if local == "" {
		t.Fatal("control: the connection to hold a write on is not connected")
	}
	payload := int(conn.MaxPayload()) - 4<<10
	client := h.clientSocket(t, local)
	if err := syscall.SetsockoptInt(client, syscall.SOL_SOCKET, syscall.SO_SNDBUF, holdSocketBuffer); err != nil {
		t.Fatalf("set the send buffer of the socket at %s: %v", local, err)
	}
	send, err := syscall.GetsockoptInt(client, syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	if err != nil {
		t.Fatalf("read the send buffer of the socket at %s: %v", local, err)
	}
	receive := h.receiveBuffer(t, local)
	if payload <= 2*(send+receive+proxyReadBuffer) {
		t.Fatalf("the hold cannot be proven on this host: a payload of %d bytes is not more than twice what the stack can take of it, a send buffer of %d bytes, a receive buffer of %d bytes and a read in flight of %d bytes",
			payload, send, receive, proxyReadBuffer)
	}
	h.Freeze()
	release = sync.OnceFunc(h.Restore)
	t.Cleanup(release)
	publisher := make(chan string, 1)
	published := make(chan error, 1)
	go func() {
		publisher <- goroutineID()
		published <- conn.Publish("testnats.write-lock", make([]byte, payload))
	}()
	id := <-publisher
	dump := make([]byte, 1<<20)
	deadline := time.Now().Add(30 * time.Second)
	for {
		publishing := goroutineStack(goroutineDump(&dump), id)
		if strings.Contains(publishing, natsWriterFlushFrame) && strings.Contains(publishing, connPublishFrame) {
			return release
		}
		select {
		case err := <-published:
			if err != nil {
				t.Fatalf("the publish of %d bytes failed while the proxy was frozen, before it was seen in its write: %v", payload, err)
			}
			t.Fatalf("the publish of %d bytes returned while the proxy was frozen, before it was seen in its write: the stack took more of it than its send buffer of %d bytes, its receive buffer of %d bytes and the read in flight allow",
				payload, send, receive)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the publish of %d bytes did not reach its write to the socket within 30 s of the proxy's freeze, so no write holds nats.go's lock. The publishing goroutine:\n%s", payload, publishing)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The frames HoldWriteLock looks for, as runtime.Stack prints them: a dot in the last element of a
// package's path is escaped (nats%2ego).
const (
	natsWriterFlushFrame = "github.com/nats-io/nats%2ego.(*natsWriter).flush("
	connPublishFrame     = "github.com/nats-io/nats%2ego.(*Conn).publish("
)

// clientSocket is the descriptor of the socket conn writes to, the client side of the connection
// the hold's proxy accepted for it: the one descriptor of the process whose local address is local,
// conn's, and whose peer is the proxy's listener. It reads every open descriptor (/proc/self/fd, or
// /dev/fd where there is no /proc) with getsockname and getpeername, and fails the test, naming the
// address, unless exactly one matches.
func (h *HandshakeHold) clientSocket(t testing.TB, local string) int {
	t.Helper()
	peer := h.Proxy.listener.Addr().String()
	descriptors, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		descriptors, err = os.ReadDir("/dev/fd")
	}
	if err != nil {
		t.Fatalf("list the process's descriptors to find the socket at %s: %v", local, err)
	}
	var found []int
	for _, descriptor := range descriptors {
		fd, err := strconv.Atoi(descriptor.Name())
		if err != nil {
			continue
		}
		if name, err := syscall.Getsockname(fd); err != nil || sockaddrString(name) != local {
			continue
		}
		if remote, err := syscall.Getpeername(fd); err != nil || sockaddrString(remote) != peer {
			continue
		}
		found = append(found, fd)
	}
	if len(found) != 1 {
		t.Fatalf("control: %d descriptors of the process are a socket at %s connected to the proxy at %s, want 1", len(found), local, peer)
	}
	return found[0]
}

// receiveBuffer is the receive buffer (SO_RCVBUF) the stack reports for the connection the hold's
// proxy accepted from local.
func (h *HandshakeHold) receiveBuffer(t testing.TB, local string) int {
	t.Helper()
	var accepted syscall.Conn
	h.Proxy.mu.Lock()
	for relayed := range h.Proxy.conns {
		if relayed.RemoteAddr().String() == local {
			accepted, _ = relayed.(syscall.Conn)
		}
	}
	h.Proxy.mu.Unlock()
	if accepted == nil {
		t.Fatalf("control: the proxy relays no connection from %s", local)
	}
	raw, err := accepted.SyscallConn()
	if err != nil {
		t.Fatalf("reach the socket the proxy accepted from %s: %v", local, err)
	}
	var size int
	var read error
	if err := raw.Control(func(fd uintptr) {
		size, read = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
	}); err != nil {
		t.Fatalf("reach the socket the proxy accepted from %s: %v", local, err)
	}
	if read != nil {
		t.Fatalf("read the receive buffer of the socket the proxy accepted from %s: %v", local, read)
	}
	return size
}

// sockaddrString is address as net.TCPAddr.String writes it, or "" for an address that is not an
// IPv4 or IPv6 one.
func sockaddrString(address syscall.Sockaddr) string {
	switch address := address.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(address.Addr), uint16(address.Port)).String()
	case *syscall.SockaddrInet6:
		return netip.AddrPortFrom(netip.AddrFrom16(address.Addr), uint16(address.Port)).String()
	}
	return ""
}

// goroutineStack is the stack of goroutine id in dump, every goroutine's stack (goroutineDump), or
// "" when the goroutine has ended.
func goroutineStack(dump, id string) string {
	for _, stack := range strings.Split(dump, "\n\n") {
		if strings.HasPrefix(stack, "goroutine "+id+" ") {
			return stack
		}
	}
	return ""
}

// goroutineID is the calling goroutine's id, the number on the first line of its stack, such as
// "goroutine 7 [running]:".
func goroutineID() string {
	buffer := make([]byte, 64)
	header := strings.TrimPrefix(string(buffer[:runtime.Stack(buffer, false)]), "goroutine ")
	id, _, _ := strings.Cut(header, " ")
	return id
}

// goroutineDump is every goroutine's stack (runtime.Stack), read into *buffer, which it grows until
// the dump fits.
func goroutineDump(buffer *[]byte) string {
	for {
		n := runtime.Stack(*buffer, true)
		if n < len(*buffer) {
			return string((*buffer)[:n])
		}
		*buffer = make([]byte, 2*len(*buffer))
	}
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

// lockFree reports whether nats.go answers a read of conn's status within lockHeldWait. Once the
// wait's timer has fired it reads the answer again without waiting, so a process that was not
// scheduled for the whole wait, when the answer and the timer are both ready, reads the lock as
// free when the read has answered.
func lockFree(conn *natsgo.Conn) bool {
	read := make(chan struct{})
	go func() {
		conn.Status()
		close(read)
	}()
	timer := time.NewTimer(lockHeldWait)
	defer timer.Stop()
	select {
	case <-read:
		return true
	case <-timer.C:
	}
	select {
	case <-read:
		return true
	default:
		return false
	}
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
