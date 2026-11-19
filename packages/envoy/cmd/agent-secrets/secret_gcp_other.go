//go:build !unix

// packages/envoy/cmd/agent-secrets/secret_gcp_other.go
//
// On a platform that is not Unix, runGcloud ends a gcloud whose context has ended by killing its
// process alone, exec.CommandContext's own default, and an end by a signal kills that process
// alone too. There is no group, so nothing is killed once gcloud has been waited for: its pid may
// name another process by then.
package main

import (
	"os"
	"os/exec"
)

func killGroupOnCancel(*exec.Cmd) {}

// killGcloudGroup kills gcloud's process, pid, which runRecorded has not yet waited for.
func killGcloudGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}

func killGroupLeft(int) {}
