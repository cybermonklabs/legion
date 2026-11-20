//go:build linux

package launcher

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// orphanHelperEnv marks the child run of TestALauncherWhoseTestBinaryIsKilledEndsAndDialsNoMore,
// naming the file it reports its launcher's pid in. TestMain runs no fixtureenv.Guard in such a
// child, which would take the marker out (helperChildVariables, main_test.go).
const orphanHelperEnv = "LEGION_LAUNCHER_ORPHAN_HELPER"

func init() {
	helperChildVariables = append(helperChildVariables, orphanHelperEnv)
}

// A launcher in a process of its own (processRig) ends when the test binary that started it is
// killed, and dials nothing more. One that outlived its binary would go on redialing its daemon's
// port with the hello every rig of this package takes, so a later test whose listener the kernel
// put on that port would read it as its own role. A child run of this test binary starts
// processRig, reports its launcher's pid and waits; the test kills that run with SIGKILL, as a
// timeout's panic or an OOM kill ends a test binary, and waits until the launcher has ended or
// its log shows a dial made after the kill.
//
// This file holds only what the rig had before the launcher's stdin ended it (processRig, zombie,
// shortTempDir, testwait.Eventually), so the same case runs against that rig.
func TestALauncherWhoseTestBinaryIsKilledEndsAndDialsNoMore(t *testing.T) {
	if report := os.Getenv(orphanHelperEnv); report != "" {
		processRig(t)
		launchers := childrenOf(t, os.Getpid())
		if len(launchers) != 1 {
			t.Fatalf("this run has the child processes %v, want processRig's launcher alone", launchers)
		}
		if err := os.WriteFile(report+".tmp", []byte(strconv.Itoa(launchers[0])), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(report+".tmp", report); err != nil {
			t.Fatal(err)
		}
		// Held until the test kills this run.
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}

	// The run's temporary directories, the launcher's log among them, are this test's, so the kill
	// leaves nothing of them behind.
	dir := shortTempDir(t)
	report := filepath.Join(dir, "launcher.pid")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(dir, "run.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	run := exec.Command(self, "-test.run=^TestALauncherWhoseTestBinaryIsKilledEndsAndDialsNoMore$", "-test.count=1", "-test.v")
	run.Env = append(os.Environ(), orphanHelperEnv+"="+report, "TMPDIR="+dir)
	run.Stdout, run.Stderr = output, output
	if _, err := run.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = run.Wait(); close(waited) }()
	t.Cleanup(func() { _ = run.Process.Kill(); <-waited })
	runLog := func() string {
		body, _ := os.ReadFile(output.Name())
		return string(body)
	}

	pid := 0
	testwait.Eventually(t, "the child run to report its launcher's pid", func() bool {
		select {
		case <-waited:
			t.Fatalf("the child run ended before it reported its launcher; its output:\n%s", runLog())
		default:
		}
		body, err := os.ReadFile(report)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(string(body))
		return err == nil
	})
	start, live := startOf(pid)
	if !live {
		t.Fatalf("launcher %d had ended before its run was killed; the run's output:\n%s", pid, runLog())
	}
	// Where the launcher outlives its run, the case's end takes it, and only it: a pid the kernel
	// has given another process since carries another start time.
	t.Cleanup(func() {
		if now, live := startOf(pid); live && now == start {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	// The launcher writes its log to the file processRig gave it as stderr, which the kill leaves
	// on disk.
	logPath, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", "2"))
	if err != nil {
		t.Fatalf("launcher %d's log: %v", pid, err)
	}
	launcherLog := func() string {
		body, _ := os.ReadFile(logPath)
		return string(body)
	}

	_ = run.Process.Signal(syscall.SIGKILL)
	<-waited
	// serveUntil logs each failed dial as "launcher: daemon connection: dial <address>: ...".
	dialed := func() bool { return strings.Contains(launcherLog(), "launcher: daemon connection: dial ") }
	testwait.Eventually(t, "launcher "+strconv.Itoa(pid)+" to end, or to dial again, once the binary that started it was killed", func() bool {
		if dialed() {
			return true
		}
		now, live := startOf(pid)
		return !live || now != start
	})
	if dialed() {
		t.Fatalf("launcher %d dialed its daemon again after the binary that started it was killed; its log:\n%s", pid, launcherLog())
	}
}

// childrenOf is every process whose parent is ppid, read from /proc.
func childrenOf(t *testing.T, ppid int) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var children []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if fields := statFields(pid); len(fields) > 1 && fields[1] == strconv.Itoa(ppid) {
			children = append(children, pid)
		}
	}
	return children
}

// startOf is pid's start time (field 22 of /proc/<pid>/stat), and false when pid has ended: it is
// gone, or a zombie no reaper has taken yet.
func startOf(pid int) (string, bool) {
	fields := statFields(pid)
	if len(fields) < 20 || zombie(pid) {
		return "", false
	}
	return fields[19], true
}

// statFields is the fields of /proc/<pid>/stat after the command in parentheses, which may itself
// hold spaces: the state first, then the parent's pid. It is nil when pid is gone.
func statFields(pid int) []string {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return nil
	}
	stat := string(body)
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return nil
	}
	return strings.Fields(stat[end+1:])
}
