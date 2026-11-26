package testwait

import "time"

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
