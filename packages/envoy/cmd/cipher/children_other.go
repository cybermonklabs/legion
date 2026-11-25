//go:build !unix

// packages/envoy/cmd/cipher/children_other.go
//
// On a platform that is not Unix, runChild ends a child whose context has ended by killing its
// process alone, exec.CommandContext's own default, and an end by a signal kills that process
// alone too. There is no group, so nothing is killed once the child has been waited for: its pid
// may name another process by then.
package main

import (
	"os"
	"os/exec"
)

func killGroupOnCancel(*exec.Cmd) {}

// killChildGroup kills the child's process, pid, which runChild has not yet waited for.
func killChildGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func killGroupLeft(int) {}
