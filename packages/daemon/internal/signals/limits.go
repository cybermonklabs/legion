package signals

import (
	"fmt"
	"math"
)

// limitRange is the legal limits of one signal: a number above 0 and at most max, or, for a signal
// that counts events, a whole number from 1 to max.
type limitRange struct {
	max   float64
	whole bool
}

// limitRanges is the legal limit of each signal a limit can name, by name: the idle share above 0
// and at most 1, the provider's headroom above 0 and at most 10, the merge queue's wait in hours
// above 0 and at most 168, and the kills and the approvals each a whole number from 1 to
// 1,000,000. A limit is where a signal starts to hold admission, so none can be 0 or below.
var limitRanges = map[string]limitRange{
	Idle:      {max: 1},
	Provider:  {max: 10},
	Queue:     {max: 168},
	Kills:     {max: 1_000_000, whole: true},
	Approvals: {max: 1_000_000, whole: true},
}

// CheckLimit says whether limit is a legal limit of the signal name, and an error naming the range
// where it is not: a name that is no signal a limit can name (Names), a limit that is not a finite
// number, one outside the signal's range, and, for the kills and the approvals, one that is not a
// whole number. A range is closed at its largest limit, which is legal, and open at 0, which is
// not; a signal that counts events starts at 1. It is a pure function of its
// arguments, and it clamps nothing: a measured value is never read through it.
func CheckLimit(name string, limit float64) error {
	r, named := limitRanges[name]
	switch {
	case !named:
		return fmt.Errorf("signals: %q is no signal a limit can name", name)
	case math.IsNaN(limit) || math.IsInf(limit, 0):
		return fmt.Errorf("signals: the limit of %s is %v, which is no finite number", name, limit)
	case r.whole && (limit != math.Trunc(limit) || limit < 1 || limit > r.max):
		return fmt.Errorf("signals: the limit of %s is %v, which is no whole number from 1 to %.0f", name, limit, r.max)
	case !r.whole && (limit <= 0 || limit > r.max):
		return fmt.Errorf("signals: the limit of %s is %v, which is not above 0 and at most %.0f", name, limit, r.max)
	}
	return nil
}
