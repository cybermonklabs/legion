package storetest

import (
	"bytes"
	"context"
	"net"
	"os"
	"sync/atomic"
)

// StalledSocket makes one read of a pool's socket to Postgres time out, which is how a bound of
// the server's own that passed leaves that socket: pgx stops a read in flight by putting a
// deadline on the net.Conn (pgconn's DeadlineContextWatcherHandler), and a read that meets that
// deadline answers `read tcp …: i/o timeout`. pgx reports it as a pgconn timeout and not as a
// context error whenever the context of the call that read is still live - the call was the
// acquire's ping, or a later call of the same pooled connection whose deadline pgx had not
// lifted yet (pgconn's exitPotentialWriteReadDeadlock) - and that is the shape three requests of
// INFRA-592's ten-times load step were answered 500 INTERNAL for.
//
// The stall comes after the statement reached the database, so pgconn does not mark it safe to
// retry: it is the shape whose outcome is unknown where the statement ran outside a transaction
// (store.ErrStatementOutcomeUnknown).
//
// Give Dial to a pool's ConnConfig.DialFunc (Reopened, the api package's
// testServerOptions.dialRequestPool). Every pool a store opens of its own is configured from
// that one, so pool names which of them may stall - store.RequestsPool, store.SettlementsPool -
// read off the startup message each connection writes first.
type StalledSocket struct {
	pool   string
	marker atomic.Pointer[string]
	armed  atomic.Bool
	fired  atomic.Bool
}

// NewStalledSocket is a socket for pool's connections that stalls nothing until StallAfter arms
// it.
func NewStalledSocket(pool string) *StalledSocket { return &StalledSocket{pool: pool} }

// StallAfter arms the socket: the first read that follows a statement carrying marker fails,
// once. marker is something only the statement the test means to stall carries, such as the id
// of the row it works on.
func (s *StalledSocket) StallAfter(marker string) { s.marker.Store(&marker) }

// Fired reports whether the stall has happened, which a test asserts before it reads what the
// server made of it.
func (s *StalledSocket) Fired() bool { return s.fired.Load() }

// Dial is the pool's ConnConfig.DialFunc.
func (s *StalledSocket) Dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := new(net.Dialer).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &stalledConn{Conn: conn, socket: s}, nil
}

type stalledConn struct {
	net.Conn
	socket *StalledSocket
	// greeted and stalls read the startup message, the first thing pgx writes on a connection:
	// they are touched by writes alone, which one goroutine makes at a time.
	greeted bool
	stalls  bool
}

func (c *stalledConn) Write(p []byte) (int, error) {
	if !c.greeted {
		c.greeted = true
		c.stalls = bytes.Contains(p, []byte("dispatch-"+c.socket.pool))
	}
	if marker := c.socket.marker.Load(); c.stalls && marker != nil && bytes.Contains(p, []byte(*marker)) {
		c.socket.armed.Store(true)
	}
	return c.Conn.Write(p)
}

func (c *stalledConn) Read(p []byte) (int, error) {
	if c.socket.armed.Load() && c.socket.fired.CompareAndSwap(false, true) {
		return 0, &net.OpError{
			Op: "read", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded,
		}
	}
	return c.Conn.Read(p)
}
