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
// package never sees that give-up. So each bound watches its own end (watchBound): a little before
// it, if the test still runs, the goroutine stacks are read, the goroutine that took the bound
// first. When the test ends failed they are printed after its failure, with when they were read,
// which bound they belong to, and when the test ended, so a reader can tell stacks of the failing
// wait from those of a wait that ended earlier in a test that failed later. A test that passes
// prints nothing, a test whose own give-up printed stacks (gaveUp) prints no second dump, and a
// test prints one dump at most: the latest reading.
//
// A bound is one of two kinds. One that ends at the test binary's deadline less a spare
// (Remaining's, Left's, Context's, and Bounded's when that clamp is the sooner) is watched at that
// moment, once per moment. A span of the caller's own (Bounded's bound when it is the sooner) is
// watched at its end, one span per test: a span taken later replaces one not yet ended, so a loop
// that takes a fresh span each time round is read only once it stops taking them, and a test
// whose waits each end in time pays for one reading at most, at the end of its last span.

// readLead is how long before a bound that ends at the deadline its stacks are read, and the most
// a span's are: long enough that the reading still finds the waiting goroutine in its wait on a
// machine that runs the watch's timer late; short enough that a wait still waiting then is all but
// certain to give up. A span's lead is a quarter of the span when that is less. A test whose fact
// comes inside the lead passes, and its reading is dropped.
const readLead = time.Second

// watches is the watch of each running test that took a bound, by the test's name.
var watches sync.Map

// watch is one test's: a timer for each deadline moment its bounds end at and one for its latest
// span, the bounds it watches, the latest reading and when it was read, whether a give-up of the
// test printed stacks itself, and whether the test ended.
type watch struct {
	mu      sync.Mutex
	timers  map[time.Time]*time.Timer
	span    *time.Timer
	ends    []bound
	stacks  string
	readAt  time.Time
	printed bool
	ended   bool
}

// bound is one bound a wait of the test took: the function that gave it, the line outside this
// package that called it, when the wait took it and when it ends, whether it is a span of the
// caller's own, and whether it had ended when the wait took it.
type bound struct {
	fn         string
	site       string
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
	value, loaded := watches.LoadOrStore(t.Name(), &watch{timers: map[time.Time]*time.Timer{}})
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
	b := bound{fn: fn, site: callSite(), took: took, at: at, span: span, spentTaken: !took.Before(at)}
	lead := readLead
	if span {
		lead = min(readLead, at.Sub(took)/4)
	}
	name, waiter := t.Name(), goroutineID()
	timer := time.AfterFunc(time.Until(at.Add(-lead)), func() {
		w.read(name, waiter, b, lead.String()+" before the end of "+b.String())
	})
	if span {
		if w.span != nil {
			w.span.Stop()
		}
		w.span = timer
		w.ends = slices.DeleteFunc(w.ends, func(e bound) bool { return e.span })
	} else {
		w.timers[at] = timer
	}
	w.ends = append(w.ends, b)
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
	w.stacks = dumpStacks(name, "read at "+clockOf(w.readAt)+", "+when, dump)
}

// end stops t's watch as t ends and, when t failed, prints the latest reading, then a line with
// when the test ended. When a bound has ended and nothing was read, its timer late beyond its
// lead, the stacks are read now, from the test's own goroutine, as of the latest bound that ended.
func (w *watch) end(t testing.TB) {
	failed := t.Failed()
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	if w.span != nil {
		w.span.Stop()
	}
	var late *bound
	if w.stacks == "" && !w.printed {
		for i := range w.ends {
			if !time.Now().Before(w.ends[i].at) && (late == nil || w.ends[i].at.After(late.at)) {
				late = &w.ends[i]
			}
		}
	}
	w.mu.Unlock()
	if failed && late != nil {
		w.read(t.Name(), goroutineID(), *late, "as the test ended, after the end of "+late.String())
	}
	w.mu.Lock()
	w.ended = true
	stacks, readAt, printed := w.stacks, w.readAt, w.printed
	w.mu.Unlock()
	watches.CompareAndDelete(t.Name(), w)
	if failed && stacks != "" && !printed {
		stacksPrinted.Store(true)
		now := time.Now()
		printStacks(t, stacks+fmt.Sprintf("testwait: %s: the test ended failed at %s, %s after those stacks were read\n",
			t.Name(), clockOf(now), now.Sub(readAt).Round(time.Millisecond)))
	}
}

// markPrinted records that a give-up of the test named name printed stacks itself, so its watch
// prints no second dump.
func markPrinted(name string) {
	if value, ok := watches.Load(name); ok {
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

// tracesFrom is the trace of every goroutine with waiter's first and the caller's left out, and
// whether waiter still runs; when it does not, the caller's own trace is first.
func tracesFrom(waiter int) ([]string, bool) {
	all := traces()
	header := "goroutine " + strconv.Itoa(waiter) + " "
	for i, trace := range all[1:] {
		if strings.HasPrefix(trace, header) {
			rest := slices.Delete(slices.Clone(all[1:]), i, i+1)
			return append([]string{trace}, rest...), true
		}
	}
	return all, false
}
