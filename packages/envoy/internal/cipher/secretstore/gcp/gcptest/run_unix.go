//go:build unix

package gcptest

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run runs cmd as the agent-secrets CLI's tracker of the children its forms start runs it
// (secretstore.RunChild), for a test of the GCP writer with no CLI around it: in a process group
// of its own, which the end of cmd's context kills whole, with its wait for output bounded to a
// second once the process has exited, and with what is left of the group killed once cmd has been
// waited for. It watches no signal: the CLI's own tests cover that.
func Run(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = time.Second
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	err := cmd.Wait()
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return err
}
