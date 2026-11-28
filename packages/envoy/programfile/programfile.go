// Package programfile writes a file that a process then runs: a script, a helper git runs, a
// program a copy carries. Linux refuses to run a file that some process holds open for writing
// (ETXTBSY, "text file busy"), and a Go process that starts a child forks first, so the child holds
// a copy of each descriptor the process has open until its own exec closes it (golang/go#22315). A
// file one goroutine wrote and closed can then still be open for writing in a child another
// goroutine just started, and a run of it in that span fails. Write and Append keep every fork
// os/exec makes out of the span the file is open for writing, so no child the process starts holds
// it when it runs; Write's doc names the one clone of a Go process they do not hold back. Every
// place that writes such a file writes it with Write, or adds to one with Append: the code a
// deployment runs directly, and a test through testexec.WriteFile or the daemon's
// internal/execfile. testexec.RunsBeside runs a test of such a place beside forks, and
// testexec.Unguarded names a write by package os that should be a Write.
package programfile

import (
	"io"
	"os"
	"syscall"
)

// Write opens name with os.O_WRONLY|os.O_CREATE|flag and perm, gives it perm exactly, since the
// umask may have taken bits off the create's, copies content into it and closes it. flag says what
// is done with a file already at name: os.O_EXCL refuses it, a link or a fifo included, which is
// what a write into a directory another process can write needs; os.O_TRUNC writes over it.
//
// It holds syscall.ForkLock for reading from the open to the close. Every fork os/exec makes, in
// syscall.ForkExec or syscall.StartProcess, which os.StartProcess and exec.Cmd run, takes that lock
// for writing around the fork, so none of them falls inside the write and no child they start
// holds the file's write descriptor when the file runs. The one clone of a Go process that does
// not take it is the os package's probe for pidfd support (os.checkPidfdOnce,
// syscall.doCheckClonePidfd): once per process, at its first start or find of a process, a
// CLONE_VFORK child that runs no program and exits at once, and holds a copy of each descriptor of
// the process until it does; a run of a file written in that instant can still be refused. Every
// fork os/exec makes waits for the write, so content is a reader of what is already in memory or
// in a local file, and its reads start no process: a fork there would wait on the lock it holds.
func Write(name string, flag int, perm os.FileMode, content io.Reader) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|flag, perm)
	if err != nil {
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// Append opens the file already at name for writing at its end, copies content to it and closes
// it, leaving its mode as it is and creating nothing: a stand-in a test rewrites in place to stand
// for a later build. It holds syscall.ForkLock for reading from the open to the close, as Write
// does and for the same reason, with the same one exception and the same rule for content.
func Append(name string, content io.Reader) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
