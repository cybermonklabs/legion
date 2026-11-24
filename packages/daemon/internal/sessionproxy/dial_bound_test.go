package sessionproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// dialEntry is what stallAfterProofDialer saw of a dial it never answers: when it was entered, and
// the deadline of its context, if it has one.
type dialEntry struct {
	entered, deadline time.Time
	bounded           bool
}

// stallAfterProofDialer stands in for the Cloud SQL connector whose first dial, Start's proof,
// reaches the test's upstream, and whose every later dial is never answered, as when its
// certificate refresh waits on an Admin API call that never answers: each later dial reports when
// it was entered and its context's deadline, then ends only when that context does.
type stallAfterProofDialer struct {
	addr    string
	entered chan dialEntry

	mu    sync.Mutex
	dials int
}

func (s *stallAfterProofDialer) Dial(ctx context.Context, _ string) (net.Conn, error) {
	s.mu.Lock()
	s.dials++
	proof := s.dials == 1
	s.mu.Unlock()
	if proof {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", s.addr)
	}
	deadline, bounded := ctx.Deadline()
	s.entered <- dialEntry{entered: time.Now(), deadline: deadline, bounded: bounded}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *stallAfterProofDialer) Close() error { return nil }

func (s *stallAfterProofDialer) build(context.Context, Signer) (Dialer, error) { return s, nil }

// A connection's dial is bounded as Start's proof dial is, by DialTimeout: the connector refreshes
// its certificate inside a dial while it holds its cache (lazy refresh), so a dial with no bound
// whose refresh got no answer would hold that connection, and every dial queued behind it, for as
// long as the proxy serves. The bound is read from the stalled dial itself, its context's deadline
// against the time it was entered, never from a clock around the connection. Once the bound ends
// the dial, the proxy closes the connection with nothing written back and logs one line naming the
// instance and the bound.
func TestAConnectionsDialEndsAtItsBound(t *testing.T) {
	up := newUpstream(t)
	dialer := &stallAfterProofDialer{addr: up.listener.Addr().String(), entered: make(chan dialEntry, 1)}
	socket := socketPath(t)
	var logged strings.Builder
	proxy := startConfigured(context.Background(), t, up, Config{Socket: socket, NewDialer: dialer.build, Log: &logged})
	defer func() { _ = proxy.Close() }()

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	var entry dialEntry
	select {
	case entry = <-dialer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy did not dial the upstream for the connection")
	}
	if !entry.bounded {
		t.Fatal("the connection's dial has no deadline: a dial whose certificate refresh never answers holds the connection, and the connector's cache, for as long as the proxy serves")
	}
	if span := entry.deadline.Sub(entry.entered); span > testDialTimeout {
		t.Fatalf("the connection's dial ends %s after it was entered, want within the dial bound %s", span, testDialTimeout)
	}

	// The bound ends the dial, and the proxy closes the connection. The read's own deadline is only
	// the test's guard, well past the bound.
	if err := client.SetReadDeadline(time.Now().Add(testDialTimeout + time.Minute)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	buf := make([]byte, 64)
	n, err := client.Read(buf)
	if n != 0 {
		t.Fatalf("the proxy wrote %q to a connection whose dial got no answer", buf[:n])
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the client's read: %v, want EOF once the dial's bound ended it", err)
	}
	// Close returns once every connection's serve has ended, so the log is whole after it.
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := "sessionproxy: could not dial " + testInstance + " for a connection: no answer within " + testDialTimeout.String() + ": context deadline exceeded"
	if !strings.Contains(logged.String(), want) {
		t.Fatalf("the proxy logged %q, want the line %q", logged.String(), want)
	}
}
