package testwait

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// childEnv tells the re-executed test binary to run the helpers below, which skip in an ordinary
// run. One gives up on purpose under a short -timeout and the other reads the deadline under
// -timeout 0, and the parent names each flag, so each runs in a binary of its own.
const childEnv = "TESTWAIT_CHILD"

// childTimeout is the -timeout the parent gives the child that gives up: its waits give up 5 s
// into the run, 30 s before its deadline.
const childTimeout = "35s"

// A wait whose condition holds returns from its first question: it asks once and never sleeps
// at the pace first.
func TestAWaitThatHoldsReturnsAtOnce(t *testing.T) {
	asks := 0
	Eventually(t, "a condition that holds", func() bool { asks++; return true })
	if asks != 1 {
		t.Fatalf("Eventually asked a condition that holds %d times, want 1", asks)
	}
	asks = 0
	if got := Until(t, "a reading that holds", func() (string, bool) { asks++; return "the reading", true }); got != "the reading" {
		t.Fatalf("Until returned %q, want the reading that held", got)
	}
	if asks != 1 {
		t.Fatalf("Until asked a reading that holds %d times, want 1", asks)
	}
	sent := make(chan int, 1)
	sent <- 7
	if got := Receive(t, "a value already sent", sent); got != 7 {
		t.Fatalf("Receive returned %d, want the 7 sent", got)
	}
	var stream Stream[string]
	stream.Add("already there")
	if got := Next(t, "an item already added", &stream, func(string) bool { return true }); got != "already there" {
		t.Fatalf("Next returned %q, want the item already added", got)
	}
}

// Until returns the reading that held, not one it read before it.
func TestUntilReturnsTheReadingThatHeld(t *testing.T) {
	asks := 0
	got := Until(t, "the third reading", func() (int, bool) { asks++; return asks, asks == 3 })
	if got != 3 {
		t.Fatalf("Until returned %d, want the third reading, the one that held", got)
	}
}

// Next reads a stream in order: it returns the first unread item that matches, and the items
// before it are passed over, so no later Next reads them.
func TestNextAdvancesItsCursor(t *testing.T) {
	var stream Stream[string]
	for _, item := range []string{"a", "b", "c"} {
		stream.Add(item)
	}
	if got := Next(t, "b", &stream, func(item string) bool { return item == "b" }); got != "b" {
		t.Fatalf("Next for b returned %q", got)
	}
	// a came before b, so the Next for b passed it over: a match for a or c finds c.
	if got := Next(t, "a or c", &stream, func(item string) bool { return item == "a" || item == "c" }); got != "c" {
		t.Fatalf("Next for a or c returned %q, want c: a was passed over by the Next for b", got)
	}
	// Every item is read: the next Next waits for an Add, which a producer makes.
	go stream.Add("d")
	if got := Next(t, "an item added after the wait began", &stream, func(string) bool { return true }); got != "d" {
		t.Fatalf("Next for a later item returned %q, want d", got)
	}
}

// Producers on goroutines of their own add while the test goroutine reads: every item arrives
// once, and the race detector sees the stream's lock rather than a race.
func TestNextReadsEveryItemThatConcurrentProducersAdd(t *testing.T) {
	const producers, each = 4, 100
	var stream Stream[string]
	var wrote sync.WaitGroup
	for producer := range producers {
		wrote.Go(func() {
			for item := range each {
				stream.Add(fmt.Sprintf("%d/%d", producer, item))
			}
		})
	}
	read := make([]string, 0, producers*each)
	for range producers * each {
		read = append(read, Next(t, "the next item a producer adds", &stream, func(string) bool { return true }))
	}
	wrote.Wait()
	slices.Sort(read)
	if unique := slices.Compact(slices.Clone(read)); len(unique) != producers*each {
		t.Fatalf("Next read %d distinct items of %d added", len(unique), producers*each)
	}
}

// Receive returns a value a sender sends later, and returns on a close with the zero value.
func TestReceiveReturnsASentValueAndAClose(t *testing.T) {
	sent := make(chan string)
	go func() { sent <- "sent later" }()
	if got := Receive(t, "a value a sender sends later", sent); got != "sent later" {
		t.Fatalf("Receive returned %q, want the value sent", got)
	}
	closed := make(chan string)
	close(closed)
	if got := Receive(t, "the close of a channel", closed); got != "" {
		t.Fatalf("Receive on a closed channel returned %q, want the zero value", got)
	}
}

// Context's deadline is the moment a wait gives up, and the context ends with the test that took it.
func TestContextEndsWhereAWaitGivesUpAndWithTheTest(t *testing.T) {
	var taken context.Context
	t.Run("taker", func(t *testing.T) {
		taken = Context(t)
		end, bounded := giveUpAt(t)
		deadline, has := taken.Deadline()
		if has != bounded || !deadline.Equal(end) {
			t.Fatalf("Context's deadline is %v (%t), want the moment a wait gives up, %v (%t)", deadline, has, end, bounded)
		}
		if err := taken.Err(); err != nil {
			t.Fatalf("Context ended before its test did: %v", err)
		}
	})
	if err := taken.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Context outlived the test that took it: its error is %v, want context.Canceled", err)
	}
}

// A wait gives up the margin before the test binary's deadline. A *testing.T names that
// deadline; a testing.TB without Deadline, and TestMain, read it from the -timeout flag and the
// process's start, which is no later than testing's own.
func TestAWaitGivesUpTheMarginBeforeTheDeadline(t *testing.T) {
	deadline, bounded := t.Deadline()
	end, ok := giveUpAt(t)
	if ok != bounded || ok && !end.Equal(deadline.Add(-margin)) {
		t.Fatalf("a wait in a test whose deadline is %v (%t) gives up at %v (%t), want %s before it", deadline, bounded, end, ok, margin)
	}
	fromFlag, ok := giveUpAt(noDeadline{t})
	if ok != bounded || ok && fromFlag.After(end) {
		t.Fatalf("a wait in a testing.TB without Deadline gives up at %v (%t), want no later than %v (%t)", fromFlag, ok, end, bounded)
	}
	if inMain, ok := mainGiveUpAt(); ok != bounded || !inMain.Equal(fromFlag) {
		t.Fatalf("a wait in TestMain gives up at %v (%t), want where a testing.TB without Deadline does, %v (%t)", inMain, ok, fromFlag, bounded)
	}
}

// InMain returns nil from its first question when its condition holds, and once it holds when it
// comes to.
func TestInMainReturnsNilOnceItsConditionHolds(t *testing.T) {
	asks := 0
	if err := InMain("a condition that holds", func() bool { asks++; return true }); err != nil || asks != 1 {
		t.Fatalf("InMain over a condition that holds returned %v after %d questions, want nil after 1", err, asks)
	}
	asks = 0
	if err := InMain("a condition that holds at its third question", func() bool { asks++; return asks == 3 }); err != nil || asks != 3 {
		t.Fatalf("InMain over a condition that holds at its third question returned %v after %d questions, want nil after 3", err, asks)
	}
}

// Span and Quiet hold the test for their span when given a reason, and Quiet passes when nothing
// comes.
func TestSpanAndQuietHoldTheirSpan(t *testing.T) {
	Span(t, time.Millisecond, "the test of Span itself: its span is what it proves")
	Quiet(t, "a channel no one sends on", make(chan int), time.Millisecond, "the test of Quiet itself: its span is what it proves")
}

// Span and Quiet refuse a span with no reason: one is a sleep.
func TestSpanAndQuietRefuseAnEmptyWhy(t *testing.T) {
	for _, why := range []string{"", " \t\n"} {
		failure := failureOf(t, func(tb testing.TB) { Span(tb, time.Hour, why) })
		if want := "testwait: " + t.Name() + ": Span needs a why"; !strings.HasPrefix(failure, want) {
			t.Fatalf("Span with the why %q failed with %q, want a failure that begins %q", why, failure, want)
		}
		failure = failureOf(t, func(tb testing.TB) { Quiet(tb, "a channel", make(chan int), time.Hour, why) })
		if want := "testwait: " + t.Name() + ": Quiet needs a why"; !strings.HasPrefix(failure, want) {
			t.Fatalf("Quiet with the why %q failed with %q, want a failure that begins %q", why, failure, want)
		}
	}
}

// Quiet fails naming what yielded and the value, and a close counts as yielding.
func TestQuietFailsWhenItsChannelYields(t *testing.T) {
	sent := make(chan string, 1)
	sent <- "an early stop"
	failure := failureOf(t, func(tb testing.TB) { Quiet(tb, "the stop", sent, time.Hour, "the stop is reported nowhere else") })
	if want := "the stop yielded an early stop within 1h0m0s"; !strings.Contains(failure, want) {
		t.Fatalf("Quiet over a channel holding a value failed with %q, want it to say %q", failure, want)
	}
	closed := make(chan string)
	close(closed)
	failure = failureOf(t, func(tb testing.TB) { Quiet(tb, "the stop", closed, time.Hour, "the stop is reported nowhere else") })
	if want := "the stop was closed within 1h0m0s"; !strings.Contains(failure, want) {
		t.Fatalf("Quiet over a closed channel failed with %q, want it to say %q", failure, want)
	}
}

// Under a short -timeout every wait gives up 30 s before the deadline, before Go's alarm: it writes
// its line to stderr and fails the test with the same text, which carries the wait's last reading
// where it has one. A wait that starts once the margin has passed, a wait in a cleanup included,
// asks once and fails.
func TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestTheWaitsGiveUpChild$", "-test.timeout="+childTimeout, "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	var stdout, stderr bytes.Buffer
	child.Stdout, child.Stderr = &stdout, &stderr
	if err := child.Run(); err == nil {
		t.Fatalf("the child was meant to fail; it passed:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	if strings.Contains(stdout.String(), "panic: test timed out") || strings.Contains(stderr.String(), "panic: test timed out") {
		t.Fatalf("the child's waits ran into the binary's deadline rather than giving up before it:\n%s\nstderr:\n%s", &stdout, &stderr)
	}
	name := "TestTheWaitsGiveUpChild/"
	ending := " 30s before the test binary's deadline"
	want := []string{
		"testwait: " + name + "until: gave up waiting for a reading that never holds" + ending + "; last read: a later reading",
		"testwait: " + name + "eventually: gave up waiting for a condition that never holds" + ending,
		"testwait: " + name + "next_on_an_empty_stream: gave up waiting for an item no one adds" + ending + "; last read: none",
		"testwait: " + name + "next_past_unmatched_items: gave up waiting for an item that matches" + ending + `; last read: the latest\nitem`,
		"testwait: " + name + "receive: gave up waiting for a send no one makes" + ending,
		"testwait: " + name + "cleanup: gave up waiting for a condition a cleanup waits for" + ending,
		"testwait: " + name + "span: gave up waiting for the end of a 1h0m0s span (a span past the margin)" + ending,
		"testwait: TestMain: gave up waiting for a condition TestMain waits for" + ending,
	}
	written := strings.Split(stderr.String(), "\n")
	for _, line := range want {
		if !slices.Contains(written, line) {
			t.Errorf("the child wrote no stderr line %q; its stderr:\n%s", line, &stderr)
		}
		if !strings.Contains(stdout.String(), line+"\n") {
			t.Errorf("the child failed no test with %q; its output:\n%s", line, &stdout)
		}
	}
	if !strings.Contains(stdout.String(), "the condition was asked 1 time(s)") {
		t.Errorf("a wait that began after the margin did not ask exactly once; the child's output:\n%s", &stdout)
	}
}

// Under -timeout 0 there is no deadline: a wait has no moment to give up at and no timer, in a
// test or in TestMain, and Context has no deadline.
func TestUnderNoTimeoutAWaitHasNoEnd(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run=^TestNoDeadlineChild$", "-test.timeout=0", "-test.v")
	child.Env = append(os.Environ(), childEnv+"=1")
	output, err := child.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "--- PASS: TestNoDeadlineChild (") {
		t.Fatalf("under -timeout 0 the child did not pass: %v\n%s", err, output)
	}
}

// TestTheWaitsGiveUpChild is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure,
// run under -timeout 35s: its first wait gives up 5 s in, and every later one starts past the
// margin. It runs only when the parent re-executes this binary.
func TestTheWaitsGiveUpChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestAWaitGivesUpWithItsLineOnStderrAndTheSameFailure")
	}
	t.Run("until", func(t *testing.T) {
		asks := 0
		Until(t, "a reading that never holds", func() (string, bool) {
			asks++
			if asks == 1 {
				return "the first reading", false
			}
			return "a later reading", false
		})
	})
	t.Run("eventually", func(t *testing.T) {
		asks := 0
		t.Cleanup(func() { t.Logf("the condition was asked %d time(s)", asks) })
		Eventually(t, "a condition that never holds", func() bool { asks++; return false })
	})
	t.Run("next on an empty stream", func(t *testing.T) {
		var stream Stream[string]
		Next(t, "an item no one adds", &stream, func(string) bool { return true })
	})
	t.Run("next past unmatched items", func(t *testing.T) {
		var stream Stream[string]
		stream.Add("an earlier item")
		stream.Add("the latest\nitem")
		Next(t, "an item that matches", &stream, func(string) bool { return false })
	})
	t.Run("receive", func(t *testing.T) {
		Receive(t, "a send no one makes", make(chan int))
	})
	t.Run("cleanup", func(t *testing.T) {
		t.Cleanup(func() { Eventually(t, "a condition a cleanup waits for", func() bool { return false }) })
	})
	t.Run("span", func(t *testing.T) {
		Span(t, time.Hour, "a span past the margin")
	})
	t.Run("in main", func(t *testing.T) {
		if err := InMain("a condition TestMain waits for", func() bool { return false }); err != nil {
			t.Fatal(err)
		}
	})
}

// TestNoDeadlineChild is the child of TestUnderNoTimeoutAWaitHasNoEnd, run under -timeout 0. It
// runs only when the parent re-executes this binary.
func TestNoDeadlineChild(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		t.Skip(childEnv + " is not set: this test is the child of TestUnderNoTimeoutAWaitHasNoEnd")
	}
	if end, ok := giveUpAt(t); ok {
		t.Errorf("under -timeout 0 a wait gives up at %v, want never", end)
	}
	if end, ok := giveUpAt(noDeadline{t}); ok {
		t.Errorf("under -timeout 0 a wait in a testing.TB without Deadline gives up at %v, want never", end)
	}
	if end, ok := mainGiveUpAt(); ok {
		t.Errorf("under -timeout 0 a wait in TestMain gives up at %v, want never", end)
	}
	expired, stop := expiry(t)
	stop()
	if expired != nil {
		t.Errorf("under -timeout 0 a wait has a timer to give up on, want none")
	}
	if deadline, ok := Context(t).Deadline(); ok {
		t.Errorf("under -timeout 0 Context has the deadline %v, want none", deadline)
	}
}

// noDeadline is a testing.TB with no Deadline method, as a benchmark's is.
type noDeadline struct{ testing.TB }

// failing is a testing.TB whose Fatalf keeps its text and ends the goroutine, as testing's does,
// so a test reads a failure the function under test raised without failing itself.
type failing struct {
	testing.TB
	text string
}

func (f *failing) Fatalf(format string, args ...any) {
	f.text = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// failureOf runs call on a goroutine of its own with a failing testing.TB, and returns the text it
// failed with, or "" when it returned.
func failureOf(t *testing.T, call func(testing.TB)) string {
	t.Helper()
	tb := &failing{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(tb)
	}()
	Receive(t, "the call to end", done)
	return tb.text
}
