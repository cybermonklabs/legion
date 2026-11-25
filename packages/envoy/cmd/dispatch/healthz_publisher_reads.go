package main

import (
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/outbox"
)

// publisherReadLine is the line stampedPublisher logs for each read. Rows wait on its words
// (cmd/dispatch/multiprocess_test.go's publisherReadsOf), so a change to them is a change to
// those rows too.
const publisherReadLine = "dispatch: /healthz read the outbox publisher"

// stampedPublisher is a test process's outbox publisher as /healthz reads it under
// DISPATCH_TEST_HEALTHZ_PUBLISHER_READS=1: each read of Publishing logs, on log, what it answered
// and two moments of clock, read_from taken before the read and read_by after it, so the instant
// the answer describes is inside them. clock is the one the outbox's publisher hold is timed on
// in this process - the real clock, since main gives the outbox no clock of its own - and log is
// the process's logger, so the two moments are on the clock of the `started`, `stopped` and
// `stopped_by` the outbox's took, lost and released lines carry, in the same output, and are
// written as those are, as RFC3339Nano strings.
//
// A row judges each read by those moments alone, against those lines' moments: never by where
// the line falls in the output, since this line is written after the read and the outbox's own
// lines are written after the stores they report, so either can come out on the other side of
// the instant it is about. It is the read's own record because the answer /healthz serves is the
// value this read returned. Unset, the handler reads the publisher itself and logs nothing.
type stampedPublisher struct {
	outbox *outbox.Outbox
	clock  clock.Clock
	log    *slog.Logger
}

func (p stampedPublisher) Publishing() bool {
	from := p.clock.Now()
	publishing := p.outbox.Publishing()
	by := p.clock.Now()
	p.log.Info(publisherReadLine, "publishing", publishing,
		"read_from", from.Format(time.RFC3339Nano), "read_by", by.Format(time.RFC3339Nano))
	return publishing
}
