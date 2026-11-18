package bus_test

import (
	"bytes"
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// relayProxy relays NATS connections to a server, so a test can act on the link between a client
// and the server. With a hold set it keeps each new connection until the test lets it go, which is
// how a slow dial looks, and with refuse set it closes the connection instead of dialling the
// server, which is how a dial to a NATS that is gone looks. Once armed, it forwards nothing more
// the client sends on the connections it relays at that moment, as a failing link loses a request
// in flight; drop closes every connection it relays, so nats.go reconnects in place, through a
// relay that forwards again.
type relayProxy struct {
	listener net.Listener
	target   string
	relayed  string
	// hold, when not nil, is what each new connection waits on before the proxy dials or refuses
	// it: a dial in flight for as long as the test keeps it (holdDials).
	hold   atomic.Pointer[chan struct{}]
	refuse atomic.Bool
	// held receives once for each connection the hold keeps.
	held chan struct{}
	// swallowed receives once what an armed connection held back carries the trigger arm named.
	swallowed chan struct{}

	mu      sync.Mutex
	trigger []byte
	links   []*relayLink
}

// relayLink is one relayed connection; server is nil until the proxy has dialled it.
type relayLink struct {
	client, server net.Conn
	swallow        atomic.Bool
}

// startRelayProxy relays to the server uri names, from loopback. Its url is uri pointed at the
// proxy.
func startRelayProxy(t *testing.T, uri string) *relayProxy {
	t.Helper()
	return startRelayProxyFrom(t, "127.0.0.1", uri)
}

// startRelayProxyFrom relays to the server uri names from a port it holds on host, so a test that
// needs the server reached by an address of this machine other than loopback names that address.
// Its url is uri pointed at the proxy, userinfo and all.
func startRelayProxyFrom(t *testing.T, host, uri string) *relayProxy {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %q: %v", uri, err)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("proxy listen on %s: %v", host, err)
	}
	relayed := *server
	relayed.Host = listener.Addr().String()
	p := &relayProxy{
		listener:  listener,
		target:    server.Host,
		relayed:   relayed.String(),
		held:      make(chan struct{}, 16),
		swallowed: make(chan struct{}, 1),
	}
	t.Cleanup(p.close)
	go p.serve()
	return p
}

func (p *relayProxy) url() string { return p.relayed }

func (p *relayProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.relay(client)
	}
}

func (p *relayProxy) relay(client net.Conn) {
	link := &relayLink{client: client}
	p.mu.Lock()
	p.links = append(p.links, link)
	p.mu.Unlock()
	if hold := p.hold.Load(); hold != nil {
		p.held <- struct{}{}
		<-*hold
	}
	if p.refuse.Load() {
		_ = client.Close()
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	link.server = server
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	p.forward(link)
}

// forward copies what the client sends to the server until either side ends, holding it back
// while the link swallows.
func (p *relayProxy) forward(link *relayLink) {
	defer func() { _ = link.server.Close() }()
	buf := make([]byte, 32*1024)
	var held []byte
	for {
		n, err := link.client.Read(buf)
		if n > 0 {
			if link.swallow.Load() {
				held = append(held, buf[:n]...)
				if p.carriesTrigger(held) {
					select {
					case p.swallowed <- struct{}{}:
					default:
					}
				}
			} else if _, err := link.server.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *relayProxy) carriesTrigger(held []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.trigger) > 0 && bytes.Contains(held, p.trigger)
}

// arm swallows what the client sends on every connection relayed at this moment, and reports on
// swallowed once what one of them held back carries trigger.
func (p *relayProxy) arm(trigger string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trigger = []byte(trigger)
	for _, link := range p.links {
		link.swallow.Store(true)
	}
}

func (p *relayProxy) drop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, link := range p.links {
		_ = link.client.Close()
		if link.server != nil {
			_ = link.server.Close()
		}
	}
	p.links = nil
}

func (p *relayProxy) close() {
	_ = p.listener.Close()
	p.drop()
}

// holdDials keeps every connection the proxy relays from here on until the test lets it go, which
// it does as the test ends when it has not already; a dial held this way is one in flight. Each
// held connection is reported on held.
func (p *relayProxy) holdDials(t *testing.T) (letGo func()) {
	t.Helper()
	gate := make(chan struct{})
	p.hold.Store(&gate)
	letGo = sync.OnceFunc(func() {
		p.hold.Store(nil)
		close(gate)
	})
	t.Cleanup(letGo)
	return letGo
}
