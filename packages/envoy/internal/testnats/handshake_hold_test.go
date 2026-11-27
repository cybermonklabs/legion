package testnats

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/testwait"
)

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
	RequireSocketQueues(t)
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

// The hold counts a handshake attempt at its CONNECT, not at its dial, and only under a hold, as
// Attempts and Hold say: Attempts counts the CONNECTs whose answer the hold keeps, and the channel
// Hold returns closes at the first of them. A bare TCP client stands in for nats.go, so the row says
// when the CONNECT goes. Each count is read only once the row has closed the client and the hold
// has ended the connection (endRawClient): the hold counts a connection as ended last, after both
// directions of its relay have returned, so a count made anywhere in its handling, however late,
// is read.
//   - Outside a hold, a client that dials the hold's relay, sends CONNECT and PING and has the
//     server's PONG back moves neither Dials nor Attempts.
//   - Under a hold, a client that has been sent the server's INFO and sends nothing is counted as
//     one dial and no attempt, and the channel stays open. It is closed only after the INFO
//     arrives, so the hold has written the INFO, and done whatever follows that write, before the
//     connection ends.
//   - Under the hold, a client that sends its CONNECT closes the channel, and is counted as one
//     more dial and one attempt.
func TestAHoldCountsAnAttemptAtItsConnectNotAtItsDial(t *testing.T) {
	_, uri := Start(t)
	hold := StartHandshakeHold(t, uri)
	const connect = "CONNECT {\"verbose\":false,\"pedantic\":false}\r\n"

	free, freeLines := rawHoldClient(t, hold)
	if info := testwait.Receive(t, "the server's INFO through the hold's relay, outside a hold", freeLines); !strings.HasPrefix(info, "INFO ") {
		t.Fatalf("outside a hold, the hold's relay sent the client %q first, want the server's INFO", info)
	}
	writeRaw(t, free, connect+"PING\r\n")
	if pong := testwait.Receive(t, "the server's PONG through the hold's relay, outside a hold", freeLines); pong != "PONG\r\n" {
		t.Fatalf("outside a hold, the server answered the client's CONNECT and PING with %q, want PONG", pong)
	}
	endRawClient(t, hold, free, 1)
	if dials, attempts := hold.Dials(), hold.Attempts(); dials != 0 || attempts != 0 {
		t.Fatalf("outside a hold, a client whose CONNECT was answered was counted as %d dial(s) and %d held handshake attempt(s), want none: the hold counts outside a hold", dials, attempts)
	}

	held := hold.Hold()
	t.Cleanup(hold.Release)
	silent, silentLines := rawHoldClient(t, hold)
	if info := testwait.Receive(t, "the server's INFO through the hold's relay, under the hold", silentLines); !strings.HasPrefix(info, "INFO ") {
		t.Fatalf("under the hold, the hold's relay sent the client %q first, want the server's INFO", info)
	}
	endRawClient(t, hold, silent, 2)
	if dials := hold.Dials(); dials != 1 {
		t.Fatalf("under the hold, a client that was sent the server's INFO and sent nothing was counted as %d dial(s), want 1", dials)
	}
	if attempts := hold.Attempts(); attempts != 0 {
		t.Fatalf("under the hold, a client that was sent the server's INFO and sent no CONNECT was counted as %d held handshake attempt(s), want 0: the hold counts an attempt before its CONNECT", attempts)
	}
	select {
	case <-held:
		t.Fatal("under the hold, a client that was sent the server's INFO and sent no CONNECT closed the channel Hold returned: the hold reads a held CONNECT before one came")
	default:
	}

	client, lines := rawHoldClient(t, hold)
	if info := testwait.Receive(t, "the server's INFO through the hold's relay, under the hold", lines); !strings.HasPrefix(info, "INFO ") {
		t.Fatalf("under the hold, the hold's relay sent the second client %q first, want the server's INFO", info)
	}
	writeRaw(t, client, connect)
	testwait.Receive(t, "the channel Hold returned to close once the client's CONNECT reached the hold", held)
	endRawClient(t, hold, client, 3)
	if dials, attempts := hold.Dials(), hold.Attempts(); dials != 2 || attempts != 1 {
		t.Fatalf("under the hold, a client whose CONNECT closed the channel Hold returned left the hold at %d dial(s) and %d held handshake attempt(s), want 2 dials (it and the silent client) and 1 attempt", dials, attempts)
	}
}

// endRawClient closes conn, a client of the hold's relay (rawHoldClient), and waits until the hold
// has ended ended connections in all (HandshakeHold.ended), so the counts the row reads next
// include everything the hold counted for conn. Its one bound is the test binary's deadline
// (testwait.Until).
func endRawClient(t *testing.T, hold *HandshakeHold, conn net.Conn, ended int64) {
	t.Helper()
	_ = conn.Close()
	testwait.Until(t, fmt.Sprintf("the hold to have ended %d connection(s) of its relay", ended), func() (int64, bool) {
		got := hold.ended.Load()
		return got, got >= ended
	})
}

// rawHoldClient dials the hold's own relay as a bare TCP client and returns the connection and the
// lines the relay sends it, one per receive. The channel is closed when the connection ends, so a
// receive that finds it closed reads "" at once rather than waiting.
func rawHoldClient(t *testing.T, hold *HandshakeHold) (net.Conn, <-chan string) {
	t.Helper()
	relay := hold.listener.Addr().String()
	conn, err := net.DialTimeout("tcp", relay, dialTimeout)
	if err != nil {
		t.Fatalf("dial the hold's relay at %s: %v", relay, err)
	}
	lines, done := make(chan string), make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = conn.Close()
	})
	go func() {
		defer close(lines)
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			select {
			case lines <- line:
			case <-done:
				return
			}
		}
	}()
	return conn, lines
}

// writeRaw writes text, NATS protocol lines, on conn, failing the test when the write fails.
func writeRaw(t *testing.T, conn net.Conn, text string) {
	t.Helper()
	if _, err := conn.Write([]byte(text)); err != nil {
		t.Fatalf("write %q to the hold's relay: %v", text, err)
	}
}
