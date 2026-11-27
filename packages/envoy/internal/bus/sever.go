package bus

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
	"weak"

	"github.com/nats-io/nats.go"
)

// errSevered is the answer to every dial of a connection Sever has ended.
var errSevered = errors.New("bus: the connection was severed")

// severableDialer is the nats.CustomDialer of every connection the bus dials (connectWithContext).
// It dials as that connection would otherwise, directly or through the SOCKS5 proxy, and keeps a
// weak reference to the socket of each net.Conn it hands nats.go, so that sever can end the
// connection without nats.go's connection lock (Client.Sever). It keeps every socket it handed out
// that something still holds, not only the last: nats.go dials from its reconnect goroutine, and
// natscred's probe dials through this same dialer, since it copies the connection's CustomDialer.
// The references are weak because nats.go does not close every socket it drops: a reconnect whose
// handshake fails leaves its socket to the garbage collector, whose finalizer closes it, and a
// strong reference here would keep that socket, and its descriptor, for the life of the process.
type severableDialer struct {
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// severing is the context of every dial, and sever cancels it, so a dial in progress -- one a
	// reconnect attempt makes holding nats.go's lock -- ends with it.
	severing context.Context
	cancel   context.CancelFunc

	mu      sync.Mutex
	open    []weak.Pointer[severableConn]
	severed bool
	// marks counts the writes callers marked (mark), unwritten those of them not yet handed to
	// nats.go, and marking is set from a mark until a flush confirms every marked write
	// (confirmMarks). Every socket the dialer holds or hands out while marking is set is marked
	// (severableConn.marked), so it is reset rather than closed.
	marks     uint64
	unwritten int
	marking   bool
}

func newSeverableDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) *severableDialer {
	severing, cancel := context.WithCancel(context.Background())
	return &severableDialer{dial: dial, severing: severing, cancel: cancel}
}

// socketSlot is the context key under which a dial records the TCP socket it made, for a dialer
// whose net.Conn wraps that socket: the SOCKS5 proxy's (socks5Proxy).
type socketSlot struct{}

// Dial dials address and keeps the socket of the net.Conn it returns, unless the connection has
// been severed, which it answers errSevered. It drops the references whose sockets are gone.
func (d *severableDialer) Dial(network, address string) (net.Conn, error) {
	var socket net.Conn
	conn, err := d.dial(context.WithValue(d.severing, socketSlot{}, &socket), network, address)
	if err != nil {
		return nil, err
	}
	if socket == nil {
		socket = conn
	}
	kept := &severableConn{Conn: conn, socket: socket}
	d.mu.Lock()
	defer d.mu.Unlock()
	kept.marked = d.marking
	if d.severed {
		reset(socket)
		return nil, errSevered
	}
	live := d.open[:0]
	for _, ref := range d.open {
		if ref.Value() != nil {
			live = append(live, ref)
		}
	}
	d.open = append(live, weak.Make(kept))
	return kept, nil
}

// sever resets every socket the dialer handed out that is still held (reset), ends a dial in
// progress, and refuses every later dial.
func (d *severableDialer) sever() {
	d.mu.Lock()
	d.severed = true
	open := d.open
	d.open = nil
	d.mu.Unlock()
	d.cancel()
	for _, ref := range open {
		if conn := ref.Value(); conn != nil {
			reset(conn.socket)
		}
	}
}

// mark marks every socket the dialer holds, and every one it hands out until a flush confirms the
// mark (confirmMarks), as carrying a write the server has not confirmed. written records that the
// marked write has been handed to nats.go, or has failed.
func (d *severableDialer) mark() (written func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.marks++
	d.unwritten++
	d.marking = true
	for _, ref := range d.open {
		if conn := ref.Value(); conn != nil {
			conn.setMarked(true)
		}
	}
	return sync.OnceFunc(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.unwritten--
	})
}

// markState is what a flush that confirms the marks (confirmMarks) read before it began.
type markState struct {
	marks     uint64
	unwritten int
}

func (d *severableDialer) markState() markState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return markState{marks: d.marks, unwritten: d.unwritten}
}

// confirmMarks clears the marks once a flush that began at before has answered, when every marked
// write had been handed to nats.go by then and no write has been marked since: each was then
// written ahead of the flush's PING, on the socket the flush answered on or on one closed before
// it, which its mark reset. A write marked since, or one still to be written, keeps them. It reports
// whether no mark is left.
func (d *severableDialer) confirmMarks(before markState) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if before.unwritten != 0 || d.marks != before.marks {
		return false
	}
	d.marking = false
	for _, ref := range d.open {
		if conn := ref.Value(); conn != nil {
			conn.setMarked(false)
		}
	}
	return true
}

// marked reports whether a mark is left (mark, confirmMarks).
func (d *severableDialer) marked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.marking
}

// severableConn is a net.Conn severableDialer handed nats.go, with the TCP socket under it. Its close
// is a reset (reset) when its write failed or while it is marked as carrying a write the server has
// not confirmed (severableDialer.mark): what the kernel still holds of it would otherwise go on being
// sent once the link moves, after nats.go had given the connection up. nats.go gives up a connection
// whose write failed, and one it declares stale, closing its socket with no failed write
// (processOpErr), as it closes its socket at Close after its final flush. A close of a socket with
// no failed write and no mark is a normal close, so every client that marks nothing closes as it
// always did.
type severableConn struct {
	net.Conn
	socket net.Conn

	mu          sync.Mutex
	writeFailed bool
	marked      bool
}

func (c *severableConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if err != nil {
		c.mu.Lock()
		c.writeFailed = true
		c.mu.Unlock()
	}
	return n, err
}

func (c *severableConn) setMarked(marked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.marked = marked
}

func (c *severableConn) Close() error {
	c.mu.Lock()
	resetting := c.writeFailed || c.marked
	c.mu.Unlock()
	if resetting {
		reset(c.socket)
	}
	return c.Conn.Close()
}

// reset closes socket with SO_LINGER at 0, so the close is a TCP reset: whatever the kernel still
// holds unsent is discarded, where an ordinary close would go on sending it, after the write that
// was blocked on it, for as long as TCP retransmits. A socket that has no linger setting is closed.
func reset(socket net.Conn) {
	if lingering, ok := socket.(interface{ SetLinger(sec int) error }); ok {
		_ = lingering.SetLinger(0)
	}
	_ = socket.Close()
}

// severableConnection is a client's connection with its dialer (Client.severable).
type severableConnection struct {
	conn   *nats.Conn
	dialer *severableDialer
}

// noteSeverable records nc, which the caller is making c.Conn under c.mu, as the connection Sever
// ends. CustomDialer is set before the connection is made and never written after (natscred's
// probe reads it the same way), so the read takes no lock of nats.go's.
func (c *Client) noteSeverable(nc *nats.Conn) {
	dialer, _ := nc.Opts.CustomDialer.(*severableDialer)
	c.severable.Store(&severableConnection{conn: nc, dialer: dialer})
}

// Sever ends the client's connection at once, for a caller that must know that nothing it has
// handed nats.go on that connection reaches the server any more, and cannot wait for nats.go's
// connection lock, which a write to a stalled socket holds until its write deadline (a minute,
// nats.go's FlusherTimeout), nor for the client's own lock, which a caller holds while it waits for
// nats.go's (Connected, Close). It reads the connection without either (severable). It resets every
// socket of the connection (severableDialer), so a write holding nats.go's lock fails at once and
// what the kernel holds unsent is discarded; it refuses the connection every later dial, so no
// reconnect of it sends anything; and then it closes it, which the reset socket lets nats.go do at
// once. A publish that was waiting for the lock then either fails or lands in a buffer nats.go never
// writes again: the reset socket's writer fails, and the close drops the rest. The client replaces
// the closed connection as after any close (onClosed), re-subscribing what it holds. A socket
// through the SOCKS5 proxy is reset too; what the proxy had already read from it is beyond the
// client's reach.
func (c *Client) Sever() {
	current := c.severable.Load()
	if current == nil {
		return
	}
	if current.dialer != nil {
		current.dialer.sever()
	}
	current.conn.Close()
}

// MarkUnconfirmed marks the client's connection as about to carry a write whose arrival the caller
// must know of before it lets another process act, for a caller that calls it before the write and
// written once nats.go has taken the write or refused it. Until a flush confirms the write
// (ConfirmMarked), each socket of the connection -- the one it has, and each one nats.go dials to
// reconnect -- is reset rather than closed (severableConn): at Close, and when nats.go declares the
// connection stale and reconnects. So the write is either confirmed, or discarded with the socket that
// still held it in its kernel send queue, and never sent after the caller went on. The mark on a
// socket dialed to reconnect is a safeguard: a bus connection refuses a publish while it reconnects
// (ReconnectBufSize -1), and nats.go drops what it had buffered for the old socket as it binds the new
// one, so a write marked before the reconnect never reaches the new socket. A client with no
// connection marks nothing.
func (c *Client) MarkUnconfirmed() (written func()) {
	current := c.severable.Load()
	if current == nil || current.dialer == nil {
		return func() {}
	}
	return current.dialer.mark()
}

// ConfirmMarked reports whether no write marked on the client's connection (MarkUnconfirmed) can
// reach the server later: the connection is closed, which reset every marked socket, or a flush of it
// answered within timeout and cleared every mark. A flush answers on the socket its PING went out on
// and on no other, since nats.go fails every flush pending as it gives a socket up (processOpErr),
// and each marked write went out ahead of that PING on the same socket, or on an older one its mark
// reset as it closed. So a flush on a newer socket confirms nothing an older one can still send. A
// flush clears the marks, and ConfirmMarked reports true, only when every marked write had been
// taken by nats.go as it began and none was marked since (severableDialer.confirmMarks): a write
// still to be taken, or one taken behind the PING, is not confirmed. It reads the connection without
// the client's lock (severable).
func (c *Client) ConfirmMarked(timeout time.Duration) bool {
	current := c.severable.Load()
	if current == nil {
		return true
	}
	var before markState
	if current.dialer != nil {
		before = current.dialer.markState()
	}
	if current.conn.IsClosed() {
		return true
	}
	if current.conn.FlushTimeout(timeout) != nil {
		return false
	}
	return current.dialer == nil || current.dialer.confirmMarks(before)
}

// CarriesUnconfirmed reports whether the client's connection carries a write marked
// (MarkUnconfirmed) that no flush has confirmed (ConfirmMarked). It reads the connection without the
// client's lock (severable).
func (c *Client) CarriesUnconfirmed() bool {
	current := c.severable.Load()
	return current != nil && current.dialer != nil && current.dialer.marked()
}
