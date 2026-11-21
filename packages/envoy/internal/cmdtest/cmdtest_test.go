package cmdtest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/sockettest"
	"github.com/sjawhar/envoy/listenfd"
)

const (
	// heldPortChildEnv makes this test binary run heldPortChild instead of its tests.
	heldPortChildEnv = "CMDTEST_HELD_PORT_CHILD"
	// heldPortFD is the variable Pass names the child's inherited socket in.
	heldPortFD = "CMDTEST_LISTEN_FD"
)

func TestMain(m *testing.M) {
	if os.Getenv(heldPortChildEnv) == "1" {
		os.Exit(heldPortChild())
	}
	os.Exit(m.Run())
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
// the process serves on the socket, and the socket is gone once it exits.
func TestHeldPortStaysHeldUntilItsProcessServes(t *testing.T) {
	held := HoldPort(t)
	// The socket itself, by its inode, which the checks below read rather than its port: once the
	// socket is gone, any other process on the machine may bind the port.
	inode := socketInode(t, held.file)
	self := os.Getpid()
	checkSocket(t, "after HoldPort", inode, map[int]int{self: 1}, 1)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), heldPortChildEnv+"=1")
	held.Pass(cmd, heldPortFD)
	serve, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	child := Start(t, "the held-port child", cmd)
	checkSocket(t, "after Start, before the child served", inode, map[int]int{self: 0, cmd.Process.Pid: 1}, 1)

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

	// The test's cleanup has not run yet, so a socket gone now shows Start closed the test's copy
	// and the child released its own on exit.
	checkReleased(t, held, inode, map[int]int{self: 0, cmd.Process.Pid: 0})
}

// checkReleased fails the test unless the held socket is gone once every process holding it has
// closed it or exited. On Linux it reads the socket's holders and TCP rows (checkSocket), and also
// that nothing else they started holds a copy. Elsewhere there is no /proc, so it binds held.Addr
// again and fails if the port is not free, as the test did before it read /proc: a bind can fail
// because another program on the machine took the freed port, which is why Linux reads the socket
// instead, and no CI runner is off Linux.
func checkReleased(t *testing.T, held *HeldPort, inode uint64, copies map[int]int) {
	t.Helper()
	if runtime.GOOS == "linux" {
		checkSocket(t, "after the child exited", inode, copies, 0)
		return
	}
	rebound, err := net.Listen("tcp", held.Addr)
	if err != nil {
		t.Fatalf("bind %s after the child exited: %v; want it free", held.Addr, err)
	}
	_ = rebound.Close()
}

// socketInode is the inode of the socket file holds: /proc/<pid>/fd names the socket
// socket:[<inode>] in every process holding it, and its rows in /proc/net/tcp carry it.
func socketInode(t *testing.T, file *os.File) uint64 {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat the held socket: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat the held socket: %T is no syscall.Stat_t", info.Sys())
	}
	return stat.Ino
}

// checkSocket fails the test, naming when, unless each pid in copies holds the socket of inode on
// that many descriptors (sockettest.Descriptors), and the TCP tables list it in rows rows
// (sockettest.TCPRows): one while any process holds it listening, none once every holder has closed
// it. A process that has exited and been reaped holds none. It reads /proc, so off Linux it checks
// nothing and says so; checkReleased binds the port again there instead.
func checkSocket(t *testing.T, when string, inode uint64, copies map[int]int, rows int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Logf("%s: the held socket's holders are read from /proc, which %s has not; not checked", when, runtime.GOOS)
		return
	}
	for pid, want := range copies {
		got, err := sockettest.Descriptors(pid, inode)
		if err != nil {
			t.Fatalf("%s: read process %d's descriptors: %v", when, pid, err)
		}
		if len(got) != want {
			t.Errorf("%s: process %d holds the held socket on descriptors %v, want %d of them", when, pid, got, want)
		}
	}
	all, err := sockettest.TCPRows()
	if err != nil {
		t.Fatalf("%s: read the TCP tables: %v", when, err)
	}
	listed := slices.DeleteFunc(all, func(row sockettest.Row) bool { return row.Inode != inode })
	if len(listed) != rows {
		t.Errorf("%s: the TCP tables list the held socket in %d rows %+v, want %d", when, len(listed), listed, rows)
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
