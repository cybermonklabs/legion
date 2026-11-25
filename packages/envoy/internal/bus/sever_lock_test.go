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

// Sever does not wait for the client's own lock. A caller can hold that lock while it waits for
// nats.go's: ResendRefusedCore reads its connection's status under it, and Close closes the
// connection under it. Connected does not: it reads the status with the client's lock released. A
// Sever that took the client's lock to find the connection would wait behind such a caller for as
// long as a write holds nats.go's lock, and once the link moves, the publish waiting behind the
// write would go out before the sever. Two subtests: with no such caller (the control), and with a
// ResendRefusedCore left waiting for nats.go's lock, which the row waits for until the client's
// lock stays taken. Each requires that Sever returns while the write still holds the lock, and that
// the publish that waited behind the write never reaches the server: once the client has a
// connection of its own again, a second marker published and flushed on it is the first the
// subscriber receives.
func TestASeverDoesNotWaitForACallerHoldingTheClientsLock(t *testing.T) {
	// Serial (busTestParallelism): it reads back something the whole process shares, every
	// goroutine's stack, which HoldStalledWrite reads to find its stalled write.
	testnats.RequireSocketQueues(t)
	const subject = "testbus.sever.locked-marker"
	for _, withRead := range []bool{false, true} {
		name := "no read of the connection waits"
		if withRead {
			name = "a read holding the client's lock waits for nats.go's lock"
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
			thaw := hold.HoldStalledWrite(t, conn)
			go func() { _ = conn.Publish(subject, []byte("before")) }()
			testnats.RequireLockHeld(t, conn)
			if withRead {
				read := make(chan struct{})
				go func() {
					_ = client.ResendRefusedCore()
					close(read)
				}()
				// The read holds the client's lock while it waits for nats.go's. The row polls until
				// the lock stays taken across three polls, which no caller that holds it only briefly
				// does, so Sever runs once the read holds it.
				taken := 0
				testwait.WithinPolling(t, 10*time.Second, 5*time.Millisecond, "the read holding the client's lock", func() bool {
					select {
					case <-read:
						t.Fatal("control: the read under the client's lock answered while a write held nats.go's lock")
					default:
					}
					if client.mu.TryLock() {
						client.mu.Unlock()
						taken = 0
						return false
					}
					taken++
					return taken >= 3
				})
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
			testwait.WithinPolling(t, 30*time.Second, 50*time.Millisecond, "the client's connection of its own after the sever", func() bool {
				return client.Connected() && client.Connection() != conn
			})
			replacement := client.Connection()
			if err := replacement.Publish(subject, []byte("after")); err != nil {
				t.Fatalf("publish the second marker on the replacement connection: %v", err)
			}
			if err := replacement.Flush(); err != nil {
				t.Fatalf("flush the replacement connection: %v", err)
			}
			select {
			case msg := <-received:
				if string(msg.Data) != "after" {
					t.Fatalf("the subscriber received %q before the second marker: the severed connection sent the publish that waited behind the held write", msg.Data)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("control: the second marker did not reach the subscriber within 30 s")
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
