package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// The boot fact runs beside the serving API. One that fails while the daemon still serves — a
// timeout included — is logged at ERROR and applied again, never skipped in silence; only the
// daemon's stop ends it unapplied, and that stop logs no error.
func TestABootFactThatFailsWhileServingIsRetriedAndOnlyTheStopEndsIt(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	calls := 0
	retrying := func(attempt int, wait time.Duration, err error) {
		log.Error("legion daemon could not apply its boot; retrying", "attempt", attempt, "retryIn", wait, "error", err)
	}
	retryUntilApplied(context.Background(), time.Millisecond, time.Millisecond, func(context.Context, int) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}, retrying)
	if calls != 2 || strings.Count(logged.String(), "level=ERROR") != 1 || !strings.Contains(logged.String(), "deadline exceeded") {
		t.Fatalf("applied %d times, log %q; want the timeout logged at ERROR once and applied again", calls, logged.String())
	}

	logged.Reset()
	ctx, stop := context.WithCancel(context.Background())
	calls = 0
	retryUntilApplied(ctx, time.Millisecond, time.Millisecond, func(context.Context, int) error {
		calls++
		stop()
		return context.Canceled
	}, retrying)
	if calls != 1 || logged.Len() != 0 {
		t.Fatalf("after the stop: applied %d times, log %q; want one attempt and no error", calls, logged.String())
	}
}
