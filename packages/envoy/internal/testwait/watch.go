// packages/envoy/internal/testwait/watch.go and packages/daemon/internal/testwait/watch.go are the
// same file, as stacks.go is.

package testwait

import (
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A wait with a failure text of its own that takes its bound from Bounded, Remaining, Left or
// Context gives up when that bound ends, at the test binary's deadline less a spare, and its
// caller fails the test with its own text: this package never sees that give-up. So the bound
// watches its own end (watchBound): readLead before that moment, if the test still runs, the
// goroutine stacks are read, the goroutine that took the bound first, and when the test fails
// they are printed after its failure, as its test ends. A test that passes prints nothing, however
// late it ran, and a test whose own give-up of this package printed stacks (gaveUp) prints no
// second dump. A bound that ends before the deadline less its spare, a span of the caller's own
// (Bounded's bound when it is the shorter), is no deadline and is not watched.

// readLead is how long before a watched bound ends its stacks are read: long enough that the
// reading still finds the waiting goroutine in its wait, which ends at that moment, on a machine
// that runs the watch's timer late; short enough that a wait that is still waiting then is all
// but certain to give up. A test whose fact comes in that second passes, and its reading is
// dropped.
const readLead = time.Second

// watches is the watch of each running test that took a watched bound, by the test's name.
var watches sync.Map

// watch is one test's: a timer for each moment its bounds end at, the stacks read at the first of
// those to come, whether a give-up of the test printed stacks itself, and whether the test ended.
type watch struct {
	mu      sync.Mutex
	timers  map[time.Time]*time.Timer
	ends    []bound
	stacks  string
	printed bool
	ended   bool
}

// watchBound watches the bound a wait of t took from this package, which ends at at (see the
// comment above). A bound that ended before the wait took it, as every bound does after the
// margin, is the side effect of what used the deadline up: its stacks are read only when no
// give-up of the test binary has printed any (stacksPrinted).
func watchBound(t testing.TB, at time.Time) {
	value, loaded := watches.LoadOrStore(t.Name(), &watch{timers: map[time.Time]*time.Timer{}})
	w := value.(*watch)
	if !loaded {
		t.Cleanup(func() { w.end(t) })
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, armed := w.timers[at]; armed || w.ended {
		return
	}
	name, waiter, spent := t.Name(), goroutineID(), !time.Now().Before(at)
	w.ends = append(w.ends, bound{at: at, spent: spent})
	w.timers[at] = time.AfterFunc(time.Until(at.Add(-readLead)), func() {
		w.read(name, waiter, spent, readLead.String()+" before the bound the test's wait took from testwait ended")
	})
}

// bound is a moment a watched bound ends at, and whether it had ended when the wait took it.
type bound struct {
	at    time.Time
	spent bool
}

// read reads the goroutine stacks, waiter's first, unless the watch has a reading already, its
// test printed stacks itself, the test ended, or the bound was spent and a give-up of the binary
// has printed stacks.
func (w *watch) read(name string, waiter int, spent bool, when string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stacks != "" || w.printed || w.ended || (spent && stacksPrinted.Load()) {
		return
	}
	dump, found := tracesFrom(waiter)
	if !found {
		when += " (the goroutine that took the bound had returned, so the first stack is the watch's own)"
	}
	w.stacks = dumpStacks(name, when, dump)
}

// end stops t's watch as t ends and, when t failed, prints the stacks it read. When a bound has
// ended with no reading, its timer late beyond readLead, the stacks are read now, from the test's
// own goroutine; if every bound that ended was spent, only when no give-up has printed any.
func (w *watch) end(t testing.TB) {
	failed := t.Failed()
	w.mu.Lock()
	for _, timer := range w.timers {
		timer.Stop()
	}
	ended, spent := false, true
	for _, b := range w.ends {
		if !time.Now().Before(b.at) {
			ended, spent = true, spent && b.spent
		}
	}
	late := ended && !w.printed && w.stacks == ""
	w.mu.Unlock()
	if failed && late {
		w.read(t.Name(), goroutineID(), spent, "as the test ended, after the bound its wait took from testwait ended")
	}
	w.mu.Lock()
	w.ended = true
	stacks, printed := w.stacks, w.printed
	w.mu.Unlock()
	watches.CompareAndDelete(t.Name(), w)
	if failed && stacks != "" && !printed {
		stacksPrinted.Store(true)
		printStacks(t, stacks)
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
