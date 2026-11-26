package storetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testcleanup"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// teardownEndChildEnv selects TestTheTeardownEndsAtItsBoundChild in a child of this test binary.
const teardownEndChildEnv = "STORETEST_TEARDOWN_END_CHILD"

// teardownEndChildTimeout is the child's -timeout. Its connect end (testdb.ConnectEnd) is 30 s
// into its run, the time its OpenFrom has before that end, as the past-end row's first statement
// has; its teardown's bound is testdb.TeardownGrace past the -timeout, counted from the start of
// its tests (testdb.TeardownDeadline).
const teardownEndChildTimeout = 60 * time.Second

// teardownEndTemplate is the line in which the child names the template it made, so this row can
// drop it whatever the child's Run did with it.
var teardownEndTemplate = regexp.MustCompile(`the child made the template (dispatch_template_[0-9a-f]+)`)

// TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered is Tester5486's finding at f358579ae6 on
// cybermonklabs/legion#5189, made a row: a drop whose statement the server never answers held Run,
// and with it the test binary, until cmd/go's kill ("ran too long", naming no drop and no
// database), or for ever under -timeout 0. The child, under -timeout 60s, opens a store cloned from
// a template of its own (OpenFrom) through a relay that lets every login through and holds back
// each drop's statement (holdDrops): the clone's drop, which its test's cleanup starts, and the
// template's, which Run starts once the tests return. Run must then end at its bound,
// testdb.TeardownGrace past the child's -timeout, with a line naming each of the two databases,
// and exit with its tests' code. A Run whose wait for the drops has no bound never ends, and this
// row kills the child and fails by name testwait.Margin past that bound. A Run that starts the
// template's drop only after the clones' drops have ended never sends it once a clone's drop
// reaches the bound, and leaves the template unnamed: the relay then holds no template's drop, and
// the row fails by name (review 6067498217 at 5d0efe15a1). The row fails, too, where the child
// ended before its bound: then nothing held its teardown, and the run proves nothing. It drops the
// child's databases itself, on the server directly. The child skips, and this row on its skip,
// where its OpenFrom failed past its connect end, too slow for the row to say anything.
//
// It needs no superuser, only a server it reaches over TCP without TLS, since the relay reads the
// protocol's messages: it skips, by name, where DISPATCH_TEST_DATABASE_URL does not say
// sslmode=disable or names no host of its own, as CI's and the build cluster's both do.
func TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered(t *testing.T) {
	if os.Getenv(teardownEndChildEnv) != "" {
		t.Skip("this is the child's binary")
	}
	ready(t)
	upstream, why := plainServerAddress(shared.server)
	if why != "" {
		t.Skip(why)
	}
	relay := holdDrops(t, upstream)
	server := shared.server
	server.Host = relay.address()
	var output bytes.Buffer
	// The child's databases, which no drop of its reached the server for: each whose drop the
	// relay held, and the template it names, dropped here once the row ends, before the relay's
	// own close.
	testcleanup.Add(t, func() {
		left := map[string]bool{}
		for _, statement := range relay.heldStatements() {
			left[strings.TrimPrefix(statement, "drop database ")] = true
		}
		if match := teardownEndTemplate.FindStringSubmatch(output.String()); match != nil {
			left[match[1]] = true
		}
		for name := range left {
			dropDatabase(name)
		}
	})

	bound := teardownEndChildTimeout + testdb.TeardownGrace
	giveUp := bound + testwait.Margin
	ctx, cancel := context.WithTimeout(testwait.Context(t), giveUp)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTheTeardownEndsAtItsBoundChild$", "-test.timeout="+teardownEndChildTimeout.String(), "-test.v")
	child.Env = append(os.Environ(), DatabaseURLEnv+"="+server.String(), teardownEndChildEnv+"=1")
	child.Stdout, child.Stderr = &output, &output
	began := time.Now()
	err := child.Run()
	took := time.Since(began)
	for _, hold := range relay.holds() {
		t.Logf("the relay held %s", hold)
	}
	if ctx.Err() != nil {
		t.Fatalf("the child was still running %s after it began, %s past its teardown's bound, so it was killed: Run waits for a drop the server never answers with no bound:\n%s", took.Round(time.Second), (took - bound).Round(time.Second), &output)
	}
	if err != nil {
		t.Fatalf("the child exited %v after %s: Run did not end at its bound with its tests' code:\n%s", err, took.Round(time.Second), &output)
	}
	if strings.Contains(output.String(), "--- SKIP: TestTheTeardownEndsAtItsBoundChild (") {
		t.Skipf("the child skipped, so this machine was too slow for the row to say anything:\n%s", &output)
	}
	if !strings.Contains(output.String(), "--- PASS: TestTheTeardownEndsAtItsBoundChild (") {
		t.Fatalf("the child's row did not pass, so it made no database to drop:\n%s", &output)
	}
	var clones, templateDrops []string
	for _, statement := range relay.heldStatements() {
		switch name := strings.TrimPrefix(statement, "drop database "); {
		case strings.HasPrefix(name, "dispatch_test_"):
			clones = append(clones, name)
		case strings.HasPrefix(name, "dispatch_template_"):
			templateDrops = append(templateDrops, name)
		}
	}
	if len(clones) == 0 {
		t.Fatalf("the relay held no clone's drop, so nothing kept the child's teardown waiting and this run proves nothing:\n%s", &output)
	}
	if len(templateDrops) == 0 {
		t.Fatalf("the relay held no template's drop: Run sent none by its bound, so the template stays on the server, named by no line:\n%s", &output)
	}
	named := append(clones, templateDrops...)
	for _, name := range named {
		if line := "storetest: drop database " + name + ": still waiting at the teardown's bound"; !strings.Contains(output.String(), line) {
			t.Fatalf("the child ended without the line %q, so its teardown did not end at its bound naming that database:\n%s", line, &output)
		}
	}
	if took < bound {
		t.Fatalf("the child ended %s after it began, before its teardown's bound %s in: nothing held its drops, so this run proves nothing:\n%s", took.Round(time.Millisecond), bound, &output)
	}
	t.Logf("the child ended %s after it began, at its teardown's bound (%s in), naming %s", took.Round(time.Second), bound, strings.Join(named, " and "))
}

// TestTheTeardownEndsAtItsBoundChild is the child of
// TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered. It runs only when the parent re-executes
// this binary, with DISPATCH_TEST_DATABASE_URL naming the parent's relay.
func TestTheTeardownEndsAtItsBoundChild(t *testing.T) {
	if os.Getenv(teardownEndChildEnv) == "" {
		t.Skip(teardownEndChildEnv + " is not set: this test is the child of TestTheTeardownEndsAtItsBoundWhileADropIsUnanswered")
	}
	ready(t)
	end, bounded := testdb.ConnectEnd(t)
	if !bounded {
		t.Fatal("the child runs with no -test.timeout, so its teardown has no bound")
	}
	const key = "teardown-end"
	failure := fatalOf(t, func(t testing.TB) { OpenFrom(t, key, func(string) error { return nil }) })
	templates.Lock()
	if entry := templates.byKey[key]; entry != nil && entry.name != "" {
		t.Logf("the child made the template %s", entry.name)
	}
	templates.Unlock()
	if failure != "" {
		if past := time.Since(end); past >= 0 {
			t.Skipf("the child's OpenFrom failed %s past its connect end, too slow for the row to say anything: %s", past.Round(time.Millisecond), failure)
		}
		t.Fatalf("the child's OpenFrom, before its connect end, failed: %s", failure)
	}
}

// plainServerAddress is the host and port the relay dials for server, or why the row cannot use
// it: a server reached by TLS, whose messages the relay cannot read, or one the URL does not name
// by its host and port alone.
func plainServerAddress(server url.URL) (string, string) {
	query := server.Query()
	if query.Get("sslmode") != "disable" {
		return "", DatabaseURLEnv + " does not say sslmode=disable, and the relay reads the protocol's messages, which TLS hides"
	}
	if server.Hostname() == "" || query.Has("host") || query.Has("hostaddr") || query.Has("port") {
		return "", DatabaseURLEnv + " does not name its server by the URL's host and port alone"
	}
	port := server.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(server.Hostname(), port), ""
}

// dropRelay is a TCP relay between a child's admin pool and the Postgres server. It passes every
// byte the server sends, and every message a client sends but one: a simple-protocol query (a Q
// message) whose statement begins "drop database". That message it records and never sends on,
// and it sends the server nothing more of that connection, which stays open both ways. So a drop's
// login is answered, and its statement never reaches the server.
type dropRelay struct {
	listener net.Listener
	mu       sync.Mutex
	// statements are the statements the relay held, and at when it held each.
	statements []string
	at         []time.Time
	// conns are the connections the relay made or took, each closed at close; closed says close
	// has run.
	conns  []net.Conn
	closed bool
}

// holdDrops starts a dropRelay to upstream on a loopback listener (testport.ListenLoopback), and
// closes it and every connection it made when t ends.
func holdDrops(t *testing.T, upstream string) *dropRelay {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the relay: %v", err)
	}
	relay := &dropRelay{listener: listener}
	go relay.serve(upstream)
	testcleanup.Add(t, relay.close)
	return relay
}

func (r *dropRelay) address() string { return r.listener.Addr().String() }

// holds says each statement the relay held and when, in the order it held them.
func (r *dropRelay) holds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	holds := make([]string, len(r.statements))
	for i, statement := range r.statements {
		holds[i] = fmt.Sprintf("%q from %s", statement, r.at[i].UTC().Format(time.RFC3339Nano))
	}
	return holds
}

// heldStatements is each statement the relay held, in the order it held them.
func (r *dropRelay) heldStatements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.statements)
}

// track keeps conn for close, and answers false, closing conn, once the relay is closed.
func (r *dropRelay) track(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		_ = conn.Close()
		return false
	}
	r.conns = append(r.conns, conn)
	return true
}

func (r *dropRelay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	_ = r.listener.Close()
	for _, conn := range r.conns {
		_ = conn.Close()
	}
}

func (r *dropRelay) serve(upstream string) {
	for {
		client, err := r.listener.Accept()
		if err != nil {
			return
		}
		go r.relay(client, upstream)
	}
}

// relay carries one client connection to upstream: the server's bytes back as they come, and the
// client's messages on (pass) until it sends a drop.
func (r *dropRelay) relay(client net.Conn, upstream string) {
	if !r.track(client) {
		return
	}
	server, err := net.DialTimeout("tcp", upstream, 10*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	if !r.track(server) {
		return
	}
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	if !r.pass(bufio.NewReader(client), server) {
		_ = server.Close()
	}
}

// pass sends the server each message the client sends: the startup message, which has no type
// byte, then typed messages (a type byte and a length that counts itself). It answers true, leaving
// both connections open, once it has held a drop, and false once the client's side has ended.
func (r *dropRelay) pass(client *bufio.Reader, server net.Conn) bool {
	length, startup, err := readMessage(client)
	if err != nil {
		return false
	}
	if _, err := server.Write(append(length[:], startup...)); err != nil {
		return false
	}
	for {
		kind, err := client.ReadByte()
		if err != nil {
			return false
		}
		length, body, err := readMessage(client)
		if err != nil {
			return false
		}
		if statement := string(bytes.TrimRight(body, "\x00")); kind == 'Q' && strings.HasPrefix(statement, "drop database ") {
			r.mu.Lock()
			r.statements = append(r.statements, statement)
			r.at = append(r.at, time.Now())
			r.mu.Unlock()
			return true
		}
		if _, err := server.Write(append(append([]byte{kind}, length[:]...), body...)); err != nil {
			return false
		}
	}
}

// readMessage reads a message's length, which counts itself, and the body that follows it.
func readMessage(client *bufio.Reader) ([4]byte, []byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(client, length[:]); err != nil {
		return length, nil, err
	}
	size := binary.BigEndian.Uint32(length[:])
	if size < 4 {
		return length, nil, fmt.Errorf("a message length of %d, shorter than the length itself", size)
	}
	body := make([]byte, size-4)
	_, err := io.ReadFull(client, body)
	return length, body, err
}
