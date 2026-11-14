// Package kvwatchtest holds the stand-in for time.AfterFunc that a test hands a KV watcher
// (kvwatch.WithAfterFunc) to keep the watcher's long waits, its scheduled read above all, for the
// test to fire itself. internal/store's role claim repair tests and cmd/listener's grant guard
// tests both drive a real role claim watcher this way, and which waits a test keeps follows which
// waits the watcher arms, so the rule lives here once rather than in a copy per package that a
// change to those waits must find twice. It is an ordinary package rather than a _test.go file
// because Go cannot share test-only code across packages (see internal/cputest's identical
// reasoning). kvwatch's own tests cannot import it, since it imports kvwatch, and keep fakes of
// their own.
package kvwatchtest

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// Waits stands in for time.AfterFunc in a watcher (kvwatch.WithAfterFunc). It keeps every wait
// longer than a scan's idle timeout (kvwatch.ScanIdleTimeout) for the test to fire or leave
// unfired - the scheduled read (kvwatch.WithScheduledRead), and the minute a watcher stop nobody
// asked for waits before its ERROR - and hands every shorter one (the stream probe, a re-read, a
// scan's idle wait) to time.AfterFunc, so the watcher reads and follows its bucket as it does in
// production and completes no scheduled read the test did not fire. A kept wait records its
// delay, so a test reads the interval the watcher armed rather than assuming it. The zero Waits
// keeps nothing yet and is ready for use.
type Waits struct {
	mu   sync.Mutex
	kept []*Wait
}

// Wait is one wait a Waits kept: the watcher's own Timer for it, which the test fires.
type Wait struct {
	delay   time.Duration
	fire    func()
	stopped atomic.Bool
}

// Delay is how long after it was armed the watcher asked the wait to fire.
func (w *Wait) Delay() time.Duration { return w.delay }

// Fire runs the wait's callback now, on the caller's goroutine, as its timer would have once its
// delay passed. It does not stop the wait: the watcher stops it, or arms the next one, as it does
// after a timer that fired.
func (w *Wait) Fire() { w.fire() }

// Stop is kvwatch.Timer's: it reports whether the wait was still armed.
func (w *Wait) Stop() bool { return !w.stopped.Swap(true) }

// After is the AfterFunc the watcher arms its waits through (kvwatch.WithAfterFunc).
func (w *Waits) After(d time.Duration, f func()) kvwatch.Timer {
	if d <= kvwatch.ScanIdleTimeout {
		return time.AfterFunc(d, f)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	wait := &Wait{delay: d, fire: f}
	w.kept = append(w.kept, wait)
	return wait
}

// Armed is the kept wait the watcher armed last and has not stopped, and nil when there is none.
func (w *Waits) Armed() *Wait {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i := len(w.kept) - 1; i >= 0; i-- {
		if !w.kept[i].stopped.Load() {
			return w.kept[i]
		}
	}
	return nil
}
