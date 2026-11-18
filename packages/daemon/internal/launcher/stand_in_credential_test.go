package launcher

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// opened is a connection a test's daemon end accepted, with the first frame it opened with and
// the reader that read it, which holds whatever the peer sent after it.
type opened struct {
	conn   net.Conn
	reader *bufio.Reader
	frame  shimwire.Frame
}

// openers holds the accept loop of each listener a test's daemon end listens on (fromTheCode).
var openers sync.Map // net.Listener → *opener

// opener is the accept loop of one listener: the connections that opened with the code under
// test's credential, in the order their openings arrived, and every connection still being read.
// Once its test cleans up it logs nothing and closes what it holds.
type opener struct {
	accepted chan opened
	mu       sync.Mutex
	done     bool
	pending  map[net.Conn]bool
}

// fromTheCode is the next connection on listener whose first frame carries the credential of the
// code under test (carries says which do). The listener's accept loop, started by its first call,
// reads each connection's first frame on a goroutine of its own, so a connection that sends
// nothing holds up no other. A connection that opens otherwise is another process's: the loopback
// port the kernel gives a listener may be one a process of the machine still dials. It is closed
// and logged, and fails nothing, so the daemon end's power to fail the test stays with the code
// under test's own connections. Every call on one listener passes the same carries.
func fromTheCode(t *testing.T, listener net.Listener, what string, carries func(shimwire.Frame) bool) opened {
	t.Helper()
	fresh := &opener{accepted: make(chan opened, 16), pending: map[net.Conn]bool{}}
	loaded, running := openers.LoadOrStore(listener, fresh)
	o := loaded.(*opener)
	if !running {
		t.Cleanup(o.stop)
		go o.accept(t, listener, carries)
	}
	return testwait.Receive(t, what, o.accepted)
}

// stop ends the opener's logging and closes every connection it still reads or holds.
func (o *opener) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.done = true
	for conn := range o.pending {
		_ = conn.Close()
	}
	for {
		select {
		case unread := <-o.accepted:
			_ = unread.conn.Close()
		default:
			return
		}
	}
}

// accept is the opener's loop: it runs until listener closes.
func (o *opener) accept(t *testing.T, listener net.Listener, carries func(shimwire.Frame) bool) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		o.mu.Lock()
		if o.done {
			o.mu.Unlock()
			_ = conn.Close()
			continue
		}
		o.pending[conn] = true
		o.mu.Unlock()
		go func() {
			reader := bufio.NewReader(conn)
			line, err := reader.ReadBytes('\n')
			frame, decodeErr := shimwire.Frame(nil), err
			if err == nil {
				frame, decodeErr = shimwire.Decode(line)
			}
			o.mu.Lock()
			defer o.mu.Unlock()
			delete(o.pending, conn)
			if !o.done && decodeErr == nil && carries(frame) {
				o.accepted <- opened{conn: conn, reader: reader, frame: frame}
				return
			}
			_ = conn.Close()
			if !o.done {
				t.Logf("a stand-in closed a connection from %s that opened with no credential of the code under test's (%.200q)", conn.RemoteAddr(), line)
			}
		}()
	}
}

// launcherHello is the opening of a launcher under test: its hello, carrying the launcher token
// every launcher of this package's tests is given ("launcher-token", or roleToken's
// "launcher-token-<role>").
func launcherHello(frame shimwire.Frame) bool {
	hello, ok := frame.(shimwire.LauncherHello)
	return ok && strings.HasPrefix(hello.Token, "launcher-token")
}

// shimHello is the opening of a shim under test: its hello, carrying bootToken.
func shimHello(bootToken string) func(shimwire.Frame) bool {
	return func(frame shimwire.Frame) bool {
		hello, ok := frame.(shimwire.Hello2)
		return ok && hello.BootToken == bootToken
	}
}

// standInChildEnv marks the child run of TestTheDaemonEndFailsItsTestOnlyOnTheLaunchersOwnHello.
const standInChildEnv = "LEGION_LAUNCHER_STAND_IN_CHILD"

// dialWith dials listener and sends line, or frame where line is empty and frame is set, as a
// peer of the daemon end would open.
func dialWith(t *testing.T, listener net.Listener, line string, frame shimwire.Frame) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	switch {
	case line != "":
		_, err = conn.Write([]byte(line + "\n"))
	case frame != nil:
		err = shimwire.NewWriter(conn).WriteFrame(frame)
	}
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// The daemon end of a launcher's connection takes the first connection that opens with the
// launcher's hello, whatever opened before it: a connection that sends nothing, an HTTP request a
// helper of the machine sent to a reused port (dispatch://INFRA-3904), a hello under another
// launcher's token, and a frame that is no hello are each closed and fail nothing. A connection
// that opens with the launcher's token in a hello acceptRole does not expect still fails the test:
// a child run of this test binary serves one to acceptRole, and the child fails, naming the hello.
func TestTheDaemonEndFailsItsTestOnlyOnTheLaunchersOwnHello(t *testing.T) {
	if os.Getenv(standInChildEnv) == "1" {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		dialWith(t, listener, "", shimwire.LauncherHello{Token: "launcher-token", Role: "tester"})
		acceptRole(t, listener)
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	dialWith(t, listener, "", nil)
	dialWith(t, listener, "GET /computeMetadata/v1/instance/service-accounts/default/identity HTTP/1.1", nil)
	dialWith(t, listener, "", shimwire.LauncherHello{Token: "another-launchers-token", Role: "tester", LauncherID: "l-0"})
	dialWith(t, listener, "", shimwire.HelloAck{})
	dialWith(t, listener, "", shimwire.LauncherHello{Token: "launcher-token", Role: "tester", LauncherID: "l-1"})
	if conn := acceptRole(t, listener); conn.hello.LauncherID != "l-1" || conn.hello.Token != "launcher-token" {
		t.Fatalf("acceptRole took %#v, want the launcher's own hello", conn.hello)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestTheDaemonEndFailsItsTestOnlyOnTheLaunchersOwnHello$", "-test.count=1")
	child.Env = append(os.Environ(), standInChildEnv+"=1")
	out, err := child.CombinedOutput()
	if exit, failed := err.(*exec.ExitError); !failed || exit.ExitCode() != 1 || !strings.Contains(string(out), "hello = shimwire.LauncherHello") {
		t.Fatalf("the child run that served a hello under the launcher's token without a launcher id = %v, output:\n%s\nwant exit status 1 and acceptRole's failure naming the hello", err, out)
	}
}
