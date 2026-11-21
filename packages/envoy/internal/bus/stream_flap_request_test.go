package bus

import (
	"bytes"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
)

// The other half of the flap TestAFlapRunsTheSecondDisconnectHookAtOnce makes: the second outage
// lands after the reconnect hook's ensure has read the server version, while its first stream
// request is on the wire and unanswered. The hooks ran on nats.go's one callback goroutine, and so
// does every disconnect callback (AddDisconnectHook, the KV caches' among them), so an ensure that
// waited for that request's answer held the second disconnect's callback for the request's whole
// wait (the JetStream context's MaxWait, 10 s) while the connection was already down. The run is
// now off that goroutine (bus.Client.onReconnect, which queueReconnect hands it to) and under a
// context its connection's loss ends (whileConnected).
//
// Seen on the real surface at 3be3907a48: an Envoy listener behind a proxy that dropped its "envoy"
// connection the moment a reconnect completed logged that second disconnect 10 s after the drop,
// beside "envoy nats reconnect hook failed … stall_ms=10004" and "stream request unanswered …
// STREAM.INFO … context deadline exceeded" (in 1 of 3 flaps; in the other 2 the drop beat the
// version read, the case the other test pins). This test makes that order exact with a proxy in
// front of the test server: after the first outage it lets the reconnect through and closes the
// connection when the client's first $JS.API.STREAM request reaches it, unanswered.
func TestAFlapDuringTheEnsuresRequestRunsTheSecondDisconnectHookAtOnce(t *testing.T) {
	_, uri := testnats.Start(t)
	proxy := newFlapProxy(t, strings.TrimPrefix(uri, "nats://"))
	client, err := ConnectOwningStream([]string{"nats://" + proxy.addr()}, WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	disconnects := make(chan time.Time, 8)
	client.AddDisconnectHook(func() { disconnects <- time.Now() })

	proxy.cutAndArm(time.Second)
	select {
	case <-disconnects:
	case <-time.After(10 * time.Second):
		t.Fatal("the first disconnect hook did not run within 10s of the first outage")
	}
	var dropped time.Time
	select {
	case dropped = <-proxy.dropped:
	case <-time.After(30 * time.Second):
		t.Fatal("no stream request reached the proxy within 30s of the first outage: the reconnect's ensure never asked")
	}
	select {
	case ran := <-disconnects:
		if late := ran.Sub(dropped); late > 2*time.Second {
			t.Fatalf("the second disconnect hook ran %s after the second outage began, want at once: the ensure hook held nats.go's callback goroutine on its unanswered stream request", late)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the second disconnect hook did not run within 20s of the second outage")
	}
}

// flapProxy forwards every connection to upstream. cutAndArm closes every open connection and
// refuses new ones for refuse; the first connection after that is forwarded until the client
// writes a $JS.API.STREAM request, which is not forwarded: both sides are closed there, the time is
// sent on dropped, and new connections are refused for three seconds.
type flapProxy struct {
	t        *testing.T
	ln       net.Listener
	upstream string
	dropped  chan time.Time

	mu          sync.Mutex
	open        map[net.Conn]net.Conn
	refuseUntil time.Time
	armed       bool
}

func newFlapProxy(t *testing.T, upstream string) *flapProxy {
	t.Helper()
	ln, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &flapProxy{t: t, ln: ln, upstream: upstream, dropped: make(chan time.Time, 1), open: map[net.Conn]net.Conn{}}
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for c, u := range p.open {
			_ = c.Close()
			_ = u.Close()
		}
	})
	go p.accept()
	return p
}

func (p *flapProxy) addr() string { return p.ln.Addr().String() }

func (p *flapProxy) cutAndArm(refuse time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c, u := range p.open {
		_ = c.Close()
		_ = u.Close()
		delete(p.open, c)
	}
	p.refuseUntil = time.Now().Add(refuse)
	p.armed = true
}

func (p *flapProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refused := time.Now().Before(p.refuseUntil)
		p.mu.Unlock()
		if refused {
			_ = c.Close()
			continue
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
		go p.forward(c, u)
	}
}

func (p *flapProxy) forward(c, u net.Conn) {
	defer func() {
		_ = c.Close()
		_ = u.Close()
		p.mu.Lock()
		delete(p.open, c)
		p.mu.Unlock()
	}()
	buf := make([]byte, 64*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			p.mu.Lock()
			drop := p.armed && (bytes.Contains(chunk, []byte("PUB $JS.API.STREAM.")) || bytes.Contains(chunk, []byte("HPUB $JS.API.STREAM.")))
			if drop {
				p.armed = false
				p.refuseUntil = time.Now().Add(3 * time.Second)
			}
			p.mu.Unlock()
			if drop {
				p.dropped <- time.Now()
				return
			}
			if _, werr := u.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
