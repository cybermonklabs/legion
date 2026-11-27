package cireconcile

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// advancingPaceClock executes the production waits on a fake clock. Each wait advances exactly
// the duration requested by production, including any pass deadline crossed along the way.
// The test checks the requested gaps, not a second copy of the pacing arithmetic.
type advancingPaceClock struct {
	*clocktest.Fake
	mu    sync.Mutex
	waits []time.Duration
}

func (c *advancingPaceClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	c.Advance(d)
	return ctx.Err()
}

func (h *harness) paceOnTheTestClock() *advancingPaceClock {
	h.t.Helper()
	h.boundByTheTest()
	c := &advancingPaceClock{Fake: h.bound}
	h.reconciler.passClock = checkedPassClock{Clock: c, h: h}
	h.github.now = c.Now
	h.github.queryWait = c.Advance
	return c
}

func (c *advancingPaceClock) takeWaits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	waits := c.waits
	c.waits = nil
	return waits
}

func (h *harness) requireWaits(c *advancingPaceClock, want ...time.Duration) {
	h.t.Helper()
	if got := c.takeWaits(); !slices.Equal(got, want) {
		h.t.Fatalf("the production pacing waits are %v, want exactly %v", got, want)
	}
}

// expiredPassClock ends each deadline as production arms it, so a test can exercise a pass
// whose bound is already spent without racing a one-nanosecond wall-clock timeout.
type expiredPassClock struct {
	clock.Clock
	advance func(time.Duration)
}

func (c expiredPassClock) WithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	bounded, cancel := c.Clock.WithTimeout(ctx, timeout)
	c.advance(timeout)
	return bounded, cancel
}
