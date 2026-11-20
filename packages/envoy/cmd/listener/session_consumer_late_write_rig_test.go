package main

// Rigs for the cases of a consumer write whose answer does not come back
// (session_consumer_unanswered_write_test.go, session_consumer_lost_write_test.go,
// session_consumer_unanswered_correction_test.go):
//
//   - frameRelay holds or drops one frame of the listener's session-consumer connection, a
//     consumer create or update, and lets every frame after it through at once, which is the order
//     nats-server's pool of API workers can give a slow request (bus.UnansweredWrite);
//   - linkProxy holds every byte of one link from a consumer create or update on, and can close
//     that link's client side alone, so nats.go reconnects while the server still holds the old
//     link open with the held bytes unread: a write delayed in the network past a reconnect;
//   - apiAudits reads the server's audit advisory of each JetStream API request, which the server
//     sends right after its answer (server/jetstream_api.go v2.15.1, sendAPIResponse), so a case
//     can wait for the fact that a held write was applied rather than for a time.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// frameRelay is a TCP relay in front of a NATS server that parses the client's frames and hands
// the occurrence-th PUB or HPUB whose subject starts with prefix to the act set for it, forwarding
// every other frame as it comes.
type frameRelay struct {
	ln     net.Listener
	target string
	url    string
	prefix []byte
	mu     sync.Mutex
	seen   int
	act    map[int]func([]byte) []byte
	acted  chan int
}

func startFrameRelay(t *testing.T, uri, prefix string) *frameRelay {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %q: %v", uri, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	relayed := *server
	relayed.Host = ln.Addr().String()
	r := &frameRelay{ln: ln, target: server.Host, url: relayed.String(), prefix: []byte(prefix), act: map[int]func([]byte) []byte{}, acted: make(chan int, 16)}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.link(c)
		}
	}()
	return r
}

// on sets what the occurrence-th matching frame, counted from the relay's start, is handed to: act
// answers the bytes to forward, or nil to drop the frame, and runs on a goroutine of its own.
func (r *frameRelay) on(occurrence int, act func([]byte) []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.act[occurrence] = act
}

// next is the occurrence the next matching frame will be.
func (r *frameRelay) next() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen + 1
}

func (r *frameRelay) link(client net.Conn) {
	server, err := net.Dial("tcp", r.target)
	if err != nil {
		_ = client.Close()
		return
	}
	go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
	defer func() { _ = server.Close() }()
	var wmu sync.Mutex
	write := func(b []byte) {
		wmu.Lock()
		defer wmu.Unlock()
		_, _ = server.Write(b)
	}
	in := bufio.NewReaderSize(client, 64*1024)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			return
		}
		frame := line
		if bytes.HasPrefix(line, []byte("PUB ")) || bytes.HasPrefix(line, []byte("HPUB ")) {
			fields := bytes.Fields(line)
			size, perr := strconv.Atoi(string(fields[len(fields)-1]))
			if perr != nil {
				return
			}
			body := make([]byte, size+2)
			if _, err := io.ReadFull(in, body); err != nil {
				return
			}
			frame = append(append([]byte{}, line...), body...)
			if bytes.HasPrefix(fields[1], r.prefix) {
				r.mu.Lock()
				r.seen++
				n := r.seen
				act := r.act[n]
				r.mu.Unlock()
				if act != nil {
					go func(f []byte) {
						if out := act(f); out != nil {
							write(out)
						}
						r.acted <- n
					}(frame)
					continue
				}
			}
		}
		write(frame)
	}
}

// holdFor is an act that runs before, then holds the frame for d and forwards it.
func holdFor(d time.Duration, before func()) func([]byte) []byte {
	return func(f []byte) []byte {
		if before != nil {
			before()
		}
		time.Sleep(d)
		return f
	}
}

// linkProxy is a TCP proxy in front of a NATS server that holds every byte of the one link that
// carried an armed trigger, from the chunk that completed it on.
type linkProxy struct {
	ln      net.Listener
	target  string
	url     string
	mu      sync.Mutex
	trigger []byte
	held    *proxyLink
	heldCh  chan struct{}
	links   []*proxyLink
}

type proxyLink struct {
	client, server net.Conn
	mu             sync.Mutex
	holding        bool
	buf            []byte
	seen           []byte
}

func startLinkProxy(t *testing.T, uri string) *linkProxy {
	t.Helper()
	server, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse %q: %v", uri, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	relayed := *server
	relayed.Host = ln.Addr().String()
	p := &linkProxy{ln: ln, target: server.Host, url: relayed.String(), heldCh: make(chan struct{}, 1)}
	t.Cleanup(p.close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.relay(c)
		}
	}()
	return p
}

func (p *linkProxy) relay(c net.Conn) {
	s, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	link := &proxyLink{client: c, server: s}
	p.mu.Lock()
	p.links = append(p.links, link)
	p.mu.Unlock()
	go func() {
		_, _ = io.Copy(c, s)
		_ = c.Close()
	}()
	buf := make([]byte, 32*1024)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if !p.hold(link, data) {
				if _, werr := s.Write(data); werr != nil {
					return
				}
			}
		}
		if err != nil {
			link.mu.Lock()
			holding := link.holding
			link.mu.Unlock()
			if !holding {
				_ = s.Close()
			}
			return
		}
	}
}

// hold answers whether data is held: every byte of the one link that carried the armed trigger,
// from the chunk that completed it on.
func (p *linkProxy) hold(link *proxyLink, data []byte) bool {
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.holding {
		link.buf = append(link.buf, data...)
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.trigger == nil || p.held != nil {
		return false
	}
	link.seen = append(link.seen, data...)
	if !bytes.Contains(link.seen, p.trigger) {
		return false
	}
	p.held, p.trigger = link, nil
	link.holding = true
	link.buf = append(link.buf, data...)
	select {
	case p.heldCh <- struct{}{}:
	default:
	}
	return true
}

func (p *linkProxy) arm(trigger string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.trigger = []byte(trigger)
	for _, l := range p.links {
		l.mu.Lock()
		l.seen = nil
		l.mu.Unlock()
	}
}

// dropClient closes the held link's client side only: nats.go sees its connection go and
// reconnects through the proxy, while the old link's server side stays open with the held bytes.
func (p *linkProxy) dropClient() {
	p.mu.Lock()
	link := p.held
	p.mu.Unlock()
	_ = link.client.Close()
}

// release writes the held bytes to the held link's server side, in order.
func (p *linkProxy) release(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	link := p.held
	p.mu.Unlock()
	link.mu.Lock()
	buf := link.buf
	link.buf, link.holding = nil, false
	link.mu.Unlock()
	if _, err := link.server.Write(buf); err != nil {
		t.Fatalf("release the held bytes on the old link: %v", err)
	}
	t.Logf("released %d held bytes on the old link", len(buf))
}

func (p *linkProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, l := range p.links {
		_ = l.client.Close()
		_ = l.server.Close()
	}
}

// apiAudits is the server's audit advisories of its JetStream API requests, read from the moment
// watchAPIAudits subscribes.
type apiAudits struct {
	mu     sync.Mutex
	audits []apiAudit
	added  chan struct{}
}

type apiAudit struct {
	Subject  string `json:"subject"`
	Request  string `json:"request"`
	Response string `json:"response"`
}

func watchAPIAudits(t *testing.T, uri string) *apiAudits {
	t.Helper()
	nc, err := nats.Connect(uri, nats.Name("envoy-test-api-audits"))
	if err != nil {
		t.Fatalf("connect for the API audit advisories: %v", err)
	}
	t.Cleanup(nc.Close)
	a := &apiAudits{added: make(chan struct{}, 1)}
	if _, err := nc.Subscribe("$JS.EVENT.ADVISORY.API", func(m *nats.Msg) {
		var audit apiAudit
		if json.Unmarshal(m.Data, &audit) != nil {
			return
		}
		a.mu.Lock()
		a.audits = append(a.audits, audit)
		a.mu.Unlock()
		select {
		case a.added <- struct{}{}:
		default:
		}
	}); err != nil {
		t.Fatalf("subscribe the API audit advisories: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush the API audit subscription: %v", err)
	}
	return a
}

// count is how many audits have come so far: an awaitApplied that starts from it reads only the
// requests the server answered after it.
func (a *apiAudits) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.audits)
}

// awaitApplied waits, for at most within, for the server's audit, from the from-th on, of a
// consumer create or update of sessionID's consumer whose request match accepts and whose answer
// is no error: the fact that the server applied such a write. It fails the test when none comes.
func (a *apiAudits) awaitApplied(t *testing.T, from int, sessionID string, match func(request string) bool, within time.Duration) {
	t.Helper()
	name, _ := bus.SessionConsumerName(sessionID)
	subject := "$JS.API.CONSUMER.CREATE." + bus.Stream + "." + name
	deadline := time.After(within)
	for {
		a.mu.Lock()
		for _, audit := range a.audits[min(from, len(a.audits)):] {
			if audit.Subject == subject && match(audit.Request) && !strings.Contains(audit.Response, `"error"`) {
				a.mu.Unlock()
				return
			}
		}
		a.mu.Unlock()
		select {
		case <-a.added:
		case <-deadline:
			t.Fatalf("the server applied no write of %s's consumer that the case waits for within %v", sessionID, within)
		}
	}
}

// carrying is an awaitApplied match for a request whose filters hold topic.
func carrying(topic string) func(string) bool {
	return func(request string) bool { return strings.Contains(request, topic) }
}

// lacking is an awaitApplied match for a request whose filters do not hold topic.
func lacking(topic string) func(string) bool {
	return func(request string) bool { return !strings.Contains(request, topic) }
}

// startPingServer starts a NATS server of the test's own whose ping_interval is pingInterval and
// whose ping_max is pingMax, and answers its URL.
func startPingServer(t *testing.T, pingInterval time.Duration, pingMax int) string {
	t.Helper()
	config := "host: 127.0.0.1\nping_interval: \"" + pingInterval.String() + "\"\nping_max: " + strconv.Itoa(pingMax) + "\n"
	_, uri := testnats.StartConfig(t, config, []string{"-js"}, true)
	return uri
}
