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

// A counted policy applies its fixed number of attempts whatever the context's deadline, so
// production's bound does not move when a caller holds a deadline. A deadline policy applies as many
// attempts as fit before the deadline, and none-capped (0) where the context has no deadline.
func TestMaxAttempts(t *testing.T) {
	t.Parallel()
	short, cancelShort := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShort()
	long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLong()
	tooShortForOne, cancelTiny := context.WithTimeout(context.Background(), LockTimeout-time.Second)
	defer cancelTiny()
	twentySeconds, cancelTwenty := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelTwenty()

	// A counted policy ignores the deadline.
	if got := ProductionRetryPolicy.maxAttempts(context.Background()); got != LockTimeoutAttempts {
		t.Errorf("ProductionRetryPolicy.maxAttempts(no deadline) = %d, want %d", got, LockTimeoutAttempts)
	}
	if got := ProductionRetryPolicy.maxAttempts(short); got != LockTimeoutAttempts {
		t.Errorf("ProductionRetryPolicy.maxAttempts(2s deadline) = %d, want %d: a counted policy does not shrink under a short deadline", got, LockTimeoutAttempts)
	}

	// A deadline policy with no deadline is uncapped, so the runner retries until the context is
	// cancelled (as under -timeout 0).
	if got := DeadlineRetryPolicy().maxAttempts(context.Background()); got != 0 {
		t.Errorf("DeadlineRetryPolicy().maxAttempts(no deadline) = %d, want 0 (no cap)", got)
	}
	// A deadline with no room for even one full attempt still makes one.
	if got := DeadlineRetryPolicy().maxAttempts(tooShortForOne); got != 1 {
		t.Errorf("DeadlineRetryPolicy().maxAttempts(%s deadline) = %d, want 1", LockTimeout-time.Second, got)
	}
	// A long deadline buys far more attempts than production's fixed bound: the point of the policy.
	if got := DeadlineRetryPolicy().maxAttempts(long); got <= LockTimeoutAttempts {
		t.Errorf("DeadlineRetryPolicy().maxAttempts(1h deadline) = %d, want more than production's %d", got, LockTimeoutAttempts)
	}
	// 20 s holds floor(20/(5+2)) = 2 whole attempt-and-pause cycles; allow a clock tick either way.
	if got := DeadlineRetryPolicy().maxAttempts(twentySeconds); got < 2 || got > 4 {
		t.Errorf("DeadlineRetryPolicy().maxAttempts(20s deadline) = %d, want about 2", got)
	}
}

// attemptsField logs the cap for a counted policy and names the deadline for an uncapped one, so a
// production give-up still logs attempts=3 (an int) exactly as before.
func TestAttemptsField(t *testing.T) {
	t.Parallel()
	if got := attemptsField(LockTimeoutAttempts); got != LockTimeoutAttempts {
		t.Errorf("attemptsField(%d) = %v, want the int %d (production's log must not move)", LockTimeoutAttempts, got, LockTimeoutAttempts)
	}
	if got := attemptsField(0); got != "until the context's deadline" {
		t.Errorf("attemptsField(0) = %v, want the deadline note", got)
	}
}

// Finding A (reviewer, #5806): a public policy can set Pause to -LockTimeout, which would make
// maxAttempts divide by zero. pause() clamps a negative Pause to 0, so the helper survives it.
func TestMaxAttemptsSurvivesANegativePause(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	p := RetryPolicy{Attempts: 0, Pause: -LockTimeout} // without the clamp, cycle == 0 and maxAttempts panics
	if p.pause() != 0 {
		t.Errorf("pause() with Pause=-LockTimeout = %s, want 0", p.pause())
	}
	if got := p.maxAttempts(ctx); got <= 0 {
		t.Errorf("maxAttempts with Pause=-LockTimeout = %d, want a positive count and no panic", got)
	}
}

// Finding B (reviewer, #5806): an attempt the context cuts short must come back in the runner's
// named "after N attempts" form, not as a bare context error, so a deadline-bounded run keeps the
// named-error promise while errors.Is still finds the context error.
func TestRetryWrapsAContextErrorInTheNamedForm(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the context is done, as a caller's deadline firing mid-attempt leaves it
	ctx = WithRetryPolicy(ctx, DeadlineRetryPolicy())
	notALockTimeout := func(error) bool { return false }
	err := retryLockTimeouts(ctx, "a migration", "migration", "0001_x.up.sql", notALockTimeout, func(context.Context) error {
		return context.DeadlineExceeded
	})
	if err == nil {
		t.Fatal("retryLockTimeouts returned nil, want a wrapped context error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want errors.Is(context.DeadlineExceeded) to hold", err)
	}
	if !strings.Contains(err.Error(), "after 1 attempts") {
		t.Errorf("err = %q, want the named \"after N attempts\" form, not a bare context error", err)
	}
}
