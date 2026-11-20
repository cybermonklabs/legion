package main

// The order of the results that state reads (R7: "Of several results that count, the latest
// counts, and a later failure cancels an earlier pass"): Dispatch's record time, by its instant, then
// its order of record, seq.

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// A time of the JSON files is RFC 3339 with an upper-case T, and Z or an offset, and the reader
// orders it by its instant; the zero instant of year 1, at any offset, is no time. Each row: carry
// at m2, the commit that production runs at deploy d1, with a caught failing example of its check
// of production and one more failing run at d1, recorded after its pass (seq 6), at the row's at.
// Its pass is at 12:00:00Z, so 13:00:01+01:00 is one second after it and 12:59:59+01:00 one second
// before it, whatever their text.
func TestATimeIsReadByItsInstant(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "production-delivery-takes-under-a-second"
	for _, row := range []struct {
		name, at, want string
	}{
		{"a failure one second after the pass, at +01:00", "2026-11-12T13:00:01+01:00", "  check " + check + ": false (failed, counted from d1)"},
		{"a failure one second before the pass, at +01:00", "2026-11-12T12:59:59+01:00", "  check " + check + ": true (passed, counted from d1)"},
		{"a failure at the zero instant, at +01:00", "0001-01-01T01:00:00+01:00", ""},
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
			run := map[string]any{"check": check, "check_version": 1, "kind": "run", "outcome": "fail", "deploy": "d1", "leg": "", "at": row.at, "seq": 6}
			writeJSONFile(t, filepath.Join(dir, "results.json"), append(results, fault, run))
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if row.want == "" {
				expect(t, got, result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") + ": result 6: it sets no at\n"})
				return
			}
			if got.code != 0 || !slices.Contains(strings.Split(got.stdout, "\n"), row.want) {
				t.Errorf("exit %d, no line %q in:\n%s%s", got.code, row.want, got.stdout, got.stderr)
			}
		})
	}
}

// R9: "Six cases read as unknown, and never as true. … And a check whose current text has no
// failing example that failed when it should." R7: "Of several results that count, the latest
// counts." The order of record is at, then seq. Each row: carry, with one more failing example of
// its code check at c1, on which the test missed the fault, at the at of carry's caught failing
// example, with the row's seq. With no seq, or seq 0, the record would sort before the caught one,
// and the check would read true at m2, the head of main: the reader refuses it. With seq 5 it is
// the later record, and the check reads unknown there. TestAcceptAFailureWithNoSeqIsNeverHidden is
// the same for a failure of production.
func TestAMissedFailingExampleWithNoSeqIsNeverHidden(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	for _, row := range []struct {
		name string
		seq  any // nil: the member is absent
		want []string
	}{
		{"no seq", nil, nil},
		{"seq 0", 0, nil},
		{"control: seq 5, after the caught failing example", 5, []string{
			"statement a-stored-message-survives-a-restart: unknown (check a-restart-loses-no-message: failing_example_missed)",
			"  check a-restart-loses-no-message: unknown (failing_example_missed)"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			var results []any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			missed := map[string]any{"check": "a-restart-loses-no-message", "check_version": 1, "kind": "failing_example", "outcome": "pass",
				"commit": "c1", "job": "go-checks", "leg": "", "attempt": 1, "at": "2026-11-12T10:05:00Z", "fault_author": "agent-b", "test_authors": []string{"agent-a"}}
			if row.seq != nil {
				missed["seq"] = row.seq
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), append(results, missed))
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if row.want == nil {
				expect(t, got, result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") + ": result 5: it sets no seq\n"})
				return
			}
			lines := subjectLines(got.stdout, "m2, the head of main")
			for _, want := range row.want {
				if got.code != 0 || !slices.Contains(lines, want) {
					t.Errorf("exit %d, no line %q at m2 in:\n%s%s", got.code, want, got.stdout, got.stderr)
				}
			}
		})
	}
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." Of the
// refusals of a check that count at a subject, the state names one rule, and the order of
// results.json, which says nothing, does not choose it: the rule is the least by its name. Each
// order: carry, with a pass of its code check at c2 after c1's, and CI's refusal of the check at c1
// (test-not-in-code) and at c2 (test-did-not-run), both commits whose results count at c2 and at m2;
// results.json as carry writes it, and in the reverse order, each record with its own seq. Both
// print the same bytes in text and in --json, where the state names test-did-not-run.
func TestTheRuleOfARefusalIsTheSameInEachOrderOfTheResults(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	outputs := map[string][2]string{}
	for _, order := range []string{"as written", "reversed"} {
		dir := copyScenario(t, "carry")
		var results []map[string]any
		readJSONFile(t, filepath.Join(dir, "results.json"), &results)
		pass := map[string]any{}
		for k, v := range results[0] {
			pass[k] = v
		}
		pass["commit"], pass["at"], pass["seq"] = "c2", "2026-11-13T09:00:00Z", 5
		results = append(results, pass)
		if order == "reversed" {
			slices.Reverse(results)
		}
		writeJSONFile(t, filepath.Join(dir, "results.json"), results)
		writeJSONFile(t, filepath.Join(dir, "uncited.json"), map[string]any{
			"c1": map[string]any{check: "test-not-in-code"}, "c2": map[string]any{check: "test-did-not-run"}})
		text := runBinary(t, binary, "", "state", "--scenario", dir)
		asJSON := runBinary(t, binary, "", "state", "--scenario", dir, "--json")
		if text.code != 0 || asJSON.code != 0 || text.stderr+asJSON.stderr != "" {
			t.Fatalf("%s: exit %d and %d, stderr %q: want exit 0 and nothing on stderr", order, text.code, asJSON.code, text.stderr+asJSON.stderr)
		}
		for _, subject := range []string{"c2, the head of listener-restart", "m2, the head of main"} {
			if want := "  check " + check + ": unknown (uncited)"; !slices.Contains(subjectLines(text.stdout, subject), want) {
				t.Errorf("%s, at subject %s: no line %q in:\n%s", order, subject, want, text.stdout)
			}
		}
		if want := `"reason":"uncited","detail":"test-did-not-run"`; strings.Count(asJSON.stdout, want) != 2 {
			t.Errorf("%s: --json names the rule %q %d times, want 2 (at c2 and at m2):\n%s", order, want, strings.Count(asJSON.stdout, want), asJSON.stdout)
		}
		outputs[order] = [2]string{text.stdout, asJSON.stdout}
	}
	if outputs["as written"] != outputs["reversed"] {
		t.Errorf("the output depends on the order of results.json:\nas written:\n%s\nreversed:\n%s", outputs["as written"], outputs["reversed"])
	}
}
