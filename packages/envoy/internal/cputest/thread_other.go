//go:build !linux

package cputest

import "time"

// Thread is Process on a system whose getrusage counts no thread on its own: there a call that
// OfCall times counts each goroutine of the process.
func Thread() time.Duration {
	return Process()
}
