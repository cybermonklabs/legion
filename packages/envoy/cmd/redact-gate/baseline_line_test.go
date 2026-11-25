package main

import (
	"strings"
	"testing"
	"time"
)

// TestTheBaselineLineSaysEachKindOfRowsBound: stage 4's PASS line says what bounds each kind of
// row it ran, as its run takes it: a Go row's run row_timeout plus four times its start-up run
// (runBound), a Bun row's row_timeout alone (runBun), and names no kind it did not run.
func TestTheBaselineLineSaysEachKindOfRowsBound(t *testing.T) {
	const goBound = "each Go row's run is bounded by row_timeout, 48s, plus four times its start-up run"
	const bunBound = "each Bun row's run is bounded by row_timeout, 48s, alone"
	goRow, bunRow := &row{ID: "go-row"}, &row{ID: "bun-row", bun: true}
	for _, c := range []struct {
		name          string
		gated         []*row
		want, without []string
	}{
		{"both", []*row{goRow, bunRow}, []string{"2 rows' tests pass unmutated (1 Go, 1 Bun)", goBound, bunBound}, nil},
		{"go", []*row{goRow}, []string{"(1 Go, 0 Bun)", goBound}, []string{"Bun row's"}},
		{"bun", []*row{bunRow}, []string{"(0 Go, 1 Bun)", bunBound}, []string{"Go row's", "start-up"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			line := baselinePassed(c.gated, 3*time.Second, "go-row", 48*time.Second)
			if !strings.HasPrefix(line, "PASS stage 4 (baseline): ") {
				t.Errorf("the line %q is not stage 4's PASS line", line)
			}
			for _, want := range c.want {
				if !strings.Contains(line, want) {
					t.Errorf("the line %q; want %q in it", line, want)
				}
			}
			for _, not := range c.without {
				if strings.Contains(line, not) {
					t.Errorf("the line %q names %q, a kind of row the stage did not run", line, not)
				}
			}
		})
	}
}
