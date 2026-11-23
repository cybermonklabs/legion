package testnats

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testport"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A handshake hold finds the INFO and CONNECT lines across TCP reads: a client whose CONNECT
// arrives in two reads, split inside the word, still marks the hold held (protocolStream), and the
// server's answer after its INFO line waits for Release. The row's server is a plain TCP listener,
// so the hold relays exactly the bytes the row writes. It decides by the hold's events: the hold
// reports the answer waiting once it has read the answer and before it writes it
// (HandshakeHold.waiting), so the client has read nothing at that moment, and the answer reaches
// the client once the row calls Release.
//
// The control is the hold matching "CONNECT " inside one read: the channel Hold returns never
// closes.
func TestAHandshakeHoldFindsItsLinesAcrossReads(t *testing.T) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	hold := StartHandshakeHold(t, "nats://"+listener.Addr().String())
	t.Cleanup(hold.Release)
	held := hold.Hold()
	address, err := url.Parse(hold.URL())
	if err != nil {
		t.Fatalf("parse the hold's URL %q: %v", hold.URL(), err)
	}
	client, err := net.Dial("tcp", address.Host)
	if err != nil {
		t.Fatalf("dial the hold: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept the hold's dial: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	if _, err := server.Write([]byte("INFO {}\r\n")); err != nil {
		t.Fatalf("write INFO: %v", err)
	}
	read := bufio.NewReader(client)
	if line, err := read.ReadString('\n'); err != nil || line != "INFO {}\r\n" {
		t.Fatalf("the client read %q, %v, want the server's INFO line", line, err)
	}
	if _, err := client.Write([]byte("CON")); err != nil {
		t.Fatalf("write the CONNECT's first part: %v", err)
	}
	// The hold relays the first part before the rest is written, so its two reads are two: the
	// server reads "CON" first.
	first := make([]byte, 3)
	if _, err := io.ReadFull(server, first); err != nil || string(first) != "CON" {
		t.Fatalf("the server read %q, %v, want the CONNECT's first part alone", first, err)
	}
	if _, err := client.Write([]byte("NECT {}\r\n")); err != nil {
		t.Fatalf("write the CONNECT's rest: %v", err)
	}
	select {
	case <-held:
	case <-time.After(Remaining(t)):
		t.Fatal("a CONNECT split across two reads had not marked the hold held by the test's deadline")
	}
	if _, err := server.Write([]byte("+OK\r\n")); err != nil {
		t.Fatalf("write the answer: %v", err)
	}
	answered := make(chan string, 1)
	go func() {
		line, _ := read.ReadString('\n')
		answered <- line
	}()
	select {
	case <-hold.waiting:
	case <-time.After(Remaining(t)):
		t.Fatal("the server's answer never waited for the hold by the test's deadline")
	}
	select {
	case line := <-answered:
		t.Fatalf("the client read %q while the hold held the server's answer", line)
	default:
	}
	hold.Release()
	select {
	case line := <-answered:
		if line != "+OK\r\n" {
			t.Fatalf("the client read %q once the hold ended, want the server's answer", line)
		}
	case <-time.After(Remaining(t)):
		t.Fatal("the server's answer never reached the client after Release")
	}
}

// startHeldConnection makes one connection through hold to listener, relays the server's INFO and
// the client's CONNECT, and writes the server's answer, which the hold keeps: it waits for the
// hold's report that the answer waits. It returns the client's side.
func startHeldConnection(t *testing.T, hold *HandshakeHold, listener net.Listener) net.Conn {
	t.Helper()
	address, err := url.Parse(hold.URL())
	if err != nil {
		t.Fatalf("parse the hold's URL %q: %v", hold.URL(), err)
	}
	client, err := net.Dial("tcp", address.Host)
	if err != nil {
		t.Fatalf("dial the hold: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept the hold's dial: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	if _, err := server.Write([]byte("INFO {}\r\n")); err != nil {
		t.Fatalf("write INFO: %v", err)
	}
	if line, err := bufio.NewReader(client).ReadString('\n'); err != nil || line != "INFO {}\r\n" {
		t.Fatalf("the client read %q, %v, want the server's INFO line", line, err)
	}
	if _, err := client.Write([]byte("CONNECT {}\r\n")); err != nil {
		t.Fatalf("write the CONNECT: %v", err)
	}
	connect := make([]byte, len("CONNECT {}\r\n"))
	if _, err := io.ReadFull(server, connect); err != nil {
		t.Fatalf("the server read %q, %v, want the CONNECT", connect, err)
	}
	if _, err := server.Write([]byte("+OK\r\n")); err != nil {
		t.Fatalf("write the answer: %v", err)
	}
	select {
	case <-hold.waiting:
	case <-time.After(Remaining(t)):
		t.Fatal("the server's answer never waited for the hold by the test's deadline")
	}
	return client
}

// A handshake hold keeps each connection's answer until Release or that connection's own end,
// never until another's end: the first held connection's client closes, so its wait ends by its
// own end and Ended closes; a connection made after that is held all the same, and its wait ends
// only at Release. It decides by the hold's events: a wait that must block reports so before it
// blocks (HandshakeHold.waiting), and each blocked wait reports why it ended (HandshakeHold.passed).
//
// The control is each wait ending at Ended as well, the first held connection's end: the later
// connection's answer passes at once, never waits, and the row fails at the test's deadline.
func TestAHandshakeHoldHoldsEachConnectionUntilReleaseOrItsOwnEnd(t *testing.T) {
	listener, err := testport.ListenLoopback()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	hold := StartHandshakeHold(t, "nats://"+listener.Addr().String())
	t.Cleanup(hold.Release)
	hold.Hold()
	first := startHeldConnection(t, hold, listener)

	_ = first.Close()
	next := func(what string) string {
		t.Helper()
		select {
		case reason := <-hold.passed:
			return reason
		case <-time.After(Remaining(t)):
			t.Fatalf("%s: no held answer's wait ended by the test's deadline", what)
		}
		return ""
	}
	if reason := next("the first connection's end"); reason != "own end" {
		t.Fatalf("the first held answer's wait ended by %q, want its own connection's end", reason)
	}
	select {
	case <-hold.Ended():
	case <-time.After(Remaining(t)):
		t.Fatal("Ended never closed after the first held connection ended")
	}
	startHeldConnection(t, hold, listener)
	hold.Release()
	if reason := next("Release"); reason != "release" {
		t.Fatalf("the later held answer's wait ended by %q, want Release's release", reason)
	}
}

// HoldWriteLock returns holding nats.go's connection lock, which the lock's own state shows: a try
// to take it for reading fails. A publish begun under the hold returns once the release lets the
// lock go and reaches a subscriber, on a connection that stayed connected throughout. No reading
// of how long a call takes decides any of it. The negative controls: a HoldWriteLock that freezes
// the proxy and returns without the lock fails the try; a release that does not let the lock go
// fails the publish's return.
func TestHoldWriteLockReturnsHoldingTheConnectionLock(t *testing.T) {
	uri := URL(t)
	hold := StartHandshakeHold(t, uri)
	conn, disconnected, _ := proxiedClient(t, hold.Proxy)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	received := make(chan *natsgo.Msg, 1)
	if _, err := direct.ChanSubscribe("testnats.held", received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := direct.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	release := hold.HoldWriteLock(t, conn)
	lock, found := connLock(t, conn)
	if !found {
		t.FailNow()
	}
	if lock.TryRLock() {
		lock.RUnlock()
		t.Fatal("HoldWriteLock returned with nats.go's connection lock free: a read of it was taken at once")
	}
	published := make(chan error, 1)
	go func() { published <- conn.Publish("testnats.held", []byte("held")) }()
	release()
	if err := testwait.Receive(t, "the publish begun under the hold to return once the release let the lock go", published); err != nil {
		t.Fatalf("the publish begun under the hold answered %v once the release let the lock go, want nil", err)
	}
	if msg := testwait.Receive(t, "the publish begun under the hold at a subscriber", received); string(msg.Data) != "held" {
		t.Fatalf("the subscriber received %q, want the publish begun under the hold", msg.Data)
	}
	select {
	case <-disconnected:
		t.Fatal("the client was disconnected by the hold, which keeps its connection open")
	default:
	}
}

// HoldStalledWrite holds nats.go's connection lock with a write of nats.go's own, which a close of
// the connection's socket ends: once the row closes the socket the client dialed, with the proxy
// still frozen, nats.go lets the lock go and a read of the connection's status answers. The control
// is HoldWriteLock on the same rig: its lock is the row's own, so the read still waits after the
// close, which shows that the rig tells the two apart.
func TestHoldStalledWriteHoldsTheLockWithAWriteTheSocketsCloseEnds(t *testing.T) {
	for _, stalled := range []bool{true, false} {
		name := "HoldStalledWrite"
		if !stalled {
			name = "HoldWriteLock, the control"
		}
		t.Run(name, func(t *testing.T) {
			hold := StartHandshakeHold(t, URL(t))
			dialer := &socketDialer{}
			conn, err := natsgo.Connect(hold.URL(), natsgo.SetCustomDialer(dialer), natsgo.NoReconnect())
			if err != nil {
				t.Fatalf("connect through the hold: %v", err)
			}
			t.Cleanup(conn.Close)
			socket := dialer.last.Load()
			if socket == nil {
				t.Fatal("control: the client dialed no socket through the row's dialer")
			}
			var release func()
			if stalled {
				release = hold.HoldStalledWrite(t, conn)
			} else {
				release = hold.HoldWriteLock(t, conn)
			}
			t.Cleanup(release)
			RequireLockHeld(t, conn)
			_ = (*socket).Close()
			read := make(chan struct{})
			go func() {
				conn.Status()
				close(read)
			}()
			select {
			case <-read:
				if !stalled {
					t.Fatal("control: a read of the status answered once the socket closed while HoldWriteLock held the lock")
				}
			case <-time.After(10 * time.Second):
				if stalled {
					t.Fatal("a read of the status still waited 10 s after the socket closed: the lock HoldStalledWrite held was not the write's")
				}
			}
		})
	}
}

// socketDialer dials as nats.go's own dialer does and keeps the last socket it handed out.
type socketDialer struct {
	last atomic.Pointer[net.Conn]
}

func (d *socketDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: dialTimeout}).Dial(network, address)
	if err == nil {
		d.last.Store(&conn)
	}
	return conn, err
}

// holdChildEnv tells the re-executed test binary to run TestHoldWriteLockOnAHooksGoroutineChild,
// which skips in an ordinary run.
const holdChildEnv = "TESTNATS_HOLD_CHILD"

// hookWentOn is the line the child logs once a HoldWriteLock called on a goroutine of its own has
// returned there.
const hookWentOn = "the hook's goroutine went on after HoldWriteLock"

// HoldWriteLock called on a goroutine other than the test's, as the hand-back rows' hooks call it,
// fails the test by name and returns there when it cannot take the lock, so the hook goes on: once
// when its guard passes while another holder keeps the lock, and once when the binary's deadline is
// already no more than the guard's spare away. A t.Fatal there would end the hook's goroutine
// rather than the test, and the child would log that HoldWriteLock never returned. The child runs
// under -timeout 45s, so its guard passes about 15 s in.
func TestHoldWriteLockOnAHooksGoroutineFailsByNameAndReturns(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestHoldWriteLockOnAHooksGoroutineChild$", "-test.timeout=45s", "-test.v")
	child.Env = append(os.Environ(), holdChildEnv+"=1")
	output, err := child.CombinedOutput()
	text := string(output)
	if err == nil {
		t.Fatalf("the child was meant to fail both rows; it passed:\n%s", text)
	}
	if strings.Contains(text, "panic:") {
		t.Fatalf("the child panicked rather than failing its rows by name:\n%s", text)
	}
	for _, want := range []string{
		"another nats.go call still held the connection lock 30s before the test binary's deadline: nothing let it go",
		"--- FAIL: TestHoldWriteLockOnAHooksGoroutineChild/the_guard_passes (",
		"testwait: TestHoldWriteLockOnAHooksGoroutineChild/the_deadline_is_spent: the test binary's deadline leaves ",
		"--- FAIL: TestHoldWriteLockOnAHooksGoroutineChild/the_deadline_is_spent (",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the child's output does not say %q:\n%s", want, text)
		}
	}
	if went := strings.Count(text, hookWentOn); went != 2 {
		t.Errorf("the child logged %q %d time(s), want once for each row:\n%s", hookWentOn, went, text)
	}
}

// TestHoldWriteLockOnAHooksGoroutineChild is the child of
// TestHoldWriteLockOnAHooksGoroutineFailsByNameAndReturns. Its first row holds the connection's lock
// itself, so the guard of the HoldWriteLock its hook calls passes; that leaves the binary's deadline
// no more than the guard's spare away, so the second row's HoldWriteLock finds it spent. It runs only
// when the parent re-executes this binary.
func TestHoldWriteLockOnAHooksGoroutineChild(t *testing.T) {
	if os.Getenv(holdChildEnv) == "" {
		t.Skip(holdChildEnv + " is not set: this test is the child of TestHoldWriteLockOnAHooksGoroutineFailsByNameAndReturns")
	}
	uri := URL(t)
	t.Run("the guard passes", func(t *testing.T) {
		hold := StartHandshakeHold(t, uri)
		conn, _, _ := proxiedClient(t, hold.Proxy)
		lock, found := connLock(t, conn)
		if !found {
			t.FailNow()
		}
		lock.Lock()
		t.Cleanup(lock.Unlock)
		holdOnAHooksGoroutine(t, hold, conn)
	})
	t.Run("the deadline is spent", func(t *testing.T) {
		hold := StartHandshakeHold(t, uri)
		conn, _, _ := proxiedClient(t, hold.Proxy)
		holdOnAHooksGoroutine(t, hold, conn)
	})
}

// holdOnAHooksGoroutine calls HoldWriteLock on a goroutine of its own, as a hook does, waits for
// that goroutine to end, and logs hookWentOn when HoldWriteLock returned on it rather than ending it.
func holdOnAHooksGoroutine(t *testing.T, hold *HandshakeHold, conn *natsgo.Conn) {
	t.Helper()
	returned, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		hold.HoldWriteLock(t, conn)
		close(returned)
	}()
	<-ended
	select {
	case <-returned:
		t.Log(hookWentOn)
	default:
		t.Log("HoldWriteLock ended the hook's goroutine without returning")
	}
}
