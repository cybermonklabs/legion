package leakguard

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
)

type visibilityTestClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *visibilityTestClock) Now() time.Time { return c.now }

func (c *visibilityTestClock) Sleep(ctx context.Context, wait time.Duration) error {
	c.waits = append(c.waits, wait)
	c.now = c.now.Add(wait)
	return ctx.Err()
}

// A rate limit reset close enough to the call's bound is waited out once, then the visibility is
// read again rather than treating an unchecked call as safe.
func TestVisibilityRetriesRateLimitAtNearReset(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(time.Minute)
	var reads int
	visibility, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			if reads == 1 {
				return "", &rateLimitError{reset: reset}
			}
			return Public, nil
		},
	}).Visibility(bound(t), "invented-org/invented-repo")
	if err != nil || visibility != Public {
		t.Fatalf("read visibility = %q, %v; want public after the reset", visibility, err)
	}
	if reads != 2 {
		t.Errorf("the visibility was read %d times, want the failed read and one retry", reads)
	}
	if want := []time.Duration{time.Minute}; !slices.Equal(clock.waits, want) {
		t.Errorf("the waits are %v, want %v", clock.waits, want)
	}
}

// A reset beyond the two-minute limit refuses at once and tells the role when the installation's
// limit resets, instead of waiting through a call it cannot clear.
func TestVisibilityRefusesRateLimitBeyondTheWaitBound(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(10 * time.Minute)
	var reads int
	_, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			reads++
			return "", &rateLimitError{reset: reset}
		},
	}).Visibility(bound(t), "invented-org/invented-repo")
	if err == nil {
		t.Fatal("the rate-limited visibility read passed")
	}
	for _, want := range []string{"installation's rate limit is spent", reset.Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not name %q", err, want)
		}
	}
	if reads != 1 {
		t.Errorf("the visibility was read %d times, want no retry", reads)
	}
	if len(clock.waits) != 0 {
		t.Errorf("the waits are %v, want none", clock.waits)
	}
}
