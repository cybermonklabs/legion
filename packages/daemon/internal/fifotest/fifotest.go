// Package fifotest tells a test whether code it runs waits in a plain open of a fifo: the open
// without O_NONBLOCK that a fifo's reader makes and that waits for a writer for as long as none
// comes. A row whose defect is such an open reads this from the kernel instead of timing the code
// under test, so a correct tree that is only slow is never failed for it.
//
// The kernel's record is what each thread of the process is asleep in
// (/proc/self/task/<tid>/syscall). Linux keeps it; gVisor, which every Legion pod runs, and
// darwin keep none, and there a Watch answers ErrUnsupported. A row asks Require first.
package fifotest

import (
	"errors"
	"os"
	"testing"
)

// ErrUnsupported is the answer where the kernel keeps no record of what each thread of the
// process is asleep in, so no Watch can tell a plain open of a fifo from a correct one.
var ErrUnsupported = errors.New("fifotest: the kernel keeps no record of what each thread of this process is asleep in (/proc/self/task/<tid>/syscall), as gVisor and darwin keep none")

// Watch watches for a plain open of the fifo at Path. Its zero value with Path set is ready.
type Watch struct {
	Path string
	// seen says the last call found a thread of this process asleep in a plain open.
	seen bool
}

// Require stops the test where no Watch can answer, under the daemon's rule for a test this
// machine cannot run (internal/launcher's skipOutsideCI): under GITHUB_ACTIONS it fails, since
// CI's runners keep the record and a runner that keeps none is broken, never a green job with the
// proof gone from it; anywhere else it skips, naming why. A kernel whose record cannot be read for
// any other reason fails the test: then the row has no answer, not a clean one.
func Require(t testing.TB) {
	t.Helper()
	err := supported()
	switch {
	case err == nil:
		return
	case errors.Is(err, ErrUnsupported):
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("%v: CI's runners keep it, so this runner is broken", err)
		}
		t.Skipf("%v: this row cannot tell a plain open of its fifo from a correct one here", err)
	default:
		t.Fatalf("cannot tell whether the kernel keeps a record of what each thread is asleep in: %v", err)
	}
}
