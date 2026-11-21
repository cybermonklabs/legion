package testnats

import (
	"net"
	"net/url"
	"runtime"
	"testing"
	"time"
)

// A client another test left dialing its own server's old address, 127.0.0.1 and a port the
// kernel has since given this server, does not reach this server: neither the relay a test's URL
// names nor the nats-server's own client listener behind it, which both listen on the server's
// own loopback address (testport.LoopbackIP). The stray client dials as a leftover bus client does,
// every 200 ms; the control is the same client at the server's own URL, which reaches it.
func TestAClientDialingThisServersPortOnTheSharedLoopbackDoesNotReachIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux routes the whole of 127.0.0.0/8 to the loopback; elsewhere every server here listens on 127.0.0.1")
	}
	srv, uri := Start(t)
	own, err := ConnectSeed(t, uri, "")
	if err != nil {
		t.Fatalf("connect to the server: %v", err)
	}
	server := own.ConnectedServerId()
	own.Close()
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse the server's URL %q: %v", uri, err)
	}
	// strayReaches dials address the way a leftover client does and answers how many of its five
	// dials, 200 ms apart, reached this server.
	strayReaches := func(address string) int {
		reached := 0
		for range 5 {
			if nc, err := ConnectSeed(t, address, ""); err == nil {
				if nc.ConnectedServerId() == server {
					reached++
				}
				nc.Close()
			}
			time.Sleep(200 * time.Millisecond)
		}
		return reached
	}
	if !t.Run("control: the same client at the server's own URL reaches it", func(t *testing.T) {
		if reached := strayReaches(uri); reached != 5 {
			t.Fatalf("the client reached the server at its own URL %s on %d of 5 dials, so it cannot be used to check isolation", uri, reached)
		}
	}) {
		return
	}
	relayPort := parsed.Port()
	if reached := strayReaches("nats://" + net.JoinHostPort("127.0.0.1", relayPort)); reached != 0 {
		t.Fatalf("a client dialing 127.0.0.1:%s, the relay's port, reached the server at %s on %d of 5 dials", relayPort, uri, reached)
	}
	backend, err := srv.address()
	if err != nil {
		t.Fatalf("read the nats-server's own client address: %v", err)
	}
	_, backendPort, err := net.SplitHostPort(backend)
	if err != nil {
		t.Fatalf("read the port of %q: %v", backend, err)
	}
	if reached := strayReaches("nats://" + net.JoinHostPort("127.0.0.1", backendPort)); reached != 0 {
		t.Fatalf("a client dialing 127.0.0.1:%s, the nats-server's own client port, reached it on %d of 5 dials", backendPort, reached)
	}
}
