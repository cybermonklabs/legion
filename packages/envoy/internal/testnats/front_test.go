package testnats

import (
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

// greeter is a TCP server on a loopback port of its own that writes its name to each connection it
// accepts and then echoes what the connection sends, and hands the test its side of each one.
type greeter struct {
	name     string
	listener net.Listener
	accepted chan net.Conn
}

func startGreeter(t *testing.T, name string) *greeter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for %s: %v", name, err)
	}
	g := &greeter{name: name, listener: listener, accepted: make(chan net.Conn, 8)}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			g.accepted <- conn
			go func() {
				defer conn.Close()
				if _, err := conn.Write([]byte(name)); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return g
}

// readFor reads exactly len(want) bytes from conn within 2 s and fails the test unless they are
// want.
func readFor(t *testing.T, conn net.Conn, want, what string) {
	t.Helper()
	got := make([]byte, len(want))
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != want {
		t.Fatalf("%s: read %q, %v; want %q", what, got, err, want)
	}
}

// readsEOF fails the test unless conn ends within 2 s with nothing more to read.
func readsEOF(t *testing.T, conn net.Conn, what string) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("%s: read %d bytes, %v; want the connection closed (EOF)", what, n, err)
	}
}

// A front holds one address for the test's life and relays each connection to wherever its target
// says the server listens as the connection arrives: a connection made while the target names no
// server is closed at once; one made after the target moves reaches the new server, while one the
// front relays already keeps its server until that server drops it, and then the client's side is
// closed too.
func TestAFrontRelaysToWhereverItsServerListensAndClosesWhileItListensNowhere(t *testing.T) {
	var mu sync.Mutex
	address := ""
	front := StartFront(t, func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if address == "" {
			return "", errors.New("the server is not listening")
		}
		return address, nil
	})
	point := func(to string) {
		mu.Lock()
		address = to
		mu.Unlock()
	}
	dial := func() net.Conn { return dialFront(t, front) }

	readsEOF(t, dial(), "a connection while the front names no server")

	first := startGreeter(t, "first")
	point(first.listener.Addr().String())
	kept := dial()
	readFor(t, kept, "first", "a connection once the front names the first server")
	firstSide := <-first.accepted

	second := startGreeter(t, "second")
	point(second.listener.Addr().String())
	readFor(t, dial(), "second", "a connection once the front names the second server")
	if _, err := kept.Write([]byte("still")); err != nil {
		t.Fatalf("write on the connection relayed to the first server: %v", err)
	}
	readFor(t, kept, "still", "the connection relayed to the first server, after the front moved")

	_ = firstSide.Close()
	readsEOF(t, kept, "the connection relayed to the first server, after that server dropped it")

	point("")
	readsEOF(t, dial(), "a connection once the front names no server again")
}

// dialFront connects to the front's address within a second, for the test's life.
func dialFront(t *testing.T, front *Front) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", front.Addr(), time.Second)
	if err != nil {
		t.Fatalf("dial the front at %s: %v", front.Addr(), err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A connection whose target the front read just before its caller stopped the target and then the
// server is relayed nowhere: another program that binds the port the server released in the
// meantime gets no byte of the client's. The target below makes that whole stop, and the other
// program's bind of the released port, between its read and the front's dial.
func TestAFrontRelaysNothingToThePortItsServerReleasedAfterItReadTheTarget(t *testing.T) {
	server := startGreeter(t, "server")
	released := server.listener.Addr().String()
	var mu sync.Mutex
	stopped := false
	var rival net.Listener
	var rivalErr error
	front := StartFront(t, func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return "", errors.New("the server is not listening")
		}
		stopped = true
		_ = server.listener.Close()
		rival, rivalErr = net.Listen("tcp", released)
		return released, nil
	})
	conn := dialFront(t, front)
	if _, err := conn.Write([]byte("for the server")); err != nil {
		t.Fatalf("write to the front: %v", err)
	}
	// The relay closes the client's connection with the client's bytes unread, so the kernel ends
	// it with a reset rather than an EOF; either is a connection closed with nothing relayed.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := conn.Read(make([]byte, 1)); n != 0 || !(errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		t.Fatalf("a connection whose target was read before the server released its port: read %d bytes, %v; want the connection closed", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if rivalErr != nil {
		t.Fatalf("the other program could not bind the released port %s: %v", released, rivalErr)
	}
	t.Cleanup(func() { _ = rival.Close() })
	// The front's dial reached the other program's port, so its connection waits in the backlog;
	// it must end with no byte of the client's in it.
	if err := rival.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	side, err := rival.Accept()
	if err != nil {
		t.Fatalf("the other program accepted no connection of the front's dial: %v", err)
	}
	t.Cleanup(func() { _ = side.Close() })
	readsEOF(t, side, "the other program's side of the front's dial")
}
