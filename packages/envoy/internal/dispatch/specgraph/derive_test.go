package specgraph

import (
	"path/filepath"
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
// subject of R10, so Derive and CheckStates give an error and no state, in the words that the
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
				Outcome: OutcomeFail, Leg: new(""), At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no deploy, the subject that a check of production reads"},
		{name: "a run of a check of the graph with no graph_tx", add: []Check{checkStateGraph}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateGraph.ID, CheckDigest: checkStateGraph.Digest(), Kind: ResultRun,
				Outcome: OutcomeFail, Deploy: "d1", Leg: new(""), At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no graph_tx, the subject that a check of the graph reads"},
		{name: "control: a failing example of a query of production names no subject", add: []Check{checkStateProduction}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultFailingExample,
				Outcome: OutcomeFail, Leg: new(""), At: checkStateRun, Seq: 3})
		}},
		{name: "a run of a program check of production with no test_commit", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
				Deploy: "d1", Job: q.Test.Job, Leg: new(""), At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets no test_commit, the commit of the test that ran"},
		{name: "a failing example of a program check of production with no test_commit", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultFailingExample, Outcome: OutcomePass,
				Leg: new(""), At: checkStateRun, Seq: 3, FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
		}, want: "specgraph: result 3: it sets no test_commit, the commit of the test that ran"},
		{name: "TestCheckState, item 19: the failing example of a program check of production names no commit of its test",
			setup: func(s *checkCase) { s.productionProgram("docs/x.md", "") },
			want:  "specgraph: result 4: it sets no test_commit, the commit of the test that ran"},
		{name: "TestARunOfProductionOrTheGraphCountsForTheCodeThatRanIt: production, the pass names no commit of its test",
			setup: func(s *checkCase) {
				q := checkStateProductionProgram
				s.add(q)
				s.tieLikeC(q.ID)
				s.commit("c2", "c1", "docs/x.md")
				s.Subject = Point{Commit: "c2", Deploy: "d2", GraphTx: "tx1"}
				s.Results = append(s.Results, Result{Check: q.ID, Kind: ResultRun, Outcome: OutcomePass, Deploy: "d2", Job: q.Test.Job, At: checkStateRun.Add(time.Hour)},
					Result{Check: q.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, TestCommit: "c2",
						At: checkStateRun.Add(65 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
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
			n.Versions = []ApprovedVersion{{Version: 1, Document: n.Versions[0].Document}}
			in.Approved.Nodes[checkStateLeaf.ID] = n
		}, want: `specgraph: approved node "a-stored-message-survives-a-restart" version 1: it has no time`},
		{name: "an approved node with no version", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.Versions = nil
			in.Approved.Nodes[checkStateCheck.ID] = n
		}, want: `specgraph: approved node "a-restart-loses-no-message": it has no version`},
		{name: "an approved text with no document", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.Versions = []ApprovedVersion{{Version: 1, At: checkStateApproved}}
			in.Approved.Nodes[checkStateCheck.ID] = n
		}, want: `specgraph: approved node "a-restart-loses-no-message" version 1: no-node: a spec file holds one or more nodes (line 1 of its document)`},
		{name: "a commit of the history with no files", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1"}}}
		}, want: `specgraph: history: commit "c1": it sets no files`},
		// A tree with no path, or a content label with no character, would read as a tree, or a
		// content, that other commits share, so a result would carry between them (R8).
		{name: "a commit of the history whose tree holds no path", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{}}}}
		}, want: `specgraph: history: commit "c1": its files hold no path, and the tree of a commit holds at least one file`},
		{name: "a content label of the history that is empty", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{"go.work": ""}}}}
		}, want: `specgraph: history: commit "c1": the content label of "go.work" is empty, and a label has at least one character`},
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
		{name: "a job run with no job", change: func(in *Input) { in.JobRuns = []JobRun{{Leg: new(""), Commit: "c1", Ended: new(true)}} },
			want: "specgraph: job run 1: it sets no job"},
		{name: "a job run with no commit", change: func(in *Input) { in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Ended: new(true)}} },
			want: "specgraph: job run 1: it sets no commit"},
		// The runs and the checks that CI refused only lower a state: a run of a leg that ended with
		// no result reads unknown (R9), and so does a check that CI refused (R48). A caller that gives
		// none of them would read as one in whose history no job ran and CI refused no check, so it
		// gives each, empty for none. A run that has not ended is a run of its job too.
		{name: "control: a job run that has not ended", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c1", Ended: new(false)}}
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
		{name: "a run whose leg names nothing", change: func(in *Input) { in.Results[0].Leg = new(" ") },
			want: "specgraph: result 1: it sets a leg of no letter and no digit, which names no leg"},
		{name: "a run of a check of production whose deploy names nothing", add: []Check{checkStateProduction}, change: func(in *Input) {
			in.Results = append(in.Results, Result{Check: checkStateProduction.ID, CheckDigest: checkStateProduction.Digest(), Kind: ResultRun,
				Outcome: OutcomeFail, Deploy: " ", Leg: new(""), At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets a deploy of no letter and no digit, which names no deploy"},
		{name: "a run of a program check of production whose test_commit names nothing", add: []Check{checkStateProductionProgram}, change: func(in *Input) {
			q := checkStateProductionProgram
			in.Results = append(in.Results, Result{Check: q.ID, CheckDigest: q.Digest(), Kind: ResultRun, Outcome: OutcomeFail,
				Deploy: "d1", Job: q.Test.Job, Leg: new(""), TestCommit: " ", At: checkStateRun, Seq: 3})
		}, want: "specgraph: result 3: it sets a test_commit of no letter and no digit, which names no commit"},
		{name: "a job run whose job names nothing", change: func(in *Input) { in.JobRuns = []JobRun{{Job: " ", Leg: new(""), Commit: "c1", Ended: new(true)}} },
			want: "specgraph: job run 1: it sets a job of no letter and no digit, which names no job"},
		{name: "a job run whose leg names nothing", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(" "), Commit: "c1", Ended: new(true)}}
		},
			want: "specgraph: job run 1: it sets a leg of no letter and no digit, which names no leg"},
		{name: "a job run whose commit names nothing", change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: " ", Ended: new(true)}}
		},
			want: "specgraph: job run 1: it sets a commit of no letter and no digit, which names no commit"},
		// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of
		// another." "" is the leg of no matrix, and one job at one commit has legs or it has none: its
		// runs and its run results there in both forms say two things that cannot both be true, and an
		// older run result of the leg "" would answer a run of no matrix where the job had legs, at a
		// commit where the results of that commit count (TestARunResultOfNoMatrixAnswersNoRunWhereItsJobHadLegs).
		// A state reads no leg of a failing example, and the runs of one job at two commits can differ.
		{name: `job runs of one job at one commit in the leg "" and in a named leg`, change: func(in *Input) {
			in.Results[0].Leg = new("linux")
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new("linux"), Commit: "c1", Ended: new(true)}, {Job: "go-checks", Leg: new(""), Commit: "c1", Ended: new(true)}}
		}, want: `specgraph: job run 2: it sets the leg "", and job run 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `job runs of one job at one commit in a named leg and in the leg ""`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c1", Ended: new(true)}, {Job: "go-checks", Leg: new("arm"), Commit: "c1", Ended: new(false)}}
		}, want: `specgraph: job run 2: it sets the leg "arm", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result in the leg "" where the runs of its job at its commit name legs`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new("linux"), Commit: "c1", Ended: new(true)}}
		}, want: `specgraph: result 1: it sets the leg "", and job run 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `a run result in a named leg where the runs of its job at its commit are of the leg ""`, change: func(in *Input) {
			in.Results[0].Leg = new("linux")
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c1", Ended: new(true)}}
		}, want: `specgraph: result 1: it sets the leg "linux", and job run 1 of its job "go-checks" at the commit "c1" sets the leg ""; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		{name: `control: a failing example in the leg "" where the runs of its job at its commit name legs`, change: func(in *Input) {
			in.Results[0].Leg = new("linux")
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new("linux"), Commit: "c1", Ended: new(true)}}
		}},
		{name: `control: runs of one job in the leg "" at c1 and in a named leg at c2`, change: func(in *Input) {
			in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c1", Ended: new(true)}, {Job: "go-checks", Leg: new("linux"), Commit: "c2", Ended: new(true)}}
		}},
		{name: `run results of one job at a commit with no run of it, in a named leg and in the leg ""`, change: func(in *Input) {
			in.Results[0].Leg = new("linux")
			none := in.Results[0]
			none.Leg, none.Seq = new(""), 3
			in.Results = append(in.Results, none)
		}, want: `specgraph: result 3: it sets the leg "", and result 1 of its job "go-checks" at the commit "c1" sets the leg "linux"; one job at one commit has legs or it has none, and "" is the leg of no matrix`},
		// A run result that names no job, or no commit of its job's run, is a record of no job at one
		// commit: the rule reads none of them. Each is of an older text, so no rule of its text asks a
		// job or a commit of it.
		{name: `control: run results of no job at one commit, in the leg "" and in a named leg`, change: func(in *Input) {
			for i, leg := range []string{"", "linux"} {
				in.Results = append(in.Results, Result{Check: checkStateCheck.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
					Commit: "c1", Leg: new(leg), At: checkStateRun, Seq: int64(3 + i)})
			}
		}},
		{name: `control: run results of one job at no commit, in the leg "" and in a named leg`, change: func(in *Input) {
			for i, leg := range []string{"", "linux"} {
				in.Results = append(in.Results, Result{Check: checkStateCheck.ID, CheckDigest: "an-older-text", Kind: ResultRun, Outcome: OutcomePass,
					Job: "go-checks", Leg: new(leg), At: checkStateRun, Seq: int64(3 + i)})
			}
		}},
		{name: "a commit of the history whose id names nothing", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: " ", Files: map[string]string{"go.work": "1"}}}}
		}, want: `specgraph: history: commit " ": its id has no letter and no digit, so it names no commit`},
		{name: "a commit of the history whose parent names nothing", change: func(in *Input) {
			in.History = &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Parent: " ", Files: map[string]string{"go.work": "1"}}}}
		}, want: `specgraph: history: commit "c1": it sets a parent of no letter and no digit, which names no commit`},
		// A member marked present that a caller does not set is refused, as a file that leaves it out
		// is, since its zero value is a value: "" is the leg of no matrix, false a run that has not
		// ended, a node that the approved spec keeps, and a statement whose checks decide all of it.
		{name: "a result that does not set its leg", change: func(in *Input) { in.Results[0].Leg = nil },
			want: `specgraph: result 1: it sets no leg; a result of no matrix sets ""`},
		{name: "a job run that does not set its leg", change: func(in *Input) { in.JobRuns = []JobRun{{Job: "go-checks", Commit: "c1", Ended: new(true)}} },
			want: `specgraph: job run 1: it sets no leg; a run of no matrix sets ""`},
		{name: "a job run that does not set ended", change: func(in *Input) { in.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c1"}} },
			want: "specgraph: job run 1: it sets no ended; a run that has not ended sets false"},
		{name: "an approved node that does not set removed", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateCheck.ID]
			n.Removed = nil
			in.Approved.Nodes[checkStateCheck.ID] = n
		}, want: `specgraph: approved node "a-restart-loses-no-message": it sets no removed; a node that the approved spec keeps sets false`},
		{name: "an approved node that does not set partly_checked", change: func(in *Input) {
			n := in.Approved.Nodes[checkStateLeaf.ID]
			n.PartlyChecked = nil
			in.Approved.Nodes[checkStateLeaf.ID] = n
		}, want: `specgraph: approved node "a-stored-message-survives-a-restart": it sets no partly_checked; a statement whose checks decide all of it sets false`},
		// M1: a path of a tree of the history is a repository path, by the rule of NewTie
		// (isRepoPath): no tie covers "./go.work", so a change there would carry a result (R8).
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
			checks, checksErr := CheckStates(in)
			if row.want == "" {
				if err != nil || checksErr != nil || len(state.Statements) == 0 || len(checks) == 0 {
					t.Fatalf("Derive: %d statements, error %v; CheckStates: %d checks, error %v; want states and no error",
						len(state.Statements), err, len(checks), checksErr)
				}
				return
			}
			if err == nil || err.Error() != row.want {
				t.Errorf("Derive: error %v, state %+v\nwant the error %q", err, state, row.want)
			}
			if checksErr == nil || checksErr.Error() != row.want {
				t.Errorf("CheckStates: error %v, states %+v\nwant the error %q", checksErr, checks, row.want)
			}
			if state.Checks != nil || state.Statements != nil || checks != nil {
				t.Errorf("with the error, Derive gave %+v and CheckStates %+v; want no state", state, checks)
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

// checkStates is CheckStates(in), and fails the test when CheckStates refuses in.
func checkStates(t *testing.T, in Input) []CheckState {
	t.Helper()
	states, err := CheckStates(in)
	if err != nil {
		t.Fatalf("CheckStates: %v", err)
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
			`history.json: specgraph: history: commit "c1": the content label of "a.go" is empty, and a label has at least one character`},
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
