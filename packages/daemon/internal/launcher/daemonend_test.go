package launcher

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
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
// Another process of the machine can reach the listener too, since the kernel can hand it a
// loopback port that a client of another test binary still redials. Each connection's first line
// is read on a goroutine of its own, so one that sends nothing holds up no other. One whose first
// line is no modelled frame of the wire (shimwire's Raw, or no frame at all), or is the hello of a
// launcher or shim of another listener, is closed and logged, and fails nothing (read), and so is
// one still silent when the case ends. The code under test's opening carries a token of this
// listener's own (ownToken), so no launcher or shim of another rig, this package's own included,
// opens as this one's code. No code but the loop accepts from the listener (a case reads only its
// Addr), so no case takes a connection past the loop.
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
// its first frame and the reader that read it; a connection that opened with a modelled frame that
// is no opening, with that frame (misopened); or the report that the code returned, with what it
// returned.
type arrival struct {
	conn      net.Conn
	reader    *bufio.Reader
	frame     shimwire.Frame
	misopened bool
	returned  bool
	err       error
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

// read reads conn's first line and hands the connection on when it is the code under test's
// opening. It closes and logs any other connection, which fails nothing: one that sent nothing, a
// line that is no modelled frame of the wire (shimwire's Raw, such as a stranger's JSON object, or
// no frame at all), or the hello of a launcher or shim under a token that is not this listener's.
// A first frame that is a modelled frame and no hello fails the test, though, and ends the wait for
// the code's next opening at once (next): the launcher and the shim each open with their hello, as
// does any other process's launcher or shim, so such a frame is the code under test opening wrong.
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
		switch frame.(type) {
		case nil, shimwire.Raw, shimwire.Hello, shimwire.Hello2, shimwire.LauncherHello:
			end.t.Logf("the daemon end closed a connection from %s that opened with no opening of the code under test (%.200q, %v)",
				conn.RemoteAddr(), line, readErr)
		default:
			end.t.Errorf("a connection from %s opened with a %T, a modelled frame that no launcher or shim sends first (%.200q)", conn.RemoteAddr(), frame, line)
			end.deliver(arrival{frame: frame, misopened: true})
		}
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
// code that returns first, or opens a connection with a modelled frame that is no opening, fails
// the test at once, naming what it returned or the frame. The test's cleanup closes the connection.
func (end *daemonEnd) next(t *testing.T, what string) arrival {
	t.Helper()
	got := testwait.Receive(t, "a connection to "+end.Addr().String()+" that opens with "+what, end.arrivals)
	switch {
	case got.returned:
		t.Fatalf("the code under test returned (%v) before it opened the connection this case waits for", got.err)
	case got.misopened:
		t.Fatalf("the code under test opened a connection with a %T, a modelled frame that is no opening, while this case waited for a connection that opens with %s",
			got.frame, what)
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
// returns before it connects fails it at once, naming what it returned: the rig and the fleet each
// report their launcher's return (codeReturned), here of a stop grace Run refuses before it dials.
// A child run of this test binary holds each, under a -timeout of 20 s, whose margin is 5 s: the
// silent role's wait outlasts the 10 s the rig's read once had, and ends by the wait's own text
// rather than the binary's panic.
func TestTheWaitForAHelloEndsOnTheHelloTheLaunchersReturnOrTheDeadline(t *testing.T) {
	t.Parallel()
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
		newSessionRig(t, Config{StopGrace: -time.Second})
		return
	case "fleet-returned":
		newFleet(t, -time.Second, "tester")
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
	for _, child := range []string{"returned", "fleet-returned"} {
		t.Run(child, func(t *testing.T) {
			out, code := runChild(t, run, child, 20*time.Second)
			const want = "the code under test returned (launcher: stop grace must be positive) before it opened the connection this case waits for"
			if code != 1 || !strings.Contains(out, want) {
				t.Fatalf("the child run exited %d, want 1 and %q; its output:\n%s", code, want, out)
			}
		})
	}
}

// The wait for a role's next frame has no bound of its own either (roleConn.next). A connection
// that opens with the launcher's hello and then sends nothing more fails the test its margin before
// the test binary's deadline, naming what it waited for, and not at a read deadline of the rig's:
// a child run of this test binary, under a -timeout of 20 s, waits past the 10 s that read once had.
func TestTheWaitForARolesNextFrameHasNoBoundButTheDeadline(t *testing.T) {
	t.Parallel()
	if os.Getenv(helloWaitChildEnv) == "next-frame" {
		token := ownToken("launcher-token")
		end := listenForLauncher(t, token)
		conn, err := net.Dial("tcp", end.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := shimwire.NewWriter(conn).WriteFrame(launcherHelloOf(token)); err != nil {
			t.Fatal(err)
		}
		acceptRole(t, end).next()
		return
	}
	out, code := runChild(t, "^TestTheWaitForARolesNextFrameHasNoBoundButTheDeadline$", "next-frame", 20*time.Second)
	if code != 1 {
		t.Fatalf("the child run exited %d, want 1; its output:\n%s", code, out)
	}
	testwait.RequireGaveUp(t, out, "the launcher's next frame on the connection of role tester", 20*time.Second)
}

// launcherHelloOf is the hello a launcher of role tester sends under token.
func launcherHelloOf(token string) shimwire.LauncherHello {
	return shimwire.LauncherHello{Token: token, Sandbox: "legion-legion-legion-208", Role: "tester", PodUID: "pod-1", LauncherID: "launcher-1"}
}

// Each listener's token is its own: two ownToken calls for one name give two tokens, each that name
// and a suffix.
func TestEachOwnTokenIsItsOwn(t *testing.T) {
	t.Parallel()
	first, second := ownToken("launcher-token"), ownToken("launcher-token")
	if first == second || !strings.HasPrefix(first, "launcher-token-") || !strings.HasPrefix(second, "launcher-token-") {
		t.Fatalf("ownToken gave %q and %q, want two different tokens, each launcher-token and a suffix", first, second)
	}
}

// A stranger whose first line is a JSON object of no frame this package models (shimwire's Raw) is
// closed and logged as any other stranger is, and fails nothing: any process can reach a reused
// loopback port, and an object is no opening of a launcher or shim. A child run's stranger sends
// {"x":1} first and waits for the daemon end's close; the child passes and logs that close once, by
// the stranger's own address.
func TestAStrangersJSONObjectIsClosedAndLoggedAndFailsNothing(t *testing.T) {
	t.Parallel()
	const object = `{"x":1}` + "\n"
	if os.Getenv(helloWaitChildEnv) == "json-stranger" {
		end := listenForLauncher(t, ownToken("launcher-token"))
		conn, err := net.Dial("tcp", end.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("the stranger dials from %s", conn.LocalAddr())
		if _, err := io.WriteString(conn, object); err != nil {
			t.Fatal(err)
		}
		requireClosedByTheDaemonEnd(t, "the stranger's connection", conn)
		return
	}
	out, code := runChild(t, "^TestAStrangersJSONObjectIsClosedAndLoggedAndFailsNothing$", "json-stranger", 20*time.Second)
	_, after, _ := strings.Cut(out, "the stranger dials from ")
	stranger, _, _ := strings.Cut(after, "\n")
	stranger = strings.TrimSpace(stranger)
	line := fmt.Sprintf("the daemon end closed a connection from %s that opened with no opening of the code under test (%.200q, <nil>)", stranger, object)
	if code != 0 || stranger == "" || strings.Count(out, line) != 1 {
		t.Fatalf("the child run exited %d, want 0 and the line %q once; its output:\n%s", code, line, out)
	}
}

// A connection of the code under test that opens with a modelled frame other than a hello fails
// the wait for its opening at once, naming the frame, and not at the wait's margin: the launcher
// and the shim each open with their hello, so such a frame is the code opening wrong. A child run's
// code opens with launcher_hello_ack, keeps its connection open, and waits for its hello under a
// -timeout of 20 s; the child fails by the frame's name and gives up no wait. A hello under a token
// not this listener's cannot fail so: the daemon end cannot tell it from a stranger's.
func TestAnOpeningThatIsAnotherModelledFrameFailsTheWaitAtOnce(t *testing.T) {
	t.Parallel()
	if os.Getenv(helloWaitChildEnv) == "misopened" {
		end := listenForLauncher(t, ownToken("launcher-token"))
		conn, err := net.Dial("tcp", end.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := shimwire.NewWriter(conn).WriteFrame(shimwire.LauncherHelloAck{}); err != nil {
			t.Fatal(err)
		}
		acceptRole(t, end)
		return
	}
	out, code := runChild(t, "^TestAnOpeningThatIsAnotherModelledFrameFailsTheWaitAtOnce$", "misopened", 20*time.Second)
	const want = "the code under test opened a connection with a shimwire.LauncherHelloAck, a modelled frame that is no opening"
	if code != 1 || !strings.Contains(out, want) || strings.Contains(out, "gave up waiting for") {
		t.Fatalf("the child run exited %d, want 1, %q and no wait given up; its output:\n%s", code, want, out)
	}
}

// When the case ends, the daemon end closes every connection it still holds: one that sent nothing
// the whole case long, which it logs once, as it logs every other connection it closes, and one
// that opened as the code under test opens but that no case took. A child run holds one of each
// open past the daemon end's own cleanup, as another process's client would, and reads the close
// of each. The stray is known by its own address, so a connection another process opens to the
// child's port changes nothing.
func TestTheCasesEndClosesWhatTheDaemonEndHoldsAndLogsASilentConnectionOnce(t *testing.T) {
	t.Parallel()
	if os.Getenv(helloWaitChildEnv) == "silent-to-the-end" {
		var stray, untaken net.Conn
		// Registered before the daemon end's own, so it runs after the daemon end has stopped.
		t.Cleanup(func() {
			for what, conn := range map[string]net.Conn{"the stray": stray, "the connection no case took": untaken} {
				if conn != nil {
					requireClosedByTheDaemonEnd(t, what, conn)
				}
			}
		})
		token := ownToken("launcher-token")
		end := listenForLauncher(t, token)
		var err error
		if stray, err = net.Dial("tcp", end.Addr().String()); err != nil {
			t.Fatal(err)
		}
		t.Logf("the stray dials from %s", stray.LocalAddr())
		if untaken, err = net.Dial("tcp", end.Addr().String()); err != nil {
			t.Fatal(err)
		}
		if err := shimwire.NewWriter(untaken).WriteFrame(launcherHelloOf(token)); err != nil {
			t.Fatal(err)
		}
		testwait.Eventually(t, "the daemon end to read the stray's first line and hold the connection no case took", func() bool {
			end.mu.Lock()
			defer end.mu.Unlock()
			reading := false
			for conn := range end.pending {
				reading = reading || conn.RemoteAddr().String() == stray.LocalAddr().String()
			}
			return reading && len(end.arrivals) == 1
		})
		return
	}
	out, code := runChild(t, "^TestTheCasesEndClosesWhatTheDaemonEndHoldsAndLogsASilentConnectionOnce$", "silent-to-the-end", testwait.Remaining(t))
	_, after, _ := strings.Cut(out, "the stray dials from ")
	stray, _, _ := strings.Cut(after, "\n")
	stray = strings.TrimSpace(stray)
	closed := "the daemon end closed a connection from " + stray + " "
	line := closed + "that sent nothing by the end of the case"
	if code != 0 || stray == "" || strings.Count(out, line) != 1 || strings.Count(out, closed) != 1 {
		t.Fatalf("the child run exited %d, want 0 and the line %q once, and no other close of the stray; its output:\n%s", code, line, out)
	}
}

// requireClosedByTheDaemonEnd fails t unless the daemon end has closed conn, what names: a read of
// conn gives EOF. The read runs on a goroutine and is waited for through testwait.Receive, so a
// connection the daemon end never closed fails at the wait's margin. conn's own end is closed then.
func requireClosedByTheDaemonEnd(t *testing.T, what string, conn net.Conn) {
	t.Helper()
	defer func() { _ = conn.Close() }()
	reads := make(chan error, 1)
	go func() {
		_, err := conn.Read(make([]byte, 1))
		reads <- err
	}()
	if err := testwait.Receive(t, "the daemon end's close of "+what, reads); err != io.EOF {
		t.Fatalf("a read of %s gave %v, want the EOF of the daemon end's close", what, err)
	}
}
