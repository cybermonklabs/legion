// packages/envoy/internal/cipher/helper/peer_test.go
//go:build linux

package helper

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// childOutlives is how long a child process of a test is to live, as `sleep`'s operand: until the
// test binary's deadline (t.Deadline) and a minute past it, or for ever when the binary has none
// (-timeout 0). A child that ended at a fixed 30 s was retired by the helper ("session retired
// why=process exited") under a test the machine held that long, and the row then failed on what it
// was not testing (Tester5002, for #5023). No test outlives its binary's deadline, so no held test
// outlives such a child; and a child whose binary was killed before its cleanup ran (SIGKILL, go
// test's alarm, a panic on another goroutine) still ends by itself a minute after that deadline.
func childOutlives(t *testing.T) string {
	deadline, ok := t.Deadline()
	if !ok {
		return "infinity"
	}
	return strconv.Itoa(int((time.Until(deadline) + time.Minute + time.Second - 1) / time.Second))
}

// sleeper starts a child process of this test that lives until its cleanup kills it, or by itself
// until a minute past the test binary's deadline (childOutlives).
func sleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", childOutlives(t))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// ompTree starts a session launcher with no session — a process whose kernel comm is `omp`, as a
// real launcher's is: sh started through a symlink of that name — and n `sleep` children of it,
// each living as long as sleeper's (childOutlives), and returns the launcher's pid and the
// children's. It fails, rather than going on, when the launcher's comm does not read `omp`: a test
// of the launcher check needs a launcher it accepts.
func ompTree(t *testing.T, n int) (int, []int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(t.TempDir(), "omp")
	if err := os.Symlink(sh, launcher); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(launcher, "-c", `i=0; while [ "$i" -lt "$1" ]; do sleep "$2" & echo "$!"; i=$((i + 1)); done; wait`, "omp", strconv.Itoa(n), childOutlives(t))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() })
	lines := bufio.NewScanner(stdout)
	children := make([]int, 0, n)
	for len(children) < n && lines.Scan() {
		pid, err := strconv.Atoi(strings.TrimSpace(lines.Text()))
		if err != nil {
			t.Fatalf("the launcher printed %q, want a child's pid", lines.Text())
		}
		children = append(children, pid)
	}
	if len(children) != n {
		t.Fatalf("the launcher started %d children, want %d", len(children), n)
	}
	if comm, err := procComm(cmd.Process.Pid); err != nil || comm != "omp" {
		t.Fatalf("the launcher's comm is %q (%v), want omp", comm, err)
	}
	return cmd.Process.Pid, children
}

// TestOpenAncestorPinsAStrictAncestorOfThePeer: the helper takes as a session root only a live,
// strict ancestor of the calling process that this same user owns and that is a session
// launcher (its comm is `omp`). The peer here is a child of a launcher; a sibling, the peer
// itself, pid 1 and a pid that is not there are each refused, and so is an ancestor another login
// owns — a `login` or `sudo` above this one would otherwise be pinnable as a session root by any
// process under it — and an ancestor that is no launcher: this test process, above the
// launcher, stands for a tmux server or a login shell, whose whole tree a caller would otherwise
// make one session, every session started under it later included.
func TestOpenAncestorPinsAStrictAncestorOfThePeer(t *testing.T) {
	omp, children := ompTree(t, 1)
	sibling := sleeper(t)
	peer, err := PinPID(children[0])
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()

	root, err := openAncestor(peer, omp, procParent, processUID, procComm)
	if err != nil {
		t.Fatalf("the peer's launcher must be pinnable as its session root: %v", err)
	}
	if got := root.PID(); got != omp {
		t.Fatalf("the pinned root is %d, want the launcher %d", got, omp)
	}
	root.Close()

	for _, tc := range []struct {
		name string
		root int
		uid  func(int) (uint32, error)
	}{
		{"a sibling", sibling.Process.Pid, processUID},
		{"the peer itself", children[0], processUID},
		{"pid 1", 1, processUID},
		{"a pid that is not there", 1 << 22, processUID},
		{"an ancestor another login owns", omp, func(pid int) (uint32, error) {
			if pid == omp {
				return 4242, nil
			}
			return processUID(pid)
		}},
		{"an ancestor that is no session launcher", os.Getpid(), processUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := openAncestor(peer, tc.root, procParent, tc.uid, procComm)
			if err == nil {
				root.Close()
				t.Fatalf("openAncestor(%s) must refuse", tc.name)
			}
		})
	}
}

// TestDescendsFromRealProcesses: descendsFrom over /proc says a child descends from its parent and
// a process from itself, never a parent from its child or pid 1 from anything, and every one of
// those real walks reaches root or init without an error.
func TestDescendsFromRealProcesses(t *testing.T) {
	child := sleeper(t)
	self := os.Getpid()
	for _, tc := range []struct {
		name      string
		pid, root int
		want      bool
	}{
		{"a child descends from the test process", child.Process.Pid, self, true},
		{"a process descends from itself", self, self, true},
		{"a parent does not descend from its child", self, child.Process.Pid, false},
		{"pid 1 descends from nothing", 1, self, false},
	} {
		if in, err := descendsFrom(tc.pid, tc.root, procParent); in != tc.want || err != nil {
			t.Fatalf("%s: descendsFrom(%d, %d) = %v, %v; want %v, nil", tc.name, tc.pid, tc.root, in, err, tc.want)
		}
	}
}

func TestStartTicksNamesOneIncarnation(t *testing.T) {
	child := sleeper(t)
	ticks, err := StartTicks(child.Process.Pid)
	if err != nil || ticks == 0 {
		t.Fatalf("start ticks: %d %v", ticks, err)
	}
	if _, err := StartTicks(1 << 22); err == nil {
		t.Fatal("a pid that does not exist must error")
	}
}

func TestPinnedPeerReportsExit(t *testing.T) {
	child := sleeper(t)
	peer, err := PinPID(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if got := peer.PID(); got != child.Process.Pid {
		t.Fatalf("pinned pid %d, want %d", got, child.Process.Pid)
	}
	exited := make(chan bool, 1)
	go func() { exited <- peer.WaitExit(nil) }()
	_ = child.Process.Kill()
	_ = child.Wait()
	select {
	case ok := <-exited:
		if !ok {
			t.Fatal("WaitExit returned false for an exited process")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitExit did not observe the exit within 3 s")
	}
	if got := peer.PID(); got != 0 {
		t.Fatalf("a dead pinned process must read as pid 0, got %d", got)
	}
}

func TestWaitExitStops(t *testing.T) {
	child := sleeper(t)
	peer, err := PinPID(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- peer.WaitExit(stop) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("WaitExit must return false when stopped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WaitExit ignored stop")
	}
}

func TestPeerOfUnixConnIsTheDialer(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peer, err := PeerOf(server)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if got := peer.PID(); got != os.Getpid() {
		t.Fatalf("peer pid %d, want this process %d", got, os.Getpid())
	}
}

func TestCallRoundTrip(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "h.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadBytes('\n')
		_, _ = conn.Write([]byte(`{"ok":true,"proof":"p","enrollment_id":"e"}` + "\n"))
	}()
	resp, err := Call(sock, Request{Op: "sign", Method: "GET", URL: "https://x/v1/enrollments/self"}, 2*time.Second)
	if err != nil || !resp.OK || resp.Proof != "p" || resp.EnrollmentID != "e" {
		t.Fatalf("round trip: %+v %v", resp, err)
	}
	if _, err := Call(filepath.Join(shortTempDir(t), "absent.sock"), Request{Op: "sessions"}, time.Second); err == nil {
		t.Fatal("an absent socket must error")
	}
}
