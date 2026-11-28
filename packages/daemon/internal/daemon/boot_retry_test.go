package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// The boot fact runs beside the serving API, and the boot's reads and the signal deadline's
// firings retry by the same loop (retryUntilApplied). An attempt that fails while the daemon still
// serves — a timeout included — is told to the caller's retrying, once, with its attempt and its
// error, and applied again; the caller writes its own line of it (runBooted's ERROR line). Only the
// daemon's stop ends the loop unapplied, and a failure at the stop is told nothing. The test
// records each attempt and error that retrying receives, so it reads what the loop tells and not a
// line of its own.
func TestABootFactThatFailsWhileServingIsRetriedAndOnlyTheStopEndsIt(t *testing.T) {
	t.Parallel()
	type told struct {
		attempt int
		err     error
	}
	var retried []told
	retrying := func(attempt int, _ time.Duration, err error) {
		retried = append(retried, told{attempt: attempt, err: err})
	}

	calls := 0
	retryUntilApplied(context.Background(), time.Millisecond, time.Millisecond, func(_ context.Context, attempt int) error {
		calls++
		if attempt == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}, retrying)
	if want := []told{{attempt: 1, err: context.DeadlineExceeded}}; calls != 2 || !reflect.DeepEqual(retried, want) {
		t.Fatalf("applied %d times, retrying told %+v; want the timeout of attempt 1 told once and applied again", calls, retried)
	}

	retried, calls = nil, 0
	refused := errors.New("the database refused the boot")
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	retryUntilApplied(ctx, time.Millisecond, time.Millisecond, func(_ context.Context, attempt int) error {
		calls++
		if attempt == 1 {
			return refused
		}
		stop()
		return context.Canceled
	}, retrying)
	if want := []told{{attempt: 1, err: refused}}; calls != 2 || !reflect.DeepEqual(retried, want) {
		t.Fatalf("applied %d times, retrying told %+v; want the failure of attempt 1 told once, and the attempt the stop ended told nothing", calls, retried)
	}
}
