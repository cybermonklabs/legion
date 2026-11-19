package session

import (
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
)

// SetupNATS is setupNATS for the package's external tests, so they connect to the package's shared
// test NATS in an account of their own.
var SetupNATS = setupNATS

// withKnownSessionRetry shortens the waits between the background repair's attempts, so a test
// meets the attempt that repairs the store without waiting out the production schedule
// (knownBucketRetryFirst, knownBucketRetryMax). It is test-only: no deployment changes them.
func withKnownSessionRetry(first, longest time.Duration) KnownSessionsOption {
	return func(o *knownSessionsOpts) {
		o.retryFirst, o.retryMax = first, longest
	}
}

// withKnownSessionWatch adds options to the ones the store's watcher is built with: a clock that
// moves past the stale bound (kvwatch.WithNow), say. It is test-only: no deployment sets them.
func withKnownSessionWatch(options ...kvwatch.Option) KnownSessionsOption {
	return func(o *knownSessionsOpts) {
		o.watch = append(o.watch, options...)
	}
}
