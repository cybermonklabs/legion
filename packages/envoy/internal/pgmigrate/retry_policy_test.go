package pgmigrate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A context carries no policy by default, so every production caller and every test that drives a
// runner directly gets ProductionRetryPolicy; WithRetryPolicy is what a test harness uses to put a
// deadline policy on the context it gives Migrate.
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
		t.Errorf("ProductionRetryPolicy = %+v, want the constants (%d attempts, %s pause): the rows and docs that name them must stay true", ProductionRetryPolicy, LockTimeoutAttempts, LockTimeoutRetryPause)
	}
}

// retryMode is a counted cap for a counted policy whatever the context's deadline; deadline mode for
// a deadline policy on a context with a deadline; and a LockTimeoutAttempts cap, not deadline mode,
// for a deadline policy on a context with no deadline, so a benchmark's endless context gives up
// rather than retrying for ever.
func TestRetryMode(t *testing.T) {
	t.Parallel()
	withDeadline, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	if dm, cap, _ := ProductionRetryPolicy.retryMode(context.Background()); dm || cap != LockTimeoutAttempts {
		t.Errorf("ProductionRetryPolicy.retryMode(no deadline) = (deadlineMode %v, cap %d), want (false, %d)", dm, cap, LockTimeoutAttempts)
	}
	if dm, cap, _ := ProductionRetryPolicy.retryMode(withDeadline); dm || cap != LockTimeoutAttempts {
		t.Errorf("ProductionRetryPolicy.retryMode(1h deadline) = (deadlineMode %v, cap %d), want (false, %d): a counted policy does not become deadline mode under a deadline", dm, cap, LockTimeoutAttempts)
	}
	if dm, _, d := DeadlineRetryPolicy().retryMode(withDeadline); !dm || d.IsZero() {
		t.Errorf("DeadlineRetryPolicy().retryMode(1h deadline) = (deadlineMode %v, d.IsZero %v), want deadline mode carrying the deadline", dm, d.IsZero())
	}
	if dm, cap, _ := DeadlineRetryPolicy().retryMode(context.Background()); dm || cap != LockTimeoutAttempts {
		t.Errorf("DeadlineRetryPolicy().retryMode(no deadline) = (deadlineMode %v, cap %d), want the counted fallback (false, %d), not retry-for-ever", dm, cap, LockTimeoutAttempts)
	}
}

// attemptsField logs the cap for a counted bound and names the deadline in deadline mode, so a
// production give-up still logs attempts=3 (an int) exactly as before.
func TestAttemptsField(t *testing.T) {
	t.Parallel()
	if got := attemptsField(false, LockTimeoutAttempts); got != LockTimeoutAttempts {
		t.Errorf("attemptsField(false, %d) = %v, want the int %d (production's log must not move)", LockTimeoutAttempts, got, LockTimeoutAttempts)
	}
	if got := attemptsField(true, 0); got != "until the context's deadline" {
		t.Errorf("attemptsField(true, 0) = %v, want the deadline note", got)
	}
}

// validate refuses a negative Attempts by name before any attempt runs — the same unusable-field
// class as a negative Pause, which pause() clamps — and retryLockTimeouts refuses it without running
// the attempt.
func TestValidateRefusesNegativeAttempts(t *testing.T) {
	t.Parallel()
	if err := (RetryPolicy{Attempts: -1}).validate(); err == nil {
		t.Error("validate(Attempts -1) = nil, want a refusal naming the field")
	}
	for _, p := range []RetryPolicy{ProductionRetryPolicy, DeadlineRetryPolicy()} {
		if err := p.validate(); err != nil {
			t.Errorf("validate(%+v) = %v, want nil", p, err)
		}
	}
	ran := false
	err := retryLockTimeouts(WithRetryPolicy(context.Background(), RetryPolicy{Attempts: -2}),
		"a migration", "migration", "0001_x.up.sql",
		func(error) bool { return true },
		func(context.Context) error { ran = true; return nil })
	if err == nil {
		t.Error("retryLockTimeouts with Attempts -2 = nil, want the validate refusal")
	}
	if ran {
		t.Error("retryLockTimeouts with Attempts -2 ran the attempt, want refusal before the first attempt")
	}
}

// A negative Pause has no meaning and is clamped to 0, so the runner does not wait a negative time
// (there is no longer a precomputed count for it to divide by zero).
func TestNegativePauseIsClamped(t *testing.T) {
	t.Parallel()
	if got := (RetryPolicy{Attempts: 2, Pause: -LockTimeout}).pause(); got != 0 {
		t.Errorf("pause() with Pause=-LockTimeout = %s, want 0", got)
	}
}

// Finding 2: under the production default a non-timeout error that merely arrives while the context
// is done — a boot stopped by SIGTERM mid-migration, or a real migration error racing the deadline —
// comes back unchanged, not in the "after N attempts" give-up form, so production's error text does
// not move and a real error is not misread as the deadline's.
func TestProductionDefaultDoesNotWrapAContextOrMigrationError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	notALockTimeout := func(error) bool { return false }

	sentinel := errors.New("a real migration error")
	if err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", notALockTimeout,
		func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) || strings.Contains(err.Error(), "after") {
		t.Errorf("err = %q, want the migration's own error unwrapped under the production default", err)
	}
	if err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", notALockTimeout,
		func(context.Context) error { return context.Canceled }); strings.Contains(err.Error(), "after") {
		t.Errorf("production default wrapped a context.Canceled as %q, want it unwrapped (a SIGTERM'd boot keeps its own error)", err)
	}
}

// Finding 7 (was TestRetryWrapsAContextErrorInTheNamedForm): in deadline mode an attempt the context
// cuts short comes back in the named "after N attempts" form with errors.Is still holding; a
// non-timeout, non-context error comes back unwrapped. The past deadline makes the attempt's error
// and ctx.Err() agree, rather than cancelling before the first attempt.
func TestDeadlineModeWrapsAContextErrorAndPassesOthersThrough(t *testing.T) {
	t.Parallel()
	past, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	past = WithRetryPolicy(past, DeadlineRetryPolicy())
	err := retryLockTimeouts(past, "a migration", "migration", "0001_x.up.sql",
		func(error) bool { return false },
		func(context.Context) error { return context.DeadlineExceeded })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is(context.DeadlineExceeded) to hold", err)
	}
	if !strings.Contains(err.Error(), "after 1 attempts") {
		t.Errorf("err = %q, want the named \"after N attempts\" form, not a bare context error", err)
	}

	live, cancelLive := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLive()
	live = WithRetryPolicy(live, DeadlineRetryPolicy())
	sentinel := errors.New("a real migration error")
	if err := retryLockTimeouts(live, "a migration", "migration", "0001_x.up.sql",
		func(error) bool { return false },
		func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) || strings.Contains(err.Error(), "after") {
		t.Errorf("err = %v, want a non-timeout error on a live context back unwrapped", err)
	}
}

// Finding 1: a deadline policy reads each attempt's own duration instead of a count computed up front, so a short-lock_timeout migration keeps retrying toward the deadline, and the give-up carries the *LockTimeoutError named rather than a
// bare context error. The up-front count would have allowed about one attempt for this 1 s span. The
// stub honours ctx and the policy runs with no pause, so the deadline can only fall inside an attempt;
// deleting the proactive give-up check in retryLockTimeouts then lets the deadline cut the final
// attempt, and the give-up carries context.DeadlineExceeded — the mutation the last assertion catches.
func TestDeadlineModeReadsEachAttemptAndGivesUpNamed(t *testing.T) {
	t.Parallel()
	lte := &LockTimeoutError{Migration: "0001_x.up.sql"}
	timedOut := func(err error) bool { var e *LockTimeoutError; return errors.As(err, &e) }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx = WithRetryPolicy(ctx, RetryPolicy{Pause: 0}) // no pause: the deadline can only fall inside an attempt
	attempts := 0
	err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", timedOut,
		func(ctx context.Context) error {
			attempts++
			select {
			case <-time.After(100 * time.Millisecond): // a short-lock_timeout attempt
				return lte
			case <-ctx.Done(): // the deadline fell inside this attempt
				return ctx.Err()
			}
		})
	if attempts < 5 {
		t.Errorf("made %d attempts, want many: a short-lock_timeout attempt keeps retrying toward the deadline, not the ~1 a LockTimeout-cycle count gives for 1s", attempts)
	}
	var got *LockTimeoutError
	if !errors.As(err, &got) {
		t.Fatalf("err = %v, want the *LockTimeoutError carried through (a named give-up, not a bare context error)", err)
	}
	// The proactive give-up fired before the deadline cut an attempt, so the give-up is the last lock
	// timeout and does NOT carry the context deadline. Delete the `time.Until(deadline) < pause+took`
	// check in retryLockTimeouts and this assertion fails: the deadline then falls inside the final
	// attempt, the stub returns ctx.Err(), and the give-up carries context.DeadlineExceeded.
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v carries context.DeadlineExceeded: the deadline cut an attempt, so the proactive give-up check did not fire", err)
	}
	if !strings.Contains(err.Error(), "after") {
		t.Errorf("err = %q, want the named \"after N attempts\" form", err)
	}
}

// Finding 3: a deadline policy on a context with no deadline falls back to the counted
// LockTimeoutAttempts cap, so a benchmark's endless context gives up with the named error rather than
// retrying for ever.
func TestDeadlineModeWithoutDeadlineGivesUp(t *testing.T) {
	t.Parallel()
	lte := &LockTimeoutError{Migration: "0001_x.up.sql"}
	timedOut := func(err error) bool { var e *LockTimeoutError; return errors.As(err, &e) }
	ctx := WithRetryPolicy(context.Background(), RetryPolicy{Pause: 0})
	attempts := 0
	err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", timedOut,
		func(context.Context) error { attempts++; return lte })
	if attempts != LockTimeoutAttempts {
		t.Errorf("made %d attempts on an endless context, want the counted fallback %d (not retry for ever)", attempts, LockTimeoutAttempts)
	}
	var got *LockTimeoutError
	if !errors.As(err, &got) {
		t.Errorf("err = %v, want the *LockTimeoutError named", err)
	}
}

// The tester's edge, now closed by the wrap: once a lock timeout has occurred, a later attempt the
// deadline cuts still gives up NAMED -- the last *LockTimeoutError is joined into the give-up -- not
// with a bare context error. Four 100 ms lock-timeout attempts, then a fifth that blocks until the
// deadline cuts it: the construction the row's tester used to show the bare-context-error edge.
func TestDeadlineModeNamesTheLockWhenALaterAttemptIsCut(t *testing.T) {
	t.Parallel()
	lte := &LockTimeoutError{Migration: "0001_x.up.sql"}
	timedOut := func(err error) bool { var e *LockTimeoutError; return errors.As(err, &e) }
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	ctx = WithRetryPolicy(ctx, RetryPolicy{Pause: 0}) // deadline mode
	n := 0
	err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", timedOut,
		func(ctx context.Context) error {
			n++
			if n <= 4 {
				time.Sleep(100 * time.Millisecond)
				return lte
			}
			<-ctx.Done() // a longer attempt the deadline cuts before it can time out
			return ctx.Err()
		})
	if n < 5 {
		t.Fatalf("made %d attempts, want the fifth (the one the deadline cuts) to be reached", n)
	}
	var got *LockTimeoutError
	if !errors.As(err, &got) {
		t.Errorf("err = %v, want the *LockTimeoutError named even though the deadline cut the last attempt, not a bare context error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is(context.DeadlineExceeded) to still hold", err)
	}
}

// Finding (reviewer round 3): under the production default a later attempt's context error, even after
// an earlier lock timeout, comes back UNCHANGED — the "after N attempts" wrap is deadline-mode only, so
// a boot stopped by SIGTERM mid-run keeps its own error whether or not a migration timed out first.
func TestProductionDefaultDoesNotWrapAfterATimeout(t *testing.T) {
	t.Parallel()
	lte := &LockTimeoutError{Migration: "0001_x.up.sql"}
	timedOut := func(err error) bool { var e *LockTimeoutError; return errors.As(err, &e) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A counted policy (not deadline mode) with no pause: the first attempt times out, the second is
	// cut by cancellation.
	ctx = WithRetryPolicy(ctx, RetryPolicy{Attempts: 3, Pause: 0})
	n := 0
	err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", timedOut,
		func(context.Context) error {
			n++
			if n == 1 {
				return lte
			}
			cancel()
			return context.Canceled
		})
	if strings.Contains(err.Error(), "after") {
		t.Errorf("err = %q, want the context error unchanged under the production default, not an \"after N attempts\" wrap", err)
	}
	if errors.As(err, new(*LockTimeoutError)) {
		t.Errorf("err = %v, want the bare context error that cut the attempt, not the earlier lock timeout", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(context.Canceled)", err)
	}
}
