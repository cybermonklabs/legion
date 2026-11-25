// Package programbound is where a wait of legion's on something outside it, a program it starts or
// a service it calls, takes its bound. Each bound is a variable its package registers beside its
// declaration with the value the product means (Register), and each wait asks for it when it
// starts (Of).
//
// In a legion binary that is all: Of answers the variable. A test binary of this module that runs
// these waits on stand-ins gives them its own deadline instead (SetDeadline), because each product
// bound is sized for the real program, and on a loaded machine a stand-in, or a git behind a
// script, can take longer to start and answer than the real program's bound allows: at a box load
// of about 1,100 a git behind a one-line script ran past `legion gh`'s 30 s for its configuration
// read, and the row read the kill as a refusal. A program that never answers still fails its row by
// name, at the deadline less a margin. A binary such a test builds and runs carries the deadline in
// at link time (linkedDeadline), which no environment or flag of a released binary can set.
package programbound

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// margin is how long before the test binary's deadline a bound given that deadline runs out: the
// time a row whose program never answered has to fail by name, and its cleanups to run, before go
// test's alarm panics the binary. It is testwait.Receive's.
const margin = 30 * time.Second

// linkedDeadline is the deadline of the test binary that built this binary, in RFC 3339 with
// nanoseconds, linked in by cmd/legion's tests alone (`-ldflags "-X
// github.com/sjawhar/legion/daemon/internal/programbound.linkedDeadline=<t>"`, its legionBuild);
// empty in every other build, release and image builds included.
var linkedDeadline string

var (
	mu sync.RWMutex
	// registered is each registered bound and the value it held when it was registered.
	registered = map[*time.Duration]time.Duration{}
	// deadline is the test binary's deadline, zero in a legion binary.
	deadline time.Time
)

// init reads linkedDeadline. A value that is set and does not parse is a broken link of the
// tests' build, which would quietly give the built binary the product bounds back, so it panics.
func init() {
	if linkedDeadline == "" {
		return
	}
	linked, err := time.Parse(time.RFC3339Nano, linkedDeadline)
	if err != nil {
		panic(fmt.Sprintf("programbound: linkedDeadline %q is no RFC 3339 time: %v", linkedDeadline, err))
	}
	deadline = linked
}

// Register records each of bounds, with the value it holds now as the product's own. A package
// registers its bounds in an init, before any test can set one.
func Register(bounds ...*time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	for _, bound := range bounds {
		registered[bound] = *bound
	}
}

// Of is the bound a wait that starts now takes: *bound, unless a test binary's deadline is set
// and *bound is registered and still holds its registered value, when it is what is left of that
// deadline less margin. A bound a test has set to a value of its own, to read its stop, keeps that
// value.
func Of(bound *time.Duration) time.Duration {
	mu.RLock()
	defer mu.RUnlock()
	if product, ok := registered[bound]; !ok || deadline.IsZero() || *bound != product {
		return *bound
	}
	return time.Until(deadline) - margin
}

// SetDeadline gives every registered bound no test has set the deadline end (Of). Only a test
// binary calls it, from its TestMain, before any test runs; anywhere else it panics, so no
// production caller can stretch every bound (testing.Testing).
func SetDeadline(end time.Time) {
	if !testing.Testing() {
		panic("programbound: SetDeadline outside a test binary")
	}
	mu.Lock()
	defer mu.Unlock()
	deadline = end
}
