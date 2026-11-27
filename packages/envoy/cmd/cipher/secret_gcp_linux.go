//go:build linux

// packages/envoy/cmd/cipher/secret_gcp_linux.go
//
// How gcloud's process ends with this command on Linux.
package main

import "syscall"

// gcloudProcAttr starts gcloud in a process group of its own (killGroupOnCancel) with SIGKILL as
// its parent-death signal: where this command ends by a SIGKILL, which runs none of its own code,
// the kernel kills gcloud's process with it, instead of leaving it running in its group until it
// ends by itself, its call no longer waited for by anyone. The signal follows the thread that
// started gcloud, not this process, so runRecorded keeps that thread from Start until gcloud has
// been waited for. It reaches gcloud's process alone, which gcloud's own script replaces with its
// Python (exec); a child a gcloud forks and leaves behind is not reached.
func gcloudProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
