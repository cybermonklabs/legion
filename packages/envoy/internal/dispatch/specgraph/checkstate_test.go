package specgraph

import (
	"cmp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The state of a check at one subject, in the decision order of R7 and R9, where a run counts only
// at its own commit and a failing example also at another commit while no file of the check's tie
// changed (the rule that replaces The Spec's R8). Each row is the base
// with one change: at c1 a pass of C's job and a failing example on which C's test failed, with a
// fault that author-b planted in the test of author-a, both after the approval of L and T; subject
// the head c1 of branch b; Now one hour after the newest result. "c2 differs in X" means c2's tree
// is c1's with X changed. A row keeps the base's caught failing example unless it says so, so a
// row that reads false reads false by its run alone. A failure decides only after each unknown case
// of R9 (R9: "Six cases read as unknown, and never as true"), which the rows named "a failure" pin.
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
		{name: "v-state 5, a failure: the failure is older than the time limit", change: func(c *checkCase) {
			c.replace(withTimeLimit(checkStateCheck, 24*time.Hour))
			c.Results[0].Outcome = OutcomeFail
			c.Now = checkStateRun.Add(25 * time.Hour)
		}, want: CheckState{State: "unknown", Reason: "expired"}},
		// R12: "no leg replaces the result of another"; R9's expired case reads each leg: a fresh
		// pass of one leg does not hide that the newest pass of another is older than the limit.
		{name: "a time limit of 24 hours: leg linux passed 1 hour ago and leg arm 30 hours ago", change: func(c *checkCase) {
			c.replace(withTimeLimit(checkStateCheck, 24*time.Hour))
			c.Results = []Result{
				legRun("c1", "arm", 1, OutcomePass, checkStateRun), caughtFault("c1", checkStateRun.Add(5*time.Minute)),
				legRun("c1", "linux", 1, OutcomePass, checkStateRun.Add(29*time.Hour)),
			}
			c.Now = checkStateRun.Add(30 * time.Hour)
		}, want: CheckState{State: "unknown", Reason: "expired"}},
		{name: "control: a time limit of 24 hours, leg linux passed 1 hour ago and leg arm 2 hours ago", change: func(c *checkCase) {
			c.replace(withTimeLimit(checkStateCheck, 24*time.Hour))
			c.Results = []Result{
				legRun("c1", "arm", 1, OutcomePass, checkStateRun.Add(28*time.Hour)), caughtFault("c1", checkStateRun.Add(5*time.Minute)),
				legRun("c1", "linux", 1, OutcomePass, checkStateRun.Add(29*time.Hour)),
			}
			c.Now = checkStateRun.Add(30 * time.Hour)
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		// R7: "The procedure is the text of the check and the code that runs it, so a change to either
		// ends the standing of earlier results." C's text T1 changes to T2 and back to T1: the results
		// of T1 from before the change carry T1's digest, and the approval of T1 again ends them.
		{name: "C's text changes to another and back after its results: no result of the text approved last", change: func(c *checkCase) {
			other := checkStateCheck
			other.FailingExample = "A listener that drops the first stored message when it starts."
			c.reapprove(checkStateRun.Add(2*time.Hour), other)
			c.reapprove(checkStateRun.Add(3*time.Hour), checkStateCheck)
		}, want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "control: C's text changes to another and back, then its job passes again and its test catches the fault again", change: func(c *checkCase) {
			other := checkStateCheck
			other.FailingExample = "A listener that drops the first stored message when it starts."
			c.reapprove(checkStateRun.Add(2*time.Hour), other)
			c.reapprove(checkStateRun.Add(3*time.Hour), checkStateCheck)
			c.Results = append(c.Results, checkRun("c1", OutcomePass, checkStateRun.Add(4*time.Hour)), caughtFault("c1", checkStateRun.Add(4*time.Hour+5*time.Minute)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
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
		{name: "v-state 7, two legs: leg linux passes and leg arm fails", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), legRun("c1", "arm", 1, OutcomeFail, checkStateRun),
				caughtFault("c1", checkStateRun.Add(5*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "v-state 7, legs and attempts: leg linux fails at attempt 1 and leg arm passes at attempt 2", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomeFail, checkStateRun), legRun("c1", "arm", 2, OutcomePass, checkStateRun.Add(10*time.Minute)),
				caughtFault("c1", checkStateRun.Add(15*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "legs across commits: leg linux's pass at c1 does not count at c2, where leg arm fails", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), caughtFault("c1", checkStateRun.Add(5*time.Minute)),
				legRun("c2", "arm", 1, OutcomeFail, checkStateRun.Add(time.Hour)),
			}
		}, want: CheckState{State: "false", Reason: "failed", From: "c2"}},
		{name: "a disagreement of c1 does not count at c2: a pass and a fail at c1, then a pass at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results,
				legRun("c1", "", 2, OutcomeFail, checkStateRun.Add(10*time.Minute)), checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "a leg could not run", change: func(c *checkCase) {
			c.Results = []Result{
				legRun("c1", "linux", 1, OutcomePass, checkStateRun), legRun("c1", "arm", 1, OutcomeCouldNotRun, checkStateRun),
				caughtFault("c1", checkStateRun.Add(5*time.Minute)),
			}
		}, want: CheckState{State: "unknown", Reason: "could_not_run"}},
		{name: "a silent leg: leg arm of the job ended with no result", change: func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "arm", Commit: "c1", Ended: true}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}},
		// R7: "A step that left no result reads as unknown: silence is not a pass." A leg with no
		// result whose run of the job has not ended is no silent leg and no pass: it reads unknown
		// (no_result) until it reports, and a run of the job that has not ended is no fault of CI; a
		// leg that has a result reads that result, whether or not a newer run of it has ended.
		{name: "a leg that has not reported: leg linux passed, and the run of leg arm has not ended", change: func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "arm", Commit: "c1", Ended: false}}
		}, want: CheckState{State: "unknown", Reason: "no_result", Detail: "arm"}},
		{name: "a silent leg beside one that has not reported: leg arm ended with no result, and the run of leg x86 has not ended", change: func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "arm", Commit: "c1", Ended: true},
				{Job: "go-checks", Leg: "x86", Commit: "c1", Ended: false}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}},
		{name: "control: legs linux and arm passed, and a newer run of leg arm has not ended", change: func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.Results = append(c.Results, legRun("c1", "arm", 1, OutcomePass, checkStateRun))
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "linux", Commit: "c1", Ended: true}, {Job: "go-checks", Leg: "arm", Commit: "c1", Ended: true},
				{Job: "go-checks", Leg: "arm", Commit: "c1", Ended: false}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "no run of C, and the run of its job has not ended: no fault of CI", change: func(c *checkCase) {
			c.Results = c.Results[1:]
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c1", Ended: false}}
		}, want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "v-state 8: no failing-example result", change: func(c *checkCase) { c.Results = c.Results[:1] },
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "v-state 8, a failure: the run fails, no failing-example result", change: func(c *checkCase) {
			c.Results = c.Results[:1]
			c.Results[0].Outcome = OutcomeFail
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "v-state 11, ended: a merge that changes no file of the tie does not carry c1's pass to c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
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
		{name: "v-state 15, a failure: the run fails and the author of the test wrote the fault", change: func(c *checkCase) {
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
			block, err := checkStateCheck.Print()
			if err != nil {
				c.t.Fatal(err)
			}
			optional := strings.Replace(block, ` job="go-checks"}`, ` job="go-checks" failing_example="optional"}`, 1)
			if _, problems := Parse(map[string][]byte{"spec.md": []byte(optional)}); len(problems) == 0 || problems[0].Rule != "unknown-attribute" {
				c.t.Errorf("Parse of a check that makes its failing example optional: %+v, want unknown-attribute", problems)
			}
			c.Results = c.Results[:1]
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		// R9: "And a check whose current text has no failing example that failed when it should."
		// A text that names no failing example has none: the pilot builds the approved spec from the
		// store and calls deriveChecks with no reader, so deriveChecks itself reads it unknown.
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
		// The derivation gives no clean state for a tree that the rules refuse, since the pilot
		// calls it with no reader. A check that Validate refuses reads unknown, refused, with the
		// rule as the detail, whatever its results (TestNoTreeThatTheRulesRefuseReadsTrueOrMet).
		{name: "the tree rules refuse a program check that names no job", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Test.Job = "" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "no-job"}},
		{name: "the tree rules refuse a program check that names no test", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Test = nil })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "no-test"}},
		{name: "the tree rules refuse a check whose claim has no letter and no digit", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Claim = "-" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "no-claim"}},
		{name: "the tree rules refuse a check of a procedure that is not program, query or agent", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Procedure = "script" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "bad-procedure"}},
		{name: "the tree rules refuse a check of a subject that is not code, production or graph", change: func(c *checkCase) {
			c.refuse(func(r *Check) { r.Subject = "staging" })
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "bad-subject"}},
		{name: "the tree rules refuse the file of the check for a problem of a term", change: func(c *checkCase) {
			c.files["spec.md"] = append(c.files["spec.md"], Term{ID: "message", Name: "message", Definition: "-"})
		}, want: CheckState{State: "unknown", Reason: "refused", Detail: "bad-term"}},
		{name: "the tree rules refuse a file that holds no check and no statement for a problem of a term", change: func(c *checkCase) {
			c.files["terms.md"] = []specNode{Term{ID: "message", Name: "message", Definition: "-"}}
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
		{name: "a read that the repository records: c2 changes a fixture of another package", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/contracts/fixtures/dispatch-targeted-delivery.json")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "the job ran at c2, and C passed there", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c2", Ended: true}}
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "the job ran at c2 and its test did not (v-citations 5): c1's pass does not count at c2, so CI's fault", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c2", Ended: true}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault"}},
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
			c.files = map[string][]specNode{"spec.md": {checkStateTop, checkStateCheck}, "restart.md": {checkStateLeaf}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "T's text, above L's, is approved again after the pass", change: func(c *checkCase) {
			changed := checkStateTop
			changed.Text = "Envoy delivers each message to the session that it is for, and to no other session."
			c.reapprove(checkStateRun.Add(2*time.Hour), changed)
		}, want: CheckState{State: "unknown", Reason: "statement_changed", Detail: checkStateTop.ID}},
		{name: "a squash outside the tie: b1's caught failing example counts at m2, beside m2's own pass", change: func(c *checkCase) {
			c.commit("m1", "c1")
			c.commit("b1", "m1", "packages/envoy/cmd/listener/main.go")
			c.commit("m2", "m1", "docs/x.md")["packages/envoy/cmd/listener/main.go"] = "b1" // the squash of b1 on m1
			c.Subject = Point{Commit: "m2", Branch: "main"}
			c.Results[0].Commit, c.Results[1] = "b1", plantedOn(c.Results[1], "b1")
			c.Results = append(c.Results, checkRun("m2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "m2"}},
		{name: "a squash inside the tie: main changed a file of the tie before the squash", change: func(c *checkCase) {
			c.commit("m1", "c1")
			c.commit("b1", "m1", "packages/envoy/cmd/listener/restart_test.go")
			c.commit("m1b", "m1", "packages/envoy/cmd/listener/main.go")
			c.commit("m2", "m1b")["packages/envoy/cmd/listener/restart_test.go"] = "b1" // b1's test on m1b
			c.Subject = Point{Commit: "m2", Branch: "main"}
			c.Results[0].Commit, c.Results[1] = "b1", plantedOn(c.Results[1], "b1")
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "a commit of another branch: the caught failing example of x1 counts at c2, beside c2's own pass", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.commit("x1", "c2", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[0].Commit, c.Results[1] = "x1", plantedOn(c.Results[1], "x1")
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "the pass and the caught failing example at x9, a commit that the history does not hold", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[0].Commit, c.Results[1] = "x9", plantedOn(c.Results[1], "x9")
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "v-citations 4: CI refused the check at the subject", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Uncited = map[string]map[string]string{"c2": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." The
		// own results of the refused commit that support true, a pass and a caught failing example,
		// count nowhere, a failing example's commit too, the commit that its fault was planted on for a
		// program check of the code, and for a program check of production the commit of its test; the
		// results of another commit stay.
		{name: "R48, a refusal at the commit of the caught failing example alone takes out that example at c2, where that commit's failing examples count", change: func(c *checkCase) {
			c.commit("x1", "c1", "docs/x.md")
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[1] = plantedOn(c.Results[1], "x1")
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
			c.Uncited = map[string]map[string]string{"x1": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "control: a refusal at the fault's own commit, which CI tested, takes out nothing: the example counts from the commit that its fault was planted on", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
			c.Uncited = map[string]map[string]string{c.Results[1].Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		// The results of every other commit stay, whatever CI refused elsewhere: a refusal at c1 takes
		// out c1's caught failing example, and one planted on x1, whose tree is c1's outside the tie,
		// counts at c2 beside c2's own pass. Of two refusals at commits whose taken-out results would
		// count at the subject, where no run counts, the state names the least rule by its name, so
		// the order of the results does not choose it.
		{name: "R48, a refusal at c1 takes out c1's caught failing example, and one planted on x1 counts at c2 beside c2's own pass", change: func(c *checkCase) {
			c.commit("x1", "c1", "docs/x.md")
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, caughtFault("x1", checkStateRun.Add(10*time.Minute)), checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
			c.Uncited = map[string]map[string]string{"c1": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "R48, refusals at c1 and at x1, whose caught failing examples count at c2, where no run counts: the least rule by its name", change: refusedAtC1AndX1(false),
			want: CheckState{State: "unknown", Reason: "uncited", Detail: "test-did-not-run"}},
		{name: "R48, the same with the results in the other order", change: refusedAtC1AndX1(true),
			want: CheckState{State: "unknown", Reason: "uncited", Detail: "test-did-not-run"}},
		// R9 and the plan's t-graph-3: a failing example of a program check of the code counts from
		// the commit that its fault was planted on, "never at the commit that CI tested", and a record
		// that does not say where the step planted its fault counts nowhere; as the newest failing
		// example it makes the check unknown, with the reason that names what it lacks, since the
		// latest answer of the step decides (R7). Each row's missed failing example comes after c1's
		// caught one, which alone would make the check true.
		{name: "R9, a later failing example that missed its fault and names no planted_on: unknown (no_planted_on), and not true from c1's caught one", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn = "" }))
		}, want: CheckState{State: "unknown", Reason: "no_planted_on"}},
		{name: "R9, a later failing example that missed its fault and names no fault path: unknown (no_fault_paths)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.FaultPaths = nil }))
		}, want: CheckState{State: "unknown", Reason: "no_fault_paths"}},
		{name: "R9, a later failing example that missed its fault and names its own commit as planted_on: unknown (planted_on_tested_commit)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.Commit = r.PlantedOn }))
		}, want: CheckState{State: "unknown", Reason: "planted_on_tested_commit"}},
		// The same of c1's caught failing example, the newest one: a record that does not say where
		// its fault was planted counts nowhere, whatever its commit.
		{name: "R9, c1's caught failing example names no fault path: unknown (no_fault_paths)", change: func(c *checkCase) { c.Results[1].FaultPaths = nil },
			want: CheckState{State: "unknown", Reason: "no_fault_paths"}},
		{name: "R9, c1's caught failing example names no planted_on: unknown (no_planted_on)", change: func(c *checkCase) { c.Results[1].PlantedOn = "" },
			want: CheckState{State: "unknown", Reason: "no_planted_on"}},
		{name: "R9, c1's caught failing example names no planted_on, and c1 as its commit: unknown (no_planted_on)", change: func(c *checkCase) {
			c.Results[1].PlantedOn, c.Results[1].Commit = "", "c1"
		}, want: CheckState{State: "unknown", Reason: "no_planted_on"}},
		{name: "R48, a refusal at planted_on does not take out a later record with no fault path", change: func(c *checkCase) {
			c.commit("c3", "c1", "docs/x.md")
			c.Results = append(c.Results, laterMissed(func(r *Result) {
				r.PlantedOn, r.Commit, r.FaultPaths = "c3", "f-c3", nil
			}))
			c.Uncited = map[string]map[string]string{"c3": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "no_fault_paths"}},
		{name: "R48, a refusal at planted_on does not take out a later record planted at its tested commit", change: func(c *checkCase) {
			c.commit("c3", "c1", "docs/x.md")
			c.Results = append(c.Results, laterMissed(func(r *Result) {
				r.PlantedOn, r.Commit = "c3", "c3"
			}))
			c.Uncited = map[string]map[string]string{"c3": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "planted_on_tested_commit"}},
		// R48 takes out a failing example by the commit that it counts from. A later record that does
		// not say where its fault was planted counts from no commit, so CI's refusal at its own commit,
		// which CI tested, leaves its reason.
		{name: "R48, a refusal at its own commit does not take out a later record with no planted_on", change: func(c *checkCase) {
			later := laterMissed(func(r *Result) { r.PlantedOn = "" })
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "no_planted_on"}},
		{name: "R48, a refusal at its own commit does not take out a later record with no fault path", change: func(c *checkCase) {
			later := laterMissed(func(r *Result) { r.FaultPaths = nil })
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "no_fault_paths"}},
		{name: "R48, a refusal at its own commit does not take out a later record with a blank fault path", change: func(c *checkCase) {
			later := laterMissed(func(r *Result) { r.FaultPaths = append(r.FaultPaths, " ") })
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "blank_fault_path"}},
		{name: "R48, a refusal at its own commit does not take out a later record planted at that commit", change: func(c *checkCase) {
			later := laterMissed(func(r *Result) { r.PlantedOn = r.Commit })
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "planted_on_tested_commit"}},
		{name: "control: a refusal at its own commit does not take out a later missed record planted on c1", change: func(c *checkCase) {
			later := laterMissed(func(*Result) {})
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "failing_example_missed"}},
		// A missed failing example whose planted_on names no commit of the history cannot be placed,
		// so it cannot let an older caught one make the check true.
		{name: "R9, a later missed failing example planted on c9, no commit of the history", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn = "c9" }))
		}, want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "c9"}},
		{name: "R9, a later missed failing example planted on f-c1, no commit of the history", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn = "f-c1" }))
		}, want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "f-c1"}},
		// Such a record counts from no commit, so CI's refusal at the commit that its planted_on names,
		// or at its own commit, which CI tested, does not take it out, and c1's caught one does not
		// decide.
		{name: "R48, a refusal at planted_on does not take out a later missed record planted on c9, no commit of the history", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn = "c9" }))
			c.Uncited = map[string]map[string]string{"c9": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "c9"}},
		{name: "R48, a refusal at planted_on does not take out a later missed record planted on f-c1, no commit of the history", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn = "f-c1" }))
			c.Uncited = map[string]map[string]string{"f-c1": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "f-c1"}},
		{name: "R48, a refusal at its own commit does not take out a later missed record planted on c9, no commit of the history", change: func(c *checkCase) {
			later := laterMissed(func(r *Result) { r.PlantedOn = "c9" })
			c.Results = append(c.Results, later)
			c.Uncited = map[string]map[string]string{later.Commit: {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "c9"}},
		// The rule that replaces The Spec's R8 makes one exception to "A result counts only at the
		// commit that it read": "A failing example that a check's test caught". So a failing example
		// that missed its fault counts only at the commit that it counts from, and a missed record
		// planted on c3 says nothing at c1, with CI's refusal at c3 or without it, whatever changed
		// between them.
		{name: "R48 and the rule that replaces The Spec's R8, a refusal at planted_on and a later missed record planted on c3, where no file of the tie changed: the missed record counts nowhere at c1, true from c1", change: func(c *checkCase) {
			c.commit("c3", "c1", "docs/x.md")
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn, r.Commit = "c3", "f-c3" }))
			c.Uncited = map[string]map[string]string{"c3": {c.check: "test-not-in-code"}}
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "the rule that replaces The Spec's R8, a later missed failing example planted on c3, where no file of the tie changed, counts nowhere at c1: true from c1", change: func(c *checkCase) {
			c.commit("c3", "c1", "docs/x.md")
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.PlantedOn, r.Commit = "c3", "f-c3" }))
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "control: the later failing example that missed its fault, planted on c1 with its path: unknown (failing_example_missed)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(*Result) {}))
		}, want: CheckState{State: "unknown", Reason: "failing_example_missed"}},
		{name: "the rule that replaces The Spec's R8, a later failing example planted on c1 that missed its fault counts nowhere at c2, where no file of the tie changed: true from c2's own pass", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, laterMissed(func(*Result) {}), checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		// A fault path that is empty or only white space is no path: a record whose paths are all
		// such reads as one with no path, and one with such a path beside a real one reads unknown with
		// a reason of its own, and is neither left out nor counted.
		{name: "R9, a later failing example that missed its fault and whose one fault path is a space: unknown (no_fault_paths), and not true from c1's caught one", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.FaultPaths = []string{" "} }))
		}, want: CheckState{State: "unknown", Reason: "no_fault_paths"}},
		{name: "R9, a later failing example that missed its fault and whose fault paths are its source file and a tab: unknown (blank_fault_path), and not true from c1's caught one", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.FaultPaths = append(r.FaultPaths, "\t") }))
		}, want: CheckState{State: "unknown", Reason: "blank_fault_path"}},
		{name: "R9, a later failing example that missed its fault and whose fault paths are a space and its source file: unknown (blank_fault_path)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.FaultPaths = append([]string{" "}, r.FaultPaths...) }))
		}, want: CheckState{State: "unknown", Reason: "blank_fault_path"}},
		{name: "control: a later failing example that missed its fault, whose fault path is a source file of the tie with a space in its name: unknown (failing_example_missed)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.FaultPaths = []string{"packages/envoy/cmd/listener/main two.go"} }))
		}, want: CheckState{State: "unknown", Reason: "failing_example_missed"}},
		{name: "control: an earlier failing example that names no planted_on, and c1's caught one after it: true", change: func(c *checkCase) {
			unplanted := c.Results[1]
			unplanted.PlantedOn, unplanted.At = "", checkStateRun.Add(time.Minute)
			c.Results = slices.Insert(c.Results, 1, unplanted)
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		// R7: "Of several results that count, the latest counts." The order of record is the time
		// of a result, then its seq: of two failing examples at one time, the one recorded later
		// decides.
		{name: "R7, a later failing example that missed its fault, at the time of c1's caught one and recorded after it: unknown (failing_example_missed)", change: func(c *checkCase) {
			c.Results = append(c.Results, laterMissed(func(r *Result) { r.At = c.Results[1].At }))
		}, want: CheckState{State: "unknown", Reason: "failing_example_missed"}},
		{name: "control: the same failing example recorded before c1's caught one: true", change: func(c *checkCase) {
			c.Results = slices.Insert(c.Results, 1, laterMissed(func(r *Result) { r.At = c.Results[1].At }))
		}, want: CheckState{State: "true", Reason: "passed", From: "c1"}},
		{name: "R9, c1's caught failing example names c1, which CI tested, as planted_on: unknown (planted_on_tested_commit) at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results[1].Commit = "c1"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "planted_on_tested_commit"}},
		{name: "R9, the subject is the fault's own commit, which CI tested, with the tree and the ties of c1, where the fault was planted: unknown (no_failing_example)", change: func(c *checkCase) {
			c.commit(c.Results[1].Commit, "c1")
			c.Subject.Commit = c.Results[1].Commit
			c.Results = append(c.Results, checkRun(c.Results[1].Commit, OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "control: a subject with the tree and the ties of c1 at another commit than the fault's: true from its own pass", change: func(c *checkCase) {
			c.commit("c2", "c1")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "R48, a refusal of a program check of production at the commit of the test that caught its fault takes out that example", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "no_failing_example"}},
		{name: "R48, a program check of production that CI refused at the commit of its subject", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
			c.Uncited = map[string]map[string]string{"c2": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		// The runs of a program check of production or of the graph count at the commit of their
		// test as its failing examples do, so a refusal there takes them out too, at c2, where the
		// results of c1 count.
		{name: "control: a program check of production whose pass ran the test of c1, which counts at c2", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c1", "c2")
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "true", Reason: "passed", From: "d2"}},
		{name: "R48, a refusal of a program check of production at the commit of the test of its pass takes out that run", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c1", "c2")
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "control: a program check of the graph whose pass ran the test of c1, which counts at c2", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c1", "c2")
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "true", Reason: "passed", From: "tx1"}},
		{name: "R48, a refusal of a program check of the graph at the commit of the test of its pass takes out that run", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c1", "c2")
			c.Uncited = map[string]map[string]string{"c1": {checkStateGraphProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		// With its pass of the test of c2, the failing example that the test of c1 caught decides:
		// a refusal at c1 takes that example out at c2, as it does for a program check of production.
		{name: "control: a program check of the graph whose pass ran the test of c2 and whose failing example the test of c1 caught", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c2", "c1")
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "true", Reason: "passed", From: "tx1"}},
		{name: "R48, a refusal of a program check of the graph at the commit of the test that caught its fault takes out that example", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c2", "c1")
			c.Uncited = map[string]map[string]string{"c1": {checkStateGraphProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "no_failing_example"}},
		// R48 bounds a refusal to its commit: "At that commit, such a check reads unknown, and no
		// earlier result of it counts." At another subject R7 decides ("a later failure cancels an
		// earlier pass"), and R9's unknown cases read "never as true". So a refusal takes out of the
		// results of its commit only what supports true, a pass and a failing example that caught its
		// fault, and keeps a failure, a run that could not run and a failing example that missed its
		// fault wherever they count. Each row: Q or its like of the graph, at subject c2 under deploy
		// d2 at tx1, with a pass of the test of c2 and a failing example that the test of c2 caught,
		// and the row's newer result of the test of c1, where CI refused the check.
		{name: "R48, a refusal of a program check of production at the commit of the test of a newer failure keeps that failure", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateProductionProgram, ResultRun, OutcomeFail, "c1", 2*time.Hour))
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "false", Reason: "failed", From: "d2"}},
		{name: "R48, a refusal of a program check of the graph at the commit of the test of a newer failure keeps that failure", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateGraphProgram, ResultRun, OutcomeFail, "c1", 2*time.Hour))
			c.Uncited = map[string]map[string]string{"c1": {checkStateGraphProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "R48, a refusal of a program check of production at the commit of the test of a newer run that could not run keeps that run", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateProductionProgram, ResultRun, OutcomeCouldNotRun, "c1", 2*time.Hour))
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "could_not_run"}},
		{name: "R48, a refusal of a program check of the graph at the commit of the test of a newer run that could not run keeps that run", change: func(c *checkCase) {
			c.programPass(checkStateGraphProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateGraphProgram, ResultRun, OutcomeCouldNotRun, "c1", 2*time.Hour))
			c.Uncited = map[string]map[string]string{"c1": {checkStateGraphProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "could_not_run"}},
		// A failing example of the test of c1 that missed its fault counts only at c1 (the rule that
		// replaces The Spec's R8 carries a caught one alone), so at c2 it says nothing, with CI's
		// refusal at c1 as without it, and the failing example that the test of c2 caught decides.
		{name: "R48 and the rule that replaces The Spec's R8, a refusal of a program check of production at the commit of the test of a newer failing example that missed its fault: that example counts nowhere at c2, true from d2", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateProductionProgram, ResultFailingExample, OutcomePass, "c1", 70*time.Minute))
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "true", Reason: "passed", From: "d2"}},
		{name: "control: a refusal of a program check of production at the commit of the test of a newer pass takes out that pass, and the older failure decides", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c1", "c2")
			c.Results = append(c.Results, programResult(checkStateProductionProgram, ResultRun, OutcomeFail, "c2", 30*time.Minute))
			c.Uncited = map[string]map[string]string{"c1": {checkStateProductionProgram.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "false", Reason: "failed", From: "d2"}},
		{name: "control: with no refusal, the newer failure of the test of c1 decides", change: func(c *checkCase) {
			c.programPass(checkStateProductionProgram, "c2", "c2")
			c.Results = append(c.Results, programResult(checkStateProductionProgram, ResultRun, OutcomeFail, "c1", 2*time.Hour))
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "false", Reason: "failed", From: "d2"}},
		// uncited at a subject names a refusal that took out a result that would count there (step
		// 2). A pass of the test of c1 that read the deploy d1, or the transaction tx2, never counts
		// at the subject under d2 at tx1, so CI's refusal at c1 takes out no result that counts
		// there, and the run of the job that ended at c2 with no result that counts reads ci_fault
		// (R9), with the refusal as without it.
		{name: "R48, a refusal of a program check of production at the commit of the test of a pass under another deploy: the job that ended at c2 reads ci_fault",
			change: passReadAnotherSubject(checkStateProductionProgram, true), want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "ci_fault"}},
		{name: "control: with no refusal, a pass of a program check of production under another deploy counts nowhere, and the job that ended at c2 reads ci_fault",
			change: passReadAnotherSubject(checkStateProductionProgram, false), want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "ci_fault"}},
		{name: "R48, a refusal of a program check of the graph at the commit of the test of a pass at another transaction: the job that ended at c2 reads ci_fault",
			change: passReadAnotherSubject(checkStateGraphProgram, true), want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "ci_fault"}},
		{name: "control: with no refusal, a pass of a program check of the graph at another transaction counts nowhere, and the job that ended at c2 reads ci_fault",
			change: passReadAnotherSubject(checkStateGraphProgram, false), want: CheckState{ID: checkStateGraphProgram.ID, State: "unknown", Reason: "ci_fault"}},
		// The same of a failing example of a program check of the code: it counts never at a
		// subject at its own commit, which CI tested (step 7), so at the fault's own commit, with the
		// tree and the ties of c1, CI's refusal at c1 takes out no failing example that counts there,
		// and the run of the job that ended there with no result that counts reads ci_fault (R9),
		// with the refusal as without it.
		{name: "R48, a refusal at c1 at the fault's own commit, where c1's caught failing example counts nowhere: the job that ended there reads ci_fault",
			change: atTheFaultsOwnCommit(true), want: CheckState{State: "unknown", Reason: "ci_fault"}},
		{name: "control: with no refusal, c1's caught failing example counts nowhere at the fault's own commit, and the job that ended there reads ci_fault",
			change: atTheFaultsOwnCommit(false), want: CheckState{State: "unknown", Reason: "ci_fault"}},
		// An agent or a query of the code counts its runs at their own commit alone, and its failing
		// examples also at another commit, by C's rule for files (countsAtACommit): a refusal at c1
		// takes out its caught failing example at c2, where it counts. With its pass at c1, no run
		// counts at c2, so it reads subject_moved with no refusal, and uncited with the refusal at
		// c1, which took out a result that would count at c2; with its pass at c2 the failing example
		// of c1 decides, and the refusal takes out that example.
		{name: "control: an agent of the code whose pass at c1 does not count at c2", change: func(c *checkCase) {
			c.codeResults(checkStateCodeAgent, "c1")
		}, want: CheckState{ID: checkStateCodeAgent.ID, State: "unknown", Reason: "subject_moved"}},
		{name: "R48, a refusal of an agent of the code at c1, the commit of its results, takes out its failing example, which counts at c2, where no run counts", change: func(c *checkCase) {
			c.codeResults(checkStateCodeAgent, "c1")
			c.Uncited = map[string]map[string]string{"c1": {checkStateCodeAgent.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateCodeAgent.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "control: an agent of the code whose pass at c2 and failing example at c1 count at c2", change: func(c *checkCase) {
			c.codeResults(checkStateCodeAgent, "c2")
		}, want: CheckState{ID: checkStateCodeAgent.ID, State: "true", Reason: "passed", From: "c2"}},
		{name: "R48, a refusal of an agent of the code at c1, the commit of its failing example, takes that example out at c2, where it counts", change: func(c *checkCase) {
			c.codeResults(checkStateCodeAgent, "c2")
			c.Uncited = map[string]map[string]string{"c1": {checkStateCodeAgent.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateCodeAgent.ID, State: "unknown", Reason: "no_failing_example"}},
		{name: "control: a query of the code whose pass at c1 does not count at c2", change: func(c *checkCase) {
			c.codeResults(checkStateCodeQuery, "c1")
		}, want: CheckState{ID: checkStateCodeQuery.ID, State: "unknown", Reason: "subject_moved"}},
		{name: "R48, a refusal of a query of the code at c1, the commit of its results, takes out its failing example, which counts at c2, where no run counts", change: func(c *checkCase) {
			c.codeResults(checkStateCodeQuery, "c1")
			c.Uncited = map[string]map[string]string{"c1": {checkStateCodeQuery.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateCodeQuery.ID, State: "unknown", Reason: "uncited", Detail: "test-not-in-code"}},
		{name: "control: a query of the code whose pass at c2 and failing example at c1 count at c2", change: func(c *checkCase) {
			c.codeResults(checkStateCodeQuery, "c2")
		}, want: CheckState{ID: checkStateCodeQuery.ID, State: "true", Reason: "passed", From: "c2"}},
		{name: "R48, a refusal of a query of the code at c1, the commit of its failing example, takes that example out at c2, where it counts", change: func(c *checkCase) {
			c.codeResults(checkStateCodeQuery, "c2")
			c.Uncited = map[string]map[string]string{"c1": {checkStateCodeQuery.ID: "test-not-in-code"}}
		}, want: CheckState{ID: checkStateCodeQuery.ID, State: "unknown", Reason: "no_failing_example"}},
		{name: "R9, the job ended with no result", change: func(c *checkCase) {
			c.commit("c2", "c1", "packages/envoy/cmd/listener/main.go")
			c.Subject.Commit = "c2"
			c.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: "c2", Ended: true}}
		}, want: CheckState{State: "unknown", Reason: "ci_fault"}},
		{name: "a failure at c2 decides there, and c1's pass does not count at c2", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomeFail, checkStateRun.Add(time.Hour)))
		}, want: CheckState{State: "false", Reason: "failed", From: "c2"}},
		{name: "a failure at c2 and a later second attempt that passes at c1 do not count at c3", change: func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.commit("c3", "c1", "docs/x.md")
			c.Subject.Commit = "c3"
			c.Results = append(c.Results,
				checkRun("c2", OutcomeFail, checkStateRun.Add(time.Hour)), legRun("c1", "", 2, OutcomePass, checkStateRun.Add(2*time.Hour)))
		}, want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "a failure at c2 does not count at c3, which passes after a fix to a file of the tie", change: func(c *checkCase) {
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
		// R48: "At that commit, such a check reads unknown, and no earlier result of it counts." For
		// a query or an agent of production or of the graph too, CI's refusal at S.Commit counts, and
		// one at another commit does not (step 2).
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
		// A result of a query or an agent of production or of the graph belongs to no commit, so a
		// refusal at the commit that its test_commit names takes out nothing of it, and R7 decides: "a
		// later failure cancels an earlier pass", and a newest failing example that missed its fault
		// reads unknown. Each for a query and for an agent of production.
		{name: "R48, a query of production whose newest run fails and names test_commit c1, where CI refused the query",
			change: newestRunFailsAtC1(checkStateProduction), want: CheckState{ID: checkStateProduction.ID, State: "false", Reason: "failed", From: "d2"}},
		{name: "R48, a query of production whose newest failing example missed its fault and names test_commit c1, where CI refused the query",
			change: newestFaultMissedAtC1(checkStateProduction), want: CheckState{ID: checkStateProduction.ID, State: "unknown", Reason: "failing_example_missed"}},
		{name: "R48, an agent of production whose newest run fails and names test_commit c1, where CI refused the agent",
			change: newestRunFailsAtC1(checkStateProductionAgent), want: CheckState{ID: checkStateProductionAgent.ID, State: "false", Reason: "failed", From: "d2"}},
		{name: "R48, an agent of production whose newest failing example missed its fault and names test_commit c1, where CI refused the agent",
			change: newestFaultMissedAtC1(checkStateProductionAgent), want: CheckState{ID: checkStateProductionAgent.ID, State: "unknown", Reason: "failing_example_missed"}},
		{name: "R48, a query of the graph whose newest run fails and names test_commit c1, where CI refused the query",
			change: newestGraphRunFailsAtC1(checkStateGraph), want: CheckState{ID: checkStateGraph.ID, State: "unknown", Reason: "disagree", Disagree: true}},
		{name: "R48, an agent of the graph whose newest run fails and names test_commit c1, where CI refused the agent",
			change: newestGraphRunFailsAtC1(checkStateGraphAgent), want: CheckState{ID: checkStateGraphAgent.ID, State: "unknown", Reason: "disagree", Disagree: true}},
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
		{name: "a program check of production whose test changed after it caught the fault", change: func(c *checkCase) {
			c.productionProgram("packages/envoy/cmd/listener/restart_test.go", "c1")
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "unknown", Reason: "no_failing_example"}},
		{name: "a program check of production whose test did not change after it caught the fault", change: func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
		}, want: CheckState{ID: checkStateProductionProgram.ID, State: "true", Reason: "passed", From: "d2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			row.change(s)
			states := checkStates(t, s.input())
			if !slices.IsSortedFunc(states, func(a, b CheckState) int { return cmp.Compare(a.ID, b.ID) }) {
				t.Errorf("deriveChecks is not sorted by id: %+v", states)
			}
			want := row.want
			if want.ID == "" {
				want.ID = checkStateCheck.ID
			}
			i := slices.IndexFunc(states, func(s CheckState) bool { return s.ID == want.ID })
			switch {
			case row.absent && i >= 0:
				t.Errorf("deriveChecks holds %+v, want no state of %s", states[i], want.ID)
			case row.absent:
			case i < 0:
				t.Errorf("deriveChecks = %+v, want a state of %s", states, want.ID)
			case states[i] != want:
				t.Errorf("deriveChecks:\n got %+v\nwant %+v", states[i], want)
			}
		})
	}
}

// atTheFaultsOwnCommit is the change of a row of TestCheckState: subject the commit of c1's caught
// failing example, which CI tested, with the tree and the ties of c1, where the fault was planted,
// so that the example counts nowhere there; no run of C at the subject; a run of C's job that ended
// there with no result; and, with refused, CI's refusal of C at c1.
func atTheFaultsOwnCommit(refused bool) func(c *checkCase) {
	return func(c *checkCase) {
		c.commit(c.Results[1].Commit, "c1")
		c.Subject.Commit = c.Results[1].Commit
		c.JobRuns = []JobRun{{Job: "go-checks", Leg: "", Commit: c.Subject.Commit, Ended: true}}
		if refused {
			c.Uncited = map[string]map[string]string{"c1": {c.check: "test-not-in-code"}}
		}
	}
}

// refusedAtC1AndX1 is the change of a row of TestCheckState: subject c2, whose tree is c1's outside
// the tie, where no run of C counts; c1's caught failing example and one planted on x1, whose tree
// is c1's outside the tie too, after it, each of which counts at c2; and CI's refusals of C at c1
// (test-not-in-code) and at x1 (test-did-not-run). With reversed, the results in the other order.
func refusedAtC1AndX1(reversed bool) func(c *checkCase) {
	return func(c *checkCase) {
		c.commit("x1", "c1", "docs/x.md")
		c.commit("c2", "c1", "docs/x.md")
		c.Subject.Commit = "c2"
		c.Results = append(c.Results, caughtFault("x1", checkStateRun.Add(10*time.Minute)))
		if reversed {
			slices.Reverse(c.Results)
		}
		c.Uncited = map[string]map[string]string{"c1": {c.check: "test-not-in-code"}, "x1": {c.check: "test-did-not-run"}}
	}
}

// The files and the approved spec decide which checks deriveChecks judges: each check that the
// approved spec holds a current text of, in the order of its id. A check whose current approved text
// the files do not hold reads not built, whatever its results, also when they hold an earlier
// approved text of it; a check the files hold that the approved spec never held has no state here,
// since the comparison of R2 refuses it.
func TestDeriveChecksJudgesTheApprovedChecks(t *testing.T) {
	t.Parallel()
	s := newCheckCase(t)
	s.add(checkStateGraph)
	s.files["spec.md"] = append(s.files["spec.md"], checkStateProduction) // in the files, never approved
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
		t.Errorf("deriveChecks:\n got %+v\nwant %+v", states, want)
	}
}

// R9: "No setting makes the failing example optional", and a check "whose current text has no
// failing example that failed when it should" reads unknown. The rules of the tree and the
// derivation read a missing failing example by one rule: Validate refuses the check with
// no-failing-example exactly when deriveChecks reads it unknown, no_failing_example, whatever its
// results. Each row is the base of TestCheckState, a pass and a caught fault at c1, with the failing
// example of C's text, in the files and in the approved spec, replaced by the row's text.
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
			refused := slices.ContainsFunc(Validate(s.parse(), Options{}), func(p Problem) bool {
				return p.Node == c.ID && p.Rule == "no-failing-example"
			})
			if refused != row.missing {
				t.Errorf("Validate refuses the failing example %q with no-failing-example: %t, want %t", row.text, refused, row.missing)
			}
			want := CheckState{ID: c.ID, State: "true", Reason: "passed", From: "c1"}
			if row.missing {
				want = CheckState{ID: c.ID, State: "unknown", Reason: "no_failing_example"}
			}
			if states := checkStates(t, s.input()); !slices.Contains(states, want) {
				t.Errorf("deriveChecks with the failing example %q:\n got %+v\nwant %+v", row.text, states, want)
			}
		})
	}
}
