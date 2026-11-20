package sessionproxy

// What the database's own end of a connection does to the proxy's Fault, the record a child's exit
// is read by (launcher's wait): a connection's dial the database refused or never answered, and a
// served connection it ended with no word or with a FATAL error about its own state, are faults
// until a dial that starts later reaches it.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
)

// holdEach has every dial from now on wait d before it dials, past any wait a read deadline of a
// test would give it.
func (f *fakeDialer) holdEach(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hold = d
}

// dialAt points every dial from now on at addr.
func (f *fakeDialer) dialAt(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addr = addr
}

// heldDial is a dial holdNext holds: entered closes once it has begun, and it dials addr once
// release closes.
type heldDial struct {
	addr             string
	entered, release chan struct{}
}

// holdNext holds the next dial until the returned dial's release closes, then has it dial addr.
func (f *fakeDialer) holdNext(addr string) *heldDial {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = &heldDial{addr: addr, entered: make(chan struct{}), release: make(chan struct{})}
	return f.held
}

// A connection's own dial the database refused, after a start whose proof dial it answered, is
// the proxy's Fault, ErrUnreachable, until a later connection's dial reaches the database; a dial
// refused over anything else, a sign-in the instance refused, is no fault of the store's. Each
// dial is held past any wait a read deadline would give it, and each look at Fault waits for the
// proxy to have finished with that dial: a refused one by the close of its client, which serve
// makes after it records, and one that reached the database by bytes that crossed the proxy.
func TestAConnectionsDialTheDatabaseRefusedIsTheProxysFaultUntilOneReachesIt(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)
	defer proxy.Close()
	dialer.holdEach(750 * time.Millisecond)
	if err := proxy.Fault(); err != nil {
		t.Fatalf("Fault before any connection = %v, want none", err)
	}

	dialer.dialAt(testdb.RefusedAddress(t))
	awaitClosed(t, dialProxy(t, socket), "a connection whose dial the database refused")
	if err := proxy.Fault(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Fault after a connection whose dial the database refused = %v, want ErrUnreachable", err)
	}

	dialer.dialAt(up.listener.Addr().String())
	client := dialProxy(t, socket)
	writeAll(t, client, "ping")
	if got := readString(t, up.next(t), 4); got != "ping" {
		t.Fatalf("the upstream read %q of the client's ping", got)
	}
	if err := proxy.Fault(); err != nil {
		t.Fatalf("Fault after a connection whose dial reached the database = %v, want none", err)
	}

	dialer.failWith(errors.New("the instance refused the login"))
	awaitClosed(t, dialProxy(t, socket), "a connection whose sign-in the instance refused")
	if err := proxy.Fault(); err != nil {
		t.Fatalf("Fault after a dial refused over the sign-in = %v, want none: the database answered", err)
	}
}

// Fault is the latest event's, each dial's as of its start: a slow dial that fails after a later
// connection's dial reached the database leaves no fault while that later connection is served,
// and a dial that starts after the later one and fails is the fault.
func TestAFaultIsOfTheLatestDialToStartNotTheLatestToEnd(t *testing.T) {
	up := newUpstream(t)
	dialer := &fakeDialer{addr: up.listener.Addr().String()}
	socket := socketPath(t)
	proxy := startProxy(context.Background(), t, up, dialer, socket)
	defer proxy.Close()

	held := dialer.holdNext(testdb.RefusedAddress(t))
	slow := dialProxy(t, socket)
	<-held.entered
	served := dialProxy(t, socket)
	writeAll(t, served, "ping")
	if got := readString(t, up.next(t), 4); got != "ping" {
		t.Fatalf("the upstream read %q of the later client's ping", got)
	}
	close(held.release)
	awaitClosed(t, slow, "the connection whose slow dial the database refused")
	if err := proxy.Fault(); err != nil {
		t.Fatalf("Fault = %v while the later connection, whose dial reached the database, is served; want none", err)
	}

	dialer.dialAt(testdb.RefusedAddress(t))
	awaitClosed(t, dialProxy(t, socket), "a connection whose dial started last and the database refused")
	if err := proxy.Fault(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Fault after the latest dial was refused = %v, want ErrUnreachable", err)
	}
}

// A served connection the database ends with no word of its own, by a reset or by a close (an end
// of file), is the proxy's Fault, as a dial it refused is, until a dial that starts later reaches
// it: a child holding that connection exited on a store that stopped answering it. So is one it
// ends with a FATAL error about its own state (57P03, starting up), recorded as the error crosses.
// One it ends with a FATAL error about the connection's configuration (28P01, a password it
// refuses) is not, nor is the end of a session the client ended in order (Terminate), the end of a
// cancel request, which the database answers by ending it, one the proxy itself closes once its
// drain is over, or one whose client closed it with no Terminate, as a child that exits does: the
// proxy then closes the upstream itself, and its own read meets that close (net.ErrClosed).
func TestAServedConnectionTheDatabaseEndsWithNoWordOrOverItsOwnStateIsTheProxysFault(t *testing.T) {
	for name, tc := range map[string]struct {
		// sent is what the client sends, which the database reads whole before end ends it; with
		// clientEnds the client closes the connection itself instead.
		sent       []byte
		end        func(t *testing.T, served net.Conn, proxy *Proxy)
		clientEnds bool
		fault      bool
	}{
		"the database resets it": {sent: []byte("ping"), fault: true, end: func(t *testing.T, served net.Conn, _ *Proxy) {
			_ = served.(*net.TCPConn).SetLinger(0)
			_ = served.Close()
		}},
		"the database closes it with no word (an end of file)": {sent: startupMessage(), fault: true, end: func(t *testing.T, served net.Conn, _ *Proxy) {
			_ = served.Close()
		}},
		"the database ends it starting up (FATAL 57P03)": {sent: startupMessage(), fault: true, end: func(t *testing.T, served net.Conn, _ *Proxy) {
			writeAll(t, served, string(errorResponse("FATAL", "57P03")))
			_ = served.Close()
		}},
		"control: the database refuses the password (FATAL 28P01)": {sent: startupMessage(), end: func(t *testing.T, served net.Conn, _ *Proxy) {
			writeAll(t, served, string(errorResponse("FATAL", "28P01")))
			_ = served.Close()
		}},
		"control: the client ends its session in order (Terminate)": {sent: append(startupMessage(), 'X', 0, 0, 0, 4), end: func(t *testing.T, served net.Conn, _ *Proxy) {
			_ = served.Close()
		}},
		"control: the database ends a cancel request": {sent: cancelRequestMessage(), end: func(t *testing.T, served net.Conn, _ *Proxy) {
			_ = served.Close()
		}},
		"control: the proxy closes it once its drain is over": {sent: []byte("ping"), end: func(t *testing.T, _ net.Conn, proxy *Proxy) {
			_ = proxy.Close()
		}},
		"control: the client closes it with no Terminate": {sent: startupMessage(), clientEnds: true},
	} {
		t.Run(name, func(t *testing.T) {
			up := newUpstream(t)
			dialer := &fakeDialer{addr: up.listener.Addr().String()}
			socket := socketPath(t)
			proxy := startProxy(context.Background(), t, up, dialer, socket)
			defer proxy.Close()
			client := dialProxy(t, socket)
			writeAll(t, client, string(tc.sent))
			served := up.next(t)
			if got := readString(t, served, len(tc.sent)); got != string(tc.sent) {
				t.Fatalf("the upstream read %q of the client's %q", got, tc.sent)
			}
			if tc.clientEnds {
				_ = client.Close()
				awaitUntracked(t, proxy)
			} else {
				tc.end(t, served, proxy)
				awaitClosed(t, client, "the client of a connection whose upstream ended")
			}
			if got := proxy.Fault(); errors.Is(got, ErrUnreachable) != tc.fault {
				t.Fatalf("Fault after the served connection ended = %v, want ErrUnreachable %t", got, tc.fault)
			}
			if !tc.fault {
				return
			}
			client = dialProxy(t, socket)
			writeAll(t, client, "ping")
			if got := readString(t, up.next(t), 4); got != "ping" {
				t.Fatalf("the upstream read %q of the next client's ping", got)
			}
			if err := proxy.Fault(); err != nil {
				t.Fatalf("Fault after a later dial reached the database = %v, want none", err)
			}
		})
	}
}

// The proxy reads a FATAL error's SQLSTATE however its bytes arrive, here one at a time, after an
// SSL request the database declined. After one the database accepted nothing more can be read, and
// an ERROR, which ends no connection, is not read as the database's word on an end.
func TestTheProxyReadsAFatalErrorAcrossReadsAfterADeclinedEncryption(t *testing.T) {
	ssl := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), sslRequest)
	for name, tc := range map[string]struct {
		answer   byte
		severity string
		fatal    bool
	}{
		"the database declines the encryption (N), then ends the connection starting up (FATAL 57P03)": {answer: 'N', severity: "FATAL", fatal: true},
		"control: the database accepts the encryption (S), and nothing after it is read":               {answer: 'S', severity: "FATAL"},
		"control: the database declines the encryption, then answers an ERROR":                         {answer: 'N', severity: "ERROR"},
	} {
		t.Run(name, func(t *testing.T) {
			var w wire
			w.fromClient(ssl)
			code, fatal := w.fromServer([]byte{tc.answer})
			w.fromClient(startupMessage())
			for _, b := range errorResponse(tc.severity, "57P03") {
				if c, f := w.fromServer([]byte{b}); f {
					code, fatal = c, f
				}
			}
			if fatal != tc.fatal || fatal && code != "57P03" {
				t.Fatalf("the error read as SQLSTATE %q, FATAL %t; want FATAL %t, 57P03 when FATAL", code, fatal, tc.fatal)
			}
		})
	}
}

// startupMessage is a protocol 3.0 startup message, as a client opens a session with it.
func startupMessage() []byte {
	body := append(binary.BigEndian.AppendUint32(nil, 3<<16), "user\x00legion\x00database\x00sessions\x00\x00"...)
	return append(binary.BigEndian.AppendUint32(nil, uint32(4+len(body))), body...)
}

// cancelRequestMessage is a cancel request, which a client sends on a connection of its own.
func cancelRequestMessage() []byte {
	message := binary.BigEndian.AppendUint32(nil, 16)
	message = binary.BigEndian.AppendUint32(message, cancelRequest)
	message = binary.BigEndian.AppendUint32(message, 4242)
	return binary.BigEndian.AppendUint32(message, 2424)
}

// errorResponse is an error a Postgres server sends, of severity and SQLSTATE code.
func errorResponse(severity, code string) []byte {
	fields := "S" + severity + "\x00V" + severity + "\x00C" + code + "\x00Mthe test's error\x00\x00"
	return append(binary.BigEndian.AppendUint32([]byte{'E'}, uint32(4+len(fields))), fields...)
}

// dialProxy is a client of the proxy at socket, closed when the test ends.
func dialProxy(t *testing.T, socket string) net.Conn {
	t.Helper()
	client, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// awaitClosed waits for the proxy to close client, which serve does once it is done with that
// connection: after it recorded a failed dial, or after both copies of a served connection ended.
// What the database sent before it ended the connection is read and dropped. A proxy that does not
// close it within a bound no dial of a test takes fails the test by name.
func awaitClosed(t *testing.T, client net.Conn, what string) {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	if _, err := io.Copy(io.Discard, client); err != nil {
		t.Fatalf("the proxy did not close %s: the read answered %v", what, err)
	}
}

// awaitUntracked waits until the proxy serves no connection, both copies of each having ended, so
// whatever the end of one recorded is recorded; it fails the test by name past ten seconds.
func awaitUntracked(t *testing.T, proxy *Proxy) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		proxy.connsMu.Lock()
		served := len(proxy.conns)
		proxy.connsMu.Unlock()
		if served == 0 {
			return
		}
	}
	t.Fatal("the proxy still served a connection ten seconds after its client closed it")
}
