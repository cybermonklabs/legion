package specgraph

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// A member of an Input that the caller did not set is its zero value, and the derivation would read
// that value as a real one: a failure with no time as older than every approval, one with no seq as
// the first record of its time, a run with no subject, no job or no test commit as one that counts
// nowhere, a subject with no now as one at which no time limit ends, an approved text with no time
// as one that makes no result suspect, a commit of the history with no files and a known tie with no
// path as no change between two commits, a job run with no job, no commit or not ended as no run of
// the check's job, no list of job runs or of the checks that CI refused as none, and no ties or no
// history as a caller that knows none, with which a failure at another commit counts nowhere. Each
// turns a state toward true or hides a failure (R7: "A pass and a failure on one such subject and one
// procedure make the state unknown", "a later failure cancels an earlier pass"), and so do an
// approved node under another key than its id, which Current leaves out, and a subject that is no
// subject of R10, so Derive and deriveChecks give an error and no state, in the words that the
// scenario reader refuses the same input with.
// Each row: the case of TestCheckState, at c1, the head of branch b, with the row's checks added,
// its setup, and its change of the Input. The three rows that name a test of their own are that
// test's rows, whose input is refused here: they read unknown before.
func TestTheDerivationRefusesAnInputThatIsNotWhole(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		add    []Check          // checks that the case approves and the files hold before the Input is made
		setup  func(*checkCase) // a change of the case before the Input is made; nil for none
		change func(in *Input)  // a change of the Input; nil for none
		want   string           // the error; "" for none
	}{
		{name: "control: the case as it is"},
		{name: "a result with no at", change: func(in *Input) { in.Results[0].At = time.Time{} },
			want: "specgraph: result 1: it sets no at"},
		{name: "a failing example with no at", change: func(in *Input) { in.Results[1].At = time.Time{} },
			want: "specgraph: result 2: it sets no at"},
		{name: "a result with no check", change: func(in *Input) { in.Results[0].Check = "" },
			want: "specgraph: result 1: it sets no check"},
		{name: "a result with no digest", change: func(in *Input) { in.Results[0].CheckDigest = "" },
			want: "specgraph: result 1: it carries no digest of the text of its check"},
		{name: "a result of another kind", change: func(in *Input) { in.Results[0].Kind = "" },
			want: `specgraph: result 1: kind "" is not run or failing_example`},
		{name: "a result of another outcome", change: func(in *Input) { in.Results[0].Outcome = "" },
			want: `specgraph: result 1: outcome "" is not pass, fail or could_not_run`},
		{name: "a result that names two subjects", change: func(in *Input) { in.Results[0].Deploy = "d1" },
			want: "specgraph: result 1: it names the subjects commit and deploy; a result reads one subject"},
		{name: "a run of a check of the code with no commit", change: func(in *Input) { in.Results[0].Commit = "" },
			want: "specgraph: result 1: it sets no commit, the subject that a check of the code reads"},
		{name: "a failing example of a check of the code with no commit", change: func(in *Input) { in.Results[1].Commit = "" },
			want: "specgraph: result 2: it sets no commit, the subject that a check of the code reads"},
		{name: "a run of a program check with no job", change: func(in *Input) { in.Results[0].Job = "" },
			want: "specgraph: result 1: it sets no job, and only the reports of the job of its check count"},
		{name: "a run of a check of production with no deploy", add: []Check{checkStateProduction}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultRun,
				Outcome: OutcomeFail, Leg: "", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no deploy, the subject that a check of production reads"},
		{name: "a run of a check of the graph with no graph_tx", add: []Check{checkStateGraph}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateGraph.ID, CheckDigest: checkStateGraph.Digest(), Kind: ResultRun,
				Outcome: OutcomeFail, Deploy: "d1", Leg: "", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no graph_tx, the subject that a check of the graph reads"},
		{name: "control: a failing example of a query of production names no subject", add: []Check{checkStateProduction}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultFailingExample,
				Outcome: OutcomeFail, Leg: "", At: checkStateRun, Seq: 3})
		}},
		{name: "a run of a program check of production with no test_commit", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
				Deploy: "d1", Job: q.Test.Job, Leg: "", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no test_commit, the commit of the test that ran"},
		{name: "a failing example of a program check of production with no test_commit", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultFailingExample, Outcome: OutcomePass,
				Leg: "", At: checkStateRun, Seq: 3, FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
		}, want: "specgraph: result 3: it sets no test_commit, the commit of the test that ran"},
		{name: "TestCheckState, item 19: the failing example of a program check of production names no commit of its test",
			setup: func(s *checkCase) { s.productionProgram("docs/x.md", "") },
			want:  "specgraph: result 4: it sets no test_commit, the commit of the test that ran"},
		{name: "TestARunOfProductionOrTheGraphCountsForTheCodeThatRanIt: production, the pass names no commit of its test",
			setup: func(s *checkCase) {
				s.programPass(checkStateProductionProgram, "", "c2")
				s.remove(checkStateCheck.ID)
			}, want: "specgraph: result 3: it sets no test_commit, the commit of the test that ran"},
		{name: "TestAGraphCheckReadsTheTransactionOfTheSubject: a pass that names no transaction, at a deploy that names none",
			setup: func(s *checkCase) {
				g := checkStateGraph
				s.add(g)
				s.tieLikeC(g.ID)
				s.commit("c2", "c1", "packages/envoy/cmd/listener/restart_test.go")
				s.Subject = Point{Commit: "c2", Deploy: "d2"}
				s.Results = append(s.Results, Result{Check: g.ID, Kind: ResultRun, Outcome: OutcomePass, At: checkStateRun.Add(time.Hour)},
					Result{Check: g.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, TestCommit: "c2",
						At: checkStateRun.Add(5 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
				s.remove(checkStateCheck.ID)
			}, want: "specgraph: result 3: it sets no graph_tx, the subject that a check of the graph reads"},
		{name: "control: a result of an earlier text of its check is judged by the members that each result sets", change: func(in *Input) {
			in.Results[0].CheckDigest, in.Results[0].Commit, in.Results[0].Job = "an-earlier-text", "", ""
		}},
		{name: "two results of a check at one at and one seq", change: func(in *Input) {
			in.Results[1].At, in.Results[1].Seq = in.Results[0].At, in.Results[0].Seq
		}, want: "specgraph: result 2: it has the at and the seq of result 1, so neither of the two is the later"},
		// R7: "Of several results that count, the latest counts, and a later failure cancels an
		// earlier pass." The order of record is at, then seq, which Dispatch numbers from 1: a result
		// with seq 0 would sort before each result of its at, so a later failure would be hidden.
		{name: "a result with no seq", change: func(in *Input) { in.Results[1].Seq = 0 },
			want: "specgraph: result 2: it sets no seq"},
		{name: "a result whose seq is below 1", change: func(in *Input) { in.Results[0].Seq = -4 },
			want: "specgraph: result 1: it sets no seq"},
		{name: "a failure of production with no seq, at the at of its pass", add: []Check{checkStateProduction}, setup: func(s *checkCase) {
			s.Subject = Point{Commit: "c1", Deploy: "d1"}
			s.Results = append(s.Results, productionPassed(checkStateProduction, "d1")...)
			s.Results = append(s.Results, productionRun(checkStateProduction, "d1", OutcomeFail, checkStateRun))
		}, change: func(in *Input) { in.Results[4].Seq = 0 }, want: "specgraph: result 5: it sets no seq"},
		{name: "control: the same failure of production, recorded after its pass",
			add: []Check{checkStateProduction}, setup: func(s *checkCase) {
				s.Subject = Point{Commit: "c1", Deploy: "d1"}
				s.Results = append(s.Results, productionPassed(checkStateProduction, "d1")...)
				s.Results = append(s.Results, productionRun(checkStateProduction, "d1", OutcomeFail, checkStateRun))
			}},
		// The entry reads each approved node by its key, as Current does, so a node under another key
		// than its own id would be left out, with its checks (R10: "Not met: one of them is false").
		{name: "an approved node under another key than its id", change: func(in *Input) {
			in.Approved.Nodes["x-"+checkStateCheck.ID] = in.Approved.Nodes[checkStateCheck.ID]
			delete(in.Approved.Nodes, checkStateCheck.ID)
		}, want: `specgraph: approved spec: node "a-restart-loses-no-message" is under the id "x-a-restart-loses-no-message"`},
		// R22: "Dispatch refuses the approval of a change request that puts a statement under a parent
		// that the approved spec does not hold." The derivation reads a relation only to a node of its
		// kind, so the entry refuses, as the reader does, a relation of an approved block that names no
		// node of the approved spec of its kind (approvedFault), and a check whose block leaves out
		// serves, which names no statement (R6).
		{name: "an approved parent that names no node", change: func(in *Input) {
			approveText(in, func(s *Statement) { s.Parent = "no-such-statement" })
		}, want: `specgraph: approved node "a-stored-message-survives-a-restart" version 1: bad-approved-block: parent "no-such-statement" names no node of the approved spec (line 1 of its block)`},
		{name: "an approved parent that names a check", change: func(in *Input) {
			approveText(in, func(s *Statement) { s.Parent = checkStateCheck.ID })
		}, want: `specgraph: approved node "a-stored-message-survives-a-restart" version 1: bad-approved-block: parent "a-restart-loses-no-message" names a check, not a statement (line 1 of its block)`},
		// R5: "The parent and support relations taken together form no cycle, and Dispatch refuses a
		// change that makes one."
		{name: "a loop of parents in the approved spec", change: func(in *Input) {
			top := checkStateTop
			top.Parent = checkStateLeaf.ID
			block, err := top.Print()
			if err != nil {
				panic(err)
			}
			n := in.Approved.Nodes[top.ID]
			n.Versions = []ApprovedVersion{{Version: 1, At: checkStateApproved, Block: block}}
			in.Approved.Nodes[top.ID] = n
		}, want: "specgraph: approved spec: loop: a-stored-message-survives-a-restart -[parent]-> messages-reach-sessions -[parent]-> a-stored-message-survives-a-restart"},
		{name: "an approved node with no id", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.ID = ""
			delete(in.Approved.Nodes, checkStateCheck.ID)
			in.Approved.Nodes[""] = n
		}, want: "specgraph: approved spec: a node has no id"},
		{name: "a subject with no commit", change: func(in *Input) { in.Subject.Commit = "" },
			want: "specgraph: subject: it sets no commit"},
		{name: "a subject with no now", change: func(in *Input) { in.Now = time.Time{} },
			want: "specgraph: subject: it sets no now"},
		{name: "a subject with neither branch nor deploy", change: func(in *Input) { in.Subject.Branch = "" },
			want: "specgraph: subject: it sets neither branch nor deploy; a subject sets exactly one of them"},
		// R10: "The subject is the head of the branch that the files are read from, or the commit that
		// production runs"; one subject is one of the two, and the head of a branch reads no graph.
		{name: "a subject with a branch and a deploy", change: func(in *Input) { in.Subject.Deploy = "d1" },
			want: "specgraph: subject: it sets both branch and deploy; a subject sets exactly one of them"},
		{name: "a subject of a branch with a transaction of the graph", change: func(in *Input) { in.Subject.GraphTx = "tx1" },
			want: "specgraph: subject: it sets graph_tx with no deploy; the commit that production runs reads the graph, and the head of a branch does not"},
		{name: "an approved text with no time", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateLeaf.ID]
			n.Versions = []ApprovedVersion{{Version: 1, Block: n.Versions[0].Block}}
			in.Approved.Nodes[checkStateLeaf.ID] = n
		}, want: `specgraph: approved node "a-stored-message-survives-a-restart" version 1: it has no time`},
		{name: "an approved node with no version", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.Versions = nil
			in.Approved.Nodes[checkStateCheck.ID] = n
		}, want: `specgraph: approved node "a-restart-loses-no-message": it has no version`},
		{name: "an approved text with no block", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.Versions = []ApprovedVersion{{Version: 1, At: checkStateApproved}}
			in.Approved.Nodes[checkStateCheck.ID] = n
		}, want: `specgraph: approved node "a-restart-loses-no-message" version 1: bad-approved-block: the block holds 0 nodes, not one (line 1 of its block)`},
		{name: "a commit of the history with no files", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1"}}}
		}, want: `specgraph: history: commit "c1": it sets no files`},
		// A tree with no path, or a content label of no letter and no digit, would read as a tree, or
		// a content, that other commits share, so a failing example would count across them
		// (deriveChecks, step 7), and a label changed to " " as a change of a file that a tie covers.
		{name: "a commit of the history whose tree holds no path", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{}}}}
		}, want: `specgraph: history: commit "c1": its files hold no path, and the tree of a commit holds at least one file`},
		{name: "a content label of the history that is empty", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{"go.work": ""}}}}
		}, want: `specgraph: history: commit "c1": the content label of "go.work" has no letter and no digit, so it names no content`},
		{name: "a content label of the history of no letter and no digit", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{"go.work": " "}}}}
		}, want: `specgraph: history: commit "c1": the content label of "go.work" has no letter and no digit, so it names no content`},
		// A nil value of each type that implements History, a type of the package or of the caller,
		// would panic where a result carries, and an Input with no history gives a FixtureHistory of
		// no commit, with which no result carries.
		{name: "a nil *FixtureHistory", change: func(in *Input) { in.History = (*FixtureHistory)(nil) },
			want: "specgraph: history: a nil *specgraph.FixtureHistory is no history; an Input with no history sets FixtureHistory{Commits: []FixtureCommit{}}"},
		{name: "a nil *GitHistory", change: func(in *Input) { in.History = (*GitHistory)(nil) },
			want: "specgraph: history: a nil *specgraph.GitHistory is no history; an Input with no history sets FixtureHistory{Commits: []FixtureCommit{}}"},
		{name: "a nil History of a type of the caller", change: func(in *Input) { in.History = historyOfTheCaller(nil) },
			want: "specgraph: history: a nil specgraph.historyOfTheCaller is no history; an Input with no history sets FixtureHistory{Commits: []FixtureCommit{}}"},
		{name: "a GitHistory with no Dir", change: func(in *Input) { in.History = GitHistory{} },
			want: "specgraph: history: a GitHistory sets no Dir, so git would read the clone of the working directory of the process"},
		// A caller that gives no History, or no Ties, is not one that knows that it has none: with no
		// carry a failure at another commit says nothing about the subject's, so the state could read
		// true where the whole input reads unknown. So it states each, empty for none: the empty value
		// that is not nil, as a file sets "commits": [] and is refused with no commits.
		{name: "no History", change: func(in *Input) { in.History = nil },
			want: "specgraph: it sets no History; an Input with no history sets FixtureHistory{Commits: []FixtureCommit{}}"},
		{name: "control: a FixtureHistory of no commit", change: func(in *Input) { in.History = FixtureHistory{Commits: []FixtureCommit{}} }},
		{name: "a FixtureHistory that does not set its commits", change: func(in *Input) { in.History = FixtureHistory{} },
			want: "specgraph: history: it sets no commits; a history of no commit sets an empty list"},
		{name: "a *FixtureHistory that does not set its commits", change: func(in *Input) { in.History = &FixtureHistory{} },
			want: "specgraph: history: it sets no commits; a history of no commit sets an empty list"},
		{name: "no Ties", change: func(in *Input) { in.Ties = nil },
			want: "specgraph: it sets no Ties; an Input that knows no tie sets CommitTies{}"},
		{name: "control: an empty map of ties", change: func(in *Input) { in.Ties = CommitTies{} }},
		{name: "a commit of Ties with no map", change: func(in *Input) { in.Ties = CommitTies{"c1": nil} },
			want: `specgraph: ties: commit "c1": it sets no map of the ties there, and a commit where no tie is known has an empty map or no key`},
		{name: "control: a commit of Ties with an empty map", change: func(in *Input) { in.Ties = CommitTies{"c1": {}} }},
		{name: "a known tie with no path", change: func(in *Input) { in.Ties = CommitTies{"c1": {checkStateCheck.ID: {Known: true}}} },
			want: `specgraph: ties: commit "c1": check "a-restart-loses-no-message": specgraph: a tie holds no path`},
		{name: "control: a tie that is not known has no path", change: func(in *Input) { in.Ties = CommitTies{"c1": {checkStateCheck.ID: {}}} }},
		// M3: a caller that wrote the paths of a tie and left Known out would read as one with no tie
		// (R18: "A tie that Dispatch cannot derive reads as unknown, not as none").
		{name: "a tie that is not known and holds a path", change: func(in *Input) {
			in.Ties = CommitTies{"c1": {checkStateCheck.ID: {Paths: in.Ties["c1"][checkStateCheck.ID].Paths}}}
		}, want: `specgraph: ties: commit "c1": check "a-restart-loses-no-message": a tie that is not known holds no path, and a tie that holds one is known`},
		{name: "a commit of Ties of no letter and no digit", change: func(in *Input) { in.Ties = CommitTies{" ": {}} },
			want: `specgraph: ties: commit " ": a commit of no letter and no digit names no commit`},
		{name: "a check of Ties of no letter and no digit", change: func(in *Input) { in.Ties = CommitTies{"c1": {"": in.Ties["c1"][checkStateCheck.ID]}} },
			want: `specgraph: ties: commit "c1": check "": a check of no letter and no digit names no check`},
		{name: "a job run with no job", change: func(in *Input) { in.JobRuns = []JobRun{{Leg: "", Commit: "c1", Ended: true}} },
			want: "specgraph: job run 1: it sets no job"},
		{name: "a job run with no commit", change: func(in *Input) { in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Ended: true}} },
			want: "specgraph: job run 1: it sets no commit"},
		// The runs and the checks that CI refused never move a state toward true: a run of a leg that
		// ended with no result reads unknown (R9), and so does a check that CI refused (R48). A caller
		// that gives none of them would read as one in whose history no job ran and CI refused no
		// check, so it gives each, empty for none. A run that has not ended is a run of its job too.
		{name: "control: a job run that has not ended", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: false}}
		}},
		// R13: a query "reads the graph at one transaction", and the runner knows the one that it
		// tried to read, the subject's: a run that could not run names it too.
		{name: "a run of a check of the graph that could not run and names no transaction", add: []Check{checkStateGraph}, setup: func(s *checkCase) {
			s.Subject = Point{Commit: "c1", Deploy: "d1", GraphTx: "tx1"}
			s.Results = append(s.Results, Result{Check: checkStateGraph.ID, Kind: ResultRun, Outcome: OutcomeCouldNotRun, At: checkStateRun})
		}, want: "specgraph: result 3: it sets no graph_tx, the subject that a check of the graph reads"},
		{name: "no list of job runs", change: func(in *Input) { in.JobRuns = nil },
			want: "specgraph: it sets no JobRuns; an Input at a commit where no job ran sets an empty list"},
		{name: "no map of the checks that CI refused", change: func(in *Input) { in.Uncited = nil },
			want: "specgraph: it sets no Uncited; an Input in whose history CI refused no check sets an empty map"},
		// R48: a refusal of CI names the rule that refused the check, and a commit with no refusal
		// has an empty map, or no key: a map that is nil, or a rule "", names nothing.
		{name: "control: a commit of Uncited with an empty map", change: func(in *Input) { in.Uncited = map[string]map[string]string{"c2": {}} }},
		{name: "a commit of Uncited with no map", change: func(in *Input) { in.Uncited = map[string]map[string]string{"c2": nil} },
			want: `specgraph: uncited: commit "c2": it sets no map of the checks that CI refused there, and a commit where CI refused no check has an empty map or no key`},
		{name: "a rule of Uncited that is empty", change: func(in *Input) {
			in.Uncited = map[string]map[string]string{"c2": {checkStateCheck.ID: ""}}
		}, want: `specgraph: uncited: commit "c2": check "a-restart-loses-no-message": its rule is empty, and a refusal of CI names the rule that refused the check`},
		// A name that the library takes, as a value or as the key of a map, has a letter or a digit
		// (nameFault), by the rule that the reader of the files reads it with: one of none names
		// nothing, so its record would count nowhere, or under a name that no other record reads.
		{name: "a commit of Uncited that is empty", change: func(in *Input) {
			in.Uncited = map[string]map[string]string{"": {checkStateCheck.ID: "test-not-in-code"}}
		}, want: `specgraph: uncited: commit "": a commit of no letter and no digit names no commit`},
		{name: "a commit of Uncited of no letter and no digit", change: func(in *Input) {
			in.Uncited = map[string]map[string]string{" ": {checkStateCheck.ID: "test-not-in-code"}}
		}, want: `specgraph: uncited: commit " ": a commit of no letter and no digit names no commit`},
		{name: "a check of Uncited that is empty", change: func(in *Input) {
			in.Uncited = map[string]map[string]string{"c1": {"": "test-not-in-code"}}
		}, want: `specgraph: uncited: commit "c1": check "": a check of no letter and no digit names no check`},
		{name: "a subject whose commit names nothing", change: func(in *Input) { in.Subject.Commit = " " },
			want: "specgraph: subject: it sets a commit of no letter and no digit, which names no commit"},
		{name: "a subject whose branch names nothing", change: func(in *Input) { in.Subject.Branch = "\u200b" },
			want: "specgraph: subject: it sets a branch of no letter and no digit, which names no branch"},
		{name: "a run whose job names nothing", change: func(in *Input) { in.Results[0].Job = " " },
			want: "specgraph: result 1: it sets a job of no letter and no digit, which names no job"},
		{name: "a failing example whose commit names nothing", change: func(in *Input) { in.Results[1].Commit = " " },
			want: "specgraph: result 2: it sets a commit of no letter and no digit, which names no commit"},
		{name: "a failing example whose planted_on names nothing", change: func(in *Input) { in.Results[1].PlantedOn = " " },
			want: "specgraph: result 2: it sets a planted_on of no letter and no digit, which names no commit"},
		{name: "a failing example whose fault path is no repository path", change: func(in *Input) { in.Results[1].FaultPaths = []string{"packages//envoy/main.go"} },
			want: `specgraph: result 2: it sets the fault path "packages//envoy/main.go", which is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a run whose leg names nothing", change: func(in *Input) { in.Results[0].Leg = " " },
			want: "specgraph: result 1: it sets a leg of no letter and no digit, which names no leg"},
		{name: "a run of a check of production whose deploy names nothing", add: []Check{checkStateProduction}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultRun,
				Outcome: OutcomeFail, Deploy: " ", Leg: "", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets a deploy of no letter and no digit, which names no deploy"},
		{name: "a run of a program check of production whose test_commit names nothing", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
				Deploy: "d1", Job: q.Test.Job, Leg: "", TestCommit: " ", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets a test_commit of no letter and no digit, which names no commit"},
		{name: "a job run whose job names nothing", change: func(in *Input) { in.JobRuns = []JobRun{{Job: " ", Leg: "", Commit: "c1", Ended: true}} },
			want: "specgraph: job run 1: it sets a job of no letter and no digit, which names no job"},
		{name: "a job run whose leg names nothing", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: " ", Commit: "c1", Ended: true}}
		},
			want: "specgraph: job run 1: it sets a leg of no letter and no digit, which names no leg"},
		{name: "a job run whose commit names nothing", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: " ", Ended: true}}
		},
			want: "specgraph: job run 1: it sets a commit of no letter and no digit, which names no commit"},
		// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of
		// another." "" is the leg of no matrix, and one job at one commit has legs or it has none: its
		// runs and its run results there in both forms say two things that cannot both be true, and an
		// older run result of the leg "" would answer a run of no matrix where the job had legs, at
		// the commit of that result. A state reads no leg of a failing example, and the runs of one job
		// at two commits can differ.
		{name: `job runs of one job at one commit in the leg "" and in a named leg`, change: func(in *Input) {
			in.Results[0].Leg = "linux"
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "", Commit: "c1", Ended: true}}
		}, want: `specgraph: job run 2: it sets the leg "", and job run 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `job runs of one job at one commit in a named leg and in the leg ""`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "arm", Commit: "c1", Ended: false}}
		}, want: `specgraph: job run 2: it sets the leg "arm", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result in the leg "" where the runs of its job at its commit name legs`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}}
		}, want: `specgraph: result 1: it sets the leg "", and job run 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result in a named leg where the runs of its job at its commit are of the leg ""`, change: func(in *Input) {
			in.Results[0].Leg = "linux"
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}}
		}, want: `specgraph: result 1: it sets the leg "linux", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `control: a failing example in the leg "" where the runs of its job at its commit name legs`, change: func(in *Input) {
			in.Results[0].Leg = "linux"
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}}
		}},
		{name: `control: runs of one job in the leg "" at c1 and in a named leg at c2`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "linux", Commit: "c2", Ended: true}}
		}},
		{name: `run results of one job at a commit with no run of it, in a named leg and in the leg ""`, change: func(in *Input) {
			in.Results[0].Leg = "linux"
			none := in.Results[0]
			none.Leg, none.Seq = "", 3
			in.Results = append(in.Results, none)
		}, want: `specgraph: result 3: it sets the leg "", and result 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		// The commit of a run result is decided by the text of its check that ran it
		// (countsAtACommit): a run result of a program check of production or of the graph is a record
		// of its job at its test_commit, where its test ran, one of an agent or a query of the code is
		// a record of its job at its commit, as one of a program check of the code is, and one of a
		// query or an agent of production or of the graph is a record of its job at no commit,
		// whatever test_commit it names, since such a check reads no commit and no state reads a leg
		// of it. Of a result whose text the entry does not hold, it reads the commit that the result
		// names and no test_commit, since that text alone could make the test_commit the commit where
		// its job ran.
		{name: `a run result of a program check of production in a named leg where the runs of its job at its test_commit are of the leg ""`,
			add: []Check{checkStateProductionProgram}, change: func(in *Input) {
				q := checkStateProductionProgram
				in.JobRuns = []JobRun{{Job: q.Test.Job, Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Deploy: "d1", Job: q.Test.Job, Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}, want: `specgraph: result 3: it sets the leg "linux", and job run 1 of its job "production-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result of a program check of the graph in a named leg where the runs of its job at its test_commit are of the leg ""`,
			add: []Check{checkStateGraphProgram}, change: func(in *Input) {
				g := checkStateGraphProgram
				in.JobRuns = []JobRun{{Job: g.Test.Job, Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: g.ID, CheckDigest: g.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					GraphTx: "tx1", Job: g.Test.Job, Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}, want: `specgraph: result 3: it sets the leg "linux", and job run 1 of its job "graph-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result of an agent of the code in a named leg where the runs of its job at its commit are of the leg ""`,
			add: []Check{checkStateCodeAgent}, change: func(in *Input) {
				a := checkStateCodeAgent
				in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: a.ID, CheckDigest: a.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Commit: "c1", Job: "go-checks", Leg: "linux", At: checkStateRun, Seq: 3})
			}, want: `specgraph: result 3: it sets the leg "linux", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result of a query of the code in a named leg where the runs of its job at its commit are of the leg ""`,
			add: []Check{checkStateCodeQuery}, change: func(in *Input) {
				q := checkStateCodeQuery
				in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Commit: "c1", Job: "go-checks", Leg: "linux", At: checkStateRun, Seq: 3})
			}, want: `specgraph: result 3: it sets the leg "linux", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `control: a run result of a query of production that names that job and test_commit c1, in a named leg`,
			add: []Check{checkStateProduction}, change: func(in *Input) {
				in.JobRuns = []JobRun{{Job: "production-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Deploy: "d1", Job: "production-checks", Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}},
		{name: `control: the same run result of a query of production with no test_commit`,
			add: []Check{checkStateProduction}, change: func(in *Input) {
				in.JobRuns = []JobRun{{Job: "production-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Deploy: "d1", Job: "production-checks", Leg: "linux", At: checkStateRun, Seq: 3})
			}},
		{name: `control: a run result of an agent of production that names that job and test_commit c1, in a named leg`,
			add: []Check{checkStateProductionAgent}, change: func(in *Input) {
				a := checkStateProductionAgent
				in.JobRuns = []JobRun{{Job: "production-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: a.ID, CheckDigest: a.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					Deploy: "d1", Job: "production-checks", Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}},
		{name: `control: a run result of a query of the graph that names that job and test_commit c1, in a named leg`,
			add: []Check{checkStateGraph}, change: func(in *Input) {
				in.JobRuns = []JobRun{{Job: "production-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: checkStateGraph.ID, CheckDigest: checkStateGraph.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					GraphTx: "tx1", Job: "production-checks", Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}},
		{name: `control: a run result of an agent of the graph that names that job and test_commit c1, in a named leg`,
			add: []Check{checkStateGraphAgent}, change: func(in *Input) {
				a := checkStateGraphAgent
				in.JobRuns = []JobRun{{Job: "production-checks", Leg: "", Commit: "c1", Ended: true}}
				in.Results = append(in.Results, Result{Check: a.ID, CheckDigest: a.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
					GraphTx: "tx1", Job: "production-checks", Leg: "linux", TestCommit: "c1", At: checkStateRun, Seq: 3})
			}},
		{name: `control: run results of an older text of a program check of production at one test_commit, in the leg "" and in a named leg`,
			add: []Check{checkStateProductionProgram}, change: func(in *Input) {
				q := checkStateProductionProgram
				for i, leg := range []string{"", "linux"} {
					in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
						Deploy: "d1", Job: q.Test.Job, Leg: leg, TestCommit: "c1", At: checkStateRun, Seq: int64(3 + i)})
				}
			}},
		{name: `a run result of an older text of a check of the code in a named leg where the runs of its job at its commit are of the leg ""`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: true}}
			in.Results = append(in.Results, Result{Check: checkStateCheck.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
				Commit: "c1", Job: "go-checks", Leg: "linux", At: checkStateRun, Seq: 3})
		}, want: `specgraph: result 3: it sets the leg "linux", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		// A run result that names no job, or no commit of its job's run, is a record of no job at one
		// commit: the rule reads none of them. Each is of an older text, so no rule of its text asks a
		// job or a commit of it.
		{name: `control: run results of no job at one commit, in the leg "" and in a named leg`, change: func(in *Input) {
			for i, leg := range []string{"", "linux"} {
				in.Results = append(in.Results, Result{Check: checkStateCheck.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
					Commit: "c1", Leg: leg, At: checkStateRun, Seq: int64(3 + i)})
			}
		}},
		{name: `control: run results of one job at no commit, in the leg "" and in a named leg`, change: func(in *Input) {
			for i, leg := range []string{"", "linux"} {
				in.Results = append(in.Results, Result{Check: checkStateCheck.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
					Job: "go-checks", Leg: leg, At: checkStateRun, Seq: int64(3 + i)})
			}
		}},
		{name: "a commit of the history whose id names nothing", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: " ", Files: map[string]string{"go.work": "1"}}}}
		}, want: `specgraph: history: commit " ": its id has no letter and no digit, so it names no commit`},
		{name: "a commit of the history whose parent names nothing", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Parent: " ", Files: map[string]string{"go.work": "1"}}}}
		}, want: `specgraph: history: commit "c1": it sets a parent of no letter and no digit, which names no commit`},
		// A member whose zero is a value is a plain value, and a caller that leaves it zero gives that
		// value, which the entry takes: "" is the leg of no matrix, false a run that has not ended, a
		// node that the approved spec keeps, and a statement whose checks decide all of it. The run of
		// the case is of the leg "" (checkRun). A file that leaves one of them out is refused by its
		// reader (TestScenarioRefusals, TestAnApprovedNodeSetsRemovedAndPartlyChecked).
		{name: `control: a job run that sets no leg and no ended, a run of no matrix that has not ended`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Commit: "c1"}}
		}},
		{name: "control: each approved node built with no removed and no partly_checked, kept and decided by all of its checks", change: func(in *Input) {
			for id, n := range in.Approved.Nodes {
				in.Approved.Nodes[id] = ApprovedNode{ID: n.ID, Versions: n.Versions}
			}
		}},
		// M1: a path of a tree of the history is a repository path, by the rule of NewTie
		// (isRepoPath): no tie covers "./go.work", so a failing example would count across a change there.
		{name: "a path of the history that starts with ./", change: func(in *Input) { in.History = historyWithPath("./go.work") },
			want: `specgraph: history: commit "c1": the path "./go.work" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a path of the history that starts with /", change: func(in *Input) { in.History = historyWithPath("/go.work") },
			want: `specgraph: history: commit "c1": the path "/go.work" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a path of the history that ends with /", change: func(in *Input) { in.History = historyWithPath("go.work/") },
			want: `specgraph: history: commit "c1": the path "go.work/" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a path of the history with a doubled /", change: func(in *Input) { in.History = historyWithPath("packages//envoy/go.mod") },
			want: `specgraph: history: commit "c1": the path "packages//envoy/go.mod" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a path of the history that is empty", change: func(in *Input) { in.History = historyWithPath("") },
			want: `specgraph: history: commit "c1": the path "" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "a path of the history with a part ..", change: func(in *Input) { in.History = historyWithPath("packages/../go.work") },
			want: `specgraph: history: commit "c1": the path "packages/../go.work" is no repository path, whose parts "/" separates and none of which is empty, "." or ".."`},
		{name: "control: a path of the history with a blank, a * and a \\", change: func(in *Input) { in.History = historyWithPath(`docs/a b*\.md`) }},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			for _, c := range row.add {
				s.add(c)
			}
			if row.setup != nil {
				row.setup(s)
			}
			in := s.input()
			if row.change != nil {
				row.change(&in)
			}
			state, err := Derive(in)
			_, checks, checksErr := deriveChecks(in)
			if row.want == "" {
				if err != nil || checksErr != nil || len(state.Statements) == 0 || len(checks) == 0 {
					t.Fatalf("Derive: %d statements, error %v; deriveChecks: %d checks, error %v; want states and no error",
						len(state.Statements), err, len(checks), checksErr)
				}
				return
			}
			if err == nil || err.Error() != row.want {
				t.Errorf("Derive: error %v, state %+v\nwant the error %q", err, state, row.want)
			}
			if checksErr == nil || checksErr.Error() != row.want {
				t.Errorf("deriveChecks: error %v, states %+v\nwant the error %q", checksErr, checks, row.want)
			}
			if state.Checks != nil || state.Statements != nil || checks != nil {
				t.Errorf("with the error, Derive gave %+v and deriveChecks %+v; want no state", state, checks)
			}
		})
	}
}

// approveText makes the leaf of TestCheckState, with change, the one approved text of its node in
// in, as the approval of its base wrote it.
func approveText(in *Input, change func(s *Statement)) {
	leaf := checkStateLeaf
	change(&leaf)
	block, err := leaf.Print()
	if err != nil {
		panic(err)
	}
	n := in.Approved.Nodes[leaf.ID]
	n.Versions = []ApprovedVersion{{Version: 1, At: checkStateApproved, Block: block}}
	in.Approved.Nodes[leaf.ID] = n
}

// The entry of the derivation is the one guard of a caller that builds an Approved itself, as the
// pilot does from the store (Current's doc): no reader is on that path. It refuses, in the words of
// the reader, each approved spec that no approval writes, which would make a statement read met
// where the approved text holds a check that is false. Each row: wait-cycle's Input at m1, where
// envoy-delivers-messages reads not met, with the approved blocks that the row changes; the
// controls read a state, and each other row gives the error and no state, from Derive and from
// deriveChecks alike. R22: "Dispatch refuses the approval of a change request that puts a statement
// under a parent that the approved spec does not hold"; R6: a check's claim is "narrower than its
// statement", so a check serves one statement; R5: "The parent and support relations taken
// together form no cycle, and Dispatch refuses a change that makes one." The derivation has no loop
// guard of its own, so this refusal is the only one: a loop of parents, a loop of supporters and a
// loop of both are rows. A change of the approved spec is a new version of a node, so the loop
// rule reads the newest text of each node: a loop that version 2 of a node of two versions makes
// is refused, and one that its version 1 alone made is
// taken (the control). The kind rule reads the newest text of each node that the approved spec
// keeps, the text that the derivation reads, too. R15: "An interface is a component marked as an
// interface", so envoy-to-dispatch, a component at version 1, is an interface at version 2: a
// component whose version 1 or whose removed text is under it is taken (the controls), and one
// whose current text is under it is refused; an older text that names no node is refused, since
// Dispatch keeps each text that the approved spec held (R2).
func TestTheEntryRefusesARelationOfNoNodeOfItsKindAndALoopOfTheApprovedSpec(t *testing.T) {
	t.Parallel()
	const survives, late = "a-message-survives-a-restart", "a-message-reaches-a-late-session"
	parentOf := func(id, parent string) func(nodes map[string]ApprovedNode) {
		return func(nodes map[string]ApprovedNode) {
			n := nodes[id]
			v := n.Versions[0]
			v.Block = strings.Replace(v.Block, `parent="envoy-delivers-messages"`, `parent="`+parent+`"`, 1)
			n.Versions = []ApprovedVersion{v}
			nodes[id] = n
		}
	}
	// servesOf makes id, a statement under envoy-delivers-messages, a supporter of supported.
	servesOf := func(id, supported string) func(nodes map[string]ApprovedNode) {
		return func(nodes map[string]ApprovedNode) {
			n := nodes[id]
			v := n.Versions[0]
			v.Block = strings.Replace(v.Block, `parent="envoy-delivers-messages"}`, `parent="envoy-delivers-messages" serves="`+supported+`"}`, 1)
			n.Versions = []ApprovedVersion{v}
			nodes[id] = n
		}
	}
	// twoVersions gives id two approved versions at the time of its one version, the first with the
	// parent first and the second with the parent second, where "" keeps the block as written.
	twoVersions := func(id, first, second string) func(nodes map[string]ApprovedNode) {
		return func(nodes map[string]ApprovedNode) {
			n := nodes[id]
			version := func(number int, parent string) ApprovedVersion {
				v := n.Versions[0]
				v.Version = number
				if parent != "" {
					v.Block = strings.Replace(v.Block, `parent="envoy-delivers-messages"`, `parent="`+parent+`"`, 1)
				}
				return v
			}
			n.Versions = []ApprovedVersion{version(1, first), version(2, second)}
			nodes[id] = n
		}
	}
	// components adds dispatch and envoy, two components, envoy-to-dispatch, a component at version 1
	// and the interface between them at version 2, and envoy-delivery, a component under first at
	// version 1, and under second at version 2 where second is not "", removed where it is; each
	// version approved a day after the one before, from the approval of envoy-delivers-messages.
	components := func(first, second string) func(nodes map[string]ApprovedNode) {
		return func(nodes map[string]ApprovedNode) {
			at := nodes["envoy-delivers-messages"].Versions[0].At
			node := func(texts ...Component) ApprovedNode {
				n := ApprovedNode{ID: texts[0].ID, Removed: false, PartlyChecked: false}
				for i, c := range texts {
					block, err := c.Print()
					if err != nil {
						panic(err)
					}
					n.Versions = append(n.Versions, ApprovedVersion{Version: i + 1, At: at.AddDate(0, 0, i), Block: block})
				}
				return n
			}
			component := func(id, parent string, joins ...string) Component {
				return Component{ID: id, Name: "Envoy", Purpose: "It delivers each stored message.", Parent: parent, Interface: len(joins) > 0, Joins: joins}
			}
			for _, n := range []ApprovedNode{node(component("dispatch", "")), node(component("envoy", "")),
				node(component("envoy-to-dispatch", ""), component("envoy-to-dispatch", "", "dispatch", "envoy"))} {
				nodes[n.ID] = n
			}
			delivery := node(component("envoy-delivery", first))
			if second == "" {
				delivery.Removed = true
			} else {
				delivery = node(component("envoy-delivery", first), component("envoy-delivery", second))
			}
			nodes[delivery.ID] = delivery
		}
	}
	for _, row := range []struct {
		name    string
		changes []func(nodes map[string]ApprovedNode)
		want    string // the error; "" for a control
	}{
		{name: "control: wait-cycle as written"},
		{name: "control: the parent a statement that the approved spec removed", changes: []func(map[string]ApprovedNode){parentOf(survives, "old-parent")}},
		{name: "the parent no node", changes: []func(map[string]ApprovedNode){parentOf(survives, "no-such-statement")},
			want: `specgraph: approved node "a-message-survives-a-restart" version 1: bad-approved-block: parent "no-such-statement" names no node of the approved spec (line 1 of its block)`},
		{name: "the parent the check of the statement", changes: []func(map[string]ApprovedNode){parentOf(survives, "a-restart-keeps-the-message")},
			want: `specgraph: approved node "a-message-survives-a-restart" version 1: bad-approved-block: parent "a-restart-keeps-the-message" names a check, not a statement (line 1 of its block)`},
		{name: "the parent a check of another statement", changes: []func(map[string]ApprovedNode){parentOf(survives, "a-late-session-gets-the-message")},
			want: `specgraph: approved node "a-message-survives-a-restart" version 1: bad-approved-block: parent "a-late-session-gets-the-message" names a check, not a statement (line 1 of its block)`},
		{name: "the failing check with its serves left out", changes: []func(map[string]ApprovedNode){func(nodes map[string]ApprovedNode) {
			n := nodes["a-restart-keeps-the-message"]
			v := n.Versions[0]
			v.Block = strings.Replace(v.Block, ` serves="a-message-survives-a-restart"`, "", 1)
			n.Versions = []ApprovedVersion{v}
			nodes[n.ID] = n
		}}, want: `specgraph: approved node "a-restart-keeps-the-message" version 1: bad-approved-block: serves "" names no node of the approved spec (line 1 of its block)`},
		{name: "a statement its own parent", changes: []func(map[string]ApprovedNode){parentOf(survives, survives)},
			want: "specgraph: approved spec: loop: a-message-survives-a-restart -[parent]-> a-message-survives-a-restart"},
		{name: "version 2 of a statement of two versions makes it its own parent", changes: []func(map[string]ApprovedNode){twoVersions(survives, "", survives)},
			want: "specgraph: approved spec: loop: a-message-survives-a-restart -[parent]-> a-message-survives-a-restart"},
		{name: "control: version 1 of a statement of two versions made it its own parent, and version 2 is as written",
			changes: []func(map[string]ApprovedNode){twoVersions(survives, survives, "")}},
		{name: "control: version 1 of a component under a component that is an interface at its version 2, and version 2 under envoy",
			changes: []func(map[string]ApprovedNode){components("envoy-to-dispatch", "envoy")}},
		{name: "control: a removed component under a component that is an interface at its version 2",
			changes: []func(map[string]ApprovedNode){components("envoy-to-dispatch", "")}},
		{name: "version 2 of a component, its current text, under a component that is an interface at its version 2",
			changes: []func(map[string]ApprovedNode){components("envoy", "envoy-to-dispatch")},
			want:    `specgraph: approved node "envoy-delivery" version 2: bad-approved-block: parent "envoy-to-dispatch" names an interface, not a component (line 1 of its block)`},
		{name: "a removed component under no node", changes: []func(map[string]ApprovedNode){components("no-such-component", "")},
			want: `specgraph: approved node "envoy-delivery" version 1: bad-approved-block: parent "no-such-component" names no node of the approved spec (line 1 of its block)`},
		{name: "version 1 of a statement of two versions under no node, and version 2 as written",
			changes: []func(map[string]ApprovedNode){twoVersions(survives, "no-such-statement", "")},
			want:    `specgraph: approved node "a-message-survives-a-restart" version 1: bad-approved-block: parent "no-such-statement" names no node of the approved spec (line 1 of its block)`},
		{name: "two statements each the parent of the other", changes: []func(map[string]ApprovedNode){parentOf(survives, late), parentOf(late, survives)},
			want: "specgraph: approved spec: loop: a-message-reaches-a-late-session -[parent]-> a-message-survives-a-restart -[parent]-> a-message-reaches-a-late-session"},
		{name: "a statement that serves the statement under it", changes: []func(map[string]ApprovedNode){func(nodes map[string]ApprovedNode) {
			n := nodes["envoy-delivers-messages"]
			v := n.Versions[0]
			v.Block = strings.Replace(v.Block, `kind="behavior"}`, `kind="behavior" serves="`+survives+`"}`, 1)
			n.Versions = []ApprovedVersion{v}
			nodes[n.ID] = n
		}}, want: "specgraph: approved spec: loop: a-message-survives-a-restart -[parent]-> envoy-delivers-messages -[serves]-> a-message-survives-a-restart"},
		{name: "a statement that serves itself", changes: []func(map[string]ApprovedNode){servesOf(survives, survives)},
			want: "specgraph: approved spec: loop: a-message-survives-a-restart -[serves]-> a-message-survives-a-restart"},
		{name: "two statements each a supporter of the other", changes: []func(map[string]ApprovedNode){servesOf(survives, late), servesOf(late, survives)},
			want: "specgraph: approved spec: loop: a-message-reaches-a-late-session -[serves]-> a-message-survives-a-restart -[serves]-> a-message-reaches-a-late-session"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s, err := ReadScenario(scenarioFS(t, filepath.Join("testdata", "scenarios", "wait-cycle")))
			if err != nil {
				t.Fatal(err)
			}
			in := s.Input(0)
			nodes := map[string]ApprovedNode{"old-parent": {ID: "old-parent", Removed: true, PartlyChecked: false, Versions: []ApprovedVersion{{
				Version: 1, At: in.Approved.Nodes["envoy-delivers-messages"].Versions[0].At,
				Block: ":::requirement{#old-parent kind=\"behavior\"}\nEnvoy keeps each message.\n:::\n"}}}}
			for id, n := range in.Approved.Nodes {
				nodes[id] = n
			}
			for _, change := range row.changes {
				change(nodes)
			}
			in.Approved = Approved{Nodes: nodes}
			state, err := Derive(in)
			_, checks, checksErr := deriveChecks(in)
			above := ""
			for _, st := range state.Statements {
				if st.ID == "envoy-delivers-messages" {
					above = st.State
				}
			}
			switch {
			case row.want == "" && (err != nil || checksErr != nil || above == ""):
				t.Errorf("Derive: error %v, envoy-delivers-messages %q; deriveChecks: error %v; want a state", err, above, checksErr)
			case row.want != "" && (fmt.Sprint(err) != row.want || fmt.Sprint(checksErr) != row.want || state.Statements != nil || checks != nil):
				t.Errorf("Derive: error %v, envoy-delivers-messages %q; deriveChecks: error %v, %d checks\nwant the error %q and no state", err, above, checksErr, len(checks), row.want)
			}
		})
	}
}

// derive is Derive(in), and fails the test when Derive refuses in.
func derive(t *testing.T, in Input) State {
	t.Helper()
	state, err := Derive(in)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	return state
}

// checkStates is the states that deriveChecks gives for in, and fails the test when deriveChecks
// refuses in.
func checkStates(t *testing.T, in Input) []CheckState {
	t.Helper()
	_, states, err := deriveChecks(in)
	if err != nil {
		t.Fatalf("deriveChecks: %v", err)
	}
	return states
}

// historyOfTheCaller is a History of a type that a caller of the library defines.
type historyOfTheCaller func(from, to string) ([]string, bool, error)

func (h historyOfTheCaller) Changed(from, to string) ([]string, bool, error) { return h(from, to) }

// historyWithPath is a history of the one commit c1, whose tree holds go.work and path.
func historyWithPath(path string) *FixtureHistory {
	return &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{"go.work": "1", path: "1"}}}}
}

// A refusal names one fault in each run, whatever the order in which Go gives the keys of a map: of
// two faults of one file, the least place, by commit and then by path or by check. Each scenario is
// read 20 times, since each range over a map gives its keys in another order.
func TestARefusalNamesTheLeastPlaceOfTwoFaults(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ name, file, content, want string }{
		{"two empty content labels in one tree of the history", "history.json",
			`{"commits":[{"id":"c1","files":{"b.go":"","a.go":"","go.work":"1"}}]}`,
			`history.json: specgraph: history: commit "c1": the content label of "a.go" has no letter and no digit, so it names no content`},
		{"two empty rules of uncited.json at two commits", "uncited.json",
			`{"c2":{"a-restart-loses-no-message":""},"c1":{"a-restart-loses-no-message":""}}`,
			`uncited.json: commit "c1": check "a-restart-loses-no-message": its rule is empty, and a refusal of CI names the rule that refused the check`},
		{"a commit of uncited.json that names nothing, and a check of it that names nothing", "uncited.json",
			`{" ":{"":"test-not-in-code"}}`,
			`uncited.json: commit " ": a commit of no letter and no digit names no commit`},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fsys := scenarioFS(t, filepath.Join("testdata", "scenarios", "carry"))
			fsys[row.file] = &fstest.MapFile{Data: []byte(row.content)}
			for run := range 20 {
				if _, err := ReadScenario(fsys); err == nil || err.Error() != row.want {
					t.Fatalf("read %d: %v, want %q", run+1, err, row.want)
				}
			}
		})
	}
}
