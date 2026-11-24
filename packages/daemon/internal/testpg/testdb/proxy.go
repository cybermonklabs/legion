package testdb

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Delayed is dsn reached through a TCP proxy on 127.0.0.1 that holds each chunk the client sends
// for delay before passing it to the server, and passes the server's answers back at once: the
// network between a daemon and a Postgres that is not on its machine. A client that waits for each
// answer before it writes again - every statement outside a pipeline - pays delay once per round
// trip, as many times as the statements it sends one after another. A chunk is held from the
// moment the proxy reads it, not queued behind the hold of the chunk before, so a client that
// writes without waiting is delayed once, not once per chunk. The proxy and every connection
// through it close when the test ends, before the database is dropped (Database's cleanup is
// registered first, so it runs after this one).
//
// The server the proxy dials is the one pgconn.ParseConfig reads from dsn, and the URL it answers
// is rewriteDSN's, which reads that URL back with the same parser: this helper moves the host of
// a DSN, so it takes the check DatabaseURL's comment describes. A refusal names the fields that
// moved, or the string a parser refused, and prints no value of either, so it prints no
// password.
func Delayed(t testing.TB, dsn string, delay time.Duration) string {
	t.Helper()
	listener, upstream, through := listenProxy(t, dsn, "the delayed Postgres proxy")
	proxy := &delayedProxy{listener: listener, upstream: upstream, delay: delay}
	proxy.wg.Add(1)
	go proxy.serve()
	t.Cleanup(proxy.close)
	return through
}

// listenProxy is a listener on 127.0.0.1 for a proxy to the server pgconn.ParseConfig reads from
// dsn, that server's address, which the proxy dials, and dsn rewritten by rewriteDSN to reach the
// listener. proxy names the proxy in the refusal of a listen that fails. Delayed, Outage and
// CommitFaults take their URLs from it.
func listenProxy(t testing.TB, dsn, proxy string) (net.Listener, string, string) {
	t.Helper()
	const named = "the database URL to proxy"
	target, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx refuses %s", named)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for %s: %v", proxy, err)
	}
	address := listener.Addr().(*net.TCPAddr)
	through, err := rewriteDSN(named, dsn, rewrite{host: address.IP.String(), port: uint16(address.Port)})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	return listener, net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))), through
}

// delayedProxy is Delayed's proxy, closed with the client and server connections open through it.
type delayedProxy struct {
	listener net.Listener
	upstream string
	delay    time.Duration
	connSet
	wg sync.WaitGroup
}

func (p *delayedProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream)
		if err != nil {
			_ = client.Close()
			continue
		}
		if !p.track(client, server) {
			return
		}
		p.wg.Add(2)
		go func() {
			defer p.wg.Done()
			_, _ = io.Copy(client, server)
			p.untrack(client, server)
		}()
		go func() {
			defer p.wg.Done()
			p.relayDelayed(client, server)
			p.untrack(client, server)
		}()
	}
}

// chunk is what one read of the client returned, and when it may be written to the server.
type chunk struct {
	data []byte
	due  time.Time
}

// relayDelayed copies client to server, each chunk written delay after it was read. A reader and
// a writer run apart, so a chunk read while an earlier one is held is held from its own read.
func (p *delayedProxy) relayDelayed(client, server net.Conn) {
	chunks := make(chan chunk, 64)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 64<<10)
		for {
			n, err := client.Read(buffer)
			if n > 0 {
				chunks <- chunk{data: bytes.Clone(buffer[:n]), due: time.Now().Add(p.delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for next := range chunks {
		time.Sleep(time.Until(next.due))
		if _, err := server.Write(next.data); err != nil {
			// The reader stops once its connection is closed below; drain what it still sends.
			p.untrack(client, server)
			for range chunks {
			}
			return
		}
	}
}

// connSet is the connections open through a proxy, closed when the proxy closes. A connection a
// proxy accepts after that is closed at once rather than tracked.
type connSet struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// track records conns, and closes them instead when the proxy is already closed.
func (s *connSet) track(conns ...net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	for _, conn := range conns {
		s.conns[conn] = struct{}{}
	}
	return true
}

// untrack closes conns, a pair once either direction ends.
func (s *connSet) untrack(conns ...net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, conn := range conns {
		delete(s.conns, conn)
		_ = conn.Close()
	}
}

// closeAll closes listener and every connection open through the proxy, and marks it closed.
func (s *connSet) closeAll(listener net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	_ = listener.Close()
	for conn := range s.conns {
		_ = conn.Close()
	}
	clear(s.conns)
}

func (p *delayedProxy) close() {
	p.closeAll(p.listener)
	p.wg.Wait()
}
