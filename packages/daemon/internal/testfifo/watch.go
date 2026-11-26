package testfifo

// This file is the other half of the package: Plant's held reader shows an open of a fifo for
// writing, and a Watch shows code under test that waits in a plain open of a fifo for reading,
// the open without O_NONBLOCK that waits for a writer for as long as none comes, which no held
// reader can show. A row whose defect is such an open reads it from the kernel instead of timing
// the code under test, so a correct tree that is only slow is never failed for it.
//
// The kernel's record is what each thread of the process is asleep in
// (/proc/self/task/<tid>/syscall). Linux keeps it; gVisor, which every Legion pod runs, and
// darwin keep none, and there a Watch answers ErrUnsupported. A row asks RequireWatch first.

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// ErrUnsupported is the answer where the kernel keeps no record of what each thread of the
// process is asleep in, so no Watch can tell a plain open of a fifo from a correct one.
var ErrUnsupported = errors.New("testfifo: the kernel keeps no record of what each thread of this process is asleep in (/proc/self/task/<tid>/syscall), as gVisor and darwin keep none")

// Watch watches for a plain open of the fifo at Path. Its zero value with Path set is ready.
type Watch struct {
	Path string
	// seen says the last call found a thread of this process asleep in a plain open.
	seen bool
}

// WatchBeside watches the fifos at paths beside the row's own wait for the code under test, once
// start is closed (nil: at once), asking each Watch every testwait.Poll until the returned stop is
// called. The row waits for the code's answer with testwait.Receive, which sets no span of its
// own, and calls stop when it has the answer: stop answers the path of a fifo the code opened
// plainly, or an error from the kernel's record, or neither. A Watch that finds a plain open has
// been the writer that open needed, so the code under test answers, and the row's wait ends with
// it. stop may be called more than once, and from a deferred call of a row that has failed.
func WatchBeside(start <-chan struct{}, paths ...string) (stop func() (string, error)) {
	type answer struct {
		opened string
		err    error
	}
	quit := make(chan struct{})
	answered := make(chan answer, 1)
	go func() {
		watches := make([]*Watch, len(paths))
		for i, path := range paths {
			watches[i] = &Watch{Path: path}
		}
		poll := time.NewTicker(testwait.Poll)
		defer poll.Stop()
		for started := start == nil; ; {
			select {
			case <-quit:
				answered <- answer{}
				return
			case <-poll.C:
			}
			if !started {
				select {
				case <-start:
					started = true
				default:
					continue
				}
			}
			for _, watch := range watches {
				opened, err := watch.Opened()
				if err != nil || opened {
					seen := answer{err: err}
					if opened {
						seen.opened = watch.Path
					}
					answered <- seen
					return
				}
			}
		}
	}()
	var once sync.Once
	var seen answer
	return func() (string, error) {
		once.Do(func() {
			close(quit)
			seen = <-answered
		})
		return seen.opened, seen.err
	}
}

// RequireWatch stops the test where no Watch can answer, under the daemon's rule for a test this
// machine cannot run (internal/launcher's skipOutsideCI): under GITHUB_ACTIONS it fails, since
// CI's runners keep the record and a runner that keeps none is broken, never a green job with the
// proof gone from it; anywhere else it skips, naming why. A kernel whose record cannot be read for
// any other reason fails the test: then the row has no answer, not a clean one.
func RequireWatch(t testing.TB) {
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
