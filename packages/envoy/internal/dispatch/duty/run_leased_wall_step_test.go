package duty_test

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
)

// wallStepTick pairs the wall reading at a fire with what its process handed the gate.
type wallStepTick struct {
	wall time.Time
	leasedTick
}

const wallStepInterval = time.Minute

// stepLeasedLoops runs one process across a wall-clock step and starts another after the step.
// Their ticks are offset by half an interval so only one tries the lease at a time.
func stepLeasedLoops(t *testing.T, step time.Duration, firesAfterStep int) []wallStepTick {
	t.Helper()
	const name = "wall-step-loop"
	fake := clocktest.New(tickDue)
	database := storetest.Open(t)
	pass := func(context.Context) {}
	first := startLoggedLeasedLoop(t, fake, leasesOf(t, database), name, wallStepInterval, pass)
	var second *logtest.Buffer
	fire := func(step time.Duration) wallStepTick {
		t.Helper()
		loops := 1
		var secondBefore int
		if second != nil {
			loops = 2
			secondBefore = second.Len()
		}
		at := fake.AwaitArmed(t, "each wall-step process's next tick", loops)[0]
		fake.Set(at)
		// The fired loop re-arms only after logging and giving the lease back. With only one
		// tick due, only its log can grow; there is no need to assume which process fired.
		fake.AwaitArmed(t, "the wall-step process to re-arm after its gate returns", loops)
		logs := first
		if second != nil && second.Len() > secondBefore {
			logs = second
		}
		return wallStepTick{wall: at.Add(step), leasedTick: nextLeasedTick(t, logs, name, "the wall-step tick")}
	}

	ticks := []wallStepTick{fire(0)}
	fake.StepWall(step)
	fake.Advance(wallStepInterval / 2)
	second = startLoggedLeasedLoop(t, fake, leasesOf(t, storetest.SecondProcess(t, database)), name, wallStepInterval, pass)
	for range firesAfterStep {
		ticks = append(ticks, fire(step))
	}
	return ticks
}

// requireWallPeriods checks that each process hands the gate the stepped wall reading at the fire.
// A period runs on the first fire into it and is refused on each later fire, from either process.
func requireWallPeriods(t *testing.T, ticks []wallStepTick) {
	t.Helper()
	seen := make(map[time.Time]bool)
	for i, tick := range ticks {
		period := duty.PeriodBoundary(tick.wall, wallStepInterval)
		if !tick.due.Equal(tick.wall) || !tick.period.Equal(period) {
			t.Errorf("fire %d, at %s on the wall clock, handed Exclusive %s and took the period at %s; want its wall reading and the period at %s",
				i+1, tick.wall.Format(time.TimeOnly), tick.due.UTC().Format(time.TimeOnly), tick.period.UTC().Format(time.TimeOnly), period.Format(time.TimeOnly))
		}
		if wantRun := !seen[period]; tick.ran != wantRun {
			t.Errorf("fire %d, into the wall period at %s: ran %t, want %t; only the first fire into a period may run",
				i+1, period.Format(time.TimeOnly), tick.ran, wantRun)
		}
		seen[period] = true
	}
}

// After a step forward, both processes use the stepped wall clock, as they did with time.Ticker.
// No fire takes the period that the wall skipped; using the old grid would make one take it.
func TestRunLeasedBucketsOnTheWallClockAcrossAStepForward(t *testing.T) {
	t.Parallel()
	ticks := stepLeasedLoops(t, wallStepInterval, 4)
	requireWallPeriods(t, ticks)
	skipped := tickDue.Add(2 * wallStepInterval)
	for i, tick := range ticks {
		if tick.period.Equal(skipped) {
			t.Errorf("fire %d took the period at %s, which the wall clock stepped over", i+1, skipped.Format(time.TimeOnly))
		}
	}
}

// After a step back, the first fire is refused because it falls in a period already served.
// Later fires still bucket by the stepped wall clock for both processes. The fake has no
// monotonic clock reading, so the step can move a deadline computed after it; the test checks
// the wall reading each fire delivers, not the cadence across the step.
func TestRunLeasedBucketsOnTheWallClockAcrossAStepBack(t *testing.T) {
	t.Parallel()
	ticks := stepLeasedLoops(t, -wallStepInterval, 5)
	requireWallPeriods(t, ticks)
	if ticks[1].ran {
		t.Errorf("the first fire after the step back ran in the already served wall period at %s",
			duty.PeriodBoundary(ticks[1].wall, wallStepInterval).Format(time.TimeOnly))
	}
}
