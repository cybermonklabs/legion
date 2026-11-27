package main

//ci:builds packages/envoy ./cmd/listener buildListener builds . in ../../../cmd/listener (root go.work)

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/envoy/infra/internal/pulumitest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/sockettest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The wrapper's real-process tests: the wrapper this directory builds, run as the held-port image
// runs it, against the socket server the live tests use (pulumitest.HoldPort) and the real
// envoy-listener of this branch.

// TestMain makes this test process the subreaper of what it starts (becomeSubreaper), so what the
// wrapper leaves behind stays among the test's descendants, which is all requireTheTestsAlone
// walks; it then runs the package's tests through the shared NATS harness's own Main
// (testnats.Main), which cmdtest.Run holds the build directory around, so each command the tests
// run is built once for the whole test process.
func TestMain(m *testing.M) {
	becomeSubreaper()
	os.Exit(cmdtest.Run(func() int { return testnats.Main(m) }))
}

// The wrapper takes the socket the test holds and becomes the listener, which serves on it: the
// listener runs in the wrapper's process, and of the socket it holds only its own close-on-exec
// copy, so nothing it starts inherits the port.
func TestTheListenerServesTheHeldSocketInTheWrappersProcess(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	listener := buildListener(t)
	held := pulumitest.HoldPort(t)
	port := held.Addr().(*net.TCPAddr).Port
	process := startWrapper(t, wrapper, port, listener)

	if addr, want := listeningAddr(t, process), held.Addr().String(); addr != want {
		t.Fatalf("the listener logged that it listens on %s, not on the held %s:\n%s", addr, want, process.Output.String())
	}
	requireAnswers(t, process, port)

	pid := process.Cmd.Process.Pid
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		t.Fatalf("read what process %d runs: %v", pid, err)
	}
	want, err := filepath.EvalSymlinks(listener)
	if err != nil {
		t.Fatal(err)
	}
	if exe != want {
		t.Fatalf("the wrapper's process %d runs %s, not the listener %s: the wrapper did not execute it in its place", pid, exe, want)
	}

	holders, err := sockettest.Descriptors(pid, listeningInode(t, port))
	if err != nil {
		t.Fatalf("read the listener's descriptors: %v", err)
	}
	if len(holders) != 1 {
		t.Fatalf("the listener holds the socket on descriptors %v, want exactly one: its own serving copy", holders)
	}
	if holders[0] == 3 {
		t.Fatal("the listener still holds the socket on descriptor 3, the one it inherited, rather than only its own copy")
	}
	if !closeOnExec(t, pid, holders[0]) {
		t.Fatalf("the listener's copy of the socket, descriptor %d, is not close-on-exec, so a process it starts inherits the port", holders[0])
	}
}

// With no test serving the abstract socket, the wrapper exits 1 at once, naming the socket it
// asked, and the port stays the test's alone.
func TestWithNoSocketServedTheWrapperExitsNamingIt(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a loopback port: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port := held.Addr().(*net.TCPAddr).Port
	process := startWrapper(t, wrapper, port, filepath.Join(t.TempDir(), "never-executed"))
	requireFailedNaming(t, process, pulumitest.HeldPortSocket(port))
	requireTheTestsAlone(t, held)
}

// A listener binary that does not exist ends the wrapper with exit 1, naming the socket, and leaves
// no process holding the socket it received: the port is the test's alone again.
func TestATargetThatCannotRunLeavesTheSocketTheTestsAlone(t *testing.T) {
	skipOffLinux(t)
	wrapper := cmdtest.Build(t, "envoy-listener-heldport")
	held := pulumitest.HoldPort(t)
	port := held.Addr().(*net.TCPAddr).Port
	missing := filepath.Join(t.TempDir(), "no-such-listener")
	process := startWrapper(t, wrapper, port, missing)
	requireFailedNaming(t, process, pulumitest.HeldPortSocket(port))
	if output := process.Output.String(); !strings.Contains(output, "execute "+missing) {
		t.Errorf("the wrapper did not say it failed to execute %s:\n%s", missing, output)
	}
	requireTheTestsAlone(t, held)
}

func skipOffLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("the held-port wrapper runs on linux only, and this is %s", runtime.GOOS)
	}
}

// buildListener builds packages/envoy/cmd/listener from this branch as Envoy's own jobs do, in the
// workspace of the repository's root go.work, and returns the binary.
func buildListener(t *testing.T) string {
	t.Helper()
	return cmdtest.BuildCommand(t, cmdtest.Command{Name: "envoy-listener", Dir: filepath.Join("..", "..", "..", "cmd", "listener")})
}

// startWrapper runs the wrapper as the held-port image runs it on the container's PORT, executing
// target, with the package's shared NATS: the listener the wrapper executes serves before it
// connects, and once it has connected it stays up, so /healthz answers for as long as the row
// reads it. A NATS that never answers ends the listener's start 60 s after its listening line
// ("connect to NATS and reconcile the ENVOY_NOTIFICATIONS stream: ... i/o timeout"), which made
// that 60 s the row's real bound (INFRA-3904, found by TesterHeldportLoadtest).
func startWrapper(t *testing.T, wrapper string, port int, target string) *cmdtest.Process {
	t.Helper()
	cmd := exec.Command(wrapper)
	cmd.Env = []string{
		"PORT=" + strconv.Itoa(port),
		"HELDPORT_WRAPPER_EXEC=" + target,
		"ENVOY_MACHINE_ID=heldport-wrapper-test",
		"NATS_URLS=" + testnats.URL(t),
	}
	return cmdtest.Start(t, "the wrapper", cmd)
}

// listeningRecord matches the listener's JSON "envoy-listener listening" line, capturing its addr.
var listeningRecord = regexp.MustCompile(`"msg":"envoy-listener listening".*"addr":"([^"]*)"`)

// listeningAddr waits for the listener's "envoy-listener listening" line and returns its addr.
func listeningAddr(t *testing.T, process *cmdtest.Process) string {
	t.Helper()
	process.WaitForOutput(t, `"msg":"envoy-listener listening"`)
	match := listeningRecord.FindStringSubmatch(process.Output.String())
	if match == nil {
		t.Fatalf("the listener's listening line carries no addr:\n%s", process.Output.String())
	}
	return match[1]
}

// requireAnswers waits for /healthz on port to answer at all, and fails the test when the process
// exits first or it never does. Each request carries the shared wait's context (testwait.Context),
// so no request has a bound of its own: a 5 s client timeout here cut every request of an answer
// that took longer, and the row then failed at the listener's own start bound rather than on its
// fact (INFRA-3904, found by TesterHeldportLoadtest).
func requireAnswers(t *testing.T, process *cmdtest.Process, port int) {
	t.Helper()
	ctx := testwait.Context(t)
	url := "http://127.0.0.1:" + strconv.Itoa(port) + "/healthz"
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	testwait.Until(t, fmt.Sprintf("an answer of any kind from /healthz on %d", port), func() (error, bool) {
		select {
		case <-process.Exited:
			t.Fatalf("the process exited before /healthz answered on %d:\n%s", port, process.Output.String())
		default:
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build the /healthz request: %v", err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		return err, err == nil
	})
}

// requireFailedNaming waits for the wrapper to exit, and fails the test unless it exited 1 naming
// socket.
func requireFailedNaming(t *testing.T, process *cmdtest.Process, socket string) {
	t.Helper()
	testwait.Receive(t, "the wrapper to exit", process.Exited)
	output := process.Output.String()
	if code := process.Cmd.ProcessState.ExitCode(); code != 1 {
		t.Errorf("the wrapper exited %d, want 1:\n%s", code, output)
	}
	if !strings.Contains(output, socket) {
		t.Errorf("the wrapper's refusal does not name %s:\n%s", socket, output)
	}
}

// requireTheTestsAlone fails the test when a process other than the test's holds the socket held
// listens on, then closes the test's copy and requires the socket gone from the TCP tables: no
// process kept it. It reads the socket by its inode, not a bind of its address, which any other
// process on the machine may take once the socket is closed.
func requireTheTestsAlone(t *testing.T, held net.Listener) {
	t.Helper()
	addr := held.Addr().String()
	inode := listeningInode(t, held.Addr().(*net.TCPAddr).Port)
	for pid, fds := range holdersOf(t, inode) {
		t.Errorf("process %d holds the socket on %s on descriptors %v", pid, addr, fds)
	}
	if err := held.Close(); err != nil {
		t.Fatalf("close the test's copy of the socket on %s: %v", addr, err)
	}
	rows, err := sockettest.TCPRows()
	if err != nil {
		t.Fatalf("read the TCP tables: %v", err)
	}
	if listed := slices.DeleteFunc(rows, func(row sockettest.Row) bool { return row.Inode != inode }); len(listed) != 0 {
		t.Fatalf("the TCP tables still list the socket on %s once the test closed its copy: %+v", addr, listed)
	}
}

// holdersOf returns each of this test's descendants that holds the socket of inode, with the
// descriptors it holds it on. Only a process the test started, or one that started, can have it:
// the test handed the socket to the wrapper alone, and a process the wrapper left behind is still
// the test's, its subreaper's (TestMain). So it reads those processes and no other of the machine.
func holdersOf(t *testing.T, inode uint64) map[int][]int {
	t.Helper()
	descendants, err := descendantsOf(os.Getpid())
	if err != nil {
		t.Fatalf("list this test's descendants: %v", err)
	}
	holders := map[int][]int{}
	for _, pid := range descendants {
		fds, err := sockettest.Descriptors(pid, inode)
		if err != nil {
			t.Fatalf("read process %d's descriptors: %v", pid, err)
		}
		if len(fds) > 0 {
			holders[pid] = fds
		}
	}
	return holders
}

// A process that holds the socket after the process it was started from has exited is still found:
// it was handed to this test, the subreaper, and not to the machine's init, so walking the test's
// descendants alone reaches what the wrapper could leave behind.
func TestAnOrphanThatHoldsTheSocketIsAHolder(t *testing.T) {
	skipOffLinux(t)
	held := pulumitest.HoldPort(t)
	inode := listeningInode(t, held.Addr().(*net.TCPAddr).Port)
	file, err := held.File()
	if err != nil {
		t.Fatalf("copy the held socket: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "orphan")
	// The shell starts a sleep holding the socket on descriptor 3, records its pid and exits, so
	// the sleep's parent is gone before the walk.
	cmd := exec.Command("sh", "-c", `sleep 300 & echo $! >"$1"`, "orphan", pidFile)
	cmd.ExtraFiles = []*os.File{file}
	err = cmd.Run()
	_ = file.Close()
	if err != nil {
		t.Fatalf("start the orphan: %v", err)
	}
	text, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := strconv.Atoi(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatalf("the orphan's pid %q: %v", text, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(orphan, syscall.SIGKILL) })
	if fds := holdersOf(t, inode)[orphan]; len(fds) == 0 {
		t.Fatalf("the orphan %d, whose parent exited, holds the socket, and the walk of this test's descendants did not find it", orphan)
	}
}

// listeningInode returns the inode of the one socket listening on 127.0.0.1:port.
func listeningInode(t *testing.T, port int) uint64 {
	t.Helper()
	rows, err := sockettest.TCPRows()
	if err != nil {
		t.Fatal(err)
	}
	// The local address as the host's byte order prints it (127.0.0.1 is 0100007F on the
	// little-endian machines the tests run on) and the port, in hexadecimal; state 0A is LISTEN.
	local := fmt.Sprintf("0100007F:%04X", port)
	var inodes []uint64
	for _, row := range rows {
		if row.Local == local && row.State == "0A" {
			inodes = append(inodes, row.Inode)
		}
	}
	if len(inodes) != 1 {
		t.Fatalf("the TCP tables list %d sockets listening on 127.0.0.1:%d, want exactly one", len(inodes), port)
	}
	return inodes[0]
}

// closeOnExec reports whether descriptor fd of process pid is close-on-exec, from the octal flags
// line of /proc/<pid>/fdinfo/<fd>.
func closeOnExec(t *testing.T, pid, fd int) bool {
	t.Helper()
	info, err := os.ReadFile(fmt.Sprintf("/proc/%d/fdinfo/%d", pid, fd))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(info), "\n") {
		if value, found := strings.CutPrefix(line, "flags:"); found {
			flags, err := strconv.ParseUint(strings.TrimSpace(value), 8, 64)
			if err != nil {
				t.Fatalf("fdinfo of descriptor %d of process %d has flags %q", fd, pid, value)
			}
			return flags&syscall.O_CLOEXEC != 0
		}
	}
	t.Fatalf("fdinfo of descriptor %d of process %d has no flags line:\n%s", fd, pid, info)
	return false
}
