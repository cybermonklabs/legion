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
// counts after a run of its job ends, it reads unknown, and Dispatch reports a fault of CI"), and a
// failing example that missed its fault (R9). A refusal of CI cancels the results of its commit
// wherever they count, and no result of another commit
// (TestARefusalOfCICancelsTheResultsOfItsCommitAlone). A leg with no result that counts, whose run
// has not ended, is no silent leg and no pass: it reads unknown (no_result) until it reports, and a
// run of the job that has not ended is no fault of CI; a leg that has a result that counts reads
// that result. An older result of a leg that counts answers a later run of that leg that ended
// with no result (R8: "So an unrelated merge leaves a check true"), and a result of that run that
// says it could not run is its answer (R7: "Of several results that count, the latest counts").
// TestAcceptASilentLegIsNotHiddenWhereItsResultsCarry holds the silent leg. Each row: carry, with a
// subject at c1 itself, whose spec files are m2's, and the row's change; the check reads the row's
// line at c1, and at c2 and at m2, the head of main, where the results of c1 carry.
func TestAFactAgainstACheckCountsWhereTheResultsOfItsCommitCount(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	for _, row := range []struct {
		name   string
		change func(results []map[string]any, jobs []any) ([]map[string]any, []any)
		want   string // the line of the check at c1, c2 and m2
	}{
		{"control: carry's records read true at c1 and where they carry", func(r []map[string]any, j []any) ([]map[string]any, []any) {
			return r, j
		}, "true (passed, counted from c1)"},
		{"the job ended at c1 with no result of the check", func(r []map[string]any, j []any) ([]map[string]any, []any) {
			return r[1:], j // carry's pass at c1 taken out: the job's run there ended, and c3's pass does not carry
		}, "unknown (ci_fault)"},
		{"a failing example at c1 that missed its fault, after the caught one", func(r []map[string]any, j []any) ([]map[string]any, []any) {
			missed := map[string]any{}
			for k, v := range r[1] {
				missed[k] = v
			}
			missed["outcome"], missed["at"], missed["seq"] = "pass", "2026-11-12T10:06:00Z", 5
			return append(r, missed), j
		}, "unknown (failing_example_missed)"},
		{"leg linux passed at c1, and the run of leg arm there has not ended", func(r []map[string]any, _ []any) ([]map[string]any, []any) {
			r[0]["leg"] = "linux"
			return r, []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": false},
			}
		}, "unknown (no_result)"},
		{"no run of the check counts, and the job's run at c1 has not ended", func(r []map[string]any, _ []any) ([]map[string]any, []any) {
			return []map[string]any{r[1], r[3]}, []any{map[string]any{"job": "go-checks", "leg": "", "commit": "c1", "ended": false}}
		}, "unknown (no_result)"},
		{"leg linux passed at c1, the run of leg arm there ended with no result, and a run of leg x86 there has not ended", func(r []map[string]any, _ []any) ([]map[string]any, []any) {
			r[0]["leg"] = "linux"
			return r, []any{
				map[string]any{"job": "go-checks", "leg": "linux", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "arm", "commit": "c1", "ended": true},
				map[string]any{"job": "go-checks", "leg": "x86", "commit": "c1", "ended": false},
			}
		}, "unknown (ci_fault)"},
		{"a pass at c1, and a later run of the job at c2 that ended with no result", func(r []map[string]any, j []any) ([]map[string]any, []any) {
			return r, append(j, map[string]any{"job": "go-checks", "leg": "", "commit": "c2", "ended": true})
		}, "true (passed, counted from c1)"},
		{"the same, and the run at c2 reports that it could not run", func(r []map[string]any, j []any) ([]map[string]any, []any) {
			cannot := withMembers(r[0], map[string]any{"commit": "c2", "outcome": "could_not_run", "at": "2026-11-13T09:00:00Z", "seq": 5})
			return append(r, cannot), append(j, map[string]any{"job": "go-checks", "leg": "", "commit": "c2", "ended": true})
		}, "unknown (could_not_run)"},
		{"control: leg linux and leg arm passed at c1, and a run of leg arm there has not ended", func(r []map[string]any, _ []any) ([]map[string]any, []any) {
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
			withSubjectAtC1(t, dir)
			var results []map[string]any
			readJSONFile(t, filepath.Join(dir, "results.json"), &results)
			var jobs []any
			readJSONFile(t, filepath.Join(dir, "jobs.json"), &jobs)
			results, jobs = row.change(results, jobs)
			writeJSONFile(t, filepath.Join(dir, "results.json"), results)
			writeJSONFile(t, filepath.Join(dir, "jobs.json"), jobs)
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

// withSubjectAtC1 gives carry at dir a first subject at c1, the head of c1-head, whose spec files
// are m2's.
func withSubjectAtC1(t *testing.T, dir string) {
	t.Helper()
	if err := os.CopyFS(filepath.Join(dir, "files", "c1"), os.DirFS(filepath.Join(dir, "files", "m2"))); err != nil {
		t.Fatal(err)
	}
	var subjects []any
	readJSONFile(t, filepath.Join(dir, "subjects.json"), &subjects)
	writeJSONFile(t, filepath.Join(dir, "subjects.json"), append([]any{map[string]any{"commit": "c1", "branch": "c1-head", "now": "2026-11-14T00:00:00Z"}}, subjects...))
}

// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of another."
// One job at one commit has legs or it has none. In silent-leg the job had legs at c1, whose run
// results are of leg linux, and here it runs at m2 with no matrix, in one run of leg "" that ended
// with no result: no result of c1 answers that run, so the check reads unknown (ci_fault) at m2,
// where the results of c1 count (the control; R9: "When a check has no result that counts after a
// run of its job ends, it reads unknown"). A run result of leg "" at c1 beside those of leg linux
// would answer it, and the check would read true (passed, counted from c1), so the reader refuses
// the two forms, with a line that names the file, the commit, the job and the two legs. The rule
// reads a run result at the commit where the judge counts it, its commit, so a test_commit, which
// a result of a check of the code does not need, changes neither (the deep audit's input: a
// test_commit of no commit of the history, or of m2, where the run of no matrix is).
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
	var base []map[string]any
	readJSONFile(t, filepath.Join(dir, "results.json"), &base)
	if base[0]["kind"] != "run" || base[0]["commit"] != "c1" || base[2]["kind"] != "run" || base[2]["leg"] != "linux" {
		t.Fatalf("silent-leg's results 1 and 3 are no runs of leg linux at c1: %v", base[:3])
	}
	const mix = `one job at one commit has legs or it has none, and "" is the leg of no matrix`
	for _, row := range []struct {
		result int
		set    map[string]any
		want   string
	}{
		{0, map[string]any{"leg": ""}, `result 3: it sets the leg "linux", and result 1 of its job "go-checks" at the commit "c1" sets the leg ""; ` + mix},
		{0, map[string]any{"leg": "", "test_commit": "c9"}, `result 3: it sets the leg "linux", and result 1 of its job "go-checks" at the commit "c1" sets the leg ""; ` + mix},
		{2, map[string]any{"leg": "", "test_commit": "c9"}, `result 3: it sets the leg "", and result 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; ` + mix},
		{2, map[string]any{"leg": "", "test_commit": "m2"}, `result 3: it sets the leg "", and result 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; ` + mix},
	} {
		results := slices.Clone(base)
		results[row.result] = withMembers(results[row.result], row.set)
		writeJSONFile(t, filepath.Join(dir, "results.json"), results)
		expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") + ": " + row.want + "\n"})
	}
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." R7: "A
// pass and a failure on one such subject and one procedure make the state unknown." At the commit
// where CI refused the check, the check reads unknown (uncited) with the refusal's rule, whatever
// else holds there: its own results before or after the refusal, a result carried from another
// commit, or newer evidence of another commit that counts there. The refused commit's own results
// count nowhere, at it or where R8 would carry them: R48 cancelled them at their commit, so R8 has
// nothing to carry. The own results of every other commit are R7's and R8's, whatever CI refused
// elsewhere: a refusal cancels no result of another commit. Each row: carry, with a subject at c1
// whose spec files are m2's (withSubjectAtC1), the row's records, c0 as c1's parent with c1's tree
// and ties where the row says so, and a run of the job that ended at m2 but where the row says
// none; CI refused the check at the row's commit. The check reads unknown (uncited) there, the
// row's line at each other subject of c1, c2 and m2, where the results of c0, c1, c2 and m2 carry
// (R8), and with no refusal the row's line of none at each of the three. c1's results are recorded
// on 12 November, its pass at 10:00 and its caught failing example at 10:05; m2's pass is on 13
// November at 12:00 where the row does not say.
func TestARefusalOfCICancelsTheResultsOfItsCommitAlone(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	type change = func(r []map[string]any) []map[string]any
	// run adds a run of the check at commit with its outcome, recorded at its time and seq, and fault
	// a caught failing example planted on commit; at is m2's pass, beside the run of the job that
	// ended at m2; failsAtC1 makes c1's run at 10:00 a failure.
	run := func(commit, outcome, runAt string, seq int) change {
		return func(r []map[string]any) []map[string]any {
			return append(r, withMembers(r[0], map[string]any{"commit": commit, "outcome": outcome, "at": runAt, "seq": seq}))
		}
	}
	fault := func(commit, faultAt string, seq int) change {
		return func(r []map[string]any) []map[string]any {
			return append(r, withMembers(plantedOn(r[1], commit), map[string]any{"at": faultAt, "seq": seq}))
		}
	}
	at := func(passAt string) change { return run("m2", "pass", passAt, 6) }
	failsAtC1 := func(r []map[string]any) []map[string]any {
		r[0] = withMembers(r[0], map[string]any{"outcome": "fail"})
		return r
	}
	subjects := map[string]string{"c1": "c1, the head of c1-head", "c2": "c2, the head of listener-restart", "m2": "m2, the head of main", "c4": "c4, the head of listener-change"}
	for _, row := range []struct {
		name       string
		refused    string // the commit where CI refused the check
		changes    []change
		want       string // the line of the check at each of c1, c2 and m2 but the refused commit
		none       string // the line of the check at each of c1, c2 and m2 with no refusal
		c0         bool
		noM2JobRun bool
	}{
		{name: "carry's records alone: c1's results count nowhere", refused: "c1", want: "unknown (uncited)", none: "true (passed, counted from c1)"},
		{name: "m2's pass, with no failing example of m2: c1's caught failing example counts nowhere", refused: "c1", changes: []change{at("2026-11-13T12:00:00Z")},
			want: "unknown (no_failing_example)", none: "true (passed, counted from m2)"},
		{name: "m2's pass, and a caught failing example at c2 newer than c1's results", refused: "c1",
			changes: []change{at("2026-11-13T12:00:00Z"), fault("c2", "2026-11-13T11:00:00Z", 7)}, want: "true (passed, counted from m2)", none: "true (passed, counted from m2)"},
		{name: "m2's pass and its caught failing example, recorded before c1's results, are m2's own", refused: "c1",
			changes: []change{at("2026-11-12T09:00:00Z"), fault("m2", "2026-11-12T09:05:00Z", 7)}, want: "true (passed, counted from m2)", none: "true (passed, counted from c1)"},
		{name: "m2's pass recorded between c1's pass and c1's failing example", refused: "c1",
			changes: []change{at("2026-11-12T10:02:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7)}, want: "true (passed, counted from m2)", none: "true (passed, counted from m2)"},
		// Each row below holds a failure, a run that could not run or a failing example that missed its
		// fault. The refusal at c1 takes out c1's own, and cancels none of another commit's: R7 reads
		// a pass and a failure of one procedure as unknown.
		{name: "unknown stays unknown: c2 fails at 10:03, between c1's pass and c1's failing example, then passes at 12:00", refused: "c1",
			changes: []change{run("c2", "fail", "2026-11-12T10:03:00Z", 5), run("c2", "pass", "2026-11-12T12:00:00Z", 6), fault("c2", "2026-11-12T12:05:00Z", 7)},
			want:    "unknown (disagree)", none: "unknown (disagree)"},
		{name: "c2 fails at 10:03, and m2 passes", refused: "c1",
			changes: []change{run("c2", "fail", "2026-11-12T10:03:00Z", 5), at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7)},
			want:    "unknown (disagree)", none: "unknown (disagree)"},
		{name: "c1 fails at 10:00, and c0, c1's parent, passes at 11:00: c1's failure counts nowhere", refused: "c1", c0: true,
			changes: []change{failsAtC1, run("c0", "pass", "2026-11-12T11:00:00Z", 5), fault("c0", "2026-11-12T11:05:00Z", 6)},
			want:    "true (passed, counted from c0)", none: "unknown (disagree)"},
		{name: "c0, c1's parent, passes at 09:00, and c1 fails at 10:00: c1's failure counts nowhere", refused: "c1", c0: true,
			changes: []change{failsAtC1, run("c0", "pass", "2026-11-12T09:00:00Z", 5), fault("c0", "2026-11-12T09:05:00Z", 6)},
			want:    "true (passed, counted from c0)", none: "false (failed, counted from c1)"},
		{name: "c2 fails at 10:03 and passes at 10:04, before c1's failing example, and catches its fault at 12:05", refused: "c1",
			changes: []change{run("c2", "fail", "2026-11-12T10:03:00Z", 5), run("c2", "pass", "2026-11-12T10:04:00Z", 6), fault("c2", "2026-11-12T12:05:00Z", 7)},
			want:    "unknown (disagree)", none: "unknown (disagree)"},
		{name: "c0, c1's parent, fails at 09:00, and c1 holds only its caught failing example: c0's failure stays", refused: "c1", c0: true,
			changes: []change{func(r []map[string]any) []map[string]any {
				r[0] = withMembers(r[0], map[string]any{"commit": "c0", "outcome": "fail", "at": "2026-11-12T09:00:00Z"})
				return r
			}, fault("m2", "2026-11-13T12:05:00Z", 6)}, want: "false (failed, counted from c0)", none: "false (failed, counted from c0)"},
		{name: "a failure at c2 at 11:00, newer than c1's results, with c2's caught failing example", refused: "c1",
			changes: []change{run("c2", "fail", "2026-11-12T11:00:00Z", 5), fault("c2", "2026-11-12T11:05:00Z", 6)},
			want:    "false (failed, counted from c2)", none: "false (failed, counted from c2)"},
		{name: "c2 passes at 10:02, before c1's failing example, fails at 11:00 and catches its fault at 11:05", refused: "c1",
			changes: []change{run("c2", "pass", "2026-11-12T10:02:00Z", 5), run("c2", "fail", "2026-11-12T11:00:00Z", 6), fault("c2", "2026-11-12T11:05:00Z", 7)},
			want:    "unknown (disagree)", none: "unknown (disagree)"},
		{name: "c1 fails at 10:00, and m2 passes: c1's failure counts nowhere", refused: "c1",
			changes: []change{failsAtC1, at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7)},
			want:    "true (passed, counted from m2)", none: "unknown (disagree)"},
		{name: "a run at c2 at 10:03 that could not run, between c1's pass and c1's failing example", refused: "c1",
			changes: []change{run("c2", "could_not_run", "2026-11-12T10:03:00Z", 5)}, want: "unknown (could_not_run)", none: "unknown (could_not_run)"},
		{name: "a failing example at c1 at 10:06 that missed its fault, and m2's later pass: c1's failing examples count nowhere", refused: "c1",
			changes: []change{func(r []map[string]any) []map[string]any {
				return append(r, withMembers(r[1], map[string]any{"outcome": "pass", "at": "2026-11-12T10:06:00Z", "seq": 5}))
			}, at("2026-11-13T12:00:00Z")}, want: "unknown (no_failing_example)", none: "unknown (failing_example_missed)"},
		{name: "a failing example at m2 that missed its fault, after m2's caught one, and m2's pass", refused: "c1",
			changes: []change{at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7), func(r []map[string]any) []map[string]any {
				return append(r, withMembers(plantedOn(r[1], "m2"), map[string]any{"outcome": "pass", "at": "2026-11-13T12:06:00Z", "seq": 8}))
			}}, want: "unknown (failing_example_missed)", none: "unknown (failing_example_missed)"},
		// At the refused commit itself, newer evidence of another commit does not decide either.
		{name: "CI refused the check at c2, which holds no result and no run", refused: "c2", noM2JobRun: true,
			want: "true (passed, counted from c1)", none: "true (passed, counted from c1)"},
		{name: "CI refused the check at c2, which passes at 10:30, and m2 passes with its caught failing example", refused: "c2",
			changes: []change{run("c2", "pass", "2026-11-12T10:30:00Z", 5), at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7)},
			want:    "true (passed, counted from m2)", none: "true (passed, counted from m2)"},
		{name: "CI refused the check at c2, which fails at 10:30, and m2 passes with its caught failing example: c2's failure counts nowhere", refused: "c2",
			changes: []change{run("c2", "fail", "2026-11-12T10:30:00Z", 5), at("2026-11-13T12:00:00Z"), fault("m2", "2026-11-13T12:05:00Z", 7)},
			want:    "true (passed, counted from m2)", none: "unknown (disagree)"},
		{name: "control: CI refused the check at c4, whose results do not count at c1, c2 or m2, and c4 passes", refused: "c4",
			changes: []change{run("c4", "pass", "2026-11-13T11:00:00Z", 5)}, want: "true (passed, counted from c1)", none: "true (passed, counted from c1)"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			lines := func(uncited map[string]any) map[string][]string {
				dir := copyScenario(t, "carry")
				withSubjectAtC1(t, dir)
				if row.c0 {
					withParentOfC1(t, dir)
				}
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
				writeJSONFile(t, filepath.Join(dir, "uncited.json"), uncited)
				got := runBinary(t, binary, "", "state", "--scenario", dir)
				if got.code != 0 || got.stderr != "" {
					t.Fatalf("exit %d, stderr %q: want exit 0 and nothing on stderr", got.code, got.stderr)
				}
				byCommit := map[string][]string{}
				for commit, subject := range subjects {
					byCommit[commit] = subjectLines(got.stdout, subject)
				}
				return byCommit
			}
			got := lines(map[string]any{row.refused: map[string]any{check: "test-did-not-run"}})
			if line := "  check " + check + ": unknown (uncited)"; !slices.Contains(got[row.refused], line) {
				t.Errorf("at the refused commit %s: %q, want the line %q", row.refused, got[row.refused], line)
			}
			none := lines(map[string]any{})
			for _, commit := range []string{"c1", "c2", "m2"} {
				if line := "  check " + check + ": " + row.want; commit != row.refused && !slices.Contains(got[commit], line) {
					t.Errorf("at subject %s: %q, want the line %q", subjects[commit], got[commit], line)
				}
				if line := "  check " + check + ": " + row.none; !slices.Contains(none[commit], line) {
					t.Errorf("with no refusal, at subject %s: %q, want the line %q", subjects[commit], none[commit], line)
				}
			}
		})
	}
}

// withParentOfC1 gives carry's c1 a parent c0, with c1's tree and c1's ties, so the results of c0
// count where those of c1 count.
func withParentOfC1(t *testing.T, dir string) {
	t.Helper()
	var history map[string]any
	readJSONFile(t, filepath.Join(dir, "history.json"), &history)
	commits := history["commits"].([]any)
	i := slices.IndexFunc(commits, func(c any) bool { return c.(map[string]any)["id"] == "c1" })
	if i < 0 {
		t.Fatal("carry's history holds no c1")
	}
	c1 := commits[i].(map[string]any)
	c0 := map[string]any{"id": "c0", "parent": c1["parent"], "files": maps.Clone(c1["files"].(map[string]any))}
	c1["parent"] = "c0"
	history["commits"] = slices.Insert(commits, i, any(c0))
	writeJSONFile(t, filepath.Join(dir, "history.json"), history)
	var ties map[string]any
	readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
	ties["c0"] = ties["c1"]
	writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
}

// withMembers is a copy of the record r with the members of set set to their values.
func withMembers(r, set map[string]any) map[string]any {
	c := maps.Clone(r)
	maps.Copy(c, set)
	return c
}

// plantedOn is a copy of r, a failing example of carry's program check of the code, with the fault
// that the step planted on commit: its commit the fault's own commit "f-<commit>", which CI tested,
// its planted_on commit, from which it counts, and its one fault path a source file of the check's
// tie, as carry's own failing example has.
func plantedOn(r map[string]any, commit string) map[string]any {
	return withMembers(r, map[string]any{"commit": "f-" + commit, "planted_on": commit, "fault_paths": []any{"packages/envoy/cmd/listener/main.go"}})
}
