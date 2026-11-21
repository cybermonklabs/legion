package main

// Red test of the acceptance of #4191 at b4a44ca71b, a file of the branch whose body is as its
// tester wrote it. R12: "Each leg of a matrix reports its own result, and no leg replaces the
// result of another." R7: "A step that left no result reads as unknown: silence is not a pass."
// R9: "When a check has no result that counts after a run of its job ends, it reads unknown, and
// Dispatch reports a fault of CI." At b4a44ca71b the judge looked for a silent leg only among the
// job runs at the subject's own commit, while the runs that count come from every commit whose
// results carry (R8): a matrix whose arm leg ended at c1 with no result read unknown (ci_fault) at
// c1, and true at m2, where c1's linux pass carried and c1's silent arm did not. A silent leg now
// counts wherever the results of its commit count.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

func TestAcceptASilentLegIsNotHiddenWhereItsResultsCarry(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	// lineAt is the line of check under the subject commit, "" for none.
	lineAt := func(stdout, commit string) string {
		subject := ""
		for _, l := range strings.Split(stdout, "\n") {
			if rest, ok := strings.CutPrefix(l, "spec-check: subject "); ok {
				subject, _, _ = strings.Cut(rest, ",")
			} else if subject == commit && strings.HasPrefix(l, "  check "+check+": ") {
				return strings.TrimPrefix(l, "  check "+check+": ")
			}
		}
		return ""
	}
	for _, row := range []struct {
		name  string
		arm   string // "": the arm leg reports nothing at c1
		check func(t *testing.T, got result)
	}{
		{"the arm leg of go-checks ended at c1 with no result", "", func(t *testing.T, got result) {
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if at := lineAt(got.stdout, "c1"); at != "unknown (ci_fault)" {
				t.Fatalf("at c1 the check reads %q, want unknown (ci_fault): the input does not hold the silent leg\n%s", at, got.stdout)
			}
			for _, commit := range []string{"c2", "m2"} {
				if at := lineAt(got.stdout, commit); strings.HasPrefix(at, "true") {
					t.Errorf("at %s, where the results of c1 carry, the check reads %q: c1's linux pass replaces the result that its arm leg never gave", commit, at)
				}
			}
		}},
		{"control: the arm leg passes at c1 too, and m2 reads true", "pass", func(t *testing.T, got result) {
			if at := lineAt(got.stdout, "m2"); got.code != 0 || at != "true (passed, counted from c1)" {
				t.Errorf("exit %d, at m2 %q, want true (passed, counted from c1):\n%s%s", got.code, at, got.stdout, got.stderr)
			}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			if err := os.CopyFS(filepath.Join(dir, "files", "c1"), os.DirFS(filepath.Join(dir, "files", "m2"))); err != nil {
				t.Fatal(err)
			}
			var subjects []any
			readJSONFile(t, filepath.Join(dir, "subjects.json"), &subjects)
			subjects = append([]any{map[string]any{"commit": "c1", "branch": "c1-head", "now": "2026-11-14T00:00:00Z"}}, subjects...)
			writeJSONFile(t, filepath.Join(dir, "subjects.json"), subjects)
			var results []map[string]any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			results[0]["leg"] = "linux" // carry's pass of the check at c1
			if row.arm != "" {
				arm := map[string]any{}
				for k, v := range results[0] {
					arm[k] = v
				}
				arm["leg"], arm["outcome"], arm["seq"] = "arm", row.arm, 5
				results = append(results, arm)
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), results)
			writeJSONFile(t, filepath.Join(dir, "jobs.json"), []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "", "commit": "c3", "ended": true},
			})
			row.check(t, runBinary(t, binary, "", "state", "--scenario", dir))
		})
	}
}
