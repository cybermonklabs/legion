package bus

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A KV watcher's ordered consumer reports nats.ErrConsumerNotActive only while its connection is
// not connected - connected, nats.go resets the consumer instead - and every consumer Envoy runs
// with idle heartbeats is such a watcher. The report restates a disconnect the bus has already
// logged, once per watcher: six ERROR lines in one NATS gap of a deployment pin rehearsal. It is a
// WARN. Every other async error stays an ERROR (LEGION-278), "consumer not found" among them,
// bare or wrapped: a shutdown ends each KV watcher and deletes its consumer itself, before the
// connection's drain (kvwatch.Watcher.End), so a delete nats.go's drain reports belongs to a
// watcher nothing ended, and an ordered consumer nats.go failed to recreate wraps the error.
func TestAConsumerNotActiveReportIsAWarning(t *testing.T) {
	logs := captureLogs(t)

	report := options("async-error-test", nil, nil, nil, nil, nil, nil).AsyncErrorCB
	report(nil, &nats.Subscription{Subject: "_INBOX.watcher"}, nats.ErrConsumerNotActive)
	report(nil, &nats.Subscription{Subject: "_INBOX.other"}, nats.ErrSlowConsumer)
	report(nil, &nats.Subscription{Subject: "_INBOX.recreate"}, fmt.Errorf("%w: recreating ordered consumer", nats.ErrConsumerNotFound))
	report(nil, &nats.Subscription{Subject: "_INBOX.drained"}, nats.ErrConsumerNotFound)

	// Only this client's records: a client an earlier test closed can still log through the same
	// default.
	var levels []string
	for _, record := range logs.records(t) {
		if record["name"] == "async-error-test" {
			levels = append(levels, fmt.Sprint(record["subject"], "=", record["level"]))
		}
	}
	if got, want := strings.Join(levels, " "), "_INBOX.watcher=WARN _INBOX.other=ERROR _INBOX.recreate=ERROR _INBOX.drained=ERROR"; got != want {
		t.Fatalf("async error levels = %s, want %s", got, want)
	}
}

// A server that closes or resets the connection during a reconnect's handshake - one restarting,
// as in TestListenerFollowsTheInterestRegistryAcrossNATSRestarts - makes nats.go report the read's
// error (io.EOF, or "connection reset by peer") to the async error callback with no subscription,
// and to nothing else. That is a failed reconnect attempt, logged as one: a WARN on the reconnect
// throttle, never an ERROR, which an operator's alert reads as a fault. The server here is a fake
// that completes the first handshake, drops that connection, and closes or resets every later one
// after reading its CONNECT, so the client's real reconnect loop reports exactly that read's
// error. Every other async error stays an ERROR: the same EOF reported for a connected
// connection, and a slow consumer.
func TestAHandshakeTheServerClosedIsAFailedReconnectAttempt(t *testing.T) {
	for _, row := range []struct {
		name string
		// end ends a reconnect's connection once its CONNECT is read.
		end func(net.Conn)
		// read is what the client's failed read reports.
		read string
	}{
		{name: "closed", end: func(conn net.Conn) { _ = conn.Close() }, read: "EOF"},
		{name: "reset", end: func(conn net.Conn) {
			// No linger: the close sends a reset rather than a FIN.
			_ = conn.(*net.TCPConn).SetLinger(0)
			_ = conn.Close()
		}, read: "connection reset by peer"},
	} {
		t.Run(row.name, func(t *testing.T) {
			logs := captureLogs(t)
			listener, err := testport.ListenLoopback()
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			connected := make(chan net.Conn, 1)
			go func() {
				for first := true; ; first = false {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					_, _ = io.WriteString(conn, `INFO {"server_id":"fake","version":"2.10.29","proto":1,"max_payload":1048576}`+"\r\n")
					reader := bufio.NewReader(conn)
					// The CONNECT and the PING are both read before the connection ends: a close
					// that left the PING unread would reach the client as a reset, not an EOF.
					if _, err := reader.ReadString('\n'); err != nil { // CONNECT
						_ = conn.Close()
						continue
					}
					if _, err := reader.ReadString('\n'); err != nil { // PING
						_ = conn.Close()
						continue
					}
					if !first {
						row.end(conn) // the handshake's PING is never answered
						continue
					}
					_, _ = io.WriteString(conn, "PONG\r\n")
					connected <- conn
				}
			}()

			name := "handshake-" + row.name + "-test"
			url := "nats://" + listener.Addr().String()
			opts := options(name, []string{url}, nil, nil, nil, nil, nil)
			opts.ReconnectWait = 50 * time.Millisecond
			nc, err := opts.Connect()
			if err != nil {
				t.Fatalf("connect to the fake server: %v", err)
			}
			t.Cleanup(nc.Close)
			_ = (<-connected).Close()

			lines := func() []string {
				var out []string
				for _, record := range logs.records(t) {
					// The disconnect's own INFO line carries the EOF that ended the first
					// connection.
					text, _ := record["error"].(string)
					if record["name"] == name && strings.Contains(text, row.read) && record["msg"] != "envoy nats disconnected" {
						out = append(out, fmt.Sprint(record["level"], " ", record["msg"]))
					}
				}
				return out
			}
			reported := func() bool { return len(lines()) > 0 }
			if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 20*time.Millisecond, reported) {
				t.Fatalf("the client never reported the handshake the server %s", row.name)
			}
			if got, want := lines()[0], "WARN envoy nats reconnect attempt failed"; got != want {
				t.Fatalf("a reconnect handshake the server %s logged %q, want %q", row.name, got, want)
			}
			for _, line := range lines() {
				if strings.HasPrefix(line, "ERROR") {
					t.Fatalf("a reconnect handshake the server %s logged %q", row.name, line)
				}
			}
		})
	}

	// The same EOF for a connection that is connected, and a slow consumer, stay ERRORs.
	other := captureLogs(t)
	report := options("other-async-error-test", nil, nil, nil, nil, nil, nil).AsyncErrorCB
	report(&nats.Conn{}, nil, io.EOF)
	report(nil, &nats.Subscription{Subject: "_INBOX.slow"}, nats.ErrSlowConsumer)
	var levels []string
	for _, record := range other.records(t) {
		if record["name"] == "other-async-error-test" {
			levels = append(levels, fmt.Sprint(record["level"], " ", record["msg"], " ", record["error"]))
		}
	}
	if got, want := strings.Join(levels, " | "), "ERROR envoy nats async error EOF | ERROR envoy nats async error nats: slow consumer, messages dropped"; got != want {
		t.Fatalf("other async errors logged %q, want %q", got, want)
	}
}

// handshakeReadFailed takes every way a real read of a connection fails, read from real sockets:
// the peer's close, the peer's reset and a read past its deadline. An error the handshake builds
// itself, as a credentials callback's, is none of them.
func TestAHandshakeReadFailsTheWaysARealReadFails(t *testing.T) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	readOf := func(t *testing.T, peer func(net.Conn), deadline time.Time) error {
		t.Helper()
		accepted := make(chan net.Conn, 1)
		go func() {
			conn, err := listener.Accept()
			if err == nil {
				accepted <- conn
			}
		}()
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		server := <-accepted
		t.Cleanup(func() { _ = server.Close() })
		peer(server)
		if !deadline.IsZero() {
			_ = conn.SetReadDeadline(deadline)
		}
		_, err = bufio.NewReader(conn).ReadString('\n')
		return err
	}
	closed := readOf(t, func(conn net.Conn) { _ = conn.Close() }, time.Time{})
	reset := readOf(t, func(conn net.Conn) {
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}, time.Time{})
	timedOut := readOf(t, func(net.Conn) {}, time.Now().Add(-time.Second))
	for _, row := range []struct {
		name string
		err  error
		want bool
	}{
		{"the peer closed", closed, true},
		{"the peer reset", reset, true},
		{"the read's deadline passed", timedOut, true},
		{"a credentials callback failed", errors.New("nkey seed: bad file"), false},
	} {
		if row.err == nil {
			t.Fatalf("%s: the read did not fail", row.name)
		}
		if got := handshakeReadFailed(row.err); got != row.want {
			t.Errorf("%s (%v): handshakeReadFailed = %t, want %t", row.name, row.err, got, row.want)
		}
	}
}

// Only the server's refusal of the client's core subscription, on its subject and in its queue
// group, reported by the connection the client holds, marks it refused (CoreSubOK), and so does a
// reload's revocation of its subject, which names a sid and no queue group. A refusal of a publish,
// of another subscription, of the same subject in another queue group or outside one, a revocation
// of another subject, and one a connection the client has replaced reports late, leave it as it
// is. The server quotes each name with %q, so a queue group holding a double quote is still its
// own. The errors are built as nats.go builds the one it hands the async error callback for a
// server's -ERR (processErr).
func TestOnlyTheServersRefusalOfTheCoreSubscriptionMarksItRefused(t *testing.T) {
	t.Parallel()
	violation := func(text string) error { return fmt.Errorf("%w: %s", nats.ErrPermissionViolation, text) }
	own := violation(`Permissions Violation for Subscription to "notifications.>" using queue "live"`)
	current := &nats.Conn{}
	for _, tc := range []struct {
		name  string
		queue string
		conn  *nats.Conn
		err   error
		want  bool
	}{
		{name: "its own refusal", conn: current, err: own, want: true},
		{name: "a publish refusal", conn: current, err: violation(`Permissions Violation for Publish to "notifications.>"`)},
		{name: "another subject", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.role.>" using queue "live"`)},
		{name: "another queue group", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.>" using queue "other"`)},
		{name: "no queue group", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.>"`)},
		{name: "a reload's revocation", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.>" (sid "3")`), want: true},
		{name: "a reload's revocation of another subject", conn: current, err: violation(`Permissions Violation for Subscription to "notifications.role.>" (sid "3")`)},
		{name: "a revocation from a replaced connection", conn: &nats.Conn{}, err: violation(`Permissions Violation for Subscription to "notifications.>" (sid "3")`)},
		{name: "its own refusal of a queue group holding a double quote", queue: `li"ve`, conn: current, err: violation(fmt.Sprintf("Permissions Violation for Subscription to %q using queue %q", "notifications.>", `li"ve`)), want: true},
		{name: "a replaced connection", conn: &nats.Conn{}, err: own},
		{name: "not a permissions violation", conn: current, err: errors.New(own.Error())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			queue := cmp.Or(tc.queue, "live")
			client := &Client{Conn: current}
			client.subscriptions[coreSubscription] = recoverableSubscription{
				transport: coreSubscription,
				subject:   "notifications.>",
				queue:     queue,
				handler:   func(*nats.Msg) {},
			}
			client.onAsyncError(tc.conn, tc.err)
			if got := client.subscriptions[coreSubscription].refused; got != tc.want {
				t.Fatalf("refused = %t after %v, want %t", got, tc.err, tc.want)
			}
		})
	}
}
