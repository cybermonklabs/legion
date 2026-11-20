package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// R8: "A result at one commit also counts at another commit when no code that the check runs
// changed between the two." A fact against a check at a commit counts wherever the results of that
// commit count, as it counts at the commit itself: a leg that ended with no result (R12: "Each leg
// of a matrix reports its own result, and no leg replaces the result of another"; R7: "silence is
// not a pass"), a run of the job that ended with no result (R9: "When a check has no result that
// counts after a run of its job ends, it reads unknown, and Dispatch reports a fault of CI"), a
// refusal of CI (R48: "At that commit, such a check reads unknown, and no earlier result of it
// counts", so no result of it there also counts elsewhere), and a failing example that missed its
// fault (R9). A fact at a commit whose results do not count at the subject, such as c4, whose
// listener code differs, counts nowhere else. A leg with no result that counts, whose run has not
// ended, is no silent leg and no pass: it reads unknown (no_result) until it reports, and a run of
// the job that has not ended is no fault of CI; a leg that has a result that counts reads that
// result. An older result of a leg that counts answers a later run of that leg that ended with no
// result (R8: "So an unrelated merge leaves a check true"), and a result of that run that says it
// could not run is its answer (R7: "Of several results that count, the latest counts").
// TestAcceptASilentLegIsNotHiddenWhereItsResultsCarry holds the silent leg. Each row: carry, with a
// subject at c1 itself, whose spec files are m2's, and the row's change; the check reads the row's
// line at c1, and at c2 and at m2, the head of main, where the results of c1 carry.
func TestAFactAgainstACheckCountsWhereTheResultsOfItsCommitCount(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	for _, row := range []struct {
		name   string
		change func(results []map[string]any, jobs []any, uncited map[string]any) ([]map[string]any, []any)
		want   string // the line of the check at c1, c2 and m2
	}{
		{"control: carry's records read true at c1 and where they carry", func(r []map[string]any, j []any, _ map[string]any) ([]map[string]any, []any) {
			return r, j
		}, "true (passed, counted from c1)"},
		{"CI refused the check at c1", func(r []map[string]any, j []any, uncited map[string]any) ([]map[string]any, []any) {
			uncited["c1"] = map[string]any{check: "test-not-in-code"}
			return r, j
		}, "unknown (uncited)"},
		{"control: CI refused the check at c4, whose results do not count at c1, c2 or m2", func(r []map[string]any, j []any, uncited map[string]any) ([]map[string]any, []any) {
			uncited["c4"] = map[string]any{check: "test-not-in-code"}
			return append(r, withMembers(r[0], map[string]any{"commit": "c4", "at": "2026-11-13T11:00:00Z", "seq": 5})), j
		}, "true (passed, counted from c1)"},
		{"the job ended at c1 with no result of the check", func(r []map[string]any, j []any, _ map[string]any) ([]map[string]any, []any) {
			return r[1:], j // carry's pass at c1 taken out: the job's run there ended, and c3's pass does not carry
		}, "unknown (ci_fault)"},
		{"a failing example at c1 that missed its fault, after the caught one", func(r []map[string]any, j []any, _ map[string]any) ([]map[string]any, []any) {
			missed := map[string]any{}
			for k, v := range r[1] {
				missed[k] = v
			}
			missed["outcome"], missed["at"], missed["seq"] = "pass", "2026-11-12T10:06:00Z", 5
			return append(r, missed), j
		}, "unknown (failing_example_missed)"},
		{"leg linux passed at c1, and the run of leg arm there has not ended", func(r []map[string]any, _ []any, _ map[string]any) ([]map[string]any, []any) {
			r[0]["leg"] = "linux"
			return r, []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": false},
			}
		}, "unknown (no_result)"},
		{"no run of the check counts, and the job's run at c1 has not ended", func(r []map[string]any, _ []any, _ map[string]any) ([]map[string]any, []any) {
			return []map[string]any{r[1], r[3]}, []any{map[string]any{"job": "go-checks", "leg": "", "commit": "c1", "ended": false}}
		}, "unknown (no_result)"},
		{"leg linux passed at c1, the run of leg arm there ended with no result, and a run of leg x86 there has not ended", func(r []map[string]any, _ []any, _ map[string]any) ([]map[string]any, []any) {
			r[0]["leg"] = "linux"
			return r, []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "x86", "commit": "c1", "ended": false},
			}
		}, "unknown (ci_fault)"},
		{"a pass at c1, and a later run of the job at c2 that ended with no result", func(r []map[string]any, j []any, _ map[string]any) ([]map[string]any, []any) {
			return r, append(j, map[string]any{"job": "go-checks", "leg": "", "commit": "c2", "ended": true})
		}, "true (passed, counted from c1)"},
		{"the same, and the run at c2 reports that it could not run", func(r []map[string]any, j []any, _ map[string]any) ([]map[string]any, []any) {
			cannot := withMembers(r[0], map[string]any{"commit": "c2", "outcome": "could_not_run", "at": "2026-11-13T09:00:00Z", "seq": 5})
			return append(r, cannot), append(j, map[string]any{"job": "go-checks", "leg": "", "commit": "c2", "ended": true})
		}, "unknown (could_not_run)"},
		{"control: leg linux and leg arm passed at c1, and a run of leg arm there has not ended", func(r []map[string]any, _ []any, _ map[string]any) ([]map[string]any, []any) {
			r[0]["leg"] = "linux"
			arm := map[string]any{}
			for k, v := range r[0] {
				arm[k] = v
			}
			arm["leg"], arm["seq"] = "arm", 5
			return append(r, arm), []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": false},
			}
		}, "true (passed, counted from c1)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			if err := os.CopyFS(filepath.Join(dir, "files", "c1"), os.DirFS(filepath.Join(dir, "files", "m2"))); err != nil {
				t.Fatal(err)
			}
			var subjects []any
			readJSONFile(t, filepath.Join(dir, "subjects.json"), &subjects)
			writeJSONFile(t, filepath.Join(dir, "subjects.json"), append([]any{map[string]any{"commit": "c1", "branch": "c1-head", "now": "2026-11-14T00:00:00Z"}}, subjects...))
			var results []map[string]any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			var jobs []any
			readJSONFile(t, filepath.Join(dir, "jobs.json"), &jobs)
			uncited := map[string]any{}
			results, jobs = row.change(results, jobs, uncited)
			writeJSONFile(t, filepath.Join(dir, "results.json"), results)
			writeJSONFile(t, filepath.Join(dir, "jobs.json"), jobs)
			writeJSONFile(t, filepath.Join(dir, "uncited.json"), uncited)
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
			}
			for _, subject := range []string{"c1, the head of c1-head", "c2, the head of listener-restart", "m2, the head of main"} {
				want := "  check " + check + ": " + row.want
				if lines := subjectLines(got.stdout, subject); !slices.Contains(lines, want) {
					t.Errorf("at subject %s: %q, want the line %q", subject, lines, want)
				}
			}
		})
	}
}

// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of another."
// One job at one commit has legs or it has none. In silent-leg the job had legs at c1, whose run
// results are of leg linux, and here it runs at m2 with no matrix, in one run of leg "" that ended
// with no result: no result of c1 answers that run, so the check reads unknown (ci_fault) at m2,
// where the results of c1 count (the control; R9: "When a check has no result that counts after a
// run of its job ends, it reads unknown"). A run result of leg "" at c1 beside those of leg linux
// would answer it, and the check would read true (passed, counted from c1), so the reader refuses
// the two forms, with a line that names the file, the commit, the job and the two legs.
func TestARunResultOfNoMatrixAnswersNoRunWhereItsJobHadLegs(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	dir := copyScenario(t, "silent-leg")
	writeJSONFile(t, filepath.Join(dir, "jobs.json"), []any{map[string]any{"job": "go-checks", "leg": "", "commit": "m2", "ended": true}})
	got := runBinary(t, binary, "", "state", "--scenario", dir)
	want := "  check a-restart-loses-no-message: unknown (ci_fault)"
	if lines := subjectLines(got.stdout, "m2, the head of main"); got.code != 0 || got.stderr != "" || !slices.Contains(lines, want) {
		t.Fatalf("control: exit %d, stderr %q, at m2 %q; want exit 0 and the line %q", got.code, got.stderr, lines, want)
	}
	var results []map[string]any
	readJSONFile(t, filepath.Join(dir, "results.json"), &results)
	if results[0]["kind"] != "run" || results[0]["commit"] != "c1" || results[2]["kind"] != "run" || results[2]["leg"] != "linux" {
		t.Fatalf("silent-leg's results 1 and 3 are no runs of leg linux at c1: %v", results[:3])
	}
	results[0]["leg"] = ""
	writeJSONFile(t, filepath.Join(dir, "results.json"), results)
	expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") +
		`: result 3: it sets the leg "linux", and result 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix` + "\n"})
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts"; R7: "Of
// several results that count, the latest counts"; R9: "a check whose current text has no failing
// example that failed when it should" reads unknown. A refusal of CI cancels each piece of evidence
// of the check at its commit and earlier, the runs, the results and the failing examples, and no
// newer one. The refusal carries no time, so "earlier" is the order of record of the results of its
// commit: a result recorded no later than the newest of them is cancelled. A refusal at a commit
// that holds no result of the check counts at that commit alone, as the subject. Each row: carry,
// with the row's records; the check reads the row's line at c2, where c1's and m2's results carry
// (R8), and at m2, the head of main. m2's pass and the run of the job that ended at m2 are the
// reviewer's case; c1's results are recorded on 12 November.
func TestARefusalOfCICancelsTheEvidenceOfItsCommitAndOlder(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	refused := map[string]any{"c1": map[string]any{check: "test-did-not-run"}}
	// at m2 is m2's pass recorded at its time, and the run of the job that ended at m2; fault adds a
	// caught failing example at commit, recorded at its time.
	at := func(passAt string) func(r []map[string]any) []map[string]any {
		return func(r []map[string]any) []map[string]any {
			return append(r, withMembers(r[0], map[string]any{"commit": "m2", "at": passAt, "seq": 6}))
		}
	}
	fault := func(commit, faultAt string) func(r []map[string]any) []map[string]any {
		return func(r []map[string]any) []map[string]any {
			return append(r, withMembers(r[1], map[string]any{"commit": commit, "at": faultAt, "seq": 7}))
		}
	}
	for _, row := range []struct {
		name       string
		changes    []func(r []map[string]any) []map[string]any
		uncited    map[string]any
		c2, m2     string // the line of the check at c2 and at m2
		noM2JobRun bool
	}{
		{name: "control: m2's pass and no refusal", changes: []func(r []map[string]any) []map[string]any{at("2026-11-13T12:00:00Z")}, uncited: map[string]any{},
			c2: "true (passed, counted from m2)", m2: "true (passed, counted from m2)"},
		{name: "CI refused the check at c1: its failing example is cancelled with its pass, and m2's pass has none", changes: []func(r []map[string]any) []map[string]any{at("2026-11-13T12:00:00Z")},
			uncited: refused, c2: "unknown (no_failing_example)", m2: "unknown (no_failing_example)"},
		{name: "the same, and a caught failing example at m2", changes: []func(r []map[string]any) []map[string]any{at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z")},
			uncited: refused, c2: "true (passed, counted from m2)", m2: "true (passed, counted from m2)"},
		{name: "the same, and a caught failing example at c2, newer than c1's results", changes: []func(r []map[string]any) []map[string]any{at("2026-11-13T12:00:00Z"), fault("c2", "2026-11-13T11:00:00Z")},
			uncited: refused, c2: "true (passed, counted from m2)", m2: "true (passed, counted from m2)"},
		{name: "m2's pass and its caught failing example recorded before c1's results", changes: []func(r []map[string]any) []map[string]any{at("2026-11-12T09:00:00Z"), fault("m2", "2026-11-12T09:05:00Z")},
			uncited: refused, c2: "unknown (uncited)", m2: "unknown (uncited)"},
		{name: "control: the same with no refusal", changes: []func(r []map[string]any) []map[string]any{at("2026-11-12T09:00:00Z"), fault("m2", "2026-11-12T09:05:00Z")},
			uncited: map[string]any{}, c2: "true (passed, counted from c1)", m2: "true (passed, counted from c1)"},
		// The refusal's place is the newest record of c1, its failing example at 10:05, not its pass at
		// 10:00: m2's pass at 10:02 is older, so it is cancelled, and m2's later failing example alone
		// counts no run.
		{name: "m2's pass recorded between c1's pass and c1's failing example", changes: []func(r []map[string]any) []map[string]any{at("2026-11-12T10:02:00Z"), fault("m2", "2026-11-13T12:05:00Z")},
			uncited: refused, c2: "unknown (uncited)", m2: "unknown (uncited)"},
		{name: "CI refused the check at c2, which holds no result and no run: the refusal counts at c2 alone", noM2JobRun: true,
			uncited: map[string]any{"c2": map[string]any{check: "test-not-in-code"}}, c2: "unknown (uncited)", m2: "true (passed, counted from c1)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := copyScenario(t, "carry")
			var results []map[string]any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			for _, change := range row.changes {
				results = change(results)
			}
			var jobs []any
			readJSONFile(t, filepath.Join(dir, "jobs.json"), &jobs)
			if !row.noM2JobRun {
				jobs = append(jobs, map[string]any{"job": "go-checks", "leg": "", "commit": "m2", "ended": true})
			}
			writeJSONFile(t, filepath.Join(dir, "results.json"), results)
			writeJSONFile(t, filepath.Join(dir, "jobs.json"), jobs)
			writeJSONFile(t, filepath.Join(dir, "uncited.json"), row.uncited)
			got := runBinary(t, binary, "", "state", "--scenario", dir)
			if got.code != 0 || got.stderr != "" {
				t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
			}
			for subject, want := range map[string]string{"c2, the head of listener-restart": row.c2, "m2, the head of main": row.m2} {
				if lines := subjectLines(got.stdout, subject); !slices.Contains(lines, "  check "+check+": "+want) {
					t.Errorf("at subject %s: %q, want the line %q", subject, lines, "  check "+check+": "+want)
				}
			}
		})
	}
}

// withMembers is a copy of the record r with the members of set set to their values.
func withMembers(r, set map[string]any) map[string]any {
	c := maps.Clone(r)
	maps.Copy(c, set)
	return c
}
