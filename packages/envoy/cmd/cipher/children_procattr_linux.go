//go:build linux

// packages/envoy/cmd/cipher/children_procattr_linux.go
//
// How a child the secret forms start ends with this command on Linux.
package main

import "syscall"

// childProcAttr starts a child of runChild (gcloud, today) in a process group of its own
// (killGroupOnCancel) with SIGKILL as its parent-death signal: where this command ends by a
// SIGKILL, which runs none of its own code, the kernel kills the child's process with it, instead
// of leaving it running in its group until it ends by itself, its call no longer waited for by
// anyone. The signal follows the thread that started the child, not this process, so runChild
// keeps that thread from Start until the child has been waited for. It reaches the child's process
// alone, which gcloud's own script replaces with its Python (exec); a process a child forks and
// leaves behind is not reached.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
