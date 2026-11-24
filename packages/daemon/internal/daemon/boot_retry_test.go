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
// timeout included — is logged at ERROR, as the line the caller names with the caller's own
// attributes, and applied again, never skipped in silence; only the daemon's stop ends it
// unapplied, and that stop logs no error.
func TestABootFactThatFailsWhileServingIsRetriedAndOnlyTheStopEndsIt(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	calls := 0
	retryUntilApplied(context.Background(), log, "the fact could not be applied", time.Millisecond, time.Millisecond, func(context.Context) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}, "claim", "legion-legion-legion-208-tester")
	if calls != 2 || strings.Count(logged.String(), "level=ERROR") != 1 || !strings.Contains(logged.String(), "deadline exceeded") ||
		!strings.Contains(logged.String(), `msg="the fact could not be applied"`) || !strings.Contains(logged.String(), "claim=legion-legion-legion-208-tester attempt=1") {
		t.Fatalf("applied %d times, log %q; want the timeout logged at ERROR once, as the caller's line with its attributes, and applied again", calls, logged.String())
	}

	logged.Reset()
	ctx, stop := context.WithCancel(context.Background())
	calls = 0
	retryUntilApplied(ctx, log, "the fact could not be applied", time.Millisecond, time.Millisecond, func(context.Context) error {
		calls++
		stop()
		return context.Canceled
	})
	if calls != 1 || logged.Len() != 0 {
		t.Fatalf("after the stop: applied %d times, log %q; want one attempt and no error", calls, logged.String())
	}
}
