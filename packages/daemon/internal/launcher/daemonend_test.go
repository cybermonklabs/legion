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

// daemonEnd is the daemon's end of a loopback listener that the code under test dials (a launcher,
// or a shim), with the accept loop that reads every connection reaching it. A connection is the
// code under test's only when its first line is the opening that code sends (opens says which).
// Another process of the machine can reach the listener too: the kernel hands a listener a
// loopback port that a client of another test binary may still redial (on the dev box, a NATS
// client an envoy cmd/listener test left reconnecting to its stopped server dialled
// 127.0.0.1:36031 about every 6 s, sending nothing, the client's way of waiting for the server to
// speak first). Each connection's first line is read on a goroutine of its own, so one that sends
// nothing holds up no other; one that opens with anything else is closed and logged, and fails
// nothing.
type daemonEnd struct {
	net.Listener
	t *testing.T
	// opens reports whether a connection's first frame is the opening of the code under test.
	opens func(shimwire.Frame) bool
	// arrivals carries each connection that opened as the code under test opens, in the order
	// those openings were read, and the code's return once whatever runs it reports it.
	arrivals chan arrival
	mu       sync.Mutex
	// done is set by the test's cleanup: from then on the loop logs nothing and closes what it reads.
	done bool
	// pending is each connection whose first line is still being read.
	pending map[net.Conn]bool
}

// arrival is one thing next receives: a connection that opened as the code under test opens, with
// its first frame and the reader that read it, or the report that the code returned, with what it
// returned.
type arrival struct {
	conn     net.Conn
	reader   *bufio.Reader
	frame    shimwire.Frame
	returned bool
	err      error
}

// waitingArrivals is how many arrivals the daemon end keeps that no case has taken: more than a
// pod's roles and their redials, and code that opens more connections fails its test.
const waitingArrivals = 64

// listenForLauncher is listenFor a launcher under test whose hellos carry one of tokens.
func listenForLauncher(t *testing.T, tokens ...string) *daemonEnd {
	t.Helper()
	return listenFor(t, func(frame shimwire.Frame) bool {
		hello, ok := frame.(shimwire.LauncherHello)
		return ok && slices.Contains(tokens, hello.Token)
	})
}

// shimHello is the opening of a shim under test: its hello, carrying bootToken.
func shimHello(bootToken string) func(shimwire.Frame) bool {
	return func(frame shimwire.Frame) bool {
		hello, ok := frame.(shimwire.Hello2)
		return ok && hello.BootToken == bootToken
	}
}

// listenFor listens on a loopback port the kernel picks, for code under test whose connections
// open with a frame opens accepts, and starts the accept loop. Its cleanup, registered before the
// code's own, runs after the code has stopped: it closes the listener and every connection the
// loop still holds.
func listenFor(t *testing.T, opens func(shimwire.Frame) bool) *daemonEnd {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	end := &daemonEnd{Listener: listener, t: t, opens: opens, arrivals: make(chan arrival, waitingArrivals), pending: map[net.Conn]bool{}}
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

// read reads conn's first line and hands the connection on when it is the code under test's opening.
func (end *daemonEnd) read(conn net.Conn) {
	reader := bufio.NewReader(conn)
	line, readErr := reader.ReadBytes('\n')
	var frame shimwire.Frame
	ours := false
	if readErr == nil {
		if decoded, err := shimwire.Decode(line); err == nil {
			frame, ours = decoded, end.opens(decoded)
		}
	}
	end.mu.Lock()
	defer end.mu.Unlock()
	delete(end.pending, conn)
	switch {
	case end.done:
		_ = conn.Close()
	case ours:
		end.deliver(arrival{conn: conn, reader: reader, frame: frame})
	default:
		_ = conn.Close()
		end.t.Logf("the daemon end closed a connection from %s that opened with no opening of the code under test (%.200q, %v)",
			conn.RemoteAddr(), line, readErr)
	}
}

// codeReturned reports that the code under test returned err, so a wait for its next connection
// ends now rather than at the test binary's deadline. Whatever runs the code calls it once the code
// (Run, the process running it, or the shim) has returned.
func (end *daemonEnd) codeReturned(err error) {
	end.mu.Lock()
	defer end.mu.Unlock()
	if !end.done {
		end.deliver(arrival{returned: true, err: err})
	}
}

// deliver queues a for next; the caller holds mu.
func (end *daemonEnd) deliver(a arrival) {
	select {
	case end.arrivals <- a:
	default:
		if a.conn != nil {
			_ = a.conn.Close()
		}
		end.t.Errorf("the code under test opened more than %d connections that no case took", waitingArrivals)
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

// next is the code under test's next connection, opened with what names. It waits for that
// opening alone, and the one bound of the wait is the test binary's deadline (testwait.Receive);
// code that returns first fails the test at once, naming what it returned. The test's cleanup
// closes the connection.
func (end *daemonEnd) next(t *testing.T, what string) arrival {
	t.Helper()
	got := testwait.Receive(t, "a connection to "+end.Addr().String()+" that opens with "+what, end.arrivals)
	if got.returned {
		t.Fatalf("the code under test returned (%v) before it opened the connection this case waits for", got.err)
	}
	t.Cleanup(func() { _ = got.conn.Close() })
	return got
}

// acceptRole takes the launcher's next connection up to its hello (next): the daemon end of the
// role that hello names, which has not been acknowledged yet.
func acceptRole(t *testing.T, end *daemonEnd) *roleConn {
	t.Helper()
	got := end.next(t, "the hello of the launcher under test")
	hello := got.frame.(shimwire.LauncherHello)
	if hello.Role == "" || hello.LauncherID == "" {
		t.Fatalf("hello = %#v", hello)
	}
	return &roleConn{t: t, conn: got.conn, reader: got.reader, writer: shimwire.NewWriter(got.conn), hello: hello}
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
			end.codeReturned(Run(t.Context(), Config{Connect: "tcp://" + end.Addr().String(), Token: "launcher-token",
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
		{"returned", []string{"the code under test returned (launcher: no sandbox) before it opened the connection this case waits for"}},
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
