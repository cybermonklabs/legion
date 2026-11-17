//go:build unix

package clitest

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

// waitGroup waits until no process of the group cmd leads is left and answers true, or, when ctx
// ends first, kills what is left and answers false. Call it once cmd.Wait has returned.
func waitGroup(ctx context.Context, cmd *exec.Cmd) bool {
	group := -cmd.Process.Pid
	tick := time.NewTicker(groupPoll)
	defer tick.Stop()
	for {
		if err := syscall.Kill(group, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		select {
		case <-ctx.Done():
			_ = syscall.Kill(group, syscall.SIGKILL)
			return false
		case <-tick.C:
		}
	}
}
