package embed

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/testwait"
)

// countingEmbedder answers errs in order (one per call, the last repeats once exhausted) and
// records how many times Embed was called and when each call happened, on now (time.Now when nil),
// so a test can assert on both the limiter's pacing and its AIMD adjustment without a real
// model-service call.
type countingEmbedder struct {
	errs  []error
	calls []time.Time
	now   func() time.Time
}

func (c *countingEmbedder) Identity() string { return "counting" }
func (c *countingEmbedder) Limits() Limits {
	return Limits{MaxBatchTexts: 3, BackgroundTokensPerMinute: 7}
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string, _ InputType) ([][]float32, error) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	c.calls = append(c.calls, now())
	var err error
	if i := len(c.calls) - 1; i < len(c.errs) {
		err = c.errs[i]
	}
	if err != nil {
		return nil, err
	}
	return make([][]float32, len(texts)), nil
}

// Successive calls through the limiter are paced at least the floor apart on its clock: the first
// takes the burst at once, and each after it waits out the delay its reservation was given, armed
// on the limiter's fake, and reaches the inner embedder only once the row moves the fake to it and
// not one tick before.
func TestRateLimitedEmbedderPacesSuccessiveCallsAtLeastTheFloorApart(t *testing.T) {
	fake := clocktest.New(time.Now())
	inner := &countingEmbedder{now: fake.Now}
	limiter := NewRateLimitedEmbedder(inner, WithLimiterClock(fake))

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for call := 2; call <= 3; call++ {
		returned := make(chan error, 1)
		go func() {
			_, err := limiter.Embed(ctx, []string{"x"}, InputDocument)
			returned <- err
		}()
		armed := fake.AwaitArmed(t, fmt.Sprintf("the delay call %d waits out", call), 1)
		fake.Set(armed[0].Add(-time.Nanosecond))
		if len(fake.Armed()) != 1 || len(inner.calls) != call-1 {
			t.Fatalf("one tick short of its delay call %d holds timers %v and the inner embedder has %d calls, want the delay still armed and %d calls",
				call, fake.Armed(), len(inner.calls), call-1)
		}
		fake.Set(armed[0])
		if err := testwait.Receive(t, fmt.Sprintf("call %d to return once its delay passed", call), returned); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if len(inner.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(inner.calls))
	}
	for i := 1; i < len(inner.calls); i++ {
		if gap := inner.calls[i].Sub(inner.calls[i-1]); gap < rateLimitFloor {
			t.Errorf("gap between call %d and %d = %v on the limiter's clock, want at least the floor, %v", i-1, i, gap, rateLimitFloor)
		}
	}
}

func TestRateLimitedEmbedderBacksOffImmediatelyOnThrottle(t *testing.T) {
	throttleErr := embedtest.FakeThrottleError{Code: "ThrottlingException"}
	inner := &countingEmbedder{errs: []error{throttleErr}}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.setInterval(10 * time.Millisecond)

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); !errors.Is(err, throttleErr) {
		t.Fatalf("Embed error = %v, want the throttle error passed through unchanged", err)
	}
	if got := limiter.Interval(); got != 20*time.Millisecond {
		t.Errorf("Interval() after one throttle = %v, want 20ms (doubled from 10ms)", got)
	}
}

func TestRateLimitedEmbedderRecoversGraduallyAfterSuccess(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.setInterval(rateLimitFloor + time.Second) // above rateLimitFloor, as if recovering from backoff

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got, want := limiter.Interval(), rateLimitFloor+time.Second-rateLimitRecoveryStep; got != want {
		t.Errorf("Interval() after one success = %v, want %v (one additive step down)", got, want)
	}
}

func TestRateLimitedEmbedderNeverRecoversBelowTheFloor(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner) // starts at rateLimitFloor already

	ctx := context.Background()
	for range 5 {
		if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if got := limiter.Interval(); got != rateLimitFloor {
		t.Errorf("Interval() after 5 successes starting at the floor = %v, want %v (floored)", got, rateLimitFloor)
	}
}

func TestRateLimitedEmbedderNeverBacksOffPastTheCeiling(t *testing.T) {
	throttleErr := embedtest.FakeThrottleError{Code: "ThrottlingException"}
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.setInterval(retry.MaxDelay / 2)

	ctx := context.Background()
	inner.errs = []error{throttleErr}
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); !errors.Is(err, throttleErr) {
		t.Fatalf("Embed error = %v, want the throttle error", err)
	}
	if got := limiter.Interval(); got != retry.MaxDelay {
		t.Errorf("Interval() after doubling past the ceiling = %v, want %v (capped)", got, retry.MaxDelay)
	}
}

// TestRateLimitedEmbedderNeverGatesAnUnwrappedSiblingCall is the architectural proof behind
// cmd/dispatch's wiring: a live search request holds a direct, unwrapped reference to the same
// underlying Embedder embedqueue's RateLimitedEmbedder wraps, so it is never paced by this
// limiter at all, however deep into backoff the wrapped side currently is - this is what "a
// query never waits behind background embedding work" means at the type level, not something
// RateLimitedEmbedder itself has to coordinate.
func TestRateLimitedEmbedderNeverGatesAnUnwrappedSiblingCall(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	fake := clocktest.New(time.Now())
	limiter.clock = fake
	limiter.setInterval(time.Hour) // as deep into backoff as this limiter can ever go, exaggerated

	ctx := context.Background()
	// The "live search" stand-in: the same inner embedder, called directly, bypassing the limiter
	// entirely - exactly how cmd/dispatch wires api.Deps.Embedder versus embedqueue.Deps.Embedder.
	if _, err := inner.Embed(ctx, []string{"x"}, InputQuery); err != nil {
		t.Fatalf("unwrapped Embed: %v", err)
	}
	// The limiter's own clock is the one Embed waits its interval out on, and it is a fake the
	// test never moves: a call that went through the limiter would have armed the hour above on
	// it and still be waiting.
	if armed := fake.Armed(); len(armed) != 0 {
		t.Errorf("the unwrapped call armed %v on the limiter's clock, want nothing - it must never wait on limiter.interval", armed)
	}
}

// TestRateLimitedEmbedderForwardsIdentityAndLimits proves pacing never hides which embedder
// actually embeds: the wrapper reports the inner one's identity and numbers, not its own.
func TestRateLimitedEmbedderForwardsIdentityAndLimits(t *testing.T) {
	limiter := NewRateLimitedEmbedder(&countingEmbedder{})
	if got := limiter.Identity(); got != "counting" {
		t.Errorf("Identity() = %q, want the inner embedder's %q", got, "counting")
	}
	if got, want := limiter.Limits(), (Limits{MaxBatchTexts: 3, BackgroundTokensPerMinute: 7}); got != want {
		t.Errorf("Limits() = %+v, want the inner embedder's %+v", got, want)
	}
}

func TestRateLimitedEmbedderStopsWaitingWhenContextEnds(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	fake := clocktest.New(time.Now())
	limiter.clock = fake
	limiter.setInterval(time.Hour)
	_ = limiter.limiter.Wait(context.Background()) // consume the burst token, as if a previous call had just happened, so the next wait is forced to the full interval

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		_, err := limiter.Embed(ctx, []string{"x"}, InputDocument)
		returned <- err
	}()
	// Embed is pacing: the hour it reserved is armed on the fake, which only this test moves, so
	// the cancel below is the one thing that can end the wait.
	fake.AwaitArmed(t, "the interval Embed waits out", 1)
	cancel()
	if err := testwait.Receive(t, "Embed to return once its context ended", returned); !errors.Is(err, context.Canceled) {
		t.Errorf("Embed error = %v, want context.Canceled", err)
	}
	if len(inner.calls) != 0 {
		t.Error("the inner Embedder was called despite the context ending during pacing")
	}
}
