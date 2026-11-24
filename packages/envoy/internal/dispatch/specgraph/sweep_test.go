package specgraph

import (
	"bytes"
	"crypto/sha256"
	"encoding/json/v2"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// sweepMaps is each object of the JSON files of a scenario whose names its writer picks, as the
// forms write them ({"<repository path>":"<content label>"}, {"<commit>":{"<check id>":…}}), by its
// place: the names of the path to it from the root of the file, "*" for any name or index. A member
// of such an object is an entry of a map: a record of the scenario, which the sweep puts null in the
// place of and TestTakingOutAWholeRecordMovesAStateOnlyByItsRight takes out, and whose key the
// sweep writes as a name that names nothing. Like each table that a sweep reads, it is a function
// that gives a new value at each call, not a variable of the package.
func sweepMaps() map[string][][]string {
	return map[string][][]string{
		"history.json": {{"commits", "*", "files"}},
		"ties.json":    {{}, {"*"}},
		"uncited.json": {{}, {"*"}},
	}
}

// sweepPaths is each place of a repository path in the JSON files of a scenario: the keys of a tree
// of the history, the entries of a tie, and the paths of a fault of a result. The sweep writes each
// such path in the forms that name no repository path, which no tie entry covers.
func sweepPaths() map[string]struct{ keysOf, values [][]string } {
	return map[string]struct{ keysOf, values [][]string }{
		"history.json": {keysOf: [][]string{{"commits", "*", "files"}}},
		"ties.json":    {values: [][]string{{"*", "*", "*"}}},
		"results.json": {values: [][]string{{"*", "fault_paths", "*"}}},
	}
}

// namesOfNothing are the texts that the sweep writes at each string place and as each key of a map:
// no name, and a name of no letter and no digit.
func namesOfNothing() []string { return []string{"", " "} }

// notRepoPaths are the forms of path that name no repository path, as the sweep writes them.
func notRepoPaths(path string) []string {
	forms := []string{"./" + path, "/" + path, path + "/"}
	if strings.Contains(path, "/") {
		forms = append(forms, strings.Replace(path, "/", "//", 1))
	}
	return forms
}

// An input that says nothing at one place reads as the zero value of that place, a real value, and
// a rule that read it as one would turn a state toward true. So the sweep reads each JSON file of each
// scenario of testdata/scenarios as a JSON tree, with no Go type, and derives the state of each input
// that says nothing at one place: each member of each object of the format left out, null at each
// place (a member, an entry of a map, an element of a list, the whole file), each file of the
// scenario taken out; each name that names nothing: "" and " " at each string place and as each
// key of each map of the format (sweepMaps), and each repository path of a tree, a tie or a fault
// written as "./<path>", "/<path>", "<path>/" and with a "/" doubled (sweepPaths). No such input
// may move a check toward true, a statement or its own state toward met, or a wait toward ends,
// against the state of the whole scenario: a member with a mark of zero is no exception, and a mark
// whose reading moves a state toward true is wrong. The reader may refuse the input. A record taken
// out whole, such as a result, is another input, not one that says nothing
// (TestTakingOutAWholeRecordMovesAStateOnlyByItsRight), and so is a named leg written where a
// record has "": it states that the job had legs there, which an older result of that leg answers
// by right (R8). The scenarios hold each shape that a rule reads: a check of the code with a matrix
// and a silent leg, its failing example planted on a commit (planted_on, fault_paths), a wait on
// its statement, a failure of production at the at of its pass, a refusal of CI, and, in
// production-and-graph, a program check of production whose runs name the commit of their test
// (test_commit), a query of the graph at the transaction of its subject (graph_tx), and a run of a
// job that has not ended. So each member that a rule reads is in a record of some scenario, and
// each place of it is swept. The sweep also reads the shapes that no scenario holds (sweepBases).
// Each sweep fails an input whose files are its base's (sweepAgainst), so that a writer that writes
// nothing fails by name, and each sweep reads back each write that it makes against the write that
// it means (filesRead): each file by its name, with each other file of the base unchanged and no
// file added; a JSON file as the tree of the base's file with the one change that the input names,
// compared whole (changedFile); a text file by its bytes (filesWritten); and a refusal of CI as the
// reader reads it (refusalRead). The sweep builds what it means before its writer runs, from a
// decode of the base of its own; a writer is a function of the package, which gets a tree or files
// of its own and copies of the place and the value (changedFile, filesWritten). So a writer that
// writes another value, writes it at another place, under another key or under another file name,
// or changes another place of the file too, fails by name. Each sweep derives each input in the
// test process (scenarioStates), judges it against its base (sweepAgainst), and reads and refuses
// at least its floors of inputs of each base (sweepFloors), so that a sweep that builds no input,
// or drops some, fails by name too. One right, which sweepInputs gives the input where it writes
// it: a repository path of a tree written " " is a real path, since git holds a file of that name
// (git ls-tree of a tree that holds one lists the path " "), so the tree changed at a path that a
// tie covers, and a result there, a failure among them, carries no more across it (R8); a base
// whose failure at one commit carries to another shows it. A path of a fault written " " is no
// path, as one of only white space is none, so its record does not say what its fault changed, and
// as the newest failing example it reads unknown, no_fault_paths where it was the one path and
// blank_fault_path beside a real one: it has no right, and a base whose failing example of two
// paths that missed its fault is newer than a caught one shows that it needs none. A content label
// is no path, and one of " " names no content, so the reader refuses it and it has no right.
func TestNoInputThatSaysNothingMovesAStateTowardTrue(t *testing.T) {
	t.Parallel()
	bases := sweepBases(t)
	floorsCover(t, "tree", bases)
	for _, name := range slices.Sorted(maps.Keys(bases)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := bases[name]
			inputs, roundTrip := sweepInputs(t, base)
			if got, err := scenarioStates(roundTrip); err != nil || !maps.Equal(got, baseStates(t, base)) {
				t.Fatalf("each JSON file of the scenario written again by the sweep does not read as the scenario: %v", err)
			}
			n := sweepAgainst(t, base, inputs)
			atFloor(t, "tree", name, n)
			legs := 0 // the inputs that write the leg of no matrix at the leg of a record
			for _, in := range inputs {
				if strings.HasSuffix(in.what, `/leg as ""`) {
					legs++
				}
			}
			t.Logf(`%d inputs: %d refused, %d read, %d of them toward true by right; %d of them a leg written as ""`,
				n.inputs, n.refused, n.read, n.byRight, legs)
		})
	}
}

// baseStates is the state of each check, statement and wait of the base (scenarioStates).
func baseStates(t *testing.T, base fstest.MapFS) map[string]string {
	t.Helper()
	whole, err := scenarioStates(base)
	if err != nil {
		t.Fatalf("the base: %v", err)
	}
	return whole
}

// sweepCount is what a sweep read of the inputs that it built (sweepAgainst): how many it built, how
// many of them the reader or the derivation refused, how many it read with no state moved toward
// true beyond the input's right, and how many of those moved a state toward true by their right,
// with what each of them changed. An input that fails the judgment fails the test and is counted
// nowhere.
type sweepCount struct {
	inputs, refused, read, byRight int
	rights                         []string
}

// sweepAgainst derives base and each of its inputs in the test process (scenarioStates) and judges
// each input that the reader and the derivation read against the base (judgeAnswer): an input that
// moves a state toward true beyond its right fails the test by its line. A right with allowed covers
// only the places where the input reads the state that allowed holds there. Two rights name no
// place, so an input that has one is judged by its right alone: a repository path of a tree written
// " " (pathOfOneSpace), whose tree then changed at a path that a tie covers, so the carry across that
// commit ends at each subject after it; and a parent left out of an approved block (parentLeftOut),
// whose statement then sits at the top of the tree, so the statement above it loses it, which can
// leave that one and each above it met. The places that such an input may move are those that the
// derivation itself reaches from its change, so a list of them would be a second derivation. An
// input whose files are the base's (sameFiles) fails the test too: its writer wrote nothing, and it
// would read as the base and pass. So does an input whose files are those of an earlier input of the
// list, byte for byte (filesKey): the sweep would read one case twice and the case that it means not
// at all, as when a writer or a table turned one input into another.
func sweepAgainst(t *testing.T, base fstest.MapFS, inputs []sweepInput) sweepCount {
	t.Helper()
	whole := baseStates(t, base)
	n := sweepCount{inputs: len(inputs)}
	seen := map[[sha256.Size]byte]string{}
	for _, in := range inputs {
		if sameFiles(t, base, in.fsys) {
			t.Errorf("%s: the input's files are the base's, so the sweep wrote nothing", in.what)
			continue
		}
		key := filesKey(in.fsys)
		if first, held := seen[key]; held {
			t.Errorf("%s: the input's files are those of the input %s, byte for byte, so the sweep reads one case twice", in.what, first)
			continue
		}
		seen[key] = in.what
		got, err := scenarioStates(in.fsys)
		if err != nil {
			n.refused++
			continue
		}
		toward, beyond, err := judgeAnswer(whole, got, in.right != "", in.allowed)
		switch {
		case err != nil:
			t.Errorf("%s: %v", in.what, err)
		case len(beyond) > 0 && in.right != "":
			t.Errorf("%s, beyond its right (%s): %s", in.what, in.right, strings.Join(beyond, "; "))
		case len(beyond) > 0:
			t.Errorf("%s: %s", in.what, strings.Join(beyond, "; "))
		default:
			n.read++
			if len(toward) > 0 {
				n.byRight++
				n.rights = append(n.rights, in.what)
			}
		}
	}
	return n
}

// filesKey is the sha256 of the files of the scenario fsys, each by its name and the length and the
// bytes of its data, in the order of the names: two scenarios have one key where they hold the same
// files, byte for byte.
func filesKey(fsys fstest.MapFS) [sha256.Size]byte {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(fsys)) {
		fmt.Fprintf(h, "%q %d\n", name, len(fsys[name].Data))
		h.Write(fsys[name].Data)
	}
	return [sha256.Size]byte(h.Sum(nil))
}

// sameFiles is whether the scenarios a and b hold the same files with the same contents: each JSON
// file as its decoded tree, since the base's files are as their author wrote them and a write is as
// encodeTree writes it, and each other file by its bytes.
func sameFiles(t *testing.T, a, b fstest.MapFS) bool {
	t.Helper()
	if !slices.Equal(slices.Sorted(maps.Keys(a)), slices.Sorted(maps.Keys(b))) {
		return false
	}
	for name, file := range a {
		switch {
		case bytes.Equal(file.Data, b[name].Data):
			continue
		case path.Ext(name) != ".json":
			return false
		}
		var x, y any
		if json.Unmarshal(file.Data, &x) != nil || json.Unmarshal(b[name].Data, &y) != nil || !reflect.DeepEqual(x, y) {
			return false
		}
	}
	return true
}

// A refusal of CI at a commit A makes its check read unknown at A, whatever else holds there (R48:
// "At that commit, such a check reads unknown, and no earlier result of it counts"), and it takes
// the results of the check that refusalsTakeOut states for its base out of the derivation. So adding
// one moves no check toward true, no statement or its own state toward met, and no wait toward ends,
// but by one right: where the refusal takes results out, the results that stay can decide, and they
// can read true (refusalTakesItsResultsOut). The right covers each place of a subject at another
// commit than A where the input reads what the base reads with those results taken out of
// results.json (withoutResultsOf), and no place at A, where R48 gives none. A refusal that the table
// does not name takes out nothing, and has no right. The sweep adds one refusal at a time to
// uncited.json of each base (sweepBases), at each commit that a record of the base names, for each
// check of its approved spec that the base does not refuse there, and reads each one back as the
// reader reads it, against the refusals that it means, built before its writer runs (refusalAdded,
// refusalRead). A row of the table that names no refusal that the sweep adds, a base that is no base
// of the sweeps, and a seq that names no one result of the row's check fail the test by name.
func TestAddingARefusalOfCIMovesAStateTowardTrueOnlyByTakingOutItsCommitsResults(t *testing.T) {
	t.Parallel()
	bases := sweepBases(t)
	floorsCover(t, "refusals", bases)
	takesOut := refusalsTakeOut()
	for _, name := range slices.Sorted(maps.Keys(takesOut)) {
		if _, held := bases[name]; !held {
			t.Errorf("refusalsTakeOut states the refusals of %s, which is no base of the sweeps", name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(bases)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := bases[name]
			s, err := ReadScenario(base)
			if err != nil {
				t.Fatalf("the base: %v", err)
			}
			current, _ := s.Approved.Current()
			rows := takesOut[name]
			added := make([]bool, len(rows))
			var inputs []sweepInput
			for _, commit := range namesOf(t, base)["commit"] {
				for _, c := range current.Checks {
					if _, refused := s.Uncited[commit][c.ID]; refused {
						continue
					}
					fsys := refusalAdded(t, base, commit, c.ID)
					in := sweepInput{what: fmt.Sprintf("uncited.json: a refusal of %s at %s", c.ID, commit), fsys: fsys}
					if i := slices.IndexFunc(rows, func(r refusalOut) bool { return r.commit == commit && r.check == c.ID }); i >= 0 {
						added[i] = true
						in.right, in.allowed = refusalTakesItsResultsOut, withoutResultsOf(t, base, rows[i])
					}
					inputs = append(inputs, in)
				}
			}
			for i, r := range rows {
				if !added[i] {
					t.Errorf("refusalsTakeOut states that a refusal of %s at %s takes out the results of seq %v, and the sweep adds no such refusal, or the row is there twice", r.check, r.commit, r.seqs)
				}
			}
			n := sweepAgainst(t, base, inputs)
			atFloor(t, "refusals", name, n)
			t.Logf("%d refusals added: %d refused, %d read, %d of them toward true by right: %q",
				n.inputs, n.refused, n.read, n.byRight, n.rights)
		})
	}
}

// refusalOut is one row of refusalsTakeOut: CI's refusal of the check check at the commit commit, and
// the seq of each result of that check that the refusal takes out of the derivation.
type refusalOut struct {
	commit, check string
	seqs          []int64
}

// refusalsTakeOut is, for each base of the sweeps (sweepBases) by its name, each refusal of CI that
// the refusal sweep adds and that takes results out of the derivation, with those results by their
// seq in the base's results.json. It is data, written from the records of each base, and not a
// second reading of the rule that the derivation applies: a change of what a refusal takes out is a
// change of these rows. A refusal that it does not name takes out nothing. It is a function that
// gives a new table at each call.
func refusalsTakeOut() map[string][]refusalOut {
	const code, program = "a-restart-loses-no-message", "a-production-restart-loses-no-message"
	// The rows that several bases share: of carry and of most bases built from it, a refusal of its
	// check at c1 takes out its pass at c1 (seq 1) and its failing example planted on c1 (seq 2), and
	// one at c3 its pass at c3 (seq 3); of each base of production-and-graph that keeps its program
	// check of production, a refusal at c1 takes out that check's three results, each of test_commit
	// c1; and of each base of wait-cycle, a refusal at b1 takes out the run and the failing example of
	// each of its three checks there.
	carry := []refusalOut{{"c1", code, []int64{1, 2}}, {"c3", code, []int64{3}}}
	production := []refusalOut{{"c1", program, []int64{1, 2, 3}}}
	waitCycle := []refusalOut{
		{"b1", "a-sent-message-reaches-a-listener", []int64{1, 2}},
		{"b1", "a-late-session-gets-the-message", []int64{3, 4}},
		{"b1", "a-restart-keeps-the-message", []int64{5, 6}},
	}
	return map[string][]refusalOut{
		"carry": carry,
		"carry with a failure of c0, c1's parent, and a later pass of c2": slices.Concat(carry,
			[]refusalOut{{"c0", code, []int64{5}}, {"c2", code, []int64{6, 7}}}),
		"carry with a later failing example planted by no step":                      carry,
		"carry with a later failing example planted on c1 that missed its fault":     {{"c1", code, []int64{1, 2, 5}}, {"c3", code, []int64{3}}},
		"carry with a later failing example planted on c9, no commit of the history": carry,
		"carry with c2's failure at 10:30 and a later pass of m2": slices.Concat(carry,
			[]refusalOut{{"c2", code, []int64{5}}, {"m2", code, []int64{6, 7}}}),
		"carry with c2's failure between c1's pass and c1's failing example": slices.Concat(carry,
			[]refusalOut{{"c2", code, []int64{5, 6, 7}}}),
		"carry with its failing example planted by no step": {{"c1", code, []int64{1}}, {"c3", code, []int64{3}}},
		"earlier-text": {{"c0", "messages-come-in-the-order-of-their-writes", []int64{1, 2}}},
		"one-commit-two-answers": {
			{"c1", "relay-answers-within-the-limit", []int64{1, 2}},
			{"c1", "relay-answers-on-linux-and-arm", []int64{3, 4, 5}},
			{"c1", "relay-answers-once-per-request", []int64{6, 7, 8}},
			{"c1", "relay-refuses-a-malformed-request", []int64{9, 10}},
		},
		"production-and-graph": production,
		"production-and-graph with CI's refusal at c1 of its check of the graph as a query of production":                 production,
		"production-and-graph with CI's refusal at c1 of its check of the graph as an agent":                              production,
		"production-and-graph with CI's refusal at c1 of its check of the graph as an agent of production":                production,
		"production-and-graph with CI's refusal at c1 of its query of the graph":                                          production,
		"production-and-graph with its program check of production as a program check of the graph":                       production,
		"production-and-graph with test_commit c1 on the failing run of its check of the graph as a query of production":  production,
		"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent":               production,
		"production-and-graph with test_commit c1 on the failing run of its check of the graph as an agent of production": production,
		"production-and-graph with test_commit c1 on the failing run of its query of the graph":                           production,
		"refusal":    carry,
		"silent-leg": {{"c1", code, []int64{1, 2, 3}}},
		"silent-leg with its matrix dropped at m2": {{"c1", code, []int64{1, 2, 3}}},
		"wait-cycle": waitCycle,
		"wait-cycle with a second, passing check of a-message-survives-a-restart": slices.Concat(waitCycle,
			[]refusalOut{{"b1", "a-restart-keeps-the-order", []int64{7, 8}}}),
		"wait-cycle with the outcomes of its two first runs swapped": waitCycle,
	}
}

// refusalTakesItsResultsOut is the right of a refusal of CI that takes results of its check out of
// the derivation (refusalsTakeOut): those results count nowhere, and the results that stay decide.
const refusalTakesItsResultsOut = `the results that the refusal takes out count nowhere, and the results that stay decide (R48: "no earlier result of it counts")`

// withoutResultsOf is the states of base (scenarioStates) with each result that the refusal of row
// takes out (refusalsTakeOut) taken out of results.json, and with no state of a subject at the row's
// commit: the places that the refusal may move toward true, each to the state given here. Each seq of
// the row names one result of base, of the row's check, or the test fails by the row. The results
// that it means, as the reader reads them, and the place of each record to take out come from a
// decode of base of their own, made before the writer runs; the writer (resultsWithout) is handed a
// copy of those places and the tree of results.json decoded for it alone, and the reader reads its
// write back.
func withoutResultsOf(t *testing.T, base fstest.MapFS, row refusalOut) map[string]string {
	t.Helper()
	s := decodedScenario(t, base)
	var out []int
	for _, seq := range row.seqs {
		of := func(r Result) bool { return r.Seq == seq }
		at := slices.IndexFunc(s.Results, of)
		if at < 0 || s.Results[at].Check != row.check || slices.ContainsFunc(s.Results[at+1:], of) {
			t.Fatalf("refusalsTakeOut states that a refusal of %s at %s takes out the result of seq %d, which is no one result of that check", row.check, row.commit, seq)
		}
		out = append(out, at)
	}
	want := make([]Result, 0, len(s.Results))
	for i, r := range s.Results {
		if !slices.Contains(out, i) {
			want = append(want, r)
		}
	}
	fsys := editTree(t, base, "results.json", resultsWithout(slices.Clone(out)))
	if read, err := ReadScenario(fsys); err != nil || !reflect.DeepEqual(read.Results, want) {
		t.Fatalf("the sweep took the results of seq %v out of results.json, and the reader reads %d results (%v), where it should read %d", row.seqs, len(read.Results), err, len(want))
	}
	states, err := scenarioStates(fsys)
	if err != nil {
		t.Fatalf("the base with the results of seq %v taken out: %v", row.seqs, err)
	}
	for i, subject := range s.Subjects {
		if subject.Commit == row.commit {
			maps.DeleteFunc(states, func(at, _ string) bool { return strings.HasPrefix(at, fmt.Sprintf("subject %d ", i+1)) })
		}
	}
	return states
}

// resultsWithout is the writer of withoutResultsOf: the tree of results.json that it is handed
// without each record at a place of out.
func resultsWithout(out []int) treeEdit {
	return func(v any) any {
		kept := []any{}
		for i, record := range v.([]any) {
			if !slices.Contains(out, i) {
				kept = append(kept, record)
			}
		}
		return kept
	}
}

// decodedScenario is the scenario of base as ReadScenario reads it, decoded for its caller alone.
func decodedScenario(t *testing.T, base fstest.MapFS) Scenario {
	t.Helper()
	s, err := ReadScenario(base)
	if err != nil {
		t.Fatalf("the base: %v", err)
	}
	return s
}

// plantedOnElsewhere is the right of a planted_on added to a failing example where it names a commit
// of the history: R8 decides whether that record counts at a subject, and where it does not, the
// older evidence decides, as with no such record.
const plantedOnElsewhere = `a planted_on that names a commit of the history counts only where R8 carries it; elsewhere older evidence decides`

// withoutResult is the states of base with result i of results.json taken out (scenarioStates).
func withoutResult(t *testing.T, base fstest.MapFS, i int) map[string]string {
	t.Helper()
	fsys := changedFile(t, base, "results.json", treeChange{place: []string{strconv.Itoa(i)}, out: true}, leftOutAt)
	states, err := scenarioStates(fsys)
	if err != nil {
		t.Fatalf("the base with result %d taken out: %v", i+1, err)
	}
	return states
}

// addableMembers is each member that a record of a JSON file of a scenario can leave out (the
// twelve members marked zero that a file can leave out, memberMark), by the place of the list of
// its records and the kind of the values that the sweep gives it.
func addableMembers() map[string]addableRecords {
	return map[string]addableRecords{
		"results.json": {nil, [][2]string{{"commit", "commit"}, {"deploy", "deploy"}, {"graph_tx", "graph_tx"}, {"job", "job"},
			{"attempt", "attempt"}, {"fault_author", "author"}, {"test_authors", "authors"}, {"test_commit", "commit"},
			{"planted_on", "commit"}, {"fault_paths", "paths"}}},
		"subjects.json": {nil, [][2]string{{"graph_tx", "graph_tx"}}},
		"history.json":  {[]string{"commits"}, [][2]string{{"parent", "commit"}}},
	}
}

// addableRecords is the records of a JSON file to which the member sweep adds members: the place of
// their list, and each member with its kind (addableMembers).
type addableRecords struct {
	records []string
	members [][2]string // each member and its kind
}

// A member that a record leaves out, added, states one more fact of the record. No such input may
// move a state toward true: the sweep adds each member of addableMembers to each record of each base
// (sweepBases) that leaves it out, one at a time, with each value of its kind that a record of the
// base names and one that none names. A test_commit on a result of a check of the code moves
// nothing, since the commit of a result has one reading, its commit (resultCommit), for the judge
// and for the leg rule alike; one on a result of a query or an agent of production or of the graph
// moves nothing, also where CI refused the check at that commit, since such a check reads no commit
// (CountsAtACommit); and a commit on a result of a check of production or of the graph is refused.
// A planted_on alone, or fault_paths alone, on a failing example of a program check of the code
// that no step planted moves no state: it counts from planted_on only with both, and with one of
// the two it reads unknown with the reason that names the other (CheckStates, step 7). A planted_on
// that names a commit of the history has one right: where R8 does not carry that commit to a
// subject, the record counts nowhere there and the base without that record decides
// (plantedOnElsewhere). A planted_on that names no commit of the history has none: such a record
// reads unknown, planted_on_outside_history.
func TestAddingAMemberMovesNoStateTowardTrue(t *testing.T) {
	t.Parallel()
	bases := sweepBases(t)
	floorsCover(t, "members", bases)
	for _, name := range slices.Sorted(maps.Keys(bases)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := bases[name]
			s, err := ReadScenario(base)
			if err != nil {
				t.Fatalf("the base: %v", err)
			}
			names := namesOf(t, base)
			values := map[string][]any{"attempt": {2}}
			for kind, other := range map[string]string{"commit": "c9", "deploy": "d9", "graph_tx": "g9", "job": "other-job", "author": "other-author", "path": "packages/envoy/cmd/other/main.go"} {
				for slices.Contains(names[kind], other) {
					other += "x" // the value that no record of the base names: carry with a failing example planted on c9 names c9
				}
				for _, v := range append(slices.Clone(names[kind]), other) {
					values[kind] = append(values[kind], v)
					switch kind {
					case "author":
						values["authors"] = append(values["authors"], []any{v})
					case "path":
						values["paths"] = append(values["paths"], []any{v})
					}
				}
			}
			var inputs []sweepInput
			addable := addableMembers()
			for _, file := range slices.Sorted(maps.Keys(addable)) {
				var tree any
				if err := json.Unmarshal(base[file].Data, &tree); err != nil {
					t.Fatalf("%s: %v", file, err)
				}
				list := tree
				for _, name := range addable[file].records {
					list = list.(map[string]any)[name]
				}
				for i, record := range list.([]any) {
					place := append(slices.Clone(addable[file].records), strconv.Itoa(i))
					for _, m := range addable[file].members {
						if _, set := record.(map[string]any)[m[0]]; set {
							continue
						}
						for _, v := range values[m[1]] {
							// The read back takes the member and its value from the input's words, and
							// changedFile builds the file that it means before the writer runs and hands the
							// writer copies of its own.
							fsys := changedFile(t, base, file, treeChange{place: append(slices.Clone(place), m[0]), value: v}, valueAt)
							in := sweepInput{what: fmt.Sprintf("%s %s with %s %s", file, treePointer(place), m[0], encodeTree(t, v)), fsys: fsys}
							if commit, named := v.(string); file == "results.json" && m[0] == "planted_on" && named {
								if _, held, err := s.History.Changed(commit, commit); err == nil && held {
									in.right, in.allowed = plantedOnElsewhere, withoutResult(t, base, i)
								}
							}
							inputs = append(inputs, in)
						}
					}
				}
			}
			n := sweepAgainst(t, base, inputs)
			atFloor(t, "members", name, n)
			t.Logf("%d members added: %d refused, %d read", n.inputs, n.refused, n.read)
		})
	}
}

// namesOf is each name that a record of the scenario fsys names, by its kind (commit, deploy,
// graph_tx, job, author, path), sorted, each once: the commits of the history, of the subjects, of
// the results, among them the commits that faults were planted on, of the job runs and of the maps
// of ties.json and uncited.json; the deploys and the transactions of the subjects and of the
// results; the jobs of the results and of the job runs; the authors of the results; and the paths
// of the trees of the history and of the faults of the results.
func namesOf(t *testing.T, fsys fstest.MapFS) map[string][]string {
	t.Helper()
	names := map[string]map[string]bool{}
	add := func(kind string, v any) {
		if s, ok := v.(string); ok && s != "" {
			if names[kind] == nil {
				names[kind] = map[string]bool{}
			}
			names[kind][s] = true
		}
	}
	read := func(file string) any {
		var tree any
		if err := json.Unmarshal(fsys[file].Data, &tree); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		return tree
	}
	for _, c := range read("history.json").(map[string]any)["commits"].([]any) {
		add("commit", c.(map[string]any)["id"])
		for path := range c.(map[string]any)["files"].(map[string]any) {
			add("path", path)
		}
	}
	for _, file := range []string{"subjects.json", "results.json", "jobs.json"} {
		for _, r := range read(file).([]any) {
			r := r.(map[string]any)
			add("commit", r["commit"])
			add("commit", r["test_commit"])
			add("commit", r["planted_on"])
			add("deploy", r["deploy"])
			add("graph_tx", r["graph_tx"])
			add("job", r["job"])
			add("author", r["fault_author"])
			for member, kind := range map[string]string{"test_authors": "author", "fault_paths": "path"} {
				if list, ok := r[member].([]any); ok {
					for _, v := range list {
						add(kind, v)
					}
				}
			}
		}
	}
	for _, file := range []string{"ties.json", "uncited.json"} {
		for commit := range read(file).(map[string]any) {
			add("commit", commit)
		}
	}
	sorted := map[string][]string{}
	for kind, set := range names {
		sorted[kind] = slices.Sorted(maps.Keys(set))
	}
	return sorted
}

// sweepInput is one input of the sweep: what it changed, for the line of a failure alone, the files
// of the scenario with it, the reason of its right to move a state toward true, "" for none, and the
// states that the right allows, by place, nil for a right that covers each place.
type sweepInput struct {
	what    string
	fsys    fstest.MapFS
	right   string
	allowed map[string]string
}

// pathOfOneSpace is the right of a repository path of a tree of the history written " ": a real
// path, since git holds a file of that name, so the tree changed at a path that a tie covers.
const pathOfOneSpace = `the path " " of the tree is a real path, which git holds, so the tree changed at a path that a tie covers, and the carry ends (R8)`

// sweepInputs is each input of the sweep of the scenario base, and base with each JSON file written
// again by the sweep's own decoder and encoder, unchanged, which the sweep reads as base. Each input
// is read back against the change that its words name, which changedFile builds into the file that
// it means before the writer runs; the writers are functions of the package (nullAt, leftOutAt,
// renamedAt, valueAt, takenOut), each handed copies of its own.
func sweepInputs(t *testing.T, base fstest.MapFS) ([]sweepInput, fstest.MapFS) {
	t.Helper()
	var inputs []sweepInput
	entryMaps, pathPlaces := sweepMaps(), sweepPaths()
	roundTrip := maps.Clone(base)
	with := func(what, name string, want treeChange, write treeWriter) *sweepInput {
		inputs = append(inputs, sweepInput{what: what, fsys: changedFile(t, base, name, want, write)})
		return &inputs[len(inputs)-1]
	}
	for _, name := range slices.Sorted(maps.Keys(base)) {
		out := filesWritten(t, base, map[string][]byte{name: nil}, takenOut(name))
		inputs = append(inputs, sweepInput{what: name + " taken out", fsys: out})
		if path.Ext(name) != ".json" {
			continue
		}
		var tree any
		if err := json.Unmarshal(base[name].Data, &tree); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		roundTrip[name] = &fstest.MapFile{Data: encodeTree(t, tree)}
		with(name+" as null", name, treeChange{}, nullAt)
		var walk func(v any, at []string)
		walk = func(v any, at []string) {
			switch v := v.(type) {
			case map[string]any:
				entries := slices.ContainsFunc(entryMaps[name], func(p []string) bool { return placeMatches(p, at) })
				paths := slices.ContainsFunc(pathPlaces[name].keysOf, func(p []string) bool { return placeMatches(p, at) })
				for _, key := range slices.Sorted(maps.Keys(v)) {
					place := append(slices.Clone(at), key)
					with(name+" "+treePointer(place)+" as null", name, treeChange{place: place}, nullAt)
					if !entries {
						with(name+" "+treePointer(place)+" left out", name, treeChange{place: place, out: true}, leftOutAt)
					}
					var renames []string
					if entries {
						renames = namesOfNothing()
					}
					if paths {
						renames = append(slices.Clone(renames), notRepoPaths(key)...)
					}
					for _, renamed := range renames {
						if _, taken := v[renamed]; !taken {
							in := with(fmt.Sprintf("%s %s as the key %q", name, treePointer(place), renamed), name, treeChange{place: place, value: renamed, renamed: true}, renamedAt)
							if paths && renamed == " " {
								in.right = pathOfOneSpace
							}
						}
					}
					walk(v[key], place)
				}
			case []any:
				for i, x := range v {
					place := append(slices.Clone(at), strconv.Itoa(i))
					with(name+" "+treePointer(place)+" as null", name, treeChange{place: place}, nullAt)
					walk(x, place)
				}
			case string:
				forms := namesOfNothing()
				if slices.ContainsFunc(pathPlaces[name].values, func(p []string) bool { return placeMatches(p, at) }) {
					forms = append(forms, notRepoPaths(v)...)
				}
				for _, form := range forms {
					if form != v {
						with(fmt.Sprintf("%s %s as %q", name, treePointer(at), form), name, treeChange{place: at, value: form}, valueAt)
					}
				}
			}
		}
		walk(tree, nil)
	}
	return inputs, roundTrip
}

// recordKinds is each kind of record of a scenario, a fact that CI or the repository recorded about
// a check, by its file and its place: a result, a run of a job, a refusal of CI, a tie, a commit of
// the history and a file of a commit's tree. The approved spec, the subjects and the units are what
// the derivation judges and where, not records.
func recordKinds() map[string][][]string {
	return map[string][][]string{
		"results.json": {{"*"}},
		"jobs.json":    {{"*"}},
		"uncited.json": {{"*"}, {"*", "*"}},
		"ties.json":    {{"*"}, {"*", "*"}},
		"history.json": {{"commits", "*"}, {"commits", "*", "files", "*"}},
	}
}

// A record taken out whole is another input, not one that says nothing: the history in which the
// fact did not happen. A record that states a fact against a check, taken out, can move a state
// toward true by right, and each kind of record has a row here, with the state of the check with
// the record and with it taken out, and its reason; a kind with no row fails the test. A file taken
// out of a commit's tree is not there at that commit, so it changed, and the carry of each result
// across it ends, a failure's as a pass's (R8: "A result at one commit also counts at another
// commit when no code that the check runs changed between the two"), as a changed label ends it:
// it moves a state away from true where a pass carried, and toward true where a failure carried (a
// row of each), as a path of a tree written " " does in
// TestNoInputThatSaysNothingMovesAStateTowardTrue. Each row derives the check at the subject of its
// scenario with the record, and with the record taken out; setup makes the scenario first.
func TestTakingOutAWholeRecordMovesAStateOnlyByItsRight(t *testing.T) {
	t.Parallel()
	const c = "a-restart-loses-no-message"
	const q = "production-delivery-takes-under-a-second"
	type row struct {
		kind, reason  string
		scenario      string
		setup         func(t *testing.T, fsys fstest.MapFS) fstest.MapFS
		file          string
		place         []string
		subject       int    // from 1
		check         string // whose state the row reads
		with, without string // the state of the check with the record and with it taken out
	}
	// carriedFailure is carry with a failure of c at c1 after its pass, which carries to c2, and a
	// pass at c2 after it and a caught failing example planted on c2: c2 reads unknown (disagree).
	carriedFailure := func(t *testing.T, fsys fstest.MapFS) fstest.MapFS {
		return editTree(t, fsys, "results.json", failureCarriedToC2())
	}
	rows := []row{
		{kind: "a file of a commit's tree", reason: "a tied file that the later commit does not hold changed, so c1's pass carries no more to c2 (R8)",
			scenario: "carry", file: "history.json", place: []string{"commits", "2", "files", "packages/envoy/cmd/listener/main.go"},
			subject: 1, check: c, with: "true", without: "unknown"},
		{kind: "a file of a commit's tree", reason: "a tied file that the later commit does not hold changed, so c1's failure carries no more to c2, and c2's own pass and caught failing example count (R8)",
			scenario: "carry", setup: carriedFailure, file: "history.json", place: []string{"commits", "2", "files", "packages/envoy/cmd/listener/main.go"},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "a failing result", reason: "the failure is the record against the check, and the history without it holds no failure (R7: \"a later failure cancels an earlier pass\")",
			scenario: "silent-leg", file: "results.json", place: []string{"5"},
			subject: 3, check: q, with: "false", without: "true"},
		{kind: "a run of a job", reason: "with no run of leg arm that ended, no leg is silent (R9: \"after a run of its job ends\")",
			scenario: "silent-leg", file: "jobs.json", place: []string{"1"},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "the refusals of CI at a commit", reason: "CI then refused no check at the commit (R48)",
			scenario: "refusal", file: "uncited.json", place: []string{"c2"},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "a refusal of CI", reason: "CI then refused no check at the commit (R48)",
			scenario: "refusal", file: "uncited.json", place: []string{"c2", c},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "the ties at a commit", reason: "no tie is then known at the commit, and a tie that is not known carries no result (R18, R8), so the failure there counts nowhere else",
			scenario: "carry", setup: carriedFailure, file: "ties.json", place: []string{"c1"},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "a tie", reason: "the tie is then not known at the commit, and a tie that is not known carries no result (R18, R8), so the failure there counts nowhere else",
			scenario: "carry", setup: carriedFailure, file: "ties.json", place: []string{"c1", c},
			subject: 1, check: c, with: "unknown", without: "true"},
		{kind: "a commit of the history", reason: "the history then holds no tree of the commit, so no result carries from it (R8), and the failure there counts nowhere else",
			scenario: "carry", setup: carriedFailure, file: "history.json", place: []string{"commits", "1"},
			subject: 1, check: c, with: "unknown", without: "true"},
	}
	kinds := recordKinds()
	for kind := range maps.Keys(kinds) {
		for _, pattern := range kinds[kind] {
			if !slices.ContainsFunc(rows, func(r row) bool { return r.file == kind && placeMatches(pattern, r.place) }) {
				t.Errorf("no row takes out a record of %s at %s", kind, treePointer(pattern))
			}
		}
	}
	for _, r := range rows {
		base := scenarioFS(t, filepath.Join("testdata", "scenarios", r.scenario))
		if r.setup != nil {
			base = r.setup(t, base)
		}
		without := changedFile(t, base, r.file, treeChange{place: r.place, out: true}, leftOutAt)
		for _, read := range []struct {
			fsys fstest.MapFS
			want string
			form string
		}{{base, r.with, "with"}, {without, r.without, "without"}} {
			states, err := scenarioStates(read.fsys)
			if err != nil {
				t.Errorf("%s, %s the record: %v", r.kind, read.form, err)
				continue
			}
			at := ""
			for key := range states {
				if strings.HasPrefix(key, fmt.Sprintf("subject %d ", r.subject)) && strings.HasSuffix(key, " check "+r.check) {
					at = key
				}
			}
			if got := states[at]; got != read.want {
				t.Errorf("%s, %s the record %s %s: the check reads %q, want %q (%s)", r.kind, read.form, r.file, treePointer(r.place), got, read.want, r.reason)
			}
		}
	}
}

// failureCarriedToC2 is the writer of carry's results.json that adds a failure of its first
// record's check at c1 after its pass, a pass at c2 after it, and a copy of its caught failing
// example planted on c2 (TestTakingOutAWholeRecordMovesAStateOnlyByItsRight).
func failureCarriedToC2() treeEdit {
	return func(v any) any {
		results := v.([]any)
		run := func(commit, outcome, at string, seq int) map[string]any {
			r := maps.Clone(results[0].(map[string]any))
			r["commit"], r["outcome"], r["at"], r["seq"] = commit, outcome, at, seq
			return r
		}
		fault := maps.Clone(results[1].(map[string]any))
		fault["commit"], fault["planted_on"], fault["at"], fault["seq"] = "f-c2", "c2", "2026-11-13T09:05:00Z", 7
		return append(results, run("c1", "fail", "2026-11-12T10:10:00Z", 5), run("c2", "pass", "2026-11-13T09:00:00Z", 6), fault)
	}
}

// refusalAdded is a copy of base, of its own (editTree), with CI's refusal of the check id at
// commit added to uncited.json by its writer (refusalAt), read back by the reader against the
// refusals that the sweep means, which it builds before the writer runs from a decode of base of
// their own: those of base, and that one with the rule test-did-not-run (refusalRead).
func refusalAdded(t *testing.T, base fstest.MapFS, commit, id string) fstest.MapFS {
	t.Helper()
	want := map[string]map[string]string{commit: {}}
	for at, checks := range decodedScenario(t, base).Uncited {
		want[at] = maps.Clone(checks)
	}
	want[commit][id] = "test-did-not-run"
	fsys := editTree(t, base, "uncited.json", refusalAt(commit, id))
	refusalRead(t, fsys, commit, id, want)
	return fsys
}

// refusalAt is the writer of the refusal sweep: the tree of uncited.json that it is handed with CI's
// refusal of the check id at commit, by the rule test-did-not-run.
func refusalAt(commit, id string) treeEdit {
	return func(v any) any {
		uncited := v.(map[string]any)
		checks := map[string]any{}
		if at, held := uncited[commit].(map[string]any); held {
			checks = maps.Clone(at)
		}
		checks[id] = "test-did-not-run"
		uncited[commit] = checks
		return uncited
	}
}

// refusalRead fails the test unless the scenario fsys, to which the refusal sweep added CI's
// refusal of the check id at commit, reads, through the reader, as want, the refusals that the
// sweep means, and no other refusal: the read back of the refusal by its commit, its check and its
// rule, so that a writer that writes the refusal of another check, at another commit, or an entry
// of the commit that holds no refusal, fails by name.
func refusalRead(t *testing.T, fsys fstest.MapFS, commit, id string, want map[string]map[string]string) {
	t.Helper()
	if read, err := ReadScenario(fsys); err != nil || !reflect.DeepEqual(read.Uncited, want) {
		t.Fatalf("the sweep wrote CI's refusal of %s at %s, and the reader reads the refusals %v (%v), where it should read %v", id, commit, read.Uncited, err, want)
	}
}
