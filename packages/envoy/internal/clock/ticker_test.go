package clock_test

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
)

// gridStart puts a ticker's grid ten seconds before each minute ends, so a tick taken a quarter of
// a minute late is taken in the minute after the one it fired in.
var gridStart = time.Date(2026, 9, 22, 12, 0, 50, 0, time.UTC)

// at is 12:minute:second on gridStart's day.
func at(minute, second int) time.Time { return time.Date(2026, 9, 22, 12, minute, second, 0, time.UTC) }

// sent is the tick on ticks' channel now, which the fake sent as it reached the tick's moment or
// Next put there; the row fails naming what when there is none.
func sent(t *testing.T, ticks *clock.Ticker, what string) time.Time {
	t.Helper()
	select {
	case fired := <-ticks.C():
		return fired
	default:
		t.Fatalf("%s: the ticker's channel holds no tick", what)
		return time.Time{}
	}
}

// requireArmed fails naming what unless the fake's pending deadlines are exactly want.
func requireArmed(t *testing.T, fake *clocktest.Fake, what string, want ...time.Time) {
	t.Helper()
	if armed := fake.Armed(); !slices.Equal(armed, want) {
		t.Errorf("%s: armed %v, want %v", what, armed, want)
	}
}

// requireFired fails naming what unless the tick handed fired at want.
func requireFired(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s: handed %s, want %s, the moment it fired", what, got.Format(time.TimeOnly), want.Format(time.TimeOnly))
	}
}

// A ticker hands each tick the moment it fired, never the moment its loop took it, as a
// time.Ticker hands the value it sent, and arms the next tick only once the loop's work for the one
// before is done (Next), so a test that sees it armed knows that work has returned. The row is the
// loop, on a fake whose grid is at :50.
//
// A tick whose work overruns the next one: the work runs from 12:01:50 to 12:03:05, over the tick
// due at 12:02:50, and that tick comes at once once the work is done, with the moment it fell due,
// 12:02:50, through no timer. Handed the moment the work ended, 12:03:05, it would fall in the
// minute after the one it fired in.
//
// A tick its loop takes late: the 12:03:50 tick waits on the channel until 12:05:05, and is handed
// as 12:03:50; the grid moment it waited past, 12:04:50, is dropped, as a time.Ticker's channel,
// full while its receiver is away, drops it, and the next tick is due at 12:05:50.
func TestATickerHandsEachTickTheMomentItFired(t *testing.T) {
	fake := clocktest.New(gridStart)
	ticks := clock.NewTicker(fake, time.Minute, false)
	defer ticks.Stop()
	requireArmed(t, fake, "the first tick, one interval in", at(1, 50))

	fake.Set(at(1, 50))
	requireFired(t, "the first tick", sent(t, ticks, "the first tick"), at(1, 50))
	if taken := ticks.Took(); !taken.Equal(at(1, 50)) {
		t.Errorf("Took read %s, want the fake's moment as the loop took the tick, 12:01:50", taken.Format(time.TimeOnly))
	}
	// The tick's work, which runs over the next tick and the minute's end.
	fake.Set(at(3, 5))
	requireArmed(t, fake, "the work of the first tick, before Next")
	ticks.Next()
	requireArmed(t, fake, "Next once the work overran the next tick, which fell due as it ran")
	const overrun = "the tick the first one's work overran, at once after it"
	requireFired(t, overrun, sent(t, ticks, overrun), at(2, 50))
	ticks.Took()
	ticks.Next()
	requireArmed(t, fake, "the tick after the overrun, on the grid", at(3, 50))

	fake.Set(at(3, 50))
	// The loop comes for the tick late, past the grid moment after it.
	fake.Set(at(5, 5))
	const late = "the tick at 12:03:50, taken at 12:05:05"
	requireFired(t, late, sent(t, ticks, late), at(3, 50))
	ticks.Took()
	requireArmed(t, fake, "the late tick's work, before Next")
	ticks.Next()
	requireArmed(t, fake, "the tick after the late one, the grid moment it waited past dropped", at(5, 50))
}

// Two processes' tickers on one grid put each tick in the minute it fired in, however late either
// takes it: here both fire at 12:01:50, the first process takes its tick at once and the second at
// 12:02:05, in the minute after, and both are handed 12:01:50 and so both bucket it in 12:01. A
// ticker started at start hands its first tick at once, with the moment it started, and then ticks
// on the grid from there.
func TestTwoTickersOnOneGridBucketATickInThePeriodItFiredIn(t *testing.T) {
	fake := clocktest.New(gridStart)
	first := clock.NewTicker(fake, time.Minute, false)
	defer first.Stop()
	second := clock.NewTicker(fake, time.Minute, false)
	defer second.Stop()
	requireArmed(t, fake, "both processes' first ticks", at(1, 50), at(1, 50))

	fake.Set(at(1, 50))
	onTime := sent(t, first, "the first process's tick, taken at once")
	first.Took()
	first.Next()
	fake.Set(at(2, 5))
	taken := sent(t, second, "the second process's tick, taken at 12:02:05")
	second.Took()
	second.Next()
	for _, tick := range []struct {
		what string
		got  time.Time
	}{{"the first process's tick, taken at once", onTime}, {"the second process's tick, taken at 12:02:05", taken}} {
		requireFired(t, tick.what, tick.got, at(1, 50))
		if period := tick.got.Truncate(time.Minute); !period.Equal(at(1, 0)) {
			t.Errorf("%s: falls in the minute at %s, want 12:01:00, the minute it fired in", tick.what, period.Format(time.TimeOnly))
		}
	}
	requireArmed(t, fake, "both processes' next ticks, on the one grid", at(2, 50), at(2, 50))

	started := clock.NewTicker(fake, time.Minute, true)
	defer started.Stop()
	requireFired(t, "the first tick of a ticker started at start", sent(t, started, "the first tick of a ticker started at start"), at(2, 5))
	requireArmed(t, fake, "the first tick of a ticker started at start, before Next", at(2, 50), at(2, 50))
	started.Took()
	started.Next()
	requireArmed(t, fake, "the tick after the start, one interval on", at(2, 50), at(2, 50), at(3, 5))
}
