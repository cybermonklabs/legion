//go:build linux

package launcher

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/hostproc"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// The conditions the launcher's git lock sweep met on a machine several users share, where
// TestTheLauncherTakesTheWorkspaceFromItsWorkingDirectory failed while it passed in CI: a process of
// another user is visible in the PID namespace the test runs in, its comm reads exactly `git`, and
// its fd directory cannot be read by the test's user. This row makes all three itself, with a real
// git run as `nobody` through `sudo -n`, and shows each from the test's own identity, since a
// /proc mounted with hidepid hides such a process or its files and would not reproduce the fault.
//
// Under those conditions the original test passes with the /proc of its own it gives the sweep,
// and a sweep given the namespace's /proc, with this row's reason, keeps a planted lock for the
// unreadable git: the rule that a git or jj whose open files cannot be read holds every lock stays
// as it is. In CI the test binary runs in a PID namespace of its own (.github/scripts/
// private_proc.py), whose /proc lists its processes alone; there the same sweep, before the git
// starts, removes the lock, which is the control that the git this row started is what keeps it.
func TestTheOriginalConditionsLeaveTheLaunchersOwnRootAndTheUnreadableGitRule(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads every process's open files, so no process is unreadable to this test")
	}
	// The probe runs from /, as the other user's git below does: a directory that user can stand in.
	probe := exec.Command("sudo", "-n", "-u", "nobody", "true")
	probe.Dir = "/"
	if err := probe.Run(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("a process of another user needs sudo -n -u nobody, which CI's runner gives: %v", err)
		}
		t.Skipf("no sudo -n -u nobody to start a process of another user: %v", err)
	}
	reason := hostproc.Because("this row's own sweep reads the /proc of the PID namespace the test runs in on purpose: what it judges is the other user's git the row starts there")
	sweepOnce := func(t *testing.T) (string, *lockedBuffer) {
		c := newLockClone(t)
		stale := c.lock("index.lock", "stale", time.Hour)
		log := &lockedBuffer{}
		s := c.sweep(log, "/proc")
		s.hostProc = reason
		s.pass(context.Background(), false)
		return stale, log
	}

	private := inPrivateNamespace()
	if private {
		t.Run("the control: with no git of another user, the namespace's sweep removes the lock", func(t *testing.T) {
			stale, log := sweepOnce(t)
			requireRemoved(t, log, stale)
		})
	} else {
		t.Log("not in a PID namespace of the test's own (private_proc.py), so the control is not run: this machine's /proc may hold a git of another user already")
	}

	git := otherUsersGit(t)
	t.Run("the three conditions, read as the test's user", func(t *testing.T) {
		dir := filepath.Join("/proc", strconv.Itoa(git))
		pids, err := hostproc.PIDs("the hostile row's listing", "/proc", reason)
		if err != nil || !slices.Contains(pids, git) {
			t.Fatalf("the other user's git %d is not listed in this namespace's /proc (%v): it is not visible", git, err)
		}
		if uid := statusUID(t, dir); uid == strconv.Itoa(os.Getuid()) {
			t.Fatalf("the git %d runs as uid %s, the test's own: it is no process of another user", git, uid)
		}
		comm, err := os.ReadFile(filepath.Join(dir, "comm"))
		if err != nil || string(comm) != "git\n" {
			t.Fatalf("the git %d's comm reads %q (%v), want exactly git", git, comm, err)
		}
		if _, err := os.ReadDir(filepath.Join(dir, "fd")); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("the git %d's fd directory answers %v, want a permission refusal", git, err)
		}
	})
	t.Run("the original test passes with its own root", TestTheLauncherTakesTheWorkspaceFromItsWorkingDirectory)
	t.Run("the namespace's sweep keeps the lock for the unreadable git", func(t *testing.T) {
		stale, log := sweepOnce(t)
		requireKept(t, log, stale, "a git or jj whose open files cannot be read is running")
	})
}

// inPrivateNamespace says whether the test binary runs beneath private_proc.py's supervisor, PID 1
// of a PID namespace of the binary's own.
func inPrivateNamespace() bool {
	cmdline, err := os.ReadFile("/proc/1/cmdline")
	return err == nil && strings.Contains(string(cmdline), "private_proc.py\x00--init\x00")
}

// otherUsersGit starts a git as `nobody`, in /, which that user can read, waiting on a standard input
// the test holds open, and answers its pid. The test's end closes that input, which ends the git,
// and waits for sudo. The git is a descendant of the sudo the test started: its child where sudo
// has no terminal, as in CI, and its grandchild where it has one, since Ubuntu's sudoers sets
// use_pty and sudo then runs the command under a monitor sudo of its own (descendsFrom).
func otherUsersGit(t *testing.T) int {
	t.Helper()
	sudo := exec.Command("sudo", "-n", "-u", "nobody", "git", "hash-object", "--stdin")
	sudo.Dir = "/"
	stdin, err := sudo.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sudo.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		if err := sudo.Wait(); err != nil {
			t.Errorf("the other user's git ended with %v once its input closed", err)
		}
	})
	reason := hostproc.Because("it takes only a descendant of the sudo this test started")
	var git int
	testwait.Eventually(t, "the sudo to start the other user's git", func() bool {
		pids, _ := hostproc.PIDs("the hostile row's listing", "/proc", reason)
		for _, pid := range pids {
			if descendsFrom(pid, sudo.Process.Pid) {
				comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
				if err == nil && string(comm) == "git\n" {
					git = pid
					return true
				}
			}
		}
		return false
	})
	return git
}

// descendsFrom says whether ancestor is pid's parent, or its parent's parent, or one more step up:
// sudo with use_pty runs its command under a monitor sudo, so the command is the grandchild of the
// sudo a test starts in a terminal.
func descendsFrom(pid, ancestor int) bool {
	for p, hops := parentPID(pid), 0; p > 1 && hops < 3; p, hops = parentPID(p), hops+1 {
		if p == ancestor {
			return true
		}
	}
	return false
}

// parentPID is the PPid /proc/<pid>/status holds, 0 when it cannot be read.
func parentPID(pid int) int {
	file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0
	}
	defer file.Close()
	return statusField(file, "PPid:")
}

// statusUID is the real uid /proc/<pid>/status holds for the process at dir.
func statusUID(t *testing.T, dir string) string {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "status"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	return strconv.Itoa(statusField(file, "Uid:"))
}

// statusField is the first number of the status line that starts with name, 0 when there is none.
func statusField(r io.Reader, name string) int {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if rest, ok := strings.CutPrefix(scanner.Text(), name); ok {
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				n, _ := strconv.Atoi(fields[0])
				return n
			}
		}
	}
	return 0
}
