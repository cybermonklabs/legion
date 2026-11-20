package launcher

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
// nothing, and so is one still silent when the case ends. The code under test's opening carries a
// token of this listener's own (ownToken), so no launcher or shim of another rig, this package's
// own included, opens as this one's code. The listener itself is the loop's alone: nothing else
// accepts from it, so no case can take a connection past the loop.
type daemonEnd struct {
	listener net.Listener
	t        *testing.T
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

// ownToken is name with a random suffix of its own: the launcher token or boot token of one
// listener's code under test, which no other rig's code carries.
func ownToken(name string) string {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	return name + "-" + hex.EncodeToString(suffix)
}

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
	end := &daemonEnd{listener: listener, t: t, opens: opens, arrivals: make(chan arrival, waitingArrivals), pending: map[net.Conn]bool{}}
	t.Cleanup(end.stop)
	go end.accept()
	return end
}

// Addr is the address the code under test dials.
func (end *daemonEnd) Addr() net.Addr { return end.listener.Addr() }

// accept is the loop: it runs until the listener closes.
func (end *daemonEnd) accept() {
	for {
		conn, err := end.listener.Accept()
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

// stop ends the loop's logging and closes the listener and every connection the loop holds. A
// connection whose first line is still being read sent nothing the whole case long; stop logs it,
// once, as read logs any other connection it closes (read then closes nothing more and logs
// nothing, since done is set).
func (end *daemonEnd) stop() {
	_ = end.listener.Close()
	end.mu.Lock()
	defer end.mu.Unlock()
	end.done = true
	for conn := range end.pending {
		_ = conn.Close()
		end.t.Logf("the daemon end closed a connection from %s that sent nothing by the end of the case", conn.RemoteAddr())
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

// helloWaitChildEnv marks a child run of this file's tests, naming the case it runs. TestMain
// runs no fixtureenv.Guard in such a child, which would take the marker out (helperChildVariables,
// main_test.go).
const helloWaitChildEnv = "LEGION_LAUNCHER_HELLO_WAIT_CHILD"

func init() {
	helperChildVariables = append(helperChildVariables, helloWaitChildEnv)
}

// runChild runs this test binary for the tests run names, under -test.timeout timeout, with the
// child case named, and returns its output and its exit code.
func runChild(t *testing.T, run, child string, timeout time.Duration) (string, int) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run="+run, "-test.count=1", "-test.v", "-test.timeout="+timeout.String())
	command.Env = append(os.Environ(), helloWaitChildEnv+"="+child)
	out, err := command.CombinedOutput()
	code := 0
	if exit, failed := err.(*exec.ExitError); failed {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "panic: test timed out") {
		t.Fatalf("the child run ran into the test binary's deadline; its output:\n%s", out)
	}
	return string(out), code
}

// listensAt is the address a child run's daemon end listens at, as it logged it.
func listensAt(t *testing.T, out string) string {
	t.Helper()
	_, after, found := strings.Cut(out, "the daemon end listens at ")
	address, _, _ := strings.Cut(after, "\n")
	if !found || address == "" {
		t.Fatalf("the child run logged no daemon end's address; its output:\n%s", out)
	}
	return strings.TrimSpace(address)
}

// The wait for a role's hello has no bound of its own. A role whose connection never sends its
// hello fails the test its margin before the test binary's deadline, naming what it waited for
// (testwait.RequireGaveUp holds the moment and the -timeout the text names), and a launcher that
// returns before it connects fails it at once, naming what it returned. A child run of this test
// binary holds each, under a -timeout of 20 s, whose margin is 5 s: the silent role's wait outlasts
// the 10 s the rig's read once had, and ends by the wait's own text rather than the binary's panic.
func TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline(t *testing.T) {
	switch os.Getenv(helloWaitChildEnv) {
	case "silent":
		end := listenForLauncher(t, ownToken("launcher-token"))
		t.Logf("the daemon end listens at %s", end.Addr())
		conn, err := net.Dial("tcp", end.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		acceptRole(t, end)
		return
	case "returned":
		token := ownToken("launcher-token")
		end := listenForLauncher(t, token)
		// A configuration with no sandbox is refused before Run dials anything.
		go func() {
			end.codeReturned(Run(t.Context(), Config{Connect: "tcp://" + end.Addr().String(), Token: token,
				Role: "tester", PodUID: "pod-1", PrivateDir: t.TempDir()}))
		}()
		acceptRole(t, end)
		return
	}
	const run = "^TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline$"
	t.Run("silent", func(t *testing.T) {
		out, code := runChild(t, run, "silent", 20*time.Second)
		if code != 1 {
			t.Fatalf("the child run exited %d, want 1; its output:\n%s", code, out)
		}
		what := "a connection to " + listensAt(t, out) + " that opens with the hello of the launcher under test"
		testwait.RequireGaveUp(t, out, what, 20*time.Second)
	})
	t.Run("returned", func(t *testing.T) {
		out, code := runChild(t, run, "returned", 20*time.Second)
		const want = "the code under test returned (launcher: no sandbox) before it opened the connection this case waits for"
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("the child run exited %d, want 1 and %q; its output:\n%s", code, want, out)
		}
	})
}

// A connection that sends nothing through the whole case is closed when the case ends, and logged
// once, as the daemon end logs every other connection it closes. A child run dials one such
// connection, which stays open past the daemon end's own cleanup as another process's client
// would, and ends once the daemon end is reading it.
func TestAConnectionSilentThroughTheCaseIsLoggedOnce(t *testing.T) {
	if os.Getenv(helloWaitChildEnv) == "silent-to-the-end" {
		var conn net.Conn
		// Registered before the daemon end's own, so it runs after the daemon end has stopped.
		t.Cleanup(func() {
			if conn != nil {
				_ = conn.Close()
			}
		})
		end := listenForLauncher(t, ownToken("launcher-token"))
		var err error
		if conn, err = net.Dial("tcp", end.Addr().String()); err != nil {
			t.Fatal(err)
		}
		t.Logf("the stray dials from %s", conn.LocalAddr())
		testwait.Eventually(t, "the daemon end to read the stray's first line", func() bool {
			end.mu.Lock()
			defer end.mu.Unlock()
			return len(end.pending) == 1
		})
		return
	}
	out, code := runChild(t, "^TestAConnectionSilentThroughTheCaseIsLoggedOnce$", "silent-to-the-end", 10*time.Minute)
	_, after, _ := strings.Cut(out, "the stray dials from ")
	stray, _, _ := strings.Cut(after, "\n")
	line := "the daemon end closed a connection from " + strings.TrimSpace(stray) + " that sent nothing by the end of the case"
	if code != 0 || stray == "" || strings.Count(out, line) != 1 || strings.Count(out, "the daemon end closed a connection") != 1 {
		t.Fatalf("the child run exited %d, want 0 and the line %q once, and no other close; its output:\n%s", code, line, out)
	}
}

// A rig's waits still wait under a -timeout of 30 s or less: their margin is a quarter of such a
// -timeout (testwait.Receive). A child run of a rig case that takes about 2 s passes under a
// -timeout of 25 s, where a margin of 30 s would have every wait give up at once.
func TestARigsWaitsStillWaitUnderAShortTimeout(t *testing.T) {
	if os.Getenv(helloWaitChildEnv) != "" {
		return
	}
	out, code := runChild(t, "^TestTheLauncherLogsOneLineForEachAcknowledgedHello$", "", 25*time.Second)
	if code != 0 || !strings.Contains(out, "--- PASS: TestTheLauncherLogsOneLineForEachAcknowledgedHello ") {
		t.Fatalf("the rig case under -timeout 25s exited %d, want it to pass; its output:\n%s", code, out)
	}
}
