package clock

import (
	"context"
	"fmt"
	"time"
)

// Deadline is the end a test gives one of the product's own bounds in place of the duration a
// deployment arms: the moment the test's own waits give up (testwait.GiveUpAt), so a bound armed
// on a call or a step the test waits on ends no sooner than the test would give up on it, or no
// end at all for a test binary run under -timeout 0, whose waits have none. Its zero value is
// production's: every bound that takes one arms its own duration then. A moment is the wall
// clock's, as the test's give-up is. DeadlineAt makes one; String and ParseDeadline are the form a
// test hands a Dispatch process (cmd/dispatch's DISPATCH_TEST_*_DEADLINE settings).
type Deadline struct {
	at        time.Time
	unbounded bool
}

// deadlineNone is Deadline's written form for no end at all.
const deadlineNone = "none"

// DeadlineAt is the Deadline at end, or no end at all when bounded is false: what
// testwait.GiveUpAt answers, under -timeout 0 with bounded false. A bounded end is a moment; the
// zero time, which would read as production's duration, panics.
func DeadlineAt(end time.Time, bounded bool) Deadline {
	if !bounded {
		return Deadline{unbounded: true}
	}
	if end.IsZero() {
		panic("clock: DeadlineAt with a bounded end needs a moment, not the zero time")
	}
	return Deadline{at: end}
}

// ParseDeadline reads String's form back: "none", or a moment in RFC 3339. Anything else, the
// empty string among it, is an error, since only a test sets a deadline and it always sets one of
// the two.
func ParseDeadline(value string) (Deadline, error) {
	if value == deadlineNone {
		return Deadline{unbounded: true}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || at.IsZero() {
		return Deadline{}, fmt.Errorf("a test deadline is an RFC 3339 moment or %q, not %q", deadlineNone, value)
	}
	return Deadline{at: at}, nil
}

// String is the deadline's written form: its moment in RFC 3339 with nanoseconds, "none" for no
// end, and "" for the zero value, production's.
func (d Deadline) String() string {
	switch {
	case d.unbounded:
		return deadlineNone
	case d.at.IsZero():
		return ""
	}
	return d.at.Format(time.RFC3339Nano)
}

// IsZero reports whether d is the zero value, which leaves each bound production's duration.
func (d Deadline) IsZero() bool { return !d.unbounded && d.at.IsZero() }

// Bounded is the part of a Clock a Deadline arms its bound on: Real, a clocktest.Fake and every
// package's narrowing of Clock that keeps Now and WithTimeout fit it.
type Bounded interface {
	Now() time.Time
	WithTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc)
}

// WithTimeout is ctx under the bound d gives in place of production, armed on c: production for
// the zero value, the span from c's present to d's moment for a moment, and ctx with only a cancel
// of its own for no end.
func (d Deadline) WithTimeout(c Bounded, ctx context.Context, production time.Duration) (context.Context, context.CancelFunc) {
	if d.unbounded {
		return context.WithCancel(ctx)
	}
	span, _ := d.Within(c, production)
	return c.WithTimeout(ctx, span)
}

// Within is the span a timer armed now on c waits under d: production for the zero value, the span
// from c's present to d's moment for a moment, and false for no end, when nothing is to be armed.
func (d Deadline) Within(c interface{ Now() time.Time }, production time.Duration) (time.Duration, bool) {
	switch {
	case d.unbounded:
		return 0, false
	case !d.at.IsZero():
		return d.at.Sub(c.Now()), true
	}
	return production, true
}
