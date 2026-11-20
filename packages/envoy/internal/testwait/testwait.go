// Package testwait holds the bounded wait Envoy's tests share. A test that drives real
// processes, a real database or a real socket waits for something it cannot make happen on its
// own — a delivery, the transaction that answers it — and the wait is bounded by the test
// binary's own deadline rather than a fixed span: on a loaded machine a step that is merely slow
// is not a failure, and a condition that never holds still fails, naming what it waited for.
// Eventually is the wait a test usually wants; WithinPolling takes a bound and a pace of its
// own; Holds answers rather than fails, for a caller whose failure text is its own; and Bounded
// is the clamp alone, for a wait that holds the rest itself. FailureOf and FailureWithin run a
// wait, or any helper, that fails its test through Fatal and answer what it failed with: a test
// of a helper's refusal reads the refusal's own words.
package testwait

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// Poll is how often a condition is asked. It is short enough that a test which passes in
// milliseconds still does, and long enough that a waiting test costs a loaded machine little.
const Poll = 10 * time.Millisecond

// Eventually waits for condition, failing the test with what it waited for when the deadline
// passes. The deadline is a minute, or the test binary's own less a second when that is sooner,
// so a `-timeout` smaller than a minute still reports the wait rather than the binary's panic.
func Eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	WithinPolling(t, Bounded(t, time.Minute, time.Second), Poll, what, condition)
}

// Bounded is bound, or what is left of the test binary's own deadline less margin when that is
// sooner, so a -timeout smaller than bound still reports the wait rather than the binary's panic.
// Eventually is Bounded of a minute and a second; a wait with a span or a margin of its own takes
// those.
func Bounded(t *testing.T, bound, margin time.Duration) time.Duration {
	if testDeadline, ok := t.Deadline(); ok {
		if left := time.Until(testDeadline) - margin; left < bound {
			bound = left
		}
	}
	return bound
}

// WithinPolling waits for condition under the caller's own bound and its own pace, with no
// clamp to the test binary's deadline, failing the test with what it waited for when the bound
// passes: a wait whose span is the thing under test, or one that must outlast a minute, says
// how long it means to wait.
func WithinPolling(t *testing.T, bound, poll time.Duration, what string, condition func() bool) {
	t.Helper()
	if waitUntil(time.Now().Add(bound), poll, condition) {
		return
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Holds reports whether condition held before bound passed, and fails nothing itself: a caller
// that keeps a failure text of its own, prints what it read when it gave up, or carries on
// either way takes the answer rather than the test. The caller gives the pace as well, because
// a question that costs something — a database read, a walk of /proc — or that is itself an
// attempt must not be asked a hundred times a second; Poll is this package's own.
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

// FailureOf runs subject with a recorder in place of t, on a goroutine of its own, and answers
// what subject failed with through Fatal or Fatalf, or "" when it returned. The recorder ends
// subject's goroutine at the failure, as t.Fatal ends the test's, so nothing after the failure
// runs; every other method of t, Errorf and Logf among them, reaches t itself. FailureOf waits for
// subject however long it runs, so subject bounds its own wait.
func FailureOf(t testing.TB, subject func(t testing.TB)) string {
	t.Helper()
	recorded, ended := startRecorded(t, subject)
	<-ended
	return recorded.message
}

// FailureWithin is FailureOf for a subject with no bound of its own: when subject has not ended
// within bound, the test fails, saying that what still runs.
func FailureWithin(t testing.TB, bound time.Duration, what string, subject func(t testing.TB)) string {
	t.Helper()
	recorded, ended := startRecorded(t, subject)
	select {
	case <-ended:
	case <-time.After(bound):
		t.Fatalf("%s still runs after %s", what, bound)
	}
	return recorded.message
}

// fatalRecorder is t with Fatal and Fatalf recorded and the goroutine ended, as t.Fatal does.
type fatalRecorder struct {
	testing.TB
	message string
}

func (f *fatalRecorder) Fatal(args ...any) {
	f.message = fmt.Sprint(args...)
	runtime.Goexit()
}

func (f *fatalRecorder) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// startRecorded runs subject with a fatalRecorder over t on a goroutine of its own, and answers
// the recorder and a channel closed once subject has ended, by its return or by its Fatal: the
// close is deferred, so the recorder's runtime.Goexit runs it too.
func startRecorded(t testing.TB, subject func(t testing.TB)) (*fatalRecorder, <-chan struct{}) {
	recorded := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		subject(recorded)
	}()
	return recorded, ended
}
