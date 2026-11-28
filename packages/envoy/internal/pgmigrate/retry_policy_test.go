package pgmigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A context without an explicit retry policy uses ProductionRetryPolicy.
func TestPolicyFromContextDefaultsToProductionAndRoundTrips(t *testing.T) {
	t.Parallel()
	if got := policyFromContext(context.Background()); got != ProductionRetryPolicy {
		t.Errorf("policyFromContext(Background) = %+v, want ProductionRetryPolicy %+v", got, ProductionRetryPolicy)
	}
	want := RetryPolicy{Attempts: 7, Pause: 3 * time.Second}
	if got := policyFromContext(WithRetryPolicy(context.Background(), want)); got != want {
		t.Errorf("policyFromContext(WithRetryPolicy(ctx, %+v)) = %+v, want it back", want, got)
	}
	if ProductionRetryPolicy.Attempts != LockTimeoutAttempts || ProductionRetryPolicy.Pause != LockTimeoutRetryPause {
		t.Errorf("ProductionRetryPolicy = %+v, want the constants (%d attempts, %s pause)", ProductionRetryPolicy, LockTimeoutAttempts, LockTimeoutRetryPause)
	}
}

func TestRetryLimit(t *testing.T) {
	t.Parallel()
	deadline := time.Now().Add(time.Hour)
	withDeadline, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	for _, tc := range []struct {
		name     string
		policy   RetryPolicy
		ctx      context.Context
		cap      int
		deadline time.Time
	}{
		{name: "default without a deadline", policy: ProductionRetryPolicy, ctx: context.Background(), cap: LockTimeoutAttempts},
		{name: "counted cap with a deadline", policy: RetryPolicy{Attempts: 7}, ctx: withDeadline, cap: 7},
		{name: "deadline mode", policy: DeadlineRetryPolicy(), ctx: withDeadline, deadline: deadline},
		{name: "deadline policy without a deadline", policy: DeadlineRetryPolicy(), ctx: context.Background(), cap: LockTimeoutAttempts},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap, end, err := tc.policy.retryLimit(tc.ctx)
			if cap != tc.cap || end != tc.deadline || err != nil {
				t.Errorf("retryLimit = (%d, %v, %v), want (%d, %v, nil)", cap, end, err, tc.cap, tc.deadline)
			}
		})
	}
}

// attemptsField logs an integer cap or the deadline note.
func TestAttemptsField(t *testing.T) {
	t.Parallel()
	if got := attemptsField(LockTimeoutAttempts); got != LockTimeoutAttempts {
		t.Errorf("attemptsField(%d) = %v, want the int %d", LockTimeoutAttempts, got, LockTimeoutAttempts)
	}
	if got := attemptsField(0); got != "until the context's deadline" {
		t.Errorf("attemptsField(0) = %q, want %q", got, "until the context's deadline")
	}
}

// A negative attempt cap is refused before any attempt runs.
func TestRetryLimitRefusesNegativeAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	policy := RetryPolicy{Attempts: -2}
	const want = "pgmigrate: RetryPolicy.Attempts is -2: a retry count cannot be negative"
	if cap, end, err := policy.retryLimit(ctx); cap != 0 || end != (time.Time{}) || err == nil || err.Error() != want {
		t.Errorf("retryLimit = (%d, %v, %v), want (0, zero time, %q)", cap, end, err, want)
	}
	ran := false
	err := RetryLockTimeouts(WithRetryPolicy(ctx, policy), "0001_x.up.sql",
		func(context.Context) error { ran = true; return nil })
	if err == nil || err.Error() != want {
		t.Errorf("RetryLockTimeouts with Attempts -2 = %v, want %q", err, want)
	}
	if ran {
		t.Error("RetryLockTimeouts with Attempts -2 ran an attempt")
	}
}

// A negative pause is clamped to zero.
func TestNegativePauseIsClamped(t *testing.T) {
	t.Parallel()
	if got := (RetryPolicy{Attempts: 2, Pause: -LockTimeout}).pause(); got != 0 {
		t.Errorf("pause() with Pause=-LockTimeout = %s, want 0", got)
	}
}

// The default policy returns a context or migration error unchanged, even if the context is done.
func TestProductionDefaultDoesNotWrapAContextOrMigrationError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sentinel := errors.New("a real migration error")
	if err := RetryLockTimeouts(ctx, "0001_x.up.sql",
		func(context.Context) error { return sentinel }); err != sentinel {
		t.Errorf("err = %q, want the migration's own error unwrapped under the production default", err)
	}
	if err := RetryLockTimeouts(ctx, "0001_x.up.sql",
		func(context.Context) error { return context.Canceled }); err != context.Canceled {
		t.Errorf("err = %v, want context.Canceled unchanged under the production default", err)
	}
}

// Deadline mode wraps a context error with the attempt count and returns other
// non-timeout errors unchanged.
func TestDeadlineModeWrapsAContextErrorAndPassesOthersThrough(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx = WithRetryPolicy(ctx, DeadlineRetryPolicy())
	err := RetryLockTimeouts(ctx, "0001_x.up.sql",
		func(context.Context) error { return context.DeadlineExceeded })
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "after 1 attempts") {
		t.Errorf("err = %v, want context.DeadlineExceeded wrapped with the attempt count", err)
	}

	sentinel := errors.New("a real migration error")
	if err := RetryLockTimeouts(ctx, "0001_x.up.sql",
		func(context.Context) error { return sentinel }); err != sentinel {
		t.Errorf("err = %v, want a non-timeout error on a live context back unwrapped", err)
	}
}

// Deadline mode measures each attempt and stops before the deadline when another attempt
// of the same duration would not fit.
func TestDeadlineModeReadsEachAttemptAndGivesUpNamed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const migration = "0001_x.up.sql"
		lte := &LockTimeoutError{Migration: migration}
		ctx, cancel := context.WithTimeout(context.Background(), 975*time.Millisecond)
		defer cancel()
		ctx = WithRetryPolicy(ctx, RetryPolicy{})
		attempts := 0
		err := RetryLockTimeouts(ctx, migration, func(ctx context.Context) error {
			attempts++
			if attempts == 1 {
				time.Sleep(350 * time.Millisecond)
			} else {
				time.Sleep(100 * time.Millisecond)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			return lte
		})
		if attempts != 7 {
			t.Errorf("made %d attempts, want 7 (one at 350ms and six at 100ms)", attempts)
		}
		if ctx.Err() != nil {
			t.Errorf("context ended before proactive give-up: %v", ctx.Err())
		}
		var got *LockTimeoutError
		if !errors.As(err, &got) || got != lte || errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "after 7 attempts") {
			t.Errorf("err = %v, want only the lock timeout wrapped with the attempt count", err)
		}
	})
}

// A deadline policy without a context deadline falls back to LockTimeoutAttempts.
func TestDeadlineModeWithoutDeadlineGivesUp(t *testing.T) {
	t.Parallel()
	const migration = "0001_x.up.sql"
	lte := &LockTimeoutError{Migration: migration}
	ctx := WithRetryPolicy(context.Background(), RetryPolicy{})
	attempts := 0
	err := RetryLockTimeouts(ctx, migration, func(context.Context) error { attempts++; return lte })
	if attempts != LockTimeoutAttempts {
		t.Errorf("made %d attempts on an endless context, want the counted fallback %d (not retry for ever)", attempts, LockTimeoutAttempts)
	}
	var got *LockTimeoutError
	if !errors.As(err, &got) {
		t.Errorf("err = %v, want the *LockTimeoutError named", err)
	}
}

// If the deadline cuts a later attempt without a lock read, the error retains the earlier timeout.
func TestDeadlineModeNamesTheLockWhenALaterAttemptIsCut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const migration = "0001_x.up.sql"
		lte := &LockTimeoutError{Migration: migration}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ctx = WithRetryPolicy(ctx, RetryPolicy{})
		attempts := 0
		err := RetryLockTimeouts(ctx, migration, func(ctx context.Context) error {
			attempts++
			if attempts == 1 {
				return lte
			}
			<-ctx.Done()
			return ctx.Err()
		})
		if attempts != 2 {
			t.Errorf("made %d attempts, want 2", attempts)
		}
		if !errors.Is(err, lte) || !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "after 2 attempts") {
			t.Errorf("err = %v, want the earlier timeout and context.DeadlineExceeded wrapped with the attempt count", err)
		}
	})
}

func TestDeadlineModeKeepsAnEarlierTimeoutAndAContextCutWithALockRead(t *testing.T) {
	t.Parallel()
	const migration = "0001_x.up.sql"
	wait := &LockWait{LockType: "relation", Object: "things"}
	lte := &LockTimeoutError{Migration: migration, Wait: wait, Err: errors.New("lock timeout")}
	cut := &LockTimeoutError{Migration: migration, Wait: wait, Err: context.DeadlineExceeded}
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx = WithRetryPolicy(ctx, RetryPolicy{})
	attempts := 0
	err := RetryLockTimeouts(ctx, migration, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return lte
		}
		if attempts > 2 {
			t.Fatal("retried after a context-cut error")
		}
		return cut
	})
	if attempts != 2 {
		t.Errorf("made %d attempts, want 2", attempts)
	}
	if !errors.Is(err, lte) || !errors.Is(err, cut) || !errors.Is(err, context.DeadlineExceeded) ||
		!strings.Contains(err.Error(), "after 2 attempts") {
		t.Errorf("err = %v, want both lock errors and context.DeadlineExceeded with the attempt count", err)
	}
}

func TestProductionDefaultDoesNotRetryAContextCutWithALockRead(t *testing.T) {
	t.Parallel()
	const migration = "0001_x.up.sql"
	cut := &LockTimeoutError{
		Migration: migration,
		Wait:      &LockWait{LockType: "relation", Object: "things"},
		Err:       context.Canceled,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	err := RetryLockTimeouts(ctx, migration, func(context.Context) error {
		attempts++
		cancel()
		return cut
	})
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1", attempts)
	}
	if err != cut {
		t.Errorf("err = %v, want the original context-cut error %v unchanged", err, cut)
	}
}

// The default policy returns a context error unchanged even after a lock timeout.
func TestProductionDefaultDoesNotWrapAfterATimeout(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const migration = "0001_x.up.sql"
		lte := &LockTimeoutError{Migration: migration}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		attempts := 0
		err := RetryLockTimeouts(ctx, migration, func(context.Context) error {
			attempts++
			if attempts == 1 {
				return lte
			}
			cancel()
			return context.Canceled
		})
		if attempts != 2 {
			t.Errorf("made %d attempts, want 2", attempts)
		}
		if err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled unchanged after a lock timeout", err)
		}
	})
}
