package storetest

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// PoolPing is the statement pgxpool sends to check a connection that sat idle before it hands the
// connection out (pgconn.PgConn.Ping): a comment, which reads and writes nothing.
const PoolPing = "-- ping"

// SentStatements records every statement the connections of the store SentStore opens send to the
// server. It reads the bytes each connection writes before they leave, so a statement shows
// whatever sent it: a query, a batch or pipeline, a statement prepared and then run by its name,
// a portal bound in one round trip and run in a later one, or a call on the pgconn under a pgx
// connection. A pgx tracer sees only some of these: a QueryTracer misses a batch's statements, and
// no tracer sees a call on the pgconn. A statement is one execution, as the server runs it: a
// simple query's text, an Execute of a portal with the SQL and values the portal was bound with,
// or a function call. A Bind that no Execute follows runs nothing and records nothing.
//
// Its boundary is the store's own connections: those of its pool, the lease and listener
// connections opened from its configuration, and any connection opened from a copy of that
// configuration, since each dials through the same recorder. A connection the code under test
// opens itself from a connection string dials past it, and what that connection sends is not
// recorded.
type SentStatements struct {
	mu    sync.Mutex
	sent  []SentStatement
	blind []string
}

// SentStatement is one statement a connection sent (SentStatements).
type SentStatement struct {
	SQL     string
	formats []int16
	values  [][]byte
}

// Text is the index'th value bound to s, as text: a text value's bytes are the same in either
// format.
func (s SentStatement) Text(index int) (string, bool) {
	if index >= len(s.values) || s.values[index] == nil {
		return "", false
	}
	return string(s.values[index]), true
}

// Int8 is the index'th value bound to s, as a bigint: eight bytes, big-endian, in the binary
// format, and its decimal digits in the text format.
func (s SentStatement) Int8(index int) (int64, bool) {
	if index >= len(s.values) || s.values[index] == nil {
		return 0, false
	}
	value := s.values[index]
	if s.format(index) == 1 {
		if len(value) != 8 {
			return 0, false
		}
		return int64(binary.BigEndian.Uint64(value)), true
	}
	number, err := strconv.ParseInt(string(value), 10, 64)
	return number, err == nil
}

// format is the format code of s's index'th value: none given is text for every value, and one
// given is for every value.
func (s SentStatement) format(index int) int16 {
	switch {
	case len(s.formats) == 0:
		return 0
	case len(s.formats) == 1:
		return s.formats[0]
	case index < len(s.formats):
		return s.formats[index]
	default:
		return 0
	}
}

// Statements is every statement recorded, in the order the connections sent them.
func (r *SentStatements) Statements() []SentStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SentStatement(nil), r.sent...)
}

// Blind is each thing the recorder could not read: a connection that asked for encryption, a
// message it could not decode, a Bind of a statement its connection never prepared, or an Execute
// of a portal its connection has not bound. A test that reads Statements must fail on any of them,
// since a statement the recorder could not read may be the one the test looks for.
func (r *SentStatements) Blind() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.blind...)
}

func (r *SentStatements) record(statement SentStatement) {
	r.mu.Lock()
	r.sent = append(r.sent, statement)
	r.mu.Unlock()
}

func (r *SentStatements) blindTo(what string) {
	r.mu.Lock()
	r.blind = append(r.blind, what)
	r.mu.Unlock()
}

// SentStore is a store on database's database whose every connection records what it sends in
// sent: the pool's connections, and the lease and listener connections opened from its
// configuration (store.Pool.Leases, store.Pool.Listener). It opens no encrypted connection, since
// sent could not read one: a server that requires encryption refuses the connection, and the test
// fails rather than reading nothing.
func SentStore(t testing.TB, database *store.Store, sent *SentStatements) *store.Store {
	t.Helper()
	return &store.Store{Pool: reopenedPool(t, database.Pool, func(config *pgxpool.Config) {
		config.ConnConfig.TLSConfig = nil
		for _, fallback := range config.ConnConfig.Fallbacks {
			fallback.TLSConfig = nil
		}
		dial := config.ConnConfig.DialFunc
		if dial == nil {
			dial = (&net.Dialer{}).DialContext
		}
		config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &sendingConn{Conn: conn, sent: sent, prepared: map[string]string{}, portals: map[string]boundPortal{}}, nil
		}
	})}
}

// The codes of the packets a client may send before its startup packet, in place of the protocol
// version.
const (
	cancelRequestCode  = 80877102
	sslRequestCode     = 80877103
	gssEncRequestCode  = 80877104
	startupHeaderBytes = 8
	messageHeaderBytes = 5
)

// boundPortal is a portal a Bind made: the statement an Execute of it runs, and how many sync
// points (syncs) the connection had written before the Bind.
type boundPortal struct {
	statement SentStatement
	after     int
}

// sendingConn is a connection whose writes its SentStatements reads before they leave, and whose
// reads it reads for the server's ReadyForQuery messages, which say when a transaction ended and
// with it every portal bound in it. written and answered hold the bytes of a message not yet
// whole each way; prepared holds this connection's prepared statements and portals its portals,
// by name. syncs counts the sync points written - a Sync, a simple query, a function call - each
// of which the server answers with one ReadyForQuery; ready counts those answers read, after the
// one that ends the startup (greeted).
type sendingConn struct {
	net.Conn
	sent     *SentStatements
	mu       sync.Mutex
	written  []byte
	answered []byte
	started  bool
	greeted  bool
	lost     bool
	prepared map[string]string
	portals  map[string]boundPortal
	syncs    int
	ready    int
}

func (c *sendingConn) Write(p []byte) (int, error) {
	c.readWritten(p)
	return c.Conn.Write(p)
}

func (c *sendingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.readAnswered(p[:n])
	}
	return n, err
}

// readWritten reads the whole messages the bytes written so far hold. A packet before the startup
// packet has no type byte: its length, then a code. Every message after it has a type byte, then
// its length, which counts itself and not the type byte.
func (c *sendingConn) readWritten(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lost {
		return
	}
	c.written = append(c.written, p...)
	for {
		if !c.started {
			if len(c.written) < startupHeaderBytes {
				return
			}
			length := int(binary.BigEndian.Uint32(c.written))
			if length < startupHeaderBytes {
				c.lose(fmt.Sprintf("a startup packet of length %d", length))
				return
			}
			if len(c.written) < length {
				return
			}
			switch code := binary.BigEndian.Uint32(c.written[4:8]); code {
			case sslRequestCode, gssEncRequestCode:
				c.lose(fmt.Sprintf("a connection asked for encryption (code %d), and what it sends after cannot be read", code))
				return
			case cancelRequestCode:
			default:
				c.started = true
			}
			c.written = c.written[length:]
			continue
		}
		kind, body, length, whole := c.next(c.written)
		if !whole {
			return
		}
		c.sentMessage(kind, body)
		c.written = c.written[length:]
	}
}

// readAnswered reads the whole messages the bytes read so far hold, every one of which, from the
// server, has a type byte.
func (c *sendingConn) readAnswered(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lost {
		return
	}
	c.answered = append(c.answered, p...)
	for {
		kind, body, length, whole := c.next(c.answered)
		if !whole {
			return
		}
		if kind == 'Z' {
			c.readyForQuery(body)
		}
		c.answered = c.answered[length:]
	}
}

// next is the first message pending holds: its type, its body and its whole length, or false when
// pending does not yet hold all of it. A length too short to be one loses the connection.
func (c *sendingConn) next(pending []byte) (kind byte, body []byte, length int, whole bool) {
	if len(pending) < messageHeaderBytes {
		return 0, nil, 0, false
	}
	length = int(binary.BigEndian.Uint32(pending[1:5])) + 1
	if length < messageHeaderBytes {
		c.lose(fmt.Sprintf("a %q message of length %d", pending[0], length-1))
		return 0, nil, 0, false
	}
	if len(pending) < length {
		return 0, nil, 0, false
	}
	return pending[0], pending[messageHeaderBytes:length], length, true
}

// lose records what could not be read and reads nothing more of this connection, whose later
// bytes no longer start on a message.
func (c *sendingConn) lose(what string) {
	c.lost = true
	c.written, c.answered = nil, nil
	c.sent.blindTo(what)
}

// sentMessage reads one whole message the connection sent. A simple query, an Execute and a
// function call are statements the server runs. A Parse, a Bind and a Close change what a later
// Bind's statement name or Execute's portal name stands for, and a Sync, a simple query and a
// function call are each a sync point, which the server answers with one ReadyForQuery. A simple
// query also drops the unnamed statement and the unnamed portal. The other messages - Describe,
// Flush, the copy and authentication messages - neither run a statement nor change one.
func (c *sendingConn) sentMessage(kind byte, body []byte) {
	switch kind {
	case 'Q':
		var query pgproto3.Query
		if err := query.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a simple query that does not decode: %v", err))
			return
		}
		delete(c.prepared, "")
		delete(c.portals, "")
		c.syncs++
		c.sent.record(SentStatement{SQL: query.String})
	case 'P':
		var parse pgproto3.Parse
		if err := parse.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a Parse that does not decode: %v", err))
			return
		}
		c.prepared[parse.Name] = parse.Query
	case 'B':
		var bind pgproto3.Bind
		if err := bind.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a Bind that does not decode: %v", err))
			return
		}
		query, known := c.prepared[bind.PreparedStatement]
		if !known {
			c.sent.blindTo(fmt.Sprintf("a Bind of the statement %q, which its connection never prepared", bind.PreparedStatement))
			return
		}
		values := make([][]byte, len(bind.Parameters))
		for index, value := range bind.Parameters {
			if value != nil {
				values[index] = append([]byte{}, value...)
			}
		}
		c.portals[bind.DestinationPortal] = boundPortal{
			statement: SentStatement{SQL: query, formats: append([]int16(nil), bind.ParameterFormatCodes...), values: values},
			after:     c.syncs,
		}
	case 'E':
		var execute pgproto3.Execute
		if err := execute.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("an Execute that does not decode: %v", err))
			return
		}
		portal, bound := c.portals[execute.Portal]
		if !bound {
			c.sent.blindTo(fmt.Sprintf("an Execute of the portal %q, which its connection has not bound", execute.Portal))
			return
		}
		c.sent.record(portal.statement)
	case 'C':
		var closing pgproto3.Close
		if err := closing.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a Close that does not decode: %v", err))
			return
		}
		switch closing.ObjectType {
		case 'S':
			delete(c.prepared, closing.Name)
		case 'P':
			delete(c.portals, closing.Name)
		}
	case 'S':
		c.syncs++
	case 'F':
		var call pgproto3.FunctionCall
		if err := call.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a function call that does not decode: %v", err))
			return
		}
		c.syncs++
		c.sent.record(SentStatement{SQL: fmt.Sprintf("a call of the function with object id %d", call.Function)})
	}
}

// readyForQuery reads the server's answer to a sync point. The first ReadyForQuery ends the
// startup and answers none. Each after it answers the next sync point the connection wrote, and
// says whether a transaction is still open there: when none is ('I'), the transaction that held
// every portal bound before that sync point has ended, and those portals with it. A portal bound
// after it belongs to the next transaction and stays.
func (c *sendingConn) readyForQuery(body []byte) {
	if !c.greeted {
		c.greeted = true
		return
	}
	c.ready++
	if len(body) != 1 {
		c.sent.blindTo(fmt.Sprintf("a ReadyForQuery of %d bytes", len(body)))
		return
	}
	if body[0] != 'I' {
		return
	}
	for name, portal := range c.portals {
		if portal.after < c.ready {
			delete(c.portals, name)
		}
	}
}
