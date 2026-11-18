package bus

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The other half of the flap TestAFlapRunsTheSecondDisconnectHookAtOnce makes: the second outage
// lands while a reconnect hook of the run the first reconnect began waits for its request's
// answer. The hooks ran on nats.go's one callback goroutine, and so does every disconnect callback
// (AddDisconnectHook, the KV caches' among them), so an ensure that waited for its stream
// request's answer held the second disconnect's callback for the request's whole wait (the
// JetStream context's MaxWait, 10 s) while the connection was already down. The run is now off
// that goroutine (bus.Client.afterReconnect).
//
// Seen on the real surface at 3be3907a48: an Envoy listener behind a proxy that dropped its "envoy"
// connection the moment a reconnect completed logged that second disconnect 10 s after the drop,
// beside "envoy nats reconnect hook failed … stall_ms=10004" and "stream request unanswered …
// STREAM.INFO … context deadline exceeded" (in 1 of 3 flaps; in the other 2 the drop beat the
// version read, the case the other test pins).
//
// No clock decides the order here. A reconnect hook of the test's own, added after the client's
// ensure, stands in for that ensure's request: it asks a responder that never answers
// (silentResponder), on no context of the run's, so only the connection's close at the test's end
// ends its wait, as only its own bound ended the ensure's. Once that request has reached the
// responder, the test cuts the client's connection at a proxy in front of the server (flapProxy),
// and the second disconnect's hook must run while the request is still unanswered, as it always
// is. A client that ran its reconnect hooks on nats.go's callback goroutine never runs that hook,
// and the wait for it gives up naming it.
func TestAFlapDuringTheEnsuresRequestRunsTheSecondDisconnectHookAtOnce(t *testing.T) {
	_, uri := testnats.Start(t)
	asked := silentResponder(t, uri)
	proxy := newFlapProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := ConnectOwningStream([]string{"nats://" + proxy.addr()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	disconnects := make(chan struct{}, 8)
	client.AddDisconnectHook(func() { disconnects <- struct{}{} })
	client.AddReconnectHook(func(ctx context.Context, conn *nats.Conn) error {
		// Not under the run's context: the answer is waited for as a request that waits out a bound
		// of its own waits, and the connection's close is what ends the wait.
		_, err := conn.RequestWithContext(context.WithoutCancel(ctx), unansweredSubject, nil)
		return err
	})

	proxy.cut()
	testwait.Receive(t, "the first disconnect hook once the first outage began", disconnects)
	testwait.Receive(t, "the reconnect's run of the hooks to ask the responder that never answers", asked)
	proxy.cut()
	testwait.Receive(t, "the second disconnect hook while a reconnect hook still waits for its request's answer", disconnects)
}

// unansweredSubject is the subject a stand-in reconnect hook asks on, which silentResponder takes
// and never answers.
const unansweredSubject = "flap.unanswered"

// silentResponder subscribes to unansweredSubject on a connection of its own, straight to uri's
// server, and answers nothing: a request there has a subscriber, so the server reports no lack of
// responders, and its asker waits for an answer that never comes. Each request it is sent arrives
// on the channel it returns, which is what tells a test that the request is on the wire,
// unanswered.
func silentResponder(t *testing.T, uri string) <-chan *nats.Msg {
	t.Helper()
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	asked := make(chan *nats.Msg, 8)
	if _, err := conn.ChanSubscribe(unansweredSubject, asked); err != nil {
		t.Fatalf("subscribe the silent responder: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the silent responder's subscription: %v", err)
	}
	return asked
}

// flapProxy forwards every connection to upstream until cut closes it, as a link that drops does:
// the client reconnects through it, and the connection that brings it back is forwarded too.
type flapProxy struct {
	ln       net.Listener
	upstream string

	mu   sync.Mutex
	open map[net.Conn]net.Conn
}

func newFlapProxy(t *testing.T, upstream string) *flapProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &flapProxy{ln: ln, upstream: upstream, open: map[net.Conn]net.Conn{}}
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut()
	})
	go p.accept()
	return p
}

func (p *flapProxy) addr() string { return p.ln.Addr().String() }

// cut closes every connection the proxy forwards, both of its sides.
func (p *flapProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c, u := range p.open {
		_ = c.Close()
		_ = u.Close()
		delete(p.open, c)
	}
}

func (p *flapProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		u, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.open[c] = u
		p.mu.Unlock()
		go func() { _, _ = io.Copy(c, u); _ = c.Close() }()
		go func() { _, _ = io.Copy(u, c); _ = u.Close() }()
	}
}
