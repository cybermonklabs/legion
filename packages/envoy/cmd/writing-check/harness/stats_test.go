package harness

import (
	"fmt"
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestBootstrapOfAKnownCase: two split groups of two cases each, one with both hits and one with
// none. A resample draws two groups: both the first (share 1) with chance 1/4, one of each
// (share 1/2) with chance 1/2, both the second (share 0) with chance 1/4. So the 2.5th
// percentile is 0 and the 97.5th is 1.
func TestBootstrapOfAKnownCase(t *testing.T) {
	groups := []string{"A", "A", "B", "B"}
	s := newShare(groups, []bool{true, true, false, false}, DefaultDraws, 7)
	if s.Hits != 2 || s.Groups != 2 || !near(*s.Share, 0.5) {
		t.Fatalf("share = %+v", s)
	}
	if s.CI95.Method != MethodBootstrap || s.CI95.Low != 0 || s.CI95.High != 1 {
		t.Errorf("interval = %+v, want [0, 1] by the bootstrap", *s.CI95)
	}
	// Every group alike: every resample has the one share.
	same := newShare([]string{"A", "A", "B", "B", "C", "C"}, []bool{true, false, true, false, true, false}, DefaultDraws, 7)
	if !near(same.CI95.Low, 0.5) || !near(same.CI95.High, 0.5) {
		t.Errorf("groups alike: interval = %+v, want [0.5, 0.5]", *same.CI95)
	}
	// The groups, not the cases, are resampled: one group of 99 hits and one of 1 miss is two
	// draws' worth of evidence, not a hundred cases'.
	var big []string
	var hits []bool
	for range 99 {
		big, hits = append(big, "A"), append(hits, true)
	}
	big, hits = append(big, "B"), append(hits, false)
	clustered := newShare(big, hits, DefaultDraws, 7)
	if clustered.CI95.Low != 0 || clustered.CI95.High != 1 {
		t.Errorf("two unequal groups: interval = %+v, want [0, 1]", *clustered.CI95)
	}
}

func TestBootstrapIsSeeded(t *testing.T) {
	var groups []string
	var values []float64
	for i := range 60 {
		groups = append(groups, fmt.Sprintf("G%d", i%20))
		values = append(values, float64(i%7))
	}
	a := newMean(groups, values, 2000, 1)
	b := newMean(groups, values, 2000, 1)
	c := newMean(groups, values, 2000, 2)
	if *a.CI95 != *b.CI95 {
		t.Errorf("one seed gave %+v and %+v", *a.CI95, *b.CI95)
	}
	if *a.CI95 == *c.CI95 {
		t.Errorf("two seeds gave one interval %+v", *a.CI95)
	}
	if a.CI95.Low > *a.Mean || a.CI95.High < *a.Mean {
		t.Errorf("the mean %v is outside its interval %+v", *a.Mean, *a.CI95)
	}
}

// TestTheExactBoundAtNoFailure: with no failure in 59 groups, the bound is Clopper-Pearson's
// with each group one trial: 0.05^(1/59), so the failure rate's bound is 1 − 0.05^(1/59) ≈ 3/59,
// the rule of three.
func TestTheExactBoundAtNoFailure(t *testing.T) {
	var groups []string
	var hits []bool
	for i := range 59 {
		// Two cases in each group: the bound counts groups, not cases.
		groups = append(groups, fmt.Sprintf("G%d", i), fmt.Sprintf("G%d", i))
		hits = append(hits, true, true)
	}
	s := newShare(groups, hits, DefaultDraws, 7)
	want := math.Pow(0.05, 1.0/59)
	if s.CI95.Method != MethodExact || !near(s.CI95.Low, want) || s.CI95.High != 1 || s.Groups != 59 {
		t.Errorf("no failure in 59 groups: %+v, want [%v, 1] exact", *s.CI95, want)
	}
	if math.Abs((1-s.CI95.Low)-0.04950) > 1e-4 {
		t.Errorf("1 − low = %v, want about 0.0495", 1-s.CI95.Low)
	}
	none := newShare(groups, make([]bool, len(groups)), DefaultDraws, 7)
	if none.CI95.Method != MethodExact || none.CI95.Low != 0 || !near(none.CI95.High, 1-want) {
		t.Errorf("no success in 59 groups: %+v, want [0, %v] exact", *none.CI95, 1-want)
	}
	if exactBound(0) != 1 {
		t.Error("no group bounds nothing")
	}
	if s := newShare(nil, nil, DefaultDraws, 7); s.Share != nil || s.CI95 != nil {
		t.Errorf("no case: %+v", s)
	}
}
