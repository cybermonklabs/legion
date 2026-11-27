package specgraph

import (
	"slices"
	"testing"
	"time"
)

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
			s.programBase(row.check, row.changed)
			s.Results = append(s.Results, programResult(row.check, ResultRun, OutcomePass, row.runCommit, time.Hour),
				programResult(row.check, ResultFailingExample, OutcomeFail, "c2", 65*time.Minute))
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
// results of the test at c2 count there (the test of c2 is the test of c3, deriveChecks step 4); the
// runs of legs linux and arm of Q's job ended at c2; the row's runs under d2 at the test of c2, and a
// failing example that the test at c2 caught; C taken out. A leg that ended with no result at c2
// counts wherever the results of the test of c2 count.
func TestEachLegOfAProgramCheckOfProductionKeepsItsResult(t *testing.T) {
	t.Parallel()
	q := checkStateProductionProgram
	run := func(leg string, outcome Outcome, after time.Duration) Result {
		return Result{Check: q.ID, Kind: ResultRun, Outcome: outcome, Deploy: "d2", Job: q.Test.Job, Leg: leg, TestCommit: "c2", At: checkStateRun.Add(after)}
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
			s.JobRuns = []JobRun{{Job: q.Test.Job, Leg: "linux", Commit: "c2", Ended: true}, {Job: q.Test.Job, Leg: "arm", Commit: "c2", Ended: true}}
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

// programBase is the base of programPass, passReadAnotherSubject and
// TestARunOfProductionOrTheGraphCountsForTheCodeThatRanIt: the program check q of production or of
// the graph, with C's tie, and subject c2 under deploy d2 at tx1, where c2 changes changed from c1.
func (s *checkCase) programBase(q Check, changed string) {
	s.t.Helper()
	s.add(q)
	s.tieLikeC(q.ID)
	s.commit("c2", "c1", changed)
	s.Subject = Point{Commit: "c2", Deploy: "d2", GraphTx: "tx1"}
}

// programPass is programBase, where c2 differs from c1 outside the tie, with a pass of q's test at
// runCommit, under deploy d2 or at the transaction tx1, and a failing example that its test at
// faultCommit caught.
func (s *checkCase) programPass(q Check, runCommit, faultCommit string) {
	s.t.Helper()
	s.programBase(q, "docs/x.md")
	s.Results = append(s.Results, programResult(q, ResultRun, OutcomePass, runCommit, time.Hour),
		programResult(q, ResultFailingExample, OutcomeFail, faultCommit, 65*time.Minute))
}

// programResult is a result of the program check q of production or of the graph, of kind with
// outcome, of its test at testCommit, recorded after after checkStateRun: a run under deploy d2 or
// at the transaction tx1, reported by q's job, or a failing example of author-b's fault in
// author-a's test, as programPass's results are.
func programResult(q Check, kind ResultKind, outcome Outcome, testCommit string, after time.Duration) Result {
	r := Result{Check: q.ID, Kind: kind, Outcome: outcome, TestCommit: testCommit, At: checkStateRun.Add(after)}
	if kind == ResultFailingExample {
		r.FaultAuthor, r.TestAuthors = "author-b", []string{"author-a"}
		return r
	}
	r.Job = q.Test.Job
	return readAt(q, r, "d2", "tx1")
}

// readAt is the run r of q with what it read: the transaction graphTx for a check of the graph, the
// deploy for a check of production.
func readAt(q Check, r Result, deploy, graphTx string) Result {
	if q.Subject == SubjectGraph {
		r.GraphTx = graphTx
	} else {
		r.Deploy = deploy
	}
	return r
}

// passReadAnotherSubject is the change of a row of TestCheckState: programBase with one pass of q's
// test at c1 that read the deploy d1 or the transaction tx2, so the test of the pass is the test at
// c2 and what the pass read is not the subject; a run of q's job that ended at c2; and, with
// refused, CI's refusal of q at c1.
func passReadAnotherSubject(q Check, refused bool) func(c *checkCase) {
	return func(c *checkCase) {
		c.programBase(q, "docs/x.md")
		c.Results = append(c.Results, readAt(q, programResult(q, ResultRun, OutcomePass, "c1", time.Hour), "d1", "tx2"))
		c.JobRuns = []JobRun{{Job: q.Test.Job, Commit: "c2", Ended: true}}
		if refused {
			c.Uncited = map[string]map[string]string{"c1": {q.ID: "test-not-in-code"}}
		}
	}
}

// productionRun is a run of a production check under deploy, recorded at at.
func productionRun(c Check, deploy string, outcome Outcome, at time.Time) Result {
	r := Result{Check: c.ID, Kind: ResultRun, Outcome: outcome, Deploy: deploy, At: at}
	if c.Test != nil {
		r.Job = c.Test.Job
	}
	return r
}

// newestRunFailsAtC1 is the change of a row of TestCheckState for p, a query or an agent of
// production: subject c9 under deploy d2; a pass of p under d2, a failure there an hour later that
// names test_commit c1, and a caught failing example; and CI's refusal of p at c1.
func newestRunFailsAtC1(p Check) func(c *checkCase) {
	return func(c *checkCase) {
		c.add(p)
		c.Subject = Point{Commit: "c9", Deploy: "d2"}
		fail := productionRun(p, "d2", OutcomeFail, checkStateRun.Add(3*time.Hour))
		fail.TestCommit = "c1"
		c.Results = append(c.Results,
			productionRun(p, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)), fail,
			Result{Check: p.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
		c.Uncited = map[string]map[string]string{"c1": {p.ID: "test-not-in-code"}}
	}
}

// newestFaultMissedAtC1 is the change of a row of TestCheckState for p, a query or an agent of
// production: subject c9 under deploy d2; a pass of p under d2, a caught failing example, and a
// newer one that missed its fault and names test_commit c1; and CI's refusal of p at c1.
func newestFaultMissedAtC1(p Check) func(c *checkCase) {
	return func(c *checkCase) {
		c.add(p)
		c.Subject = Point{Commit: "c9", Deploy: "d2"}
		c.Results = append(c.Results,
			productionRun(p, "d2", OutcomePass, checkStateRun.Add(2*time.Hour)),
			Result{Check: p.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)},
			Result{Check: p.ID, Kind: ResultFailingExample, Outcome: OutcomePass, TestCommit: "c1", At: checkStateRun.Add(130 * time.Minute)})
		c.Uncited = map[string]map[string]string{"c1": {p.ID: "test-not-in-code"}}
	}
}

// newestGraphRunFailsAtC1 is the change of a row of TestCheckState for g, a query or an agent of
// the graph: subject c9 under deploy d2 at the transaction tx1; a pass of g at tx1, a failure there
// an hour later that names test_commit c1, and a caught failing example; and CI's refusal of g at
// c1.
func newestGraphRunFailsAtC1(g Check) func(c *checkCase) {
	return func(c *checkCase) {
		c.add(g)
		c.Subject = Point{Commit: "c9", Deploy: "d2", GraphTx: "tx1"}
		c.Results = append(c.Results,
			Result{Check: g.ID, Kind: ResultRun, Outcome: OutcomePass, GraphTx: "tx1", At: checkStateRun.Add(2 * time.Hour)},
			Result{Check: g.ID, Kind: ResultRun, Outcome: OutcomeFail, GraphTx: "tx1", TestCommit: "c1", At: checkStateRun.Add(3 * time.Hour)},
			Result{Check: g.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(125 * time.Minute)})
		c.Uncited = map[string]map[string]string{"c1": {g.ID: "test-not-in-code"}}
	}
}
