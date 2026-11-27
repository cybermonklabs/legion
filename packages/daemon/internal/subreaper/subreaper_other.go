//go:build !linux

package subreaper

import (
	"errors"
	"runtime"
)

// Serve needs Linux's PR_SET_CHILD_SUBREAPER: no other system the daemon builds for has it, and
// nothing runs a shim that reaps orphans anywhere else (the controller pod is Linux).
func (g *Guard) Serve(func(format string, args ...any)) (func(), error) {
	return nil, errors.New("reaping orphans needs Linux's PR_SET_CHILD_SUBREAPER, and this is " + runtime.GOOS)
}
