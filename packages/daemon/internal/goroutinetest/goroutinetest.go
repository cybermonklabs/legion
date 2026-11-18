// Package goroutinetest reads the goroutine dump, for a test that waits on what another goroutine
// is doing at a point the code under test neither logs nor signals: a request parked on a lock,
// say, which a sync.Mutex records nowhere else. packages/envoy/internal/goroutinetest holds the
// same reader, because Go's internal rule keeps one module from importing the other's.
//
// A test that reads the dump finds its goroutine by the name of a function on its stack, so a
// rename of that function must be made in the test too: a name that no longer matches never lets
// the wait pass, and the row fails at the shared wait's bound, naming what it waited for. The
// dump holds every goroutine of the test binary, so a test reads it only where no other test of
// the package runs beside it (no t.Parallel).
package goroutinetest

import (
	"runtime"
	"strings"
)

// Traces returns the trace of every goroutine in the process, read from one dump of them all. Each
// trace opens with its header line, "goroutine <id> [<state>]:".
func Traces() []string {
	dump := make([]byte, 1<<20)
	for {
		n := runtime.Stack(dump, true)
		if n < len(dump) {
			return strings.Split(string(dump[:n]), "\n\n")
		}
		dump = make([]byte, 2*len(dump))
	}
}
