//go:build unix && !linux

// packages/envoy/cmd/cipher/secret_gcp_unix_other.go
//
// How gcloud's process is started where there are process groups and no parent-death signal.
package main

import (
	"os/exec"
	"syscall"
)

// gcloudProcAttr starts gcloud in a process group of its own (killGroupOnCancel); supervised is
// always false here (superviseGcloud). These platforms have no parent-death signal, so a gcloud
// still running when this command ends by a SIGKILL runs on until it ends by itself.
func gcloudProcAttr(supervised bool) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// superviseGcloud leaves cmd as it is, and reports false: a supervisor (secret_gcp_linux.go) needs
// the parent-death signal these platforms do not have.
func superviseGcloud(*exec.Cmd) bool { return false }
