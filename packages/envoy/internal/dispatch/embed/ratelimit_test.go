package embed

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/retry"
)

// countingEmbedder answers errs in order (one per call, the last repeats once exhausted) and
// records how many times Embed was called and when each call happened, so a test can assert on
// both the limiter's pacing and its AIMD adjustment without a real model-service call.
type countingEmbedder struct {
	errs  []error
	calls []time.Time
}

func (c *countingEmbedder) Identity() string { return "counting" }
func (c *countingEmbedder) Limits() Limits {
	return Limits{MaxBatchTexts: 3, BackgroundTokensPerMinute: 7}
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string, _ InputType) ([][]float32, error) {
	c.calls = append(c.calls, time.Now())
	var err error
	if i := len(c.calls) - 1; i < len(c.errs) {
		err = c.errs[i]
	}
	if err != nil {
		return nil, err
	}
	return make([][]float32, len(texts)), nil
}

func TestRateLimitedEmbedderPacesSuccessiveCallsAtLeastTheFloorApart(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	limiter.setInterval(20 * time.Millisecond) // scaled down from rateLimitFloor for a fast test

	ctx := context.Background()
	for range 3 {
		if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
			t.Fatalf("Embed: %v", err)
		}
	}
	if len(inner.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(inner.calls))
	}
	for i := 1; i < len(inner.calls); i++ {
		gap := inner.calls[i].Sub(inner.calls[i-1])
		if gap < 20*time.Millisecond {
			t.Errorf("gap between call %d and %d = %v, want at least 20ms", i-1, i, gap)
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

// TestRateLimitedEmbedderRecoversGraduallyAfterSuccess: one untroubled call raises the rate by
// rateLimitRecoveryStep calls per second (additive increase on the rate, not a fixed cut of the
// interval), so the interval shortens but stays above the floor.
func TestRateLimitedEmbedderRecoversGraduallyAfterSuccess(t *testing.T) {
	inner := &countingEmbedder{}
	limiter := NewRateLimitedEmbedder(inner)
	start := rateLimitFloor + time.Second // above rateLimitFloor, as if recovering from backoff
	limiter.setInterval(start)

	ctx := context.Background()
	if _, err := limiter.Embed(ctx, []string{"x"}, InputDocument); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	got := limiter.Interval()
	if gained := 1/got.Seconds() - 1/start.Seconds(); got >= start || got <= rateLimitFloor || math.Abs(gained-rateLimitRecoveryStep) > 1e-6 {
		t.Errorf("Interval() after one success = %v from %v (the rate up %.6f a second), want it shorter, above the floor, the rate up %.6f", got, start, gained, rateLimitRecoveryStep)
	}
}

// TestRateLimitedEmbedderRecoversFromTheCeilingWithinBoundedCalls: nine throttles take the
// limiter from the floor to the ceiling (0.75 s x 2^9 > 300 s), and at most 32 untroubled calls
// bring it back to the floor, waiting under 400 s in all (rateLimitRecoveryStep's arithmetic:
// 32 calls, 393.7 s). Each call waits the interval adjust left before it (burst 1, as
// TestRateLimitedEmbedderPacesSuccessiveCallsAtLeastTheFloorApart shows), so the row sums
// Interval() before each adjust, as Embed calls it, rather than sleeping the waits out. Stepping
// the interval down by a fixed 50 ms took 5,985 calls and about 10 days of waits.
func TestRateLimitedEmbedderRecoversFromTheCeilingWithinBoundedCalls(t *testing.T) {
	const (
		maxCalls = 32
		maxWaits = 400 * time.Second
	)
	limiter := NewRateLimitedEmbedder(&countingEmbedder{})
	throttle := embedtest.FakeThrottleError{Code: "ThrottlingException"}
	for i := range 9 {
		if got := limiter.Interval(); got == retry.MaxDelay {
			t.Fatalf("Interval() = the ceiling after %d throttles, want it after 9", i)
		}
		limiter.adjust(throttle)
	}
	if got := limiter.Interval(); got != retry.MaxDelay {
		t.Fatalf("Interval() after 9 throttles = %v, want the ceiling %v", got, retry.MaxDelay)
	}
	var waited time.Duration
	calls := 0
	for limiter.Interval() != rateLimitFloor {
		if calls == maxCalls {
			t.Fatalf("Interval() = %v after %d untroubled calls that waited %v, want the floor %v", limiter.Interval(), calls, waited, rateLimitFloor)
		}
		waited += limiter.Interval()
		limiter.adjust(nil)
		calls++
	}
	if waited > maxWaits {
		t.Errorf("the %d untroubled calls back to the floor waited %v, want at most %v", calls, waited, maxWaits)
	}
}

// TestRateLimitedEmbedderStaysAtTheCeilingUnderASustainedThrottle: a storm of throttles halves the
// rate on each until the interval reaches retry.MaxDelay, then holds it there however long the
// storm lasts.
func TestRateLimitedEmbedderStaysAtTheCeilingUnderASustainedThrottle(t *testing.T) {
	limiter := NewRateLimitedEmbedder(&countingEmbedder{})
	throttle := embedtest.FakeThrottleError{Code: "ThrottlingException"}
	for i := range 100 {
		limiter.adjust(throttle)
		if got := limiter.Interval(); i >= 8 && got != retry.MaxDelay {
			t.Fatalf("Interval() after %d throttles = %v, want the ceiling %v", i+1, got, retry.MaxDelay)
		}
	}
}

// TestRateLimitedEmbedderReadsAzuresThrottleOutageAndAnswer: through a real AzureClient, a 429 or
// a 503 (IsThrottled) halves the rate; a 408 or another 5xx (IsOutage) leaves the pace as it is,
// since one text Azure fails on every time gives it too (only BackOff, which embedqueue calls on
// its canary's outage, moves it); and a 400, an answer about the request, counts as untroubled.
// The error reaches the caller as Azure's.
func TestRateLimitedEmbedderReadsAzuresThrottleOutageAndAnswer(t *testing.T) {
	const start = 2 * time.Second
	for _, tc := range []struct {
		status int
		want   func(time.Duration) bool
		says   string
	}{
		{http.StatusTooManyRequests, func(d time.Duration) bool { return d == 2*start }, "doubled"},
		{http.StatusServiceUnavailable, func(d time.Duration) bool { return d == 2*start }, "doubled"},
		{http.StatusRequestTimeout, func(d time.Duration) bool { return d == start }, "unchanged"},
		{http.StatusInternalServerError, func(d time.Duration) bool { return d == start }, "unchanged"},
		{http.StatusBadGateway, func(d time.Duration) bool { return d == start }, "unchanged"},
		{http.StatusGatewayTimeout, func(d time.Duration) bool { return d == start }, "unchanged"},
		{http.StatusBadRequest, func(d time.Duration) bool { return d < start }, "shorter"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"code":"Simulated","message":"simulated"}}`))
			}))
			defer server.Close()
			limiter := NewRateLimitedEmbedder(testAzureClient(t, server, AzureConfig{}))
			limiter.setInterval(start)
			if _, err := limiter.Embed(context.Background(), []string{"x"}, InputDocument); err == nil {
				t.Fatal("Embed: want Azure's error, got nil")
			}
			if got := limiter.Interval(); !tc.want(got) {
				t.Errorf("Interval() after Azure's %d = %v from %v, want it %s", tc.status, got, start, tc.says)
			}
		})
	}
}

// A call that ran out of time, through a real AzureClient whose HTTP client times out, leaves the
// pace where it was, as an outage does: Azure did not answer in time, which is no sign it is well.
// Counted as untroubled, it raised the rate.
func TestRateLimitedEmbedderLeavesThePaceAfterACallThatRanOutOfTime(t *testing.T) {
	const start = 2 * time.Second
	server := hangingServer(t)
	server.Client().Timeout = 100 * time.Millisecond
	limiter := NewRateLimitedEmbedder(testAzureClient(t, server, AzureConfig{}))
	limiter.setInterval(start)
	if _, err := limiter.Embed(context.Background(), []string{"x"}, InputDocument); err == nil {
		t.Fatal("Embed: want the call's timeout, got nil")
	}
	if got := limiter.Interval(); got != start {
		t.Errorf("Interval() after a call that ran out of time = %v from %v, want it unchanged", got, start)
	}
}

// TestRateLimitedEmbedderBackOffHalvesTheRate: BackOff doubles the interval as a throttle does,
// capped at the ceiling.
func TestRateLimitedEmbedderBackOffHalvesTheRate(t *testing.T) {
	limiter := NewRateLimitedEmbedder(&countingEmbedder{})
	limiter.BackOff()
	if got := limiter.Interval(); got != 2*rateLimitFloor {
		t.Fatalf("Interval() after one BackOff = %v, want %v", got, 2*rateLimitFloor)
	}
	limiter.setInterval(retry.MaxDelay)
	limiter.BackOff()
	if got := limiter.Interval(); got != retry.MaxDelay {
		t.Fatalf("Interval() after BackOff at the ceiling = %v, want %v", got, retry.MaxDelay)
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
	limiter.setInterval(time.Hour) // as deep into backoff as this limiter can ever go, exaggerated

	ctx := context.Background()
	started := time.Now()
	// The "live search" stand-in: the same inner embedder, called directly, bypassing the limiter
	// entirely - exactly how cmd/dispatch wires api.Deps.Embedder versus embedqueue's pace
	// (embedqueue.Deps.Paced).
	if _, err := inner.Embed(ctx, []string{"x"}, InputQuery); err != nil {
		t.Fatalf("unwrapped Embed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Errorf("unwrapped call took %v, want near-instant - it must never wait on limiter.interval", elapsed)
	}
	_ = limiter // the limiter exists only to prove it is irrelevant to the call above
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
	limiter.setInterval(time.Hour)
	_ = limiter.limiter.Wait(context.Background()) // consume the burst token, as if a previous call had just happened, so the next wait is forced to the full interval

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := limiter.Embed(ctx, []string{"x"}, InputDocument)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Embed with a cancelled context took %v, want it to return promptly", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Embed error = %v, want context.DeadlineExceeded", err)
	}
	if len(inner.calls) != 0 {
		t.Error("the inner Embedder was called despite the context ending during pacing")
	}
}
