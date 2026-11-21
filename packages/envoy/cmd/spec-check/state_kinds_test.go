package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
)

// The kind of a check, its procedure and its subject, decides whether its results count at a
// commit: a result of a check of the code counts at its commit, one of a program check of
// production or of the graph at its test_commit, the commit of the test that ran, and one of a
// query or an agent of production or of the graph at no commit, whatever test_commit it names.

// graphCheck is the check of the graph of production-and-graph, a query.
const graphCheck = "the-graph-holds-a-delivery-of-each-message"

// checkKind is a kind of check by its name: its procedure and its subject.
type checkKind struct {
	kind      string
	procedure specgraph.Procedure
	subject   specgraph.Subject
}

// eachKind is each kind of check: each procedure of each subject.
func eachKind() []checkKind {
	var kinds []checkKind
	for _, s := range []struct {
		name    string
		subject specgraph.Subject
	}{{"the code", specgraph.SubjectCode}, {"production", specgraph.SubjectProduction}, {"the graph", specgraph.SubjectGraph}} {
		for _, p := range []struct {
			name      string
			procedure specgraph.Procedure
		}{{"a program check", specgraph.ProcedureProgram}, {"a query", specgraph.ProcedureQuery}, {"an agent", specgraph.ProcedureAgent}} {
			kinds = append(kinds, checkKind{p.name + " of " + s.name, p.procedure, s.subject})
		}
	}
	return kinds
}

// readsNoCommit is each kind of check that reads no commit, as production-and-graph's query of the
// graph is written in it (scenarioAs).
var readsNoCommit = []checkKind{
	{"a query of the graph", specgraph.ProcedureQuery, specgraph.SubjectGraph},
	{"an agent of the graph", specgraph.ProcedureAgent, specgraph.SubjectGraph},
	{"a query of production", specgraph.ProcedureQuery, specgraph.SubjectProduction},
	{"an agent of production", specgraph.ProcedureAgent, specgraph.SubjectProduction},
}

// scenarioAs is a copy of the scenario name in a directory of the test's own, with its check id
// written as a check of procedure and subject by the one conversion that specgraph's sweeps read
// their bases through too (specgraphtest.As): its block in each approved version and in each spec
// file, for a check of production each result of it that read a transaction of the graph reading
// the deploy d1 instead, as a result of production reads a deploy, and for a check of the graph each
// result of it that read a deploy reading the transaction tx1 instead. The test fails when the
// scenario does not hold the check's block as As reads it, and when the copy, read back as the
// command reads it (specgraph.ReadScenario), does not hold the check id as a check of procedure and
// subject in the current approved spec and in the files of each commit, so that no row passes on a
// conversion that changed nothing or wrote another kind.
func scenarioAs(t *testing.T, name, id string, procedure specgraph.Procedure, subject specgraph.Subject) string {
	t.Helper()
	fsys, err := specgraphtest.As(os.DirFS(fixture(t, "scenarios", name)), id, string(procedure), string(subject))
	if err != nil {
		t.Fatalf("%s with %s written with procedure %q and subject %q: %v", name, id, procedure, subject, err)
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dir, fsys); err != nil {
		t.Fatal(err)
	}
	s, err := specgraph.ReadScenario(os.DirFS(dir))
	if err != nil {
		t.Fatalf("%s with %s written with procedure %q and subject %q: %v", name, id, procedure, subject, err)
	}
	current, _ := s.Approved.Current()
	trees := map[string]specgraph.Tree{"the approved spec": current}
	for commit, tree := range s.Files {
		trees["the files at "+commit] = tree
	}
	for _, where := range slices.Sorted(maps.Keys(trees)) {
		if c, held := trees[where].CheckByID(id); !held || c.Procedure != procedure || c.Subject != subject {
			t.Fatalf("%s with %s written with procedure %q and subject %q: %s reads it as procedure %q and subject %q (held %t)",
				name, id, procedure, subject, where, c.Procedure, c.Subject, held)
		}
	}
	return dir
}

// editResults changes the results of the scenario at dir by change.
func editResults(t *testing.T, dir string, change func(results []map[string]any)) {
	t.Helper()
	var results []map[string]any
	readJSONFile(t, filepath.Join(dir, "results.json"), &results)
	change(results)
	writeJSONFile(t, filepath.Join(dir, "results.json"), results)
}

// readScenario is the scenario at dir as the command reads it (specgraph.ReadScenario), so that a
// row reads back the edit that it compares.
func readScenario(t *testing.T, dir string) specgraph.Scenario {
	t.Helper()
	s, err := specgraph.ReadScenario(os.DirFS(dir))
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return s
}

// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of another."
// One job at one commit has legs or it has none, and the leg rule reads a run result at the commit
// where the judge counts it, as its check decides: a result of a query or an agent of production
// or of the graph is a record of its job at no commit, so a test_commit on it changes no answer.
// Each row: production-and-graph with its check of the graph written as the row's kind, its two
// runs reported by the job production-checks and its failing run in the leg x, where that job's
// runs at c1 are of the leg "": with test_commit c1 on that run, the command prints what it prints
// with none, and the reader reads that run, with its test_commit, before the two are compared. The
// controls: for each kind of check whose results count at a commit, a run result in the leg x at
// the commit c1 where it counts, where the runs of its job at c1 are of the leg "", is refused with
// the line of the leg rule. Those kinds are read from specgraph.CountsAtACommit over
// eachKind, so a kind that it counts at a commit later has its row here with no edit of a list. For
// a check of the code, refusal's check of the code written as the kind, with its pass at its
// commit c1 by the job go-checks; for a check of production or of the graph, production-and-graph's
// program check of production written as the kind, with its failure at its test_commit c1, the
// commit of the test that ran, by the job production-checks. Each kind counts at a commit or is
// one of readsNoCommit, and not both.
func TestTheLegRuleReadsNoCommitOfACheckThatReadsNoCommit(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	for _, k := range readsNoCommit {
		t.Run(k.kind, func(t *testing.T) {
			t.Parallel()
			var got [2]result
			for i, testCommit := range []string{"", "c1"} {
				dir := scenarioAs(t, "production-and-graph", graphCheck, k.procedure, k.subject)
				editResults(t, dir, func(results []map[string]any) {
					for _, r := range results {
						if r["check"] == graphCheck && r["kind"] == "run" {
							r["job"] = "production-checks"
							if r["outcome"] == "fail" {
								r["leg"] = "x"
								if testCommit != "" {
									r["test_commit"] = testCommit
								}
							}
						}
					}
				})
				// the edit, read back as the command reads it: a failing run of the check in the leg x,
				// by production-checks, with the row's test_commit
				if !slices.ContainsFunc(readScenario(t, dir).Results, func(r specgraph.Result) bool {
					return r.Check == graphCheck && r.Kind == specgraph.ResultRun && r.Outcome == specgraph.OutcomeFail && *r.Leg == "x" &&
						r.Job == "production-checks" && r.TestCommit == testCommit
				}) {
					t.Fatalf("with test_commit %q: the reader reads no failing run of %s in the leg x by production-checks with that test_commit", testCommit, graphCheck)
				}
				got[i] = runBinary(t, binary, "", "state", "--scenario", dir)
			}
			if got[0].code != 0 || got[0].stderr != "" || !strings.Contains(got[0].stdout, "  check "+graphCheck+": ") {
				t.Fatalf("with no test_commit: exit %d, stderr %q, stdout %q; want exit 0 and a line of %s", got[0].code, got[0].stderr, got[0].stdout, graphCheck)
			}
			expect(t, got[1], got[0])
		})
	}
	for _, k := range eachKind() {
		counts := specgraph.CountsAtACommit(specgraph.Check{Subject: k.subject, Procedure: k.procedure})
		if listed := slices.Contains(readsNoCommit, k); counts == listed {
			t.Errorf("%s: it counts at a commit %t, and it is one of readsNoCommit %t; want one of the two", k.kind, counts, listed)
		}
		if !counts {
			continue
		}
		// the scenario of the row, the check that it writes as the kind, and the run result of that
		// check at c1: its number, the member that names c1, and its job
		scenario, check, n, member, job := "production-and-graph", "a-production-restart-loses-no-message", 3, "test_commit", "production-checks"
		if k.subject == specgraph.SubjectCode {
			scenario, check, n, member, job = "refusal", "a-restart-loses-no-message", 1, "commit", "go-checks"
		}
		t.Run("control: "+k.kind+" in the leg x at its "+member+" c1", func(t *testing.T) {
			t.Parallel()
			dir := scenarioAs(t, scenario, check, k.procedure, k.subject)
			editResults(t, dir, func(results []map[string]any) {
				if r := results[n-1]; r["check"] != check || r["kind"] != "run" || r[member] != "c1" || r["job"] != job || r["leg"] != "" {
					t.Fatalf("result %d of %s is no run of %s at its %s c1 by %s in the leg \"\": %v", n, scenario, check, member, job, r)
				}
				results[n-1]["leg"] = "x"
			})
			expect(t, runBinary(t, binary, "", "state", "--scenario", dir), result{code: 2, stderr: "spec-check: " + filepath.Join(dir, "results.json") +
				fmt.Sprintf(`: result %d: it sets the leg "x", and job run 1 of its job %q at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`, n, job) + "\n"})
		})
	}
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." R7: "Of
// several results that count, the latest counts, and a later failure cancels an earlier pass." A
// query or an agent of production or of the graph reads no commit, so CI's refusal of it at the
// commit that a test_commit of its result names takes out nothing of it, and R7 decides. Each row:
// production-and-graph with its check of the graph written as the row's kind and the program
// check's failure at 11:00 taken out, so that the check of the graph decides both statements; its
// failing run at 11:00 names test_commit c1, and CI refused it at c1. The command prints what it
// prints with no refusal, and the check reads unknown (disagree) at the transaction of the graph,
// where its pass and its failure count, and false at the deploy, where the newest run decides. Each
// row reads its edits back as the command reads them before the two outputs are compared.
func TestARefusalAtATestCommitTakesOutNoResultOfACheckThatReadsNoCommit(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	for _, k := range readsNoCommit {
		t.Run(k.kind, func(t *testing.T) {
			t.Parallel()
			var got [2]result
			for i, refused := range []bool{false, true} {
				dir := scenarioAs(t, "production-and-graph", graphCheck, k.procedure, k.subject)
				editResults(t, dir, func(results []map[string]any) {
					for _, r := range results {
						if r["check"] == graphCheck && r["kind"] == "run" && r["outcome"] == "fail" {
							r["test_commit"] = "c1"
						}
					}
				})
				var results []map[string]any
				readJSONFile(t, filepath.Join(dir, "results.json"), &results)
				results = slices.DeleteFunc(results, func(r map[string]any) bool {
					return r["check"] == "a-production-restart-loses-no-message" && r["kind"] == "run" && r["outcome"] == "fail"
				})
				writeJSONFile(t, filepath.Join(dir, "results.json"), results)
				if refused {
					writeJSONFile(t, filepath.Join(dir, "uncited.json"), map[string]any{"c1": map[string]any{graphCheck: "test-not-in-code"}})
				}
				// the edits, read back as the command reads them: the failing run of the check with
				// test_commit c1, no failure of the program check, and CI's refusal at c1 exactly when
				// the row says so
				s := readScenario(t, dir)
				if !slices.ContainsFunc(s.Results, func(r specgraph.Result) bool {
					return r.Check == graphCheck && r.Kind == specgraph.ResultRun && r.Outcome == specgraph.OutcomeFail && r.TestCommit == "c1"
				}) || slices.ContainsFunc(s.Results, func(r specgraph.Result) bool {
					return r.Check == "a-production-restart-loses-no-message" && r.Kind == specgraph.ResultRun && r.Outcome == specgraph.OutcomeFail
				}) || (s.Uncited["c1"][graphCheck] == "test-not-in-code") != refused {
					t.Fatalf("refused %t: the reader does not read the row's edits: %+v, uncited %v", refused, s.Results, s.Uncited)
				}
				got[i] = runBinary(t, binary, "", "state", "--scenario", dir)
			}
			want := []string{"statement messages-reach-sessions: unknown (check " + graphCheck + ": disagree)",
				"  check " + graphCheck + ": unknown (disagree)"}
			if k.subject == specgraph.SubjectProduction {
				want = []string{"statement messages-reach-sessions: not met (check " + graphCheck + " is false)",
					"  check " + graphCheck + ": false (failed, counted from d1)"}
			}
			lines := strings.Split(got[1].stdout, "\n")
			if got[1].code != 0 || got[1].stderr != "" || !slices.Contains(lines, want[0]) || !slices.Contains(lines, want[1]) {
				t.Fatalf("with CI's refusal at c1: exit %d, stderr %q, stdout:\n%s\nwant exit 0 and the lines %q", got[1].code, got[1].stderr, got[1].stdout, want)
			}
			expect(t, got[1], got[0])
		})
	}
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." R8: "A
// result at one commit also counts at another commit when no code that the check runs changed
// between the two." An agent or a query of the code reads the commit of its results, as a program
// check of the code does, so the results of a commit where CI refused it count nowhere, at it or
// where R8 would carry them: its runs and its failing examples alike. Each row: refusal with its
// check of the code written as the row's kind, its caught failing example at c1 and its pass at c1
// or at c2, each of which counts at c2 and at m2, and CI's refusal at one commit or at none. The
// runs: with the pass at c1 and CI's refusal at c2, as the scenario holds it, the check reads true
// at m2 (the control); with the refusal at c1 instead, its pass counts nowhere, and it reads
// unknown (uncited) at c2 and at m2. The failing examples: with the pass at c2 and no refusal, it
// reads true at both (the control); with the refusal at c1, the pass of c2 counts and the failing
// example of c1 counts nowhere, so it reads unknown (no_failing_example) at both.
func TestARefusalOfAnAgentOrAQueryOfTheCodeTakesOutTheResultsOfItsCommit(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-restart-loses-no-message"
	for _, k := range []struct {
		kind      string
		procedure specgraph.Procedure
	}{
		{"an agent of the code", specgraph.ProcedureAgent},
		{"a query of the code", specgraph.ProcedureQuery},
	} {
		t.Run(k.kind, func(t *testing.T) {
			t.Parallel()
			for _, row := range []struct {
				pass, refused string            // the commit of the pass, and the commit of CI's refusal, "" for none
				want          map[string]string // the line of the check at each subject
			}{
				{"c1", "c2", map[string]string{"c2, the head of listener-restart": "unknown (uncited)", "m2, the head of main": "true (passed, counted from c1)"}},
				{"c1", "c1", map[string]string{"c2, the head of listener-restart": "unknown (uncited)", "m2, the head of main": "unknown (uncited)"}},
				{"c2", "", map[string]string{"c2, the head of listener-restart": "true (passed, counted from c2)", "m2, the head of main": "true (passed, counted from c2)"}},
				{"c2", "c1", map[string]string{"c2, the head of listener-restart": "unknown (no_failing_example)", "m2, the head of main": "unknown (no_failing_example)"}},
			} {
				dir := scenarioAs(t, "refusal", check, k.procedure, specgraph.SubjectCode)
				editResults(t, dir, func(results []map[string]any) {
					if r := results[0]; r["check"] != check || r["kind"] != "run" || r["outcome"] != "pass" || r["commit"] != "c1" {
						t.Fatalf("result 1 of refusal is no pass of %s at c1: %v", check, r)
					}
					results[0]["commit"] = row.pass
				})
				uncited := map[string]any{}
				if row.refused != "" {
					uncited[row.refused] = map[string]any{check: "test-not-in-code"}
				}
				writeJSONFile(t, filepath.Join(dir, "uncited.json"), uncited)
				got := runBinary(t, binary, "", "state", "--scenario", dir)
				if got.code != 0 || got.stderr != "" {
					t.Fatalf("its pass at %s, CI's refusal at %q: exit %d, stderr %q; want exit 0 and nothing on stderr", row.pass, row.refused, got.code, got.stderr)
				}
				for subject, want := range row.want {
					if line := "  check " + check + ": " + want; !slices.Contains(subjectLines(got.stdout, subject), line) {
						t.Errorf("its pass at %s, CI's refusal at %q: at subject %s %q, want the line %q", row.pass, row.refused, subject, subjectLines(got.stdout, subject), line)
					}
				}
			}
		})
	}
}

// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." R8: "A
// result at one commit also counts at another commit when no code that the check runs changed
// between the two." A program check of production or of the graph reads the commit of its test,
// the test_commit of its results, as a check of the code reads the commit of its results, so the
// results whose test ran at a commit where CI refused the check count nowhere, at it or where R8
// would carry them: its runs and its failing examples alike. Each row: production-and-graph with its
// program check of production written as the row's kind, c2 a commit of c1's tree, the check's pass
// at test_commit c2 and its caught failing example at c1, each of which counts at m2, and CI's
// refusal at c1 or at none. The failing examples: with its failing run of 11:00 taken out, it reads
// true with no refusal (the control), and with the refusal unknown (no_failing_example): the pass of
// c2 counts, and the failing example of c1 counts nowhere. The runs: with that run of c1 one that
// could not run, it reads unknown (could_not_run) with no refusal (the control), and with the
// refusal unknown (no_failing_example): that run counts nowhere, so the pass of c2 is the newest run
// that counts. A failing run in its place would show no runs half for production, whose newest run
// decides after its failing example: the refusal takes out the failing example of c1 too, so the
// check reads unknown (no_failing_example) whether that failing run counts or not.
func TestARefusalOfAProgramCheckOfProductionOrOfTheGraphTakesOutTheResultsOfItsTestCommit(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	const check = "a-production-restart-loses-no-message"
	for _, k := range []struct {
		kind    string
		subject specgraph.Subject
		from    string // the subject of production-and-graph that a run of the kind reads
	}{
		{"a program check of production", specgraph.SubjectProduction, "d1"},
		{"a program check of the graph", specgraph.SubjectGraph, "tx1"},
	} {
		t.Run(k.kind, func(t *testing.T) {
			t.Parallel()
			for _, row := range []struct {
				half, refused, want string // the half of the rule, the commit of CI's refusal ("" for none), and the line of the check
			}{
				{"failing examples", "", "true (passed, counted from " + k.from + ")"},
				{"failing examples", "c1", "unknown (no_failing_example)"},
				{"runs", "", "unknown (could_not_run)"},
				{"runs", "c1", "unknown (no_failing_example)"},
			} {
				dir := scenarioAs(t, "production-and-graph", check, specgraph.ProcedureProgram, k.subject)
				withCommitOf(t, dir, "c2", "c1")
				var results []map[string]any
				readJSONFile(t, filepath.Join(dir, "results.json"), &results)
				for i, want := range []struct{ kind, outcome string }{{"run", "pass"}, {"failing_example", "fail"}, {"run", "fail"}} {
					if r := results[i]; r["check"] != check || r["kind"] != want.kind || r["outcome"] != want.outcome || r["test_commit"] != "c1" {
						t.Fatalf("result %d of production-and-graph is no %s of %s with the outcome %s at test_commit c1: %v", i+1, want.kind, check, want.outcome, r)
					}
				}
				results[0]["test_commit"] = "c2"
				if row.half == "runs" {
					results[2]["outcome"] = "could_not_run"
				} else {
					results = slices.Delete(results, 2, 3)
				}
				writeJSONFile(t, filepath.Join(dir, "results.json"), results)
				uncited := map[string]any{}
				if row.refused != "" {
					uncited[row.refused] = map[string]any{check: "test-not-in-code"}
				}
				writeJSONFile(t, filepath.Join(dir, "uncited.json"), uncited)
				got := runBinary(t, binary, "", "state", "--scenario", dir)
				if line := "  check " + check + ": " + row.want; got.code != 0 || got.stderr != "" || !slices.Contains(strings.Split(got.stdout, "\n"), line) {
					t.Errorf("the %s, CI's refusal at %q: exit %d, stderr %q, stdout:\n%s\nwant exit 0 and the line %q", row.half, row.refused, got.code, got.stderr, got.stdout, line)
				}
			}
		})
	}
}

// withCommitOf adds to the history of the scenario at dir the commit id, a child of the commit
// named of with the same tree, and gives id the ties of that commit, so that a result at id counts
// wherever one at that commit counts (R8).
func withCommitOf(t *testing.T, dir, id, of string) {
	t.Helper()
	var history map[string]any
	readJSONFile(t, filepath.Join(dir, "history.json"), &history)
	commits, _ := history["commits"].([]any)
	i := slices.IndexFunc(commits, func(c any) bool { commit, _ := c.(map[string]any); return commit["id"] == of })
	if i < 0 {
		t.Fatalf("the history of %s holds no commit %s", dir, of)
	}
	history["commits"] = append(commits, map[string]any{"id": id, "parent": of, "files": commits[i].(map[string]any)["files"]})
	writeJSONFile(t, filepath.Join(dir, "history.json"), history)
	var ties map[string]any
	readJSONFile(t, filepath.Join(dir, "ties.json"), &ties)
	if ties[of] == nil {
		t.Fatalf("the ties of %s hold no commit %s", dir, of)
	}
	ties[id] = ties[of]
	writeJSONFile(t, filepath.Join(dir, "ties.json"), ties)
}
