package session

import "github.com/sjawhar/envoy/internal/clock"

// SetupNATS is setupNATS for the package's external tests, so they connect to the package's shared
// test NATS in an account of their own.
var SetupNATS = setupNATS

// withKnownSessionTimers arms the waits between the background repair's attempts
// (knownBucketRetryFirst, knownBucketRetryMax) on timers, a clocktest.Fake the test moves, so a
// test makes the attempt that repairs the store when it chooses rather than waiting out the
// production schedule. It is test-only: every deployment runs the repair on the real clock.
func withKnownSessionTimers(timers clock.Clock) KnownSessionsOption {
	return func(o *knownSessionsOpts) {
		o.timers = timers
	}
}
