package embedqueue

import (
	"testing"
	"time"
)

// TestTheRecountGapAddsItsDrawnFractionOfTheJitter holds the draw that R7e's jitter rests on: for
// a T under a minute, recountGap is recountSchedule's interval plus exactly the random source's
// fraction of its jitter, so a gap that ignores its source, or scales the draw wrongly, fails. The
// loop's own T (coverageRecountTimeout) has a jitter of 0, so the loop's gaps cannot show the draw.
func TestTheRecountGapAddsItsDrawnFractionOfTheJitter(t *testing.T) {
	t.Parallel()
	const timeout = 20 * time.Second
	interval, jitter := recountSchedule(timeout)
	for _, f := range []float64{0, 0.25, 0.5, 0.999} {
		want := interval + time.Duration(f*float64(jitter))
		if gap := recountGap(func() float64 { return f }, timeout); gap != want {
			t.Errorf("T %v, draw %v: a recount starts %v after the one before, want %v", timeout, f, gap, want)
		}
	}
}
