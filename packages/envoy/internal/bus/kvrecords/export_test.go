package kvrecords

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// HoldWalkClockForTest has every walk until t ends time its passes after the first on a fake clock
// that nothing moves (walkClock), so the catch-up bound (maxCatchUp) never ends a walk and a walk
// that cannot catch its stream meets the pass bound (maxPasses), however slow the machine is. No
// test of this package runs in parallel, so no other walk reads the clock while t holds it. It is
// compiled into the package's tests only.
func HoldWalkClockForTest(t *testing.T) {
	t.Helper()
	previous := walkClock
	walkClock = clocktest.New(time.Now())
	t.Cleanup(func() { walkClock = previous })
}
