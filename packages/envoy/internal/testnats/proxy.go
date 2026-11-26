package testnats

import (
	"bytes"
	"maps"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
)

// Proxy relays a client's TCP connections to a NATS server, so a test can take one client off
// NATS while every other client keeps its own connection: Cut drops it, as a network that ends a
// connection does, and Freeze silences it, as a network that drops packets does, and Restore ends
// either. A Proxy started with ProxyLatency adds a round trip's delay to everything it relays, as
// the route to a remote NATS hub does, and one started with ProxyRate moves no faster than its
// rate, as a slow link does.
type Proxy struct {
	listener net.Listener
	target   string
	url      string
	delay    time.Duration
	done     chan struct{}
	closing  sync.Once
	// rate is the bytes a second each direction of each connection reads at, 0 for no limit.
	rate int
	// replyDrop, when set by DropFirstReplyForRequests, drops one response to an API request whose
	// subject has its prefix. Each proxy connection records its own request inboxes, while this shared
	// counter makes the proxy drop one answer total.
	replyDrop *replyDrop

	mu sync.Mutex
	// conns are both ends of every connection the proxy is relaying.
	conns map[net.Conn]struct{}
	// cut is set from Cut until Restore: every connection is closed as it is accepted.
	cut bool
	// refusedWhileCut counts the connections closed as they were accepted because the proxy was
	// cut: each is one dial the network refused.
	refusedWhileCut int
	// gate is nil while the proxy moves bytes, and open from Freeze until Restore: every read
	// and every write of a relayed connection waits for it to close.
	gate chan struct{}
	// serverGate is nil while server replies move, and open from FreezeServerReplies until Restore.
	// It holds only server-to-client reads and writes.
	serverGate chan struct{}
	// atGate holds, by the client's address, each connection whose client-to-server reader waits at
	// a freeze's gate: that reader reads nothing more of its client until Restore (gatedReaders).
	atGate map[string]bool
}

// ProxyOption configures a Proxy.
type ProxyOption func(*Proxy)

// replyDrop is the one reply a proxy may discard, shared by every connection it relays. mu guards
// dropped and each connection's recorded reply inboxes (replyDropConnection.replies).
type replyDrop struct {
	prefix  []byte
	armed   <-chan struct{}
	mu      sync.Mutex
	dropped int
}

// DropFirstReplyForRequests drops the first response to a request whose subject starts with
// prefix, once armed is closed. Requests sent before that are not recorded, so a constructor's
// initial API reads cannot consume the drop intended for the operation under test. DroppedReplies
// reports whether it fired.
func DropFirstReplyForRequests(prefix string, armed <-chan struct{}) ProxyOption {
	return func(p *Proxy) {
		p.replyDrop = &replyDrop{prefix: []byte(prefix), armed: armed}
	}
}

// DroppedReplies reports how many replies DropFirstReplyForRequests discarded.
func (p *Proxy) DroppedReplies() int {
	if p.replyDrop == nil {
		return 0
	}
	p.replyDrop.mu.Lock()
	defer p.replyDrop.mu.Unlock()
	return p.replyDrop.dropped
}

// replyDropConnection is the protocol state one proxied NATS connection needs to arm and fire a
// reply drop. The two streams are touched by their one relay direction alone.
type replyDropConnection struct {
	drop     *replyDrop
	sent     protocolStream
	received protocolStream
	// replies are the reply inboxes of the matching requests this connection sent once the drop was
	// armed: the server answers each on this connection. drop.mu guards it, as the client-to-server
	// reader adds to it while the server-to-client reader reads it.
	replies map[string]struct{}
}

// recordRequest records the reply inbox of line, a client's complete control line, once the drop
// is armed, when line is a PUB or an HPUB with a reply inbox on a subject the drop's prefix starts.
func (c *replyDropConnection) recordRequest(line []byte) {
	select {
	case <-c.drop.armed:
	default:
		return
	}
	// PUB <subject> <reply-to> <#bytes> and HPUB <subject> <reply-to> <#header bytes> <#total bytes>
	// name a reply inbox; without one, each has a field fewer.
	fields := bytes.Fields(line)
	request := (len(fields) == 4 && string(fields[0]) == "PUB") || (len(fields) == 5 && string(fields[0]) == "HPUB")
	if !request || !bytes.HasPrefix(fields[1], c.drop.prefix) {
		return
	}
	c.drop.mu.Lock()
	c.replies[string(fields[2])] = struct{}{}
	c.drop.mu.Unlock()
}

// drops reports whether line, a server's complete control line, begins the one reply the proxy
// discards: the first MSG or HMSG on a reply inbox this connection recorded.
func (c *replyDropConnection) drops(line []byte) bool {
	fields := bytes.Fields(line)
	if len(fields) < 4 || (string(fields[0]) != "MSG" && string(fields[0]) != "HMSG") {
		return false
	}
	c.drop.mu.Lock()
	defer c.drop.mu.Unlock()
	if _, recorded := c.replies[string(fields[1])]; !recorded || c.drop.dropped > 0 {
		return false
	}
	c.drop.dropped++
	return true
}

// ProxyLatency holds every chunk read from either side for half of rtt before writing it on, in
// the order it was read, so a connection through the proxy sees rtt added to each round trip and
// nothing else.
func ProxyLatency(rtt time.Duration) ProxyOption {
	return func(p *Proxy) { p.delay = rtt / 2 }
}

// ProxyRate reads each direction of each connection at about bytesPerSecond, so a sender's socket
// backs up as it does behind a slow link: the throttle is on the read, so the proxy's queue never
// holds more than one read ahead of the rate, and the sender, not the proxy, buffers the rest.
func ProxyRate(bytesPerSecond int) ProxyOption {
	return func(p *Proxy) { p.rate = bytesPerSecond }
}

// StartProxy relays to the NATS server at target, a client URL such as Start returns, on a
// loopback port and address of its own (testport.LoopbackIP), until the test ends. URL is the same
// server through the proxy.
func StartProxy(t testing.TB, target string, options ...ProxyOption) *Proxy {
	t.Helper()
	server, err := url.Parse(target)
	if err != nil || server.Host == "" {
		t.Fatalf("read the NATS URL %q the proxy relays to: %v", target, err)
	}
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for the NATS proxy: %v", err)
	}
	proxied := *server
	proxied.Host = listener.Addr().String()
	p := &Proxy{
		listener: listener,
		target:   server.Host,
		url:      proxied.String(),
		done:     make(chan struct{}),
		conns:    map[net.Conn]struct{}{},
		atGate:   map[string]bool{},
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

// FreezeServerReplies stops server replies while client writes still reach the server. Restore
// releases the replies. A test uses it when it must prove a request stored before it loses the
// request's acknowledgement.
func (p *Proxy) FreezeServerReplies() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serverGate == nil {
		p.serverGate = make(chan struct{})
	}
}

// Restore ends a Cut, a Freeze and a FreezeServerReplies: the proxy accepts and relays connections
// again, the bytes a freeze held move on, and server replies resume.
func (p *Proxy) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
	if p.gate != nil {
		close(p.gate)
		p.gate = nil
	}
	if p.serverGate != nil {
		close(p.serverGate)
		p.serverGate = nil
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
	server, err := net.DialTimeout("tcp", p.target, dialTimeout)
	if err != nil || !p.track(server) {
		if err == nil {
			_ = server.Close()
		}
		p.untrack(client)
		return
	}
	var replies *replyDropConnection
	if p.replyDrop != nil {
		replies = &replyDropConnection{drop: p.replyDrop, received: protocolStream{fromServer: true}, replies: map[string]struct{}{}}
	}
	go p.relay(server, client, false, replies)
	p.relay(client, server, true, replies)
}

// track records conn as relayed and reports true, or closes it and reports false while the proxy
// is cut or closed.
func (p *Proxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		_ = conn.Close()
		return false
	default:
	}
	if p.cut {
		p.refusedWhileCut++
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

// refusedDials is how many dials the proxy has refused while cut.
func (p *Proxy) refusedDials() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refusedWhileCut
}

// proxyReadBuffer is the most one read of a relayed connection takes. With ProxyRate the reader
// forwards each read before it sleeps for it, so a direction can run up to proxyReadBuffer bytes
// ahead of its rate.
const proxyReadBuffer = 64 << 10

// relay copies src to dst, each chunk written the proxy's delay after it was read and each read
// held to the proxy's rate, until either ends; then it closes both, which ends the other direction
// too.
func (p *Proxy) relay(dst, src net.Conn, serverToClient bool, replies *replyDropConnection) {
	defer p.untrack(dst, src)
	type chunk struct {
		data []byte
		due  time.Time
	}
	queue := make(chan chunk, 4096)
	// reader names the connection's client-to-server reader to gatedReaders, by the client's address.
	var reader string
	if !serverToClient {
		reader = src.RemoteAddr().String()
	}
	go func() {
		defer close(queue)
		buffer := make([]byte, proxyReadBuffer)
		for {
			p.pass(serverToClient, reader)
			read, err := src.Read(buffer)
			if read > 0 {
				var data []byte
				switch {
				case replies == nil:
					data = bytes.Clone(buffer[:read])
				case serverToClient:
					// filterMessages returns bytes of its own, which the next read does not overwrite.
					data = replies.received.filterMessages(buffer[:read], replies.drops)
				default:
					replies.sent.observe(buffer[:read], replies.recordRequest)
					data = bytes.Clone(buffer[:read])
				}
				if len(data) > 0 {
					queue <- chunk{data: data, due: time.Now().Add(p.delay)}
				}
				if p.rate > 0 {
					time.Sleep(time.Duration(read) * time.Second / time.Duration(p.rate))
				}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		time.Sleep(time.Until(c.due))
		p.pass(serverToClient, "")
		if _, err := dst.Write(c.data); err != nil {
			// The reader ends once src is closed below.
			break
		}
	}
	_ = src.Close()
	for range queue {
	}
}

// pass waits for any active gate before a relay read or write proceeds. serverToClient selects the
// server-to-client gate in addition to the bidirectional one. A client-to-server reader passes its
// client's address as reader, and is named in atGate while it waits at a freeze's gate.
func (p *Proxy) pass(serverToClient bool, reader string) {
	p.mu.Lock()
	gate := p.gate
	serverGate := p.serverGate
	if gate != nil && reader != "" {
		p.atGate[reader] = true
	}
	p.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-p.done:
		}
		if reader != "" {
			p.mu.Lock()
			delete(p.atGate, reader)
			p.mu.Unlock()
		}
	}
	if serverToClient && serverGate != nil {
		select {
		case <-serverGate:
		case <-p.done:
		}
	}
}

// gatedReaders is the set of client addresses whose connection's client-to-server reader waits at
// a freeze's gate now. Such a reader has made its last read of its client: it reads nothing more of
// it until Restore, so the client's socket and the proxy's end of it move no more bytes than the
// kernel already lets them.
func (p *Proxy) gatedReaders() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return maps.Clone(p.atGate)
}

// close stops listening, closes every relayed connection and releases every copy a freeze holds.
func (p *Proxy) close() {
	p.closing.Do(func() {
		close(p.done)
		_ = p.listener.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for conn := range p.conns {
			_ = conn.Close()
		}
		clear(p.conns)
	})
}
