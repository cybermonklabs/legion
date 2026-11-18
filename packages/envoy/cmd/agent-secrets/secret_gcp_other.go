//go:build !unix

// packages/envoy/cmd/agent-secrets/secret_gcp_other.go
//
// On a platform that is not Unix, runGcloud ends a gcloud whose context has ended by killing its
// process alone, exec.CommandContext's own default, and an end by a signal kills that process
// alone too.
package main

import (
	"os"
	"os/exec"
)

func killGroupOnCancel(*exec.Cmd) {}

// killGcloudGroup kills gcloud's process, pid.
func killGcloudGroup(pid int) {
	if process, err := os.FindProcess(pid); err == nil {
		_ = process.Kill()
	}
}
