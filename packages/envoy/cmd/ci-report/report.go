package main

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// Planned is what the reporter does for one check whose test part names the job: the check's id,
// its test's selector, the outcome, what the test wrote, and why the outcome is what it is when
// that is not plain. Outcome is the test's own pass, fail, skip or could_not_run when the report
// holds one run of the test; absent when it holds none, or when the check's runner writes no go
// test report; two_tests when it holds two or more runs of the test.
type Planned struct {
	Check, Test, Outcome, Observed, Why string
}

// The outcomes of a check that are no outcome of its test.
const (
	outcomeAbsent   = "absent"
	outcomeTwoTests = "two_tests"
)

// notRun is the reason of a check whose test the report holds no run of.
const notRun = "the test did not run at this commit"

// recorded reports whether a check with outcome gets a result: a pass, a failure, or a test that
// could not run. A skip, a test that did not run and a test that ran twice get none (R12: nobody
// writes a result for work that it did not do).
func recorded(outcome string) bool {
	return outcome == "pass" || outcome == "fail" || outcome == "could_not_run"
}

// planReports is what the reporter does for each check of tree that the job runs: each program
// check whose test part names job, in the tree's order, with the outcome of its test in ran, the
// job's go test report as specgraph.ReadGoTestJSON reads it. specgraph.MatchTests decides which
// entries of ran are the check's test, the one rule the reporter and Dispatch's reading of the
// files share. A check of another job is not planned, since only the job its test part names
// writes its result.
func planReports(tree specgraph.Tree, job string, ran []specgraph.TestOutcome) []Planned {
	var planned []Planned
	for _, check := range tree.Checks {
		if check.Procedure != specgraph.ProcedureProgram || check.Test == nil || check.Test.Job != job {
			continue
		}
		p := Planned{Check: check.ID, Test: check.Test.Selector}
		if check.Test.Runner != "go" {
			p.Outcome, p.Why = outcomeAbsent, "the reporter reads go test reports only; this check's runner is "+check.Test.Runner
			planned = append(planned, p)
			continue
		}
		matched, err := specgraph.MatchTests(*check.Test, ran)
		switch {
		case err != nil:
			p.Outcome, p.Why = outcomeAbsent, notRun+": "+err.Error()
		case len(matched) == 0:
			p.Outcome, p.Why = outcomeAbsent, notRun
		case len(matched) > 1:
			p.Outcome = outcomeTwoTests
			p.Why = fmt.Sprintf("the report holds %d runs of the test, and a result is the result of one test", len(matched))
		default:
			p.Outcome, p.Observed = matched[0].Outcome, matched[0].Output
			switch p.Outcome {
			case "skip":
				p.Why = "the test skipped at this commit"
			case "could_not_run":
				p.Why = "the report gives the test no one end: go test stopped it at its time limit, its process or the job ended inside it, or the report gives it two ends"
			}
		}
		planned = append(planned, p)
	}
	return planned
}
