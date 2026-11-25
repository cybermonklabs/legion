package testnats

import (
	"bufio"
	"fmt"
	"net"
	"os"
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
// of how long a call takes decides any of it. The negative controls: a HoldWriteLock that returns
// without the lock fails the try; a release that does not let the lock go fails the publish's
// return.
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
	if err := direct.FlushWithContext(Context(t)); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	release := hold.HoldWriteLock(t, conn)
	lock := connLock(t, conn)
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
// close, which shows that the rig tells the two apart. The row connects through the hold as
// Connect does, through the shared wait, which makes a connect that failed again, each dial
// carrying the wait's give-up as its socket's deadline (socketDialer, giveUpDialer).
func TestHoldStalledWriteHoldsTheLockWithAWriteTheSocketsCloseEnds(t *testing.T) {
	RequireSocketQueues(t)
	for _, stalled := range []bool{true, false} {
		name := "HoldStalledWrite"
		if !stalled {
			name = "HoldWriteLock, the control"
		}
		t.Run(name, func(t *testing.T) {
			hold := StartHandshakeHold(t, URL(t))
			dialer := &socketDialer{giveUpDialer: giveUpDialer{t: t}}
			dial := testwait.Until(t, fmt.Sprintf("a connection through the hold at %s", hold.URL()), func() (dialAttempt, bool) {
				conn, err := natsgo.Connect(hold.URL(), natsgo.SetCustomDialer(dialer), natsgo.NoReconnect())
				return dialAttempt{conn: conn, err: err}, err == nil
			})
			conn := dial.conn
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
			if stalled {
				testwait.Receive(t, "a read of the status once the socket under HoldStalledWrite's write closed", read)
				return
			}
			// The control's absence: a read that waits on a lock the row holds says so by nothing,
			// nats.go's lock has no observer the test could read, and no later action is ordered
			// after the read; the span is main's.
			testwait.Quiet(t, "a read of the status while HoldWriteLock holds the lock after the socket closed", read, 10*time.Second,
				"a read blocked on nats.go's connection lock changes no state the test can read, the lock has no observer, and no later action is ordered after the read")
		})
	}
}

// socketDialer dials as giveUpDial's dialer does (giveUpDialer), so the socket carries the shared
// wait's give-up as its deadline until the connect returns, and keeps the last socket it handed
// out: once the row's wait has ended, the socket of the connect that succeeded, the last it made.
type socketDialer struct {
	giveUpDialer
	last atomic.Pointer[net.Conn]
}

func (d *socketDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := d.giveUpDialer.Dial(network, address)
	if err == nil {
		d.last.Store(&conn)
	}
	return conn, err
}

// holdChildEnv tells the re-executed test binary to run TestTakeWriteLockOnAHooksGoroutineChild,
// which skips in an ordinary run, and holds the URL of the shared server the parent started, which
// the child's handshake hold relays to.
const holdChildEnv = "TESTNATS_HOLD_CHILD"

// hookWentOn is the line the child logs once a TakeWriteLock called on a goroutine of its own has
// returned there.
const hookWentOn = "the hook's goroutine went on after TakeWriteLock"

// holdChildGuard is the -timeout of TestTakeWriteLockOnAHooksGoroutineChild (testwait.RunChild): a
// hang guard only, which a child whose take never gives up runs into, and never what decides the
// row. The child's whole run measured 0.15 s beside two busy loops and 0.36 s under -race beside two
// (builds Jobs at the commit that made this form), and 0.30 s in an issue pod; two minutes is over
// three hundred times the slowest of them, and a setup held 20 s still passes inside it.
const holdChildGuard = 2 * time.Minute

// TakeWriteLock, the take a hand-back row's hook makes on a goroutine other than the test's,
// returns there with the shared wait's give-up as its error when another holder keeps the lock to
// the moment the wait gives up, so the hook goes on and the test goroutine fails the row by name. A
// t.Fatal there would end the hook's goroutine rather than the test, and the child would log that
// TakeWriteLock never returned. The child sets up the hold and a client through it on the wall
// clock, under the shared wait's own bound, takes the connection's lock, and then takes the give-up
// as a step of its own (testwait.SetUpDone), so the take's wait gives up at its first question
// whenever a loaded machine lets the child get there. Under a fixed -timeout of 45 s the take gave
// up 15 s in, and a setup still running then failed the child with the give-up of one of its own
// waits rather than the take's text the parent wants. The parent starts the shared server (URL)
// and passes its URL to the child, so a server whose start is slow costs the parent time and not
// the child.
func TestTakeWriteLockOnAHooksGoroutineFailsByNameAndReturns(t *testing.T) {
	uri := URL(t)
	stdout, stderr, err := testwait.RunChild(t, holdChildGuard, "^TestTakeWriteLockOnAHooksGoroutineChild$", holdChildEnv+"="+uri)
	text := stdout + stderr
	if err == nil {
		t.Fatalf("the child was meant to fail its row; it passed:\n%s", text)
	}
	if strings.Contains(text, "panic:") {
		t.Fatalf("the child panicked rather than failing its row by name:\n%s", text)
	}
	for _, want := range []string{
		"testwait: testnats: gave up waiting for nats.go to let its connection lock go, so the fixture can take it as a write does 30s before the test binary's deadline",
		"--- FAIL: TestTakeWriteLockOnAHooksGoroutineChild (",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the child's output does not say %q:\n%s", want, text)
		}
	}
	if went := strings.Count(text, hookWentOn); went != 1 {
		t.Errorf("the child logged %q %d time(s), want once:\n%s", hookWentOn, went, text)
	}
}

// TestTakeWriteLockOnAHooksGoroutineChild is the child of
// TestTakeWriteLockOnAHooksGoroutineFailsByNameAndReturns. It holds the connection's lock itself,
// and its setup ends there: the give-up is its next step (testwait.SetUpDone), so the TakeWriteLock
// its hook calls gives up at its first question. It runs only when the parent re-executes this
// binary.
func TestTakeWriteLockOnAHooksGoroutineChild(t *testing.T) {
	uri := os.Getenv(holdChildEnv)
	if uri == "" {
		t.Skip(holdChildEnv + " is not set: this test is the child of TestTakeWriteLockOnAHooksGoroutineFailsByNameAndReturns")
	}
	hold := StartHandshakeHold(t, uri)
	conn, _, _ := proxiedClient(t, hold.Proxy)
	lock := connLock(t, conn)
	lock.Lock()
	t.Cleanup(lock.Unlock)
	testwait.SetUpDone(t)
	taken, ended := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ended)
		release, err := hold.TakeWriteLock(conn)
		if release != nil {
			release()
		}
		taken <- err
	}()
	<-ended
	select {
	case err := <-taken:
		t.Log(hookWentOn)
		if err != nil {
			t.Error(err)
		}
	default:
		t.Log("TakeWriteLock ended the hook's goroutine without returning")
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
	// The relay listens on loopback and accepts at once, so the dial takes no bound of its own, as
	// the relay's own dial of the server takes none (join).
	conn, err := net.Dial("tcp", relay)
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
