package bus

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Sever does not wait for the client's own lock. A caller of Client.Connected holds that lock
// while it reads nats.go's status, which waits for nats.go's lock: an intake's stop leaves such
// reads running (its drain wait and mayHandBack) once a write holds nats.go's lock. A Sever that took
// the client's lock to find the connection waited behind that read for as long as the write held,
// and once the link moved, the publish that waited behind the write went out before the sever.
// Two subtests: with no such read (the control), and with one left waiting for nats.go's lock.
// Each requires that Sever returns while the write still holds the lock, and that the publish that
// waited behind the write never reaches the server.
func TestASeverDoesNotWaitForACallerHoldingTheClientsLock(t *testing.T) {
	const subject = "testbus.sever.locked-marker"
	for _, withRead := range []bool{false, true} {
		name := "no read of the connection waits"
		if withRead {
			name = "a Connected read waits for nats.go's lock"
		}
		t.Run(name, func(t *testing.T) {
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
			thaw := hold.HoldWriteLock(t, conn)
			go func() { _ = conn.Publish(subject, []byte("before")) }()
			testnats.RequireLockHeld(t, conn)
			if withRead {
				read := make(chan struct{})
				go func() {
					client.Connected()
					close(read)
				}()
				select {
				case <-read:
					t.Fatal("control: Connected answered while a write held nats.go's lock")
				case <-time.After(500 * time.Millisecond):
				}
			}
			severed := make(chan struct{})
			go func() {
				client.Sever()
				close(severed)
			}()
			select {
			case <-severed:
			case <-time.After(10 * time.Second):
				thaw()
				t.Fatal("Sever did not return within 10 s while the write held nats.go's lock: it waited for the client's lock")
			}
			thaw()
			// Once the link moves, a marker the severed connection still held would reach the
			// subscriber within a round trip; 3 s is the guard.
			select {
			case msg := <-received:
				t.Fatalf("the severed connection sent %q", msg.Data)
			case <-time.After(3 * time.Second):
			}
		})
	}
}

// A socket nats.go drops without closing it -- as it drops the socket of a reconnect whose
// handshake failed, which a refused credential makes on every attempt -- is not kept open by the
// bus's dialer: the garbage collector's finalizer closes it, and its peer reads the end of the
// stream. A dialer that kept every socket until nats.go closed it kept these for the life of the
// process, one descriptor per refused attempt.
func TestASeverableDialerKeepsNoSocketItsConnectionDropped(t *testing.T) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	const dropped = 8
	accepted := make(chan net.Conn, dropped)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()
	dialer := newSeverableDialer((&net.Dialer{}).DialContext)
	dialAndDrop := func() {
		if _, err := dialer.Dial("tcp", listener.Addr().String()); err != nil {
			t.Fatalf("dial: %v", err)
		}
	}
	for range dropped {
		dialAndDrop()
	}
	peers := make([]net.Conn, 0, dropped)
	for range dropped {
		select {
		case conn := <-accepted:
			t.Cleanup(func() { _ = conn.Close() })
			peers = append(peers, conn)
		case <-time.After(30 * time.Second):
			t.Fatal("control: the listener did not accept every dial")
		}
	}
	ended := make([]bool, dropped)
	testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "every dropped socket closed by the garbage collector", func() bool {
		runtime.GC()
		all := true
		for i, peer := range peers {
			if ended[i] {
				continue
			}
			_ = peer.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			_, err := peer.Read(make([]byte, 1))
			switch {
			case errors.Is(err, io.EOF):
				ended[i] = true
			case errors.Is(err, os.ErrDeadlineExceeded):
				all = false
			default:
				t.Fatalf("read the peer of a dropped socket: %v", err)
			}
		}
		return all
	})
	// The connection keeps its dialer for its whole life (nats.Options.CustomDialer), so the row
	// keeps it too: a dialer the collector frees takes everything it holds with it.
	runtime.KeepAlive(dialer)
}
