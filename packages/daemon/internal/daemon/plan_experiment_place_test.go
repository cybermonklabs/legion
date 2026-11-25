//go:build e2e

package daemon

import "testing"

// variantPlace reads `dispatch steps` as this release prints it: the line of each task of the
// issue's plan, `task <task> needs <needs>: …`, stands before that task's runs and is no run, so it
// passes over that line as parseSteps does (step_records_live_test.go). The variant's line must
// still stand right after the header, and the header's count must still be the runs below it. The
// fault is a variantPlace that takes the task line for a run, which fails the first row.
func TestVariantPlacePassesOverTheLineOfAPlansTask(t *testing.T) {
	const (
		run     = "default 1 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed"
		variant = "Experiment planning: variant b."
		task    = "task default needs []: 1 implementer passed"
	)
	for _, row := range []struct {
		name, printed string
		ok            bool
	}{
		{"the task line below the variant", "LEG-1: 1 steps in 1 tasks\n" + variant + "\n" + task + "\n" + run + "\n", true},
		{"the variant below the task line", "LEG-1: 1 steps in 1 tasks\n" + task + "\n" + variant + "\n" + run + "\n", false},
		{"a header that counts the task line as a run", "LEG-1: 2 steps in 1 tasks\n" + variant + "\n" + task + "\n" + run + "\n", false},
		{"a line that is neither a run nor a task line", "LEG-1: 1 steps in 1 tasks\n" + variant + "\ntask default 1 implementer\n" + run + "\n", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := variantPlace("LEG-1", variant, row.printed); (err == nil) != row.ok {
				t.Fatalf("variantPlace of\n%s= %v, want it to hold: %v", row.printed, err, row.ok)
			}
		})
	}
}
