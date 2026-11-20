// Package tlsalerttest is a TCP server that answers a TLS client's first record with a TLS alert
// record, so a test meets a real alert over TCP as Go's TLS client returns it: a fatal alert the
// far side sent ("remote error"), or a record the client cannot take, which it refuses with an
// alert of its own ("local error"). Each server listens for the whole test.
package tlsalerttest

import (
	"io"
	"net"
	"testing"
)

// Remote is the address of a server that answers a ClientHello with a fatal handshake_failure
// alert: the client returns a *net.OpError whose Op is "remote error".
func Remote(t testing.TB) string {
	return serve(t, []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 0x28})
}

// Local is the address of a server that answers a ClientHello with an alert record three bytes
// long, which no alert is: the client sends unexpected_message and returns a *net.OpError whose Op
// is "local error".
func Local(t testing.TB) string {
	return serve(t, []byte{0x15, 0x03, 0x03, 0x00, 0x03, 0x02, 0x28, 0x00})
}

// serve listens on loopback until t ends, and answers each connection's first TLS record, read
// whole, with answer, then reads until the client closes, so the client reads the answer rather
// than a reset.
func serve(t testing.TB, answer []byte) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				header := make([]byte, 5)
				if _, err := io.ReadFull(conn, header); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, conn, int64(header[3])<<8|int64(header[4])); err != nil {
					return
				}
				if _, err := conn.Write(answer); err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, conn)
			}()
		}
	}()
	return lis.Addr().String()
}
