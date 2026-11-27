// Package testwait holds the one wait Envoy's tests share, packages/envoy/infra's among them
// (that module imports Envoy's packages through its replace). The daemon's tests have a wait of
// their own, packages/daemon/internal/testwait, since Go's internal rule keeps the daemon's module
// from importing this one. That package is not a copy of this one: its waits take a bound of
// their own (WithinPolling, Bounded).
//
// A test that drives real processes, a real database or a real socket waits for something it
// cannot make happen on its own: a delivery, the transaction that answers it, a line in a log. Such
// a wait goes through this package, and R2 of "The design of this repository's tests" in
// docs/testing.md says why it takes no bound of the caller's.
//
// # The bound
//
// Every wait here but Eventually gives up 30 s (margin) before the test binary's deadline, the one
// that go test's -timeout sets, and at no other moment. None of them takes a bound from its caller
// or has a cap of its own: any span a caller chose would either be too long to ever end a wait or
// too short for the day the machine is loaded (the Go project made the same choice in CL 504495),
// so a step that is merely slow fails no such wait, and a fact that never comes fails it naming
// what it waited for. Under -timeout 0 there is no deadline, and such a wait has no end. Once the
// margin has passed, such a wait asks its question once and then fails, a wait in a cleanup
// included, so the cleanups keep the margin to run in.
//
// Eventually keeps the end main's Eventually had, because the rows main wrote with it were written
// under that end: it gives up a minute after it began, or a second before the test binary's
// deadline when that is sooner, and a minute after it began under -timeout 0. A step slower than a
// minute fails it, so a row whose fact can take longer waits with Until. Under a -timeout of 30 s
// or less it still waits, up to a second before the deadline, where every other wait asks once and
// fails.
//
// The pace at which a condition is asked is the package's own (10 ms), and no wait here takes one.
// legacy.go is the exception to this and to the bound above: it keeps main's UntilPolling,
// WithinPolling, Holds and Bounded, which take a pace, a bound or both from their caller, for the
// callers not yet moved off them.
//
// A wait that gives up writes "testwait: <test>: gave up waiting for <what> 30s before the test
// binary's deadline; last read: <reading>" to stderr at once, then fails the test with the same
// text (InMain, which has no test to fail, returns it as an error); "last read" is there only for
// a wait that has a reading. An Eventually writes "testwait: <test>: gave up waiting for <what>
// after Eventually's one minute", or "... 1s before the test binary's deadline" when the deadline
// came first, the same way. The direct write is there because Go's alarm panics without printing a
// running test's log, so a cleanup that outlasted the margin would otherwise take the message with
// it. The text is one line: a newline in what or in the reading is written as \n.
//
// # The waits
//
// Eventually waits until a condition holds. Until waits until a reading holds and returns it, and
// its failure prints the last value it read, so a condition that can say what it saw returns that.
// Receive waits for one receive from a channel. Next waits for an item of a Stream, such as a line
// of a captured log, and reads a stream in order: the items before the one it returns, which did
// not match, are passed over, and no later Next reads them, so two calls prove their two items came
// in that order. A test that waits for two items in either order matches both in its first Next
// and waits for the other in its second. Returns waits for a call, run on a goroutine of its own,
// to return: a call that waits on what the code under test holds, such as a database pool's Close,
// which returns only once every connection in use has come back. Context gives a call that takes
// a context the same bound, and GiveUpAt names its moment for a call that takes a deadline or a
// span instead (a socket's SetDeadline, a client's timeout option). InMain waits until a condition
// holds, as Eventually does, under the bound of every other wait, for a wait in TestMain, which has
// no t: it takes its deadline from the -timeout flag and the process's start, and returns an error
// where the others fail the test. InHelper and ReceiveInHelper are InMain and Receive for a helper
// whose callers hand it no t, such as clocktest's fake, and name that helper where InMain names
// TestMain. A wait inside a testing/synctest bubble takes the t Bubble hands its function, since
// the bubble's own t refuses to name a deadline: Bubble names what was left of the test's deadline
// as the bubble began, past the bubble's start on its clock.
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
// read, or, where a timer of the code's own may let it act late, call Idle with a span past that
// timer and then read; or wait for the effect of a later action (send a marker after the thing,
// wait for the marker's effect, then read). Only when none of the three reaches the code does a
// test hold a span, through Span or Quiet, whose why says which of them does not reach it and why.
// Idle's why names the timer its span outlasts. Idle, Span and Quiet refuse an empty why, and
// none of them holds a span of its own: the caller passes it.
//
// # A child that gives up
//
// A row that reads how a wait gives up, or what a fixture does once it has, runs the test that
// does it as a child of the test binary (RunChild, which runs the binary the row's own test file
// names), once, under a -timeout that is a hang guard only. The child runs its setup on the wall
// clock, under the shared wait's own bound, and then takes the give-up as a step of its own
// (SetUpDone): from then on its waits read a clock that stands where they give up, so what the
// child reports follows from what its waits did, not from how long a loaded machine took over the
// setup. This package's own child rows give their waits such a clock from their start, and move it
// themselves or let the timers their waits arm on it move it.
//
// A fixture that keeps a span or a deadline of its own, a hang guard on a request, a stand-in's
// latency, a socket's deadline at the give-up, reads it on the same clock (Now, Within, Pause,
// WallTime, Stepped): on the wall clock it is what it was, and in a child that took the step it
// passes on the step clock, in its moment's order, and one that would end past the binary's
// deadline there ends the binary as go test's alarm would. None of them is a wait of a test's own.
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

// eventuallySpare is how long before the test binary's deadline Eventually gives up when that comes
// before its minute: the second main's Eventually left (Bounded(t, time.Minute, time.Second)), so a
// -timeout shorter than a minute still reports the wait rather than go test's alarm.
const eventuallySpare = time.Second

// Eventually waits until holds reports true. It gives up where main's Eventually did, on the clock
// the waits read: a minute after it began, or a second before the test binary's deadline
// (eventuallySpare) when that is sooner, and a minute after it began under -timeout 0. So it is the
// one wait here that does not give up at the margin. Its failure names the end that came
// ("testwait: <test>: gave up waiting for <what> after Eventually's one minute", or "... 1s before
// the test binary's deadline"). A row whose fact can take longer than a minute calls Until, which
// has no minute.
func Eventually(t testing.TB, what string, holds func() bool) {
	t.Helper()
	waits := currentClock()
	end, when := eventuallyEnd(t, waits.Now())
	if !askedBy(waits, end, holds) {
		t.Fatalf("%s", announce(t.Name(), what, when, nil))
	}
}

// eventuallyEnd is the moment an Eventually of t that began at now gives up, and how its failure
// says that moment (afterTheMinute, aSecondBeforeTheDeadline): a minute past now, or the test
// binary's deadline less eventuallySpare when that is sooner.
func eventuallyEnd(t testing.TB, now time.Time) (time.Time, string) {
	end, when := now.Add(time.Minute), afterTheMinute
	if deadline, bounded := deadlineOf(t); bounded && deadline.Add(-eventuallySpare).Before(end) {
		end, when = deadline.Add(-eventuallySpare), aSecondBeforeTheDeadline
	}
	return end, when
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
	value, ok := received(ch, expired)
	if !ok {
		gaveUp(t, what, nil)
	}
	return value
}

// received waits for one receive from ch until expired receives, then takes a value that came
// beside the timer, and reports whether it received one.
func received[T any](ch <-chan T, expired <-chan time.Time) (T, bool) {
	select {
	case value := <-ch:
		return value, true
	case <-expired:
	}
	// The margin has passed: ask once more, so a value that came beside the timer is taken.
	select {
	case value := <-ch:
		return value, true
	default:
	}
	var zero T
	return zero, false
}

// Returns runs fn on a goroutine of its own and waits for it to return. It is for a call that
// waits on what the code under test holds or does, which a row that failed, or the defect it
// caught, can leave held for good (a pool's Close waits for every connection in use to come back):
// the wait then fails naming what, where the call made on the test goroutine would hang the test
// binary to its timeout. What fn writes is the caller's to read once Returns has returned. fn
// runs off the test goroutine, so it calls no method of t that ends the test.
func Returns(t testing.TB, what string, fn func()) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		fn()
	}()
	Receive(t, what, returned)
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

// GiveUpAt is the moment every wait in t but Eventually gives up: the test binary's deadline less
// the margin, and false when the binary has no deadline (-timeout 0). It is the one reading of that
// moment outside this package, for a call that takes a deadline or a span where Context gives a
// context.
func GiveUpAt(t testing.TB) (time.Time, bool) {
	return lessMargin(deadlineOf(t))
}

// deadlineOf is the test binary's deadline as t names it, and false when the binary has none
// (-timeout 0). A *testing.T, or any testing.TB with a Deadline method, names it; for another (a
// benchmark) it is the -timeout flag's span from the process's start, which is how testing arms its
// own alarm.
func deadlineOf(t testing.TB) (time.Time, bool) {
	if named, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		return named.Deadline()
	}
	return flagDeadline()
}

// Context gives a context whose deadline is the binary's deadline less the margin (GiveUpAt),
// cancelled when t ends. Under -timeout 0 it has no deadline.
func Context(t testing.TB) context.Context {
	end, bounded := GiveUpAt(t)
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
	requireWhy(t, "Span", why, whyNoProofReaches)
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
	requireWhy(t, "Quiet", why, whyNoProofReaches)
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
	return InHelper("TestMain", what, holds)
}

// InHelper is InMain for a wait in a helper that has no t because its callers hand it none: its
// give-up text names helper where InMain's names TestMain ("testwait: <helper>: gave up waiting
// for <what> …"), and its bound and its parse of the command line are InMain's.
func InHelper(helper, what string, holds func() bool) error {
	end, bounded := mainGiveUpAt()
	if asked(end, bounded, holds) {
		return nil
	}
	return errors.New(announce(helper, what, beforeTheDeadline, nil))
}

// ReceiveInHelper is Receive for a helper that has no t because its callers hand it none, as
// InHelper is InMain for one: it waits for one receive from ch and returns what it received and
// nil. Its bound and its parse of the command line are InMain's. Once the margin has passed it
// takes a value that came beside the timer, or writes "testwait: <helper>: gave up waiting for
// <what> …" to stderr and returns the zero value and an error with the same text.
func ReceiveInHelper[T any](helper, what string, ch <-chan T) (T, error) {
	expired, stop := expiryAt(mainGiveUpAt())
	defer stop()
	value, ok := received(ch, expired)
	if !ok {
		return value, errors.New(announce(helper, what, beforeTheDeadline, nil))
	}
	return value, nil
}

// whyNoProofReaches is what Span's and Quiet's why says.
const whyNoProofReaches = "which of the test's state, synctest.Wait and a later action's effect does not reach the code, and why"

// requireWhy fails t when why says nothing: a span with no reason is a sleep. says is what
// function's why must say, for the failure to name.
func requireWhy(t testing.TB, function, why, says string) {
	t.Helper()
	if strings.TrimSpace(why) == "" {
		t.Fatalf("testwait: %s: %s needs a why: %s", t.Name(), function, says)
	}
}

// poll asks holds at the package's pace until it reports true. Once the moment GiveUpAt names has
// passed and holds still reports false, gaveUp fails t, with reading's text when the wait has one.
func poll(t testing.TB, what string, holds func() bool, reading func() string) {
	t.Helper()
	end, bounded := GiveUpAt(t)
	if !asked(end, bounded, holds) {
		gaveUp(t, what, reading)
	}
}

// asked asks holds at the package's pace until it reports true, and reports whether it did. Once
// end has passed a false answer ends the asking, so the question is asked once after end; with no
// end (bounded false) it asks until holds reports true.
func asked(end time.Time, bounded bool, holds func() bool) bool {
	for !holds() {
		if bounded && !currentClock().Now().Before(end) {
			return false
		}
		time.Sleep(pace)
	}
	return true
}

// askedBy asks holds until it reports true, and reports whether it did, for an Eventually that
// gives up at end on waits, the clock it read as it began. Once end has passed a false answer ends
// the asking, so the question is asked once after end. Between two questions it waits for the
// package's pace, or for end when that comes first (nextQuestion); it arms end's timer on waits
// only once its first question was answered false before end.
func askedBy(waits waitsClock, end time.Time, holds func() bool) bool {
	if holds() {
		return true
	}
	if !waits.Now().Before(end) {
		return false
	}
	expired, stop := waits.Timer(end)
	defer stop()
	var pacer *time.Timer
	var paced <-chan time.Time
	if _, stepped := waits.(*stepClock); !stepped {
		pacer = time.NewTimer(pace)
		defer pacer.Stop()
		paced = pacer.C
	}
	for {
		nextQuestion(paced, expired)
		if holds() {
			return true
		}
		if !waits.Now().Before(end) {
			return false
		}
		if pacer != nil {
			pacer.Reset(pace)
		}
	}
}

// nextQuestion waits for an Eventually's next question: the receive of paced, the package's pace
// after the last question, or of expired, the timer of the wait's end. On a step clock paced is
// nil, since nothing the machine does moves that clock: the wait then waits for its end's timer
// alone, which the clock fires once the wait is blocked here (waitFrames), so there an Eventually
// that never holds asks once, then once more at its end.
func nextQuestion(paced, expired <-chan time.Time) {
	select {
	case <-paced:
	case <-expired:
	}
}

// expiry is a channel that receives once the moment GiveUpAt names for t has passed, nil (which
// never receives) under -timeout 0, and the function that releases its timer.
func expiry(t testing.TB) (<-chan time.Time, func() bool) {
	return expiryAt(GiveUpAt(t))
}

// expiryAt is a channel that receives once end has passed, nil (which never receives) when there
// is no end (bounded false), and the function that releases its timer.
func expiryAt(end time.Time, bounded bool) (<-chan time.Time, func() bool) {
	if !bounded {
		return nil, func() bool { return false }
	}
	return currentClock().Timer(end)
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

// beforeTheDeadline is when a wait the margin ended gave up, as its failure says it.
var beforeTheDeadline = fmt.Sprintf("%s before the test binary's deadline", margin)

// afterTheMinute is when an Eventually its minute ended gave up, as its failure says it.
const afterTheMinute = "after Eventually's one minute"

// aSecondBeforeTheDeadline is when an Eventually the test binary's deadline ended, before its
// minute, gave up, as its failure says it.
var aSecondBeforeTheDeadline = fmt.Sprintf("%s before the test binary's deadline", eventuallySpare)

// announce is the text of a wait that gave up when (beforeTheDeadline, afterTheMinute,
// aSecondBeforeTheDeadline), in the test named name: it writes the text to stderr at once, in one
// write, and returns it. reading is the wait's last reading, or nil for a wait that has none.
func announce(name, what, when string, reading func() string) string {
	text := fmt.Sprintf("testwait: %s: gave up waiting for %s %s", name, what, when)
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
	t.Fatalf("%s", announce(t.Name(), what, beforeTheDeadline, reading))
}
