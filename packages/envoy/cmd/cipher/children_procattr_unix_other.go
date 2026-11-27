//go:build unix && !linux

// packages/envoy/cmd/cipher/children_procattr_unix_other.go
//
// How a child the secret forms start is started where there are process groups and no
// parent-death signal.
package main

import "syscall"

// childProcAttr starts a child of runChild (gcloud, today) in a process group of its own
// (killGroupOnCancel). These platforms have no parent-death signal, so a child still running when
// this command ends by a SIGKILL runs on until it ends by itself.
func childProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
