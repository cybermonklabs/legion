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

// SentStatements records every statement the server ran or refused on the connections of the store
// SentStore opens. It reads the bytes each connection writes and the server's answers on the same
// connection, so a statement shows whatever sent it: a query, a batch or pipeline, a statement
// prepared and then run by its name, a portal bound in one round trip and run in a later one, or a
// call on the pgconn under a pgx connection. A pgx tracer sees only some of these: a QueryTracer
// misses a batch's statements, and no tracer sees a call on the pgconn.
//
// Every effect follows the server's answer, not the client's send. A Parse, a Bind and a Close
// take effect on their ParseComplete, BindComplete and CloseComplete. A statement is recorded when
// the server says it ran it: an Execute of a portal on its CommandComplete, EmptyQueryResponse or
// PortalSuspended, with the SQL and values the portal was bound with; each statement of a simple
// query on its CommandComplete or EmptyQueryResponse; a function call on its result. A statement
// the server answers with an error is recorded as Failed; the messages the server then discards,
// up to the next Sync, have no effect. A Bind that no Execute follows runs nothing and records
// nothing.
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

// SentStatement is one statement the server ran or refused (SentStatements). Failed is whether the
// server answered it with an error, which Code holds: a statement refused before it ran, or one
// that failed while it ran.
type SentStatement struct {
	SQL     string
	Failed  bool
	Code    string
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

// Statements is every statement recorded, in the order the server answered them.
func (r *SentStatements) Statements() []SentStatement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SentStatement(nil), r.sent...)
}

// Blind is each thing the recorder could not read: a connection that asked for encryption, a
// message it could not decode, or an answer of the server it could not pair with what the
// connection sent - a statement run from a portal or prepared statement it never saw made, an
// answer nothing sent asked for. A connection it could not read it reads no further. A test that
// reads Statements must fail on any of them, since a statement the recorder could not read may be
// the one the test looks for.
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
			return newSendingConn(conn, sent), nil
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

// sent is one message a connection sent that the server answers, waiting for that answer: its
// type, and what the answer makes of it.
type sent struct {
	kind byte
	// name is a Parse's statement, a Bind's or an Execute's portal, a Close's or a Describe's
	// object.
	name string
	// statement is a Bind's prepared statement.
	statement string
	// query is a Parse's SQL or a simple query's text.
	query string
	// object is a Close's or a Describe's object type: 'S' a statement, 'P' a portal.
	object  byte
	formats []int16
	values  [][]byte
	// function is a function call's object id.
	function uint32
}

// sendingConn is a connection whose writes and reads its SentStatements reads. written and
// answered hold the bytes of a message not yet whole each way. waiting holds, in order, the
// messages sent that the server has not yet answered; discarding is set from a refusal in an
// extended query until the client's next Sync, since the server discards everything sent in
// between. prepared holds the connection's prepared statements and portals its portals, by name,
// each as the server last acknowledged it.
type sendingConn struct {
	net.Conn
	recorder   *SentStatements
	mu         sync.Mutex
	written    []byte
	answered   []byte
	started    bool
	greeted    bool
	lost       bool
	waiting    []sent
	discarding bool
	prepared   map[string]string
	portals    map[string]SentStatement
}

func newSendingConn(conn net.Conn, recorder *SentStatements) *sendingConn {
	return &sendingConn{Conn: conn, recorder: recorder, prepared: map[string]string{}, portals: map[string]SentStatement{}}
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
	for !c.lost {
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
		c.written = c.written[length:]
		c.sentMessage(kind, body)
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
	for !c.lost {
		kind, body, length, whole := c.next(c.answered)
		if !whole {
			return
		}
		c.answered = c.answered[length:]
		c.answer(kind, body)
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
// messages it can no longer pair with their answers.
func (c *sendingConn) lose(what string) {
	c.lost = true
	c.written, c.answered, c.waiting = nil, nil, nil
	c.recorder.blindTo(what)
}

// sentMessage reads one whole message the connection sent, and keeps the ones the server answers -
// a Parse, a Bind, a Close, a Describe, an Execute, a Sync, a simple query, a function call - in
// order, until their answers come. A Flush, a Terminate, and the copy and authentication messages
// get no answer of their own. While the server discards what follows a refusal, everything before
// the next Sync has no effect.
func (c *sendingConn) sentMessage(kind byte, body []byte) {
	if c.discarding {
		if kind != 'S' {
			return
		}
		c.discarding = false
	}
	switch kind {
	case 'P':
		var parse pgproto3.Parse
		if err := parse.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a Parse that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, name: parse.Name, query: parse.Query})
	case 'B':
		var bind pgproto3.Bind
		if err := bind.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a Bind that does not decode: %v", err))
			return
		}
		values := make([][]byte, len(bind.Parameters))
		for index, value := range bind.Parameters {
			if value != nil {
				values[index] = append([]byte{}, value...)
			}
		}
		c.waiting = append(c.waiting, sent{
			kind: kind, name: bind.DestinationPortal, statement: bind.PreparedStatement,
			formats: append([]int16(nil), bind.ParameterFormatCodes...), values: values,
		})
	case 'C':
		var closing pgproto3.Close
		if err := closing.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a Close that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, name: closing.Name, object: closing.ObjectType})
	case 'D':
		var describe pgproto3.Describe
		if err := describe.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a Describe that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, name: describe.Name, object: describe.ObjectType})
	case 'E':
		var execute pgproto3.Execute
		if err := execute.Decode(body); err != nil {
			c.lose(fmt.Sprintf("an Execute that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, name: execute.Portal})
	case 'S':
		c.waiting = append(c.waiting, sent{kind: kind})
	case 'Q':
		var query pgproto3.Query
		if err := query.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a simple query that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, query: query.String})
	case 'F':
		var call pgproto3.FunctionCall
		if err := call.Decode(body); err != nil {
			c.lose(fmt.Sprintf("a function call that does not decode: %v", err))
			return
		}
		c.waiting = append(c.waiting, sent{kind: kind, function: call.Function})
	}
}

// answer reads one whole message from the server and pairs it with the oldest message sent that
// waits for it. The startup's own messages, up to its ReadyForQuery, answer nothing sent; a
// notice, a parameter status or a notification answers nothing either, wherever it comes.
func (c *sendingConn) answer(kind byte, body []byte) {
	if !c.greeted {
		if kind == 'Z' {
			c.greeted = true
		}
		return
	}
	switch kind {
	case 'N', 'S', 'A':
		return
	}
	if len(c.waiting) == 0 {
		c.lose(fmt.Sprintf("a %q message from the server, which nothing the connection sent waits for", kind))
		return
	}
	head := &c.waiting[0]
	switch {
	case kind == 'E':
		c.refused(body)
	case kind == 'Z':
		c.readyForQuery(body)
	case kind == '1' && head.kind == 'P':
		c.prepared[head.name] = head.query
		c.done()
	case kind == '2' && head.kind == 'B':
		query, known := c.prepared[head.statement]
		if !known {
			c.lose(fmt.Sprintf("a BindComplete for the statement %q, which the server never acknowledged preparing", head.statement))
			return
		}
		c.portals[head.name] = SentStatement{SQL: query, formats: head.formats, values: head.values}
		c.done()
	case kind == '3' && head.kind == 'C':
		if head.object == 'S' {
			delete(c.prepared, head.name)
		} else {
			delete(c.portals, head.name)
		}
		c.done()
	case kind == 't' && head.kind == 'D' && head.object == 'S':
	case (kind == 'T' || kind == 'n') && head.kind == 'D':
		c.done()
	case (kind == 'C' || kind == 'I' || kind == 's') && head.kind == 'E':
		portal, bound := c.portals[head.name]
		if !bound {
			c.lose(fmt.Sprintf("the server ran the portal %q, which it never acknowledged binding", head.name))
			return
		}
		c.recorder.record(portal)
		c.done()
	case (kind == 'C' || kind == 'I') && head.kind == 'Q':
		c.recorder.record(SentStatement{SQL: head.query})
	case kind == 'V' && head.kind == 'F':
		c.recorder.record(SentStatement{SQL: fmt.Sprintf("a call of the function with object id %d", head.function)})
	case (kind == 'D' || kind == 'G' || kind == 'H' || kind == 'W' || kind == 'd' || kind == 'c') && (head.kind == 'E' || head.kind == 'Q'):
	case kind == 'T' && head.kind == 'Q':
	default:
		c.lose(fmt.Sprintf("a %q message from the server, which the %q waiting for an answer does not expect", kind, head.kind))
	}
}

// done drops the oldest message waiting, which its answer has ended.
func (c *sendingConn) done() {
	c.waiting = c.waiting[1:]
}

// refused reads the server's ErrorResponse to the oldest message waiting. An Execute, a simple
// query or a function call it refuses is recorded as Failed. In an extended query the server then
// discards every message up to the next Sync, so none of them has any effect, and those sent after
// this answer are discarded as they come (discarding). A simple query, a function call or a Sync
// it refuses still waits for its ReadyForQuery.
func (c *sendingConn) refused(body []byte) {
	var refusal pgproto3.ErrorResponse
	if err := refusal.Decode(body); err != nil {
		c.lose(fmt.Sprintf("an ErrorResponse that does not decode: %v", err))
		return
	}
	head := c.waiting[0]
	switch head.kind {
	case 'Q':
		c.recorder.record(SentStatement{SQL: head.query, Failed: true, Code: refusal.Code})
		return
	case 'F':
		c.recorder.record(SentStatement{SQL: fmt.Sprintf("a call of the function with object id %d", head.function), Failed: true, Code: refusal.Code})
		return
	case 'S':
		return
	case 'E':
		statement, bound := c.portals[head.name]
		if !bound {
			statement = SentStatement{SQL: fmt.Sprintf("the portal %q, which the server never acknowledged binding", head.name)}
		}
		statement.Failed, statement.Code = true, refusal.Code
		c.recorder.record(statement)
	}
	for len(c.waiting) > 0 && c.waiting[0].kind != 'S' {
		c.done()
	}
	c.discarding = len(c.waiting) == 0
}

// readyForQuery reads the server's ReadyForQuery, which ends a Sync, a simple query or a function
// call, and says whether a transaction is still open there. When none is ('I'), every portal has
// ended with the transaction that held it; a simple query has also dropped the unnamed statement
// and the unnamed portal.
func (c *sendingConn) readyForQuery(body []byte) {
	head := c.waiting[0]
	if head.kind != 'S' && head.kind != 'Q' && head.kind != 'F' {
		c.lose(fmt.Sprintf("a ReadyForQuery, which the %q waiting for an answer does not expect", head.kind))
		return
	}
	if len(body) != 1 {
		c.lose(fmt.Sprintf("a ReadyForQuery of %d bytes", len(body)))
		return
	}
	c.done()
	if head.kind == 'Q' {
		delete(c.prepared, "")
		delete(c.portals, "")
	}
	if body[0] == 'I' {
		clear(c.portals)
	}
}
