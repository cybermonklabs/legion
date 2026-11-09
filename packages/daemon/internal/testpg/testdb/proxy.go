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
// a DSN, so it is the second place a reading of net/url's could send a test's traffic to a
// machine the caller never named (DatabaseURL says how the two parsers differ). The refusals name
// the reading alone and print no password.
func Delayed(t testing.TB, dsn string, delay time.Duration) string {
	t.Helper()
	const named = "the database URL to proxy"
	target, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("read %s with pgx: %s", named, parseReason(err))
	}
	upstream := net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port)))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the delayed Postgres proxy: %v", err)
	}
	address := listener.Addr().(*net.TCPAddr)
	through, err := rewriteDSN(named, dsn, rewrite{host: address.IP.String(), port: uint16(address.Port)})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	proxy := &delayedProxy{listener: listener, upstream: upstream, delay: delay, conns: map[net.Conn]struct{}{}}
	proxy.wg.Add(1)
	go proxy.serve()
	t.Cleanup(proxy.close)
	return through
}

// delayedProxy is Delayed's proxy: conns are the client and server connections open through it,
// closed with it.
type delayedProxy struct {
	listener net.Listener
	upstream string
	delay    time.Duration

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
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

// track records a pair of connections, and closes them instead when the proxy is already closed.
func (p *delayedProxy) track(client, server net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = client.Close()
		_ = server.Close()
		return false
	}
	p.conns[client], p.conns[server] = struct{}{}, struct{}{}
	return true
}

// untrack closes a pair of connections, once either direction ends.
func (p *delayedProxy) untrack(client, server net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, client)
	delete(p.conns, server)
	_ = client.Close()
	_ = server.Close()
}

func (p *delayedProxy) close() {
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
