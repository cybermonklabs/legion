package main

import (
	"fmt"
	"math"
	"slices"
)

// report prints what one subcommand measured: a RESULT line per measurement and a FAIL line for
// each that misses its bound or its rule, each headed by prefix, and counts the FAIL lines. The
// Job's verdict reads these lines (r8.sh).
type report struct {
	prefix string
	failed int
}

// result prints one measurement.
func (r *report) result(format string, args ...any) {
	fmt.Printf("RESULT "+r.prefix+" "+format+"\n", args...)
}

// fail prints one measurement that missed its bound or its rule, and marks the run failed.
func (r *report) fail(format string, args ...any) {
	r.failed++
	fmt.Printf("FAIL "+r.prefix+" "+format+"\n", args...)
}

// median is the middle of values, the mean of the two middle ones for an even count; -1 for none,
// which JSON carries where NaN would not encode.
func median(values []float64) float64 {
	if len(values) == 0 {
		return -1
	}
	sorted := slices.Sorted(slices.Values(values))
	middle := len(sorted) / 2
	if len(sorted)%2 == 0 {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
	return sorted[middle]
}

// percentile is the nearest-rank p-th percentile of values (p in (0, 1]): the smallest value at or
// above which a share p of them lie; -1 for none.
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return -1
	}
	sorted := slices.Sorted(slices.Values(values))
	rank := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(rank, 0), len(sorted)-1)]
}
