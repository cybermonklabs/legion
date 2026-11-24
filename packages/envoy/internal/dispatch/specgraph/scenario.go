package specgraph

import (
	"cmp"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// Scenario is a scenario of The Spec, in the format of testdata/scenarios that `spec-check state`
// reads: the approved spec, the spec files at the commit of each subject, the history of the
// repository, the tie of each check at each commit, every result, the runs of CI jobs, the
// subjects, the checks that CI refused at each commit (R48) by commit, and the work units. Units is
// nil exactly when the scenario has no units.json.
type Scenario struct {
	Approved Approved
	Files    map[string]Tree // by commit
	History  FixtureHistory
	Ties     CommitTies
	Results  []Result
	JobRuns  []JobRun
	Subjects []ScenarioSubject
	Uncited  map[string]map[string]string
	Units    []Unit
}

// ScenarioSubject is one subject of a scenario, in the form of subjects.json: the subject and the
// time that the states at it are derived at.
type ScenarioSubject struct {
	Point
	Now time.Time `json:"now" input:"nonzero"`
}

// ScenarioError is a fault of one file of a scenario: the file's path inside the scenario, the
// line of a spec file that does not parse (0 for none), and the fault.
type ScenarioError struct {
	File string
	Line int
	Err  error
}

func (e *ScenarioError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Err)
	}
	return e.File + ": " + e.Err.Error()
}

func (e *ScenarioError) Unwrap() error { return e.Err }

// The files of a scenario, each with its mark: each that it must hold, in the order that
// ReadScenario names a missing one, and each that it may leave out, with what its absence reads as.
// A file that says nothing would read as a real value, as a member left out does, so a scenario
// holds each file that a rule reads: uncited.json, {} when CI refused no check, as jobs.json is []
// when no job ran (TestNoInputThatSaysNothingMovesAStateTowardTrue takes out each file).
var (
	scenarioRequired = []string{"approved.json", "files", "history.json", "jobs.json", "results.json", "subjects.json", "ties.json", "uncited.json"}
	scenarioOptional = map[string]string{
		"units.json": "no unit, so no wait and no cycle of waits is read",
		"want.txt":   "ReadScenario does not read it",
	}
)

// scenarioResult is one result of results.json: a Result, whose members its own JSON names give,
// with check_version in the place of its CheckDigest, which names the approved version of the check
// that ran, and Leg nil where the file leaves it out, which readResults refuses, since "" is the
// leg of no matrix.
type scenarioResult struct {
	Result
	Leg          *string `json:"leg" input:"present: \"\" is the leg of no matrix, which the results of a job from before its matrix give"`
	CheckVersion int     `json:"check_version" input:"nonzero"`
}

// The forms of the JSON files of a scenario that ReadScenario reads itself, with the type of each
// value, as decodeJSON names them for a value of another type: the form of one element of each
// array file, whose own form is "[<element>, …]", and the form of uncited.json. A time is timeForm,
// the one text of a time of the package's forms, as approvedForm writes it.
const (
	subjectForm = `{"commit":"<commit>","branch":"<branch>","deploy":"<deploy>","graph_tx":"<transaction>","now":"` + timeForm + `"}`
	resultForm  = `{"check":"<check id>","check_version":<whole number from 1>,"kind":"<run or failing_example>","outcome":"<pass, fail or could_not_run>","commit":"<commit>","deploy":"<deploy>","graph_tx":"<transaction>","job":"<job>","leg":"<leg>","attempt":<whole number>,"at":"` + timeForm + `","seq":<whole number from 1>,"fault_author":"<author>","test_authors":["<author>", …],"test_commit":"<commit>","planted_on":"<commit>","fault_paths":["<repository path>", …]}`
	jobRunForm  = `{"job":"<job>","leg":"<leg>","commit":"<commit>","ended":<true or false>}`
	unitForm    = `{"id":"<unit>","serves":["<statement id>", …],"needs":["<unit>", …],"waits_on":["<statement id>", …]}`
	uncitedForm = `{"<commit>":{"<check id>":"<R48 rule>"}}`
)

// ReadScenario reads the scenario at the root of fsys, and refuses any file or field that the
// format does not hold, with a *ScenarioError that names the file. Each JSON file is read by
// decodeJSON, which refuses a byte that is not UTF-8, a member that the format does not name in
// its own letters, a member that one object gives two times, text after the value, and a value of
// another type than the file's form needs, which it names with the form and the place of the value.
// It refuses each member that the derivation reads and that a file does not set, by the type that it
// reads the file into (memberMark), with the functions that the entry of the derivation refuses the
// same member with (Input.check): one marked nonzero at its zero value, and one marked present that
// the file leaves out, since the zero value of such a member would read as a real one and turn a
// state toward true or hide a failure; a member marked zero that a file leaves out reads as its
// zero value. Each commit, check, job, leg, deploy, transaction, branch and unit that a file names,
// as a value or as a key, has a letter or a digit (nameFault), and each path of the repository is a
// repository path (isRepoPath):
//
//   - approved.json, as ReadApproved reads it, a cycle of parent and support relations among the
//     current statements among what it refuses (R5: "Dispatch refuses a change that makes one").
//     The rules of the files are not run on it: the person approves a level before the checks of
//     its statements exist, so an approved leaf can have no check, and the files then do not hold
//     it;
//   - subjects.json, [{"commit","branch","deploy","graph_tx","now"}]: at least one subject, each
//     with a commit, a now, and exactly one of branch and deploy; graph_tx, the transaction of the
//     graph that the subject reads, only with a deploy; and each of commit, branch, deploy and
//     graph_tx that it sets has a letter or a digit (subjectFault). A check of production or of
//     the graph is judged only at a deploy, and a check of the graph reads unknown there when
//     graph_tx is absent or names a transaction that no result read;
//   - files/<commit>/: the spec files at the commit of each subject, each a *.md file directly in
//     it, which Parse reads and Validate judges with no problem, as `spec-check tree` does, and no
//     directory that no subject reads;
//   - history.json, as ReadFixtureHistory reads it: it sets commits, [] for none, and each commit
//     sets its files, a tree of one path or more, each a repository path with a content label that
//     has a letter or a digit;
//   - ties.json, {"<commit>":{"<check id>":["<path or pattern>", …]}}, as ReadCommitTies reads it:
//     a check that the object of a commit does not name has a tie that is not known at that commit,
//     and so has each check at a commit that the file does not name. A result at one commit counts
//     at another only when the check's tie is known at both commits and no path that changed
//     between them is covered by the tie at either commit;
//   - results.json, [{"check","check_version","kind","outcome","commit","deploy","graph_tx","job",
//     "leg","attempt","at","seq","fault_author","test_authors","test_commit","planted_on",
//     "fault_paths"}]: check_version names an approved version of the check, whose Digest the
//     result carries, and the result is whole by that version's text (Result.fault): kind is run or
//     failing_example, outcome pass, fail or could_not_run, it names at most one of the subjects
//     commit, deploy and graph_tx, the one that it read, and it sets leg ("" for no matrix), at and
//     seq, from 1; a run, and a failing example of a check of the code, names the subject of its
//     check's kind, a run of a check of the graph that could not run too, which names the
//     transaction that it tried to read; a run of a program check that names a job names the job,
//     and a result of a program check of production or of the graph names test_commit; each of
//     fault_paths is a repository path. A failing example of a program check of the code counts
//     from planted_on, the commit that its fault was planted on, and never at its commit, the
//     fault's own commit (deriveChecks, step 7). No two results of one check have one at and one
//     seq;
//   - jobs.json, [{"job","leg","commit","ended"}]: each run sets job and commit, leg ("" for no
//     matrix), and ended, false for a run that has not ended;
//   - uncited.json, {"<commit>":{"<check id>":"<R48 rule>"}}: the checks that CI refused at each
//     commit, {} for none, each with its rule of one character or more (uncitedFault); a commit that
//     it does not name, or whose map is {}, is one at which CI refused no check;
//   - units.json (optional), [{"id","serves","needs","waits_on"}]: each id once, each of serves,
//     needs and waits_on set ([] for none), serves and waits_on naming current statements of the
//     approved spec, needs naming units of the file; with none, no wait is read;
//   - want.txt (optional), which ReadScenario does not read.
//
// A JSON null is refused at each place of each file (decodeJSON), and so is each file that holds
// more than fileBound bytes, 16 MiB, which is read no further.
func ReadScenario(fsys fs.FS) (Scenario, error) {
	var s Scenario
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return s, &ScenarioError{File: ".", Err: err}
	}
	holds := map[string]bool{}
	for _, entry := range entries {
		if _, optional := scenarioOptional[entry.Name()]; !optional && !slices.Contains(scenarioRequired, entry.Name()) {
			return s, scenarioFault(entry.Name(), "the format of a scenario holds no such file")
		}
		holds[entry.Name()] = true
	}
	for _, name := range scenarioRequired {
		if !holds[name] {
			return s, scenarioFault(name, "does not exist")
		}
	}

	if s.Approved, err = readWith(fsys, "approved.json", ReadApproved); err != nil {
		return s, err
	}
	if s.Subjects, err = readSubjects(fsys); err != nil {
		return s, err
	}
	if s.Files, err = readScenarioFiles(fsys, s.Subjects); err != nil {
		return s, err
	}
	if s.History, err = readWith(fsys, "history.json", ReadFixtureHistory); err != nil {
		return s, err
	}
	if s.Ties, err = readWith(fsys, "ties.json", ReadCommitTies); err != nil {
		return s, err
	}
	var procedures []*Check
	if s.Results, procedures, err = readResults(fsys, s.Approved); err != nil {
		return s, err
	}
	if s.JobRuns, err = readJobRuns(fsys); err != nil {
		return s, err
	}
	if fault, ofRun := legFault(s.JobRuns, s.Results, procedures); fault != "" {
		file := "results.json"
		if ofRun {
			file = "jobs.json"
		}
		return s, scenarioFault(file, "%s", fault)
	}
	if err := readJSON(fsys, "uncited.json", &s.Uncited, uncitedForm); err != nil {
		return s, err
	}
	if fault := uncitedFault(s.Uncited); fault != "" {
		return s, scenarioFault("uncited.json", "%s", fault)
	}
	if holds["units.json"] {
		if s.Units, err = readUnits(fsys, s.Approved); err != nil {
			return s, err
		}
	}
	return s, nil
}

// Input is the input of the derivation at subject i of the scenario.
func (s Scenario) Input(i int) Input {
	subject := s.Subjects[i]
	return Input{Files: s.Files[subject.Commit], Approved: s.Approved, Results: s.Results, JobRuns: s.JobRuns,
		Ties: s.Ties, History: s.History, Subject: subject.Point, Now: subject.Now, Uncited: s.Uncited}
}

// readSubjects reads subjects.json.
func readSubjects(fsys fs.FS) ([]ScenarioSubject, error) {
	subjects, err := readEach[ScenarioSubject](fsys, "subjects.json", "subject", subjectForm)
	if err != nil {
		return nil, err
	}
	if len(subjects) == 0 {
		return nil, scenarioFault("subjects.json", "it holds no subject")
	}
	for i, sub := range subjects {
		if fault := subjectFault(sub.Point, sub.Now); fault != "" {
			return nil, scenarioFault("subjects.json", "subject %d: %s", i+1, fault)
		}
	}
	return subjects, nil
}

// readScenarioFiles reads the spec files at the commit of each subject, files/<commit>/*.md,
// parses them and judges the tree by Validate, and refuses the first problem of Parse, or else of
// Validate, with its file and line.
func readScenarioFiles(fsys fs.FS, subjects []ScenarioSubject) (map[string]Tree, error) {
	trees := map[string]Tree{}
	for i, sub := range subjects {
		if _, read := trees[sub.Commit]; read {
			continue
		}
		dir := path.Join("files", sub.Commit)
		if !fs.ValidPath(dir) || strings.Count(dir, "/") != 1 {
			return nil, scenarioFault("subjects.json", "subject %d: commit %s is no name of a directory", i+1, quoted(sub.Commit))
		}
		entries, err := fs.ReadDir(fsys, dir)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, scenarioFault(dir, "does not exist, and subject %d reads the spec files of its commit there", i+1)
		}
		if err != nil {
			// The error of the file system names the directory, whose name is the commit, the writer's
			// own text: one too long for a name gives an error that holds it two times. So the line
			// names the subject, and the commit as quoted prints it, and the cause alone.
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) {
				err = pathErr.Err
			}
			return nil, scenarioFault("subjects.json", "subject %d: the spec files of its commit %s cannot be read: %v", i+1, quoted(sub.Commit), err)
		}
		files := map[string][]byte{}
		for _, entry := range entries {
			name := path.Join(dir, entry.Name())
			if !entry.Type().IsRegular() || path.Ext(entry.Name()) != ".md" {
				return nil, scenarioFault(name, "the spec files of a commit are *.md files directly in its directory")
			}
			if files[entry.Name()], err = readFile(fsys, name); err != nil {
				return nil, &ScenarioError{File: name, Err: err}
			}
		}
		if len(files) == 0 {
			return nil, scenarioFault(dir, "holds no spec file")
		}
		tree, problems := Parse(files)
		if len(problems) == 0 {
			problems = Validate(tree, Options{})
		}
		if len(problems) > 0 {
			SortProblems(problems)
			p := problems[0]
			return nil, &ScenarioError{File: path.Join(dir, p.Pos.File), Line: p.Pos.Line, Err: fmt.Errorf("%s: %s", p.Node, p.Detail)}
		}
		trees[sub.Commit] = tree
	}
	entries, err := fs.ReadDir(fsys, "files")
	if err != nil {
		return nil, &ScenarioError{File: "files", Err: err}
	}
	for _, entry := range entries {
		if _, read := trees[entry.Name()]; !read {
			return nil, scenarioFault(path.Join("files", entry.Name()), "no subject reads the spec files of this commit")
		}
	}
	return trees, nil
}

// readWith reads the file name with read, and names the file in its fault.
func readWith[T any](fsys fs.FS, name string, read func(io.Reader) (T, error)) (T, error) {
	f, err := fsys.Open(name)
	if err != nil {
		var zero T
		return zero, &ScenarioError{File: name, Err: err}
	}
	defer f.Close()
	v, err := read(f)
	if err != nil {
		return v, &ScenarioError{File: name, Err: err}
	}
	return v, nil
}

// readResults reads results.json, each result carrying the Digest of the approved version of its
// check that check_version names, and the text of that version of each, and refuses a result that
// leaves out its leg, one that Result.fault refuses by that text, and two results that
// sameRecordFault names.
func readResults(fsys fs.FS, approved Approved) ([]Result, []*Check, error) {
	read, err := readEach[scenarioResult](fsys, "results.json", "result", resultForm)
	if err != nil {
		return nil, nil, err
	}
	results := make([]Result, len(read))
	texts := make([]Check, len(read))
	procedures := make([]*Check, len(read))
	for i, r := range read {
		check, held := checkVersion(approved, r.Check, r.CheckVersion)
		if !held {
			return nil, nil, scenarioFault("results.json", "result %d: the approved spec holds no version %d of check %s", i+1, r.CheckVersion, quoted(r.Check))
		}
		if r.Leg == nil {
			return nil, nil, scenarioFault("results.json", `result %d: it sets no leg; a result of no matrix sets ""`, i+1)
		}
		results[i] = r.Result
		results[i].Leg = *r.Leg
		results[i].CheckDigest = check.Digest()
		texts[i], procedures[i] = check, &texts[i]
		if fault := results[i].fault(procedures[i]); fault != "" {
			return nil, nil, scenarioFault("results.json", "result %d: %s", i+1, fault)
		}
	}
	if fault := sameRecordFault(results); fault != "" {
		return nil, nil, scenarioFault("results.json", "%s", fault)
	}
	return results, procedures, nil
}

// checkVersion is the text of version v of the check id in the approved spec, and whether the
// approved spec holds that version of a check.
func checkVersion(approved Approved, id string, v int) (Check, bool) {
	i := slices.IndexFunc(approved.Nodes[id].Versions, func(av ApprovedVersion) bool { return av.Version == v })
	if i < 0 {
		return Check{}, false
	}
	text, problems := versionText(id, approved.Nodes[id].Versions[i])
	if len(problems) > 0 || len(text.Checks) != 1 {
		return Check{}, false
	}
	return text.Checks[0], true
}

// readJobRuns reads jobs.json, and refuses a run that leaves out its leg or ended
// (jobRunFile.unsetFault), and one that JobRun.fault refuses.
func readJobRuns(fsys fs.FS) ([]JobRun, error) {
	read, err := readEach[jobRunFile](fsys, "jobs.json", "job run", jobRunForm)
	if err != nil {
		return nil, err
	}
	runs := make([]JobRun, len(read))
	for i, r := range read {
		runs[i] = r.run()
		if fault := cmp.Or(r.unsetFault(), runs[i].fault()); fault != "" {
			return nil, scenarioFault("jobs.json", "job run %d: %s", i+1, fault)
		}
	}
	return runs, nil
}

// readUnits reads units.json, and refuses units that unitsFault refuses, among them a unit that
// does not set serves, needs or waits_on, since one that does not would read as one with none,
// which hides a cycle of waits (R29), [] being none; and one id on two units.
func readUnits(fsys fs.FS, approved Approved) ([]Unit, error) {
	units, err := readEach[Unit](fsys, "units.json", "unit", unitForm)
	if err != nil {
		return nil, err
	}
	if fault := unitsFault(units); fault != "" {
		return nil, scenarioFault("units.json", "%s", fault)
	}
	current, _ := approved.Current()
	statements := make(map[string]bool, len(current.Statements))
	for _, st := range current.Statements {
		statements[st.ID] = true
	}
	ids := make(map[string]bool, len(units))
	for _, u := range units {
		ids[u.ID] = true
	}
	for _, u := range units {
		for _, field := range []struct {
			name string
			ids  []string
		}{{"serves", u.Serves}, {"waits_on", u.WaitsOn}} {
			for _, id := range field.ids {
				if !statements[id] {
					return nil, scenarioFault("units.json", "unit %s: %s names %s, which is no current statement of the approved spec", quoted(u.ID), field.name, quoted(id))
				}
			}
		}
		for _, id := range u.Needs {
			if !ids[id] {
				return nil, scenarioFault("units.json", "unit %s: needs names %s, which is no unit of the file", quoted(u.ID), quoted(id))
			}
		}
	}
	return units, nil
}

// readEach reads the JSON array of the file name, each element a T in the form form as decodeJSON
// reads it, so the file is in the form "[<form>, …]"; a fault of an element names it as
// "<noun> <n>", from 1, and a place in it from the element.
func readEach[T any](fsys fs.FS, name, noun, form string) ([]T, error) {
	var raw []jsontext.Value
	if err := readJSON(fsys, name, &raw, "["+form+", …]"); err != nil {
		return nil, err
	}
	all := make([]T, len(raw))
	for i, element := range raw {
		if err := decodeJSON(element, &all[i], form); err != nil {
			return nil, scenarioFault(name, "%s %d: %w", noun, i+1, err)
		}
	}
	return all, nil
}

// readJSON decodes the file name into v with decodeJSON, in the form form.
func readJSON(fsys fs.FS, name string, v any, form string) error {
	content, err := readFile(fsys, name)
	if err != nil {
		return &ScenarioError{File: name, Err: err}
	}
	if err := decodeJSON(content, v, form); err != nil {
		return &ScenarioError{File: name, Err: err}
	}
	return nil
}

// readFile is the file name of fsys, at most fileBound bytes of it (readBounded).
func readFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readBounded(f)
}

// scenarioFault is the fault of the file name, the format filled with args.
func scenarioFault(name, format string, args ...any) *ScenarioError {
	return &ScenarioError{File: name, Err: fmt.Errorf(format, args...)}
}
