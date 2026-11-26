// packages/envoy/internal/testwait/watch.go and packages/daemon/internal/testwait/watch.go are the
// same file, as stacks.go is.

package testwait

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A wait with a failure text of its own that takes its bound from Bounded, Remaining, Left or
// Context gives up when that bound ends, and its caller fails the test with its own text: this
// package never sees that give-up. So each bound watches its own end (watchBound) and reads the
// goroutine stacks there, the goroutine that took the bound first. When the test ends failed they
// are printed after its failure, under a line that says when they were read and which bound they
// belong to, and then a line with when the test ended, so a reader can tell stacks of the failing
// wait from those of a wait that ended earlier in a test that failed later. A test whose own
// give-up printed stacks (gaveUp) prints no second dump, and a test prints one dump at most: the
// latest reading.
//
// A bound is one of two kinds, and each is read at its own moment:
//   - One that ends at the test binary's deadline less a spare (Remaining's, Left's, Context's, and
//     Bounded's when that clamp is the sooner) is read readLead before that moment, if the test
//     still runs then: what its wait waits on is still there, before the give-up and the cleanups
//     that follow it change it. A test reaches that moment only when it is still running a second
//     before the deadline less its spare, where almost every wait still waiting gives up.
//   - A span of the caller's own (Bounded's bound when it is the sooner) is read only once the test
//     has failed: at the span's end, or within readLead after it, as soon as the test reads as
//     failed. A wait that gives up at its span fails its test within that second, from inside the
//     wait, so the waiting goroutine's stack still runs through the wait (t.Fatal ends the
//     goroutine by running its deferred calls with the wait's frames still below them). A passing
//     test, and a loop that takes a fresh span each time round, pay for no reading at all.

// readLead is how long before a deadline bound ends its stacks are read, and how long after a
// span ends the watch waits for its test to read as failed: long enough for a machine that runs
// the watch's timer late, short enough that a wait still waiting a second before the deadline is
// all but certain to give up, and a failure a second after a span's end is that span's give-up.
const readLead = time.Second

// failedPoll is how often the watch of a span that ended asks whether its test has failed.
const failedPoll = time.Millisecond

// watches is the watch of each running test that took a bound, by the test (its *testing.T, or a
// test type wrapping one), so that a goroutine of an ended run of a test that takes a bound after
// that run ended never shares the watch of a later run under the same name.
var watches sync.Map

// watch is one test's: a timer for each deadline moment its bounds end at and one for its latest
// span, the bounds it watches, the latest reading and when it was read, how many readings it took
// and the end of the last span it finished asking about (which this package's own tests read),
// whether a give-up of the test printed stacks itself, and whether the test ended.
type watch struct {
	mu      sync.Mutex
	timers  map[time.Time]*time.Timer
	span    *time.Timer
	ends    []bound
	stacks  string
	readAt  time.Time
	reads   int
	checked time.Time
	printed bool
	ended   bool
}

// bound is one bound a wait of the test took: the function that gave it, the line outside this
// package that called it, the goroutine that took it, when it took it and when it ends, whether it
// is a span of the caller's own, and whether it had ended when the wait took it.
type bound struct {
	fn         string
	site       string
	waiter     int
	took, at   time.Time
	span       bool
	spentTaken bool
}

// String names the bound for a reader of the stacks.
func (b bound) String() string {
	return fmt.Sprintf("the bound of %s testwait.%s gave at %s at %s, which ends at %s",
		b.at.Sub(b.took).Round(time.Millisecond), b.fn, b.site, clockOf(b.took), clockOf(b.at))
}

// clockOf is t as a reader of a log compares it with the log's own times: UTC, to the millisecond.
func clockOf(t time.Time) string {
	return t.UTC().Format("15:04:05.000Z")
}

// watchBound watches the bound function fn gave a wait of t, which ends at at; span says it is a
// span of the caller's own rather than the deadline less a spare (see the comment above). A
// deadline bound that ended before the wait took it, as every one does after the margin, is the
// side effect of what used the deadline up: its stacks are read only when no give-up of the test
// binary has printed any (stacksPrinted).
func watchBound(t testing.TB, fn string, at time.Time, span bool) {
	took := time.Now()
	value, loaded := watches.LoadOrStore(t, &watch{timers: map[time.Time]*time.Timer{}})
	w := value.(*watch)
	if !loaded {
		t.Cleanup(func() { w.end(t) })
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ended {
		return
	}
	if _, armed := w.timers[at]; armed && !span {
		return
	}
	b := bound{fn: fn, site: callSite(), waiter: goroutineID(), took: took, at: at, span: span, spentTaken: !took.Before(at)}
	if span {
		if w.span != nil {
			w.span.Stop()
		}
		w.span = time.AfterFunc(time.Until(at), func() { w.readOnceFailed(t, b) })
		w.ends = slices.DeleteFunc(w.ends, func(e bound) bool { return e.span })
	} else {
		w.timers[at] = time.AfterFunc(time.Until(at.Add(-readLead)), func() {
			w.read(t.Name(), b.waiter, b, readLead.String()+" before the end of "+b.String())
		})
	}
	w.ends = append(w.ends, b)
}

// readOnceFailed reads the stacks for b, a span that has just ended, as soon as t reads as failed,
// asking every failedPoll for readLead; it reads nothing when t has not failed by then, or ends
// (its end reads them then, when it failed).
func (w *watch) readOnceFailed(t testing.TB, b bound) {
	defer func() {
		w.mu.Lock()
		w.checked = b.at
		w.mu.Unlock()
	}()
	for until := time.Now().Add(readLead); time.Now().Before(until); time.Sleep(failedPoll) {
		w.mu.Lock()
		ended := w.ended
		w.mu.Unlock()
		if ended {
			return
		}
		if t.Failed() {
			w.read(t.Name(), b.waiter, b, "as the test read as failed, "+time.Since(b.at).Round(time.Millisecond).String()+" after the end of "+b.String())
			return
		}
	}
}

// read reads the goroutine stacks, waiter's first, in place of any earlier reading, unless its
// test printed stacks itself, the test ended, or b is a deadline bound that was spent and a
// give-up of the binary has printed stacks. when says which moment and bound the reading is of.
func (w *watch) read(name string, waiter int, b bound, when string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.printed || w.ended || (b.spentTaken && stacksPrinted.Load()) {
		return
	}
	dump, found := tracesFrom(waiter)
	if !found {
		when += " (the goroutine that took the bound had returned, so the first stack is the watch's own)"
	}
	w.readAt = time.Now()
	w.reads++
	w.stacks = dumpStacks(name, "read at "+clockOf(w.readAt)+", "+when, dump)
}

// end stops t's watch as t ends and, when t failed, prints the latest reading, then a line with
// when the test ended. When a bound has ended and nothing was read, the stacks are read now, the
// goroutine that took that bound first, as of the latest such bound: a deadline bound whose timer
// ran late beyond its lead, or a span that ended no more than readLead ago, whose test failed
// and ended before the watch asked (t.Fatal runs the cleanups at once, so this is the usual way a
// span's give-up is read; the waiting goroutine, inside its t.Fatal, still runs through the wait).
// A span that ended longer ago is not read here: its test did not fail within readLead of its
// end, so the failure is not that span's give-up.
func (w *watch) end(t testing.TB) {
	failed := t.Failed()
	now := time.Now()
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	if w.span != nil {
		w.span.Stop()
	}
	var late bound
	found := false
	if w.stacks == "" && !w.printed {
		for _, b := range w.ends {
			ended := !now.Before(b.at) && (!b.span || now.Sub(b.at) <= readLead)
			if ended && (!found || b.at.After(late.at)) {
				late, found = b, true
			}
		}
	}
	w.mu.Unlock()
	if failed && found {
		w.read(t.Name(), late.waiter, late, "as the test ended, "+now.Sub(late.at).Round(time.Millisecond).String()+" after the end of "+late.String())
	}
	w.mu.Lock()
	w.ended = true
	stacks, readAt, printed := w.stacks, w.readAt, w.printed
	w.mu.Unlock()
	watches.CompareAndDelete(t, w)
	if failed && stacks != "" && !printed {
		stacksPrinted.Store(true)
		now := time.Now()
		printStacks(t, stacks+fmt.Sprintf("testwait: %s: the test ended failed at %s, %s after those stacks were read\n",
			t.Name(), clockOf(now), now.Sub(readAt).Round(time.Millisecond)))
	}
}

// markPrinted records that a give-up of t printed stacks itself, so its watch prints no second
// dump.
func markPrinted(t testing.TB) {
	if value, ok := watches.Load(t); ok {
		w := value.(*watch)
		w.mu.Lock()
		w.printed = true
		w.mu.Unlock()
	}
}

// thisPackage is the prefix of this package's functions' names, read from one of them.
var thisPackage = strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(clockOf).Pointer()).Name(), "clockOf")

// callSite is the file and line of the call that took a bound: the first frame of the caller's
// stack outside this package's own files (its tests count as callers).
func callSite() string {
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if !strings.HasPrefix(frame.Function, thisPackage) || strings.HasSuffix(frame.File, "_test.go") {
			return filepath.Base(frame.File) + ":" + strconv.Itoa(frame.Line)
		}
		if !more {
			return "an unknown line"
		}
	}
}

// goroutineID is the calling goroutine's id, read from its trace's header.
func goroutineID() int {
	header := make([]byte, 64)
	header = header[:runtime.Stack(header, false)]
	id, _ := strconv.Atoi(strings.Fields(string(header))[1])
	return id
}

// tracesFrom is the trace of every goroutine with waiter's first, and whether waiter still runs.
// The caller's own trace is left out unless the caller is waiter; when waiter does not run, the
// caller's own trace is first.
func tracesFrom(waiter int) ([]string, bool) {
	all := traces()
	header := "goroutine " + strconv.Itoa(waiter) + " "
	if strings.HasPrefix(all[0], header) {
		return all, true
	}
	for i, trace := range all[1:] {
		if strings.HasPrefix(trace, header) {
			rest := slices.Delete(slices.Clone(all[1:]), i, i+1)
			return append([]string{trace}, rest...), true
		}
	}
	return all, false
}
