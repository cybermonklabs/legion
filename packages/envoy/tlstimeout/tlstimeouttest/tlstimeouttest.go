// Package tlstimeouttest is the peer a test of a TLS handshake timeout dials: a TCP listener that
// takes every connection and never answers a TLS ClientHello, so a Go client whose transport sets
// a TLSHandshakeTimeout fails with net/http's own `net/http: TLS handshake timeout`, and curl waits
// for its --max-time. It is one body for both modules: packages/daemon imports it through its
// replace of packages/envoy, as it imports clitest.
package tlstimeouttest

import (
	"io"
	"net"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/testport"
)

// Silent is the host and port of a listener at a loopback address of its own
// (testport.ListenLoopback) that accepts every connection, reads and drops whatever the client
// sends, a ClientHello among it, and writes nothing back. It closes the listener and every
// connection it took when the test ends.
func Silent(t testing.TB) string {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("tlstimeouttest: listen for a peer that never answers: %v", err)
	}
	var (
		mu    sync.Mutex
		taken []net.Conn
		done  bool
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if done {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			taken = append(taken, conn)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		done = true
		for _, conn := range taken {
			_ = conn.Close()
		}
	})
	return listener.Addr().String()
}
