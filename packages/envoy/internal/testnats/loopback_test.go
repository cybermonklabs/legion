package testnats

import (
	"net"
	"net/url"
	"runtime"
	"testing"
	"time"
)

// A client another test left dialing its own server's old address, 127.0.0.1 and a port the
// kernel has since given a listener here, does not reach this test's server through any listener
// this package's in-process constructors open for a client to dial: the relay a test's URL names,
// the nats-server's own client listener behind it (Start), a handshake hold's proxy (StartProxy)
// and the hold's own relay between that proxy and the server (StartHandshakeHold), and a gate
// (StartGate). Each listens on a loopback address of its own (testport.LoopbackIP). A container's
// published port (StartImageConfig) is chosen by testport.LoopbackPorts and is no row here: this
// package's untagged tests start no container. The stray client dials as a leftover bus client
// does, every 200 ms; each listener's control is the same client at the listener's own address,
// which reaches the server.
func TestAClientDialingThisServersPortOnTheSharedLoopbackDoesNotReachIt(t *testing.T) {
	t.Parallel()
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
	// strayReaches dials address the way a leftover client does and answers how many of its five
	// dials, 200 ms apart, reached this server.
	strayReaches := func(t *testing.T, address string) int {
		reached := 0
		for range 5 {
			if nc, err := ConnectSeed(t, "nats://"+address, ""); err == nil {
				if nc.ConnectedServerId() == server {
					reached++
				}
				nc.Close()
			}
			time.Sleep(200 * time.Millisecond)
		}
		return reached
	}
	backend, err := srv.address()
	if err != nil {
		t.Fatalf("read the nats-server's own client address: %v", err)
	}
	hold := StartHandshakeHold(t, uri)
	listeners := []struct {
		name    string
		address string
	}{
		{"the relay a test's URL names", urlHost(t, uri)},
		{"the nats-server's own client listener", backend},
		{"a handshake hold's proxy", urlHost(t, hold.URL())},
		{"the handshake hold's own relay", hold.listener.Addr().String()},
		{"a gate", urlHost(t, StartGate(t, srv).URL())},
	}
	for _, listener := range listeners {
		t.Run(listener.name, func(t *testing.T) {
			if reached := strayReaches(t, listener.address); reached != 5 {
				t.Fatalf("control: the client reached the server at %s, %s, on %d of 5 dials, so it cannot be used to check isolation", listener.address, listener.name, reached)
			}
			_, port, err := net.SplitHostPort(listener.address)
			if err != nil {
				t.Fatalf("read the port of %q: %v", listener.address, err)
			}
			shared := net.JoinHostPort("127.0.0.1", port)
			if reached := strayReaches(t, shared); reached != 0 {
				t.Fatalf("a client dialing %s, the port of %s at %s, reached the server on %d of 5 dials", shared, listener.name, listener.address, reached)
			}
		})
	}
}

// A client another test left dialing 127.0.0.1 at the port of a handshake hold's own relay is no
// handshake attempt of this test's: during a Hold it does not reach the hold, so it moves neither
// Dials, Attempts nor the channel Hold returns, which a row reads as its own client's reconnect
// being held (HoldReconnect, AnswersWithinAttempt). The control is the same client at the relay's
// own address, whose dial and CONNECT the hold counts and whose CONNECT it holds.
func TestAClientDialingTheHoldsRelayPortOnTheSharedLoopbackIsNoHeldAttempt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("only Linux routes the whole of 127.0.0.0/8 to the loopback; elsewhere every server here listens on 127.0.0.1")
	}
	_, uri := Start(t)
	hold := StartHandshakeHold(t, uri)
	relay := hold.listener.Addr().String()
	_, port, err := net.SplitHostPort(relay)
	if err != nil {
		t.Fatalf("read the port of the hold's relay %q: %v", relay, err)
	}
	held := hold.Hold()
	t.Cleanup(hold.Release)
	shared := net.JoinHostPort("127.0.0.1", port)
	if nc, err := ConnectSeed(t, "nats://"+shared, ""); err == nil {
		nc.Close()
	}
	if dials, attempts := hold.Dials(), hold.Attempts(); dials != 0 || attempts != 0 {
		t.Fatalf("a client dialing %s, the port of the hold's relay at %s, was counted as %d dial(s) and %d held handshake attempt(s)", shared, relay, dials, attempts)
	}
	select {
	case <-held:
		t.Fatalf("a client dialing %s, the port of the hold's relay at %s, closed the channel Hold returned", shared, relay)
	default:
	}
	if nc, err := ConnectSeed(t, "nats://"+relay, ""); err == nil {
		nc.Close()
		t.Fatalf("control: a client at the hold's relay %s connected while the hold kept the server's answer to its CONNECT", relay)
	}
	if dials, attempts := hold.Dials(), hold.Attempts(); dials != 1 || attempts != 1 {
		t.Fatalf("control: a client at the hold's relay %s was counted as %d dial(s) and %d held handshake attempt(s), want 1 of each, so the counts cannot be used to check isolation", relay, dials, attempts)
	}
	select {
	case <-held:
	default:
		t.Fatalf("control: a client at the hold's relay %s left open the channel Hold returned", relay)
	}
}

// urlHost is the host and port of the NATS URL uri.
func urlHost(t *testing.T, uri string) string {
	t.Helper()
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Host == "" {
		t.Fatalf("parse the NATS URL %q: %v", uri, err)
	}
	return parsed.Host
}
