package bus

import (
	"net"
	"net/url"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testsocks5"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A connection through the SOCKS5 proxy keeps the TCP socket under the net.Conn the proxy's dialer
// returns, so a sever resets that socket: the proxy's net.Conn wraps it and has no linger to set.
// The negative control is a socks5Proxy whose forward dialer records nothing: the dialer then keeps
// the proxy's own net.Conn, and the row fails.
func TestASeverableDialThroughTheProxyKeepsItsTCPSocket(t *testing.T) {
	parsed, err := url.Parse(testnats.URL(t))
	if err != nil {
		t.Fatalf("parse the server's URL: %v", err)
	}
	proxy := testsocks5.Start(t, parsed.Host)
	proxied, err := socks5Proxy(proxiedLookup(proxy.Addr))
	if err != nil {
		t.Fatalf("read the proxy setting: %v", err)
	}
	dialer := newSeverableDialer(proxied.DialContext)
	conn, err := dialer.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatalf("dial the server through the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	kept := conn.(*severableConn)
	if _, ok := kept.socket.(*net.TCPConn); !ok {
		t.Fatalf("the dialer keeps a %T as the proxied connection's socket, want the *net.TCPConn under it", kept.socket)
	}
	if kept.socket == kept.Conn {
		t.Fatal("control: the proxy's dialer returned the TCP socket itself, so the row reads nothing it unwraps")
	}
}

// A severed connection sends nothing it had not sent, without waiting for nats.go's lock, and the
// client goes on with a connection of its own.
//
// The row holds a write on the client's link (HoldStalledWrite), which holds nats.go's connection
// lock on a connection that stays connected, and begins a publish of a marker on that connection,
// which waits for the lock behind the write. Two subtests:
//   - the link moves (the control): the publish answers nil, and the marker reaches a subscriber of
//     the row's own.
//   - severed while the publish waits: Sever returns while the write still holds its socket, and
//     the waiting publish answers. Once the link moves, the client dials a connection of its own,
//     and a second marker it publishes there reaches the subscriber, which has received no first
//     marker by then.
//
// A Sever that only closed the connection would wait for nats.go's lock behind the held write, and
// the guard on Sever's answer fails it; one that reset no socket would let the first marker go out
// once the link moves.
func TestASeveredConnectionSendsNothingAPublishWaitedBehindAHeldWrite(t *testing.T) {
	testnats.RequireSocketQueues(t)
	const subject = "testbus.sever.marker"
	start := func(t *testing.T) (*Client, *testnats.HandshakeHold, *nats.Conn, chan *nats.Msg, chan error, func()) {
		t.Helper()
		client, hold := heldHandshakeClient(t)
		subscriber := testnats.Connect(t, testnats.URL(t))
		received := make(chan *nats.Msg, 16)
		if _, err := subscriber.ChanSubscribe(subject, received); err != nil {
			t.Fatalf("subscribe to the marker: %v", err)
		}
		if err := subscriber.Flush(); err != nil {
			t.Fatalf("flush the marker's subscription: %v", err)
		}
		conn := client.Connection()
		thaw := hold.HoldStalledWrite(t, conn)
		published := make(chan error, 1)
		go func() { published <- conn.Publish(subject, []byte("before")) }()
		testnats.RequireLockHeld(t, conn)
		select {
		case err := <-published:
			t.Fatalf("control: the marker's publish answered %v while a write held nats.go's lock", err)
		default:
		}
		return client, hold, conn, received, published, thaw
	}

	t.Run("the link moves", func(t *testing.T) {
		_, _, _, received, published, thaw := start(t)
		thaw()
		if err := testwait.Receive(t, "control: the marker's publish to answer once the link moved", published); err != nil {
			t.Fatalf("control: the marker's publish answered %v once the link moved", err)
		}
		if msg := testwait.Receive(t, "control: the marker to reach the subscriber once the link moved", received); string(msg.Data) != "before" {
			t.Fatalf("control: the subscriber received %q, want the marker", msg.Data)
		}
	})

	t.Run("severed while the publish waits", func(t *testing.T) {
		client, _, conn, received, published, thaw := start(t)
		severed := make(chan struct{})
		go func() {
			client.Sever()
			close(severed)
		}()
		// A Sever that waits for nats.go's lock waits until the write is thawed, which only the
		// test's end does (HoldStalledWrite's cleanup), so the wait names it.
		testwait.Receive(t, "Sever to return while a write holds nats.go's lock", severed)
		testwait.Receive(t, "the marker's publish to answer after the sever", published)
		if !conn.IsClosed() {
			t.Fatalf("the severed connection is %v, want CLOSED", conn.Status())
		}
		thaw()
		testwait.Until(t, "the client's connection of its own after the sever", func() (bool, bool) {
			own := client.Connected() && client.Connection() != conn
			return own, own
		})
		replacement := client.Connection()
		if err := replacement.Publish(subject, []byte("after")); err != nil {
			t.Fatalf("publish the second marker on the replacement connection: %v", err)
		}
		if err := replacement.Flush(); err != nil {
			t.Fatalf("flush the replacement connection: %v", err)
		}
		if msg := testwait.Receive(t, "control: the second marker to reach the subscriber", received); string(msg.Data) != "after" {
			t.Fatalf("the subscriber received %q before the second marker: the severed connection sent the publish that waited behind the held write", msg.Data)
		}
	})
}
