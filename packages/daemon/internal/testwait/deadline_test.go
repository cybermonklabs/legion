package testwait

import (
	"context"
	"errors"
	"flag"
	"testing"
	"time"
)

// A StandInDeadline ends as the context.WithTimeout or context.WithTimeoutCause it stands in for
// ends, for every end it can have: its deadline passing, its cancel, and its parent's end. Each row
// ends a real one and a stand-in the same way, then reads both, and a context derived from each
// with context.WithTimeoutCause, as a command's own limit derives from an attempt's budget, through
// Err and context.Cause: the two reads code tells one end from another by.
func TestAStandInDeadlineEndsAsTheDeadlineItStandsInFor(t *testing.T) {
	type key struct{}
	errOwn, errChild, errParent := errors.New("its own cause"), errors.New("the child's own limit"), errors.New("the parent's cause")
	for _, row := range []struct {
		name string
		// cause is the deadline's cause (nil: context.WithTimeout's), and passes whether its
		// deadline passes: the real one's is then a nanosecond, and an hour otherwise, which the
		// row's own end comes before.
		cause  error
		passes bool
		// real ends the real deadline as the row says, given its cancel and its parent's; stand
		// ends the stand-in the same way.
		real  func(cancel context.CancelFunc, cancelParent context.CancelCauseFunc)
		stand func(pass func(error), cancel context.CancelFunc, cancelParent context.CancelCauseFunc)
	}{
		{name: "its deadline passes, with a cause", cause: errOwn, passes: true,
			real:  func(context.CancelFunc, context.CancelCauseFunc) {},
			stand: func(pass func(error), _ context.CancelFunc, _ context.CancelCauseFunc) { pass(errOwn) }},
		{name: "its deadline passes, with no cause", passes: true,
			real:  func(context.CancelFunc, context.CancelCauseFunc) {},
			stand: func(pass func(error), _ context.CancelFunc, _ context.CancelCauseFunc) { pass(nil) }},
		{name: "its deadline passes, then its cancel", cause: errOwn, passes: true,
			real:  func(context.CancelFunc, context.CancelCauseFunc) {},
			stand: func(pass func(error), cancel context.CancelFunc, _ context.CancelCauseFunc) { pass(errOwn); cancel() }},
		{name: "its cancel", cause: errOwn,
			real:  func(cancel context.CancelFunc, _ context.CancelCauseFunc) { cancel() },
			stand: func(_ func(error), cancel context.CancelFunc, _ context.CancelCauseFunc) { cancel() }},
		{name: "its parent ends first", cause: errOwn,
			real: func(_ context.CancelFunc, cancelParent context.CancelCauseFunc) { cancelParent(errParent) },
			stand: func(_ func(error), _ context.CancelFunc, cancelParent context.CancelCauseFunc) {
				cancelParent(errParent)
			}},
	} {
		t.Run(row.name, func(t *testing.T) {
			length := time.Hour
			if row.passes {
				length = time.Nanosecond
			}
			realParent, cancelRealParent := context.WithCancelCause(context.WithValue(context.Background(), key{}, "carried"))
			defer cancelRealParent(nil)
			var real context.Context
			var cancelReal context.CancelFunc
			if row.cause == nil {
				real, cancelReal = context.WithTimeout(realParent, length)
			} else {
				real, cancelReal = context.WithTimeoutCause(realParent, length, row.cause)
			}
			defer cancelReal()
			realChild, cancelRealChild := context.WithTimeoutCause(real, time.Hour, errChild)
			defer cancelRealChild()
			row.real(cancelReal, cancelRealParent)
			Receive(t, "the real deadline's end", real.Done())
			Receive(t, "the end of the real deadline's child", realChild.Done())

			standParent, cancelStandParent := context.WithCancelCause(context.WithValue(context.Background(), key{}, "carried"))
			defer cancelStandParent(nil)
			stand, pass, cancel := StandInDeadline(standParent)
			defer cancel()
			standChild, cancelStandChild := context.WithTimeoutCause(stand, time.Hour, errChild)
			defer cancelStandChild()
			if stand.Err() != nil || context.Cause(stand) != nil {
				t.Fatalf("the stand-in before its end: Err %v, cause %v; want neither", stand.Err(), context.Cause(stand))
			}
			before := time.Now()
			row.stand(pass, cancel, cancelStandParent)
			after := time.Now()
			Receive(t, "the end of the stand-in's child", standChild.Done())
			Receive(t, "the stand-in's end", stand.Done())
			if row.passes {
				if at, ok := stand.Deadline(); !ok || at.Before(before) || at.After(after) {
					t.Errorf("the stand-in's deadline once it passed = %s (one set: %t), want the moment it passed, between %s and %s",
						at.Format(time.RFC3339Nano), ok, before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
				}
			}

			for _, read := range []struct {
				what       string
				real, gave error
			}{
				{"Err", real.Err(), stand.Err()},
				{"context.Cause", context.Cause(real), context.Cause(stand)},
				{"the child's Err", realChild.Err(), standChild.Err()},
				{"the child's context.Cause", context.Cause(realChild), context.Cause(standChild)},
			} {
				if read.gave != read.real {
					t.Errorf("%s of the stand-in = %v, want %v, as the real deadline gives", read.what, read.gave, read.real)
				}
			}
			if got := standChild.Value(key{}); got != "carried" {
				t.Errorf("a value of the stand-in's parent, read through its child = %v, want %q", got, "carried")
			}
		})
	}
}

// A StandInDeadline reads its parent's end at once, as a context the standard library derives
// from that parent does: made under a parent that has ended, or read right after its parent ended,
// it is ended, with the parent's Err and cause, before any goroutine has run; and a pass that comes
// after its parent ended changes nothing. So a command started under it after the parent's end
// never starts, as under a real deadline's child.
func TestAStandInDeadlineReadsItsParentsEndAtOnce(t *testing.T) {
	errParent := errors.New("the parent's cause")
	for _, row := range []struct {
		name        string
		madeAfter   bool // the stand-in is made after its parent ended, else before
		passedAfter bool // the stand-in's pass comes after its parent ended, before any read
	}{
		{name: "made under a parent that has ended", madeAfter: true},
		{name: "read right after its parent ended"},
		{name: "passed after its parent ended", passedAfter: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			parent, cancelParent := context.WithCancelCause(context.Background())
			defer cancelParent(nil)
			if row.madeAfter {
				cancelParent(errParent)
			}
			real, cancelReal := context.WithTimeoutCause(parent, time.Hour, errors.New("its own cause"))
			defer cancelReal()
			stand, pass, cancel := StandInDeadline(parent)
			defer cancel()
			cancelParent(errParent)
			if row.passedAfter {
				pass(nil)
			}
			select {
			case <-stand.Done():
			default:
				t.Fatalf("the stand-in's Done is open right after its parent ended; a real deadline's child's is closed: %v", real.Err())
			}
			if stand.Err() != real.Err() || context.Cause(stand) != context.Cause(real) {
				t.Errorf("the stand-in reads Err %v and cause %v right after its parent ended, want %v and %v, as the real deadline's child", stand.Err(), context.Cause(stand), real.Err(), context.Cause(real))
			}
		})
	}
}

// A StandInDeadline that nothing reads still ends when its parent ends: a waiter that took its
// Done before the parent ended, such as the goroutine the standard library starts to end a context
// derived from it, is woken with no read of the stand-in after the end. The wait below reads the
// channel alone, never the stand-in, since every read of the stand-in looks at its parent first and
// would end it on its own. A stand-in that learns the end closes the channel at once, so the wait
// is Eventually's, which gives up testwait's margin, 30 s, before the test binary's deadline: under
// go's default -timeout a stand-in that does not learn the end fails here by name. Under a -timeout
// of 30 s or less the margin has passed before the wait begins, and the row fails at once.
func TestAStandInDeadlineNothingReadsEndsWithItsParent(t *testing.T) {
	errParent := errors.New("the parent's cause")
	parent, cancelParent := context.WithCancelCause(context.Background())
	defer cancelParent(nil)
	stand, _, cancel := StandInDeadline(parent)
	defer cancel()
	done := stand.Done()
	cancelParent(errParent)
	Eventually(t, "the end of a stand-in nothing read, after its parent ended", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	if stand.Err() != context.Canceled || context.Cause(stand) != errParent {
		t.Errorf("the stand-in reads Err %v and cause %v, want %v and %v, its parent's", stand.Err(), context.Cause(stand), context.Canceled, errParent)
	}
}

// BinaryDeadline is go test's -timeout reckoned from the call: read just before the call, now
// plus the timeout is no later than its answer, and read just after it no earlier. Under
// -timeout 0 it answers no deadline. The reads and the answer come from one monotonic clock, so
// the bounds need no tolerance.
func TestBinaryDeadlineIsTheTimeoutReckonedFromTheCall(t *testing.T) {
	timeout := flag.Lookup("test.timeout").Value.(flag.Getter).Get().(time.Duration)
	before := time.Now()
	got, ok := BinaryDeadline()
	after := time.Now()
	if timeout <= 0 {
		if ok {
			t.Fatalf("under -timeout %s BinaryDeadline = %s, want none", timeout, got.Format(time.RFC3339Nano))
		}
		return
	}
	if !ok {
		t.Fatalf("under -timeout %s BinaryDeadline answered none", timeout)
	}
	if least, most := before.Add(timeout), after.Add(timeout); got.Before(least) || got.After(most) {
		t.Fatalf("BinaryDeadline = %s, want between %s and %s: -timeout %s from the call",
			got.Format(time.RFC3339Nano), least.Format(time.RFC3339Nano), most.Format(time.RFC3339Nano), timeout)
	}
}
