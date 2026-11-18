//go:build unix

// packages/envoy/cmd/agent-secrets/secret_gcp_unix.go
//
// How runGcloud ends a gcloud whose context has ended, where there are process groups.
package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in a process group of its own, and makes the end of its context
// kill that whole group, not cmd's process alone: the gcloud on PATH can be a shell script that
// runs gcloud's Python as its child, and a kill of the script alone leaves that child running. A
// group that is gone already answers os.ErrProcessDone, as exec.Cmd's own kill does for a process
// that is done, so a gcloud that ended by itself keeps its own exit status. Call it on a Cmd from
// exec.CommandContext, before Start, and replace cmd.SysProcAttr nowhere after it: without
// Setpgid the kill finds no group and nothing is killed at the deadline.
// In a group of its own gcloud does not get a signal sent to this command's group, such as a
// terminal's interrupt, so an end by such a signal kills the group itself (exitBySignal,
// killGcloudGroup).
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// killGcloudGroup kills the process group gcloud, pid, leads (killGroupOnCancel).
func killGcloudGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
