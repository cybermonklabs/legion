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
// fork of this process out until it lets go. Write and Append hold it for reading from the open to
// the close, so a fork happens either before the file is opened or after it is closed, and no child
// ever holds its write descriptor. internal/leakguard's private copy of the shared check
// (writeCopyFile) holds the lock the same way.
package execfile

import (
	"os"
	"syscall"
)

// Write writes body to path with mode, as os.WriteFile does, with no fork of this process between
// the file's open and its close.
func Write(path string, body []byte, mode os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, body, mode)
}

// Append adds body to the end of the existing file at path, with no fork of this process between
// the file's open and its close.
func Append(path string, body []byte) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.Write(body)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
