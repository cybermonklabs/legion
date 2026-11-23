package testwait

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// A product's fixed bound on a call (a record's wait in the CI store, a code flow's read of its
// issuer) is for a remote that accepts the call and never answers. A test's own remote answers in
// the machine's time, which on a loaded machine can be longer, and a test of what the answer does
// then reads the bound's timeout as the answer. Such a test gives the product a seam and arms it
// with the bound's end (BoundEnd) in place of the bound.

// BoundEndAt is when a wait that stands in for a product's bound, begun at now, gives up in a test
// binary whose deadline is deadline: margin before the deadline, so the failing test has margin to
// run its cleanups in and its line reaches stderr before Go's alarm; but never sooner than bound
// after now, so the wait has at least the time the product gave it, and never later than the
// deadline. Under a short -timeout it is therefore bound after now, or the deadline itself when
// less than bound is left.
func BoundEndAt(deadline, now time.Time, bound time.Duration) time.Time {
	end := deadline.Add(-margin)
	if floor := now.Add(bound); end.Before(floor) {
		end = floor
	}
	if end.After(deadline) {
		end = deadline
	}
	return end
}

// BoundEnd is BoundEndAt of t's deadline at now, and false under -timeout 0 and for a benchmark,
// which have no deadline: only the answer the wait waits for ends it then.
func BoundEnd(t testing.TB, bound time.Duration) (time.Time, bool) {
	timed, ok := t.(interface{ Deadline() (time.Time, bool) })
	if !ok {
		return time.Time{}, false
	}
	deadline, bounded := timed.Deadline()
	if !bounded {
		return time.Time{}, false
	}
	return BoundEndAt(deadline, time.Now(), bound), true
}

// WithBoundEnd is ctx bounded at BoundEnd(t, bound), read now, or bounded by nothing where t has
// no deadline.
func WithBoundEnd(ctx context.Context, t testing.TB, bound time.Duration) (context.Context, context.CancelFunc) {
	if end, bounded := BoundEnd(t, bound); bounded {
		return context.WithDeadline(ctx, end)
	}
	return context.WithCancel(ctx)
}

// BoundArm is the arm a product that takes one in place of its fixed bounds is handed in t
// (cistore.WithWaitArm, oidc.WithWaitArm): given the caller's context and the product's bound, it
// returns the context bounded at BoundEnd(t, bound), read when the wait begins (WithBoundEnd).
func BoundArm(t testing.TB) func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
	return func(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
		return WithBoundEnd(ctx, t, bound)
	}
}

// UntilBoundEnd waits until read reports true, asking every Poll, and returns what it read then.
// It gives up at BoundEnd(t, bound), read when it begins, in place of a fixed bound the wait had
// before; giving up, it writes its line to stderr at once and fails t with the same text, as Until
// does, so the line is kept whatever follows.
func UntilBoundEnd[T any](t testing.TB, bound time.Duration, what string, read func() (T, bool)) T {
	t.Helper()
	var value T
	end, bounded := BoundEnd(t, bound)
	if asked(end, bounded, Poll, func() bool {
		var ok bool
		value, ok = read()
		return ok
	}) {
		return value
	}
	text := oneLine.Replace(fmt.Sprintf("testwait: %s: gave up waiting for %s at %s, the end of its bound; last read: %+v",
		t.Name(), what, end.UTC().Format(time.RFC3339Nano), value))
	_, _ = os.Stderr.WriteString(text + "\n")
	t.Fatalf("%s", text)
	return value
}
