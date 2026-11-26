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
	"testing"
	"time"
)

// stackBound is the most text a give-up prints of goroutine stacks after the waiting goroutine's
// own. The delivery hub's rig was 14.7 KiB; an api package alarm dump was 548 KiB, so 64 KiB keeps
// one test readable and says what it cut.
const stackBound = 64 << 10

// oneLine keeps diagnostic text to one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// Wait is one wait that ended. Began and End are the caller's values, and EndDescription says what
// its end means (the test deadline less a caller's spare, for example).
type Wait struct {
	What                    string
	Began, End              time.Time
	EndDescription          string
	// Spent says the caller's clock had already passed End when the wait began.
	Spent bool
	// When says when the stacks were read. Empty means the wait gave up itself.
	When string
}

// waited is a completed wait that can explain a later one that begins after its end.
type waited struct {
	test, what string
	duration   time.Duration
}

// binary holds the state shared by every Envoy and daemon testwait package linked into this test
// binary. The longest completed wait is named in a collateral failure; a first spent wait with no
// completed wait does not make a false causal claim or print an unrelated dump.
var binary struct {
	sync.Mutex
	longest *waited
}

// ResetForTest clears process state for a test that must isolate a test binary's diagnostics.
// Production wait packages never call it.
func ResetForTest() {
	binary.Lock()
	binary.longest = nil
	binary.Unlock()
}

// Diagnostics is the stack text that follows the failure of wait in t. A wait that reached its end
// records itself and prints a dump. A wait that began after its own end did not wait: its line says
// it is collateral, how long it waited, how far after its caller's end it began, and names the
// longest completed testwait wait recorded so far. It prints no dump: a later real wait prints the
// relevant one, and a first spent wait has no evidence that it caused the deadline to pass.
func Diagnostics(t testing.TB, wait Wait) string {
	now := time.Now()
	wait.What = oneLine.Replace(wait.What)
	if !wait.Spent {
		duration := now.Sub(wait.Began)
		binary.Lock()
		if binary.longest == nil || duration > binary.longest.duration {
			binary.longest = &waited{test: t.Name(), what: wait.What, duration: duration}
		}
		binary.Unlock()
		when := wait.When
		if when == "" {
			when = "as the wait gave up"
		}
		return dumpStacks(t.Name(), when, traces())
	}

	late := wait.Began.Sub(wait.End)
	duration := now.Sub(wait.Began)
	binary.Lock()
	cause := binary.longest
	binary.Unlock()
	line := fmt.Sprintf("testwait: %s: this give-up is collateral: it began %s after %s and waited %s; ",
		t.Name(), late.Round(time.Millisecond), wait.EndDescription, duration.Round(time.Millisecond))
	if cause == nil {
		return line + "no completed testwait give-up is recorded yet\n"
	}
	return fmt.Sprintf("%sthe longest completed testwait give-up recorded so far waited %s: %s, waiting for %s\n",
		line, cause.duration.Round(time.Millisecond), cause.test, cause.what)
}

// Print writes stacks to t's output, beside t's own log. go test -json files each line under t,
// and go test without -v prints them with t's failure.
func Print(t testing.TB, stacks string) {
	_, _ = io.WriteString(t.Output(), stacks)
}

// Fail records text as t's failure, writes the diagnostics after it, then stops t. Every internal
// testwait failure follows this order so a reader sees the line before its stacks.
func Fail(t testing.TB, text, diagnostics string) {
	t.Helper()
	t.Errorf("%s", text)
	Print(t, diagnostics)
	t.FailNow()
}

// traces is the trace of every goroutine, the calling one's first, from one dump of them all.
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

// owner is whose goroutine a stack is, in the order a give-up prints them.
type owner int

const (
	ownedByWait owner = iota
	ownedByTest
	ownedByNone
	ownedByBinary
	ownedByOtherTest
)

type goroutine struct {
	id      int
	creator int
	trace   string
	key     string
	test    bool
	runner  bool
	owner   owner
}

var (
	traceHeader  = regexp.MustCompile(`^goroutine (\d+)(?: [^\[]*)? \[(.*)\]:$`)
	traceCreator = regexp.MustCompile(`(?m)^created by (.*) in goroutine (\d+)$`)
	waitedFor    = regexp.MustCompile(`, \d+ minutes?`)
	frameArgs    = regexp.MustCompile(`\([^()]*\)$`)
	frameOffset  = regexp.MustCompile(` \+0x[0-9a-f]+( fp=.*)?$`)
)

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

type stackGroup struct {
	owner      owner
	goroutines []*goroutine
}

type groupKey struct {
	owner owner
	stack string
}

const groupIDs = 20

func goroutineStacks(name string, traces []string) string {
	return dumpStacks(name, "as the wait gave up", traces)
}

func dumpStacks(name, when string, traces []string) string {
	goroutines := ownTraces(traces)
	counts := map[owner]int{}
	var groups []*stackGroup
	index := map[groupKey]*stackGroup{}
	for _, g := range goroutines[1:] {
		counts[g.owner]++
		if g.owner == ownedByOtherTest {
			continue
		}
		key := groupKey{owner: g.owner, stack: g.key}
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
