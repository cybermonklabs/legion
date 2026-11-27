package testnats

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/testport"
)

const (
	// silentWebSocketChildEnv has TestAConnectToASilentWebSocketServerFailsByNameAtTheGiveUp run as
	// its own child, which connects under a deadline of silentWebSocketDeadline.
	silentWebSocketChildEnv = "TESTNATS_SILENT_WEBSOCKET_CHILD"

	// silentWebSocketDeadline is the child's go test -timeout. The shared wait gives up 30 s before
	// it, 5 s in: the child's setup, one listener, needs little of that, and its connect runs from
	// there to the give-up.
	silentWebSocketDeadline = 35 * time.Second
)

// silentWebSocketGiveUp is the child's failure: the shared wait's give-up naming the connection it
// waited for, whose last dial ended on the deadline the dial set (giveUpDial), an i/o timeout.
var silentWebSocketGiveUp = regexp.MustCompile(`gave up waiting for a connection to the NATS server at wss://\S+ 30s before the test binary's deadline; last read: [^\n]*i/o timeout`)

// A connect to a wss:// URL whose server accepts the connection and sends nothing fails the test at
// the shared wait's give-up, naming the connection it waited for, before go test's timeout panics.
// nats.go runs a wss:// URL's TLS handshake before it sets its connect's own deadline, so only the
// deadline the dial sets (giveUpDial) bounds that handshake: without it the handshake, and the
// wait asking for the connection, hold to the panic. Main's
// TestAConnectToASilentWebSocketServerEndsWithTheWait held the same of main's wait.
func TestAConnectToASilentWebSocketServerFailsByNameAtTheGiveUp(t *testing.T) {
	if os.Getenv(silentWebSocketChildEnv) != "" {
		Connect(t, "wss://"+silentServer(t))
		t.Fatal("the connect to a wss:// server that sends nothing returned a connection")
		return
	}
	child := exec.Command(os.Args[0], "-test.run", "^"+t.Name()+"$", "-test.count=1", "-test.timeout="+silentWebSocketDeadline.String())
	child.Env = append(os.Environ(), silentWebSocketChildEnv+"=1")
	output, _ := child.CombinedOutput() // the child fails by design: its server never answers
	if bytes.Contains(output, []byte("panic: test timed out")) || !silentWebSocketGiveUp.Match(output) {
		t.Fatalf("the child's connect to a wss:// server that sends nothing did not fail at the shared wait's give-up, on the deadline its dial set, before its %s deadline:\n%s", silentWebSocketDeadline, output)
	}
}

// silentServer listens on a loopback address of its own, holds every connection it accepts open
// without sending a byte until the test ends, and returns the address.
func silentServer(t *testing.T) string {
	t.Helper()
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen for a server that sends nothing: %v", err)
	}
	var (
		mu   sync.Mutex
		held []net.Conn
	)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			conn.Close()
		}
	})
	return listener.Addr().String()
}
