//go:build !unix

package main

import "os"

// raiseSignal sends nothing on a target that is no Unix, such as Windows, where a process cannot
// end itself by a signal: exitBySignal exits with the status a shell gives for sig instead.
func raiseSignal(os.Signal) bool { return false }
