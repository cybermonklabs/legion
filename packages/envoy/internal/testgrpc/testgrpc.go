// Package testgrpc serves a test's gRPC stand-in on a loopback port and stops it when the test ends
// without waiting on a peer the test does not own.
//
// grpc's Server.Stop closes the connections that have finished their HTTP/2 handshake, but it
// waits for each accepted connection still in that handshake (Server.handleRawConn) until the
// server's connection timeout, 120 s unless the server sets one. A port of 127.0.0.1 is open to
// every process of the machine, so a client another program left reconnecting to a port it once
// used can reach a stand-in that took the port since: a NATS client sends nothing until the server
// it dialed speaks first, and one such peer holds Stop, and so the test's end, for the whole
// timeout.
package testgrpc

import (
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
)

// Serve serves server on a port of 127.0.0.1 that the kernel chooses, until t ends, and answers that
// port's address. When t ends it closes the listener and every connection the listener accepted,
// the ones still in their handshake included, and only then stops server, so the stop waits on no
// peer, whoever's it is.
func Serve(t testing.TB, server *grpc.Server) string {
	t.Helper()
	addr, stop, err := serve(server)
	if err != nil {
		t.Fatalf("testgrpc: listen on loopback: %v", err)
	}
	t.Cleanup(stop)
	return addr
}

// serve is Serve without the test: it answers the address and the stop Serve runs when its test
// ends.
func serve(server *grpc.Server) (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	owned := &ownedListener{Listener: listener}
	go func() { _ = server.Serve(owned) }()
	return listener.Addr().String(), func() {
		owned.closeAll()
		server.Stop()
	}, nil
}

// ownedListener keeps every connection it accepts, so closeAll can close them.
type ownedListener struct {
	net.Listener
	mu     sync.Mutex
	closed bool
	conns  []net.Conn
}

// Accept accepts the listener's next connection and keeps it; one accepted once closeAll has run is
// closed at once, as the listener's own Accept would refuse it.
func (l *ownedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		_ = conn.Close()
		return nil, net.ErrClosed
	}
	l.conns = append(l.conns, conn)
	return conn, nil
}

// closeAll closes the listener and every connection it accepted.
func (l *ownedListener) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	_ = l.Listener.Close()
	for _, conn := range l.conns {
		_ = conn.Close()
	}
}
