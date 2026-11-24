// Package notificationstream holds what the listener's notification stream, ENVOY_NOTIFICATIONS,
// keeps, for a caller outside this module: how long it keeps a message (MaxAge).
//
// It exists because the Legion daemon (packages/daemon, module github.com/sjawhar/legion/daemon)
// needs the stream's MaxAge for its intake's early give-up and may not read it from the stream: the
// stream's info (STREAM.INFO) shows every project's subjects, which a daemon whose grant holds it to
// its projects (natsgrant.WithinProjects) is not granted. So the value reaches the daemon as its
// configuration (`notification_stream_max_age_hours`), which a deployment renders from this
// constant, and Go's internal rule bars the daemon from github.com/sjawhar/envoy/internal/contracts,
// where the number is generated from packages/contracts. This package is the one public door onto
// it.
//
// The stream's owner, internal/bus, sets the stream's MaxAge from the same generated constant
// (streamCfg, streamDuplicateWindow), and a test of that package holds the two equal. A rendered
// value is a deployment agreement, not a reading: a stream an operator changed, or one a release of
// another MaxAge made, is not what this constant says.
package notificationstream

import "github.com/sjawhar/envoy/internal/contracts"

// MaxAge is how long ENVOY_NOTIFICATIONS keeps a message: the generated
// contracts.DeliveryDuplicateWindow, which internal/bus sets as the stream's MaxAge.
const MaxAge = contracts.DeliveryDuplicateWindow
