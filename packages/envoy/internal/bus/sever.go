package bus

import (
	"context"
	"errors"
	"net"
	"sync"
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

// severableConn is a net.Conn severableDialer handed nats.go, with the TCP socket under it. A
// socket whose write failed is reset when it is closed (reset): nats.go gives up a connection whose
// write failed, and what the kernel still holds of it is part of an operation nats.go will not
// finish, which a normal close would go on sending once the link moves. A close after writes that
// succeeded, nats.go's own Close after its final flush among them, is a normal close.
type severableConn struct {
	net.Conn
	socket net.Conn

	mu          sync.Mutex
	writeFailed bool
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

func (c *severableConn) Close() error {
	c.mu.Lock()
	failed := c.writeFailed
	c.mu.Unlock()
	if failed {
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
