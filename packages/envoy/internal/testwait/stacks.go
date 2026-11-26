// packages/envoy/internal/testwait/stacks.go and packages/daemon/internal/testwait/stacks.go are
// the same file, as reading.go is: each module's testwait gives up the same way.

package testwait

import (
	"fmt"
	"io"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// stackBound is the most text a give-up prints of the goroutine stacks after the waiting
// goroutine's own, which it always prints. A wait that gives up has outlived what it waited for,
// so the stacks are the one record of where the test and the code it started stood; go test's
// alarm would print them all, but only by killing the binary, and the margin gives up before it.
// One test's goroutines, the delivery hub's rig of packages/envoy/internal/dispatch/api with its
// server, hub, relay, listeners and pools, came to 14.7 KiB in a run that held only that test; the
// whole dump of that package's binary, at its alarm with one test running, was 755 goroutines and
// 548 KiB. 64 KiB holds the first four times over and cuts the second, and the stacks this bound
// cuts are the last in the order below.
const stackBound = 64 << 10

// stacksPrinted is whether this test binary has printed the goroutine stacks of the moment its
// deadline was used up: a give-up of a wait that waited until the margin, the first give-up of a
// wait begun past it, or a watched bound at the deadline (watch.go). The stacks a watch read at a
// span's end do not count: they are of an earlier moment.
var stacksPrinted atomic.Bool

// cause is the first give-up of this test binary whose wait waited until its end at the deadline,
// rather than beginning past the margin: its test, and what its wait waited for. That wait used
// the deadline up, and every later wait begun past the margin fails because of it.
var cause struct {
	sync.Mutex
	test, what string
}

// noteCause records a give-up in the test named test, of a wait for what that waited until its
// end at the deadline, as the cause, unless an earlier one is recorded. what is kept on one line,
// as the give-up line writes it (oneLine).
func noteCause(test, what string) {
	cause.Lock()
	defer cause.Unlock()
	if cause.test == "" {
		cause.test, cause.what = test, oneLine.Replace(what)
	}
}

// collateral is the line a give-up in the test named name prints when its wait began past the
// margin: that the give-up is collateral, a failure of no fault of the wait's own, and which give-up
// used the deadline up (cause), so a reader of a log with many such failures finds the one that
// is real.
func collateral(name string) string {
	cause.Lock()
	test, what := cause.test, cause.what
	cause.Unlock()
	const opening = "testwait: %s: this give-up is collateral: the wait began after the margin before the test binary's deadline had passed, so it did not wait; "
	if test == "" {
		return fmt.Sprintf(opening+"no earlier wait of testwait gave up at the deadline, so the deadline was used up by a test that waits without testwait, or the -timeout is no longer than the margin\n", name)
	}
	return fmt.Sprintf(opening+"the wait that used the deadline up gave up earlier: %s, waiting for %s, whose goroutine stacks are printed with that test's failure\n", name, test, what)
}

// stacksFor is the text a give-up in t, of a wait for what, prints after its failure. A wait that
// waited until the margin prints the goroutine stacks (goroutineStacks) and is recorded as the
// cause, when it is the first. A wait that began past the margin (spent) did not wait, so its
// failure is false: it prints the collateral line, and the stacks only when the binary has printed
// none of the moment its deadline was used up (stacksPrinted), so that moment's stacks are printed
// once. Such a later give-up gets none of its own test's stacks, which the earlier dump, of another
// test's give-up, did not print; what its test was doing is that give-up's effect.
func stacksFor(t testing.TB, what string, spent bool) string {
	if spent {
		line := collateral(t.Name())
		if !stacksPrinted.CompareAndSwap(false, true) {
			return line
		}
		markPrinted(t)
		return line + goroutineStacks(t.Name(), traces())
	}
	stacksPrinted.Store(true)
	noteCause(t.Name(), what)
	markPrinted(t)
	return goroutineStacks(t.Name(), traces())
}

// traces is the trace of every goroutine, the calling one's first, from one dump of them all
// (runtime.Stack). It reads the dump itself, so this package imports no other package of its
// module (docs/testing.md).
func traces() []string {
	dump := make([]byte, 1<<20)
	for {
		n := runtime.Stack(dump, true)
		if n < len(dump) {
			return strings.Split(strings.TrimRight(string(dump[:n]), "\n"), "\n\n")
		}
		dump = make([]byte, 2*len(dump))
	}
}

// printStacks writes stacks, stacksFor's text, to t's output, beside t's own log: go test -json
// files each line under t, and go test without -v prints them with t's failure.
func printStacks(t testing.TB, stacks string) {
	_, _ = io.WriteString(t.Output(), stacks)
}

// owner is whose goroutine a stack is, in the order a give-up prints them.
type owner int

const (
	// ownedByWait is the goroutine that waited: the first trace of the dump.
	ownedByWait owner = iota
	// ownedByTest is a goroutine the waiting goroutine's test started, or one of its subtests,
	// itself or through goroutines that still run.
	ownedByTest
	// ownedByNone is a goroutine whose line of creators reaches one that has ended before it
	// reaches a test or the binary's main goroutine: a goroutine the test started from one that
	// has returned (a relay a request started, say) or one an ended test left behind.
	ownedByNone
	// ownedByBinary is a goroutine the binary's main goroutine started, before or around its
	// tests (TestMain's).
	ownedByBinary
	// ownedByOtherTest is a goroutine another running test started. Its stacks are not printed:
	// what it does is that test's, which prints its own when one of its waits gives up.
	ownedByOtherTest
)

// goroutine is one trace of a dump of every goroutine, as runtime.Stack writes it.
type goroutine struct {
	id      int
	creator int // 0 when the trace names no creator, as the main goroutine's
	trace   string
	key     string // the state and frames, less argument values, offsets and how long it waited
	test    bool   // a test's own goroutine: testing.tRunner runs it, and testing.runTests does not
	runner  bool   // the goroutine testing.runTests runs the top-level tests from
	owner   owner
}

// The parts of a trace's text: its header ("goroutine 7 [select, 2 minutes]:", with " gp=… m=…"
// before the state under GOTRACEBACK=system or crash), the line naming its creator, and what a
// frame's lines carry that differs between goroutines of one stack: argument values, the offset,
// and under GOTRACEBACK=system or crash the frame's fp=, sp= and pc=.
var (
	traceHeader  = regexp.MustCompile(`^goroutine (\d+)(?: [^\[]*)? \[(.*)\]:$`)
	traceCreator = regexp.MustCompile(`(?m)^created by (.*) in goroutine (\d+)$`)
	waitedFor    = regexp.MustCompile(`, \d+ minutes?`)
	frameArgs    = regexp.MustCompile(`\([^()]*\)$`)
	frameOffset  = regexp.MustCompile(` \+0x[0-9a-f]+( fp=.*)?$`)
)

// readTrace is trace's goroutine, its owner not yet known.
func readTrace(trace string) *goroutine {
	g := &goroutine{trace: strings.TrimRight(trace, "\n")}
	lines := strings.Split(g.trace, "\n")
	key := make([]string, 0, len(lines))
	if header := traceHeader.FindStringSubmatch(lines[0]); header != nil {
		g.id, _ = strconv.Atoi(header[1])
		key = append(key, waitedFor.ReplaceAllString(header[2], ""))
		lines = lines[1:]
	}
	if created := traceCreator.FindStringSubmatch(g.trace); created != nil {
		g.creator, _ = strconv.Atoi(created[2])
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "testing.tRunner("):
			g.test = true
		case strings.HasPrefix(line, "testing.runTests("):
			g.runner = true
		}
		if created := traceCreator.FindStringSubmatch(line); created != nil {
			line = "created by " + created[1]
		}
		key = append(key, frameOffset.ReplaceAllString(frameArgs.ReplaceAllString(line, "()"), ""))
	}
	g.test = g.test && !g.runner
	g.key = strings.Join(key, "\n")
	return g
}

// ownTraces reads traces, a dump whose first trace is the waiting goroutine's, and gives each
// goroutine its owner. The waiting goroutine's test is the outermost test goroutine above it: its
// creators are followed while they run and are tests, so a wait in a subtest owns what its parent
// test started. A goroutine is that test's when its line of running creators reaches it.
func ownTraces(traces []string) []*goroutine {
	goroutines := make([]*goroutine, 0, len(traces))
	byID := make(map[int]*goroutine, len(traces))
	for _, trace := range traces {
		g := readTrace(trace)
		goroutines = append(goroutines, g)
		if g.id != 0 {
			byID[g.id] = g
		}
	}
	goroutines[0].owner = ownedByWait
	test := goroutines[0]
	for range len(goroutines) {
		parent, running := byID[test.creator]
		if !running || parent.runner || (test.test && !parent.test) {
			break
		}
		test = parent
	}
	for _, g := range goroutines[1:] {
		g.owner = ownerOf(g, test, byID)
	}
	return goroutines
}

// ownerOf is whose goroutine g is when test is the waiting goroutine's test: it follows g's
// creators while they run, to test, to another test, to the binary's main goroutine, or to a
// creator that has ended.
func ownerOf(g, test *goroutine, byID map[int]*goroutine) owner {
	other, at := false, g
	for range len(byID) + 1 {
		if at == test {
			return ownedByTest
		}
		other = other || at.test
		parent, running := byID[at.creator]
		if !running {
			break
		}
		at = parent
	}
	switch {
	case other:
		return ownedByOtherTest
	case at.creator == 0 && at.id != 0:
		return ownedByBinary
	default:
		return ownedByNone
	}
}

// stackGroup is the goroutines of one owner whose stacks are the same but for argument values,
// offsets and how long each has waited, in the order the dump gives them.
type stackGroup struct {
	owner      owner
	goroutines []*goroutine
}

// groupIDs is how many ids of a group's goroutines its line names.
const groupIDs = 20

// goroutineStacks is what a give-up in the test named name prints of traces, a dump of every
// goroutine whose first trace is the waiting goroutine's, read as the wait gave up (dumpStacks).
func goroutineStacks(name string, traces []string) string {
	return dumpStacks(name, "as the wait gave up", traces)
}

// dumpStacks is what the test named name prints of traces, a dump of every goroutine whose first
// trace is the waiting goroutine's, read at the moment when says: a line that counts the goroutines
// by owner; the waiting goroutine's trace; then, until stackBound, each other stack once, the
// test's first, then those of no running creator, then the binary's, each stack that more than one
// goroutine shares under a line that counts them; and a last line that says how much was printed
// and what the bound cut. Other running tests' goroutines are counted and not printed.
func dumpStacks(name, when string, traces []string) string {
	goroutines := ownTraces(traces)
	counts := map[owner]int{}
	var groups []*stackGroup
	index := map[string]*stackGroup{}
	for _, g := range goroutines[1:] {
		counts[g.owner]++
		if g.owner == ownedByOtherTest {
			continue
		}
		key := strconv.Itoa(int(g.owner)) + "\n" + g.key
		group, ok := index[key]
		if !ok {
			group = &stackGroup{owner: g.owner}
			index[key] = group
			groups = append(groups, group)
		}
		group.goroutines = append(group.goroutines, g)
	}
	slices.SortStableFunc(groups, func(a, b *stackGroup) int { return int(a.owner) - int(b.owner) })

	var out strings.Builder
	fmt.Fprintf(&out, "testwait: %s: the goroutine stacks %s: %d goroutines: the waiting one, %d this test started, %d whose creator is no longer running, %d of the test binary, and %d other running tests started, which are not printed\n",
		name, when, len(goroutines), counts[ownedByTest], counts[ownedByNone], counts[ownedByBinary], counts[ownedByOtherTest])
	out.WriteString(goroutines[0].trace + "\n\n")
	printed, printedGoroutines, size := 1, 1, 0
	cutGroups, cutGoroutines := 0, 0
	for _, group := range groups {
		var text strings.Builder
		if n := len(group.goroutines); n > 1 {
			ids := make([]string, 0, min(n, groupIDs))
			for _, g := range group.goroutines[:min(n, groupIDs)] {
				ids = append(ids, strconv.Itoa(g.id))
			}
			more := ""
			if n > groupIDs {
				more = fmt.Sprintf(" and %d more", n-groupIDs)
			}
			fmt.Fprintf(&text, "testwait: the stack below is that of %d goroutines: %s%s\n", n, strings.Join(ids, " "), more)
		}
		text.WriteString(group.goroutines[0].trace + "\n\n")
		if cutGroups > 0 || size+text.Len() > stackBound {
			cutGroups++
			cutGoroutines += len(group.goroutines)
			continue
		}
		out.WriteString(text.String())
		size += text.Len()
		printed++
		printedGoroutines += len(group.goroutines)
	}
	fmt.Fprintf(&out, "testwait: %s: end of the goroutine stacks: %d stacks of %d goroutines printed, %d bytes after the waiting one's; %d stacks of %d goroutines not printed, past the %d KiB bound\n",
		name, printed, printedGoroutines, size, cutGroups, cutGoroutines, stackBound>>10)
	return out.String()
}
