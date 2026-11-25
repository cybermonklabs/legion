package testwait

import (
	"fmt"
	"strings"
	"testing"
)

// trace is a goroutine's trace as runtime.Stack writes it: its header, a frame per function, and
// the line naming its creator when creator is not 0.
func trace(id int, state string, creator int, functions ...string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "goroutine %d [%s]:\n", id, state)
	for i, function := range functions {
		fmt.Fprintf(&out, "%s(0x%x, 0x%x)\n\t/src/example.go:%d +0x%x\n", function, 0xc000+id, i, 10+i, 0x20+id)
	}
	if creator != 0 {
		fmt.Fprintf(&out, "created by example.start in goroutine %d\n\t/src/start.go:7 +0x%x", creator, 0x40+id)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

// A dump whose goroutines belong to the waiting test, to another test, to the binary and to a
// creator that has ended prints the waiting goroutine's trace first, then the test's own, then the
// ended creator's, then the binary's, and counts the other test's without printing them. The
// waiting goroutine is a subtest's, so its parent test's goroutines, and its sibling subtest's, are
// the test's too.
func TestTheStacksPrintTheWaitingTestsGoroutinesFirstAndNoOtherTests(t *testing.T) {
	dump := []string{
		trace(12, "running", 10, "example.awaitTheFact", "testing.tRunner"),
		trace(1, "chan receive", 0, "testing.tRunner", "testing.runTests", "main.main"),
		trace(40, "select", 1, "example.poolHealthCheck"),
		trace(10, "chan receive", 1, "example.TestWaits", "testing.tRunner"),
		trace(11, "IO wait", 10, "example.listenForTheFact"),
		trace(13, "chan receive", 10, "example.TestWaitsSibling", "testing.tRunner"),
		trace(14, "select", 13, "example.siblingWorker"),
		trace(15, "select", 11, "example.relayTheFact"),
		trace(20, "chan receive", 1, "example.TestOther", "testing.tRunner"),
		trace(21, "select", 20, "example.otherTestsWorker"),
		trace(30, "select", 99, "example.leftByAnEndedRequest"),
	}
	got := goroutineStacks("TestWaits/sub", dump)
	if want := "testwait: TestWaits/sub: the goroutine stacks as the wait gave up: 11 goroutines: the waiting one, 5 this test started, 1 whose creator is no longer running, 2 of the test binary, and 2 other running tests started, which are not printed\n"; !strings.HasPrefix(got, want) {
		t.Errorf("the stacks open\n%.300s\nwant the line\n%s", got, want)
	}
	order := []string{"example.awaitTheFact(", "example.TestWaits(", "example.listenForTheFact(", "example.TestWaitsSibling(", "example.siblingWorker(", "example.relayTheFact(", "example.leftByAnEndedRequest(", "main.main(", "example.poolHealthCheck("}
	last := -1
	for _, frame := range order {
		at := strings.Index(got, frame)
		if at < 0 || at < last {
			t.Errorf("the frame %s is at %d of the stacks, want after %d, in the order %q:\n%s", frame, at, last, order, got)
		}
		last = at
	}
	for _, frame := range []string{"example.TestOther(", "example.otherTestsWorker("} {
		if strings.Contains(got, frame) {
			t.Errorf("the stacks print %s, another running test's goroutine:\n%s", frame, got)
		}
	}
	if !strings.Contains(got, trace(12, "running", 10, "example.awaitTheFact", "testing.tRunner")+"\n") {
		t.Errorf("the stacks do not hold the waiting goroutine's trace whole:\n%s", got)
	}
	if want := "testwait: TestWaits/sub: end of the goroutine stacks: 9 stacks of 9 goroutines printed, "; !strings.Contains(got, want) {
		t.Errorf("the stacks hold no last line %q:\n%s", want, got)
	}
}

// Goroutines whose stacks differ only in argument values, offsets and how long each has waited
// print one stack, under a line that counts them and names their ids.
func TestGoroutinesOfOneStackPrintItOnce(t *testing.T) {
	dump := []string{trace(5, "running", 1, "example.awaitTheFact", "testing.tRunner"), trace(1, "chan receive", 0, "testing.runTests")}
	for id := 6; id < 9; id++ {
		dump = append(dump, trace(id, fmt.Sprintf("select, %d minutes", id), 5, "example.worker"))
	}
	got := goroutineStacks("TestWaits", dump)
	if n := strings.Count(got, "example.worker("); n != 1 {
		t.Errorf("the stacks print the worker's frame %d times, want once:\n%s", n, got)
	}
	if want := "testwait: the stack below is that of 3 goroutines: 6 7 8\n" + trace(6, "select, 6 minutes", 5, "example.worker") + "\n"; !strings.Contains(got, want) {
		t.Errorf("the stacks hold no\n%s\n:\n%s", want, got)
	}
}

// Under GOTRACEBACK=system or crash, runtime.Stack's headers name the goroutine's g and m before its
// state, and each frame's file line ends with its fp, sp and pc: the stacks still know whose each
// goroutine is, and still print two goroutines of one stack once.
func TestTheStacksReadATracebackOfLevelSystem(t *testing.T) {
	system := func(id int, state string, creator int, functions ...string) string {
		text := trace(id, state, creator, functions...)
		text = strings.Replace(text, fmt.Sprintf("goroutine %d [", id), fmt.Sprintf("goroutine %d gp=0x%x m=nil [", id, 0xc00+id), 1)
		var lines []string
		for line := range strings.Lines(text) {
			line = strings.TrimSuffix(line, "\n")
			if strings.HasPrefix(line, "\t") {
				line += fmt.Sprintf(" fp=0x%x sp=0x%x pc=0x%x", 0xf00+id, 0xe00+id, 0xd00+id)
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	dump := []string{
		system(5, "running", 1, "example.awaitTheFact", "testing.tRunner"),
		system(1, "chan receive", 0, "testing.runTests"),
		system(6, "select", 5, "example.worker"),
		system(7, "select", 5, "example.worker"),
		system(8, "chan receive", 1, "example.TestOther", "testing.tRunner"),
	}
	got := goroutineStacks("TestWaits", dump)
	if want := "the goroutine stacks as the wait gave up: 5 goroutines: the waiting one, 2 this test started, 0 whose creator is no longer running, 1 of the test binary, and 1 other running tests started"; !strings.Contains(got, want) {
		t.Errorf("the stacks of a system traceback open\n%.300s\nwant them to count %q", got, want)
	}
	if want := "testwait: the stack below is that of 2 goroutines: 6 7\n"; !strings.Contains(got, want) || strings.Contains(got, "example.TestOther(") {
		t.Errorf("the stacks of a system traceback hold no %q, or print another test's goroutine:\n%s", want, got)
	}
}

// Past stackBound of text after the waiting goroutine's trace, the stacks print no more, and the
// last line counts what they left out; the waiting goroutine's trace is printed whatever its size.
func TestTheStacksStopAtTheirBound(t *testing.T) {
	frames := make([]string, 0, 400)
	for i := range 400 {
		frames = append(frames, fmt.Sprintf("example.deep%d", i))
	}
	waiting := trace(5, "running", 1, append(frames, "testing.tRunner")...)
	dump := []string{waiting, trace(1, "chan receive", 0, "testing.runTests")}
	for id := 100; id < 1100; id++ {
		dump = append(dump, trace(id, "select", 5, fmt.Sprintf("example.worker%d", id), "example.loop", "example.serve"))
	}
	got := goroutineStacks("TestWaits", dump)
	if len(waiting) < stackBound/16 || !strings.Contains(got, waiting+"\n") {
		t.Fatalf("the stacks do not hold the waiting goroutine's trace of %d bytes whole", len(waiting))
	}
	_, after, _ := strings.Cut(got, waiting+"\n\n")
	printed, _, _ := strings.Cut(after, "testwait: TestWaits: end of the goroutine stacks: ")
	if len(printed) > stackBound {
		t.Errorf("the stacks print %d bytes after the waiting goroutine's trace, more than the bound of %d", len(printed), stackBound)
	}
	var stacks, cut int
	if _, err := fmt.Sscanf(got[strings.LastIndex(got, "end of the goroutine stacks: "):], "end of the goroutine stacks: %d stacks of", &stacks); err != nil {
		t.Fatalf("the last line of the stacks does not count them: %v\n%s", err, got[strings.LastIndex(got, "testwait: "):])
	}
	if _, err := fmt.Sscanf(got[strings.LastIndex(got, "; ")+2:], "%d stacks of", &cut); err != nil || stacks+cut != 1002 || cut == 0 {
		t.Errorf("the last line counts %d stacks printed and %d cut (%v), want a cut and 1002 in all:\n%s", stacks, cut, err, got[strings.LastIndex(got, "testwait: "):])
	}
	if n := strings.Count(printed, "example.loop("); n != stacks-1 {
		t.Errorf("the stacks print %d workers, and their last line counts %d stacks with the waiting one", n, stacks)
	}
}

// The first give-up of a binary prints the stacks, a wait begun past the margin among them; a later
// wait begun past the margin prints one line saying why it prints none, and a later wait that
// waited until the margin prints them again.
func TestAGiveUpBegunPastTheMarginPrintsTheStacksOnlyWhenNoneHave(t *testing.T) {
	was := stacksPrinted.Load()
	t.Cleanup(func() { stacksPrinted.Store(was) })
	header := "testwait: TestWaits: the goroutine stacks as the wait gave up: "
	stacksPrinted.Store(false)
	if got := stacksFor("TestWaits", true); !strings.HasPrefix(got, header) {
		t.Errorf("the binary's first give-up, begun past the margin, printed %.200q, want the stacks", got)
	}
	if got, want := stacksFor("TestWaits", true), "testwait: TestWaits: no goroutine stacks for this give-up: an earlier give-up of this test binary printed them\n"; got != want {
		t.Errorf("a later give-up begun past the margin printed %.200q, want %q", got, want)
	}
	if got := stacksFor("TestWaits", false); !strings.HasPrefix(got, header) {
		t.Errorf("a later give-up that waited until the margin printed %.200q, want the stacks", got)
	}
}
