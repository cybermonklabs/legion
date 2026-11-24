//go:build unix && !linux

// packages/envoy/cmd/cipher/secret_gcp_unix_other.go
//
// How gcloud's process is started where there are process groups and no parent-death signal.
package main

import "syscall"

// gcloudProcAttr starts gcloud in a process group of its own (killGroupOnCancel). These platforms
// have no parent-death signal, so a gcloud still running when this command ends by a SIGKILL runs
// on until it ends by itself.
func gcloudProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
