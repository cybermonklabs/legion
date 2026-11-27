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
	listener, upstream, through := listenProxy(t, dsn, "the delayed Postgres proxy", false)
	p := startProxy(t, listener, upstream)
	p.serve(func(client net.Conn) {
		p.forward(client, func(client, server net.Conn) { p.relayDelayed(client, server, delay) }, copyToClient)
	})
	return through
}

// listenProxy is a listener on 127.0.0.1 for a proxy to the server pgconn.ParseConfig reads from
// dsn, that server's address, which the proxy dials, and dsn rewritten by rewriteDSN to reach the
// listener, with TLS off where plain is set. proxy names the proxy in the refusal of a listen that
// fails. Delayed, Outage and CommitFaults take their URLs from it.
func listenProxy(t testing.TB, dsn, proxy string, plain bool) (net.Listener, string, string) {
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
	through, err := rewriteDSN(named, dsn, rewrite{host: address.IP.String(), port: uint16(address.Port), plain: plain})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	return listener, net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port))), through
}

// proxy is a TCP proxy on 127.0.0.1 to a Postgres: its listener, the server it dials, the
// connections open through it and the goroutines that serve them. It closes, with every
// connection through it, when the test ends, and a connection it takes after that is closed at
// once. Delayed and Outage are proxies that differ in what they do with each connection.
type proxy struct {
	listener net.Listener
	upstream string

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// startProxy is a proxy on listener to upstream, closed at the test's end.
func startProxy(t testing.TB, listener net.Listener, upstream string) *proxy {
	p := &proxy{listener: listener, upstream: upstream, conns: map[net.Conn]struct{}{}}
	t.Cleanup(p.close)
	return p
}

// serve hands each connection the proxy accepts to accept, on a goroutine of its own that the
// proxy's close waits for, until the listener closes.
func (p *proxy) serve(accept func(client net.Conn)) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			client, err := p.listener.Accept()
			if err != nil {
				return
			}
			accept(client)
		}
	}()
}

// forward dials the server for client and relays the pair both ways: client to server through
// toServer, server to client through toClient, each on a goroutine of its own. The pair closes
// once either way ends.
func (p *proxy) forward(client net.Conn, toServer, toClient func(client, server net.Conn)) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	if !p.track(client, server) {
		return
	}
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		toServer(client, server)
		p.untrack(client, server)
	}()
	go func() {
		defer p.wg.Done()
		toClient(client, server)
		p.untrack(client, server)
	}()
}

// copyToClient copies the server's bytes to the client as they come.
func copyToClient(client, server net.Conn) {
	_, _ = io.Copy(client, server)
}

// copyToServer copies the client's bytes to the server as they come.
func copyToServer(client, server net.Conn) {
	_, _ = io.Copy(server, client)
}

// track records conns, and closes them instead when the proxy is already closed.
func (p *proxy) track(conns ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		for _, conn := range conns {
			_ = conn.Close()
		}
		return false
	}
	for _, conn := range conns {
		p.conns[conn] = struct{}{}
	}
	return true
}

// untrack closes conns, a pair once either way ends.
func (p *proxy) untrack(conns ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, conn := range conns {
		delete(p.conns, conn)
		_ = conn.Close()
	}
}

func (p *proxy) close() {
	p.mu.Lock()
	p.closed = true
	_ = p.listener.Close()
	for conn := range p.conns {
		_ = conn.Close()
	}
	clear(p.conns)
	p.mu.Unlock()
	p.wg.Wait()
}

// chunk is what one read of the client returned, and when it may be written to the server.
type chunk struct {
	data []byte
	due  time.Time
}

// relayDelayed copies client to server, each chunk written delay after it was read. A reader and
// a writer run apart, so a chunk read while an earlier one is held is held from its own read.
func (p *proxy) relayDelayed(client, server net.Conn, delay time.Duration) {
	chunks := make(chan chunk, 64)
	go func() {
		defer close(chunks)
		buffer := make([]byte, 64<<10)
		for {
			n, err := client.Read(buffer)
			if n > 0 {
				chunks <- chunk{data: bytes.Clone(buffer[:n]), due: time.Now().Add(delay)}
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
