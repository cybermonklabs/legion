package testdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// faultMetBound is the longest Met waits for an armed fault to meet its COMMIT.
const faultMetBound = 10 * time.Second

// Cut is how a fault breaks the client's connection at the COMMIT it meets.
type Cut int

const (
	CutBeforeCommit        Cut = iota + 1 // the client's connection closes and the COMMIT never reaches the server: the server rolls back
	CutAfterCommit                        // the COMMIT reaches the server, its answer is read and dropped, the client's connection closes
	CutAfterCommitRefusing                // CutAfterCommit, with Refuse(true) taking effect before the client's connection closes
)

// CommitFaults is dsn through a TCP proxy on 127.0.0.1 that passes the Postgres protocol unchanged
// until a fault is armed.
//
// An armed fault applies once, to the next Query message whose text, trimmed and in lower case, is
// `commit`, on a connection whose transaction has sent SQL containing the fault's match since its
// last Query message starting with `begin`; an empty match matches any transaction. A Query
// message and a Parse message send their SQL, and so does a Bind message, through the SQL the
// connection's Parse gave the prepared statement it names, because pgx runs a statement it has
// cached with a Bind alone. The channel a fault answers closes once the fault has met its COMMIT,
// and a test waits on it only through Met. Arming a fault while another is still pending fails the
// test.
//
// The URL comes from listenProxy, as Delayed's does, and its refusals print no value of the DSN.
// Every connection through the proxy closes when the test's context ends, before any cleanup
// runs, and the close drops any bytes a fault holds, so a hold of an hour never holds the test's
// end, even where a cleanup waits for the connection a held Commit has (a pool's Close). The
// proxy's own cleanup waits for its goroutines before the database is dropped (Database's cleanup
// is registered first, so it runs after this one).
func CommitFaults(t testing.TB, dsn string) (string, *CommitFault) {
	t.Helper()
	listener, upstream, through := listenProxy(t, dsn, "the commit fault proxy", false)
	fault := &CommitFault{t: t, listener: listener, upstream: upstream, links: map[*link]struct{}{}}
	fault.wg.Add(1)
	go fault.serve()
	context.AfterFunc(t.Context(), fault.close)
	t.Cleanup(fault.close)
	return through, fault
}

// CommitFault is CommitFaults' proxy: the fault armed on it, whether it refuses connections, and
// the links open through it, closed with it.
type CommitFault struct {
	t        testing.TB
	listener net.Listener
	upstream string

	mu      sync.Mutex
	closed  bool
	refused bool
	armed   *armedFault
	links   map[*link]struct{}
	wg      sync.WaitGroup
}

// armedFault is a fault waiting for its COMMIT: the SQL that COMMIT's transaction must have sent,
// what the fault does to that COMMIT's link, and the channel meet closes once it has done it.
type armedFault struct {
	match string
	meet  func(l *link, commit []byte, fired chan<- struct{})
	fired chan struct{}
}

// HoldReply arms a fault that writes its COMMIT to the server, closes its channel, and from then
// on holds the server's bytes to that client for hold.
func (f *CommitFault) HoldReply(match string, hold time.Duration) <-chan struct{} {
	f.t.Helper()
	return f.arm(match, func(l *link, commit []byte, fired chan<- struct{}) {
		l.mu.Lock()
		l.heldUntil = time.Now().Add(hold)
		l.mu.Unlock()
		_, _ = l.server.Write(commit)
		close(fired)
	})
}

// Cut arms a fault that breaks the client's connection at its COMMIT, as cut says, and closes its
// channel once the client's connection is closed.
func (f *CommitFault) Cut(match string, cut Cut) <-chan struct{} {
	f.t.Helper()
	switch cut {
	case CutBeforeCommit:
		return f.arm(match, func(l *link, _ []byte, fired chan<- struct{}) {
			f.untrack(l)
			close(fired)
		})
	case CutAfterCommit:
		return f.arm(match, func(l *link, commit []byte, fired chan<- struct{}) {
			l.dropReply(func() {
				f.untrack(l)
				close(fired)
			})
			_, _ = l.server.Write(commit)
		})
	case CutAfterCommitRefusing:
		return f.arm(match, func(l *link, commit []byte, fired chan<- struct{}) {
			l.dropReply(func() {
				f.Refuse(true)
				close(fired)
			})
			_, _ = l.server.Write(commit)
		})
	}
	f.t.Fatalf("testdb: Cut %d is none of CutBeforeCommit, CutAfterCommit and CutAfterCommitRefusing", cut)
	return nil
}

// Refuse(true) closes every open connection through the proxy and closes each new one at once.
// Refuse(false) passes new connections again.
func (f *CommitFault) Refuse(refused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused = refused
	if refused {
		for l := range f.links {
			l.end()
		}
		clear(f.links)
	}
}

// Met waits for fired, the channel of an armed fault, for at most testwait.Bounded(t,
// faultMetBound, time.Second), and fails the test if the fault met no COMMIT by then.
func Met(t *testing.T, fired <-chan struct{}) {
	t.Helper()
	bound := testwait.Bounded(t, faultMetBound, time.Second)
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-fired:
		return
	case <-timer.C:
	}
	// Asked once more, so a fault that met its COMMIT as the bound passed is not reported missed.
	select {
	case <-fired:
	default:
		t.Fatalf("the armed fault met no COMMIT within %s: its match names no SQL that transaction sent", bound)
	}
}

func (f *CommitFault) arm(match string, meet func(l *link, commit []byte, fired chan<- struct{})) <-chan struct{} {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.armed != nil {
		f.t.Fatalf("testdb: a fault is armed while another still waits for its COMMIT, the one matching %q", f.armed.match)
	}
	f.armed = &armedFault{match: match, meet: meet, fired: make(chan struct{})}
	return f.armed.fired
}

// take is the armed fault, disarmed, when the transaction of l has sent SQL holding its match.
func (f *CommitFault) take(l *link) *armedFault {
	f.mu.Lock()
	defer f.mu.Unlock()
	fault := f.armed
	if fault == nil || !l.sentHolds(fault.match) {
		return nil
	}
	f.armed = nil
	return fault
}

func (f *CommitFault) serve() {
	defer f.wg.Done()
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", f.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		l := &link{client: client, server: server, ended: make(chan struct{}), statements: map[string]string{}}
		if !f.track(l) {
			continue
		}
		f.wg.Add(2)
		go func() {
			defer f.wg.Done()
			l.answer()
			f.untrack(l)
		}()
		go func() {
			defer f.wg.Done()
			f.relay(l)
			f.untrack(l)
		}()
	}
}

// track records l, and ends it instead when the proxy is closed or refusing.
func (f *CommitFault) track(l *link) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.refused {
		l.end()
		return false
	}
	f.links[l] = struct{}{}
	return true
}

// untrack ends l, once either direction ends or a fault cuts it.
func (f *CommitFault) untrack(l *link) {
	f.mu.Lock()
	delete(f.links, l)
	f.mu.Unlock()
	l.end()
}

// close ends every link and waits for the proxy's goroutines. It runs as the test's context ends
// and again as the proxy's cleanup, which finds nothing left open.
func (f *CommitFault) close() {
	f.mu.Lock()
	f.closed = true
	_ = f.listener.Close()
	for l := range f.links {
		l.end()
	}
	clear(f.links)
	f.mu.Unlock()
	f.wg.Wait()
}

// relay passes the client's messages to the server, one whole message at a time, so that it reads
// the SQL each one sends and meets an armed fault at its COMMIT.
func (f *CommitFault) relay(l *link) {
	in := bufio.NewReader(l.client)
	out := bufio.NewWriter(l.server)
	untyped := true
	for {
		message, err := nextMessage(in, untyped)
		if err != nil {
			return
		}
		if untyped {
			untyped = negotiates(message)
		} else if l.read(message) {
			if fault := f.take(l); fault != nil {
				if out.Flush() != nil {
					return
				}
				fault.meet(l, message, fault.fired)
				continue
			}
		}
		if _, err := out.Write(message); err != nil {
			return
		}
		if in.Buffered() == 0 {
			if err := out.Flush(); err != nil {
				return
			}
		}
	}
}

// maxStartupLength is the longest message without a type byte that the server reads
// (PostgreSQL's MAX_STARTUP_PACKET_LENGTH). The bytes of a TLS handshake read as a longer one, and
// the proxy ends such a link as the server would.
const maxStartupLength = 10000

// nextMessage is the client's next message, whole. Before the startup message, the client's
// messages carry no type byte, only an int32 length; after it, a type byte and an int32 length.
func nextMessage(in *bufio.Reader, untyped bool) ([]byte, error) {
	header := 5
	if untyped {
		header = 4
	}
	head, err := in.Peek(header)
	if err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint32(head[header-4:]))
	if length < 4 || untyped && (length < 8 || length > maxStartupLength) {
		return nil, fmt.Errorf("a message of length %d is none the server reads", length)
	}
	message := make([]byte, header-4+length)
	if _, err := io.ReadFull(in, message); err != nil {
		return nil, err
	}
	return message, nil
}

// negotiates says whether an untyped message asks the server for an encryption it answers with one
// byte (SSLRequest, GSSENCRequest), so the client's next message carries no type byte either.
func negotiates(message []byte) bool {
	const sslRequest, gssEncRequest = 80877103, 80877104
	code := binary.BigEndian.Uint32(message[4:8])
	return code == sslRequest || code == gssEncRequest
}

// link is one client's connection through the proxy, and the server connection dialled for it.
type link struct {
	client, server net.Conn
	ended          chan struct{} // closed once end has closed both connections
	endOnce        sync.Once

	// statements and sent are read and written by relay alone.
	statements map[string]string // the SQL of each prepared statement, by the name its Parse gave it
	sent       []string          // the SQL the transaction has sent since its last BEGIN

	mu        sync.Mutex
	heldUntil time.Time // the server's bytes to the client wait until then
	onReply   func()    // when set, the server's answer is read and dropped, and onReply runs
}

// read notes the SQL a typed message of the client sends, and says whether the message is a
// COMMIT.
func (l *link) read(message []byte) bool {
	body := message[5:]
	switch message[0] {
	case 'Q':
		sql, _ := cString(body)
		switch text := strings.ToLower(strings.TrimSpace(sql)); {
		case text == "commit":
			return true
		case strings.HasPrefix(text, "begin"):
			l.sent = l.sent[:0]
		default:
			l.sent = append(l.sent, sql)
		}
	case 'P':
		name, rest := cString(body)
		sql, _ := cString(rest)
		l.statements[name] = sql
		l.sent = append(l.sent, sql)
	case 'B':
		_, rest := cString(body) // the portal
		statement, _ := cString(rest)
		if sql, ok := l.statements[statement]; ok {
			l.sent = append(l.sent, sql)
		}
	}
	return false
}

// sentHolds says whether the transaction has sent SQL containing match; "" matches any.
func (l *link) sentHolds(match string) bool {
	if match == "" {
		return true
	}
	for _, sql := range l.sent {
		if strings.Contains(sql, match) {
			return true
		}
	}
	return false
}

// dropReply makes the server's next answer to this client read and dropped, up to its
// ReadyForQuery, after which reply runs. It is set before the COMMIT is written, so the answer to
// that COMMIT is the one dropped.
func (l *link) dropReply(reply func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onReply = reply
}

// answer copies the server's bytes to the client. Each chunk waits until heldUntil. Once onReply
// is set, the server's bytes are dropped until they hold a whole ReadyForQuery, and onReply runs.
// The client has read every answer before it sends its COMMIT, so the dropped bytes start at a
// message.
func (l *link) answer() {
	buffer := make([]byte, 64<<10)
	var dropped []byte
	for {
		n, err := l.server.Read(buffer)
		if n > 0 {
			l.mu.Lock()
			heldUntil, reply := l.heldUntil, l.onReply
			l.mu.Unlock()
			if reply != nil {
				dropped = append(dropped, buffer[:n]...)
				if readyForQuery(dropped) {
					reply()
					return
				}
			} else {
				if !l.waitUntil(heldUntil) {
					return
				}
				if _, err := l.client.Write(buffer[:n]); err != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// waitUntil waits until held, and says false if the link ended first, which drops what it held.
func (l *link) waitUntil(held time.Time) bool {
	wait := time.Until(held)
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-l.ended:
		return false
	}
}

// end closes both connections of the link, once.
func (l *link) end() {
	l.endOnce.Do(func() {
		_ = l.client.Close()
		_ = l.server.Close()
		close(l.ended)
	})
}

// readyForQuery says whether the server's messages in answer, read from the start of a message,
// hold a whole ReadyForQuery: the end of the server's answer to a query.
func readyForQuery(answer []byte) bool {
	for len(answer) >= 5 {
		length := int(binary.BigEndian.Uint32(answer[1:5]))
		if len(answer) < 1+length {
			return false
		}
		if answer[0] == 'Z' {
			return true
		}
		answer = answer[1+length:]
	}
	return false
}

// cString is the NUL-terminated string at the start of b, and what follows its NUL.
func cString(b []byte) (string, []byte) {
	end := bytes.IndexByte(b, 0)
	if end < 0 {
		return string(b), nil
	}
	return string(b[:end]), b[end+1:]
}
