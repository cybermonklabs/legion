package specgraph

// The bases that the sweeps of sweep_test.go and blocksweep_test.go read (sweepBases), the helpers
// that build and check them, and the test of specgraphtest.As, through which some of them are built.

import (
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
)

// sweepBases is each base that the sweeps read, by its name: each scenario of testdata/scenarios,
// and twenty shapes that no scenario holds. silent-leg with the job's matrix dropped after c1, one
// run of no matrix at m2 that ended with no result: there a run result of the leg "" at c1, where
// the job had legs, would answer that run, and the check would read true (legFault refuses it).
// Three of carry where a failure and a later pass count at one subject, so the check reads unknown
// (disagree; R7: "A pass and a failure on one such subject and one procedure make the state
// unknown"): a failure of c2 at 10:03, between c1's pass and c1's caught failing example, and a
// later pass of c2 with its caught failing example; c0, c1's parent with c1's tree and ties, whose
// failure at 09:00 counts where c1's results count, and a later pass of c2; and a failure of c2 at
// 10:30 and a pass of m2 on 13 November with its caught failing example; each caught failing
// example of c2 or m2 is planted on its commit. A refusal of CI at a commit makes its check read
// unknown there and takes out the results of that commit alone (R48: "At that commit, such a check
// reads unknown"), so a rule by which a refusal cancelled the failure of another commit, or let
// newer evidence decide at the refused commit, would read the later pass true. And carry with a
// later failing example, planted on c1 in a fault of its own of two paths, carry's and a second
// source file of the tie, on which the test missed its fault: the check reads unknown
// (failing_example_missed) at c2 and at m2, and the member sweep takes out its planted_on and its
// fault_paths, and the sweep writes each of its two fault paths as " ", a blank path beside a real
// one, which leaves a record that does not say where its fault was planted or what it changed,
// newer than carry's caught one, so that a rule by which such a record counted nowhere and let the
// older caught one decide would read the check true (CheckStates, step 7). And the same later
// failing example with no planted_on: the check reads unknown (no_planted_on) at c2 and at m2, and
// the refusal sweep refuses the check at its own commit, f2, which must not take it out; the member
// sweep adds each planted_on that a record names and c9, no commit of the history. And the same
// later failing example planted on c9, which no commit of the history holds: it counts from no
// commit, so the check reads unknown (planted_on_outside_history) at c2 and at m2, and the refusal
// sweep refuses the check at c9 and at f2, neither of which may take it out (countedCommit gives it
// no commit, so neither refusal has a right). And carry with its failing example in the form of a
// query or an agent of the code, its commit c1 and no planted_on and no fault path: as no step
// planted it, it counts nowhere, and the check reads unknown (no_planted_on) at c2 and at m2, so
// the member sweep adds planted_on, and fault_paths, to a failing example that neither alone makes
// count. And two of wait-cycle where a check that is false is the one fault under a statement, so a
// failing check that reads as a check of no statement would leave that statement met:
// envoy-delivers-messages, with its own check false and the check of a-message-survives-a-restart
// true (its two outcomes swapped); and a-message-survives-a-restart, with a second check that passes
// beside its failing one. And eight of production-and-graph, two
// for each kind of check that reads no commit: its query of the graph,
// and that check written as an agent of the graph, as a query of production and as an agent of
// production. A result of each belongs to no commit, so CI's refusal at the commit that its
// test_commit names takes out nothing of it (CheckStates, step 2), and R7 decides. Of each two, one
// holds test_commit c1 on the check's failing run and the other CI's refusal of the check at c1:
// the refusal sweep adds the refusal to the one and the member sweep the test_commit to the other,
// so an input of each holds both facts, and a rule by which such a refusal took out that run would
// read the check's earlier pass true. And two of production-and-graph with its program check of
// production written as another kind: as a program check of the graph, whose runs read the
// transaction tx1 and count at the commit of their test, and as an agent of production, whose block
// names no test. Each of the twenty reads, at the subjects of its shape, the state that the shape
// gives its check (readsAt): the ten above the states that their shapes are for, and the ten of
// production-and-graph their check of the graph, or the check that they write, in the state that
// the check's kind gives its results; the eight that write a check also read it as its kind
// (readsAs). A state does not show each fact that a builder adds (a base that holds a test_commit or
// a refusal reads the same state with that fact as without it, by the rule that the base is for),
// so each base built from carry, in one row table, and each of the eight of production-and-graph
// that hold a test_commit or a refusal, also reads back the facts that its builder adds, as the
// reader reads them, from a statement of those facts apart from the builder's input: each record
// that a builder of carry adds, with its kind, outcome, commit and planted_on, and each run of
// go-checks; and a run of the check that failed and names test_commit c1, or CI's refusal of the
// check at c1. And no base built from another reads, through the reader, as the base that it came
// from, so that a builder that changed nothing, or only the bytes of a file, gives no base.
func sweepBases(t *testing.T) map[string]fstest.MapFS {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", "scenarios", "*"))
	if err != nil || len(dirs) < 7 {
		t.Fatalf("the scenarios: %q, %v", dirs, err)
	}
	bases := map[string]fstest.MapFS{}
	for _, dir := range dirs {
		bases[filepath.Base(dir)] = scenarioFS(t, dir)
	}
	dropped := maps.Clone(bases["silent-leg"])
	dropped["jobs.json"] = &fstest.MapFile{Data: []byte(`[{"job":"go-checks","leg":"","commit":"m2","ended":true}]`)}
	bases["silent-leg with its matrix dropped at m2"] = dropped
	// from is the base that each built base came from, by the built base's name.
	from := map[string]fstest.MapFS{"silent-leg with its matrix dropped at m2": bases["silent-leg"]}
	// carry is carry with records added to results.json, each a copy of carry's result 1 (from 0, a
	// pass of a-restart-loses-no-message at c1) or 2 (from 1, its caught failing example, planted on
	// c1) with the members of set, and a run of go-checks that ended at each commit of jobsAt; a copy
	// of the failing example planted on another commit sets planted_on and a commit of its own.
	type add struct {
		from int
		set  map[string]any
	}
	carry := func(jobsAt []string, adds ...add) fstest.MapFS {
		fsys := maps.Clone(bases["carry"])
		editTree(t, fsys, "results.json", func() treeEdit {
			return func(v any) any {
				results := v.([]any)
				for _, a := range adds {
					r := maps.Clone(results[a.from].(map[string]any))
					maps.Copy(r, a.set)
					results = append(results, r)
				}
				return results
			}
		})
		editTree(t, fsys, "jobs.json", func() treeEdit {
			return func(v any) any {
				jobs := v.([]any)
				for _, commit := range jobsAt {
					jobs = append(jobs, map[string]any{"job": "go-checks", "leg": "", "commit": commit, "ended": true})
				}
				return jobs
			}
		})
		return fsys
	}
	const code, faultPath, secondPath = "a-restart-loses-no-message", "packages/envoy/cmd/listener/main.go", "packages/envoy/cmd/listener/store.go"
	// has is whether s holds a result of code of kind with outcome, at commit, planted on plantedOn;
	// ended is whether s holds a run of go-checks of no matrix that ended at each of commits.
	has := func(s Scenario, kind ResultKind, outcome Outcome, commit, plantedOn string) bool {
		return slices.ContainsFunc(s.Results, func(r Result) bool {
			return r.Check == code && r.Kind == kind && r.Outcome == outcome && r.Commit == commit && r.PlantedOn == plantedOn
		})
	}
	ended := func(s Scenario, commits ...string) bool {
		return !slices.ContainsFunc(commits, func(commit string) bool {
			return !slices.ContainsFunc(s.JobRuns, func(r JobRun) bool { return r.Job == "go-checks" && *r.Leg == "" && r.Commit == commit && *r.Ended })
		})
	}
	// withParent makes c0 the parent of c1, with c1's tree and ties.
	withParent := func(fsys fstest.MapFS) {
		editTree(t, fsys, "history.json", func() treeEdit {
			return func(v any) any {
				history := v.(map[string]any)
				commits := history["commits"].([]any)
				i := slices.IndexFunc(commits, func(c any) bool { return c.(map[string]any)["id"] == "c1" })
				c1 := maps.Clone(commits[i].(map[string]any))
				c0 := map[string]any{"id": "c0", "parent": c1["parent"], "files": c1["files"]}
				c1["parent"] = "c0"
				commits = slices.Clone(commits)
				commits[i] = c1
				history["commits"] = slices.Insert(commits, i, any(c0))
				return history
			}
		})
		editTree(t, fsys, "ties.json", func() treeEdit {
			return func(v any) any {
				ties := v.(map[string]any)
				ties["c0"] = ties["c1"]
				return ties
			}
		})
	}
	// Each base built from carry is one row: its name, the facts that its builder adds, the builder's
	// input (the runs of go-checks, the records added, and a further edit), and the read-back of
	// those facts as the reader reads them, stated apart from the input, so that a builder, or an
	// input of it, that adds nothing or another thing fails by name.
	for _, row := range []struct {
		name, facts string
		jobsAt      []string
		adds        []add
		then        func(fstest.MapFS) // nil for none
		holds       func(Scenario) bool
	}{
		{"carry with c2's failure between c1's pass and c1's failing example",
			"a failure of c2, a later pass of c2, a caught failing example planted on c2 at the commit f-c2, and a run of go-checks that ended at c2",
			[]string{"c2"}, []add{
				{0, map[string]any{"commit": "c2", "outcome": "fail", "at": "2026-11-12T10:03:00Z", "seq": 5}},
				{0, map[string]any{"commit": "c2", "at": "2026-11-12T12:00:00Z", "seq": 6}},
				{1, map[string]any{"commit": "f-c2", "planted_on": "c2", "at": "2026-11-12T12:05:00Z", "seq": 7}}}, nil,
			func(s Scenario) bool {
				return has(s, ResultRun, OutcomeFail, "c2", "") && has(s, ResultRun, OutcomePass, "c2", "") && has(s, ResultFailingExample, OutcomeFail, "f-c2", "c2") && ended(s, "c2")
			}},
		{"carry with a failure of c0, c1's parent, and a later pass of c2",
			"c0, the parent of c1 with c1's tree and ties, a failure of c0, a later pass of c2, a caught failing example planted on c2 at the commit f-c2, and runs of go-checks that ended at c0 and at c2",
			[]string{"c0", "c2"}, []add{
				{0, map[string]any{"commit": "c0", "outcome": "fail", "at": "2026-11-12T09:00:00Z", "seq": 5}},
				{0, map[string]any{"commit": "c2", "at": "2026-11-12T12:00:00Z", "seq": 6}},
				{1, map[string]any{"commit": "f-c2", "planted_on": "c2", "at": "2026-11-12T12:05:00Z", "seq": 7}}}, withParent,
			func(s Scenario) bool {
				i := slices.IndexFunc(s.History.Commits, func(c FixtureCommit) bool { return c.ID == "c1" })
				changed, ok, err := s.History.Changed("c0", "c1")
				return i >= 0 && s.History.Commits[i].Parent == "c0" && ok && err == nil && len(changed) == 0 && s.Ties["c0"][code].Known &&
					has(s, ResultRun, OutcomeFail, "c0", "") && has(s, ResultRun, OutcomePass, "c2", "") && has(s, ResultFailingExample, OutcomeFail, "f-c2", "c2") && ended(s, "c0", "c2")
			}},
		{"carry with c2's failure at 10:30 and a later pass of m2",
			"a failure of c2, a later pass of m2, a caught failing example planted on m2 at the commit f-m2, and runs of go-checks that ended at c2 and at m2",
			[]string{"c2", "m2"}, []add{
				{0, map[string]any{"commit": "c2", "outcome": "fail", "at": "2026-11-12T10:30:00Z", "seq": 5}},
				{0, map[string]any{"commit": "m2", "at": "2026-11-13T12:00:00Z", "seq": 6}},
				{1, map[string]any{"commit": "f-m2", "planted_on": "m2", "at": "2026-11-13T12:05:00Z", "seq": 7}}}, nil,
			func(s Scenario) bool {
				return has(s, ResultRun, OutcomeFail, "c2", "") && has(s, ResultRun, OutcomePass, "m2", "") && has(s, ResultFailingExample, OutcomeFail, "f-m2", "m2") && ended(s, "c2", "m2")
			}},
		{"carry with a later failing example planted on c1 that missed its fault",
			"a failing example after carry's caught one, at the commit f2, planted on c1 with carry's fault path and a second source file of the tie, on which the test missed its fault",
			nil, []add{{1, map[string]any{"commit": "f2", "outcome": "pass", "seq": 5, "fault_paths": []any{faultPath, secondPath}}}}, nil,
			func(s Scenario) bool {
				return slices.ContainsFunc(s.Results, func(r Result) bool {
					return r.Check == code && r.Kind == ResultFailingExample && r.Outcome == OutcomePass && r.Commit == "f2" && r.PlantedOn == "c1" &&
						slices.Equal(r.FaultPaths, []string{faultPath, secondPath}) && r.Seq == 5
				})
			}},
		{"carry with a later failing example planted by no step",
			"a failing example after carry's caught one, at the commit f2, with no planted_on and with carry's fault path and a second source file of the tie, on which the test missed its fault",
			nil, []add{{1, map[string]any{"commit": "f2", "outcome": "pass", "seq": 5, "fault_paths": []any{faultPath, secondPath}}}}, func(fsys fstest.MapFS) {
				editTree(t, fsys, "results.json", func() treeEdit {
					return func(v any) any {
						results := v.([]any)
						delete(results[len(results)-1].(map[string]any), "planted_on")
						return results
					}
				})
			},
			func(s Scenario) bool {
				return slices.ContainsFunc(s.Results, func(r Result) bool {
					return r.Check == code && r.Kind == ResultFailingExample && r.Outcome == OutcomePass && r.Commit == "f2" && r.PlantedOn == "" &&
						slices.Equal(r.FaultPaths, []string{faultPath, secondPath}) && r.Seq == 5
				})
			}},
		{"carry with a later failing example planted on c9, no commit of the history",
			"a failing example after carry's caught one, at the commit f2, planted on c9, which the history does not hold, with carry's fault path and a second source file of the tie, on which the test missed its fault",
			nil, []add{{1, map[string]any{"commit": "f2", "outcome": "pass", "seq": 5, "planted_on": "c9", "fault_paths": []any{faultPath, secondPath}}}}, nil,
			func(s Scenario) bool {
				_, held, err := s.History.Changed("c9", "c9")
				return err == nil && !held && slices.ContainsFunc(s.Results, func(r Result) bool {
					return r.Check == code && r.Kind == ResultFailingExample && r.Outcome == OutcomePass && r.Commit == "f2" && r.PlantedOn == "c9" &&
						slices.Equal(r.FaultPaths, []string{faultPath, secondPath}) && r.Seq == 5
				})
			}},
		{"carry with its failing example planted by no step",
			"a caught failing example at the commit c1 with no planted_on and no fault path",
			nil, nil, func(fsys fstest.MapFS) {
				editTree(t, fsys, "results.json", func() treeEdit {
					return func(v any) any {
						fault := v.([]any)[1].(map[string]any)
						fault["commit"] = fault["planted_on"]
						delete(fault, "planted_on")
						delete(fault, "fault_paths")
						return v
					}
				})
			},
			func(s Scenario) bool {
				return has(s, ResultFailingExample, OutcomeFail, "c1", "") && !slices.ContainsFunc(s.Results, func(r Result) bool { return len(r.FaultPaths) > 0 })
			}},
	} {
		fsys := carry(row.jobsAt, row.adds...)
		if row.then != nil {
			row.then(fsys)
		}
		if s, err := ReadScenario(fsys); err != nil || !row.holds(s) {
			t.Fatalf("%s: the reader does not read the facts that its builder adds, %s (%v)", row.name, row.facts, err)
		}
		bases[row.name], from[row.name] = fsys, bases["carry"]
	}
	swapped := maps.Clone(bases["wait-cycle"])
	editTree(t, swapped, "results.json", func() treeEdit {
		return func(v any) any {
			for _, r := range v.([]any) {
				switch r := r.(map[string]any); r["seq"] {
				case 1.0:
					r["outcome"] = "fail"
				case 5.0:
					r["outcome"] = "pass"
				}
			}
			return v
		}
	})
	bases["wait-cycle with the outcomes of its two first runs swapped"] = swapped
	bases["wait-cycle with a second, passing check of a-message-survives-a-restart"] = withSecondCheck(t, bases["wait-cycle"])
	from["wait-cycle with the outcomes of its two first runs swapped"] = bases["wait-cycle"]
	from["wait-cycle with a second, passing check of a-message-survives-a-restart"] = bases["wait-cycle"]
	// The ten bases above read, at the subjects of their shapes, the states that the shapes are for
	// (readsAt): the check of the code of silent-leg unknown (ci_fault) at m2, at the run of no
	// matrix, which names no leg; that of carry unknown (disagree) at c2 and at m2, where its failure
	// and its later pass count, unknown (failing_example_missed) there with a later failing example
	// that missed its fault, unknown (no_planted_on) there with a failing example planted by no
	// step, later or caught, and unknown (planted_on_outside_history) there with a later one planted
	// on c9; and at m1 of wait-cycle, the own check of envoy-delivers-messages false and the check of
	// a-message-survives-a-restart true, or that check false and the one added beside it true.
	disagree := CheckState{State: checkUnknown, Reason: reasonDisagree, Disagree: true}
	passedAtB1 := CheckState{State: checkTrue, Reason: reasonPassed, From: "b1"}
	failedAtB1 := CheckState{State: checkFalse, Reason: reasonFailed, From: "b1"}
	for _, r := range []struct {
		base, check string
		want        CheckState
		commits     []string
	}{
		{"silent-leg with its matrix dropped at m2", code, CheckState{State: checkUnknown, Reason: reasonCIFault}, []string{"m2"}},
		{"carry with c2's failure between c1's pass and c1's failing example", code, disagree, []string{"c2", "m2"}},
		{"carry with a failure of c0, c1's parent, and a later pass of c2", code, disagree, []string{"c2", "m2"}},
		{"carry with c2's failure at 10:30 and a later pass of m2", code, disagree, []string{"c2", "m2"}},
		{"carry with a later failing example planted on c1 that missed its fault", code, CheckState{State: checkUnknown, Reason: reasonFailingExampleMissed}, []string{"c2", "m2"}},
		{"carry with a later failing example planted by no step", code, CheckState{State: checkUnknown, Reason: reasonNoPlantedOn}, []string{"c2", "m2"}},
		{"carry with a later failing example planted on c9, no commit of the history", code, CheckState{State: checkUnknown, Reason: reasonPlantedOnOutside, Detail: "c9"}, []string{"c2", "m2"}},
		{"carry with its failing example planted by no step", code, CheckState{State: checkUnknown, Reason: reasonNoPlantedOn}, []string{"c2", "m2"}},
		{"wait-cycle with the outcomes of its two first runs swapped", "a-sent-message-reaches-a-listener", failedAtB1, []string{"m1"}},
		{"wait-cycle with the outcomes of its two first runs swapped", "a-restart-keeps-the-message", passedAtB1, []string{"m1"}},
		{"wait-cycle with a second, passing check of a-message-survives-a-restart", "a-restart-keeps-the-message", failedAtB1, []string{"m1"}},
		{"wait-cycle with a second, passing check of a-message-survives-a-restart", "a-restart-keeps-the-order", passedAtB1, []string{"m1"}},
	} {
		readsAt(t, r.base, bases[r.base], r.check, r.want, r.commits...)
	}
	const graph, program = "the-graph-holds-a-delivery-of-each-message", "a-production-restart-loses-no-message"
	// written is production-and-graph with its check id written as a check of procedure and subject
	// by the one conversion that the command's tests read it through too (specgraphtest.As): its text
	// in the approved spec and in the files at m2, for a check of production each run of it that read
	// the transaction tx1 reading the deploy d1 of the subject instead, as a check of production reads
	// a deploy, and for a check of the graph each run of it that read the deploy d1 reading the
	// transaction tx1 instead. The approved spec and the files read the check as that kind (readsAs).
	written := func(id string, procedure Procedure, subject Subject) fstest.MapFS {
		fsys, err := specgraphtest.As(bases["production-and-graph"], id, string(procedure), string(subject))
		if err != nil {
			t.Fatalf("production-and-graph with %s written with procedure %q and subject %q: %v", id, procedure, subject, err)
		}
		readsAs(t, fmt.Sprintf("production-and-graph with %s written with procedure %q and subject %q", id, procedure, subject), fsys, id, procedure, subject)
		return fsys
	}
	// Each base of production-and-graph reads its check of the graph, or its program check of
	// production as written, in the state that its kind gives the scenario's results of it, a pass
	// at 10:00, a caught failing example and a failure at 11:00 (readsAt), so that a conversion that
	// gives the check its kind and results that read another subject, or no result that counts,
	// gives no base: a check of the graph unknown, since its pass and its failure at the transaction
	// tx1 disagree (R7), and one of production false, since its newest run decides, at the deploy d1.
	failed := CheckState{State: checkFalse, Reason: reasonFailed, From: "d1"}
	type converted struct {
		fsys fstest.MapFS
		want CheckState
	}
	// Each kind of check that reads no commit gives two bases, one for each row below: the row's
	// builder adds its fact to the base of that kind, and the read-back reads that fact as the reader
	// reads it, so that a builder that adds nothing fails by name.
	addsTo := []struct {
		how   string
		build func(base fstest.MapFS) fstest.MapFS
		holds func(Scenario) bool
	}{
		{"with test_commit c1 on the failing run of ", func(base fstest.MapFS) fstest.MapFS {
			fsys := maps.Clone(base)
			editTree(t, fsys, "results.json", func() treeEdit {
				return func(v any) any {
					for _, r := range v.([]any) {
						if r := r.(map[string]any); r["check"] == graph && r["kind"] == "run" && r["outcome"] == "fail" {
							r["test_commit"] = "c1"
						}
					}
					return v
				}
			})
			return fsys
		}, func(s Scenario) bool {
			return slices.ContainsFunc(s.Results, func(r Result) bool {
				return r.Check == graph && r.Kind == ResultRun && r.Outcome == OutcomeFail && r.TestCommit == "c1"
			})
		}},
		{"with CI's refusal at c1 of ", func(base fstest.MapFS) fstest.MapFS {
			fsys := maps.Clone(base)
			fsys["uncited.json"] = &fstest.MapFile{Data: []byte(`{"c1": {"` + graph + `": "test-not-in-code"}}`)}
			return fsys
		}, func(s Scenario) bool { return s.Uncited["c1"][graph] == "test-not-in-code" }},
	}
	for what, base := range map[string]converted{
		"its query of the graph":                           {bases["production-and-graph"], disagree},
		"its check of the graph as an agent":               {written(graph, ProcedureAgent, SubjectGraph), disagree},
		"its check of the graph as a query of production":  {written(graph, ProcedureQuery, SubjectProduction), failed},
		"its check of the graph as an agent of production": {written(graph, ProcedureAgent, SubjectProduction), failed},
	} {
		for _, row := range addsTo {
			name, fsys := "production-and-graph "+row.how+what, row.build(base.fsys)
			readsAt(t, name, fsys, graph, base.want, "m2")
			if s, err := ReadScenario(fsys); err != nil || !row.holds(s) {
				t.Fatalf("%s: the reader does not read the fact that the base is built to hold (%v)", name, err)
			}
			bases[name], from[name] = fsys, base.fsys
		}
	}
	for what, base := range map[string]converted{
		"as a program check of the graph": {written(program, ProcedureProgram, SubjectGraph), disagree},
		"as an agent of production":       {written(program, ProcedureAgent, SubjectProduction), failed},
	} {
		name := "production-and-graph with its program check of production " + what
		readsAt(t, name, base.fsys, program, base.want, "m2")
		bases[name], from[name] = base.fsys, bases["production-and-graph"]
	}
	// No built base reads, through the reader, as the base that it came from, and each base that is
	// no scenario is built from one: a builder that changed nothing, or only the bytes of its files,
	// gives no base.
	for _, name := range slices.Sorted(maps.Keys(bases)) {
		source, built := from[name]
		switch {
		case !built && !slices.Contains(dirs, filepath.Join("testdata", "scenarios", name)):
			t.Fatalf("%s: a base that is no scenario names no base that it came from", name)
		case !built:
			continue
		}
		got, err := ReadScenario(bases[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want, err := ReadScenario(source); err != nil || reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the reader reads it as the base that it came from (%v)", name, err)
		}
	}
	return bases
}

// withSecondCheck is wait-cycle with a-restart-keeps-the-order, a check of the code that passes,
// under a-message-survives-a-restart beside its failing check: a copy of
// a-late-session-gets-the-message with its results and its ties.
func withSecondCheck(t *testing.T, base fstest.MapFS) fstest.MapFS {
	t.Helper()
	const old, added = "a-late-session-gets-the-message", "a-restart-keeps-the-order"
	fsys := maps.Clone(base)
	rename := strings.NewReplacer("#"+old, "#"+added, `serves="a-message-reaches-a-late-session"`, `serves="a-message-survives-a-restart"`)
	var block string
	editTree(t, fsys, "approved.json", func() treeEdit {
		return func(v any) any {
			nodes := v.(map[string]any)["nodes"].([]any)
			for _, n := range nodes {
				if n := n.(map[string]any); n["id"] == old {
					version := maps.Clone(n["versions"].([]any)[0].(map[string]any))
					block = rename.Replace(version["block"].(string))
					version["block"] = block
					c := maps.Clone(n)
					c["id"], c["versions"] = added, []any{version}
					nodes = append(nodes, c)
					break
				}
			}
			v.(map[string]any)["nodes"] = nodes
			return v
		}
	})
	fsys["files/m1/spec.md"] = &fstest.MapFile{Data: append(slices.Clone(fsys["files/m1/spec.md"].Data), "\n"+block...)}
	editTree(t, fsys, "results.json", func() treeEdit {
		return func(v any) any {
			results := v.([]any)
			for _, r := range slices.Clone(results) {
				if r := r.(map[string]any); r["check"] == old {
					c := maps.Clone(r)
					c["check"], c["seq"] = added, float64(len(results)+1)
					results = append(results, c)
				}
			}
			return results
		}
	})
	editTree(t, fsys, "ties.json", func() treeEdit {
		return func(v any) any {
			for _, ties := range v.(map[string]any) {
				ties := ties.(map[string]any)
				ties[added] = ties[old]
			}
			return v
		}
	})
	return fsys
}

// readsAt fails the test unless the check id of the base name reads want, with id as its ID, at
// each subject of the base whose commit is one of commits: its state, its reason, its detail and
// the subject that it counts from. It fails too when commits names no commit, or a commit that no
// subject of the base has, so that it never passes on reading no subject.
func readsAt(t *testing.T, name string, base fs.FS, id string, want CheckState, commits ...string) {
	t.Helper()
	s, err := ReadScenario(base)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(commits) == 0 {
		t.Fatalf("%s: readsAt names no commit of a subject", name)
	}
	for _, commit := range commits {
		if !slices.ContainsFunc(s.Subjects, func(subject ScenarioSubject) bool { return subject.Commit == commit }) {
			t.Fatalf("%s: no subject of it is at the commit %s", name, commit)
		}
	}
	want.ID = id
	for i, subject := range s.Subjects {
		if !slices.Contains(commits, subject.Commit) {
			continue
		}
		state, err := Derive(s.Input(i))
		if err != nil {
			t.Fatalf("%s: subject %d: %v", name, i+1, err)
		}
		if j := slices.IndexFunc(state.Checks, func(c CheckState) bool { return c.ID == id }); j < 0 || state.Checks[j] != want {
			t.Fatalf("%s: at subject %d the check %s reads %+v, want %+v", name, i+1, id, state.Checks[max(j, 0):max(j+1, 0)], want)
		}
	}
}

// readsAs is the scenario fsys, named name, as ReadScenario reads it, and fails the test unless the
// reader reads it and reads its check id as a check of procedure and subject: in the current
// approved spec and in the files of each commit. Whether the check names the test of a program is
// the reader's own rule: it refuses a program check that names no test (no-test) and a check of
// another procedure that names one (test-on-non-program), so readsAs fails on that refusal first.
func readsAs(t *testing.T, name string, fsys fs.FS, id string, procedure Procedure, subject Subject) Scenario {
	t.Helper()
	s, err := ReadScenario(fsys)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	current, _ := s.Approved.Current()
	trees := map[string]Tree{"the approved spec": current}
	for commit, tree := range s.Files {
		trees["the files at "+commit] = tree
	}
	for _, where := range slices.Sorted(maps.Keys(trees)) {
		if c, held := trees[where].CheckByID(id); !held || c.Procedure != procedure || c.Subject != subject {
			t.Fatalf("%s: %s reads %s as a check of procedure %q and subject %q (held %t); want procedure %q and subject %q",
				name, where, id, c.Procedure, c.Subject, held, procedure, subject)
		}
	}
	return s
}

// specgraphtest.As, the one conversion through which the sweeps' bases and the command's tests
// write a check of a scenario as another kind, writes each kind that it is asked for: each
// procedure of each subject (R6), from the check that the tests convert to it, refusal's program
// check of the code for a kind of the code, production-and-graph's program check of production for
// a program check of production or of the graph, and its query of the graph for a query or an agent
// of production or of the graph. Each converted scenario reads with the check as that kind, in the
// approved spec and in the files of each commit (readsAs), and each run of the check reads the
// subject of the kind: a commit and no deploy or transaction for the code, as refusal's runs of it
// do, the deploy d1 and no transaction for production, and the transaction tx1 and no deploy for
// the graph. Each failing example of a check of the code reads as its kind counts it: for a
// program check, refusal's own record, the fault's commit f1, planted on c1 with one fault path;
// for a query or an agent, the commit c1 and no planted_on or fault path, since such a check counts
// a failing example at its commit. So a fault of As for any one kind fails the row of that kind.
func TestAsWritesACheckOfEachKind(t *testing.T) {
	t.Parallel()
	for _, subject := range []Subject{SubjectCode, SubjectProduction, SubjectGraph} {
		for _, procedure := range []Procedure{ProcedureProgram, ProcedureQuery, ProcedureAgent} {
			scenario, id := "production-and-graph", "the-graph-holds-a-delivery-of-each-message"
			switch {
			case subject == SubjectCode:
				scenario, id = "refusal", "a-restart-loses-no-message"
			case procedure == ProcedureProgram:
				id = "a-production-restart-loses-no-message"
			}
			t.Run(fmt.Sprintf("a check of procedure %s and subject %s", procedure, subject), func(t *testing.T) {
				t.Parallel()
				name := fmt.Sprintf("%s with %s written with procedure %q and subject %q", scenario, id, procedure, subject)
				fsys, err := specgraphtest.As(scenarioFS(t, filepath.Join("testdata", "scenarios", scenario)), id, string(procedure), string(subject))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				s := readsAs(t, name, fsys, id, procedure, subject)
				runs := 0
				for i, r := range s.Results {
					if r.Check != id || r.Kind != ResultRun {
						continue
					}
					runs++
					read := map[Subject]bool{
						SubjectCode:       r.Commit != "" && r.Deploy == "" && r.GraphTx == "",
						SubjectProduction: r.Deploy == "d1" && r.GraphTx == "",
						SubjectGraph:      r.GraphTx == "tx1" && r.Deploy == "",
					}
					if !read[subject] {
						t.Errorf("%s: result %d, a run, reads the commit %q, the deploy %q and the transaction %q, not the subject of a check of %s", name, i+1, r.Commit, r.Deploy, r.GraphTx, subject)
					}
				}
				if runs == 0 {
					t.Fatalf("%s: no run of %s", name, id)
				}
				if subject == SubjectCode {
					want := Result{Commit: "c1"}
					if procedure == ProcedureProgram {
						want = Result{Commit: "f1", PlantedOn: "c1", FaultPaths: []string{"packages/envoy/cmd/listener/main.go"}}
					}
					faults := 0
					for i, r := range s.Results {
						if r.Check != id || r.Kind != ResultFailingExample {
							continue
						}
						faults++
						if r.Commit != want.Commit || r.PlantedOn != want.PlantedOn || !slices.Equal(r.FaultPaths, want.FaultPaths) {
							t.Errorf("%s: result %d, a failing example, reads the commit %q, planted_on %q and the fault paths %q, want %q, %q and %q",
								name, i+1, r.Commit, r.PlantedOn, r.FaultPaths, want.Commit, want.PlantedOn, want.FaultPaths)
						}
					}
					if faults == 0 {
						t.Fatalf("%s: no failing example of %s", name, id)
					}
				}
			})
		}
	}
}
