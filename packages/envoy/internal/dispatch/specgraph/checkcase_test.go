package specgraph

// The base of TestCheckState and the helpers that build and change it (checkCase), which the tests
// of the package that derive a state from a checkCase share: the nodes, times and tie of the base,
// the case and its changes, and the results that the rows add.

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// The times of TestCheckState's base: the approval of each node, and the run of its check at c1.
var (
	checkStateApproved = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	checkStateRun      = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
)

// The nodes of TestCheckState: the top statement T, the leaf L under it, and L's program check C,
// which reads the code; and the checks that some rows add under L: P, a query of production, and
// A, an agent of production; G, a query of the graph, and its like that is an agent; Q, a program
// check of production, and its like of the graph; and K, an agent of the code, and its like that is
// a query.
var (
	checkStateTop = Statement{ID: "messages-reach-sessions", Kind: KindBehavior,
		Text: "Envoy delivers each message to the session that it is for."}
	checkStateLeaf = Statement{ID: "a-stored-message-survives-a-restart", Kind: KindBehavior, Parent: "messages-reach-sessions",
		Text: "Envoy delivers each stored message to its session after the listener starts again."}
	checkStateCheck = Check{ID: "a-restart-loses-no-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectCode, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestARestartLosesNoMessage", Job: "go-checks"},
		Claim:          "A message that the listener stores before a restart reaches its session after the restart.",
		ProcedureText:  "The program stores one message, starts the listener again and reads the messages of the session.\nIt must find the message one time.",
		FailingExample: "A listener that deletes its stored messages when it starts."}
	checkStateProduction = Check{ID: "production-keeps-each-stored-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectProduction, Procedure: ProcedureQuery,
		Claim:          "Each message that production stored before the last deploy reached its session.",
		ProcedureText:  "The query reads each message that production stored before the last deploy.\nIt returns each message that did not reach its session.",
		FailingExample: "A copy of the message records with one stored message that did not reach its session."}
	checkStateProductionAgent = Check{ID: "an-agent-finds-each-stored-message-delivered", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectProduction, Procedure: ProcedureAgent,
		Claim:          "Each message that production stored before the last deploy reached its session.",
		ProcedureText:  "An agent reads each message that production stored before the last deploy.\nIt names each message that did not reach its session.",
		FailingExample: "A copy of the message records with one stored message that did not reach its session."}
	checkStateGraph = Check{ID: "the-graph-holds-a-delivery-of-each-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectGraph, Procedure: ProcedureQuery,
		Claim:          "Each message record of the graph has a delivery record.",
		ProcedureText:  "The query reads each message record of the graph.\nIt returns each message record that has no delivery record.",
		FailingExample: "A copy of the graph with one message record that has no delivery record."}
	checkStateGraphAgent = Check{ID: "an-agent-finds-a-delivery-of-each-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectGraph, Procedure: ProcedureAgent,
		Claim:          "Each message record of the graph has a delivery record.",
		ProcedureText:  "An agent reads each message record of the graph.\nIt names each message record that has no delivery record.",
		FailingExample: "A copy of the graph with one message record that has no delivery record."}
	checkStateProductionProgram = Check{ID: "a-production-restart-loses-no-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectProduction, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestAProductionRestartLosesNoMessage", Job: "production-checks"},
		Claim:          "A message that production stores before a restart reaches its session after the restart.",
		ProcedureText:  "The program stores one message in production, starts the listener again and reads the messages of the session.\nIt must find the message one time.",
		FailingExample: "A listener that deletes its stored messages when it starts."}
	checkStateGraphProgram = Check{ID: "the-graph-keeps-each-delivery", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectGraph, Procedure: ProcedureProgram,
		Test:           &Test{Runner: "go", Selector: "github.com/sjawhar/envoy/cmd/listener TestTheGraphKeepsEachDelivery", Job: "graph-checks"},
		Claim:          "Each message record of the graph keeps its delivery record after a restart.",
		ProcedureText:  "The program reads each message record of the graph after a restart.\nIt must find a delivery record for each one.",
		FailingExample: "A copy of the graph with one message record that has no delivery record."}
	checkStateCodeAgent = Check{ID: "an-agent-finds-the-message-after-a-restart", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectCode, Procedure: ProcedureAgent,
		Claim:          "A message that the listener stores before a restart reaches its session after the restart.",
		ProcedureText:  "An agent stores one message, starts the listener again and reads the messages of the session.\nIt must find the message one time.",
		FailingExample: "A listener that deletes its stored messages when it starts."}
	checkStateCodeQuery = Check{ID: "a-query-finds-the-message-after-a-restart", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectCode, Procedure: ProcedureQuery,
		Claim:          "A message that the listener stores before a restart reaches its session after the restart.",
		ProcedureText:  "The query reads the messages of the session after the listener starts again.\nIt returns each stored message that it does not find.",
		FailingExample: "A listener that deletes its stored messages when it starts."}
)

// checkStateTie is the tie of C and of Q: the files that the test of C reads.
var checkStateTie = []string{
	"go.work",
	"packages/contracts/fixtures/dispatch-targeted-delivery.json",
	"packages/envoy/cmd/listener/*",
	"packages/envoy/cmd/listener/testdata/**",
	"packages/envoy/go.mod",
	"packages/envoy/go.sum",
}

// checkCase is one row's input to deriveChecks: the spec files as nodes in files, the approved spec,
// and a history that rows add commits to. input completes it.
type checkCase struct {
	t *testing.T
	Input
	check   string                // the id of C
	files   map[string][]specNode // the nodes of each spec file, in their order
	history *FixtureHistory
}

// specNode is a statement or a check of a checkCase.
type specNode interface{ Print() (string, error) }

// newCheckCase is the base of TestCheckState.
func newCheckCase(t *testing.T) *checkCase {
	t.Helper()
	s := &checkCase{t: t, check: checkStateCheck.ID, history: &FixtureHistory{Commits: []FixtureCommit{{ID: "c1", Files: map[string]string{
		"docs/x.md": "1",
		"go.work":   "1",
		"packages/contracts/fixtures/dispatch-targeted-delivery.json": "1",
		"packages/envoy/cmd/listener/main.go":                         "1",
		"packages/envoy/cmd/listener/restart_test.go":                 "1",
		"packages/envoy/cmd/listener/testdata/a.json":                 "1",
		"packages/envoy/go.mod":                                       "1",
		"packages/envoy/go.sum":                                       "1",
	}}}}}
	s.History = s.history
	s.Approved = Approved{Nodes: map[string]ApprovedNode{}}
	s.files = map[string][]specNode{"spec.md": {checkStateTop, checkStateLeaf, checkStateCheck}}
	s.approve(checkStateApproved, checkStateTop, checkStateLeaf, checkStateCheck)
	if problems := Validate(s.parse(), Options{}); len(problems) != 0 {
		t.Fatalf("the base is not a valid tree: %+v", problems)
	}
	tie, err := NewTie(checkStateTie)
	if err != nil {
		t.Fatal(err)
	}
	s.Ties = CommitTies{"c1": {s.check: tie, checkStateProductionProgram.ID: tie}}
	s.Subject = Point{Commit: "c1", Branch: "b"}
	s.Results = []Result{checkRun("c1", OutcomePass, checkStateRun), caughtFault("c1", checkStateRun.Add(5*time.Minute))}
	return s
}

// input is the Input of the case: the files parsed, each result with no digest given the digest of
// its check's current approved text and each result with no leg the leg "" of no matrix, each
// result's Seq its place in Results, Now, unless the row set it, one hour after the newest result,
// or after checkStateRun when there is none, and an empty JobRuns and Uncited, unless the row set
// them, for a case at whose commit no job ran and CI refused no check. A row that reads a result
// with no leg sets it to nil in the Input that input gives.
func (s *checkCase) input() Input {
	s.t.Helper()
	in := s.Input
	if in.JobRuns == nil {
		in.JobRuns = []JobRun{}
	}
	if in.Uncited == nil {
		in.Uncited = map[string]map[string]string{}
	}
	in.Files = s.parse()
	current, problems := in.Approved.Current()
	if len(problems) != 0 {
		s.t.Fatalf("the approved spec of the case: %+v", problems)
	}
	in.Results = slices.Clone(in.Results)
	for i := range in.Results {
		r := &in.Results[i]
		if c, ok := current.CheckByID(r.Check); ok && r.CheckDigest == "" {
			r.CheckDigest = c.Digest()
		}
		r.Seq = int64(i + 1)
		if s.Now.IsZero() && r.At.Add(time.Hour).After(in.Now) {
			in.Now = r.At.Add(time.Hour)
		}
	}
	if in.Now.IsZero() {
		in.Now = checkStateRun.Add(time.Hour)
	}
	return in
}

// parse is the tree of the case's spec files.
func (s *checkCase) parse() Tree {
	s.t.Helper()
	tree, problems := Parse(s.specFiles())
	if len(problems) != 0 {
		s.t.Fatalf("the files of the case: %+v", problems)
	}
	return tree
}

// specFiles is the text of each spec file of the case, by name: its nodes as Print writes them.
func (s *checkCase) specFiles() map[string][]byte {
	s.t.Helper()
	files := map[string][]byte{}
	for name, nodes := range s.files {
		var blocks []string
		for _, n := range nodes {
			block, err := n.Print()
			if err != nil {
				s.t.Fatal(err)
			}
			blocks = append(blocks, block)
		}
		files[name] = []byte(strings.Join(blocks, "\n"))
	}
	return files
}

// approve adds a version of each node to the approved spec, approved at at; a node that it adds
// the approved spec keeps, and its checks decide all of it.
func (s *checkCase) approve(at time.Time, nodes ...specNode) {
	s.t.Helper()
	for _, n := range nodes {
		block, err := n.Print()
		if err != nil {
			s.t.Fatal(err)
		}
		id := specNodeID(n)
		node, held := s.Approved.Nodes[id]
		if !held {
			node = ApprovedNode{ID: id}
		}
		node.Versions = append(node.Versions, ApprovedVersion{Version: len(node.Versions) + 1, At: at, Block: block})
		s.Approved.Nodes[id] = node
	}
}

// approveEarlier puts an earlier text of a node before each version that the approved spec holds
// of it, approved at at.
func (s *checkCase) approveEarlier(at time.Time, earlier specNode) {
	s.t.Helper()
	id := specNodeID(earlier)
	later := s.Approved.Nodes[id].Versions
	delete(s.Approved.Nodes, id)
	s.approve(at, earlier)
	node := s.Approved.Nodes[id]
	for _, v := range later {
		v.Version = len(node.Versions) + 1
		node.Versions = append(node.Versions, v)
	}
	s.Approved.Nodes[id] = node
}

// reapprove approves a new text of a node at at, and the files hold it in the place of the old one.
func (s *checkCase) reapprove(at time.Time, n specNode) {
	s.t.Helper()
	s.approve(at, n)
	s.swap(n)
}

// replace makes n the only approved text of its node, approved when its node was, and the files
// hold it in the place of the old one.
func (s *checkCase) replace(n specNode) {
	s.t.Helper()
	id := specNodeID(n)
	at := s.Approved.Nodes[id].Versions[0].At
	delete(s.Approved.Nodes, id)
	s.approve(at, n)
	s.swap(n)
}

// refuse puts in the files and in the approved spec C's text with change, a text that the tree
// rules refuse, and gives each result of C the job of that text, so that only the refusal keeps
// its results from counting.
func (s *checkCase) refuse(change func(c *Check)) {
	s.t.Helper()
	c := checkStateCheck
	test := *c.Test
	c.Test = &test
	change(&c)
	s.replace(c)
	job := ""
	if c.Test != nil {
		job = c.Test.Job
	}
	for i := range s.Results {
		if s.Results[i].Check == c.ID {
			s.Results[i].Job = job
		}
	}
}

// swap puts n in the files in the place of the node with its id.
func (s *checkCase) swap(n specNode) {
	for name, nodes := range s.files {
		for i, old := range nodes {
			if specNodeID(old) == specNodeID(n) {
				s.files[name] = slices.Clone(nodes)
				s.files[name][i] = n
			}
		}
	}
}

// add approves a check with the base's nodes and writes it at the end of spec.md.
func (s *checkCase) add(c Check) {
	s.t.Helper()
	s.approve(checkStateApproved, c)
	s.files["spec.md"] = append(slices.Clone(s.files["spec.md"]), c)
}

// commit adds commit id to the history, with the tree of commit from and each path of changed given
// a label that no other commit gives it, and gives id the ties of from. It returns the tree, which a
// row can change further.
func (s *checkCase) commit(id, from string, changed ...string) map[string]string {
	s.t.Helper()
	i := slices.IndexFunc(s.history.Commits, func(c FixtureCommit) bool { return c.ID == from })
	if i < 0 {
		s.t.Fatalf("the history holds no commit %s", from)
	}
	files := map[string]string{}
	for path, label := range s.history.Commits[i].Files {
		files[path] = label
	}
	for _, path := range changed {
		files[path] = id
	}
	s.history.Commits = append(s.history.Commits, FixtureCommit{ID: id, Parent: from, Files: files})
	s.Ties[id] = map[string]Tie{}
	maps.Copy(s.Ties[id], s.Ties[from])
	return files
}

// tieLikeC gives the check id the tie of C at each commit of the case, and commit gives it to each
// commit after that as it gives C's.
func (s *checkCase) tieLikeC(id string) {
	for _, ties := range s.Ties {
		ties[id] = ties[s.check]
	}
}

// codeResults is the case of k, a query or an agent of the code, with C's tie: a pass at runCommit,
// c1 or c2, and a failing example at c1 that failed, as it must; subject c2, which differs from c1
// outside the tie.
func (s *checkCase) codeResults(k Check, runCommit string) {
	s.t.Helper()
	s.add(k)
	s.tieLikeC(k.ID)
	s.commit("c2", "c1", "docs/x.md")
	s.Subject.Commit = "c2"
	s.Results = append(s.Results, Result{Check: k.ID, Kind: ResultRun, Outcome: OutcomePass, Commit: runCommit, At: checkStateRun},
		Result{Check: k.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, Commit: "c1", At: checkStateRun.Add(5 * time.Minute)})
}

// runsAgainAt adds, for each run of the case at a commit, the same run at commit an hour later: CI
// runs a check's job again at a new commit, and a run of a check of the code counts only at its own
// commit, while a failing example that the test caught counts there too while no file of the
// check's tie changed.
func (s *checkCase) runsAgainAt(commit string) {
	for _, r := range slices.Clone(s.Results) {
		if r.Kind == ResultRun && r.Commit != "" {
			r.Commit, r.At = commit, r.At.Add(time.Hour)
			s.Results = append(s.Results, r)
		}
	}
}

// specNodeID is the id of a statement or a check.
func specNodeID(n specNode) string {
	switch n := n.(type) {
	case Statement:
		return n.ID
	case Check:
		return n.ID
	}
	panic("specNodeID of a node that is no statement and no check")
}

// withTimeLimit is c with a time limit.
func withTimeLimit(c Check, limit time.Duration) Check {
	c.TimeLimit = limit
	return c
}

// checkRun is a run of C's job at commit, leg "" and attempt 1, recorded at at.
func checkRun(commit string, outcome Outcome, at time.Time) Result {
	return legRun(commit, "", 1, outcome, at)
}

// legRun is a run of C's job at commit in one leg and attempt, recorded at at.
func legRun(commit, leg string, attempt int, outcome Outcome, at time.Time) Result {
	return Result{Check: checkStateCheck.ID, Kind: ResultRun, Outcome: outcome, Commit: commit, Job: "go-checks", Leg: leg, Attempt: attempt, At: at}
}

// caughtFault is a failing example of C on which its test failed, with the fault that the step
// planted on commit (plantedOn): author-b, who did not write the test, planted the fault.
func caughtFault(commit string, at time.Time) Result {
	return plantedOn(Result{Check: checkStateCheck.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, Job: "go-checks", Attempt: 1,
		At: at, FaultAuthor: "author-b", TestAuthors: []string{"author-a"}}, commit)
}

// laterMissed is a failing example of C after the base's caught one, ten minutes after the base's
// run, planted on c1 by the step in a fault of its own, on which the test missed its fault, with
// change made to it.
func laterMissed(change func(r *Result)) Result {
	r := caughtFault("c1", checkStateRun.Add(10*time.Minute))
	r.Commit, r.Outcome = "f2-c1", OutcomePass
	change(&r)
	return r
}

// checkStateFaultPath is the path that the fault of each failing example of a program check of the
// code changes in these tests: a source file of C's tie (checkStateTie), and no file of a test.
const checkStateFaultPath = "packages/envoy/cmd/listener/main.go"

// plantedOn is r, a failing example of a program check of the code, with the fault that the step
// planted on commit (R9: "For a check that a program runs, the failing example is a fault that a
// step plants in the code"): its Commit the fault's own commit "f-<commit>", which CI tested, its
// PlantedOn commit, from which it counts, and its FaultPaths checkStateFaultPath.
func plantedOn(r Result, commit string) Result {
	r.Commit, r.PlantedOn, r.FaultPaths = "f-"+commit, commit, []string{checkStateFaultPath}
	return r
}
