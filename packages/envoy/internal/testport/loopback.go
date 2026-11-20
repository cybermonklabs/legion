package testport

import (
	"crypto/rand"
	"fmt"
	"net"
)

// Loopback is this test binary's own loopback address: 127.a.b.c with a, b and c drawn when the
// binary starts, or 127.0.0.1 where the system serves no other loopback address (Linux serves all
// of 127.0.0.0/8). Every listener a test of this module starts in front of a NATS server or a
// database, or in place of one, opens on it (Listen), and so do testnats's servers, relays and
// gates, but for a callout server's relay, which takes an address of its own (testnats's
// calloutHost). The check in internal/textcheck (loopback_listeners_test.go) refuses a listener
// that opens on a fixed address in a package whose tests start a NATS server or a Postgres.
//
// A port a test held and let go is dialed by nobody of its binary, but it is dialed by a client of
// another test binary whose test ended without closing it: nats.go reconnects to the URL it was
// handed for as long as its process lives, and a bus client that owns the stream ensures
// ENVOY_NOTIFICATIONS on every server it reaches. On a box where many test binaries run at once,
// the system hands the same port on 127.0.0.1 to the next binary that asks, and such a client then
// reached that binary's fresh cluster: a probe of 3,200 fresh three-member clusters, 32 binaries at
// a time, found a client named "envoy" that this binary never made in 10 of them, 9 of those with
// ENVOY_NOTIFICATIONS already created, and a test that deployed the stream itself failed "nats:
// stream name already in use". A client of another binary knows only addresses its own binary gave
// it, on 127.0.0.1 for a binary built before this address or on that binary's own 127.a.b.c, so it
// reaches none of this binary's listeners.
//
// The daemon module's copy of this package (testport.go's comment) has no Loopback: it has no
// caller there.
var Loopback = ownLoopback()

// ownLoopback draws this binary's loopback address: one this process can listen on, and never
// 127.0.0.1 or another address in 127.0.0.0/16, where a binary of an earlier build listens.
func ownLoopback() string {
	for range 8 {
		var b [3]byte
		if _, err := rand.Read(b[:]); err != nil {
			break
		}
		host := fmt.Sprintf("127.%d.%d.%d", 1+int(b[0])%254, b[1], 1+int(b[2])%254)
		listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err == nil {
			_ = listener.Close()
			return host
		}
	}
	return "127.0.0.1"
}

// Listen opens a TCP listener on a port the system picks at Loopback: the listener of every test
// relay, proxy or fake server in front of a NATS server or a database, or in place of one, so no
// client of another test binary reaches it.
func Listen() (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort(Loopback, "0"))
}
