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
// or a call on the pgconn under a pgx connection. A pgx tracer sees only some of these: a
// QueryTracer misses a batch's statements, and no tracer sees a call on the pgconn. A statement is
// one execution: a simple query's text, or the Bind of a prepared statement, with the SQL that
// statement was prepared from on its connection and the values bound to it.
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
// message it could not decode, or a statement run by a name its connection never prepared. A test
// that reads Statements must fail on any of them, since a statement the recorder could not read
// may be the one the test looks for.
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
			return &sendingConn{Conn: conn, sent: sent, prepared: map[string]string{}}, nil
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

// sendingConn is a connection whose writes its SentStatements reads before they leave. pending
// holds the bytes of a message not yet whole; prepared holds this connection's prepared statements,
// by name.
type sendingConn struct {
	net.Conn
	sent     *SentStatements
	mu       sync.Mutex
	pending  []byte
	started  bool
	lost     bool
	prepared map[string]string
}

func (c *sendingConn) Write(p []byte) (int, error) {
	c.read(p)
	return c.Conn.Write(p)
}

// read reads the whole messages the bytes written so far hold. A packet before the startup packet
// has no type byte: its length, then a code. Every message after it has a type byte, then its
// length, which counts itself and not the type byte.
func (c *sendingConn) read(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lost {
		return
	}
	c.pending = append(c.pending, p...)
	for {
		if !c.started {
			if len(c.pending) < startupHeaderBytes {
				return
			}
			length := int(binary.BigEndian.Uint32(c.pending))
			if length < startupHeaderBytes {
				c.lose(fmt.Sprintf("a startup packet of length %d", length))
				return
			}
			if len(c.pending) < length {
				return
			}
			switch code := binary.BigEndian.Uint32(c.pending[4:8]); code {
			case sslRequestCode, gssEncRequestCode:
				c.lose(fmt.Sprintf("a connection asked for encryption (code %d), and what it sends after cannot be read", code))
				return
			case cancelRequestCode:
			default:
				c.started = true
			}
			c.pending = c.pending[length:]
			continue
		}
		if len(c.pending) < messageHeaderBytes {
			return
		}
		length := int(binary.BigEndian.Uint32(c.pending[1:5])) + 1
		if length < messageHeaderBytes {
			c.lose(fmt.Sprintf("a %q message of length %d", c.pending[0], length-1))
			return
		}
		if len(c.pending) < length {
			return
		}
		c.message(c.pending[0], c.pending[messageHeaderBytes:length])
		c.pending = c.pending[length:]
	}
}

// lose records what could not be read and reads nothing more of this connection, whose later
// bytes no longer start on a message.
func (c *sendingConn) lose(what string) {
	c.lost = true
	c.pending = nil
	c.sent.blindTo(what)
}

// message reads one whole message: a simple query and the Bind of a prepared statement are
// statements the server runs, and a Parse or a Close changes what a later Bind's name stands for.
// The other messages - Describe, Execute, Sync, Flush, the copy and authentication messages -
// carry no statement.
func (c *sendingConn) message(kind byte, body []byte) {
	switch kind {
	case 'Q':
		var query pgproto3.Query
		if err := query.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a simple query that does not decode: %v", err))
			return
		}
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
		c.sent.record(SentStatement{SQL: query, formats: append([]int16(nil), bind.ParameterFormatCodes...), values: values})
	case 'C':
		var closing pgproto3.Close
		if err := closing.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a Close that does not decode: %v", err))
			return
		}
		if closing.ObjectType == 'S' {
			delete(c.prepared, closing.Name)
		}
	case 'F':
		var call pgproto3.FunctionCall
		if err := call.Decode(body); err != nil {
			c.sent.blindTo(fmt.Sprintf("a function call that does not decode: %v", err))
			return
		}
		c.sent.record(SentStatement{SQL: fmt.Sprintf("a call of the function with object id %d", call.Function)})
	}
}
