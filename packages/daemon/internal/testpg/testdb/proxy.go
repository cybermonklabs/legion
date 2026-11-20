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
	through, upstream := relayFor(t, "the database URL to proxy", dsn)
	startRelay(t, through, func() string { return upstream }, func(client, server net.Conn) {
		relayDelayed(client, server, delay)
	})
	return through.url
}

// chunk is what one read of the client returned, and when it may be written to the server.
type chunk struct {
	data []byte
	due  time.Time
}

// relayDelayed copies client to server, each chunk written delay after it was read. A reader and
// a writer run apart, so a chunk read while an earlier one is held is held from its own read.
func relayDelayed(client, server net.Conn, delay time.Duration) {
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
			// The reader stops once its connection is closed here; drain what it still sends.
			_ = client.Close()
			_ = server.Close()
			for range chunks {
			}
			return
		}
	}
}

// relayListener is a fixture proxy's listener on 127.0.0.1 and the URL that reaches dsn through
// it.
type relayListener struct {
	listener net.Listener
	url      string
}

// relayFor listens for a fixture proxy in front of dsn, named named in a refusal, and answers that
// listener and the server the proxy dials for dsn: the one pgconn.ParseConfig reads from dsn, as
// Delayed says.
func relayFor(t testing.TB, named, dsn string) (relayListener, string) {
	t.Helper()
	target, err := pgconn.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgx refuses %s", named)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for a Postgres proxy in front of %s: %v", named, err)
	}
	address := listener.Addr().(*net.TCPAddr)
	through, err := rewriteDSN(named, dsn, rewrite{host: address.IP.String(), port: uint16(address.Port)})
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	return relayListener{listener: listener, url: through}, net.JoinHostPort(target.Host, strconv.Itoa(int(target.Port)))
}

// relayProxy is the one lifecycle of this package's fixture proxies (Delayed, Breaking, Flapping):
// a listener, every pair of connections open through it, and their close when the test ends. What
// a fixture varies is the server each accepted connection dials (upstream, read at each accept)
// and how the client's bytes reach it (relay, which returns once that direction is done); the
// server's answers pass back to the client at once.
type relayProxy struct {
	listener net.Listener
	upstream func() string
	relay    func(client, server net.Conn)

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// startRelay serves a relayProxy on through's listener until the test ends, then closes it and
// every connection through it.
func startRelay(t testing.TB, through relayListener, upstream func() string, relay func(client, server net.Conn)) {
	p := &relayProxy{listener: through.listener, upstream: upstream, relay: relay, conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.close)
}

func (p *relayProxy) serve() {
	defer p.wg.Done()
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.upstream())
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
			p.relay(client, server)
			p.untrack(client, server)
		}()
	}
}

// track records a pair of connections, and closes them instead when the proxy is already closed.
func (p *relayProxy) track(client, server net.Conn) bool {
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
func (p *relayProxy) untrack(client, server net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.conns, client)
	delete(p.conns, server)
	_ = client.Close()
	_ = server.Close()
}

func (p *relayProxy) close() {
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

// relayPlain copies client to server as it comes.
func relayPlain(client, server net.Conn) {
	_, _ = io.Copy(server, client)
}
