package sessionproxy

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/clock/clocktest"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// heldDialer is a dialer whose dials, once released, return a live connection whatever their
// context says: a connector dial that completed just as Close cancelled the serving context.
// Every dial after the first, which is Start's proof, is held until release is closed.
type heldDialer struct {
	fakeDialer
	held    chan struct{}
	release chan struct{}
	dialed  int
}

func (h *heldDialer) Dial(_ context.Context, instance string) (net.Conn, error) {
	h.mu.Lock()
	h.dialed++
	first := h.dialed == 1
	h.mu.Unlock()
	if !first {
		close(h.held)
		<-h.release
	}
	return h.fakeDialer.Dial(context.Background(), instance)
}

func (h *heldDialer) build(context.Context, string) (Dialer, error) { return h, nil }

// A connection whose upstream dial completes after Close has begun is closed at both ends at
// once and never copied for, so it cannot keep Close waiting until the drain is over, nor, had it
// arrived after the drain's force-close, escape that close and hold Close forever.
func TestALateDialDuringCloseIsClosedNotCopied(t *testing.T) {
	const drain = 5 * time.Second
	up := newUpstream(t)
	dialer := &heldDialer{fakeDialer: fakeDialer{addr: up.listener.Addr().String()}, held: make(chan struct{}), release: make(chan struct{})}
	socket := socketPath(t)
	proxy, err := Start(context.Background(), Config{
		Socket: socket, Instance: testInstance, ProofTimeout: testProofTimeout, Drain: drain, NewDialer: dialer.build,
		Clock: clocktest.New(testMoment),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = up.next(t).Close()

	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	defer client.Close()
	testwait.Receive(t, "the client's upstream dial", dialer.held)

	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()
	testwait.Eventually(t, "Close to mark the proxy closing", func() bool {
		proxy.connsMu.Lock()
		defer proxy.connsMu.Unlock()
		return proxy.closing
	})
	close(dialer.release)

	server := up.next(t)
	defer server.Close()
	expectClosed(t, server, "the upstream")
	expectClosed(t, client, "the client")
	// The proxy's fake clock never reaches the drain's end, so Close returns only because the late
	// connection was closed at once, not held until the drain was over.
	if err := testwait.Receive(t, "Close's return", closed); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
