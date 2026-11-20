package launcher

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// daemonEnd is the daemon's end of the loopback listener a launcher under test dials, with the
// accept loop that reads every connection reaching it. A connection is the launcher's only when
// its first line is a hello under one of the launcher's tokens. Another process of the machine
// can reach the listener too: the kernel hands a listener a loopback port that a client of
// another test binary may still redial (on the dev box, a NATS client an envoy cmd/listener test
// left reconnecting to its stopped server dialled 127.0.0.1:36031 about every 6 s, sending
// nothing, the client's way of waiting for the server to speak first). Each connection's first
// line is read on a goroutine of its own, so one that sends nothing holds up no other; one that
// opens with anything else is closed and logged, and fails nothing.
type daemonEnd struct {
	net.Listener
	t *testing.T
	// tokens are the launcher tokens the launcher under test carries in its hellos.
	tokens []string
	// arrivals carries each connection that opened with the launcher's hello, in the order the
	// hellos were read, and the launcher's return once the code that runs it reports it.
	arrivals chan arrival
	mu       sync.Mutex
	// done is set by the test's cleanup: from then on the loop logs nothing and closes what it reads.
	done bool
	// pending is each connection whose first line is still being read.
	pending map[net.Conn]bool
}

// arrival is one thing acceptRole receives: a connection that opened with the launcher's hello,
// with the reader that read it, or the report that the launcher returned, with what it returned.
type arrival struct {
	conn     net.Conn
	reader   *bufio.Reader
	hello    shimwire.LauncherHello
	returned bool
	err      error
}

// waitingArrivals is how many arrivals the daemon end keeps that no case has taken: more than a
// pod's roles and their redials, and a launcher that opens more connections fails its test.
const waitingArrivals = 64

// listenForLauncher listens on a loopback port the kernel picks, for a launcher under test whose
// hellos carry one of tokens, and starts the accept loop. Its cleanup, registered before the
// launcher's own, runs after the launcher has stopped: it closes the listener and every
// connection the loop still holds.
func listenForLauncher(t *testing.T, tokens ...string) *daemonEnd {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	end := &daemonEnd{Listener: listener, t: t, tokens: tokens, arrivals: make(chan arrival, waitingArrivals), pending: map[net.Conn]bool{}}
	t.Cleanup(end.stop)
	go end.accept()
	return end
}

// accept is the loop: it runs until the listener closes.
func (end *daemonEnd) accept() {
	for {
		conn, err := end.Accept()
		if err != nil {
			return
		}
		end.mu.Lock()
		if end.done {
			end.mu.Unlock()
			_ = conn.Close()
			continue
		}
		end.pending[conn] = true
		end.mu.Unlock()
		go end.read(conn)
	}
}

// read reads conn's first line and hands the connection on when it is the launcher's hello.
func (end *daemonEnd) read(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, readErr := reader.ReadBytes('\n')
	var hello shimwire.LauncherHello
	ours := false
	if readErr == nil {
		if frame, err := shimwire.Decode(line); err == nil {
			hello, ours = frame.(shimwire.LauncherHello)
			ours = ours && slices.Contains(end.tokens, hello.Token)
		}
	}
	end.mu.Lock()
	defer end.mu.Unlock()
	delete(end.pending, conn)
	switch {
	case end.done:
		_ = conn.Close()
	case ours:
		end.deliver(arrival{conn: conn, reader: reader, hello: hello})
	default:
		_ = conn.Close()
		end.t.Logf("the daemon end closed a connection from %s that opened with no hello of the launcher under test (%.200q, %v)",
			conn.RemoteAddr(), line, readErr)
	}
}

// launcherReturned reports that the launcher under test returned err, so a wait for its next
// connection ends now rather than at the test binary's deadline. The code that runs the launcher
// calls it once Run (or the process running it) has returned.
func (end *daemonEnd) launcherReturned(err error) {
	end.mu.Lock()
	defer end.mu.Unlock()
	if !end.done {
		end.deliver(arrival{returned: true, err: err})
	}
}

// deliver queues a for acceptRole; the caller holds mu.
func (end *daemonEnd) deliver(a arrival) {
	select {
	case end.arrivals <- a:
	default:
		if a.conn != nil {
			_ = a.conn.Close()
		}
		end.t.Errorf("the launcher under test opened more than %d connections that no case took", waitingArrivals)
	}
}

// stop ends the loop's logging and closes the listener and every connection the loop holds.
func (end *daemonEnd) stop() {
	_ = end.Close()
	end.mu.Lock()
	defer end.mu.Unlock()
	end.done = true
	for conn := range end.pending {
		_ = conn.Close()
	}
	for {
		select {
		case untaken := <-end.arrivals:
			if untaken.conn != nil {
				_ = untaken.conn.Close()
			}
		default:
			return
		}
	}
}

// acceptRole takes the launcher's next connection up to its hello: the daemon end of the role
// that hello names, which has not been acknowledged yet. It waits for that hello alone, and the
// one bound of the wait is the test binary's deadline (testwait.Receive); a launcher that returns
// first fails the test at once, naming what it returned.
func acceptRole(t *testing.T, end *daemonEnd) *roleConn {
	t.Helper()
	got := testwait.Receive(t, "a connection to "+end.Addr().String()+" that opens with the hello of the launcher under test", end.arrivals)
	if got.returned {
		t.Fatalf("the launcher under test returned (%v) before it opened the connection this case waits for", got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })
	if got.hello.Role == "" || got.hello.LauncherID == "" {
		t.Fatalf("hello = %#v", got.hello)
	}
	return &roleConn{t: t, conn: got.conn, reader: got.reader, writer: shimwire.NewWriter(got.conn), hello: got.hello}
}

// helloWaitChildEnv marks a child run of TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline,
// naming the case it runs. TestMain runs no fixtureenv.Guard in such a child, which would take
// the marker out (helperChildVariables, main_test.go).
const helloWaitChildEnv = "LEGION_LAUNCHER_HELLO_WAIT_CHILD"

func init() {
	helperChildVariables = append(helperChildVariables, helloWaitChildEnv)
}

// The wait for a role's hello has no bound of its own. A role whose connection never sends its
// hello fails the test 30 s before the test binary's deadline, naming what it waited for, and a
// launcher that returns before it connects fails it at once, naming what it returned. A child run
// of this test binary holds each, under a -timeout of 45 s: the silent role's wait outlasts the
// 10 s the rig's read once had, and ends by the wait's own text rather than the binary's panic.
func TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline(t *testing.T) {
	switch os.Getenv(helloWaitChildEnv) {
	case "silent":
		end := listenForLauncher(t, "launcher-token")
		conn, err := net.Dial("tcp", end.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		acceptRole(t, end)
		return
	case "returned":
		end := listenForLauncher(t, "launcher-token")
		// A configuration with no sandbox is refused before Run dials anything.
		go func() {
			end.launcherReturned(Run(t.Context(), Config{Connect: "tcp://" + end.Addr().String(), Token: "launcher-token",
				Role: "tester", PodUID: "pod-1", PrivateDir: t.TempDir()}))
		}()
		acceptRole(t, end)
		return
	}
	for _, tc := range []struct {
		child string
		want  []string
	}{
		{"silent", []string{
			"testwait: TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline: gave up waiting for a connection to 127.0.0.1:",
			" that opens with the hello of the launcher under test 30s before the test binary's deadline",
		}},
		{"returned", []string{"the launcher under test returned (launcher: no sandbox) before it opened the connection this case waits for"}},
	} {
		t.Run(tc.child, func(t *testing.T) {
			child := exec.Command(os.Args[0], "-test.run=^TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline$", "-test.count=1", "-test.timeout=45s")
			child.Env = append(os.Environ(), helloWaitChildEnv+"="+tc.child)
			out, err := child.CombinedOutput()
			exit, failed := err.(*exec.ExitError)
			switch {
			case !failed || exit.ExitCode() != 1:
				t.Fatalf("the child run = %v, want exit status 1; its output:\n%s", err, out)
			case strings.Contains(string(out), "panic: test timed out"):
				t.Fatalf("the child run ran into the test binary's deadline, want the wait's own failure first; its output:\n%s", out)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(out), want) {
					t.Fatalf("the child run's output does not hold %q:\n%s", want, out)
				}
			}
		})
	}
}
