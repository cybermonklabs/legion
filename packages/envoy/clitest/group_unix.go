//go:build unix

package clitest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// groupPoll is how often waitGroup asks whether a run's process group still has a process. The
// processes a binary leaves behind are not the harness's children, so no wait reports their exit.
const groupPoll = 10 * time.Millisecond

// inOwnGroup makes cmd the leader of a process group of its own, which every process it starts
// joins unless that process leaves it, and kills the whole group when the run's context ends.
func inOwnGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
}

// waitGroup waits until every process of the group cmd leads has ended and answers true, or, when
// ctx ends first, kills what is left and answers false. Call it once cmd.Wait has returned.
func waitGroup(ctx context.Context, cmd *exec.Cmd) bool {
	group := cmd.Process.Pid
	tick := time.NewTicker(groupPoll)
	defer tick.Stop()
	for {
		if groupEnded(group) {
			return true
		}
		select {
		case <-ctx.Done():
			_ = syscall.Kill(-group, syscall.SIGKILL)
			return false
		case <-tick.C:
		}
	}
}

// groupEnded reports whether every process of group pgid has ended. Signal 0 to the group reaches
// every member, a zombie included: a process the binary left behind is an orphan once the binary
// exits, and its zombie waits for whatever the kernel handed it to. Where that never reaps it, as
// where the namespace's PID 1 is a `go test` with no init above it, the zombie stays and signal 0
// still reaches the group. So while it does, /proc is read for a member that is not dead (Z or X);
// where /proc cannot be read, as on a system with none, the group counts as running.
func groupEnded(pgid int) bool {
	if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	want := []byte(strconv.Itoa(pgid))
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// The fields after the command name, which ends at the last ')': state, parent, group.
		fields := bytes.Fields(stat[bytes.LastIndexByte(stat, ')')+1:])
		if len(fields) < 3 || !bytes.Equal(fields[2], want) {
			continue
		}
		if state := fields[0]; !bytes.Equal(state, []byte("Z")) && !bytes.Equal(state, []byte("X")) {
			return false
		}
	}
	return true
}
