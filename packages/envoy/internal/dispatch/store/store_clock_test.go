package store

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// A Store that Open returned runs its timers on the real clock: its clock is nil, and clk answers
// clock.Real(), so every deployment keeps the timers and durations it had before the store took a
// clock. A Store whose clock a test set answers that test's fake.
func TestAStoreOpenReturnsKeepsTheRealClock(t *testing.T) {
	t.Parallel()
	database := openMigratedTestStore(t, latestVersion)
	if database.clock != nil {
		t.Fatalf("the clock field of a Store Open returned = %T, want nil, which clk answers as the real clock", database.clock)
	}
	if got, want := database.clk(), clock.Real(); got != want {
		t.Fatalf("the clock of a Store Open returned = %T, want clock.Real()'s %T", got, want)
	}

	fake := clocktest.New(time.Date(2026, time.November, 28, 0, 0, 0, 0, time.UTC))
	database.clock = fake
	if got := database.clk(); got != clock.Clock(fake) {
		t.Fatalf("the clock of a Store whose clock the test set to a clocktest.Fake = %T %p, want that fake (%p)", got, got, fake)
	}
}
