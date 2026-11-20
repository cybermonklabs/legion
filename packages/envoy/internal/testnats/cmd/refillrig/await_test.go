//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAwaitEndsTheFrontBeforeItKills runs the awaiter as e2e-refill.sh's interrupt(restart) does,
// on a stand-in front and a stand-in server, and reads, at the moment the server has exited,
// whether every process of the front had exited already: a front still running then would
// forward to the port the server released (scripts/e2e/lib/held-front.sh).
func TestAwaitEndsTheFrontBeforeItKills(t *testing.T) {
	front, child := startFront(t)
	server := exec.Command("sleep", "60")
	start(t, server)
	fronts := map[string]int{
		"leader " + strconv.Itoa(front.Process.Pid): pidFD(t, front.Process.Pid),
		"child " + strconv.Itoa(child):              pidFD(t, child),
	}
	serverFD := pidFD(t, server.Process.Pid)

	log := filepath.Join(t.TempDir(), "listener.log")
	line := fmt.Sprintf(`{"time":%q,"msg":"session registry bucket was recreated; reading it"}`+"\n",
		time.Now().Format(time.RFC3339Nano))
	if err := os.WriteFile(log, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	// alive receives, the moment the server has exited, the front's processes still running then.
	alive := make(chan []string, 1)
	go func() {
		if exited, err := exitedFD(serverFD, 10*time.Second); !exited {
			alive <- []string{fmt.Sprintf("(the server did not exit within 10 s: %v)", err)}
			return
		}
		var running []string
		for name, fd := range fronts {
			if exited, err := exitedFD(fd, 0); !exited {
				running = append(running, fmt.Sprintf("%s (%v)", name, err))
			}
		}
		alive <- running
	}()
	if err := runAwait([]string{"-log", log, "-match", "session registry bucket was recreated;", "-timeout", "5s",
		"-signal", "KILL", "-pid", strconv.Itoa(server.Process.Pid), "-front", strconv.Itoa(front.Process.Pid)}); err != nil {
		_ = server.Process.Kill()
		<-alive
		t.Fatalf("refillrig await: %v", err)
	}
	if running := <-alive; len(running) > 0 {
		t.Fatalf("the front's processes %s were running once the server had exited", strings.Join(running, ", "))
	}
}

// TestEndFrontLeavesNoStoppedGroup makes endFront's stop wait fail, as it does when the front's
// leader stays in uninterruptible sleep (D) for the whole grace, and then ends the front as
// e2e-refill.sh's stop_front does: SIGTERM to the group, and a wait on the leader. A stopped
// process acts on no signal but SIGKILL and SIGCONT, so a group that endFront left stopped makes
// that wait last for ever.
func TestEndFrontLeavesNoStoppedGroup(t *testing.T) {
	front, child := startFront(t)
	childFD := pidFD(t, child)
	// Each state as /proc gives it, except that a stopped process reads as D.
	neverStopped := func(pid int) byte {
		if state := procState(pid); state != 'T' {
			return state
		}
		return 'D'
	}
	if err := endFront(front.Process.Pid, 200*time.Millisecond, neverStopped); err == nil {
		t.Fatal("endFront succeeded, though its stop wait never saw the leader stop")
	}
	if err := syscall.Kill(-front.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- front.Wait() }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-front.Process.Pid, syscall.SIGKILL)
		<-waited
		t.Fatal("a wait on the front's leader had not returned 3 s after its group was sent SIGTERM: endFront left the group stopped")
	}
	if exited, err := exitedFD(childFD, 3*time.Second); !exited {
		t.Fatalf("the front's child %d had not exited 3 s after its group was sent SIGTERM (%v)", child, err)
	}
}

// TestEndFrontWaitsForAGroupWhoseLeaderExited ends a front whose leader has exited while a process
// of its group lives on, as a connection's socat can outlive the socat that forked it, and reads
// whether that process was gone once endFront returned: a process of the front still running then
// would forward to the port the server is about to release. The leader is left a zombie in one row
// and reaped in the other, the two ways /proc shows an exited leader. The test reads the group's
// process as held by the kernel (D) for the first 300 ms in which /proc shows it exited, as SIGKILL
// takes effect late on such a process (frontGrace), so an end that does not wait for that process
// returns before it reads as exited, or without reading it at all.
func TestEndFrontWaitsForAGroupWhoseLeaderExited(t *testing.T) {
	const hold = 300 * time.Millisecond
	for _, reaped := range []bool{false, true} {
		t.Run(map[bool]string{false: "a zombie leader", true: "a reaped leader"}[reaped], func(t *testing.T) {
			leader, child, childFD := startOrphan(t)
			if group, err := syscall.Getpgid(child); err != nil || group != leader.Process.Pid {
				t.Fatalf("the child %d is in group %d (%v), not in its leader's %d", child, group, err, leader.Process.Pid)
			}
			if reaped {
				_ = leader.Wait()
			}
			// exited is when a read of the child's state first found it exited.
			var exited time.Time
			state := func(pid int) byte {
				state := procState(pid)
				if pid != child || !gone(state) {
					return state
				}
				if exited.IsZero() {
					exited = time.Now()
				}
				if time.Since(exited) < hold {
					return 'D'
				}
				return state
			}
			if err := endFront(leader.Process.Pid, frontGrace, state); err != nil {
				t.Fatalf("endFront: %v", err)
			}
			if exited.IsZero() {
				t.Fatalf("endFront returned without reading whether the group's process %d had exited", child)
			}
			if held := time.Since(exited); held < hold {
				t.Fatalf("endFront returned %s after the group's process %d first read as exited, before it read as exited", held, child)
			}
			if ended, err := exitedFD(childFD, 0); !ended {
				t.Fatalf("the group's process %d was running when endFront returned (%v)", child, err)
			}
		})
	}
}

// TestMembers reads a process group's processes from a stand-in /proc, by the group each stat
// file names, whether or not the group's leader is still listed. A proc with no process's stat at
// all, or a stat that cannot be read or names no group, is an error, never an empty list.
func TestMembers(t *testing.T) {
	for _, row := range []struct {
		name  string
		files map[string]string // path under <proc>, its text; a text of "/" makes a directory
		want  []int
		err   string
	}{
		{name: "the leader, its child and a process of another group",
			files: map[string]string{"7/stat": "7 (sh) S 1 7 7 0", "12/stat": "12 (socat) S 7 7 7 0", "13/stat": "13 (sleep) S 1 13 13 0"},
			want:  []int{7, 12}},
		{name: "a child whose leader has exited",
			files: map[string]string{"12/stat": "12 (socat) T 1 7 7 0", "13/stat": "13 (sleep) S 1 13 13 0"}, want: []int{12}},
		{name: "command names with spaces and parentheses",
			files: map[string]string{"40/stat": "40 (x) S 1 7 7) S 1 13 13 0", "41/stat": "41 (a (b) c) T 1 7 7 0"}, want: []int{41}},
		{name: "entries that are no process, and a process that exited as it was read",
			files: map[string]string{"self/stat": "7 (sh) S 1 7 7 0", "net/dev": "", "14": "/", "12/stat": "12 (socat) S 1 7 7 0"},
			want:  []int{12}},
		{name: "a group with no process left", files: map[string]string{"13/stat": "13 (sleep) S 1 13 13 0"}},
		{name: "no process's stat", files: map[string]string{"self/stat": "7 (sh) S 1 7 7 0", "14": "/"}, err: "holds no process's stat"},
		{name: "a stat that cannot be read", files: map[string]string{"13/stat": "13 (sleep) S 1 13 13 0", "12/stat": "/"}, err: "12/stat"},
		{name: "a stat that names no group", files: map[string]string{"13/stat": "13 (sleep) S 1 13 13 0", "12/stat": "12 (socat) S"}, err: "12/stat names no group"},
	} {
		t.Run(row.name, func(t *testing.T) {
			proc := t.TempDir()
			for path, text := range row.files {
				path = filepath.Join(proc, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				var err error
				if text == "/" {
					err = os.Mkdir(path, 0o700)
				} else {
					err = os.WriteFile(path, []byte(text), 0o600)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := members(proc, 7)
			switch {
			case row.err == "" && err != nil:
				t.Fatalf("members: %v", err)
			case row.err != "" && (err == nil || !strings.Contains(err.Error(), row.err)):
				t.Fatalf("members returned %v, %v; want an error naming %s", got, err, row.err)
			case !slices.Equal(got, row.want):
				t.Fatalf("members returned %v; want %v", got, row.want)
			}
		})
	}
}

// startFront starts a stand-in front, a session leader (sh) with one child (sleep), as socat
// forks one process per connection, and returns it and its child's pid.
func startFront(t *testing.T) (*exec.Cmd, int) {
	t.Helper()
	front := exec.Command("sh", "-c", `sleep 60 & echo "$!"; wait`)
	front.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := front.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	start(t, front)
	var child int
	if _, err := fmt.Fscan(out, &child); err != nil {
		t.Fatalf("read the front's child pid: %v", err)
	}
	return front, child
}

// startOrphan starts a stand-in front whose leader (sh) exits as soon as it has started its one
// child (sleep), and returns the leader once it has exited, still unreaped, with its child's pid
// and a pidfd on the child. The test's end sends the child SIGKILL through that pidfd, which names
// no other process even once the child's pid is reused, and reaps the leader. No signal goes to the
// group: once its last process has exited, its number can name another program's group.
func startOrphan(t *testing.T) (*exec.Cmd, int, int) {
	t.Helper()
	leader := exec.Command("sh", "-c", `sleep 60 >/dev/null & echo "$!"`)
	leader.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := leader.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leader.Wait() })
	leaderFD := pidFD(t, leader.Process.Pid)
	var child int
	if _, err := fmt.Fscan(out, &child); err != nil {
		t.Fatalf("read the front's child pid: %v", err)
	}
	childFD := pidFD(t, child)
	t.Cleanup(func() { _ = unix.PidfdSendSignal(childFD, unix.SIGKILL, nil, 0) })
	if exited, err := exitedFD(leaderFD, 10*time.Second); !exited {
		t.Fatalf("the front's leader %d had not exited 10 s after it started its child (%v)", leader.Process.Pid, err)
	}
	return leader, child, childFD
}

// start starts cmd and, at the test's end, kills its process group (none, unless cmd leads one)
// and it, and reaps it.
func start(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// pidFD opens a pidfd on pid, which stays on that process even once its pid is reused.
func pidFD(t *testing.T, pid int) int {
	t.Helper()
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatalf("pidfd_open %d: %v", pid, err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}

// exitedFD reports whether the process of the pidfd has exited within timeout: a pidfd turns
// readable once its process is a zombie or gone. The error is the poll's own, if it failed.
func exitedFD(fd int, timeout time.Duration) (bool, error) {
	for {
		n, err := unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}, int(timeout.Milliseconds()))
		if !errors.Is(err, unix.EINTR) {
			return err == nil && n > 0, err
		}
	}
}
