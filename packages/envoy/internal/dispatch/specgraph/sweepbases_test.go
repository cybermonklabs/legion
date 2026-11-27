package specgraph

// The bases that the sweeps of sweep_test.go and blocksweep_test.go read (sweepBases), the helpers
// that build and check them, and the test of specgraphtest.As, through which some of them are built.

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph/specgraphtest"
)

// sweepBases is each base that the sweeps read, by its name (sweepBaseNames), each built alone by
// sweepBase from a read of its scenario of its own: the setup of each sweep. No code that builds one
// base holds another, so a writer of a base reaches its own base alone.
func sweepBases(t *testing.T) map[string]fstest.MapFS {
	t.Helper()
	bases := map[string]fstest.MapFS{}
	for _, name := range sweepBaseNames(t) {
		bases[name] = sweepBase(t, name)
	}
	return bases
}

// sweepBaseNames is the name of each base that the sweeps read, sorted: each scenario of
// testdata/scenarios, and each base that no scenario holds (builtBases).
func sweepBaseNames(t *testing.T) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join("testdata", "scenarios", "*"))
	if err != nil || len(dirs) < 7 {
		t.Fatalf("the scenarios: %q, %v", dirs, err)
	}
	var names []string
	for _, dir := range dirs {
		names = append(names, filepath.Base(dir))
	}
	for name := range builtBases() {
		if slices.Contains(names, name) {
			t.Fatalf("the built base %s has the name of a scenario", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// sweepBase is the base name, built alone: a scenario of testdata/scenarios, read from its files;
// or a base of builtBases, which its builder builds from a read of its scenario of its own, with no
// other base, and which its check reads back. A built base that reads, through the reader, as the
// base that it came from fails the test, so that a builder that changed nothing, or only the bytes
// of its files, gives no base.
func sweepBase(t *testing.T, name string) fstest.MapFS {
	t.Helper()
	b, built := builtBases()[name]
	if !built {
		dir := filepath.Join("testdata", "scenarios", name)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("%s: no base of that name (%v)", name, err)
		}
		return scenarioFS(t, dir)
	}
	fsys, from := b.build(t, scenarioFS(t, filepath.Join("testdata", "scenarios", b.scenario)))
	b.check(t, name, fsys)
	got, err := ReadScenario(fsys)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if want, err := ReadScenario(from); err != nil || reflect.DeepEqual(got, want) {
		t.Fatalf("%s: the reader reads it as the base that it came from (%v)", name, err)
	}
	return fsys
}

// builtBase is a base that no scenario holds (builtBases): the scenario that it is built from; its
// builder, which gets a read of that scenario of its own, changes it by writers of the package
// alone, and gives the base and the base that it came from (the scenario, or a conversion of it);
// and its check, which reads back the facts that the builder adds.
type builtBase struct {
	scenario string
	build    func(t *testing.T, fsys fstest.MapFS) (base, from fstest.MapFS)
	check    func(t *testing.T, name string, fsys fstest.MapFS)
}

// builtBases is each base that the sweeps read and no scenario holds, by its name: nineteen
// shapes. silent-leg with the job's matrix dropped after c1: one run of no matrix at m2 that ended
// with no result at m2, so the check reads unknown (ci_fault) there. Two of carry where a failure
// and a later pass count at one subject, so the check reads unknown there (disagree; R7: "A pass
// and a failure on one such subject and one procedure make the state unknown"): a failure of c2 at
// 10:03 on 12 November and later passes of c2, with a caught failing example planted on c2; and a
// failure of m2 at 09:15 on 13 November and later passes of m2, with a caught failing example
// planted on m2. A run of a check of the code counts only at the commit that it read (the rule that
// replaces The Spec's R8), so each of the two reads unknown at its own commit alone. A refusal of CI
// at a commit makes its check read unknown there and takes out, of the results of that commit, only
// those that support true (R48: "At that commit, such a check reads unknown"), so a rule by which a
// refusal took out a failure, or let newer evidence decide at the refused commit, would read the
// later pass true. And carry with a later failing example, at the commit f2, in a fault of its own
// of two paths, carry's and a second source file of the tie, on which the test missed its fault,
// with no planted_on: the check reads unknown (no_planted_on) at c2 and at m2, and
// the refusal sweep refuses the check at its own commit, f2, which must not take it out; the member
// sweep adds each planted_on that a record names and c9, no commit of the history. And the same
// later failing example planted on c9, which no commit of the history holds: it counts from no
// commit, so the check reads unknown (planted_on_outside_history) at c2 and at m2, and the refusal
// sweep refuses the check at c9 and at f2, neither of which takes it out (refusalsTakeOut names
// neither, so neither refusal has a right). And carry with its failing example in the form of a
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
// test_commit names takes out nothing of it (deriveChecks, step 2), and R7 decides. Of each two, one
// holds test_commit c1 on the check's failing run and the other CI's refusal of the check at c1:
// the refusal sweep adds the refusal to the one and the member sweep the test_commit to the other,
// so an input of each holds both facts, and a rule by which such a refusal took out that run would
// read the check's earlier pass true. And two of production-and-graph with its program check of
// production written as another kind: as a program check of the graph, whose runs read the
// transaction tx1 and count at the commit of their test, and as an agent of production, whose block
// names no test. And production-and-graph with a pass of its program check of production and a
// failing example that caught its fault, each of the test of m2, before the failure of the test of
// c1: the check reads false at m2 by that failure, which counts there by the ties. CI's refusal of
// the check at c1 takes out c1's pass and caught failing example and keeps that failure (R48), so
// the check still reads false, and a rule by which a refusal took out each result of its commit
// would read the pass of m2 true, a state that the refusal sweep compares (refusalsTakeOut). Each
// of the nineteen reads, at the subjects of its shape, the state that the shape gives its check
// (readsAt): the eight above the states that their shapes are for, the ten of production-and-graph
// their check of the graph, or the check that they write, in the state that the check's kind gives
// its results, and the last its program check of production false; the eight that write a check
// also read it as its kind (readsAs). A state does not show each fact that a builder adds (a base
// that holds a test_commit or a refusal reads the same state with that fact as without it, by the
// rule that the base is for), so each base built from carry, each of the eight of
// production-and-graph that hold a test_commit or a refusal, and the last also read back the facts
// that their builder adds, as the reader reads them, from a statement of those facts apart from the
// builder's input: each record that a builder of carry adds, with its kind, outcome, commit,
// planted_on and seq; a run of the check that failed and names test_commit c1, or CI's refusal of
// the check at c1; and the pass and the caught failing example of the test of m2, with their seq.
// Each builder writes its base's files by writers of the package (resultsAdded,
// lastPlantedOnLeftOut, ...), each a function that gets the tree of one file of that base and the
// plain data that it adds, so that no writer can name another base or this table. It is a function
// that gives a new table at each call, as each table that a sweep reads is (sweepMaps).
func builtBases() map[string]builtBase {
	const code, faultPath, secondPath = "a-restart-loses-no-message", "packages/envoy/cmd/listener/main.go", "packages/envoy/cmd/listener/store.go"
	const graph, program = "the-graph-holds-a-delivery-of-each-message", "a-production-restart-loses-no-message"
	disagree := CheckState{State: checkUnknown, Reason: reasonDisagree, Disagree: true}
	passedAtM1 := CheckState{State: CheckTrue, Reason: reasonPassed, From: "m1"}
	failedAtM1 := CheckState{State: CheckFalse, Reason: reasonFailed, From: "m1"}
	table := map[string]builtBase{
		"silent-leg with its matrix dropped at m2": {"silent-leg", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
			from := cloneFiles(fsys)
			fsys["jobs.json"] = &fstest.MapFile{Data: []byte(`[{"job":"go-checks","leg":"","commit":"m2","ended":true}]`)}
			return fsys, from
		}, func(t *testing.T, name string, fsys fstest.MapFS) {
			readsAt(t, name, fsys, code, CheckState{State: checkUnknown, Reason: reasonCIFault}, "m2")
		}},
		// The two of wait-cycle read, at m1, the own check of envoy-delivers-messages false and the
		// check of a-message-survives-a-restart true, or that check false and the one added beside it
		// true.
		"wait-cycle with the outcomes of its two first runs swapped": {"wait-cycle", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
			from := cloneFiles(fsys)
			fsys = editTree(t, fsys, "results.json", firstOutcomesSwapped())
			return fsys, from
		}, func(t *testing.T, name string, fsys fstest.MapFS) {
			readsAt(t, name, fsys, "a-sent-message-reaches-a-listener", failedAtM1, "m1")
			readsAt(t, name, fsys, "a-restart-keeps-the-message", passedAtM1, "m1")
		}},
		"wait-cycle with a second, passing check of a-message-survives-a-restart": {"wait-cycle", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
			from := cloneFiles(fsys)
			return withSecondCheck(t, fsys), from
		}, func(t *testing.T, name string, fsys fstest.MapFS) {
			readsAt(t, name, fsys, "a-restart-keeps-the-message", failedAtM1, "m1")
			readsAt(t, name, fsys, "a-restart-keeps-the-order", passedAtM1, "m1")
		}},
	}
	// has is whether s holds a result of code of kind with outcome, at commit, planted on plantedOn,
	// with seq.
	has := func(s Scenario, kind ResultKind, outcome Outcome, commit, plantedOn string, seq int64) bool {
		return slices.ContainsFunc(s.Results, func(r Result) bool {
			return r.Check == code && r.Kind == kind && r.Outcome == outcome && r.Commit == commit && r.PlantedOn == plantedOn && r.Seq == seq
		})
	}
	// Each base built from carry is one row: its name, the facts that its builder adds, the builder's
	// input (the records added to results.json, each a copy of carry's result 1, from 0, a pass of
	// a-restart-loses-no-message at c1, or 2, from 1, its caught failing example planted on c1, with
	// the members of its set, each after carry's own six records, and the writers of a further edit),
	// the state that it reads at each commit of at (readsAt): unknown (disagree) where a failure and
	// a later pass count; unknown (no_planted_on) with a failing example planted by no step, later or
	// caught; and unknown (planted_on_outside_history) with a later one planted on c9. And the
	// read-back of the facts as the reader reads them, stated apart from the input, so that a
	// builder, or an input of it, that adds nothing or another thing fails by name.
	for _, row := range []struct {
		name, facts string
		adds        []addedResult
		then        []fileEdit
		want        CheckState
		at          []string
		holds       func(Scenario) bool
	}{
		{"carry with c2's failure between c1's pass and c1's failing example",
			"a failure of c2, a later pass of c2 and a caught failing example planted on c2 at the commit f-c2",
			[]addedResult{
				{0, map[string]any{"commit": "c2", "outcome": "fail", "at": "2026-11-12T10:03:00Z", "seq": 7}},
				{0, map[string]any{"commit": "c2", "at": "2026-11-12T12:00:00Z", "seq": 8}},
				{1, map[string]any{"commit": "f-c2", "planted_on": "c2", "at": "2026-11-12T12:05:00Z", "seq": 9}}}, nil, disagree, []string{"c2"},
			func(s Scenario) bool {
				return has(s, ResultRun, OutcomeFail, "c2", "", 7) && has(s, ResultRun, OutcomePass, "c2", "", 8) && has(s, ResultFailingExample, OutcomeFail, "f-c2", "c2", 9)
			}},
		{"carry with m2's failure and a later pass of m2",
			"a failure of m2, a later pass of m2 and a caught failing example planted on m2 at the commit f-m2",
			[]addedResult{
				{0, map[string]any{"commit": "m2", "outcome": "fail", "at": "2026-11-13T09:15:00Z", "seq": 7}},
				{0, map[string]any{"commit": "m2", "at": "2026-11-13T12:00:00Z", "seq": 8}},
				{1, map[string]any{"commit": "f-m2", "planted_on": "m2", "at": "2026-11-13T12:05:00Z", "seq": 9}}}, nil, disagree, []string{"m2"},
			func(s Scenario) bool {
				return has(s, ResultRun, OutcomeFail, "m2", "", 7) && has(s, ResultRun, OutcomePass, "m2", "", 8) && has(s, ResultFailingExample, OutcomeFail, "f-m2", "m2", 9)
			}},
		{"carry with a later failing example planted by no step",
			"a failing example after carry's caught one, at the commit f2, with no planted_on and with carry's fault path and a second source file of the tie, on which the test missed its fault",
			[]addedResult{{1, map[string]any{"commit": "f2", "outcome": "pass", "seq": 7, "fault_paths": []any{faultPath, secondPath}}}},
			[]fileEdit{{"results.json", lastPlantedOnLeftOut}}, CheckState{State: checkUnknown, Reason: reasonNoPlantedOn}, []string{"c2", "m2"},
			func(s Scenario) bool {
				return slices.ContainsFunc(s.Results, func(r Result) bool {
					return r.Check == code && r.Kind == ResultFailingExample && r.Outcome == OutcomePass && r.Commit == "f2" && r.PlantedOn == "" &&
						slices.Equal(r.FaultPaths, []string{faultPath, secondPath}) && r.Seq == 7
				})
			}},
		{"carry with a later failing example planted on c9, no commit of the history",
			"a failing example after carry's caught one, at the commit f2, planted on c9, which the history does not hold, with carry's fault path and a second source file of the tie, on which the test missed its fault",
			[]addedResult{{1, map[string]any{"commit": "f2", "outcome": "pass", "seq": 7, "planted_on": "c9", "fault_paths": []any{faultPath, secondPath}}}}, nil,
			CheckState{State: checkUnknown, Reason: ReasonPlantedOnOutside, Detail: "c9"}, []string{"c2", "m2"},
			func(s Scenario) bool {
				_, held, err := s.History.Changed("c9", "c9")
				return err == nil && !held && slices.ContainsFunc(s.Results, func(r Result) bool {
					return r.Check == code && r.Kind == ResultFailingExample && r.Outcome == OutcomePass && r.Commit == "f2" && r.PlantedOn == "c9" &&
						slices.Equal(r.FaultPaths, []string{faultPath, secondPath}) && r.Seq == 7
				})
			}},
		{"carry with its failing example planted by no step",
			"a caught failing example at the commit c1 with no planted_on and no fault path",
			nil, []fileEdit{{"results.json", caughtFailingExampleUnplanted}}, CheckState{State: checkUnknown, Reason: reasonNoPlantedOn}, []string{"c2", "m2"},
			func(s Scenario) bool {
				return has(s, ResultFailingExample, OutcomeFail, "c1", "", 2) && !slices.ContainsFunc(s.Results, func(r Result) bool { return len(r.FaultPaths) > 0 })
			}},
	} {
		table[row.name] = builtBase{"carry", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
			from := cloneFiles(fsys)
			fsys = editTree(t, fsys, "results.json", resultsAdded(row.adds))
			for _, e := range row.then {
				fsys = editTree(t, fsys, e.file, e.writer())
			}
			return fsys, from
		}, func(t *testing.T, name string, fsys fstest.MapFS) {
			if s, err := ReadScenario(fsys); err != nil || !row.holds(s) {
				t.Fatalf("%s: the reader does not read the facts that its builder adds, %s (%v)", name, row.facts, err)
			}
			readsAt(t, name, fsys, code, row.want, row.at...)
		}}
	}
	// written is production-and-graph, fsys, with its check id written as a check of procedure and
	// subject by the one conversion that the command's tests read it through too (specgraphtest.As):
	// its text in the approved spec and in the files at m2, for a check of production each run of it
	// that read the transaction tx1 reading the deploy d1 of the subject instead, as a check of
	// production reads a deploy, and for a check of the graph each run of it that read the deploy d1
	// reading the transaction tx1 instead. The approved spec and the files read the check as that
	// kind (readsAs).
	written := func(id string, procedure Procedure, subject Subject) func(*testing.T, fstest.MapFS) fstest.MapFS {
		return func(t *testing.T, fsys fstest.MapFS) fstest.MapFS {
			out, err := specgraphtest.As(fsys, id, string(procedure), string(subject))
			if err != nil {
				t.Fatalf("production-and-graph with %s written with procedure %q and subject %q: %v", id, procedure, subject, err)
			}
			readsAs(t, fmt.Sprintf("production-and-graph with %s written with procedure %q and subject %q", id, procedure, subject), out, id, procedure, subject)
			return out
		}
	}
	// Each base of production-and-graph reads its check of the graph, or its program check of
	// production as written, in the state that its kind gives the scenario's results of it, a pass
	// at 10:00, a caught failing example and a failure at 11:00 (readsAt), so that a conversion that
	// gives the check its kind and results that read another subject, or no result that counts,
	// gives no base: a check of the graph unknown, since its pass and its failure at the transaction
	// tx1 disagree (R7), and one of production false, since its newest run decides, at the deploy d1.
	failed := CheckState{State: CheckFalse, Reason: reasonFailed, From: "d1"}
	type converted struct {
		convert func(*testing.T, fstest.MapFS) fstest.MapFS // nil for production-and-graph as it is
		want    CheckState
	}
	// Each kind of check that reads no commit gives two bases, one for each row below: the row's
	// builder adds its fact to the base of that kind, and the read-back reads that fact as the reader
	// reads it, so that a builder that adds nothing fails by name.
	addsTo := []struct {
		how   string
		build func(t *testing.T, fsys fstest.MapFS) fstest.MapFS
		holds func(Scenario) bool
	}{
		{"with test_commit c1 on the failing run of ", func(t *testing.T, fsys fstest.MapFS) fstest.MapFS {
			return editTree(t, fsys, "results.json", testCommitOnFailingRuns(graph))
		}, func(s Scenario) bool {
			return slices.ContainsFunc(s.Results, func(r Result) bool {
				return r.Check == graph && r.Kind == ResultRun && r.Outcome == OutcomeFail && r.TestCommit == "c1"
			})
		}},
		{"with CI's refusal at c1 of ", func(t *testing.T, fsys fstest.MapFS) fstest.MapFS {
			fsys["uncited.json"] = &fstest.MapFile{Data: []byte(`{"c1": {"` + graph + `": "test-not-in-code"}}`)}
			return fsys
		}, func(s Scenario) bool { return s.Uncited["c1"][graph] == "test-not-in-code" }},
	}
	for what, kind := range map[string]converted{
		"its query of the graph":                           {nil, disagree},
		"its check of the graph as an agent":               {written(graph, ProcedureAgent, SubjectGraph), disagree},
		"its check of the graph as a query of production":  {written(graph, ProcedureQuery, SubjectProduction), failed},
		"its check of the graph as an agent of production": {written(graph, ProcedureAgent, SubjectProduction), failed},
	} {
		for _, row := range addsTo {
			table["production-and-graph "+row.how+what] = builtBase{"production-and-graph", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
				if kind.convert != nil {
					fsys = kind.convert(t, fsys)
				}
				from := cloneFiles(fsys)
				fsys = row.build(t, fsys)
				return fsys, from
			}, func(t *testing.T, name string, fsys fstest.MapFS) {
				readsAt(t, name, fsys, graph, kind.want, "m2")
				if s, err := ReadScenario(fsys); err != nil || !row.holds(s) {
					t.Fatalf("%s: the reader does not read the fact that the base is built to hold (%v)", name, err)
				}
			}}
		}
	}
	for what, kind := range map[string]converted{
		"as a program check of the graph": {written(program, ProcedureProgram, SubjectGraph), disagree},
		"as an agent of production":       {written(program, ProcedureAgent, SubjectProduction), failed},
	} {
		table["production-and-graph with its program check of production "+what] = builtBase{"production-and-graph", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
			from := cloneFiles(fsys)
			return kind.convert(t, fsys), from
		}, func(t *testing.T, name string, fsys fstest.MapFS) {
			readsAt(t, name, fsys, program, kind.want, "m2")
		}}
	}
	// The last base adds to production-and-graph's records of its program check of production, a
	// pass under d1 at 10:00, a caught failing example at 10:05 and a failure under d1 at 11:00, each
	// of the test of c1, a copy of the pass at 10:30 and of the caught failing example at 10:35,
	// each of the test of m2.
	table["production-and-graph with a pass and a caught failing example of the test of m2 before the failure of the test of c1"] = builtBase{"production-and-graph", func(t *testing.T, fsys fstest.MapFS) (fstest.MapFS, fstest.MapFS) {
		from := cloneFiles(fsys)
		return editTree(t, fsys, "results.json", resultsAdded([]addedResult{
			{0, map[string]any{"test_commit": "m2", "at": "2026-11-12T10:30:00Z", "seq": 7}},
			{1, map[string]any{"test_commit": "m2", "at": "2026-11-12T10:35:00Z", "seq": 8}}})), from
	}, func(t *testing.T, name string, fsys fstest.MapFS) {
		of := func(s Scenario, kind ResultKind, outcome Outcome, deploy string, seq int64) bool {
			return slices.ContainsFunc(s.Results, func(r Result) bool {
				return r.Check == program && r.Kind == kind && r.Outcome == outcome && r.Deploy == deploy && r.TestCommit == "m2" && r.Seq == seq
			})
		}
		if s, err := ReadScenario(fsys); err != nil || !of(s, ResultRun, OutcomePass, "d1", 7) || !of(s, ResultFailingExample, OutcomeFail, "", 8) {
			t.Fatalf("%s: the reader does not read the pass of the test of m2 under d1 of seq 7 and its caught failing example of seq 8 (%v)", name, err)
		}
		readsAt(t, name, fsys, program, failed, "m2")
	}}
	return table
}

// fileEdit is one edit of a base that builtBases states: the JSON file that it writes, and the
// function of the package that gives its writer.
type fileEdit struct {
	file   string
	writer func() treeEdit
}

// addedResult is a record that resultsAdded adds to results.json: a copy of the record at from with
// the members of set.
type addedResult struct {
	from int
	set  map[string]any
}

// resultsAdded is the writer of results.json that adds to the tree that it is handed a record for
// each of adds.
func resultsAdded(adds []addedResult) treeEdit {
	return func(v any) any {
		results := v.([]any)
		for _, a := range adds {
			r := maps.Clone(results[a.from].(map[string]any))
			maps.Copy(r, a.set)
			results = append(results, r)
		}
		return results
	}
}

// lastPlantedOnLeftOut is the writer of results.json that leaves the planted_on of its last record
// out; caughtFailingExampleUnplanted is the one that writes carry's caught failing example, its
// second record, at the commit that it was planted on, with no planted_on and no fault path.
func lastPlantedOnLeftOut() treeEdit {
	return func(v any) any {
		results := v.([]any)
		delete(results[len(results)-1].(map[string]any), "planted_on")
		return results
	}
}

func caughtFailingExampleUnplanted() treeEdit {
	return func(v any) any {
		fault := v.([]any)[1].(map[string]any)
		fault["commit"] = fault["planted_on"]
		delete(fault, "planted_on")
		delete(fault, "fault_paths")
		return v
	}
}

// firstOutcomesSwapped is the writer of wait-cycle's results.json that writes its run of seq 1 as
// a failure and its run of seq 5 as a pass.
func firstOutcomesSwapped() treeEdit {
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
}

// testCommitOnFailingRuns is the writer of results.json that writes test_commit c1 on each failing
// run of the check id.
func testCommitOnFailingRuns(id string) treeEdit {
	return func(v any) any {
		for _, r := range v.([]any) {
			if r := r.(map[string]any); r["check"] == id && r["kind"] == "run" && r["outcome"] == "fail" {
				r["test_commit"] = "c1"
			}
		}
		return v
	}
}

// withSecondCheck is wait-cycle, base, with a-restart-keeps-the-order, a check of the code that
// passes, under a-message-survives-a-restart beside its failing check: a copy of
// a-late-session-gets-the-message with its results and its ties. The document of the copy is built
// from a decode of base's approved.json of its own, before any writer runs, and written by the
// writers of the package (nodeCopied, resultsCopied, tiesCopied) and in m1's spec file.
func withSecondCheck(t *testing.T, base fstest.MapFS) fstest.MapFS {
	t.Helper()
	const old, added = "a-late-session-gets-the-message", "a-restart-keeps-the-order"
	fsys := maps.Clone(base)
	rename := strings.NewReplacer("id: "+old, "id: "+added, "serves: a-message-reaches-a-late-session", "serves: a-message-survives-a-restart")
	var document string
	for _, n := range fileTree(t, base, "approved.json").(map[string]any)["nodes"].([]any) {
		if n := n.(map[string]any); n["id"] == old {
			document = rename.Replace(n["versions"].([]any)[0].(map[string]any)["document"].(string))
			break
		}
	}
	if document == "" {
		t.Fatalf("wait-cycle's approved.json holds no document of %s", old)
	}
	fsys = editTree(t, fsys, "approved.json", nodeCopied(old, added, document))
	fsys["files/m1/spec.yaml"] = &fstest.MapFile{Data: append(slices.Clone(fsys["files/m1/spec.yaml"].Data), document...)}
	fsys = editTree(t, fsys, "results.json", resultsCopied(old, added))
	fsys = editTree(t, fsys, "ties.json", tiesCopied(old, added))
	return fsys
}

// nodeCopied is the writer of approved.json that adds a copy of the node old, as the node added of
// one version, its first, with the document document.
func nodeCopied(old, added, document string) treeEdit {
	return func(v any) any {
		nodes := v.(map[string]any)["nodes"].([]any)
		for _, n := range nodes {
			if n := n.(map[string]any); n["id"] == old {
				version := maps.Clone(n["versions"].([]any)[0].(map[string]any))
				version["document"] = document
				c := maps.Clone(n)
				c["id"], c["versions"] = added, []any{version}
				nodes = append(nodes, c)
				break
			}
		}
		v.(map[string]any)["nodes"] = nodes
		return v
	}
}

// resultsCopied is the writer of results.json that adds a copy of each result of the check old as
// one of the check added, with the next seq; tiesCopied is the writer of ties.json that gives added
// the tie of old at each commit.
func resultsCopied(old, added string) treeEdit {
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
}

func tiesCopied(old, added string) treeEdit {
	return func(v any) any {
		for _, ties := range v.(map[string]any) {
			ties := ties.(map[string]any)
			ties[added] = ties[old]
		}
		return v
	}
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
