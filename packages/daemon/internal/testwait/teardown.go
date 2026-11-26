package testwait

import (
	"context"
	"testing"
	"time"
)

// TeardownGrace is the time after a test binary's -timeout that its post-test teardown may use.
// Go stops the binary's own alarm once m.Run returns, and cmd/go then allows one further minute
// before it kills the process. A teardown that waits for a database backend therefore ends inside
// that minute rather than on a fixed retry window that a loaded machine can spend.
const TeardownGrace = 30 * time.Second

// teardownUnbounded is the bound TeardownDeadline gives a binary run with -timeout 0. Such a
// binary has no cmd/go alarm to stay ahead of, but test cleanup still reports a backend that never
// leaves instead of waiting without a bound.
const teardownUnbounded = 5 * time.Minute

// TeardownDeadline is the deadline for work a test binary runs after m.Run returns. With
// -test.timeout it is that binary's deadline plus TeardownGrace. With -test.timeout 0 it is
// teardownUnbounded from this call.
func TeardownDeadline() time.Time {
	if deadline, bounded := flagDeadline(); bounded {
		return deadline.Add(TeardownGrace)
	}
	return time.Now().Add(teardownUnbounded)
}

// TeardownContext bounds a test cleanup that drops its database. It is taken inside the cleanup,
// so its reserve starts when the cleanup runs, not when the deadline nears: a run under a short
// -timeout still gives the drop its remaining time less the reserve. The reserve leaves the drop's
// last holder read and the cleanup's own error time before go test's alarm. Under -timeout 0 the
// bound is teardownUnbounded from the call, so a backend that never exits fails by name rather
// than retrying for ever. The caller cancels it after the cleanup.
func TeardownContext(t testing.TB) (context.Context, context.CancelFunc) {
	deadline, bounded := teardownDeadlineOf(t)
	if !bounded {
		return context.WithTimeout(context.Background(), teardownUnbounded)
	}
	reserve := min(TeardownGrace, max(time.Until(deadline), 0)/4)
	return context.WithDeadline(context.Background(), deadline.Add(-reserve))
}

// teardownDeadlineOf is the test binary's deadline: a *testing.T names its own; another testing.TB
// (a benchmark) has the -timeout flag's.
func teardownDeadlineOf(t testing.TB) (time.Time, bool) {
	if named, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		return named.Deadline()
	}
	return flagDeadline()
}
