package testdb

import (
	"io"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
)

// Connection is what an Outage proxy does with one connection it accepts.
type Connection int

const (
	// Forward relays the connection to the Postgres behind the proxy, both ways.
	Forward Connection = iota
	// Reset ends the connection with a TCP reset (SO_LINGER 0) as soon as it is accepted.
	Reset
	// Silent holds the connection open and never answers it.
	Silent
)

// OutageProxy is a Postgres that fails as a database in an outage does (Outage). DSN reaches the
// database through it.
type OutageProxy struct {
	DSN string

	listener net.Listener
	upstream string
	plan     []Connection
	silent   atomic.Bool
	connSet
	wg sync.WaitGroup
}

// Outage is dsn's Postgres reached through a TCP proxy on 127.0.0.1 that does with the n-th
// connection it accepts what plan[n] says, and forwards every connection after the plan's last,
// until Silence. Its DSN is dsn at the proxy's address with sslmode=disable: under pgx's default,
// prefer, one connection attempt can open a TLS connection and then a plain one, and a test that
// plans its connections counts one connection to each. The proxy and every connection through it
// close when the test ends, and a connection it accepts after that is closed at once.
func Outage(t testing.TB, dsn string, plan ...Connection) *OutageProxy {
	t.Helper()
	listener, upstream, through := listenProxy(t, dsn, "the outage Postgres proxy")
	disabled, err := url.Parse(through)
	if err != nil {
		_ = listener.Close()
		t.Fatal("net/url refuses the outage proxy's URL")
	}
	query := disabled.Query()
	query.Set("sslmode", "disable")
	disabled.RawQuery = query.Encode()
	proxy := &OutageProxy{DSN: disabled.String(), listener: listener, upstream: upstream, plan: plan}
	proxy.wg.Add(1)
	go proxy.serve()
	t.Cleanup(proxy.close)
	return proxy
}

// Silence makes the database answer nothing from now on: a connection the proxy accepts after it
// is held open and never answered, as Silent holds one, and a connection it already forwards drops
// every byte the server sends, so a client that sends something waits for an answer that never
// comes.
func (p *OutageProxy) Silence() {
	p.silent.Store(true)
}

func (p *OutageProxy) serve() {
	defer p.wg.Done()
	for n := 0; ; n++ {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		handling := Forward
		if n < len(p.plan) {
			handling = p.plan[n]
		}
		if p.silent.Load() {
			handling = Silent
		}
		switch handling {
		case Reset:
			_ = client.(*net.TCPConn).SetLinger(0)
			_ = client.Close()
		case Silent:
			if !p.track(client) {
				return
			}
		default:
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
				_, _ = io.Copy(server, client)
				p.untrack(client, server)
			}()
			go func() {
				defer p.wg.Done()
				p.answer(client, server)
				p.untrack(client, server)
			}()
		}
	}
}

// answer copies the server's bytes to the client, and drops them once the proxy is silenced.
func (p *OutageProxy) answer(client, server net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := server.Read(buffer)
		if n > 0 && !p.silent.Load() {
			if _, err := client.Write(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *OutageProxy) close() {
	p.closeAll(p.listener)
	p.wg.Wait()
}
