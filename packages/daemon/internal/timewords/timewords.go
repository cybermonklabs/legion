// Package timewords writes a duration as the words a notice or a log line says it in: "5 minutes",
// "20 minutes", "6 hours". Go's own form of a duration, `20m0s`, is no part of a sentence, so a
// sentence that names one of the daemon's bounds builds it from the bound's constant here, and the
// words move with the constant.
package timewords

import (
	"fmt"
	"time"
)

// Words is d in whole hours where it is two hours or more and a whole number of them, else in
// whole minutes where it is a whole number of them, else in whole seconds: 6h is "6 hours", 60m is
// "60 minutes", 1m is "1 minute" and 90s is "90 seconds". A part of a second is dropped.
func Words(d time.Duration) string {
	switch {
	case d >= 2*time.Hour && d%time.Hour == 0:
		return count(int64(d/time.Hour), "hour")
	case d >= time.Minute && d%time.Minute == 0:
		return count(int64(d/time.Minute), "minute")
	default:
		return count(int64(d/time.Second), "second")
	}
}

// count is n of unit, the unit plural for every n but 1.
func count(n int64, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
