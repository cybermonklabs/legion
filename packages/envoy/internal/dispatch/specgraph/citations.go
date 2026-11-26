package specgraph

import (
	"errors"
	"fmt"
	"slices"
)

// CodeTests finds how many tests a check's test part picks in the code at one commit. GoCode is
// the one for runner go.
type CodeTests interface {
	Find(t Test) (int, error)
}

// JobReport is what the citations job knows of one job of its workflow in one leg of its matrix
// (Leg "" for none): whether the job ran, its result being success or failure; whether it left its
// report; and the tests that its report holds.
type JobReport struct {
	Job, Leg       string
	Ran, HasReport bool
	Tests          []TestOutcome
}

// CitationFault is one check that CI refuses at a commit (R48): the check, where its block is, the
// rule it breaks and a Detail that begins with the rule.
type CitationFault struct {
	Check        string
	Pos          Position
	Rule, Detail string
}

// CitationFaults is the faults of one commit, one for each check that breaks a rule.
type CitationFaults []CitationFault

// The rules of R48, each the Rule of a CitationFault.
const (
	citeNoTest           = "no-test"
	citeTestNotInCode    = "test-not-in-code"
	citeTwoTests         = "two-tests"
	citeTestLookupFailed = "test-lookup-failed"
	citeJobNotInWorkflow = "job-not-in-workflow"
	citeJobNotAwaited    = "job-not-awaited"
	citeTestNotRun       = "test-not-run"
)

// CitationInput is what Citations reads beyond the tree: the code at the commit, the jobs of the
// workflow, the report of each leg of each job of the run, the paths that changed since the base
// commit (AllChanged: the commit has no base commit), and the tie of a check at the commit, Tie,
// and at the base commit, BaseTie. Changed must list the changes since the commit where each
// result that this commit carries stands, which for a change's branch is its merge base with the
// default branch, and never its push before: a list from that push misses the changes of the
// pushes before it. A nil Tie or BaseTie answers a tie that is not known.
type CitationInput struct {
	Code       CodeTests
	Jobs       WorkflowJobs
	Reports    []JobReport
	Changed    []string
	AllChanged bool
	Tie        func(Check) Tie
	BaseTie    func(Check) Tie
}

// Citations finds each program check of t that CI refuses at the commit (R48), in the order of
// t.Checks, with the first rule it breaks of these:
//
//   - no-test: the check names no runner or no selector, or one with no letter and no digit;
//   - test-not-in-code, two-tests: in.Code finds no test or more than one for its test part (R6:
//     "a runner and a selector, which pick one test in the code"), and a test that only a test
//     file under a build constraint declares, which go test with no tags does not build, is
//     test-not-in-code with a detail that names that file (ExcludedTestError);
//     test-lookup-failed: in.Code cannot search for it, as for a package that go cannot list;
//   - job-not-in-workflow, job-not-awaited: in.Jobs has no job of its name, or the citations job
//     does not wait for it and so cannot read its report;
//   - test-not-run: code that the check runs changed and the job did not run its test (R8: "CI
//     runs a check again when that code changed"). Each report of the job, one per leg, must hold
//     the test as a pass or a failure: a job that did not run, left no report, skipped the test,
//     does not hold it, could not run it or replayed it from go test's cache did not run it. The
//     code did not change only when the check's tie is known at the commit and at the base commit
//     and no path of Changed reaches either (changeBetween), the rule by which a result carries
//     from one commit to another (R8): a result carries from the base only while no file that its
//     test read there changed, so a file that the change deleted or moved, which the tie at the
//     commit no longer holds, counts by the base's tie. A path reaches a tie that covers it or that
//     holds a path under it, since git names a submodule that moved by its one path, the gitlink.
//     So it changed when the tie at the commit is not known (R8: "A check whose tie to its code is
//     unknown runs on each push to the branch"), when AllChanged says that the commit has no base
//     commit, when a path of Changed reaches the tie at either end (the first in sorted order
//     named), and when the tie at the base is not known, also while Changed is empty.
//
// A report is the account of the process under test (ReadGoTestJSON), so this rule stands against
// a test that is missing, skipped, filtered out or replayed from go test's cache, and not against
// test code that forges the lines of the testing package: a test that prints the marker byte and
// the run and pass lines of another test makes that test read as run, and that test can be one that
// go test did not run (TestCitationsStatedLimitAForgedRun). What stands against a test that proves
// nothing is its failing example (R9: "An agent that did not write the test writes the fault", and
// "The check reads true only when its test, as it is now, failed on that fault") and the review of
// each change to the code of its test (R25: "A change to the code of a test that runs a check is a
// change to that check").
//
// Deriving a tie can run the repository's own tools, so Citations asks for the tie at the commit
// only for a check whose test did not run, and for the tie at the base only when, besides, the tie
// at the commit is known and no path of Changed reaches it: a tie at the commit that is not known,
// or that a changed path reaches, decides alone, so the answer is the one of asking for both first.
func Citations(t Tree, in CitationInput) CitationFaults {
	var faults CitationFaults
	for _, c := range t.Checks {
		if c.Procedure != ProcedureProgram {
			continue
		}
		if rule, detail := in.cite(c); rule != "" {
			faults = append(faults, CitationFault{Check: c.ID, Pos: c.Pos, Rule: rule, Detail: rule + ": " + detail})
		}
	}
	return faults
}

// cite is the first rule that the program check c breaks and what it names, or "".
func (in CitationInput) cite(c Check) (rule, detail string) {
	if c.Test == nil || !hasWord(c.Test.Runner) || !hasWord(c.Test.Selector) {
		return citeNoTest, "the check names no test"
	}
	test := *c.Test
	var excluded *ExcludedTestError
	switch n, err := in.Code.Find(test); {
	case errors.As(err, &excluded):
		return citeTestNotInCode, fmt.Sprintf("%s is not in the code that go test builds at this commit: a build constraint leaves out %s, which declares it",
			test.Selector, excluded.File)
	case err != nil:
		return citeTestLookupFailed, fmt.Sprintf("%s could not be looked up in the code at this commit (%v)", test.Selector, err)
	case n == 0:
		return citeTestNotInCode, fmt.Sprintf("%s is not in the code at this commit", test.Selector)
	case n > 1:
		return citeTwoTests, fmt.Sprintf("%s picks %d tests", test.Selector, n)
	}
	if !in.Jobs.Jobs[test.Job] {
		return citeJobNotInWorkflow, fmt.Sprintf("no job %q", test.Job)
	}
	if !in.Jobs.Awaited[test.Job] {
		return citeJobNotAwaited, fmt.Sprintf("the citations job does not wait for %q, so it cannot read its report", test.Job)
	}
	// The reports first, since the ties are asked for only when the test did not run.
	job, why := in.notRun(test)
	if why == "" {
		return "", ""
	}
	cause := in.changeCause(c)
	if cause == "" {
		return "", ""
	}
	return citeTestNotRun, fmt.Sprintf("%s, and %s did not run %s (%s)", cause, job, test.Selector, why)
}

// changeCause is why the code that check c runs counts as changed, or "" when it did not change:
// the carry rule (changeBetween, R8), with the cause named. The tie at this commit decides alone
// when it finds a change, so the tie at the base is asked for only when it finds none.
func (in CitationInput) changeCause(c Check) string {
	tie := Tie{Why: "no tie at this commit was given"}
	if in.Tie != nil {
		tie = in.Tie(c)
	}
	changed := func() ([]string, bool) { return in.Changed, !in.AllChanged }
	change := changeBetween(changed, tie)
	if change.kind == noChange {
		base := Tie{Why: "no tie at the base commit was given"}
		if in.BaseTie != nil {
			base = in.BaseTie(c)
		}
		change = changeBetween(changed, tie, base)
	}
	switch {
	case change.kind == tieUnknown && change.tie == 0:
		return fmt.Sprintf("its tie is unknown (%s)", change.why)
	case change.kind == tieUnknown:
		return fmt.Sprintf("its tie at the base commit is unknown (%s)", change.why)
	case change.kind == pathsUnknown:
		return "no base commit"
	case change.kind == pathCovered && change.under != "" && change.tie == 0:
		return fmt.Sprintf("%s changed (the tie holds %s, a path under it)", change.path, change.under)
	case change.kind == pathCovered && change.under != "":
		return fmt.Sprintf("%s changed (the tie at the base commit holds %s, a path under it)", change.path, change.under)
	case change.kind == pathCovered && change.tie == 0:
		return change.path + " changed"
	case change.kind == pathCovered:
		return change.path + " changed (the tie at the base commit holds it)"
	}
	return ""
}

// notRun is the job of test, with the first leg in sorted order whose report does not show test
// run, and why it does not; why is "" when every leg ran test, and "the job did not run" when the
// reports hold no leg of the job.
func (in CitationInput) notRun(test Test) (job, why string) {
	reported := false
	firstLeg := ""
	for _, r := range in.Reports {
		if r.Job != test.Job {
			continue
		}
		reported = true
		if legWhy := notRunInLeg(test, r); legWhy != "" && (why == "" || r.Leg < firstLeg) {
			firstLeg, why = r.Leg, legWhy
		}
	}
	switch {
	case !reported:
		return test.Job, "the job did not run"
	case why == "":
		return "", ""
	case firstLeg != "":
		return fmt.Sprintf("%s (leg %s)", test.Job, firstLeg), why
	}
	return test.Job, why
}

// notRunInLeg is why the report of one leg does not show test run, or "" when it ran: a pass or a
// failure. A runner that MatchTests cannot read (code found the test, so the selector is sound)
// leaves the test in no report that Citations can read. Any run of the test that did not run
// decides, so a report that holds a real run and a cached replay of one test reads as not run: the
// job runs one go test -json -count=1 for each package, so its report holds one run.
func notRunInLeg(test Test, leg JobReport) string {
	switch {
	case !leg.Ran:
		return "the job did not run"
	case !leg.HasReport:
		return "the job left no report"
	}
	matched, err := MatchTests(test, leg.Tests)
	if err != nil || len(matched) == 0 {
		return "its report does not hold it"
	}
	// The first of these outcomes that a run of the test has decides.
	someRun := func(outcome string) bool {
		return slices.ContainsFunc(matched, func(o TestOutcome) bool { return o.Outcome == outcome })
	}
	switch {
	case someRun(string(OutcomeCouldNotRun)):
		return "it could not run"
	case someRun(outcomeCached):
		return "its report holds a result that go test replayed from its cache, which did not run at this commit"
	case someRun("skip"):
		return "its report shows it skipped"
	}
	return ""
}
