package specgraph

import (
	"cmp"
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
// which reads the code; and three checks that some rows add under L: P, a query of production, G, a
// query of the graph, and Q, a program check of production.
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
	checkStateGraph = Check{ID: "the-graph-holds-a-delivery-of-each-message", Serves: "a-stored-message-survives-a-restart",
		Subject: SubjectGraph, Procedure: ProcedureQuery,
		Claim:          "Each message record of the graph has a delivery record.",
		ProcedureText:  "The query reads each message record of the graph.\nIt returns each message record that has no delivery record.",
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

// The state of a check at one subject, in the decision order of R7, R8 and R9. Each row is the base
// with one change: at c1 a pass of C's job and a failing example on which C's test failed, with a
// fault that author-b planted in the test of author-a, both after the approval of L and T; subject
// the head c1 of branch b; Now one hour after the newest result. "c2 differs in X" means c2's tree
// is c1's with X changed. A row keeps the base's caught failing example unless it says so, so a
// row that reads false reads false by its run alone. A failure decides only after each unknown case
// of R9 (R9: "Five cases read as unknown, and never as true"), which the rows marked B2 pin.
func TestCheckState(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name   string
		change func(c *checkCase)
		want   CheckState // ID "" is C
		absent bool       // the check is not in the output
	}{
		{name: "base", change: func(*checkCase) {},
			want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "v-state 1: the run fails", change: func(c *checkCase) { c.Results[0].Outcome = OutcomeFail },
			want: CheckState{State: "false", Reason: "failed", From: "c1"}},
		{name: "v-state 2: no run result", change: func(c *checkCase) { c.Results = c.Results[1:] },
			want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "v-state 3: the pass carries the digest of the previous approved text", change: func(c *checkCase) {
			earlier := checkStateCheck
			earlier.Claim = "A message that the listener stores before a restart reaches its session."
			c.approveEarlier(checkStateApproved.Add(-48*time.Hour), earlier)
			c.Results[0].CheckDigest = earlier.Digest()
		}, want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "v-state 4: the subject moved", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c2"
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "v-state 5: the pass is older than the time limit", change: func(c *checkCase) {
			c.replace(withTimeLimit(checkStateCheck, 24*time.Hour))
			c.Now = checkStateRun.Add(25 * time.Hour)
		}, want: CheckState{State: "unknown", Reason: "expired"}},
		{name: "v-state 5, a failure (B2): the failure is older than the time limit", change: func(c *checkCase) {
			c.replace(withTimeLimit(checkStateCheck, 24*time.Hour))
			c.Results[0].Outcome = OutcomeFail
			c.Now = checkStateRun.Add(25 * time.Hour)
		}, want: CheckState{State: "unknown", Reason: "expired"}},
		{name: "v-state 6: the check could not run", change: func(c *checkCase) { c.Results[0].Outcome = OutcomeCouldNotRun },
			want: CheckState{State: "unknown", Reason: "could_not_run"}},
		{name: "v-state 7, v-suspect 3, prototype: a fail at attempt 2 after a pass at attempt 1", change: func(c *checkCase) {
			c.Results = append(c.Results, legRun("c1", "", 2, OutcomeFail, checkStateRun.Add(10*time.Minute)))
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "v-state 7, v-suspect 3, prototype: a pass at attempt 2 after a fail at attempt 1", change: func(c *checkCase) {
			c.Results[0].Outcome = OutcomeFail
			c.Results = append(c.Results, legRun("c1", "", 2, OutcomePass, checkStateRun.Add(10*time.Minute)))
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "v-state 7, v-suspect 3, prototype: a pass and a fail with one time", change: func(c *checkCase) {
			c.Results = append(c.Results, legRun("c1", "", 2, OutcomeFail, checkStateRun))
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "v-state 7, two legs (B1): leg linux passes and leg arm fails", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), legRun("c1", "arm", 1, OutcomeFail, checkStateRun),
				caughtFault("c1", checkStateRun.Add(5*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "v-state 7, legs and attempts (B1): leg linux fails at attempt 1 and leg arm passes at attempt 2", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomeFail, checkStateRun), legRun("c1", "arm", 2, OutcomePass, checkStateRun.Add(10*time.Minute)),
				caughtFault("c1", checkStateRun.Add(15*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "legs across commits: leg linux passes at c1 and leg arm fails at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), caughtFault("c1", checkStateRun.Add(5*time.Minute)),
				legRun("c2", "arm", 1, OutcomeFail, checkStateRun.Add(time.Hour)),
			}
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "a disagreement carries: a pass and a fail at c1, then a pass at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results,
				legRun("c1", "", 2, OutcomeFail, checkStateRun.Add(10*time.Minute)), checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "a leg could not run", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), legRun("c1", "arm", 1, OutcomeCouldNotRun, checkStateRun),
				caughtFault("c1", checkStateRun.Add(5*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "could_not_run"}},
		{name: "a silent leg: leg arm of the job ended with no result", change: func(c *checkCase) {
			c.Results[0].Leg = new("linux")
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: new("linux"), Commit: "c1", Ended: new(true)}, {Job: "go-checks", Leg: new("arm"), Commit: "c1", Ended: new(true)}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}},
		{name: "v-state 8: no failing-example result", change: func(c *checkCase) { c.Results = c.Results[:1] },
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "v-state 8, a failure (B2): the run fails, no failing-example result", change: func(c *checkCase) {
			c.Results = c.Results[:1]
			c.Results[0].Outcome = OutcomeFail
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "v-state 11: a merge changes no file of the tie", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "v-state 12: the check runs again after its code changed", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)), caughtFault("c2", checkStateRun.Add(65*time.Minute)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "v-state 12, the run alone: no new fault run after the code changed", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "v-state 13: a tie that is not known does not carry", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Ties["c2"][c.check] = Tie{Known: false, Why: "the test's imports could not be read"}
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "v-state 15: the author of the test wrote the fault", change: func(c *checkCase) { c.Results[1].FaultAuthor = "author-a" },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, a failure (B2): the run fails and the author of the test wrote the fault", change: func(c *checkCase) {
			c.Results[0].Outcome = OutcomeFail
			c.Results[1].FaultAuthor = "author-a"
		}, want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, no author: the fault names no author", change: func(c *checkCase) { c.Results[1].FaultAuthor = "" },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		// R9: "An agent that did not write the test writes the fault." A failing example whose record
		// names no author of the test cannot show that, so it does not count: the check reads unknown,
		// as it does when the author of the test wrote the fault (the row above) or the fault names no
		// author. A complete record of another agent counts.
		{name: "v-state 15, no author of the test: the record names no test authors", change: func(c *checkCase) { c.Results[1].TestAuthors = nil },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, no author of the test: the list of test authors is empty", change: func(c *checkCase) { c.Results[1].TestAuthors = []string{} },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, no author of the test: the list of test authors holds only an empty name", change: func(c *checkCase) { c.Results[1].TestAuthors = []string{""} },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, a complete record: an agent that wrote no part of the test wrote the fault", change: func(c *checkCase) {
			c.Results[1].TestAuthors = []string{"author-a", "author-c"}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "v-state 15, production: the failing example of a program check of production names no author of the test", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
			c.Results[3].TestAuthors = nil
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "fault_by_test_author"}},
		// An author of only white space names nobody, by the test that Validate reads a missing text
		// with (hasWord).
		{name: "v-state 15, no author of the test: the one test author is only white space", change: func(c *checkCase) { c.Results[1].TestAuthors = []string{" "} },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 15, no author: the author of the fault is only white space", change: func(c *checkCase) { c.Results[1].FaultAuthor = " " },
			want: CheckState{State: "unknown", Reason: "fault_by_test_author"}},
		{name: "v-state 16: the emptied test passes on the fault", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/restart_test.go")
			c.Subject.Commit = "c2"
			missed := caughtFault("c2", checkStateRun.Add(65*time.Minute))
			missed.Outcome = OutcomePass
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)), missed)
		}, want: CheckState{State: "unknown", Reason: "failing_example_missed"}},
		{name: "v-state 17: no setting makes the failing example optional", change: func(c *checkCase) {
			printed, err := checkStateCheck.Print()
			if err != nil {
				c.t.Fatal(err)
			}
			optional := printed + "failing_example_optional: true\n"
			if _, problems := Parse(map[string][]byte{"spec.yaml": []byte(optional)}); len(problems) != 1 || problems[0].Rule != "schema" {
				c.t.Errorf("Parse of a check that makes its failing example optional: %+v, want one schema problem", problems)
			}
			c.Results = c.Results[:1]
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		// R9: "And a check whose current text has no failing example that failed when it should."
		// A text that names no failing example has none: the pilot builds the approved spec from the
		// store and calls CheckStates with no reader, so CheckStates itself reads it unknown.
		{name: "R9: the approved text of the check names no failing example, whatever its results", change: func(c *checkCase) {
			bare := checkStateCheck
			bare.FailingExample = ""
			c.replace(bare)
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "R9: the approved text of the check names no failing example, and it has no result", change: func(c *checkCase) {
			bare := checkStateCheck
			bare.FailingExample = ""
			c.replace(bare)
			c.Results = nil
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "R9: the approved text of the check names no failing example, and its run fails", change: func(c *checkCase) {
			bare := checkStateCheck
			bare.FailingExample = ""
			c.replace(bare)
			c.Results[0].Outcome = OutcomeFail
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		// A failing example of no letter and no digit is a missing one, by the rule that Validate
		// reads it with (TestTheTreeAndTheDerivationReadOneMissingFailingExample).
		{name: "R9: the approved failing example of the check is a text of no letter and no digit", change: func(c *checkCase) {
			bare := checkStateCheck
			bare.FailingExample = "-"
			c.replace(bare)
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		// D1: the derivation gives no clean state for a tree that the rules refuse, since the pilot
		// calls it with no reader. A check that Validate refuses reads unknown, refused, with the
		// rule as the detail, whatever its results (TestNoTreeThatTheRulesRefuseReadsTrueOrMet).
		{name: "D1: a program check that names no job", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Test.Job = "" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "schema"}},
		{name: "D1: a program check that names no test", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Test = nil })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "schema"}},
		{name: "D1: a check whose claim has no letter and no digit", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Claim = "-" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "no-claim"}},
		{name: "D1: a check of a procedure that is not program, query or agent", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Procedure = "script" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "schema"}},
		{name: "D1: a check of a subject that is not code, production or graph", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Subject = "staging" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "schema"}},
		{name: "D1: a problem of a term in the file of the check", change: func(c *checkCase) {
			c.files["spec.yaml"] = append(c.files["spec.yaml"], Term{ID: "message", Name: "message", Definition: "-"})
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "bad-term"}},
		{name: "D1: a problem of a term in a file that holds no check and no statement", change: func(c *checkCase) {
			c.files["terms.yaml"] = []specNode{Term{ID: "message", Name: "message", Definition: "-"}}
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "bad-term"}},
		{name: "v-state 18: a merge changes a file that the test reads", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/testdata/a.json")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "a file moves out of the tie", change: func(c *checkCase) {
			files := c.commit("c2", "c1")
			delete(files, "packages/envoy/cmd/listener/testdata/a.json")
			files["docs/a.json"] = "1"
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "B5, a read that the repository records: c2 changes a fixture of another package", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/contracts/fixtures/dispatch-targeted-delivery.json")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "B5, the job ran at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c2", Ended: new(true)}}
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "R8, the job ran and its test did not (v-citations 5)", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c2", Ended: new(true)}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "R6, another job: the pass was reported by job envoy-go", change: func(c *checkCase) { c.Results[0].Job = "envoy-go" },
			want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "R6 is about runs: the caught fault names no job", change: func(c *checkCase) { c.Results[1].Job = "" },
			want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "v-suspect 1: L's text is approved again after the pass", change: func(c *checkCase) {
			changed := checkStateLeaf
			changed.Text = "Envoy delivers each stored message to its session after the listener starts again, in the order of the writes."
			c.reapprove(checkStateRun.Add(2*time.Hour), changed)
		}, want: CheckState{State: "unknown", Reason: "statement_changed", Detail: checkStateLeaf.ID}},
		{name: "v-suspect 2: L moves to another file with the same text", change: func(c *checkCase) {
			c.files = map[string][]specNode{"spec.yaml": {checkStateTop, checkStateCheck}, "restart.yaml": {checkStateLeaf}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "Review focus 3: T's text is approved again after the pass", change: func(c *checkCase) {
			changed := checkStateTop
			changed.Text = "Envoy delivers each message to the session that it is for, and to no other session."
			c.reapprove(checkStateRun.Add(2*time.Hour), changed)
		}, want: CheckState{State: "unknown", Reason: "statement_changed", Detail: checkStateTop.ID}},
		{name: "B4, a squash outside the tie", change: func(c *checkCase) {
			c.commit("m1", "c1")
			c.commit("b1", "m1", "packages/envoy/cmd/listener/main.go")
			c.commit("m2", "m1", "docs/x.md")["packages/envoy/cmd/listener/main.go"] = "b1" // the squash of b1 on m1
			c.Subject = Point{Commit: "m2", Branch: "main"}
			c.Results[0].Commit, c.Results[1].Commit = "b1", "b1"
		}, want: CheckState{State: "true", Reason: "passed", From: "b1"}},
		{name: "B4, a squash inside the tie: main changed a file of the tie before the squash", change: func(c *checkCase) {
			c.commit("m1", "c1")
			c.commit("b1", "m1", "packages/envoy/cmd/listener/restart_test.go")
			c.commit("m1b", "m1", "packages/envoy/cmd/listener/main.go")
			c.commit("m2", "m1b")["packages/envoy/cmd/listener/restart_test.go"] = "b1" // b1's test on m1b
			c.Subject = Point{Commit: "m2", Branch: "main"}
			c.Results[0].Commit, c.Results[1].Commit = "b1", "b1"
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "Review focus 4, another branch: the results at x1 count at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.commit("x1", "c2", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[0].Commit, c.Results[1].Commit = "x1", "x1"
		}, want: CheckState{State: "true", Reason: "passed", From: "x1"}},
		{name: "Review focus 4, a commit the history cannot read", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[0].Commit, c.Results[1].Commit = "x9", "x9"
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "v-citations 4: CI refused the check at the subject", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Uncited = map[string]map[string]string{"c2": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." A
		// refusal counts wherever the results of its commit count, a failing example's commit too,
		// and for a program check of production the commit of its test.
		{name: "R48, a refusal at the commit of the caught failing example alone, whose results count at c2", change: func(c *checkCase) {
			c.commit("x1", "c1", "docs/x.md")
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[1].Commit = "x1"
			c.Uncited = map[string]map[string]string{"x1": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "R48, a program check of production that CI refused at the commit of the test that caught its fault", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "R9, the job ended with no result", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: new(""), Commit: "c2", Ended: new(true)}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault"}},
		{name: "a later failure cancels a carried pass", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomeFail, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "false", Reason: "failed", From: "c2"}},
		{name: "item 17: a failure and a later pass at two commits of one tie", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.commit("c3", "c1", "docs/x.md")
			c.Subject.Commit = "c3"
			c.Results = append(c.Results,
				checkRun("c2", OutcomeFail, checkStateRun.Add(time.Hour)), legRun("c1", "", 2, OutcomePass, checkStateRun.Add(2*time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "item 17: a fix that changes a file of the tie ends the carry of the failure", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.commit("c3", "c2", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c3"
			c.Results = append(c.Results, checkRun("c2", OutcomeFail, checkStateRun.Add(time.Hour)),
				checkRun("c3", OutcomePass, checkStateRun.Add(2*time.Hour)), caughtFault("c3", checkStateRun.Add(125*time.Minute)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c3"}},
		{name: "production: the newest deploy decides", change: func(c *checkCase) {
			c.add(checkStateProduction)
			c.Subject = Point{Commit: "c9", Deploy: "d2"}
			c.Results = append(c.Results,
				productionRun(checkStateProduction, "d1", OutcomeFail, checkStateRun.Add(time.Hour)),
				productionRun(checkStateProduction, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)),
				Result{Check: checkStateProduction.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
		}, want: CheckState{ID: checkStateProduction.ID, State: "true", Reason: "passed", From: "d2"}},
		// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." A
		// query or an agent of production or of the graph has no candidate of a commit, so only CI's
		// refusal at S.Commit counts for it (step 2), and one at another commit does not.
		{name: "R48, a query of production that CI refused at the commit of a deploy subject", change: func(c *checkCase) {
			c.add(checkStateProduction)
			c.Subject = Point{Commit: "c9", Deploy: "d2"}
			c.Results = append(c.Results,
				productionRun(checkStateProduction, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)),
				Result{Check: checkStateProduction.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
			c.Uncited = map[string]map[string]string{"c9": {checkStateProduction.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProduction.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "control: a query of production that CI refused at another commit than the subject's", change: func(c *checkCase) {
			c.add(checkStateProduction)
			c.Subject = Point{Commit: "c9", Deploy: "d2"}
			c.Results = append(c.Results,
				productionRun(checkStateProduction, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)),
				Result{Check: checkStateProduction.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
			c.Uncited = map[string]map[string]string{"c8": {checkStateProduction.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProduction.ID, State: "true", Reason: "passed", From: "d2"}},
		{name: "production on a branch head is not judged", change: func(c *checkCase) {
			c.add(checkStateProduction)
			c.Subject = Point{Commit: "c9", Branch: "b"}
			c.Results = append(c.Results,
				productionRun(checkStateProduction, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)),
				Result{Check: checkStateProduction.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
		}, want: CheckState{ID: checkStateProduction.ID}, absent: true},
		{name: "graph: the transaction of the subject decides", change: func(c *checkCase) {
			c.add(checkStateGraph)
			c.Subject = Point{Commit: "c9", Deploy: "d2", GraphTx: "tx2"}
			c.Results = append(c.Results,
				Result{Check: checkStateGraph.ID, Kind: ResultRun, Outcome: OutcomePass, GraphTx: "tx1", At: checkStateRun.Add(time.Hour)},
				Result{Check: checkStateGraph.ID, Kind: ResultRun, Outcome: OutcomeFail, GraphTx: "tx2", At: checkStateRun.Add(2 * time.Hour)},
				Result{Check: checkStateGraph.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
		}, want: CheckState{ID: checkStateGraph.ID, State: "false", Reason: "failed", From: "tx2"}},
		{name: "item 19: the test of a program check of production changed after it caught the fault", change: func(c *checkCase) {
			c.productionProgram("packages/envoy/cmd/listener/restart_test.go", "c1")
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "no_failing_example"}},
		{name: "item 19: the test of a program check of production did not change", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "true", Reason: "passed", From: "d2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			row.change(s)
			states := checkStates(t, s.input())
			if !slices.IsSortedFunc(states, func(a, b CheckState) int { return cmp.Compare(a.ID, b.ID) }) {
				t.Errorf("CheckStates is not sorted by id: %+v", states)
			}
			want := row.want
			if want.ID == "" {
				want.ID = checkStateCheck.ID
			}
			i := slices.IndexFunc(states, func(s CheckState) bool { return s.ID == want.ID })
			switch {
			case row.absent && i >= 0:
				t.Errorf("CheckStates holds %+v, want no state of %s", states[i], want.ID)
			case row.absent:
			case i < 0:
				t.Errorf("CheckStates = %+v, want a state of %s", states, want.ID)
			case states[i] != want:
				t.Errorf("CheckStates:\n got %+v\nwant %+v", states[i], want)
			}
		})
	}
}

// The files and the approved spec decide which checks CheckStates judges: each check that the
// approved spec holds a current text of, in the order of its id. A check whose current approved text
// the files do not hold reads not built, whatever its results, also when they hold an earlier
// approved text of it; a check the files hold that the approved spec never held has no state here,
// since the comparison of R2 refuses it.
func TestCheckStatesJudgesTheApprovedChecks(t *testing.T) {
	t.Parallel()
	s := newCheckCase(t)
	s.add(checkStateGraph)
	s.files["spec.yaml"] = append(s.files["spec.yaml"], checkStateProduction) // in the files, never approved
	s.Subject = Point{Commit: "c1", Deploy: "d1", GraphTx: "tx1"}
	earlier := checkStateCheck
	earlier.Claim = "A message that the listener stores before a restart reaches its session."
	s.approveEarlier(checkStateApproved.Add(-48*time.Hour), earlier)
	s.swap(earlier)
	states := checkStates(t, s.input())
	want := []CheckState{
		{ID: checkStateCheck.ID, State: "unknown", Reason: "not_built"},
		{ID: checkStateGraph.ID, State: "unknown", Reason: "no_result"},
	}
	if !slices.Equal(states, want) {
		t.Errorf("CheckStates:\n got %+v\nwant %+v", states, want)
	}
}

// R9: "No setting makes the failing example optional", and a check "whose current text has no
// failing example that failed when it should" reads unknown. The rules of the tree and the
// derivation read a missing failing example by one rule: the schema (an absent one) or Validate
// (one of no letter and no digit) refuses the check exactly when CheckStates reads it unknown,
// no_failing_example, whatever its results. Each row is the base of TestCheckState, a pass and a
// caught fault at c1, with the failing example of C's text, in the files and in the approved spec,
// replaced by the row's text.
func TestTheTreeAndTheDerivationReadOneMissingFailingExample(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, text string
		missing    bool
	}{
		{"no text", "", true},
		{"a dash", "-", true},
		{"marks of punctuation", "… ?!", true},
		{"a format character", "\u200b", true},
		{"a digit", "0", false},
		{"a sentence", checkStateCheck.FailingExample, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			c := checkStateCheck
			c.FailingExample = row.text
			s.replace(c)
			tree := s.parse()
			refused := slices.ContainsFunc(append(schemaProblems(tree), Validate(tree, Options{})...), func(p Problem) bool {
				return p.Node == c.ID && (p.Rule == "no-failing-example" || p.Rule == "schema" && strings.Contains(p.Detail, "failing_example"))
			})
			if refused != row.missing {
				t.Errorf("the rules refuse the failing example %q (no-failing-example, or the schema for no failing example): %t, want %t", row.text, refused, row.missing)
			}
			want := CheckState{ID: c.ID, State: "true", Reason: "passed", From: "c1"}
			if row.missing {
				want = CheckState{ID: c.ID, State: "unknown", Reason: "no_failing_example"}
			}
			if states := checkStates(t, s.input()); !slices.Contains(states, want) {
				t.Errorf("CheckStates with the failing example %q:\n got %+v\nwant %+v", row.text, states, want)
			}
		})
	}
}

// R7: "The procedure is the text of the check and the code that runs it, so a change to either ends
// the standing of earlier results." A run of a program check of production or of the graph records
// the commit of the test that ran (TestCommit), and it counts only while no file of the check's tie
// differs between that commit and the subject's, as its failing example does (R9: "its test, as it
// is now"). A failing example that the new test caught does not make a run of the old test count.
// Each row: subject c2 under deploy d2, where c2 differs from c1 in changed; a pass of the row's
// check at the test of runCommit, and a failing example that the test at c2 caught (author-b's fault
// in author-a's test); C taken out, so that L and T follow the row's check alone.
func TestARunOfProductionOrTheGraphCountsForTheCodeThatRanIt(t *testing.T) {
	t.Parallel()
	listenerTest := "packages/envoy/cmd/listener/restart_test.go"
	for _, row := range []struct {
		name               string
		check              Check
		changed, runCommit string
		want               CheckState // ID "" is the row's check
		statements         string     // the state of L and of T
	}{
		{"control: production, c2 changes a file outside the tie", checkStateProductionProgram, "docs/x.md", "c1",
			CheckState{State: "true", Reason: "passed", From: "d2"}, "met"},
		{"production, c2 changes the test, and the pass ran the test of c1", checkStateProductionProgram, listenerTest, "c1",
			CheckState{State: "unknown", Reason: "subject_moved"}, "unknown"},
		{"control: production, c2 changes the test, and the pass ran the test of c2", checkStateProductionProgram, listenerTest, "c2",
			CheckState{State: "true", Reason: "passed", From: "d2"}, "met"},
		{"control: the graph, c2 changes a file outside the tie", checkStateGraphProgram, "docs/x.md", "c1",
			CheckState{State: "true", Reason: "passed", From: "tx1"}, "met"},
		{"the graph, c2 changes the test, and the pass ran the test of c1", checkStateGraphProgram, listenerTest, "c1",
			CheckState{State: "unknown", Reason: "subject_moved"}, "unknown"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			s.add(row.check)
			s.tieLikeC(row.check.ID)
			s.commit("c2", "c1", row.changed)
			s.Subject = Point{Commit: "c2", Deploy: "d2", GraphTx: "tx1"}
			run := Result{Check: row.check.ID, Kind: ResultRun, Outcome: OutcomePass, Job: row.check.Test.Job, TestCommit: row.runCommit, At: checkStateRun.Add(time.Hour)}
			if row.check.Subject == SubjectGraph {
				run.GraphTx = "tx1"
			} else {
				run.Deploy = "d2"
			}
			s.Results = append(s.Results, run, Result{Check: row.check.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, TestCommit: "c2",
				At: checkStateRun.Add(65 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
			want := row.want
			want.ID = row.check.ID
			deriveAlone(t, s, want, row.statements)
		})
	}
}

// R12: "Each leg of a matrix reports its own result, and no leg replaces the result of another."
// R9: "When a check has no result that counts after a run of its job ends, it reads unknown." A
// program check of production runs in the legs of its job's matrix, as a code check does: the newest
// result of each leg counts, a newer result replaces an older one only in its own leg, a leg whose
// run of the job ended with no result that counts reads unknown, and legs whose newest results
// disagree read unknown. Each row: Q at subject c2 under deploy d2, where c2 differs from c1 outside
// the tie, or, in a row that carries, at c3, which differs from c2 outside the tie too, so that the
// results of the test at c2 count there (R8); the runs of legs linux and arm of Q's job ended at c2;
// the row's runs under d2 at the test of c2, and a failing example that the test at c2 caught; C
// taken out. A leg that ended with no result at c2 counts wherever the results of c2 count.
func TestEachLegOfAProgramCheckOfProductionKeepsItsResult(t *testing.T) {
	t.Parallel()
	q := checkStateProductionProgram
	run := func(leg string, outcome Outcome, after time.Duration) Result {
		return Result{Check: q.ID, Kind: ResultRun, Outcome: outcome, Deploy: "d2", Job: q.Test.Job, Leg: new(leg), TestCommit: "c2", At: checkStateRun.Add(after)}
	}
	for _, row := range []struct {
		name       string
		runs       []Result
		want       CheckState
		statements string
		carries    bool // the subject is c3, where the results of c2 count
	}{
		{"control: leg linux and leg arm pass", []Result{run("linux", OutcomePass, time.Hour), run("arm", OutcomePass, time.Hour)},
			CheckState{State: "true", Reason: "passed", From: "d2"}, "met", false},
		{"leg linux passes after leg arm failed", []Result{run("arm", OutcomeFail, time.Hour), run("linux", OutcomePass, 2*time.Hour)},
			CheckState{State: "unknown", Reason: "disagree", Disagree: true}, "unknown", false},
		{"leg linux passes, and the run of leg arm ended with no result", []Result{run("linux", OutcomePass, time.Hour)},
			CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}, "unknown", false},
		{"leg linux passes, and the run of leg arm ended with no result, at c3, where the results of c2 count", []Result{run("linux", OutcomePass, time.Hour)},
			CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}, "unknown", true},
		{"control: leg linux and leg arm pass, at c3, where the results of c2 count", []Result{run("linux", OutcomePass, time.Hour), run("arm", OutcomePass, time.Hour)},
			CheckState{State: "true", Reason: "passed", From: "d2"}, "met", true},
		{"control: in leg arm a newer pass replaces a failure, and leg linux passes", []Result{
			run("arm", OutcomeFail, time.Hour), run("arm", OutcomePass, 2*time.Hour), run("linux", OutcomePass, 2*time.Hour)},
			CheckState{State: "true", Reason: "passed", From: "d2"}, "met", false},
		{"control: both legs fail", []Result{run("linux", OutcomeFail, time.Hour), run("arm", OutcomeFail, time.Hour)},
			CheckState{State: "false", Reason: "failed", From: "d2"}, "not_met", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			s.add(q)
			s.commit("c2", "c1", "docs/x.md")
			s.Subject = Point{Commit: "c2", Deploy: "d2"}
			if row.carries {
				s.commit("c3", "c2", "docs/x.md")
				s.Subject.Commit = "c3"
			}
			s.JobRuns = []JobRun{{Job: q.Test.Job, Leg: new("linux"), Commit: "c2", Ended: new(true)}, {Job: q.Test.Job, Leg: new("arm"), Commit: "c2", Ended: new(true)}}
			s.Results = append(append(s.Results, row.runs...), Result{Check: q.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, TestCommit: "c2",
				At: checkStateRun.Add(5 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
			want := row.want
			want.ID = q.ID
			deriveAlone(t, s, want, row.statements)
		})
	}
}

// deriveAlone takes C out of the case, so that L and T follow the other checks alone, derives the
// state, and fails the test unless the check of want reads want and L and T both read statements.
func deriveAlone(t *testing.T, s *checkCase, want CheckState, statements string) {
	t.Helper()
	s.remove(checkStateCheck.ID)
	state := derive(t, s.input())
	if i := slices.IndexFunc(state.Checks, func(c CheckState) bool { return c.ID == want.ID }); i < 0 || state.Checks[i] != want {
		t.Errorf("the checks: %+v\nwant %+v", state.Checks, want)
	}
	for _, id := range []string{checkStateLeaf.ID, checkStateTop.ID} {
		if i := slices.IndexFunc(state.Statements, func(st StatementState) bool { return st.ID == id }); i < 0 || state.Statements[i].State != statements {
			t.Errorf("statement %s in %+v: want %s", id, state.Statements, statements)
		}
	}
}

// checkCase is one row's input to CheckStates: the spec files as nodes in files, the approved spec,
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
	s.files = map[string][]specNode{"spec.yaml": {checkStateTop, checkStateLeaf, checkStateCheck}}
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
		if r.Leg == nil {
			r.Leg = new("")
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
	// A row can plant a fault of the shape of a node, which the schema finds; the derivation judges
	// the tree by the same schema (refusals), so the case reads the tree all the same.
	if problems = slices.DeleteFunc(problems, func(p Problem) bool { return p.Rule == "schema" }); len(problems) != 0 {
		s.t.Fatalf("the files of the case: %+v", problems)
	}
	return tree
}

// specFiles is the text of each spec file of the case, by name: its nodes as Print writes them.
func (s *checkCase) specFiles() map[string][]byte {
	s.t.Helper()
	files := map[string][]byte{}
	for name, nodes := range s.files {
		var printed []string
		for _, n := range nodes {
			text, err := n.Print()
			if err != nil {
				s.t.Fatal(err)
			}
			printed = append(printed, text)
		}
		files[name] = []byte(strings.Join(printed, ""))
	}
	return files
}

// approve adds a version of each node to the approved spec, approved at at; a node that it adds
// the approved spec keeps, and its checks decide all of it.
func (s *checkCase) approve(at time.Time, nodes ...specNode) {
	s.t.Helper()
	for _, n := range nodes {
		printed, err := n.Print()
		if err != nil {
			s.t.Fatal(err)
		}
		id := specNodeID(n)
		node, held := s.Approved.Nodes[id]
		if !held {
			node = ApprovedNode{ID: id, Removed: new(false), PartlyChecked: new(false)}
		}
		node.Versions = append(node.Versions, ApprovedVersion{Version: len(node.Versions) + 1, At: at, Document: printed})
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

// add approves a check with the base's nodes and writes it at the end of spec.yaml.
func (s *checkCase) add(c Check) {
	s.t.Helper()
	s.approve(checkStateApproved, c)
	s.files["spec.yaml"] = append(slices.Clone(s.files["spec.yaml"]), c)
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

// productionProgram is the case of item 19: the program check Q of production, with a pass under
// deploy d2 of its test at c2, and a failing example that its test, at commit testCommit, caught;
// subject c2 under deploy d2, where c2 differs from c1 in changed.
func (s *checkCase) productionProgram(changed, testCommit string) {
	s.t.Helper()
	q := checkStateProductionProgram
	s.add(q)
	s.commit("c2", "c1", changed)
	s.Subject = Point{Commit: "c2", Deploy: "d2"}
	run := productionRun(q, "d2", OutcomePass, checkStateRun.Add(time.Hour))
	run.TestCommit = "c2"
	fault := Result{Check: q.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, TestCommit: testCommit,
		At: checkStateRun.Add(65 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}}
	s.Results = append(s.Results, run, fault)
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
	return Result{Check: checkStateCheck.ID, Kind: ResultRun, Outcome: outcome, Commit: commit, Job: "go-checks", Leg: new(leg), Attempt: attempt, At: at}
}

// caughtFault is a failing example of C at commit on which its test failed: author-b, who did not
// write the test, planted the fault.
func caughtFault(commit string, at time.Time) Result {
	return Result{Check: checkStateCheck.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, Commit: commit, Job: "go-checks", Attempt: 1,
		At: at, FaultAuthor: "author-b", TestAuthors: []string{"author-a"}}
}

// productionRun is a run of a production check under deploy, recorded at at.
func productionRun(c Check, deploy string, outcome Outcome, at time.Time) Result {
	r := Result{Check: c.ID, Kind: ResultRun, Outcome: outcome, Deploy: deploy, At: at}
	if c.Test != nil {
		r.Job = c.Test.Job
	}
	return r
}
