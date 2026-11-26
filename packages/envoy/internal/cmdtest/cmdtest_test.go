package cmdtest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/fixtureenv"
	"github.com/sjawhar/envoy/listenfd"
)

const (
	// heldPortChildEnv makes this test binary run heldPortChild instead of its tests.
	heldPortChildEnv = "CMDTEST_HELD_PORT_CHILD"
	// heldPortFD is the variable Pass names the child's inherited socket in.
	heldPortFD = "CMDTEST_LISTEN_FD"
	// buildChildEnv makes this test binary's TestMain run its tests under Main ("main") or without
	// it ("plain"): the processes the build rows (build_test.go) start, whose TestBuildChild builds
	// hello.
	buildChildEnv = "CMDTEST_BUILD_CHILD"
)

// TestMain runs this binary as the held-port child where heldPortChildEnv is set, and runs the
// tests otherwise. Only TestMain makes that child of a copy a test starts: in a build of these
// tests without it, such a copy runs the tests, each of which starts copies that run them again.
// heldPortChildCopy refuses to start a copy in such a process (heldPortChildCopyRefusal), and
// TestACopyStartedAsTheHeldPortChildRunsAsTheChild fails in such a build by name.
func TestMain(m *testing.M) {
	if os.Getenv(heldPortChildEnv) == "1" {
		os.Exit(heldPortChild())
	}
	if os.Getenv(buildChildEnv) == "main" {
		os.Exit(Main(m))
	}
	os.Exit(m.Run())
}

// heldPortChildCopy is the path of this test binary, which a test runs as the held-port child with
// heldPortChildEnv=1 in the copy's environment. It fails t where heldPortChildCopyRefusal refuses.
func heldPortChildCopy(t *testing.T) string {
	t.Helper()
	if err := heldPortChildCopyRefusal(); err != nil {
		t.Fatal(err)
	}
	return os.Args[0]
}

// heldPortChildCopyRefusal is why this process may start no copy of this test binary, nil when it
// may. A process that has heldPortChildEnv=1 was started as the held-port child, and TestMain runs
// such a process as the child, so one that runs the tests runs in a build without TestMain, and a
// copy started there would run the tests again.
func heldPortChildCopyRefusal() error {
	if os.Getenv(heldPortChildEnv) == "1" {
		return fmt.Errorf("this process has %s=1, so a test started it as the held-port child, but it runs the tests: this build of the tests has no TestMain that runs the child, and a copy started here would run the tests again", heldPortChildEnv)
	}
	return nil
}

// heldPortChildCopyRefusal, which heldPortChildCopy asks before any copy starts, refuses a process
// that has heldPortChildEnv=1 and no other. While TestMain is in the build no copy reaches it, so
// this test is what holds the refusal.
func TestAProcessStartedAsTheHeldPortChildStartsNoCopy(t *testing.T) {
	fixtureenv.Unset(t, heldPortChildEnv)
	if err := heldPortChildCopyRefusal(); err != nil {
		t.Fatalf("with %s unset: %v, want no refusal", heldPortChildEnv, err)
	}
	t.Setenv(heldPortChildEnv, "1")
	if err := heldPortChildCopyRefusal(); err == nil || !strings.Contains(err.Error(), heldPortChildEnv+"=1") {
		t.Fatalf("with %s=1: %v, want a refusal that names it", heldPortChildEnv, err)
	}
}

// A copy started with heldPortChildEnv=1 runs as the held-port child: it waits for a line on its
// standard input, and given none says so and exits 1. The child does not read -h. In a build
// without TestMain, the testing package reads -h before any test runs, prints its own flag usage
// and exits 0, so this test fails there by name and its copy starts no copy of its own.
func TestACopyStartedAsTheHeldPortChildRunsAsTheChild(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(heldPortChildCopy(t), "-h")
	cmd.Env = append(os.Environ(), heldPortChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if want := "held-port child: wait for the line to serve: EOF\n"; !errors.As(err, &exit) || exit.ExitCode() != 1 || string(out) != want {
		t.Fatalf("a copy started as the held-port child with -h and no input: %v, printed %q, want exit 1 and %q", err, out, want)
	}
}

// heldPortChild is the process TestHeldPortStaysHeldUntilItsProcessServes starts. It waits for a
// line on standard input, then serves one connection on the socket it inherited, answering its own
// pid, and exits once the test closes that connection, so the test is the one to close first.
func heldPortChild() int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "held-port child: "+format+"\n", args...)
		return 1
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		return fail("wait for the line to serve: %v", err)
	}
	socket, err := listenfd.Inspect(heldPortFD, os.Getenv(heldPortFD))
	if err != nil {
		return fail("%v", err)
	}
	listener, err := listenfd.Adopt(socket)
	if err != nil {
		return fail("%v", err)
	}
	defer listener.Close()
	conn, err := listener.Accept()
	if err != nil {
		return fail("accept: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%d\n", os.Getpid()); err != nil {
		return fail("answer: %v", err)
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fail("wait for the test to close the connection: %v", err)
	}
	return 0
}

// A held port stays bound from HoldPort until the process it was passed to serves on it, with only
// that process holding it once Start returns: another program's bind in the meantime is refused,
// the process serves on the socket, and nothing holds the socket once it exits.
func TestHeldPortStaysHeldUntilItsProcessServes(t *testing.T) {
	held := HoldPort(t)
	// The socket's inode names it whoever holds a descriptor of it, which a port does not: once the
	// socket is closed, another program may bind the port and listen there.
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(held.file.Fd()), &stat); err != nil {
		t.Fatalf("read the held socket's inode: %v", err)
	}
	inode := strconv.FormatUint(stat.Ino, 10)
	cmd := exec.Command(heldPortChildCopy(t))
	cmd.Env = append(os.Environ(), heldPortChildEnv+"=1")
	held.Pass(cmd, heldPortFD)
	serve, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := Start(t, "the held-port child", cmd)

	// The child is waiting for its line and has not served, so only its inherited copy can hold the
	// port.
	competitor, err := net.Listen("tcp", held.Addr)
	if err == nil {
		_ = competitor.Close()
		t.Fatalf("another listener bound %s after Start, before the child served; want address already in use", held.Addr)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("another listener's bind of %s before the child served: %v; want address already in use", held.Addr, err)
	}
	if !SocketListens(t, inode) {
		t.Fatalf("/proc/net/tcp lists no listening socket of inode %s while the child holds it, so it cannot say when nothing does", inode)
	}

	if _, err := io.WriteString(serve, "serve\n"); err != nil {
		t.Fatalf("tell the child to serve: %v\n%s", err, child.Output.String())
	}
	_ = serve.Close()
	conn, err := net.Dial("tcp", held.Addr)
	if err != nil {
		t.Fatalf("dial %s: %v\n%s", held.Addr, err, child.Output.String())
	}
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		_ = conn.Close()
		t.Fatalf("read the answer on %s: %v\n%s", held.Addr, err, child.Output.String())
	}
	if got, want := strings.TrimSpace(answer), strconv.Itoa(cmd.Process.Pid); got != want {
		_ = conn.Close()
		t.Fatalf("%s answered pid %q; want the child's %s", held.Addr, got, want)
	}
	_ = conn.Close()
	child.WaitExit(t, "the test closed its connection")
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("the child exited %d:\n%s", code, child.Output.String())
	}

	// The test's cleanup has not run yet, so no listening socket of the inode now shows Start closed
	// the test's copy, and the child released its own on exit.
	if SocketListens(t, inode) {
		t.Fatalf("a socket of inode %s still listens after the child exited: the test's copy or the child's was left open", inode)
	}
}

// StartThen runs afterExit once the process has exited and before it closes Exited, so a test
// that waits on Exited sees what afterExit did. afterExit sleeps before it records anything, so
// an Exited closed before afterExit returned would show here as nothing recorded.
func TestStartThenRunsAfterExitBeforeExited(t *testing.T) {
	// -test.run=^$ makes this test binary run no test and exit 0.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	var runs int
	var exitedFirst bool
	child := StartThen(t, "the exiting child", cmd, func() {
		exitedFirst = cmd.ProcessState != nil
		time.Sleep(200 * time.Millisecond)
		runs++
	})
	child.WaitExit(t, "its start")
	if runs != 1 {
		t.Fatalf("afterExit had run %d times when Exited closed; want 1", runs)
	}
	if !exitedFirst {
		t.Fatalf("afterExit ran before the process's exit was recorded (Cmd.ProcessState was nil)")
	}
}
