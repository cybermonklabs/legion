package specgraph

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"
)

// Point is the subject that states are derived at (R10): the head of a branch, with Branch set and
// Deploy and GraphTx "", or the commit that production runs, with Deploy set and Branch "", and
// GraphTx the transaction of the graph that it reads, "" for none (R13: a query "reads the graph at
// one transaction"). A check of production or of the graph has a state only at a Point that names a
// Deploy, whatever its GraphTx. It is not named Subject, which is what a check reads.
type Point struct {
	Commit  string `json:"commit" input:"nonzero"`
	Branch  string `json:"branch" input:"one of deploy: a subject is the head of a branch or the commit that production runs"`
	Deploy  string `json:"deploy" input:"one of branch: a subject is the head of a branch or the commit that production runs"`
	GraphTx string `json:"graph_tx" input:"zero: the subject reads no transaction, so a check of the graph reads unknown"`
}

// subjectFault is the first fault of a subject, the point p and the time now that the states at it
// are derived at, as a refusal names it, and "" when it has none: it sets no commit or no now, or it
// does not set exactly one of a branch and a deploy, or it sets graph_tx with no deploy, or a
// commit, branch, deploy or graph_tx that it sets names nothing (nameFault). With no now no time
// limit of a check would end; with neither a branch nor a deploy no check of production would be
// judged, and with both or with a transaction of the graph at the head of a branch the subject
// would be none of the two that R10 names ("The subject is the head of the branch that the files
// are read from, or the commit that production runs"). The reader of subjects.json and the entry of
// the derivation both refuse a subject by it.
func subjectFault(p Point, now time.Time) string {
	switch {
	case p.Commit == "":
		return "it sets no commit"
	case now.IsZero():
		return "it sets no now"
	case p.Branch == "" && p.Deploy == "":
		return "it sets neither branch nor deploy; a subject sets exactly one of them"
	case p.Branch != "" && p.Deploy != "":
		return "it sets both branch and deploy; a subject sets exactly one of them"
	case p.GraphTx != "" && p.Deploy == "":
		return "it sets graph_tx with no deploy; the commit that production runs reads the graph, and the head of a branch does not"
	}
	return nameFault(namedMember{"commit", "commit", p.Commit}, namedMember{"branch", "branch", p.Branch},
		namedMember{"deploy", "deploy", p.Deploy}, namedMember{"graph_tx", "transaction of the graph", p.GraphTx})
}

// Input is what the states at one subject are derived from: the spec files at Subject.Commit, the
// approved spec, every stored result, the runs of CI jobs, the tie of each check at each commit, the
// history of the repository, the time of the derivation, and the checks that CI refused at each
// commit (R48), by commit and then by check id, each with the rule that refused it. Its tag input
// marks how each member reads when the caller does not set it (members.go): a run of a job never
// moves a state toward true, and a refusal by CI makes its check read unknown at its commit and
// cancels the results of that commit that support true wherever they count (R48, deriveChecks step
// 2), so each can move a state away from what the evidence without it gives, and never toward
// true, and the caller gives JobRuns and Uncited, empty for none, and Input.check refuses nil. A run
// counts only at its own commit, and a failing example also at another commit, by the ties and the
// history (the rule that replaces The Spec's R8: "A result counts only at the commit that it read";
// "A failing example that a check's test caught (The Spec R9) counts at another commit only while
// no file that the test reads changed"). With no tie, or no history, no failing example counts at
// another commit, so a caught fault at another commit then says nothing about this one. So a caller
// that gives no Ties or no History is refused too, and one that states that it has none gives
// CommitTies{} or FixtureHistory{Commits: []FixtureCommit{}}, the empty values that are not nil.
// The command gives the ties and the history of its scenario.
type Input struct {
	Files    Tree                         `input:"zero: no spec file, so each statement reads not built"`
	Approved Approved                     `input:"zero: no approved node, so no state is derived"`
	Results  []Result                     `input:"zero: no result, so each check reads unknown"`
	JobRuns  []JobRun                     `input:"nonzero"`
	Ties     CommitTies                   `input:"nonzero"`
	History  History                      `input:"nonzero"`
	Subject  Point                        `input:"nonzero"`
	Now      time.Time                    `input:"nonzero"`
	Uncited  map[string]map[string]string `input:"nonzero"`
}

// CheckState is the state of one check at the subject (R7): State "true", "false" or "unknown",
// with the Reason code of the step that decided it. From is the commit (production: the deploy;
// the graph: the transaction) of the run that counts, for "true" and "false". Disagree marks
// results that disagree (Reason "disagree"). Detail is what the reason names: the R48 rule for
// "uncited", the statement whose text changed for "statement_changed", and for "ci_fault" the leg
// that left no result, "" when the job left none at all.
type CheckState struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
	From     string `json:"from"`
	Disagree bool   `json:"disagree"`
}

// The states of a check (R7); the command names CheckTrue and CheckFalse.
const (
	CheckTrue    = "true"
	CheckFalse   = "false"
	checkUnknown = "unknown"
)

// The reason codes of a check state, each the step of deriveChecks that gives it; the command names
// ReasonNotBuilt and ReasonPlantedOnOutside.
const (
	ReasonNotBuilt             = "not_built"
	reasonRefused              = "refused"
	reasonUncited              = "uncited"
	reasonStatementChanged     = "statement_changed"
	reasonCIFault              = "ci_fault"
	reasonSubjectMoved         = "subject_moved"
	reasonNoResult             = "no_result"
	reasonDisagree             = "disagree"
	reasonCouldNotRun          = "could_not_run"
	reasonExpired              = "expired"
	reasonNoFailingExample     = "no_failing_example"
	reasonFailingExampleMissed = "failing_example_missed"
	reasonFaultByTestAuthor    = "fault_by_test_author"
	reasonNoPlantedOn          = "no_planted_on"
	reasonNoFaultPaths         = "no_fault_paths"
	reasonBlankFaultPath       = "blank_fault_path"
	reasonPlantedOnTested      = "planted_on_tested_commit"
	ReasonPlantedOnOutside     = "planted_on_outside_history"
	reasonPassed               = "passed"
	reasonFailed               = "failed"
)

// deriveChecks is the judge of in and the state at in.Subject of each check that the approved spec
// holds a current text of, sorted by id: the one entry that Derive and the tests share. A check of
// production or of the graph is judged only at a subject that names a deploy, and there a check of
// the graph counts only the results at the subject's transaction, so
// it reads unknown when the subject names no transaction or one that no result read (R10: "Only
// the checks that read the subject count. A check whose subject is production decides nothing for
// the head of a branch"; "Unknown: every other case"; the graph is production's data, R13).
//
// The state of a check C at subject S comes from the first of these steps that decides; no later
// step overrides an earlier one, and a failure decides only at step 8, after every unknown case of
// R9 ("Six cases read as unknown, and never as true"). Each helper of checkJudge.state names its
// step here and does not restate it:
//
//  1. The files do not hold C's current approved text: unknown, not_built. That text names no
//     failing example, a text with no letter and no digit by the rule that Validate reads it with
//     (hasWord): unknown, no_failing_example, whatever C's results (R9: "No setting makes the
//     failing example optional"; "a check whose current text has no failing example that failed
//     when it should" reads unknown). Validate refuses such a text in the files, and the pilot
//     builds the approved spec from the store, with no reader. A problem that Validate finds in
//     the files and that refuses C (refusals): unknown, refused, Detail its rule, since the
//     derivation gives no clean state for a tree that the rules refuse.
//  2. CI refused C at S.Commit (R48: "At that commit, such a check reads unknown, and no earlier
//     result of it counts"): unknown, uncited, Detail the rule, whatever else holds there: C's
//     results at S.Commit before or after the refusal, a result carried from another commit, or
//     newer evidence of another commit that would count there, so no result of C makes it true
//     or false at S.Commit. Of the own results of a commit where CI refused C, the candidates of
//     step 3 whose commit is that commit (resultCommit; for a failing example of a program check
//     of the code, the commit that it counts from, faultCommit), those that support true, a pass
//     and a failing example that caught its fault, count nowhere, at it or at another commit; its
//     failures, its runs that could not run and its failing examples that missed their fault count
//     wherever they would, since at another subject R7 decides ("a later failure cancels an earlier
//     pass") and R9's unknown cases read "never as true", so a refusal never moves a state toward
//     true (withoutRefused). With such a result taken out that would count at S and no run left
//     that counts at S: unknown, uncited, Detail the least rule by its name of those refusals, so
//     that the order of the results does not choose it. The own results of every other commit
//     stay, whatever CI refused elsewhere: a refusal cancels no result of another commit, so a pass
//     and a failure of one procedure at one commit read unknown, disagree, and whether a failing
//     example counts across the refused commit is the question of the ties and the history (step
//     7). A query or an agent of production or of the graph has no commit of a result, so only CI's
//     refusal at S.Commit counts for it, whatever commit a result's TestCommit names. A run of the
//     job is no result, and no refusal takes one out.
//  3. The candidates (candidates) are C's results of kind run or failing_example that carry the
//     digest of C's current text and were recorded at or after the approval of that text (R7: a
//     result counts "only for the procedure that ran it"; "a change to either ends the standing of
//     earlier results", so a text that changes and changes back has no results from before), a run
//     of a program check only when the check's job reported it (R6: "only the reports of that job
//     count"), and only those recorded at or after the newest approval of C's statement and of
//     each statement above it (R7: "A change to the text of a statement makes every result under
//     it suspect"). Results that pass the other tests and are all older than that approval:
//     unknown, statement_changed.
//  4. The runs that count at S. For a code check, the runs at S.Commit alone: a run at another
//     commit does not count at S, even where no file of C's tie changed between the two (runCounts;
//     the rule that replaces The Spec's R8: "A result counts only at the commit that it read. A CI
//     job's result counts only at the commit that the job tested"). For a production check, the
//     runs under S.Deploy; for a graph check, the runs at S.GraphTx, and none when S names no
//     transaction (R7: "A result counts only for the procedure that ran it and for the subject that
//     it read"); a run of the graph names the transaction that it read or tried to read, and
//     Input.check refuses one that names none.
//     For a program check of production or the graph, only a run whose test is the test at
//     S.Commit: its TestCommit is S.Commit, or C's tie is known at TestCommit and at S.Commit and no
//     path that changed between them is covered by the tie at either commit (carries; R18: "At each
//     commit, Dispatch derives four ties"; R7: "The procedure is the text of the check and the code
//     that runs it, so a change to either ends the standing of earlier results"); Input.check
//     refuses a result of such a check that names no TestCommit, as it refuses one that names no
//     subject of its check's kind. A run of C's job counts at S at each commit where a run of C
//     would count at S, S.Commit among them, whether or not that commit holds a result of C: it is
//     the newest answer of its commit (R9: "When a check has no result that counts after a run of
//     its job ends, it reads unknown, and Dispatch reports a fault of CI"; R7: "silence is not a
//     pass"), unlike a refusal of step 2. No run counts: unknown, ci_fault when a run of C's job
//     that counts at S ended (R9), else subject_moved when C has a run, else no_result, since a run
//     of the job that has not ended is no fault of CI. A code check, or a program check of
//     production or the graph, with a run of its job that counts at S in a leg with no run that
//     counts: unknown, ci_fault, Detail the first such leg by name whose run ended (R7: "silence is
//     not a pass"; R12: "Each leg of a matrix reports its own result, and no leg replaces the
//     result of another"), else unknown, no_result, Detail the first such leg by name, since a leg
//     whose run has not ended is no silent leg and no pass (legWithNoResult).
//  5. For a code check, at S.Commit, and for a graph check, at S.GraphTx (R13: a query "reads the
//     graph at one transaction"): a pass and a failure that count, or a pass recorded after a
//     failure that counts: unknown, disagree (R7: "A pass and a failure on one such subject and one
//     procedure make the state unknown", "a later pass does not end the report"). For each check,
//     newest runs of two legs that disagree: unknown, disagree. A production check skips the first
//     test: in each leg its newest run under S.Deploy decides (R7: "A pass, then a failure, then a
//     pass across deploys is ordinary"). Then a newest run that could not run: unknown,
//     could_not_run.
//  6. C has a time limit and the newest run that counts of one of its legs is older: unknown,
//     expired, since no leg replaces the result of another (R12), so a fresh leg does not hide a
//     leg whose result expired.
//  7. The newest failing example that counts. For a program check of the code, one whose record
//     names the commit that the step planted its fault on (PlantedOn) and one path of the fault or
//     more (FaultPaths), each a source file and no file of a test (isFaultPath) and each covered by
//     C's tie at PlantedOn where that tie is known, and that counts at S from PlantedOn: PlantedOn is
//     S.Commit, or C's tie is known at PlantedOn and at S.Commit and no path that changed between
//     them is covered by the tie at either commit (carries; the rule that replaces The Spec's R8: "A
//     failing example that a check's test caught (The Spec R9) counts at another commit only while
//     no file that the test reads changed"); never at a subject at its Commit, the commit that CI
//     tested, whatever its PlantedOn names (R9: "For a check that a program runs, the failing
//     example is a fault that a step plants in the code"; faultCommit, countingFaults). Beside
//     those, each failing example of such a check whose record does not say where the step planted
//     its fault (unplanted), or whose PlantedOn names no commit of the history (unplaced), which
//     counts nowhere: when it is the newest, unknown, with the reason that names what the record
//     lacks, no_planted_on, no_fault_paths for no path or only paths of only white space, which are
//     no paths, blank_fault_path for such a path beside a real one, planted_on_tested_commit for a
//     PlantedOn that is its own Commit, or planted_on_outside_history with the unknown commit as
//     Detail, since a later answer of the step decides (R7: "the latest counts"), and such a record
//     says nothing of the code where its fault was planted. For a query or an agent of the code, one
//     that counts at S from its commit by the same rule; for a program check of production or the
//     graph, one whose TestCommit counts at S.Commit by the same rule (R9: "its test, as it is
//     now"); for a query or an agent of production or the graph, the newest.
//     None: unknown, no_failing_example; it did not fail: unknown, failing_example_missed; for a
//     program check, its record does not show that an agent that did not write the test wrote the
//     fault (R9): it names no author of the fault, no author of the test, or the author of the
//     fault among the authors of the test: unknown, fault_by_test_author.
//  8. The newest run of each leg passed: true, passed; each failed: false, failed (R7: "the latest
//     counts, and a later failure cancels an earlier pass").
//
// deriveChecks gives an error, and no state, for an Input that is not whole (Input.check), so the
// judge never reads the zero value of a member that the caller did not set as a real one, and for
// a History that could not answer (History.Changed gives an error), since a history that listed
// nothing has not read two equal trees.
func deriveChecks(in Input) (*checkJudge, []CheckState, error) {
	j, err := newCheckJudge(in)
	if err != nil {
		return nil, nil, err
	}
	states, err := j.states()
	if err != nil {
		return nil, nil, err
	}
	return j, states, nil
}

// newCheckJudge is the judge of the checks of in: the current approved text of each node, which
// approved text of each node the files hold, the checks and statements of the files that the tree
// rules refuse, and the results of each check; or the first fault of in (Input.check). It reads a
// FixtureHistory by the tree of each commit by its id (fixtureTrees), built once, so that the
// judge's question of each commit takes no walk of the list of commits.
func newCheckJudge(in Input) (*checkJudge, error) {
	// Current leaves out a node that the approved spec removed, and Input.check refuses each other
	// node that it would leave out: one under another key than its id, one with no version, and one
	// whose block is not its one block.
	current, _ := in.Approved.Current()
	if err := in.check(current); err != nil {
		return nil, err
	}
	switch h := in.History.(type) {
	case FixtureHistory:
		in.History = h.trees()
	case *FixtureHistory:
		in.History = h.trees()
	}
	j := &checkJudge{in: in, current: current, holds: Compare(in.Files, in.Approved).Holds, refused: refusals(in.Files),
		results: map[string][]Result{}, changed: map[string]changedPaths{}, placed: map[string]bool{}}
	for _, r := range in.Results {
		j.results[r.Check] = append(j.results[r.Check], r)
	}
	return j, nil
}

// states is the state of each check that deriveChecks judges, sorted by id, or the first error of
// the history, with no state.
func (j *checkJudge) states() ([]CheckState, error) {
	var states []CheckState
	for _, c := range j.current.Checks {
		if readsProduction(c) && j.in.Subject.Deploy == "" {
			continue
		}
		state := j.state(c)
		state.ID = c.ID
		states = append(states, state)
	}
	if j.historyErr != nil {
		return nil, j.historyErr
	}
	return states, nil
}

// checkJudge derives the state of each check of one Input.
type checkJudge struct {
	in         Input
	current    Tree                    // the current text of each node of the approved spec
	holds      map[string]Holding      // which approved text of each node the files hold
	refused    map[string]string       // the rule that refuses each check and statement of the files
	results    map[string][]Result     // the results of each check, by its id
	changed    map[string]changedPaths // History.Changed(commit, Subject.Commit), by commit
	placed     map[string]bool         // whether History holds each PlantedOn that a failing example names
	historyErr error                   // the first error of History.Changed; states then gives no state
}

// refusals is the first rule, in Validate's order, that refuses each check and statement of files
// (the derivation gives no clean state for a tree that the rules refuse, since the pilot calls it
// with no reader). A problem that names a check or a statement refuses that node. Any other
// problem, of a component, an interface, a term or no node, refuses each check and statement of
// files: a file is no relation of The Spec, so the split of the tree into files changes no state.
// Validate runs with no Options, as the scenario reader runs it: the files may still hold a node
// that the approved spec removed (Compare). Each problem is read once, by sets of the ids, and none
// after each check and statement is refused, so the time grows with the tree and not with its
// square.
func refusals(files Tree) map[string]string {
	problems := Validate(files, Options{})
	if len(problems) == 0 {
		return nil
	}
	statements := make(map[string]bool, len(files.Statements))
	nodes := make(map[string]bool, len(files.Statements)+len(files.Checks))
	for _, s := range files.Statements {
		statements[s.ID], nodes[s.ID] = true, true
	}
	for _, c := range files.Checks {
		nodes[c.ID] = true
	}
	// The loop rule of Validate names the smallest id of a loop, and the loop refuses each statement
	// of its strongly connected part (loopParts), so that no state depends on the order of the ids.
	loopOf := map[string][]string{}
	for _, part := range loopParts(statementEdges(files.Statements)) {
		for _, id := range part {
			loopOf[id] = part
		}
	}
	refused := map[string]string{}
	refuse := func(rule string, ids ...string) {
		for _, id := range ids {
			if _, done := refused[id]; !done {
				refused[id] = rule
			}
		}
	}
	for _, p := range problems {
		switch {
		case p.Rule == "loop" && statements[p.Node]:
			refuse(p.Rule, loopOf[p.Node]...)
		case nodes[p.Node]:
			refuse(p.Rule, p.Node)
		default:
			refuse(p.Rule, slices.Collect(maps.Keys(nodes))...)
		}
		if len(refused) == len(nodes) {
			break // each is refused, and a later problem changes no rule
		}
	}
	return refused
}

// changedPaths is one answer of History.Changed.
type changedPaths struct {
	paths []string
	ok    bool
}

// state is the state of c at the subject, in the order of deriveChecks.
func (j *checkJudge) state(c Check) CheckState {
	if !j.holds[c.ID].holdsCurrent() {
		return unknownCheck(ReasonNotBuilt, "")
	}
	if !hasWord(c.FailingExample) {
		return unknownCheck(reasonNoFailingExample, "")
	}
	if rule, refused := j.refused[c.ID]; refused {
		return unknownCheck(reasonRefused, rule)
	}
	if rule, refused := j.in.Uncited[j.in.Subject.Commit][c.ID]; refused {
		return unknownCheck(reasonUncited, rule)
	}
	job := checkJob(c)
	runs, faults, statement, suspect := j.candidates(c, job)
	rule, refused, runs, faults := j.withoutRefused(c, runs, faults)
	counting := j.countingRuns(c, runs)
	if refused && len(counting) == 0 {
		return unknownCheck(reasonUncited, rule)
	}
	if suspect && len(runs) == 0 && len(faults) == 0 {
		return unknownCheck(reasonStatementChanged, statement)
	}

	if len(counting) == 0 {
		switch {
		case job != "" && j.jobEnded(c, job):
			return unknownCheck(reasonCIFault, "")
		case len(runs) > 0:
			return unknownCheck(reasonSubjectMoved, "")
		default:
			return unknownCheck(reasonNoResult, "")
		}
	}
	latest, _ := newestResult(counting)
	newest := map[string]Result{latest.Leg: latest}
	if countsAtACommit(c) { // the checks that a job's matrix runs
		newest = newestOfEachLeg(counting)
		switch leg, ended, found := j.legWithNoResult(c, job, newest); {
		case job == "" || !found:
		case ended:
			return unknownCheck(reasonCIFault, leg)
		default:
			return unknownCheck(reasonNoResult, leg)
		}
	}
	if c.Subject != SubjectProduction && answersDisagree(c, counting) || legsDisagree(newest) {
		state := unknownCheck(reasonDisagree, "")
		state.Disagree = true
		return state
	}
	for _, r := range newest {
		if r.Outcome != OutcomePass && r.Outcome != OutcomeFail {
			return unknownCheck(reasonCouldNotRun, "")
		}
	}

	for _, r := range newest {
		if c.TimeLimit > 0 && j.in.Now.Sub(r.At) > c.TimeLimit {
			return unknownCheck(reasonExpired, "")
		}
	}

	fault, caught := newestResult(j.countingFaults(c, faults))
	if caught && countsFromPlantedOn(c) {
		if reason, detail := j.unknownPlacement(fault); reason != "" {
			return unknownCheck(reason, detail)
		}
	}
	switch {
	case !caught:
		return unknownCheck(reasonNoFailingExample, "")
	case fault.Outcome != OutcomeFail:
		return unknownCheck(reasonFailingExampleMissed, "")
	case c.Procedure == ProcedureProgram && !byAnotherAgent(fault):
		return unknownCheck(reasonFaultByTestAuthor, "")
	}

	// After step 5 the newest run of each leg has latest's outcome: latest is the newest of its leg.
	from := readOf(c, latest.Commit, latest.Deploy, latest.GraphTx)
	if latest.Outcome == OutcomePass {
		return CheckState{State: CheckTrue, Reason: reasonPassed, From: from}
	}
	return CheckState{State: CheckFalse, Reason: reasonFailed, From: from}
}

// candidates is C's results of step 3, the runs and the failing examples apart, and the statement
// whose approval made results suspect: a result that carries the digest of C's current text and
// was recorded at or after the approval of that text, of kind run or failing_example, a run of a
// program check only when job reported it, and only when recorded at or after the newest approval
// of C's statement and of each statement above it (newestApproval); suspect is whether a result
// passed every test but that last.
func (j *checkJudge) candidates(c Check, job string) (runs, faults []Result, statement string, suspect bool) {
	statement, since := j.newestApproval(c.Serves)
	digest, approved := c.Digest(), j.in.Approved.Since(c.ID)
	for _, r := range j.results[c.ID] {
		switch {
		case r.CheckDigest != digest || r.At.Before(approved) || r.Kind != ResultRun && r.Kind != ResultFailingExample:
		case r.Kind == ResultRun && c.Procedure == ProcedureProgram && r.Job != job:
		case r.At.Before(since):
			suspect = true
		case r.Kind == ResultRun:
			runs = append(runs, r)
		default:
			faults = append(faults, r)
		}
	}
	return runs, faults, statement, suspect
}

// byAnotherAgent is whether the record of the failing example fault shows that an agent that did
// not write the test wrote the fault (R9: "An agent that did not write the test writes the fault"):
// it names the author of the fault and one or more authors of the test, and the author of the fault
// is none of them. A record that names no author of one of the two cannot show it, and an author
// of no letter and no digit, by the test that Validate reads a missing text with (hasWord), names
// nobody. Two names that could be one agent's count as one agent, the safe reading, since a fault
// whose author is the test's would read as one by another agent otherwise: the white space at the
// ends of a name and inside it counts as one space, and case does not count (agentName,
// strings.EqualFold), so "alice", " alice" and "Alice" are one agent.
func byAnotherAgent(fault Result) bool {
	namesTestAuthor := slices.ContainsFunc(fault.TestAuthors, hasWord)
	author := agentName(fault.FaultAuthor)
	return hasWord(fault.FaultAuthor) && namesTestAuthor &&
		!slices.ContainsFunc(fault.TestAuthors, func(a string) bool { return strings.EqualFold(agentName(a), author) })
}

// agentName is a name of an agent as byAnotherAgent compares it: its words, one space between two.
func agentName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

// newestApproval is the statement, of statement and each statement above it in the approved spec,
// whose current text was approved last, and the time of that approval.
func (j *checkJudge) newestApproval(statement string) (string, time.Time) {
	newest, at := "", time.Time{}
	for seen := map[string]bool{}; statement != "" && !seen[statement]; {
		seen[statement] = true
		if since := j.in.Approved.Since(statement); since.After(at) {
			newest, at = statement, since
		}
		s, held := j.current.StatementByID(statement)
		if !held {
			break
		}
		statement = s.Parent
	}
	return newest, at
}

// countingRuns is the runs of c that count at the subject (step 4): for a check whose results count
// at a commit (countsAtACommit), the runs of a commit where a run of c counts at the subject
// (runCounts); and for a check of production or of the graph, of those, the runs that read the
// subject's deploy or its transaction of the graph.
func (j *checkJudge) countingRuns(c Check, runs []Result) []Result {
	if countsAtACommit(c) {
		runs = resultsWhere(runs, func(r Result) bool { return j.runCounts(c, resultCommit(r)) })
	}
	if c.Subject == SubjectCode {
		return runs
	}
	s := j.in.Subject
	subject := readOf(c, s.Commit, s.Deploy, s.GraphTx)
	return resultsWhere(runs, func(r Result) bool { return subject != "" && readOf(c, r.Commit, r.Deploy, r.GraphTx) == subject })
}

// runCounts is whether a run of c, or a run of c's job, at commit counts at the subject (step 4).
// For a check of the code, commit is the commit that the run read, and the commit that its job
// tested, so the run counts there alone: only at the subject's own commit, also where no file that
// c's tie covers changed between the two. For a program check of production or of the graph,
// commit is the commit of its test, and the run read a deploy or a transaction of the graph, which
// countingRuns matches with the subject's; the run counts only while its test, which is code, is
// the test at the subject's commit (carries), so no run counts across a change of the test.
func (j *checkJudge) runCounts(c Check, commit string) bool {
	if c.Subject == SubjectCode {
		return commit == j.in.Subject.Commit
	}
	return j.carries(c.ID, commit)
}

// countsAtACommit is whether the commit where the job of a result of c ran (resultCommit) decides
// where the result counts (steps 2, 4 and 7): a check of the code, or a program check of
// production or of the graph, whose test is code. A query or an agent of production or of the
// graph reads no commit.
func countsAtACommit(c Check) bool {
	return c.Subject == SubjectCode || c.Procedure == ProcedureProgram
}

// readOf is, of a commit, a deploy and a transaction of the graph, the subject that a check like c
// reads (R7: "A result counts only for the procedure that ran it and for the subject that it
// read"): the commit for a check of the code, the deploy for one of production, the transaction
// for one of the graph. It reads the subject of the derivation and the subject of a result alike.
func readOf(c Check, commit, deploy, graphTx string) string {
	switch c.Subject {
	case SubjectProduction:
		return deploy
	case SubjectGraph:
		return graphTx
	}
	return commit
}

// countingFaults is the failing examples of c that count at the subject (step 7), with each of a
// program check of the code that counts nowhere (unknownPlacement) beside them, so that step 7
// reads the check unknown when such a record is the newest.
func (j *checkJudge) countingFaults(c Check, faults []Result) []Result {
	switch {
	case !countsAtACommit(c):
		return faults
	case !countsFromPlantedOn(c):
		return resultsWhere(faults, func(r Result) bool { return j.carries(c.ID, resultCommit(r)) })
	}
	return resultsWhere(faults, func(r Result) bool {
		reason, _ := j.unknownPlacement(r)
		return reason != "" || r.Commit != j.in.Subject.Commit && j.carries(c.ID, j.faultCommit(c, r))
	})
}

// countsFromPlantedOn is whether c counts a failing example from the commit that the step planted
// its fault on, rather than where its job ran: a program check of the code (R9: "For a check that a
// program runs, the failing example is a fault that a step plants in the code").
func countsFromPlantedOn(c Check) bool {
	return c.Subject == SubjectCode && c.Procedure == ProcedureProgram
}

// unplanted is the reason of step 7 for a failing example r of a program check of the code whose
// record does not say where the step planted its fault (no_planted_on, no_fault_paths,
// blank_fault_path, planted_on_tested_commit), and "" when it says.
func unplanted(r Result) string {
	blank := func(path string) bool { return strings.TrimSpace(path) == "" }
	switch {
	case r.PlantedOn == "":
		return reasonNoPlantedOn
	case !slices.ContainsFunc(r.FaultPaths, func(path string) bool { return !blank(path) }):
		return reasonNoFaultPaths
	case slices.ContainsFunc(r.FaultPaths, blank):
		return reasonBlankFaultPath
	case r.PlantedOn == r.Commit:
		return reasonPlantedOnTested
	}
	return ""
}

// unknownPlacement is the reason and detail of step 7 for a failing example r of a program check of
// the code that does not say where its fault was planted (unplanted), or whose PlantedOn names no
// commit of the history (unplaced), and "" for a record that a commit of the history can place.
func (j *checkJudge) unknownPlacement(r Result) (reason, detail string) {
	if reason := unplanted(r); reason != "" {
		return reason, ""
	}
	if j.unplaced(r) {
		return ReasonPlantedOnOutside, r.PlantedOn
	}
	return "", ""
}

// unplaced is whether r's PlantedOn names no commit of the history. A History answers that a commit
// it does not hold has no tree, so Changed(commit, commit) is not ok for it; an error is kept in
// historyErr, and the derivation then gives no state.
func (j *checkJudge) unplaced(r Result) bool {
	if held, read := j.placed[r.PlantedOn]; read {
		return !held
	}
	_, held, err := j.in.History.Changed(r.PlantedOn, r.PlantedOn)
	if err != nil && j.historyErr == nil {
		j.historyErr = err
	}
	j.placed[r.PlantedOn] = held
	return !held
}

// faultCommit is the commit that the failing example r of c counts from (step 7), "" for none: for
// a program check of the code, its PlantedOn when the record can be placed and each path of the
// fault is a source file that c's tie there covers where it is known, never its Commit; for each
// other check, the commit where r's job ran (resultCommit).
func (j *checkJudge) faultCommit(c Check, r Result) string {
	if !countsFromPlantedOn(c) {
		return resultCommit(r)
	}
	tie := j.in.Ties[r.PlantedOn][c.ID]
	if reason, _ := j.unknownPlacement(r); reason != "" || slices.ContainsFunc(r.FaultPaths, func(path string) bool {
		return !isFaultPath(path) || tie.Known && !tie.Covers(path)
	}) {
		return ""
	}
	return r.PlantedOn
}

// isFaultPath is whether path is a path that a planted fault may change: a source file, which ends
// in .go, .ts or .tsx, and no file of a test, whose name ends in _test.go, .test.ts, .test.tsx or
// .e2e.ts, or one of whose directories starts with test, ends with test or tests, or is __tests__
// or e2e. A fault in a file of a test changes the test and not the code, and R9 asks a fault "in
// the code" on which the test "as it is now" fails.
func isFaultPath(path string) bool {
	dirs := strings.Split(path, "/")
	name := dirs[len(dirs)-1]
	dirs = dirs[:len(dirs)-1]
	endsIn := func(ends ...string) bool {
		return slices.ContainsFunc(ends, func(end string) bool { return strings.HasSuffix(name, end) })
	}
	testDir := func(dir string) bool {
		return strings.HasPrefix(dir, "test") || strings.HasSuffix(dir, "test") || strings.HasSuffix(dir, "tests") || dir == "__tests__" || dir == "e2e"
	}
	return endsIn(".go", ".ts", ".tsx") && !endsIn("_test.go", ".test.ts", ".test.tsx", ".e2e.ts") && !slices.ContainsFunc(dirs, testDir)
}

// carries is whether a failing example of the check id at commit, or a result of the test of a
// program check of production or of the graph at commit, counts at the subject's commit (steps 4
// and 7; the rule that replaces The Spec's R8, which keeps a rule on files for a failing example
// alone and never makes a pass of the code count at another commit): the
// two are one commit, or the check's tie is known at both commits and no path that differs between
// their trees is covered by the tie at either commit, since a tie is a fact of a commit (R18: "At
// each commit, Dispatch derives four ties"), and a file that the test read at one commit and that
// the other deleted or added changed the code that the check runs. A History that gives an error is
// kept in historyErr, and the derivation then gives no state, so the false that carries answers
// with it is read by no state.
func (j *checkJudge) carries(check, commit string) bool {
	switch {
	case commit == "":
		return false
	case commit == j.in.Subject.Commit:
		return true
	}
	from, to := j.in.Ties[commit][check], j.in.Ties[j.in.Subject.Commit][check]
	if !from.Known || !to.Known {
		return false
	}
	changed, read := j.changed[commit]
	if !read {
		var err error
		changed.paths, changed.ok, err = j.in.History.Changed(commit, j.in.Subject.Commit)
		if err != nil && j.historyErr == nil {
			j.historyErr = err
		}
		j.changed[commit] = changed
	}
	return changed.ok && !slices.ContainsFunc(changed.paths, func(path string) bool { return from.Covers(path) || to.Covers(path) })
}

// withoutRefused takes out of runs and faults, the candidates of c, each result of a commit where
// CI refused c that supports true (step 2; supportsTrue), a run by the commit where its job ran
// (resultCommit) and a failing example by the commit that it counts from (faultCommit). refused is
// whether a result taken out would have counted at the subject, and rule the least rule, by its
// name, of those refusals. A check whose results count at no commit (countsAtACommit) keeps each
// candidate.
func (j *checkJudge) withoutRefused(c Check, runs, faults []Result) (rule string, refused bool, keptRuns, keptFaults []Result) {
	if !countsAtACommit(c) {
		return "", false, runs, faults
	}
	kept := func(commitOf func(Result) string, counts func(commit string) bool) func(Result) bool {
		return func(r Result) bool {
			commit := commitOf(r)
			other, at := j.in.Uncited[commit][c.ID]
			if !at || !supportsTrue(r) {
				return true
			}
			if counts(commit) && (!refused || other < rule) {
				rule, refused = other, true
			}
			return false
		}
	}
	keptRuns = resultsWhere(runs, kept(resultCommit, func(commit string) bool { return j.runCounts(c, commit) }))
	keptFaults = resultsWhere(faults, kept(func(r Result) string { return j.faultCommit(c, r) }, func(commit string) bool { return j.carries(c.ID, commit) }))
	return rule, refused, keptRuns, keptFaults
}

// supportsTrue is whether the result r, a run or a failing example, is evidence toward true: a run
// that passed, or a failing example on which the test failed, as it must (R9). A failure, a run that
// could not run and a failing example that missed its fault each keep a check from true.
func supportsTrue(r Result) bool {
	if r.Kind == ResultFailingExample {
		return r.Outcome == OutcomeFail
	}
	return r.Outcome == OutcomePass
}

// jobEnded is whether a run of job, in any leg, ended at a commit where a run of c counts at the
// subject (runCounts): for a check of the code, the subject's commit alone.
func (j *checkJudge) jobEnded(c Check, job string) bool {
	return slices.ContainsFunc(j.in.JobRuns, func(r JobRun) bool { return r.Job == job && r.Ended && j.runCounts(c, r.Commit) })
}

// legWithNoResult is the first leg, by name, of a run of job at a commit where a run of c counts at
// the subject (runCounts), in which no run of c counts, newest being the newest run of
// each leg that counts: of the runs that ended, else of the runs that have not ended; ended is
// whether its run ended, and found whether there is one.
func (j *checkJudge) legWithNoResult(c Check, job string, newest map[string]Result) (leg string, ended, found bool) {
	var silent, running []string
	for _, r := range j.in.JobRuns {
		if _, reported := newest[r.Leg]; r.Job != job || reported || !j.runCounts(c, r.Commit) {
			continue
		}
		if r.Ended {
			silent = append(silent, r.Leg)
		} else {
			running = append(running, r.Leg)
		}
	}
	switch {
	case len(silent) > 0:
		return slices.Min(silent), true, true
	case len(running) > 0:
		return slices.Min(running), false, true
	}
	return "", false, false
}

// answersDisagree is whether the runs of c that count, which read one subject that cannot change
// (the subject's commit, or its transaction of the graph), give two answers
// (step 5): a pass and a failure at one subject, or a pass recorded after a failure. Only a failure
// recorded after every pass is a later answer, which cancels them (R7).
func answersDisagree(c Check, counting []Result) bool {
	type answers struct{ passed, failed bool }
	bySubject := map[string]answers{}
	failed := false
	for _, r := range slices.SortedFunc(slices.Values(counting), byRecord) {
		at := readOf(c, r.Commit, r.Deploy, r.GraphTx)
		a := bySubject[at]
		switch r.Outcome {
		case OutcomePass:
			if failed {
				return true
			}
			a.passed = true
		case OutcomeFail:
			failed = true
			a.failed = true
		}
		if a.passed && a.failed {
			return true
		}
		bySubject[at] = a
	}
	return false
}

// legsDisagree is whether the newest runs of two legs, one passed and one failed (step 5): no leg
// replaces the result of another (R12).
func legsDisagree(newest map[string]Result) bool {
	passed, failed := false, false
	for _, r := range newest {
		passed = passed || r.Outcome == OutcomePass
		failed = failed || r.Outcome == OutcomeFail
	}
	return passed && failed
}

// readsProduction is whether c reads production or the graph rather than the code, so that it has a
// state only at a subject that names a deploy, the commit of its test is no subject of it, and a
// result of it names that commit as TestCommit.
func readsProduction(c Check) bool {
	return c.Subject == SubjectProduction || c.Subject == SubjectGraph
}

// checkJob is the job whose reports alone count for c (R6: "only the reports of that job count"),
// "" when c's text names none.
func checkJob(c Check) string {
	if c.Test == nil {
		return ""
	}
	return c.Test.Job
}

// newestOfEachLeg is the newest of results in each leg, by leg.
func newestOfEachLeg(results []Result) map[string]Result {
	newest := map[string]Result{}
	for _, r := range results {
		if old, held := newest[r.Leg]; !held || byRecord(r, old) > 0 {
			newest[r.Leg] = r
		}
	}
	return newest
}

// newestResult is the newest of results by (At, Seq), and false when there is none.
func newestResult(results []Result) (Result, bool) {
	if len(results) == 0 {
		return Result{}, false
	}
	return slices.MaxFunc(results, byRecord), true
}

// byRecord orders results by Dispatch's record time, then its order of record.
func byRecord(a, b Result) int {
	return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.Seq, b.Seq))
}

// resultsWhere is the results that keep accepts, in their order.
func resultsWhere(results []Result, keep func(Result) bool) []Result {
	var kept []Result
	for _, r := range results {
		if keep(r) {
			kept = append(kept, r)
		}
	}
	return kept
}

// unknownCheck is the unknown state of a check with reason and detail.
func unknownCheck(reason, detail string) CheckState {
	return CheckState{State: checkUnknown, Reason: reason, Detail: detail}
}
