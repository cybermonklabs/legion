package main

// The order of the results that state reads (R7: "Of several results that count, the latest
// counts, and a later failure cancels an earlier pass"): Dispatch's record time, by its instant, then
// its order of record, seq. How each result then reads is a rule of the library (specgraph's
// TestCheckState); these tests read the form of the two members.

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// A time of the JSON files is RFC 3339 with an upper-case T, and Z or an offset, and the reader
// orders it by its instant; the zero instant of year 1, at any offset, is no time. Each row: carry
// at m2, the commit that production runs at deploy d1, with a caught failing example of its check
// of production and one more failing run at d1, recorded after its pass with the next seq, at the
// row's at. Its pass is at 12:00:00Z, so 13:00:01+01:00 is one second after it and 12:59:59+01:00 one
// second before it, whatever their text.
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
			fault := map[string]any{"check": check, "check_version": 1, "kind": "failing_example", "outcome": "fail", "leg": "", "at": "2026-11-12T11:00:00Z", "seq": len(results) + 1}
			run := map[string]any{"check": check, "check_version": 1, "kind": "run", "outcome": "fail", "deploy": "d1", "leg": "", "at": row.at, "seq": len(results) + 2}
			writeJSONFile(t, filepath.Join(dir, "results.json"), append(results, fault, run))
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if row.want == "" {
				expect(t, got, result{code: 2, stderr: fmt.Sprintf("spec-check: %s: result %d: it sets no at\n", filepath.Join(dir, "results.json"), len(results)+2)})
				return
			}
			if got.code != 0 || !slices.Contains(strings.Split(got.stdout, "\n"), row.want) {
				t.Errorf("exit %d, no line %q in:\n%s%s", got.code, row.want, got.stdout, got.stderr)
			}
		})
	}
}

// A result names its place in the order of record by its at and its seq, and the reader refuses one
// that leaves either out, or that sets seq 0: it would read as the zero time or as seq 0, and sort
// before each record of its time, so a failure would hide behind the pass that it cancels, or a
// failing example that missed its fault behind one that caught it (R9: "Six cases read as unknown,
// and never as true"). Each row adds its records to carry's results.json, with the member left out of
// the last, or set to the row's value, and the reader refuses the file with the line that names
// that result and the member; the control of each row writes the member as a writer sets it, and
// the scenario is read.
func TestTheReaderRefusesAResultThatTheOrderOfRecordCannotPlace(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const code, production = "a-restart-loses-no-message", "production-delivery-takes-under-a-second"
	missed := func() map[string]any {
		return map[string]any{"check": code, "check_version": 1, "kind": "failing_example", "outcome": "pass", "commit": "f2", "planted_on": "c1",
			"fault_paths": []string{"packages/envoy/cmd/listener/main.go"}, "job": "go-checks", "leg": "", "attempt": 1, "at": "2026-11-12T10:05:00Z",
			"fault_author": "agent-b", "test_authors": []string{"agent-a"}}
	}
	for _, row := range []struct {
		name      string
		records   func() []map[string]any // the records that the row adds, the last of them without the member
		member    string
		bad, good any // the member's value in the row, nil to leave it out, and in the control
		fault     string
	}{
		{"a failing run of the check of the code at c1 with no at, beside its pass there", func() []map[string]any {
			return []map[string]any{{"check": code, "check_version": 1, "kind": "run", "outcome": "fail", "commit": "c1", "job": "go-checks", "leg": "", "attempt": 2, "seq": 101}}
		}, "at", nil, "2026-11-12T11:00:00Z", "it sets no at"},
		{"a failure of the check of production at deploy d1, at the at of its pass, with no seq", func() []map[string]any {
			return []map[string]any{
				{"check": production, "check_version": 1, "kind": "failing_example", "outcome": "fail", "leg": "", "at": "2026-11-12T11:00:00Z", "seq": 101},
				{"check": production, "check_version": 1, "kind": "run", "outcome": "fail", "deploy": "d1", "leg": "", "at": "2026-11-12T12:00:00Z"},
			}
		}, "seq", nil, 102, "it sets no seq"},
		{"a failing example that missed its fault, at the at of the caught one, with no seq", func() []map[string]any {
			return []map[string]any{missed()}
		}, "seq", nil, 101, "it sets no seq"},
		{"a failing example that missed its fault, at the at of the caught one, with seq 0", func() []map[string]any {
			return []map[string]any{missed()}
		}, "seq", 0, 101, "it sets no seq"},
	} {
		for _, control := range []bool{false, true} {
			name := row.name
			if control {
				name = fmt.Sprintf("control: %s, with %s %v", row.name, row.member, row.good)
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				dir := copyScenario(t, "carry")
				var results []any
				readJSONFile(t, filepath.Join(dir, "results.json"), &results)
				records := row.records()
				last := records[len(records)-1]
				switch {
				case control:
					last[row.member] = row.good
				case row.bad != nil:
					last[row.member] = row.bad
				}
				for _, r := range records {
					results = append(results, r)
				}
				writeJSONFile(t, filepath.Join(dir, "results.json"), results)
				got := runBinary(t, binary, "", "state", "--scenario", dir)
				if control {
					if got.code != 0 || got.stderr != "" {
						t.Errorf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
					}
					return
				}
				expect(t, got, result{code: 2, stderr: fmt.Sprintf("spec-check: %s: result %d: %s\n", filepath.Join(dir, "results.json"), len(results), row.fault)})
			})
		}
	}
}
