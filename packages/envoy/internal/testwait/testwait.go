// Package testwait holds the one wait Envoy's tests share.
// packages/daemon/internal/testwait and packages/envoy/internal/testwait hold the same code, because
// Go's internal rule keeps one module from importing the other's (packages/envoy/infra imports
// Envoy's through its replace), so a change to one is made to the other.
//
// A test that drives real processes, a real database or a real socket waits for something it
// cannot make happen on its own: a delivery, the transaction that answers it, a line in a log.
//
// # The one bound
//
// Every wait here gives up 30 s (margin) before the test binary's deadline, the one that go test's
// -timeout sets, and at no other moment. No wait takes a bound from its caller, and none has a cap
// of its own: any span a caller chose would either be too long to ever end a wait or too short for
// the day the machine is loaded (the Go project made the same choice in CL 504495), so a step that
// is merely slow never fails a test, and a fact that never comes fails it naming what it waited
// for. Under -timeout 0 there is no deadline and a wait has no end. Once the margin has passed, a
// wait asks its question once and then fails, a wait in a cleanup included, so the cleanups keep
// the margin to run in. The pace at which a condition is asked is the package's own (10 ms), and
// no function takes one.
//
// A wait that gives up writes "testwait: <test>: gave up waiting for <what> 30s before the test
// binary's deadline; last read: <reading>" to stderr at once, then fails the test with the same
// text (InMain, which has no test to fail, returns it as an error); "last read" is there only for
// a wait that has a reading. The direct write is there because Go's alarm panics without printing
// a running test's log, so a cleanup that outlasted the margin would otherwise take the message
// with it. The text is one line: a newline in what or in the reading is written as \n.
//
// # The waits
//
// Eventually waits until a condition holds. Until waits until a reading holds and returns it, and
// its failure prints the last value it read, so a condition that can say what it saw returns that.
// Receive waits for one receive from a channel. Next waits for an item of a Stream, such as a line
// of a captured log, and reads a stream in order: the items before the one it returns, which did
// not match, are passed over, and no later Next reads them, so two calls prove their two items came
// in that order. A test that waits for two items in either order matches both in its first Next
// and waits for the other in its second. Context gives a call that takes a context the same bound.
// InMain is Eventually for a wait in TestMain, which has no t: it takes its deadline from the
// -timeout flag and the process's start, and returns an error where the others fail the test.
//
// A condition runs on the test goroutine, so a condition that reads a report which makes its fact
// impossible (a process that exited, a daemon whose run returned) calls t.Fatalf itself and ends
// the wait at once.
//
// # Proving an absence
//
// A wait proves that something happened. That something did not happen needs a proof of its own,
// taken from the first of these that reaches the code: read the state that would make it happen;
// under testing/synctest, where the code does no real network I/O, call synctest.Wait and then
// read; or wait for the effect of a later action (send a marker after the thing, wait for the
// marker's effect, then read). Only when none of the three reaches the code does a test hold a
// span, through Span or Quiet, whose why says which of them does not reach it and why. They refuse
// an empty why.
//
// # Kept until every caller moves
//
// WithinPolling, Holds, Bounded and Poll take or name a bound or a pace of the caller's, which this
// package no longer offers. They stay only until every caller has moved to the waits above, and
// then they go.
package testwait

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// margin is how long before the test binary's deadline a wait gives up: the time the failing
// test's cleanups and the failure itself have to run in before Go's alarm panics the binary.
const margin = 30 * time.Second

// pace is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const pace = 10 * time.Millisecond

// started is when this process began, for a testing.TB that cannot name its deadline: testing
// arms its alarm at the -timeout flag's span from a moment later than this one, so the deadline
// read from here is a little early and never late.
var started = time.Now()

// Eventually waits until holds reports true.
func Eventually(t testing.TB, what string, holds func() bool) {
	t.Helper()
	poll(t, what, holds, nil)
}

// Until waits until read reports true, and returns what it read then. The failure prints the last
// value read.
func Until[T any](t testing.TB, what string, read func() (T, bool)) T {
	t.Helper()
	var value T
	poll(t, what, func() bool {
		var holds bool
		value, holds = read()
		return holds
	}, func() string { return fmt.Sprintf("%+v", value) })
	return value
}

// Receive waits for one receive from ch (a value, or the close of ch, which gives the zero value).
func Receive[T any](t testing.TB, what string, ch <-chan T) T {
	t.Helper()
	expired, stop := expiry(t)
	defer stop()
	select {
	case value := <-ch:
		return value
	case <-expired:
	}
	// The margin has passed: ask once more, so a value that came beside the timer is taken.
	select {
	case value := <-ch:
		return value
	default:
	}
	gaveUp(t, what, nil)
	var zero T
	return zero
}

// Stream holds items a producer adds (log lines, observed events); Next waits for the first unread
// item that matches. Its zero value is ready. Add may be called from any goroutine, and wakes a
// Next that waits; Next is the test goroutine's, which holds the stream's one read cursor.
type Stream[T any] struct {
	mu    sync.Mutex
	items []T
	// read is the index of the first item no Next has returned or passed over.
	read int
	// changed is closed by the next Add; nil until a Next asks for it.
	changed chan struct{}
}

// Add appends item to the stream.
func (s *Stream[T]) Add(item T) {
	s.mu.Lock()
	s.items = append(s.items, item)
	changed := s.changed
	s.changed = nil
	s.mu.Unlock()
	if changed != nil {
		close(changed)
	}
}

// unread returns the items at index from and after, or at the read cursor and after when that is
// later, with the index of the first of them and the channel the next Add closes. The slice is
// capped at its length, so an Add after the call never writes into what the caller reads.
func (s *Stream[T]) unread(from int) ([]T, int, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = max(from, s.read)
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.items[from:len(s.items):len(s.items)], from, s.changed
}

// passed moves the read cursor past the item at index.
func (s *Stream[T]) passed(index int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.read = index + 1
}

// Next waits for the first unread item of s that matches, moves the read cursor past it and
// returns it. The items before it that did not match are passed over, and no later Next reads
// them. The failure prints the last item that did not match, or none. match runs on the test
// goroutine with no lock held, so it may fail the test itself.
func Next[T any](t testing.TB, what string, s *Stream[T], match func(T) bool) T {
	t.Helper()
	expired, stop := expiry(t)
	defer stop()
	var last T
	seen, next, over := false, 0, false
	for {
		items, first, changed := s.unread(next)
		for i, item := range items {
			if match(item) {
				s.passed(first + i)
				return item
			}
			last, seen = item, true
		}
		next = first + len(items)
		if over {
			break
		}
		select {
		case <-changed:
		case <-expired:
			// The margin has passed: the loop reads what came beside the timer once, then fails.
			over = true
		}
	}
	gaveUp(t, what, func() string {
		if !seen {
			return "none"
		}
		return fmt.Sprintf("%+v", last)
	})
	var zero T
	return zero
}

// Context gives a context whose deadline is the binary's deadline less the margin, cancelled when
// t ends. Under -timeout 0 it has no deadline.
func Context(t testing.TB) context.Context {
	end, bounded := giveUpAt(t)
	if !bounded {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx
	}
	ctx, cancel := context.WithDeadline(context.Background(), end)
	t.Cleanup(cancel)
	return ctx
}

// Span holds the test for d, for the reason why. It is the one named span of a wait that proves
// an absence, for code that neither the test's state, synctest.Wait nor a later action's effect
// reaches, and why says which of them does not and why; an empty why fails the test. A span that
// would end past the margin ends there and fails, as a wait does.
func Span(t testing.TB, d time.Duration, why string) {
	t.Helper()
	requireWhy(t, "Span", why)
	expired, stop := expiry(t)
	defer stop()
	span := time.NewTimer(d)
	defer span.Stop()
	select {
	case <-span.C:
	case <-expired:
		gaveUp(t, fmt.Sprintf("the end of a %s span (%s)", d, why), nil)
	}
}

// Quiet fails t if ch yields within d; why says why no proof from state or a later action reaches
// the code, and an empty why fails the test. A close of ch counts as yielding, because a channel
// a test holds quiet is the one the thing that must not happen is reported through, and a done
// channel reports it by its close. A span that would end past the margin ends there and fails, as
// a wait does.
func Quiet[T any](t testing.TB, what string, ch <-chan T, d time.Duration, why string) {
	t.Helper()
	requireWhy(t, "Quiet", why)
	expired, stop := expiry(t)
	defer stop()
	span := time.NewTimer(d)
	defer span.Stop()
	select {
	case value, open := <-ch:
		if !open {
			t.Fatalf("testwait: %s: %s was closed within %s, where it was to stay quiet (%s)", t.Name(), what, d, why)
			return
		}
		t.Fatalf("testwait: %s: %s yielded %+v within %s, where it was to stay quiet (%s)", t.Name(), what, value, d, why)
	case <-span.C:
	case <-expired:
		gaveUp(t, fmt.Sprintf("the end of a %s quiet span of %s (%s)", d, what, why), nil)
	}
}

// InMain waits until holds reports true, for a wait in TestMain, which has no t, and returns nil
// once it does. Its bound is the one every wait here has, the test binary's deadline less the
// margin, read from the -timeout flag and the process's start; under -timeout 0 it has no end.
// Once the margin has passed it asks once more, then writes "testwait: TestMain: gave up waiting
// for <what> 30s before the test binary's deadline" to stderr and returns an error with the same
// text. Before m.Run the command line is not parsed yet, so InMain parses it, as testing asks of a
// TestMain that reads a flag and as m.Run would.
func InMain(what string, holds func() bool) error {
	end, bounded := mainGiveUpAt()
	if asked(end, bounded, holds) {
		return nil
	}
	return errors.New(announce("TestMain", what, nil))
}

// requireWhy fails t when why says nothing: a span with no reason is a sleep.
func requireWhy(t testing.TB, function, why string) {
	t.Helper()
	if strings.TrimSpace(why) == "" {
		t.Fatalf("testwait: %s: %s needs a why: which of the test's state, synctest.Wait and a later action's effect does not reach the code, and why", t.Name(), function)
	}
}

// poll asks holds at the package's pace until it reports true. Once the moment giveUpAt names has
// passed and holds still reports false, gaveUp fails t, with reading's text when the wait has one.
func poll(t testing.TB, what string, holds func() bool, reading func() string) {
	t.Helper()
	end, bounded := giveUpAt(t)
	if !asked(end, bounded, holds) {
		gaveUp(t, what, reading)
	}
}

// asked asks holds at the package's pace until it reports true, and reports whether it did. Once
// end has passed a false answer ends the asking, so the question is asked once after end; with no
// end (bounded false) it asks until holds reports true.
func asked(end time.Time, bounded bool, holds func() bool) bool {
	for !holds() {
		if bounded && !time.Now().Before(end) {
			return false
		}
		time.Sleep(pace)
	}
	return true
}

// expiry is a channel that receives once the moment giveUpAt names for t has passed, nil (which
// never receives) under -timeout 0, and the function that releases its timer.
func expiry(t testing.TB) (<-chan time.Time, func() bool) {
	end, bounded := giveUpAt(t)
	if !bounded {
		return nil, func() bool { return false }
	}
	timer := time.NewTimer(time.Until(end))
	return timer.C, timer.Stop
}

// giveUpAt is the moment a wait in t gives up: the test binary's deadline less margin, and false
// when the binary has no deadline (-timeout 0). A *testing.T, or any testing.TB with a Deadline
// method, names the deadline; for another (a benchmark) it is the -timeout flag's span from the
// process's start, which is how testing arms its own alarm.
func giveUpAt(t testing.TB) (time.Time, bool) {
	if named, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		return lessMargin(named.Deadline())
	}
	return lessMargin(flagDeadline())
}

// mainGiveUpAt is the moment a wait in TestMain gives up: the -timeout flag's deadline less
// margin, and false under -timeout 0. It parses the command line when nothing has yet.
func mainGiveUpAt() (time.Time, bool) {
	if !flag.Parsed() {
		flag.Parse()
	}
	return lessMargin(flagDeadline())
}

// lessMargin is deadline less margin, and false when there is no deadline.
func lessMargin(deadline time.Time, bounded bool) (time.Time, bool) {
	if !bounded {
		return time.Time{}, false
	}
	return deadline.Add(-margin), true
}

// flagDeadline is the -timeout flag's span from the process's start, and false when the flag is
// absent or 0.
func flagDeadline() (time.Time, bool) {
	timeout := flag.Lookup("test.timeout")
	if timeout == nil {
		return time.Time{}, false
	}
	getter, ok := timeout.Value.(flag.Getter)
	if !ok {
		return time.Time{}, false
	}
	span, ok := getter.Get().(time.Duration)
	if !ok || span <= 0 {
		return time.Time{}, false
	}
	return started.Add(span), true
}

// oneLine writes a newline or a carriage return as its escape, so a failure text is one line.
var oneLine = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// announce is the text of a wait the margin ended, in the test named name: it writes the text to
// stderr at once, in one write, and returns it. reading is the wait's last reading, or nil for a
// wait that has none.
func announce(name, what string, reading func() string) string {
	text := fmt.Sprintf("testwait: %s: gave up waiting for %s %s before the test binary's deadline", name, what, margin)
	if reading != nil {
		text += "; last read: " + reading()
	}
	text = oneLine.Replace(text)
	_, _ = os.Stderr.WriteString(text + "\n")
	return text
}

// gaveUp ends a wait in t that the margin ended: it announces the failure, then fails t with the
// same text.
func gaveUp(t testing.TB, what string, reading func() string) {
	t.Helper()
	t.Fatalf("%s", announce(t.Name(), what, reading))
}

// Poll is the package's pace, 10 ms. It is kept only until every caller of WithinPolling and
// Holds has moved to Eventually or Until, which take no pace.
const Poll = pace

// Bounded is bound, or what is left of the test binary's own deadline less margin when that is
// sooner. It is kept only until every caller has moved to Eventually, Until, Receive, Next or
// Context, whose one bound is the binary's deadline less this package's own margin.
func Bounded(t *testing.T, bound, margin time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - margin; left < bound {
			bound = left
		}
	}
	return bound
}

// WithinPolling waits for condition under the caller's own bound and its own pace, failing the
// test with what it waited for when the bound passes. It is kept only until every caller has moved
// to Eventually or Until, which take neither.
func WithinPolling(t *testing.T, bound, poll time.Duration, what string, condition func() bool) {
	t.Helper()
	if waitUntil(time.Now().Add(bound), poll, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Holds reports whether condition held before bound passed, asked every poll, and fails nothing
// itself. It is kept only until every caller has moved to Until, whose reading carries the
// failure text such a caller builds.
func Holds(bound, poll time.Duration, condition func() bool) bool {
	return waitUntil(time.Now().Add(bound), poll, condition)
}

// waitUntil asks condition every poll until it holds or deadline passes, reporting which. It
// asks once more after the deadline, so a condition that came true during the last sleep is not
// reported false: the alternative loses a race against the thing it waits for, and the cost is
// one more question on the path where the wait has already failed.
func waitUntil(deadline time.Time, poll time.Duration, condition func() bool) bool {
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(poll)
	}
	return condition()
}
