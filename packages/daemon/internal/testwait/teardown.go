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

// TeardownDiagnosticBound is held back from a direct test cleanup so Drop can read the backends
// that still hold its database before go test's alarm ends the binary.
const TeardownDiagnosticBound = time.Second

type teardownDiagnosticDeadlineKey struct{}

// TeardownDeadline is the deadline for work a test binary runs after m.Run returns. With
// -test.timeout it is that binary's deadline plus TeardownGrace. With -test.timeout 0 it is
// teardownUnbounded from this call.
func TeardownDeadline() time.Time {
	if deadline, bounded := flagDeadline(); bounded {
		return deadline.Add(TeardownGrace)
	}
	return time.Now().Add(teardownUnbounded)
}

// TeardownContext is the bound for a test cleanup that drops its database. It leaves at most one
// second, or a quarter of a short binary's remaining time, for Drop's backend diagnosis, then
// holds a final quarter of that reserve for the cleanup to report it. The caller cancels it after
// the cleanup; no t.Cleanup here can cancel it before that cleanup runs.
func TeardownContext(t testing.TB) (context.Context, context.CancelFunc) {
	deadline, bounded := teardownDeadlineOf(t)
	if !bounded {
		return context.WithTimeout(context.Background(), teardownUnbounded)
	}
	reserve := min(TeardownDiagnosticBound, max(time.Until(deadline), 0)/4)
	root := context.WithValue(context.Background(), teardownDiagnosticDeadlineKey{}, deadline.Add(-reserve/4))
	return context.WithDeadline(root, deadline.Add(-reserve))
}

// TeardownDiagnosticDeadline is the deadline TeardownContext keeps for a cleanup's backend read,
// leaving time after that read for the cleanup to report its error. The returned false means the
// context did not come from a bounded test cleanup.
func TeardownDiagnosticDeadline(ctx context.Context) (time.Time, bool) {
	deadline, ok := ctx.Value(teardownDiagnosticDeadlineKey{}).(time.Time)
	return deadline, ok
}

func teardownDeadlineOf(t testing.TB) (time.Time, bool) {
	if named, ok := t.(interface{ Deadline() (time.Time, bool) }); ok {
		return named.Deadline()
	}
	return flagDeadline()
}
