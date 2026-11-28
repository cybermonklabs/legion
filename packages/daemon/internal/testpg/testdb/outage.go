package testdb

import (
	"net"
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

	proxy  *proxy
	plan   []Connection
	silent atomic.Bool
}

// Outage is dsn's Postgres reached through a TCP proxy on 127.0.0.1 that does with the n-th
// connection it accepts what plan[n] says, and forwards every connection after the plan's last,
// until Silence. Its DSN is dsn at the proxy's address with sslmode=disable: under pgx's default,
// prefer, one connection attempt can open a TLS connection and then a plain one, and a test that
// plans its connections counts one connection to each. The proxy and every connection through it
// close when the test ends, and a connection it accepts after that is closed at once.
func Outage(t testing.TB, dsn string, plan ...Connection) *OutageProxy {
	t.Helper()
	listener, upstream, through := listenProxy(t, dsn, "the outage Postgres proxy", true)
	o := &OutageProxy{DSN: through, proxy: startProxy(t, listener, upstream), plan: plan}
	n := 0
	o.proxy.serve(func(client net.Conn) {
		handling := Forward
		if n < len(o.plan) {
			handling = o.plan[n]
		}
		n++
		if o.silent.Load() {
			handling = Silent
		}
		switch handling {
		case Reset:
			_ = client.(*net.TCPConn).SetLinger(0)
			_ = client.Close()
		case Silent:
			o.proxy.track(client)
		default:
			o.proxy.forward(client, copyToServer, o.answer)
		}
	})
	return o
}

// Silence makes the database answer nothing from now on: a connection the proxy accepts after it
// is held open and never answered, as Silent holds one, and a connection it already forwards drops
// every byte the server sends, so a client that sends something waits for an answer that never
// comes.
func (o *OutageProxy) Silence() {
	o.silent.Store(true)
}

// answer copies the server's bytes to the client, and drops them once the proxy is silenced.
func (o *OutageProxy) answer(client, server net.Conn) {
	buffer := make([]byte, 32<<10)
	for {
		n, err := server.Read(buffer)
		if n > 0 && !o.silent.Load() {
			if _, err := client.Write(buffer[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
