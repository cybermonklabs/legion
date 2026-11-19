package testnats

import (
	"bytes"
	"context"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
)

// Proxy relays a client's TCP connections to a NATS server, so a test can take one client off
// NATS while every other client keeps its own connection: Cut drops it, as a network that ends a
// connection does, and Freeze silences it, as a network that drops packets does, and Restore ends
// either. A Proxy started with ProxyLatency adds a round trip's delay to everything it relays, as
// the route to a remote NATS hub does, and one started with ProxyRate moves no faster than its
// rate, as a slow link does. Both run on the proxy's clock (ProxyClock).
type Proxy struct {
	listener net.Listener
	target   string
	url      string
	delay    time.Duration
	// clock is the clock the delay and the rate run on: clock.Real for a test that takes the
	// latency as it comes, and a clocktest.Fake for one that asserts what the proxy holds back.
	clock   clock.Clock
	ctx     context.Context
	stop    context.CancelFunc
	closing sync.Once
	// rate is the bytes a second each direction of each connection reads at, 0 for no limit.
	rate int

	mu sync.Mutex
	// conns are both ends of every connection the proxy is relaying.
	conns map[net.Conn]struct{}
	// cut is set from Cut until Restore: every connection is closed as it is accepted.
	cut bool
	// gate is nil while the proxy moves bytes, and open from Freeze until Restore: every read
	// and every write of a relayed connection waits for it to close.
	gate chan struct{}
}

// ProxyOption configures a Proxy.
type ProxyOption func(*Proxy)

// ProxyLatency holds every chunk read from either side for half of rtt before writing it on, in
// the order it was read, so a connection through the proxy sees rtt added to each round trip and
// nothing else. The holding runs on the proxy's clock (ProxyClock).
func ProxyLatency(rtt time.Duration) ProxyOption {
	return func(p *Proxy) { p.delay = rtt / 2 }
}

// ProxyClock is the clock the proxy's latency and rate run on, clock.Real by default: a test that
// asserts what a round trip through the proxy costs hands it a clocktest.Fake, whose armed
// deadlines are the chunks the proxy is holding and which moves only when the test moves it.
func ProxyClock(c clock.Clock) ProxyOption {
	return func(p *Proxy) { p.clock = c }
}

// ProxyRate reads each direction of each connection at about bytesPerSecond, so a sender's socket
// backs up as it does behind a slow link: the throttle is on the read, so the proxy's queue never
// holds more than one read ahead of the rate, and the sender, not the proxy, buffers the rest.
func ProxyRate(bytesPerSecond int) ProxyOption {
	return func(p *Proxy) { p.rate = bytesPerSecond }
}

// StartProxy relays to the NATS server at target, a client URL such as Start returns, on a
// loopback port of its own, until the test ends. URL is the same server through the proxy.
func StartProxy(t testing.TB, target string, options ...ProxyOption) *Proxy {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the proxy relays to: %v", target, err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the NATS proxy: %v", err)
	}
	proxied := *server
	proxied.Host = listener.Addr().String()
	ctx, stop := context.WithCancel(context.Background())
	p := &Proxy{
		listener: listener,
		target:   server.Host,
		url:      proxied.String(),
		clock:    clock.Real(),
		ctx:      ctx,
		stop:     stop,
		conns:    map[net.Conn]struct{}{},
	}
	for _, option := range options {
		option(p)
	}
	go p.serve()
	t.Cleanup(p.close)
	return p
}

// URL is the NATS URL of the proxy's address: the target's, credentials included, with the host
// and port the proxy listens on.
func (p *Proxy) URL() string { return p.url }

// Cut closes every connection the proxy is relaying and every one it accepts until Restore, as a
// dial the network refuses ends: a client through it is disconnected at once, and each of its
// reconnects fails.
func (p *Proxy) Cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for conn := range p.conns {
		_ = conn.Close()
	}
	clear(p.conns)
}

// Freeze stops the proxy moving bytes in either direction until Restore, and keeps every
// connection open: a client through it stays connected while nothing it sends reaches the server
// and nothing the server sends reaches it, as on a network that silently drops packets. What either
// side sent meanwhile is delivered once the proxy is restored, as TCP retransmits it.
func (p *Proxy) Freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.gate == nil {
		p.gate = make(chan struct{})
	}
}

// Restore ends a Cut and a Freeze: the proxy accepts and relays connections again, and the bytes a
// freeze held move on.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
	if p.gate != nil {
		close(p.gate)
		p.gate = nil
	}
}

// serve relays each connection the proxy accepts until it closes.
func (p *Proxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.join(client)
	}
}

// join dials the server for client and relays the two into each other, or closes client at once
// while the proxy is cut or when the dial fails.
func (p *Proxy) join(client net.Conn) {
	if !p.track(client) {
		return
	}
	server, err := net.Dial("tcp", p.target)
	if err != nil || !p.track(server) {
		if err == nil {
			_ = server.Close()
		}
		p.untrack(client)
		return
	}
	go p.relay(server, client)
	p.relay(client, server)
}

// track records conn as relayed and reports true, or closes it and reports false while the proxy
// is cut or closed.
func (p *Proxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.ctx.Done():
		_ = conn.Close()
		return false
	default:
	}
	if p.cut {
		_ = conn.Close()
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}

func (p *Proxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
		delete(p.conns, conn)
	}
}

// proxyReadBuffer is the most one read of a relayed connection takes. With ProxyRate the reader
// forwards each read before it waits for it, so a direction can run up to proxyReadBuffer bytes
// ahead of its rate.
const proxyReadBuffer = 64 << 10

// relay copies src to dst, each chunk written the proxy's delay after it was read and each read
// held to the proxy's rate, both on the proxy's clock, until either ends; then it closes both,
// which ends the other direction too.
func (p *Proxy) relay(dst, src net.Conn) {
	defer p.untrack(dst, src)
	type chunk struct {
		data []byte
		due  time.Time
	}
	queue := make(chan chunk, 4096)
	go func() {
		defer close(queue)
		buffer := make([]byte, proxyReadBuffer)
		for {
			p.pass()
			read, err := src.Read(buffer)
			if read > 0 {
				queue <- chunk{data: bytes.Clone(buffer[:read]), due: p.clock.Now().Add(p.delay)}
				if p.rate > 0 {
					if err := p.clock.Sleep(p.ctx, time.Duration(read)*time.Second/time.Duration(p.rate)); err != nil {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		// A proxy with no latency arms no timer: its chunks are due as they are read.
		if p.delay > 0 {
			if err := p.clock.Sleep(p.ctx, c.due.Sub(p.clock.Now())); err != nil {
				break
			}
		}
		p.pass()
		if _, err := dst.Write(c.data); err != nil {
			// The reader ends once src is closed below.
			break
		}
	}
	_ = src.Close()
	for range queue {
	}
}

// pass waits while the proxy is frozen, and returns at once while it is not or once it closes.
func (p *Proxy) pass() {
	p.mu.Lock()
	gate := p.gate
	p.mu.Unlock()
	if gate == nil {
		return
	}
	select {
	case <-gate:
	case <-p.ctx.Done():
	}
}

// close stops listening, closes every relayed connection and releases every copy a freeze holds.
func (p *Proxy) close() {
	p.closing.Do(func() {
		p.stop()
		_ = p.listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		clear(p.conns)
	})
}
