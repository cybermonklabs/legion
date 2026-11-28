package store

import "time"

// MicrosecondCeiling is a time bound as a statement compares it. PostgreSQL stores a timestamptz
// to the microsecond and pgx encodes a bound by truncating its nanoseconds, which lowers it: a time
// stored at the truncated microsecond, before the bound the caller named, would be kept by
// `>= since` and dropped by `< until`. Every stored time is a whole microsecond, so it is at or
// after a bound exactly when it is at or after the bound rounded up to the next microsecond, and
// before the bound exactly when it is before that: rounding up keeps both meanings. nil is no
// bound.
func MicrosecondCeiling(bound *time.Time) *time.Time {
	if bound == nil {
		return nil
	}
	ceiling := bound.Truncate(time.Microsecond)
	if ceiling.Before(*bound) {
		ceiling = ceiling.Add(time.Microsecond)
	}
	return &ceiling
}
