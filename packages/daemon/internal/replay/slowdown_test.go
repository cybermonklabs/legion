package replay

import (
	"math"
	"testing"
	"time"
)

// A work segment's length in a replay is its recorded length times f^((n − m)/42), n the issues in
// flight in the replay at its start and m in the record (spec R3, assumption 1): with as many in
// flight as the record had it keeps its recorded length at any f, with 42 more it takes f times as
// long, with 42 fewer it takes the recorded length divided by f, and at f = 1 it keeps its length
// whatever n and m are. The test holds slowed to each of those at f 1, 1.2 and 1.6, and to the
// square root of f halfway, for a segment of 90 minutes.
func TestSegmentTimeUnderSlowdown(t *testing.T) {
	t.Parallel()
	const recorded = 90 * time.Minute
	scaled := func(by float64) time.Duration { return time.Duration(math.Round(float64(recorded) * by)) }
	for _, f := range []float64{1, 1.2, 1.6} {
		for _, c := range []struct {
			name string
			n, m int
			want time.Duration
		}{
			{name: "as many in flight as the record", n: 30, m: 30, want: recorded},
			{name: "42 more in flight", n: 72, m: 30, want: scaled(f)},
			{name: "42 fewer in flight", n: 30, m: 72, want: scaled(1 / f)},
			{name: "21 more in flight", n: 51, m: 30, want: scaled(math.Sqrt(f))},
		} {
			if got := slowed(recorded, f, c.n, c.m); got != c.want {
				t.Errorf("at f %v with %s (n %d, m %d), a segment recorded at %s takes %s, want %s", f, c.name, c.n, c.m, recorded, got, c.want)
			}
		}
	}
}
