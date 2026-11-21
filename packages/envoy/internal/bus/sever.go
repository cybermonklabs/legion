package bus

import (
	"context"
	"errors"
	"net"
	"sync"
)

// errSevered is the answer to every dial of a connection Sever has ended.
var errSevered = errors.New("bus: the connection was severed")

// severableDialer is the nats.CustomDialer of every connection the bus dials (connectWithContext).
// It dials as that connection would otherwise, directly or through the SOCKS5 proxy, and keeps the
// socket of each net.Conn it hands nats.go until nats.go closes it, so that sever can end the
// connection without nats.go's connection lock (Client.Sever). It keeps every socket it handed out,
// not only the last: nats.go dials from its reconnect goroutine, and natscred's probe dials through
// this same dialer, since it copies the connection's CustomDialer.
type severableDialer struct {
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// severing is the context of every dial, and sever cancels it, so a dial in progress -- one a
	// reconnect attempt makes holding nats.go's lock -- ends with it.
	severing context.Context
	cancel   context.CancelFunc

	mu      sync.Mutex
	open    map[*severableConn]struct{}
	severed bool
}

func newSeverableDialer(dial func(ctx context.Context, network, address string) (net.Conn, error)) *severableDialer {
	severing, cancel := context.WithCancel(context.Background())
	return &severableDialer{dial: dial, severing: severing, cancel: cancel, open: map[*severableConn]struct{}{}}
}

// socketSlot is the context key under which a dial records the TCP socket it made, for a dialer
// whose net.Conn wraps that socket: the SOCKS5 proxy's (socks5Proxy).
type socketSlot struct{}

// Dial dials address and keeps the socket of the net.Conn it returns, unless the connection has
// been severed, which it answers errSevered.
func (d *severableDialer) Dial(network, address string) (net.Conn, error) {
	var socket net.Conn
	conn, err := d.dial(context.WithValue(d.severing, socketSlot{}, &socket), network, address)
	if err != nil {
		return nil, err
	}
	if socket == nil {
		socket = conn
	}
	kept := &severableConn{Conn: conn, socket: socket, dialer: d}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.severed {
		reset(socket)
		return nil, errSevered
	}
	d.open[kept] = struct{}{}
	return kept, nil
}

// sever resets every socket the dialer handed out that is still open (reset), ends a dial in
// progress, and refuses every later dial.
func (d *severableDialer) sever() {
	d.mu.Lock()
	d.severed = true
	open := d.open
	d.open = nil
	d.mu.Unlock()
	d.cancel()
	for conn := range open {
		reset(conn.socket)
	}
}

// severableConn is a net.Conn severableDialer handed nats.go, with the TCP socket under it, which
// the dialer forgets once nats.go closes it.
type severableConn struct {
	net.Conn
	socket net.Conn
	dialer *severableDialer
}

func (c *severableConn) Close() error {
	c.dialer.mu.Lock()
	delete(c.dialer.open, c)
	c.dialer.mu.Unlock()
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

// Sever ends the client's connection at once, for a caller that must know that nothing it has
// handed nats.go on that connection reaches the server any more, and cannot wait for nats.go's
// connection lock, which a write to a stalled socket holds until its write deadline (a minute,
// nats.go's FlusherTimeout). It resets every socket of the connection (severableDialer), so a write
// holding that lock fails at once and what the kernel holds unsent is discarded; it refuses the
// connection every later dial, so no reconnect of it sends anything; and then it closes it, which
// the reset socket lets nats.go do at once. A publish that was waiting for the lock then either
// fails or lands in a buffer nats.go never writes again: the reset socket's writer fails, and the
// close drops the rest. The client replaces the closed connection as after any close (onClosed),
// re-subscribing what it holds. A socket through the SOCKS5 proxy is reset too; what the proxy had
// already read from it is beyond the client's reach.
func (c *Client) Sever() {
	c.mu.Lock()
	conn := c.Conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	// CustomDialer is set before the connection is made and never written after (natscred's probe
	// reads it the same way), so this read takes no lock of nats.go's.
	if dialer, ok := conn.Opts.CustomDialer.(*severableDialer); ok {
		dialer.sever()
	}
	conn.Close()
}
