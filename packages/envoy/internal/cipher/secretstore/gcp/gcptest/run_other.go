//go:build !unix

package gcptest

import (
	"os/exec"
	"time"
)

// Run runs cmd as the agent-secrets CLI's tracker of the children its forms start runs it
// (secretstore.RunChild) where the platform has no process groups: its wait for output bounded to
// a second once the process has exited.
func Run(cmd *exec.Cmd) error {
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = time.Second
	}
	return cmd.Run()
}
