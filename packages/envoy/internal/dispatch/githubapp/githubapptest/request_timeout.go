package githubapptest

import (
	"testing"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/testwait"
)

// RequestDeadlineAtTheGiveUp is the end a test's App client gives each call it sends
// (githubapp.WithRequestDeadline(githubapptest.RequestDeadlineAtTheGiveUp(t))) where the test's
// stand-in for GitHub holds a call while the test takes its next step: a release of the hold, a
// move of a fake clock to a bound the code under test armed, a cancel, or a Close. It is the moment
// t's own waits give up (testwait.GiveUpAt), or no end under -timeout 0, so only that step, or the
// test's give-up, ends the held call. At the client's own bound, githubapp.RequestTimeout (20 s of
// the wall clock), a held call ends by itself whenever the next step comes more than 20 s after
// the hold, as it can on a loaded machine, and the code under test takes that end for GitHub's own
// failure: the row then fails on correct code, or passes on the wrong cause.
//
// It is a clock.Deadline, and not a githubapp.Option, because githubapp's own tests (package
// githubapp, client_test.go) import this package: an Option here would make this package import
// githubapp, and those tests an import cycle.
func RequestDeadlineAtTheGiveUp(t testing.TB) clock.Deadline {
	return clock.DeadlineAt(testwait.GiveUpAt(t))
}
