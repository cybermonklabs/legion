package testnats

import (
	"io"
	"net"
	"net/url"
	"testing"

	"github.com/nats-io/nats.go"
)

// A client of another process reaches a test's server when the kernel gives the server a port that
// process still dials. Before the server asked a token, such a client stayed connected and the
// test's leak check waited on it until the binary's deadline (dispatch://INFRA-3149: "the NATS
// server still has 1 client connections [6 \"envoy\"] from a previous test after 30s"). Here three
// such clients reach the server mid-test and stay open past the leak check: a nats.go client
// named envoy, a raw connection that sends a CONNECT with no token and then holds its socket, and
// a raw connection that sends nothing. None carries the test's token, so the server closes each
// one, and the test still passes its leak check. The test's own client dials the URL and is
// admitted.
func TestAClientWithoutTheTestsCredentialLeavesNoConnectionAtItsEnd(t *testing.T) {
	var strangers []func()
	// Registered before URL's, so it runs after the leak check: the strangers are still open,
	// from their own side, when the check reads the server.
	t.Cleanup(func() {
		for _, stop := range strangers {
			stop()
		}
	})
	client := URL(t)
	parsed, err := url.Parse(client)
	if err != nil {
		t.Fatal(err)
	}
	address := parsed.Host

	if conn, err := nats.Connect("nats://"+address, nats.Name("envoy")); err == nil {
		strangers = append(strangers, conn.Close)
	} else {
		t.Logf("a nats.go client without the test's token: %v", err)
	}
	connected := func() net.Conn {
		t.Helper()
		conn, err := net.Dial("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		strangers = append(strangers, func() { _ = conn.Close() })
		return conn
	}
	if _, err := io.WriteString(connected(), `CONNECT {"verbose":false,"pedantic":false,"name":"envoy"}`+"\r\nPING\r\n"); err != nil {
		t.Fatal(err)
	}
	connected()

	own := Connect(t, client)
	if err := own.Publish("testnats.own", []byte("x")); err != nil {
		t.Fatalf("the test's own client publishes: %v", err)
	}
	if err := own.Flush(); err != nil {
		t.Fatalf("the test's own client flushes: %v", err)
	}
}
