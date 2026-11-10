package storetest

import (
	"bytes"
	"context"
	"encoding/binary"
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
// read off the startup message each connection writes first. Within that pool the stall is armed
// on the one connection that sent the statement the test chose (StallAfter, StallAfterSent) and
// fires on that connection's next read, so a test stalls exactly the call it means rather than
// whichever connection of the pool reads next.
type StalledSocket struct {
	pool  string
	match atomic.Pointer[func(sent []byte) bool]
	fired atomic.Bool
}

// NewStalledSocket is a socket for pool's connections that stalls nothing until StallAfter arms
// it.
func NewStalledSocket(pool string) *StalledSocket { return &StalledSocket{pool: pool} }

// StallAfter arms the socket: from the statement carrying marker, the connection that sent it
// answers every read with that timeout. marker is something only the statement the test means to
// stall carries, such as the id of the row it works on.
func (s *StalledSocket) StallAfter(marker string) {
	s.StallAfterSent(func(sent []byte) bool { return bytes.Contains(sent, []byte(marker)) })
}

// StallAfterSent arms the socket on the first message a connection of the pool writes that
// matches reports true for: that connection's reads fail from then on. The bytes are the
// Postgres protocol messages pgx wrote, so a test that means one statement and no other matches
// the message itself (SimpleQuery).
func (s *StalledSocket) StallAfterSent(matches func(sent []byte) bool) { s.match.Store(&matches) }

// SimpleQuery matches the simple-query message pgx writes for query and nothing else. pgx sends
// a statement with no arguments that way (pgx.Conn.exec), which is how it opens a transaction:
// `begin` (pgx.TxOptions.beginSQL).
func SimpleQuery(query string) func(sent []byte) bool {
	frame := append([]byte{'Q'}, binary.BigEndian.AppendUint32(nil, uint32(len(query)+5))...)
	frame = append(append(frame, query...), 0)
	return func(sent []byte) bool { return bytes.Equal(sent, frame) }
}

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
	// armed is set by this connection's write of the statement the test chose and read by its
	// reads, which pgconn's background reader can make on another goroutine. It covers the whole
	// call rather than one read: a socket pgx has put a deadline on fails every read until pgx
	// lifts that deadline, and one failed read is not the same thing - pgx reads a simple
	// query's answer through a peek whose error it drops (pgconn's
	// MultiResultReader.NextResult), so a single failure leaves an argument-less statement,
	// `begin` among them, served as if nothing had happened. The next statement written on the
	// connection lifts it, as pgx lifts that deadline once the call it cut off has ended: a
	// pgconn timeout does not close the connection, so the pool keeps it and its next caller
	// must be served.
	armed atomic.Bool
}

func (c *stalledConn) Write(p []byte) (int, error) {
	if !c.greeted {
		c.greeted = true
		c.stalls = bytes.Contains(p, []byte("dispatch-"+c.socket.pool))
	}
	if c.armed.Load() && c.socket.fired.Load() {
		c.armed.Store(false)
	}
	matches := c.socket.match.Load()
	if c.stalls && matches != nil && !c.socket.fired.Load() && (*matches)(p) {
		c.armed.Store(true)
	}
	return c.Conn.Write(p)
}

func (c *stalledConn) Read(p []byte) (int, error) {
	if c.armed.Load() {
		c.socket.fired.Store(true)
		return 0, &net.OpError{
			Op: "read", Net: "tcp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded,
		}
	}
	return c.Conn.Read(p)
}
