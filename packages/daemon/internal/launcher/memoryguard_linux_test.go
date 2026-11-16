//go:build linux

package launcher

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/memoryguard"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"golang.org/x/sys/unix"
)

// memoryGuardRoleEnv is the marker of TestMemoryGuardOnRealRoleNamespaces' two helpers: the test
// binary re-entered as a role's role-init ("init") and as the process that init starts the way
// role-init starts its shim ("hold").
const memoryGuardRoleEnv = "LEGION_MEMORY_GUARD_TEST_ROLE"

// The helpers of this file are the test binary re-executed with its own LEGION_ marker, so
// TestMain runs no Guard in them (helperChildVariables, main_test.go). The file builds only under
// Linux, where a role's namespaces exist, so it registers its marker itself, as roleinit_test.go
// does.
func init() {
	helperChildVariables = append(helperChildVariables, memoryGuardRoleEnv)
}

// memoryGuardHeld is what the holder writes and keeps: well above the floor of
// TestMemoryGuardOnRealRoleNamespaces' fake limit, so the guard may end it.
const memoryGuardHeld = 64 << 20

// The guard reads and ends a process in a real role's namespaces. The test process stands in for
// the launcher: it starts a child in the namespaces a role's role-init is cloned into, and that
// child starts a holder of 64 MiB in a nested user namespace, as role-init starts the shim. The
// guard reads the holder's stat, exe, cmdline, ns/pid and smaps from the test's own /proc, takes
// its role from the launcher's classifier by its role-init ancestor, and ends it through the pidfd
// kill the guard runs on. The holder is the child's own, so the child waits on it and writes the
// wait status it read to the test, which reads SIGKILL there. The guard reads a fake meminfo and
// limit at its mark, and a /proc whose only processes are the test's, the child's and the
// holder's, each a link to the process's own directory in /proc, so the guard can end nothing of
// the machine's.
func TestMemoryGuardOnRealRoleNamespaces(t *testing.T) {
	switch os.Getenv(memoryGuardRoleEnv) {
	case "init":
		memoryGuardRoleInit()
	case "hold":
		memoryGuardHold()
	}
	requireRoleNamespaces(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer statusRead.Close()
	attr, err := roleNamespaceAttr()
	if err != nil {
		t.Fatal(err)
	}
	initProcess := exec.Command(self, "-test.run=^TestMemoryGuardOnRealRoleNamespaces$")
	initProcess.Env = append(os.Environ(), memoryGuardRoleEnv+"=init")
	initProcess.ExtraFiles = []*os.File{readyWrite, statusWrite}
	initProcess.SysProcAttr = attr
	output := &lockedBuffer{}
	initProcess.Stdout, initProcess.Stderr = output, output
	if err := initProcess.Start(); err != nil {
		t.Fatalf("start the role-init: %v", err)
	}
	roleInitPID := initProcess.Process.Pid
	_ = readyWrite.Close()
	_ = statusWrite.Close()
	// Killing the role-init ends its PID namespace, and the holder with it, whatever the test
	// reached.
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = initProcess.Process.Kill()
			_ = initProcess.Wait()
		}
	})
	bound := testwait.Bounded(t, time.Minute, time.Second)
	if err := readyRead.SetReadDeadline(time.Now().Add(bound)); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(readyRead).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("the holder said %q (%v), want ready; the role's output: %s", line, err, output.String())
	}
	holder := childOf(t, roleInitPID)
	if holder == 0 {
		t.Fatalf("the role-init %d has no child after its holder said it was ready; the role's output: %s", roleInitPID, output.String())
	}

	proc, cgroup := t.TempDir(), t.TempDir()
	for _, pid := range []int{os.Getpid(), roleInitPID, holder} {
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(pid)), filepath.Join(proc, strconv.Itoa(pid))); err != nil {
			t.Fatal(err)
		}
	}
	const limit = 512 << 20
	writeTestFile(t, filepath.Join(proc, "meminfo"), fmt.Sprintf("AnonPages:      %d kB\nShmem:              32 kB\n", (480<<20)>>10))
	writeTestFile(t, filepath.Join(cgroup, "memory.limit_in_bytes"), strconv.Itoa(limit)+"\n")
	writeTestFile(t, filepath.Join(cgroup, "memory.usage_in_bytes"), strconv.Itoa(limit/2)+"\n")

	classifier := newPodClassifier(t.TempDir(), []*manager{{role: "tester", child: &child{pid: roleInitPID}}}, &pidGuard{})
	var mu sync.Mutex
	var read *memoryguard.Process
	var classes map[int]memoryguard.Class
	kills := make(chan memoryguard.Kill, 1)
	lines := &lockedBuffer{}
	guard := memoryguard.New(memoryguard.Config{
		Proc: proc, Cgroup: cgroup, Clock: newGuardClock(),
		Classify: func(table *memoryguard.Table) map[int]memoryguard.Class {
			answer := classifier.classify(table)
			// The hour's scan for the largest process runs beside the kill, and may read the
			// pod once the holder is gone; the scan the kill was decided on reads it.
			if p, ok := table.Get(holder); ok {
				mu.Lock()
				defer mu.Unlock()
				read, classes = p, answer
			}
			return answer
		},
		Sink: func(k memoryguard.Kill) { kills <- k },
		Logf: func(format string, args ...any) { fmt.Fprintf(lines, format+"\n", args...) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		guard.Run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	var kill memoryguard.Kill
	select {
	case kill = <-kills:
	case <-time.After(bound):
		t.Fatalf("the guard ended nothing; its log:\n%s", lines.String())
	}
	if kill.PID != holder || kill.Role != "tester" || kill.SizeBytes < memoryGuardHeld {
		t.Fatalf("the guard ended %+v, want the holder %d of the tester, at least %d bytes; its log:\n%s", kill, holder, memoryGuardHeld, lines.String())
	}
	mu.Lock()
	holderRead, holderClass, initClass := read, classes[holder], classes[roleInitPID]
	mu.Unlock()
	switch {
	case holderRead == nil:
		t.Fatal("the guard's scan did not read the holder")
	case holderRead.PPID != roleInitPID || holderRead.Exe != self || !slices.Contains(holderRead.Args, "-test.run=^TestMemoryGuardOnRealRoleNamespaces$") ||
		!strings.HasPrefix(holderRead.PIDNamespace, "pid:["):
		t.Fatalf("the guard's scan read the holder as %+v, want the role-init %d's child running %s in a PID namespace", holderRead, roleInitPID, self)
	case holderClass != (memoryguard.Class{Role: "tester"}):
		t.Fatalf("the classifier says %+v of the holder, want the tester's by its role-init ancestor, not protected", holderClass)
	case initClass != (memoryguard.Class{Role: "tester", Protected: true}):
		t.Fatalf("the classifier says %+v of the role-init, want the tester's, protected", initClass)
	}

	if err := statusRead.SetReadDeadline(time.Now().Add(bound)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(statusRead).ReadString('\n')
	if err != nil {
		t.Fatalf("the role-init's report of the holder's wait status: %v; the role's output: %s", err, output.String())
	}
	raw, err := strconv.ParseUint(strings.TrimSpace(line), 10, 32)
	if err != nil {
		t.Fatalf("the role-init reported %q, want a wait status; the role's output: %s", line, output.String())
	}
	if status := syscall.WaitStatus(raw); !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("the holder's wait status is %#x, want SIGKILL", raw)
	}
	waited = true
	if err := initProcess.Wait(); err != nil {
		t.Fatalf("the role-init: %v; its output: %s", err, output.String())
	}
}

// memoryGuardRoleInit is the role-init of TestMemoryGuardOnRealRoleNamespaces: in a role's
// namespaces, it mounts the role's own /proc, as role-init does before it starts the shim
// (prepareRoleNamespace), since the nested user namespace's mappings are written through the
// /proc of the PID namespace the holder is started in; then it starts the holder in that nested
// user namespace as the image's unprivileged user, as runRoleShim starts the shim, hands it the
// test's ready pipe (fd 3), waits on it, and writes the wait status it read on the test's status
// pipe (fd 4).
func memoryGuardRoleInit() {
	ready, status := os.NewFile(3, "ready"), os.NewFile(4, "status")
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(1)
	}
	if err := privateMounts(); err != nil {
		fmt.Fprintln(os.Stderr, "role-init:", err)
		os.Exit(1)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		fmt.Fprintln(os.Stderr, "role-init: mount the role's own /proc:", err)
		os.Exit(1)
	}
	hold := exec.Command(self, "-test.run=^TestMemoryGuardOnRealRoleNamespaces$")
	hold.Env = append(os.Environ(), memoryGuardRoleEnv+"=hold")
	hold.ExtraFiles = []*os.File{ready}
	hold.Stdout, hold.Stderr = os.Stdout, os.Stderr
	hold.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:                    true,
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	if err := hold.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "role-init: start the holder:", err)
		os.Exit(1)
	}
	_ = ready.Close()
	_ = hold.Wait()
	ws, _ := hold.ProcessState.Sys().(syscall.WaitStatus)
	fmt.Fprintf(status, "%d\n", uint32(ws))
	os.Exit(0)
}

// memoryGuardHold is the holder: it writes every page of memoryGuardHeld bytes, says so on the
// ready pipe, and keeps them until it is killed, or for two minutes, so a test that fails before
// the kill leaves nothing behind for long.
func memoryGuardHold() {
	held := make([]byte, memoryGuardHeld)
	for i := 0; i < len(held); i += os.Getpagesize() {
		held[i] = 1
	}
	ready := os.NewFile(3, "ready")
	fmt.Fprintln(ready, "ready")
	_ = ready.Close()
	time.Sleep(2 * time.Minute)
	runtime.KeepAlive(held)
	os.Exit(0)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
