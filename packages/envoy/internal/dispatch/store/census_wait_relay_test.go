package store

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/sjawhar/envoy/internal/testport"
)

// waitRelay is a TCP relay between a census's connection and the Postgres server. It passes every
// byte both ways as it comes, and reads the server's messages as they pass: of the first row of
// each statement's answer whose query column holds marker, a literal of the holder's query, it
// keeps the wait_event_type and wait_event columns that the RowDescription before it names, as
// Postgres sent them, before it passes the row on. Of the census's statements only the diagnosis
// query reads another session's pg_stat_activity.query, so what it keeps is Postgres's answer to
// that query: the wait the census read, which no census code writes or moves.
// TestCensusRefusesALockHolderWithItsDiagnosis reads the holder's wait through it.
type waitRelay struct {
	listener net.Listener
	marker   string
	mu       sync.Mutex
	// answers are the waits kept since the last take.
	answers []postgresWait
	// conns are the connections the relay made or took, each closed at close; closed says close
	// has run.
	conns  []net.Conn
	closed bool
}

// postgresWait is a session's wait as one answer of Postgres names it: its wait_event_type and
// wait_event columns, each nil where Postgres sent null.
type postgresWait struct{ eventType, event *string }

func (w postgresWait) is(eventType, event string) bool {
	return w.eventType != nil && *w.eventType == eventType && w.event != nil && *w.event == event
}

func (w postgresWait) String() string {
	column := func(value *string) string {
		if value == nil {
			return "null"
		}
		return strconv.Quote(*value)
	}
	return "wait_event_type " + column(w.eventType) + " and wait_event " + column(w.event)
}

// relayWaits starts a waitRelay to the server databaseURL names, keeping the waits of the rows whose
// query column holds marker, and answers it with databaseURL pointed at it. It closes the relay and
// every connection it made when t ends. It fails t, naming what the relay needs, where databaseURL
// does not say sslmode=disable (the relay reads Postgres's messages, which TLS hides) or names its
// server by anything but the URL's host and port — never skips, so the restored row can never pass
// by asking nothing. CI's and the build cluster's URLs are plain TCP with sslmode=disable.
func relayWaits(t *testing.T, databaseURL, marker string) (*waitRelay, string) {
	t.Helper()
	server, err := neturl.Parse(databaseURL)
	if err != nil {
		// url.Parse's own error quotes the URL, which carries a credential.
		t.Fatal("the test database's URL does not parse")
	}
	query := server.Query()
	if query.Get("sslmode") != "disable" {
		t.Fatal("the restored census row needs a plain-TCP DISPATCH_TEST_DATABASE_URL with sslmode=disable: the relay it reads the holder's wait through reads Postgres's messages, which TLS hides")
	}
	if server.Hostname() == "" || query.Has("host") || query.Has("hostaddr") || query.Has("port") {
		t.Fatal("the restored census row needs a DISPATCH_TEST_DATABASE_URL that names its server by the URL's host and port alone, so the census can go through the relay it reads the holder's wait through")
	}
	port := server.Port()
	if port == "" {
		port = "5432"
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	relay := &waitRelay{listener: listener, marker: marker}
	go relay.serve(net.JoinHostPort(server.Hostname(), port))
	t.Cleanup(relay.close)
	relayed := *server
	relayed.Host = listener.Addr().String()
	return relay, relayed.String()
}

// take answers the waits kept since the last take, in the order Postgres sent them.
func (r *waitRelay) take() []postgresWait {
	r.mu.Lock()
	defer r.mu.Unlock()
	answers := r.answers
	r.answers = nil
	return answers
}

// track keeps conn for close, and answers false, closing conn, once the relay is closed.
func (r *waitRelay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = conn.Close()
		return false
	}
	r.conns = append(r.conns, conn)
	return true
}

func (r *waitRelay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	_ = r.listener.Close()
	for _, conn := range r.conns {
		_ = conn.Close()
	}
}

func (r *waitRelay) serve(upstream string) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client, upstream)
	}
}

// relay carries one client connection to upstream: the client's bytes on as they come, and the
// server's messages back (pass).
func (r *waitRelay) relay(client net.Conn, upstream string) {
	if !r.track(client) {
		return
	}
	server, err := net.Dial("tcp", upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	if !r.track(server) {
		_ = client.Close()
		return
	}
	go func() {
		_, _ = io.Copy(server, client)
		_ = server.Close()
	}()
	r.pass(bufio.NewReader(server), client)
	_ = client.Close()
}

// pass sends the client each message the server sends, a type byte and a length that counts
// itself, and keeps the wait of the first row of a statement's answer whose query column holds the
// relay's marker, by the columns the last RowDescription named, before it sends that row on. A
// statement's answer ends with its CommandComplete, an ErrorResponse, an EmptyQueryResponse or a
// PortalSuspended. A connection the client makes with sslmode=disable sends no SSLRequest, so
// every message the server sends it is typed. The relay frames each message itself, to pass its
// bytes on as Postgres sent them, and pgproto3 decodes the RowDescription and DataRow bodies.
func (r *waitRelay) pass(server *bufio.Reader, client net.Conn) {
	var columns *pgproto3.RowDescription
	kept := false
	for {
		kind, err := server.ReadByte()
		if err != nil {
			return
		}
		length, body, err := readPostgresMessage(server)
		if err != nil {
			return
		}
		switch kind {
		case 'T':
			columns = &pgproto3.RowDescription{}
			if columns.Decode(body) != nil {
				columns = nil
			}
		case 'D':
			if wait, ok := markedWait(columns, body, r.marker); ok && !kept {
				kept = true
				r.mu.Lock()
				r.answers = append(r.answers, wait)
				r.mu.Unlock()
			}
		case 'C', 'E', 'I', 's':
			kept = false
		}
		if _, err := client.Write(append(append([]byte{kind}, length[:]...), body...)); err != nil {
			return
		}
	}
}

// readPostgresMessage reads a message's length, which counts itself, and the body that follows it.
func readPostgresMessage(from *bufio.Reader) ([4]byte, []byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(from, length[:]); err != nil {
		return length, nil, err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size < 4 {
		return length, nil, fmt.Errorf("a message length of %d, shorter than the length itself", size)
	}
	body := make([]byte, size-4)
	_, err := io.ReadFull(from, body)
	return length, body, err
}

// markedWait is the wait a DataRow body names, by the columns of columns, where its query column
// holds marker. A text column's bytes are its text in either format Postgres answers in, and
// pgproto3 decodes a null as a nil value. ok is false for any other row, for a body that does not
// decode, and where columns is nil (no RowDescription decoded) or names no query, wait_event_type
// or wait_event column.
func markedWait(columns *pgproto3.RowDescription, body []byte, marker string) (wait postgresWait, ok bool) {
	if columns == nil {
		return postgresWait{}, false
	}
	index := func(name string) int {
		return slices.IndexFunc(columns.Fields, func(field pgproto3.FieldDescription) bool { return string(field.Name) == name })
	}
	query, eventType, event := index("query"), index("wait_event_type"), index("wait_event")
	var row pgproto3.DataRow
	if query < 0 || eventType < 0 || event < 0 || row.Decode(body) != nil {
		return postgresWait{}, false
	}
	if max(query, eventType, event) >= len(row.Values) || row.Values[query] == nil || !strings.Contains(string(row.Values[query]), marker) {
		return postgresWait{}, false
	}
	text := func(value []byte) *string {
		if value == nil {
			return nil
		}
		s := string(value)
		return &s
	}
	return postgresWait{eventType: text(row.Values[eventType]), event: text(row.Values[event])}, true
}
