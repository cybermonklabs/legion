//go:build unix

// packages/envoy/cmd/cipher/children_unix.go
//
// How runChild ends a child whose context has ended, where there are process groups.
package main

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd in a process group of its own, and makes the end of its context
// kill that whole group, not cmd's process alone: the tool on PATH can be a shell script that runs
// the real program as its child, and a kill of the script alone leaves that child running. A
// group that is gone already answers os.ErrProcessDone, as exec.Cmd's own kill does for a process
// that is done, so a child that ended by itself keeps its own exit status. runChild calls it
// before Start; replace cmd.SysProcAttr nowhere after it: without Setpgid the kill finds no group
// and nothing is killed at the deadline.
// In a group of its own the child does not get a signal sent to this command's group, such as a
// terminal's interrupt, so an end by such a signal kills the group itself (exitBySignal,
// killChildGroup). An end this command cannot handle, a SIGKILL, kills the child's process too
// where the platform can (gcloudProcAttr): nothing of this command is left to kill the group then.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = gcloudProcAttr()
	cmd.Cancel = func() error {
		err := signalGroup(cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// killChildGroup kills the process group the child, pid, leads (killGroupOnCancel); a group that
// is gone already is no error.
func killChildGroup(pid int) {
	_ = signalGroup(pid, syscall.SIGKILL)
}

// killGroupLeft kills what is left of the child's group once the child, pid, has been waited for
// (runChild).
func killGroupLeft(pid int) {
	killChildGroup(pid)
}

// signalGroup sends sig to the process group pgid. A test replaces it to see an answer the kernel
// gives only in a race (ESRCH, the group gone already) without sending any signal.
var signalGroup = func(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}
