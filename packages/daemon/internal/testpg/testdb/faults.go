package testdb

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// RefusedAddress is a TCP address on 127.0.0.1 that the test holds bound and never listens on, so
// a dial of it is refused by the host, as a database that is down refuses one, until the test
// ends. Holding the port is what keeps the refusal: a port that was free and is let go can be
// bound by any other process on the machine before the dial, which then answers it.
func RefusedAddress(t testing.TB) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("make a socket to hold a refused port: %v", err)
	}
	syscall.CloseOnExec(fd)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatalf("bind a port on 127.0.0.1 to refuse: %v", err)
	}
	bound, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatalf("read the refused port: %v", err)
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.(*syscall.SockaddrInet4).Port))
}

// RefusedDial is the error a real dial of RefusedAddress gives, for a stand-in dialer that answers
// as a connector does for a database whose host refuses it.
func RefusedDial(t testing.TB) error {
	t.Helper()
	conn, err := net.Dial("tcp", RefusedAddress(t))
	if err == nil {
		_ = conn.Close()
		t.Fatal("a dial of an address nothing listens on was answered")
	}
	return err
}

// Refusing is the address of a stand-in for a Postgres server that answers every connection's
// startup with a FATAL ErrorResponse of SQLSTATE code and message, then closes it, as a real server
// refuses a connection it cannot take: one starting up or shutting down (57P03), one with no client
// slot left (53300), or a login it does not take (28P01). It speaks nothing of the protocol past
// that first answer (startupStandIn).
func Refusing(t testing.TB, code, message string) string {
	t.Helper()
	fields := []byte("SFATAL\x00VFATAL\x00C" + code + "\x00M" + message + "\x00\x00")
	answer := binary.BigEndian.AppendUint32([]byte{'E'}, uint32(4+len(fields)))
	answer = append(answer, fields...)
	return startupStandIn(t, func(conn net.Conn) { _, _ = conn.Write(answer) })
}

// Ending is the address of a stand-in for a Postgres server that ends every connection once it has
// read its startup, before any answer: with reset, by a reset (RST), as a server that crashes or
// restarts, or a TCP balancer in front of a store that is down, ends it; otherwise by a close (an
// end of file), as a server that hangs up ends it (startupStandIn).
func Ending(t testing.TB, reset bool) string {
	t.Helper()
	return startupStandIn(t, func(conn net.Conn) {
		if tcp, ok := conn.(*net.TCPConn); ok && reset {
			_ = tcp.SetLinger(0)
		}
	})
}

// startupStandIn is the address of a stand-in for a Postgres server on 127.0.0.1 that reads each
// connection's startup message, declining an SSL or GSS encryption request before it as a server
// without TLS declines it, then does end to the connection and closes it. Its listener closes when
// the test ends.
func startupStandIn(t testing.TB, end func(conn net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				for {
					var size [4]byte
					if _, err := io.ReadFull(conn, size[:]); err != nil || binary.BigEndian.Uint32(size[:]) < 8 {
						return
					}
					body := make([]byte, binary.BigEndian.Uint32(size[:])-4)
					if _, err := io.ReadFull(conn, body); err != nil {
						return
					}
					// 80877103 is an SSLRequest's code and 80877104 a GSSENCRequest's; the startup
					// message comes next.
					if request := binary.BigEndian.Uint32(body); len(body) == 4 && (request == 80877103 || request == 80877104) {
						if _, err := conn.Write([]byte{'N'}); err != nil {
							return
						}
						continue
					}
					end(conn)
					return
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}

// Breaking is dsn reached through a TCP proxy on 127.0.0.1 that ends the first connection whose
// client sends marker, at that moment, as a session store that restarts or fails over ends the
// connections it holds: the client's end is reset (RST) when reset is set, and otherwise closed as
// a server that hangs up closes it (an end of file). What the client sent with marker never reaches
// the server. Every other connection, and every one after the one it ended, passes through as it
// is, so the store answers again at once. The proxy and every connection through it close when the
// test ends, before the database is dropped (Database's cleanup is registered first, so it runs
// after this one). The URL it answers is rewriteDSN's, as Delayed's is, for the reason Delayed
// gives.
func Breaking(t testing.TB, dsn, marker string, reset bool) string {
	t.Helper()
	through, upstream := relayFor(t, "the database URL to break", dsn)
	var broken atomic.Bool
	startRelay(t, through, func() string { return upstream }, func(client, server net.Conn) {
		relayUntil(client, server, []byte(marker), reset, &broken)
	})
	return through.url
}

// relayUntil copies client to server until the client sends marker on the first connection to send
// it, which broken marks, and ends that connection there instead. A marker split between two reads
// is still seen: each read is searched together with the end of the one before it.
func relayUntil(client, server net.Conn, marker []byte, reset bool, broken *atomic.Bool) {
	buffer := make([]byte, 64<<10)
	var tail []byte
	for {
		n, err := client.Read(buffer)
		if n > 0 {
			seen := append(tail, buffer[:n]...)
			if bytes.Contains(seen, marker) && broken.CompareAndSwap(false, true) {
				if tcp, ok := client.(*net.TCPConn); ok && reset {
					_ = tcp.SetLinger(0)
				}
				return
			}
			if _, err := server.Write(buffer[:n]); err != nil {
				return
			}
			tail = bytes.Clone(seen[max(0, len(seen)-len(marker)+1):])
		}
		if err != nil {
			return
		}
	}
}

// Flapping is a session store that goes down and comes back while a test runs: the database at a
// DSN reached through a TCP proxy on 127.0.0.1 (NewFlapping) that sends every connection it accepts
// while Down is set to a stand-in that answers it is starting up (57P03, Refusing), as a store does
// while it restarts, and counts it in Refused; every other connection reaches the database and is
// counted in Passed. Whether a connection is refused is decided as it is accepted, so a connection
// counted in Passed is served by the database however Down changes after.
type Flapping struct {
	// URL is the DSN that reaches the database through the proxy: rewriteDSN's, as Delayed's is.
	URL             string
	Down            atomic.Bool
	Refused, Passed atomic.Int32
}

// NewFlapping is dsn's database behind a Flapping proxy, down from the start when down is set. The
// proxy and every connection through it close when the test ends, as Delayed's do.
func NewFlapping(t testing.TB, dsn string, down bool) *Flapping {
	t.Helper()
	through, database := relayFor(t, "the database URL to flap", dsn)
	starting := Refusing(t, "57P03", "the database system is starting up")
	s := &Flapping{URL: through.url}
	s.Down.Store(down)
	startRelay(t, through, func() string {
		if s.Down.Load() {
			s.Refused.Add(1)
			return starting
		}
		s.Passed.Add(1)
		return database
	}, relayPlain)
	return s
}
