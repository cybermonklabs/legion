//go:build unix && !((linux || darwin) && (amd64 || arm64))

package main

import (
	"os"
	"syscall"
)

// raiseSignal ends the process by sig, and reports that it sent it, on a Unix target without the
// value prompt: Go's own handling, back after exitBySignal's signal.Reset, ends a process by a
// hang-up, an interrupt or a termination. A quit is left to exitBySignal's exit status, since Go
// answers it with a dump of every goroutine, and this target has no call to set the kernel's
// action in its place.
func raiseSignal(sig os.Signal) bool {
	s, ok := sig.(syscall.Signal)
	if !ok || s == syscall.SIGQUIT {
		return false
	}
	_ = syscall.Kill(os.Getpid(), s)
	return true
}
