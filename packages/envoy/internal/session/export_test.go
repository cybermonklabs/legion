package session

import (
	"context"
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

// withKnownSessionAttemptContext bounds each attempt of the background repair by the context bound
// gives it, in place of repairAttemptWindow (retryRepair): a test ends an attempt's window itself,
// so its row counts attempts rather than reading a clock. It is test-only.
func withKnownSessionAttemptContext(bound func(context.Context) (context.Context, context.CancelFunc)) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.attemptBound = bound }
}

// withKnownSessionOpenWait sets the wait of each retry attempt's request for the bucket in place of
// knownBucketOpenWait (knownJetStream), so a test can hold a lookup the server leaves unanswered
// past the test's own end. The store's first attempt keeps knownBucketOpenWait, so a store opened
// with this option still opens. It is test-only.
func withKnownSessionOpenWait(d time.Duration) KnownSessionsOption {
	return func(o *knownSessionsOpts) { o.openWait = d }
}

// withKnownSessionWatch adds options to the ones the store's watcher is built with: a clock that
// moves past the stale bound (kvwatch.WithNow), say. It is test-only: no deployment sets them.
func withKnownSessionWatch(options ...kvwatch.Option) KnownSessionsOption {
	return func(o *knownSessionsOpts) {
		o.watch = append(o.watch, options...)
	}
}
