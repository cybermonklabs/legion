// Package execfile writes a file that a process is about to exec, so that no process this one
// forks meanwhile can make that exec fail with "text file busy".
//
// The kernel refuses to exec a file that some process holds open for writing (ETXTBSY). Go opens
// every file close-on-exec, but a child forked while the file is open still holds a copy of the
// write descriptor from the fork until its own exec closes it. So when one goroutine writes a script
// and execs it while another goroutine of the same process starts any child, the exec of the script
// can fail: Go issue 22315. Tests that run beside each other (t.Parallel) and each write a stand-in
// script and run it meet this as soon as they overlap.
//
// syscall.ForkLock is the lock Go's own fork takes for writing; a holder for reading keeps every
// fork of this process out until it lets go. Write and Append are the envoy module's
// programfile.Write and programfile.Append, the one place that holds it for reading from the open
// to the close, so a fork happens either before the file is opened or after it is closed, and no
// child ever holds its write descriptor. The code a deployment runs writes such a file with
// programfile.Write itself (internal/runtime/workerbin's scripts, internal/leakguard's private
// copy of the shared check).
package execfile

import (
	"bytes"
	"os"

	"github.com/sjawhar/envoy/programfile"
)

// Write writes body to path, over a file already there, with no fork of this process between the
// file's open and its close. The file gets mode exactly (programfile.Write): where os.WriteFile
// takes the umask off a new file's mode and keeps an existing file's, this sets mode on both.
func Write(path string, body []byte, mode os.FileMode) error {
	return programfile.Write(path, os.O_TRUNC, mode, bytes.NewReader(body))
}

// Append adds body to the end of the existing file at path, with no fork of this process between
// the file's open and its close (programfile.Append).
func Append(path string, body []byte) error {
	return programfile.Append(path, bytes.NewReader(body))
}
