package main

// Red test of the fresh acceptance of #4191 at 7956f421eb, a file of the branch whose body is as its
// tester wrote it. R7: "Of several results that count, the latest counts, and a later failure
// cancels an earlier pass." The order of record is at, then seq. At 7956f421eb the reader refused a
// result with no at, and two results of one check at one at and one seq, but took a result with no
// seq as seq 0: a failure at the at of a pass, recorded with no seq, sorted before the pass, so a
// check of production read true with a failure on record at its deploy. The reader now refuses a
// result with no seq, and the first row takes that refusal.

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

func TestAcceptAFailureWithNoSeqIsNeverHidden(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "production-delivery-takes-under-a-second"
	for _, row := range []struct {
		name  string
		seq   any // nil: the member is absent
		check func(t *testing.T, got result)
	}{
		{"a failure at deploy d1, at the at of the pass, with no seq", nil, func(t *testing.T, got result) {
			lines := strings.Split(got.stdout, "\n")
			if got.code == 0 && slices.Contains(lines, "  check "+check+": true (passed, counted from d1)") {
				t.Errorf("a failure with no seq sorts before the pass, and the check reads true:\n%s", got.stdout)
			}
			if got.code == 2 && !strings.Contains(got.stderr, "results.json: result 6") {
				t.Errorf("the refusal names no result 6: %q", got.stderr)
			}
		}},
		{"control: the same failure with seq 6, after the pass, reads false", 6, func(t *testing.T, got result) {
			if got.code != 0 || !strings.Contains(got.stdout, "  check "+check+": false (failed, counted from d1)") {
				t.Errorf("exit %d, want false at d1:\n%s%s", got.code, got.stdout, got.stderr)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			for _, commit := range []string{"c2", "c3", "c4"} {
				removeAll(t, filepath.Join(dir, "files", commit))
			}
			writeJSONFile(t, filepath.Join(dir, "subjects.json"), []any{map[string]any{"commit": "m2", "deploy": "d1", "now": "2026-11-14T00:00:00Z"}})
			var results []any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			fault := map[string]any{"check": check, "check_version": 1, "kind": "failing_example", "outcome": "fail", "leg": "", "at": "2026-11-12T11:00:00Z", "seq": 5}
			run := map[string]any{"check": check, "check_version": 1, "kind": "run", "outcome": "fail", "deploy": "d1", "leg": "", "at": "2026-11-12T12:00:00Z"}
			if row.seq != nil {
				run["seq"] = row.seq
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), append(results, fault, run))
			row.check(t, runBinary(t, binary, "", "state", "--scenario", dir))
		})
	}
}
