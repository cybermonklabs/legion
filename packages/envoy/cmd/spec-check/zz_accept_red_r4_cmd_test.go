package main

// Red test of the fresh acceptance of #4191 at 286049fa73, a file of the branch whose body is as its
// tester wrote it. R7: "A pass and a failure on one such subject and one procedure make the state
// unknown", and "a later failure cancels an earlier pass". At 286049fa73 the scenario reader took a
// result with no "at" as the zero time, and the derivation read that failure as older than the
// approval of its statement and dropped it, so the check read true beside a failure at the same
// commit. The reader now refuses a result with no at, and the first row takes that refusal.

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

func TestAcceptAFailingRunWithNoTimeIsNeverHidden(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	for _, row := range []struct {
		name  string
		at    any // nil: the member is absent
		check func(t *testing.T, got result)
	}{
		{"a failure at c1 with no at", nil, func(t *testing.T, got result) {
			lines := strings.Split(got.stdout, "\n")
			if got.code == 0 && slices.Contains(lines, "  check a-restart-loses-no-message: true (passed, counted from c1)") {
				t.Errorf("a failing run at c1 with no time is dropped, and the check reads true:\n%s", got.stdout)
			}
			if got.code == 2 && !strings.Contains(got.stderr, "results.json: result 5") {
				t.Errorf("the refusal names no result 5: %q", got.stderr)
			}
		}},
		{"control: the same failure at 2026-11-12T11:00:00Z reads unknown (disagree)", "2026-11-12T11:00:00Z", func(t *testing.T, got result) {
			if got.code != 0 || !strings.Contains(got.stdout, "  check a-restart-loses-no-message: unknown (disagree)") {
				t.Errorf("exit %d, want unknown (disagree) at c2:\n%s%s", got.code, got.stdout, got.stderr)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			var results []any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			run := map[string]any{"check": "a-restart-loses-no-message", "check_version": 1, "kind": "run", "outcome": "fail",
				"commit": "c1", "job": "go-checks", "leg": "", "attempt": 2, "seq": 5}
			if row.at != nil {
				run["at"] = row.at
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), append(results, run))
			row.check(t, runBinary(t, binary, "", "state", "--scenario", dir))
		})
	}
}
