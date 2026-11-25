package testnats

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"syscall"
	"testing"
)

// StallRelay relays each TCP connection a client makes to a NATS server, and reads what the client
// sends through a socket whose receive buffer is the smallest the kernel allows (about a kilobyte of
// window on loopback). Stall stops it reading what the clients of the connections it relays now send:
// once the relay's window is full, everything those clients write stays in their own kernel send
// queues, unacknowledged, where nothing in user space has read it, as on a link that has stalled.
// What the server sends a stalled connection is read and dropped from then on, so a client's kernel
// is never handed bytes for a socket it has closed, which it would answer with a reset of its own.
// Connections the clients make after Stall are relayed as usual. Move reads the stalled connections
// again: a socket its client closed normally then sends everything its kernel still held, and one it
// reset sends nothing more. A stalled connection is for a client that gives it up; Move does not
// make it whole again. Freeze stalls a connection the client keeps: it holds what the server sends
// rather than dropping it, so after Move the connection carries on, both ways, as a link that
// recovered does.
type StallRelay struct {
	listener net.Listener
	target   string
	url      string

	mu       sync.Mutex
	conns    []*stallConn
	accepted int
}

// stallConn is one connection the relay relays, with the state of its stall.
type stallConn struct {
	client, server net.Conn
	// gate is nil while the relay reads the client, and open from Stall or Freeze until Move.
	gate chan struct{}
	// stalled is set by Stall and Freeze and never cleared; dropServer is set by Stall alone: the
	// server's bytes are dropped from then on, where a frozen connection holds them until Move.
	stalled    bool
	dropServer bool
	// stopped is closed once the reader has stopped at the gate Stall set, so it reads nothing more.
	stopped   chan struct{}
	stopOnce  *sync.Once
	readSince bytes.Buffer
	// ended is closed once the client's side has ended and the server has closed its side after it,
	// so the server has read and handled every byte the relay forwarded.
	ended chan struct{}
}

// StartStallRelay relays to the NATS server at target, a client URL such as URL returns, on a
// loopback port of its own, until the test ends. URL is the same server through the relay.
func StartStallRelay(t testing.TB, target string) *StallRelay {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the stall relay relays to: %v", target, err)
	}
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var set error
		if err := raw.Control(func(fd uintptr) {
			// The kernel raises 1 to its minimum, and the sockets the listener accepts take it.
			set = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1)
		}); err != nil {
			return err
		}
		return set
	}}
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the stall relay: %v", err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	r := &StallRelay{listener: listener, target: server.Host, url: relayed.String()}
	t.Cleanup(r.close)
	go r.serve()
	return r
}

// URL is the NATS URL of the relay: the target's, credentials included, with the relay's host and
// port.
func (r *StallRelay) URL() string { return r.url }

// Accepted is how many connections the relay has accepted from its clients.
func (r *StallRelay) Accepted() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted
}

// Stall stops the relay reading what the clients of every connection it relays now send, and drops
// what the server sends them from now on.
func (r *StallRelay) Stall() { r.stall(true) }

// Freeze stops the relay reading what the clients of every connection it relays now send, and holds
// what the server sends them until Move, which then relays both again.
func (r *StallRelay) Freeze() { r.stall(false) }

func (r *StallRelay) stall(dropServer bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		if c.gate == nil {
			c.gate, c.stopped, c.stopOnce = make(chan struct{}), make(chan struct{}), new(sync.Once)
		}
		c.stalled = true
		c.dropServer = dropServer
	}
}

// AwaitStopped waits until the reader of every stalled connection has stopped at its stall, failing
// the test when one has not by the test's deadline (testDeadline). A reader waiting inside a read as
// the stall began reads what its client sends next, at most stallReadBuffer bytes, and stops after
// it; a client that sends nothing keeps it there.
func (r *StallRelay) AwaitStopped(t testing.TB) {
	t.Helper()
	bound := boundOf(t)
	for _, c := range r.stalledConns() {
		r.mu.Lock()
		stopped := c.stopped
		r.mu.Unlock()
		if !awaitSignal(bound, stopped) {
			t.Fatalf("a stalled connection's reader did not stop at its stall by %s, short of the test's deadline: its client sent nothing", bound)
		}
	}
}

// ReadByRelay is what the relay read from stalled connections' clients after their stall began, and
// what their sockets' kernel receive queues hold now, which the relay has acknowledged and not read
// (peeked, so Move still forwards it). A client's byte in neither is in its own kernel's send queue,
// or still in its process.
func (r *StallRelay) ReadByRelay(t testing.TB) []byte {
	t.Helper()
	var read bytes.Buffer
	for _, c := range r.stalledConns() {
		r.mu.Lock()
		read.Write(c.readSince.Bytes())
		r.mu.Unlock()
		raw, err := c.client.(*net.TCPConn).SyscallConn()
		if err != nil {
			t.Fatalf("reach a stalled connection's socket: %v", err)
		}
		peek := make([]byte, 1<<20)
		var n int
		var peekErr error
		if err := raw.Control(func(fd uintptr) {
			n, _, peekErr = syscall.Recvfrom(int(fd), peek, syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		}); err != nil {
			t.Fatalf("peek a stalled connection's socket: %v", err)
		}
		// A socket its client reset answers ECONNRESET once its queue is empty.
		if peekErr != nil && !errors.Is(peekErr, syscall.EAGAIN) && !errors.Is(peekErr, syscall.ECONNRESET) {
			t.Fatalf("peek a stalled connection's socket: %v", peekErr)
		}
		if n > 0 {
			read.Write(peek[:n])
		}
	}
	return read.Bytes()
}

// Move reads the stalled connections' clients again.
func (r *StallRelay) Move() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		if c.gate != nil {
			close(c.gate)
			c.gate = nil
		}
	}
}

// AwaitEnded waits until every stalled connection has ended at both ends: its client's side has
// ended -- a normal close once the relay has read everything it sent, or a reset -- and the server has
// closed its side after reading everything the relay forwarded, which it handles before it closes. It
// fails the test when one has not by the test's deadline (testDeadline).
func (r *StallRelay) AwaitEnded(t testing.TB) {
	t.Helper()
	conns := r.stalledConns()
	if len(conns) == 0 {
		t.Fatal("control: the stall relay stalled no connection")
	}
	bound := boundOf(t)
	for _, c := range conns {
		if !awaitSignal(bound, c.ended) {
			t.Fatalf("a stalled connection had not ended at both ends by %s, short of the test's deadline, after the relay moved", bound)
		}
	}
}

func (r *StallRelay) stalledConns() []*stallConn {
	r.mu.Lock()
	defer r.mu.Unlock()
	var stalled []*stallConn
	for _, c := range r.conns {
		if c.stalled {
			stalled = append(stalled, c)
		}
	}
	return stalled
}

func (r *StallRelay) serve() {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.DialTimeout("tcp", r.target, dialTimeout)
		if err != nil {
			_ = client.Close()
			continue
		}
		c := &stallConn{client: client, server: server, ended: make(chan struct{})}
		r.mu.Lock()
		r.conns = append(r.conns, c)
		r.accepted++
		r.mu.Unlock()
		go r.toServer(c)
		go r.toClient(c)
	}
}

// stallReadBuffer is the most one read of a client takes, so a reader waiting inside a read as a
// stall begins takes little of what the client sends after it.
const stallReadBuffer = 512

// toServer forwards what the client sends, stopping at the gate before each read while stalled, until
// the client's side ends; then it ends the relay's writes to the server, which closes its side once
// it has handled them.
func (r *StallRelay) toServer(c *stallConn) {
	buffer := make([]byte, stallReadBuffer)
	for {
		r.mu.Lock()
		gate, stopOnce, stopped := c.gate, c.stopOnce, c.stopped
		r.mu.Unlock()
		if gate != nil {
			stopOnce.Do(func() { close(stopped) })
			<-gate
		}
		n, err := c.client.Read(buffer)
		if n > 0 {
			r.mu.Lock()
			if c.stalled {
				c.readSince.Write(buffer[:n])
			}
			r.mu.Unlock()
			if _, err := c.server.Write(buffer[:n]); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = c.server.(*net.TCPConn).CloseWrite()
}

// toClient forwards what the server sends until the server closes its side. After Stall it reads and
// drops it; while frozen (Freeze) it reads nothing until Move, so the server's bytes wait in its
// socket and reach the client once the link moves.
func (r *StallRelay) toClient(c *stallConn) {
	defer close(c.ended)
	defer c.client.Close()
	defer c.server.Close()
	buffer := make([]byte, 32<<10)
	alive := true
	for {
		r.mu.Lock()
		gate, dropServer := c.gate, c.dropServer
		r.mu.Unlock()
		if gate != nil && !dropServer {
			<-gate
		}
		n, err := c.server.Read(buffer)
		r.mu.Lock()
		dropServer = c.dropServer
		r.mu.Unlock()
		if n > 0 && alive && !dropServer {
			if _, err := c.client.Write(buffer[:n]); err != nil {
				alive = false
			}
		}
		if err != nil {
			return
		}
	}
}

// close stops listening, ends every stall and closes every connection.
func (r *StallRelay) close() {
	_ = r.listener.Close()
	r.Move()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		_ = c.client.Close()
		_ = c.server.Close()
	}
}
