package main

// The bounds the 8,001-block document version is held to, and the rule that takes the lower of
// them. This file carries no build tag, unlike the basebuild cost helpers that read these bounds
// (read_cost_test.go reads costBudget, write_cost_test.go writeCostOverRatio and
// writeCostOverAllowance), so every run of this package's tests holds the rule, with no database
// and no base build.

import (
	"testing"
	"time"
)

const (
	// costBudget is the most a read's p99 may rise above the same read at the base before a read
	// cost tool fails it (read_cost_test.go's readCostMeasure). The write cost readers hold their
	// request paths to ratios instead (write_cost_test.go, writeCostEditRatio and the two beside
	// it, and writeCostOverRatio), and the 8,001-block version to this rise beside its ratio, the
	// lower allowance deciding (writeCostOverAllowance).
	costBudget = 100 * time.Millisecond
	// writeCostOverRatio is the 8,001-block version's ratio bound, measured as the other ratio
	// bounds are, over three clean runs (write_cost_test.go, beside writeCostEditRatio).
	writeCostOverRatio = 1.25
)

// writeCostOverAllowance holds the 8,001-block path's deciding statistic at this child, here, to
// the allowance the same statistic at the commit before this child, before, gives it: the lower
// of writeCostOverRatio times before and before plus costBudget, the rise of at most 100 ms this
// path was held to before it carried a ratio. The lower decides so that neither bound is weaker
// than it was alone: below a before of 400 ms the ratio allows less, above it the rise does. It
// answers the allowance, which bound gave it ("ratio" or "rise"), and whether here is inside it,
// at or below the allowance as the ratio paths are at or below their bound. Where the two allow
// the same it names the ratio, since either names the same figure.
func writeCostOverAllowance(here, before time.Duration) (allowance time.Duration, deciding string, inside bool) {
	allowance, deciding = time.Duration(writeCostOverRatio*float64(before)), "ratio"
	if rise := before + costBudget; rise < allowance {
		allowance, deciding = rise, "rise"
	}
	return allowance, deciding, here <= allowance
}

// The 8,001-block version is held to the lower of its ratio and its rise, and a reading at its
// allowance passes. Each row is one edge of writeCostOverAllowance's rule: a commit before of
// 300 ms, where the ratio allows 375 ms and the rise 400; one of 600 ms, where the ratio allows
// 750 ms and the rise 700; and one of 400 ms, where both allow 500. A here exactly at the
// allowance passes and one a microsecond over fails, so the allowance is inclusive, as each
// ratio bound is.
//
// It starts no arm, builds no commit and opens no database, so it needs no BASELINE_SHA and
// takes milliseconds. It carries no build tag, so every run of this package's tests runs it, and
// it is run alone by name as
//
//	go test ./cmd/dispatch/ -run '^TestThe8001BlockVersionIsHeldToTheLowerOfItsRatioAndItsRise$' -count=1 -v
func TestThe8001BlockVersionIsHeldToTheLowerOfItsRatioAndItsRise(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name      string
		before    time.Duration
		allowance time.Duration
		deciding  string
	}{
		{name: "the ratio decides below 400 ms", before: 300 * time.Millisecond, allowance: 375 * time.Millisecond, deciding: "ratio"},
		{name: "the rise decides above 400 ms", before: 600 * time.Millisecond, allowance: 700 * time.Millisecond, deciding: "rise"},
		{name: "both allow the same at 400 ms", before: 400 * time.Millisecond, allowance: 500 * time.Millisecond, deciding: "ratio"},
	} {
		t.Run(row.name, func(t *testing.T) {
			allowance, deciding, inside := writeCostOverAllowance(row.allowance, row.before)
			if allowance != row.allowance || deciding != row.deciding {
				t.Errorf("a commit before of %s allows %s, the %s deciding; want %s, the %s deciding",
					row.before, allowance, deciding, row.allowance, row.deciding)
			}
			if !inside {
				t.Errorf("a commit before of %s: a reading of %s, exactly at its allowance, failed; want it to pass",
					row.before, row.allowance)
			}
			over := row.allowance + time.Microsecond
			if _, _, inside := writeCostOverAllowance(over, row.before); inside {
				t.Errorf("a commit before of %s: a reading of %s, one microsecond over its %s allowance, passed; want it to fail",
					row.before, over, row.allowance)
			}
		})
	}
}
