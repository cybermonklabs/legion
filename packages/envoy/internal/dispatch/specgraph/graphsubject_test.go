package specgraph

import (
	"slices"
	"testing"
	"time"
)

// R7: "A subject that cannot change, such as a commit, holds one answer. A pass and a failure on one
// such subject and one procedure make the state unknown." R13: a query "reads the graph at one
// transaction", and one transaction is such a subject. A pass and a failure at one transaction read
// unknown in either order, and a later pass does not end the report, while a newer transaction
// starts a new answer. Each row: G at subject c1 under deploy d2 and at the transaction of the row's
// last run, the row's runs, and a caught failing example; C taken out.
func TestOneTransactionOfTheGraphHoldsOneAnswer(t *testing.T) {
	t.Parallel()
	g := checkStateGraph
	run := func(tx string, outcome Outcome, after time.Duration) Result {
		return Result{Check: g.ID, Kind: ResultRun, Outcome: outcome, GraphTx: tx, At: checkStateRun.Add(after)}
	}
	for _, row := range []struct {
		name       string
		runs       []Result
		want       CheckState
		statements string
	}{
		{"control: a failure at tx1, then a pass at tx2", []Result{run("tx1", OutcomeFail, time.Hour), run("tx2", OutcomePass, 2*time.Hour)},
			CheckState{State: "true", Reason: "passed", From: "tx2"}, "met"},
		{"a failure at tx1, then a pass at tx1", []Result{run("tx1", OutcomeFail, time.Hour), run("tx1", OutcomePass, 2*time.Hour)},
			CheckState{State: "unknown", Reason: "disagree", Disagree: true}, "unknown"},
		{"a pass at tx1, then a failure at tx1", []Result{run("tx1", OutcomePass, time.Hour), run("tx1", OutcomeFail, 2*time.Hour)},
			CheckState{State: "unknown", Reason: "disagree", Disagree: true}, "unknown"},
		{"control: a pass at tx1, then a failure at tx2", []Result{run("tx1", OutcomePass, time.Hour), run("tx2", OutcomeFail, 2*time.Hour)},
			CheckState{State: "false", Reason: "failed", From: "tx2"}, "not_met"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			s.add(g)
			s.Subject = Point{Commit: "c1", Deploy: "d2", GraphTx: row.runs[len(row.runs)-1].GraphTx}
			s.Results = append(append(s.Results, row.runs...),
				Result{Check: g.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, At: checkStateRun.Add(5 * time.Minute)})
			want := row.want
			want.ID = g.ID
			deriveAlone(t, s, want, row.statements)
		})
	}
}

// R13: a query "reads the graph at one transaction". R7: "A result counts only for the procedure
// that ran it and for the subject that it read." The subject names its transaction of the graph, as
// it names its deploy, and a result of a graph check counts only at that transaction: a result at
// another one is of a subject that moved, whichever transaction is newer or was recorded last. R10:
// "A check whose subject is production decides nothing for the head of a branch", and the graph is
// production's data: a check of the graph has a state only at a subject that names a deploy, and
// there it reads unknown when the subject names no transaction or one that no result read (R10:
// "Unknown: every other case"). Each row: the row's check of the graph, its runs and a failing
// example that the test at c2 caught, at subject c2 under deploy d2 (or at the head of the row's
// branch) and the row's transaction, where c2 differs from c1 in the test of the program check G2;
// C taken out.
func TestAGraphCheckReadsTheTransactionOfTheSubject(t *testing.T) {
	t.Parallel()
	g, g2 := checkStateGraph, checkStateGraphProgram
	run := func(c Check, tx, testCommit string, outcome Outcome, after time.Duration) Result {
		r := Result{Check: c.ID, Kind: ResultRun, Outcome: outcome, GraphTx: tx, TestCommit: testCommit, At: checkStateRun.Add(after)}
		if c.Test != nil {
			r.Job = c.Test.Job
		}
		return r
	}
	for _, row := range []struct {
		name       string
		check      Check
		branch     string // the branch of the subject, which then names no deploy; "" for deploy d2
		tx         string // the transaction of the subject
		runs       []Result
		want       CheckState // ID "" is the row's check
		statements string
		absent     bool // the check has no state at the subject
	}{
		{name: "control: a pass at tx1, the transaction of the subject", check: g, tx: "tx1",
			runs: []Result{run(g, "tx1", "", OutcomePass, time.Hour)},
			want: CheckState{State: "true", Reason: "passed", From: "tx1"}, statements: "met"},
		{name: "a failure at tx2, the transaction of the subject, then a pass at tx1 recorded later", check: g, tx: "tx2",
			runs: []Result{run(g, "tx2", "", OutcomeFail, time.Hour), run(g, "tx1", "", OutcomePass, 2*time.Hour)},
			want: CheckState{State: "false", Reason: "failed", From: "tx2"}, statements: "not_met"},
		{name: "the same runs at subject tx3, which no result read", check: g, tx: "tx3",
			runs: []Result{run(g, "tx2", "", OutcomeFail, time.Hour), run(g, "tx1", "", OutcomePass, 2*time.Hour)},
			want: CheckState{State: "unknown", Reason: "subject_moved"}, statements: "unknown"},
		{name: "the test of c2 passes at tx1, then the test of c1 fails at tx2, the transaction of the subject", check: g2, tx: "tx2",
			runs: []Result{run(g2, "tx1", "c2", OutcomePass, time.Hour), run(g2, "tx2", "c1", OutcomeFail, 2*time.Hour)},
			want: CheckState{State: "unknown", Reason: "subject_moved"}, statements: "unknown"},
		{name: "control: the test of c2 passes at tx1, then fails at tx2, the transaction of the subject", check: g2, tx: "tx2",
			runs: []Result{run(g2, "tx1", "c2", OutcomePass, time.Hour), run(g2, "tx2", "c2", OutcomeFail, 2*time.Hour)},
			want: CheckState{State: "false", Reason: "failed", From: "tx2"}, statements: "not_met"},
		{name: "a failure at tx1, at a deploy that names no transaction", check: g, tx: "",
			runs: []Result{run(g, "tx1", "", OutcomeFail, time.Hour)},
			want: CheckState{State: "unknown", Reason: "subject_moved"}, statements: "unknown"},
		{name: "a pass at tx1, at a deploy that names no transaction", check: g, tx: "",
			runs: []Result{run(g, "tx1", "", OutcomePass, time.Hour)},
			want: CheckState{State: "unknown", Reason: "subject_moved"}, statements: "unknown"},
		// R13: "A query that cannot read its data, or that passes a time or row limit, gives unknown."
		// Such a run names the transaction that it tried to read, the subject's, and counts there
		// alone (TestTheDerivationRefusesAnInputThatIsNotWhole refuses one that names none): the newest
		// run at tx1 could not run, and one at tx2 says nothing of tx1.
		{name: "a pass at tx1, then a run at tx1 that could not run", check: g, tx: "tx1",
			runs: []Result{run(g, "tx1", "", OutcomePass, time.Hour), run(g, "tx1", "", OutcomeCouldNotRun, 2*time.Hour)},
			want: CheckState{State: "unknown", Reason: "could_not_run"}, statements: "unknown"},
		{name: "control: the same, then a pass at tx1", check: g, tx: "tx1",
			runs: []Result{run(g, "tx1", "", OutcomePass, time.Hour), run(g, "tx1", "", OutcomeCouldNotRun, 2*time.Hour), run(g, "tx1", "", OutcomePass, 3*time.Hour)},
			want: CheckState{State: "true", Reason: "passed", From: "tx1"}, statements: "met"},
		{name: "a failure at tx1, then a run at tx2 that could not run", check: g, tx: "tx1",
			runs: []Result{run(g, "tx1", "", OutcomeFail, time.Hour), run(g, "tx2", "", OutcomeCouldNotRun, 2*time.Hour)},
			want: CheckState{State: "false", Reason: "failed", From: "tx1"}, statements: "not_met"},
		{name: "the head of a branch judges no check of the graph", check: g, branch: "b",
			runs: []Result{run(g, "tx1", "", OutcomePass, time.Hour)}, absent: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			s.add(row.check)
			s.tieLikeC(row.check.ID)
			s.commit("c2", "c1", "packages/envoy/cmd/listener/restart_test.go")
			s.Subject = Point{Commit: "c2", Deploy: "d2", GraphTx: row.tx}
			if row.branch != "" {
				s.Subject = Point{Commit: "c2", Branch: row.branch, GraphTx: row.tx}
			}
			s.Results = append(append(s.Results, row.runs...), Result{Check: row.check.ID, Kind: ResultFailingExample, Outcome: OutcomeFail,
				TestCommit: "c2", At: checkStateRun.Add(5 * time.Minute), FaultAuthor: "author-b", TestAuthors: []string{"author-a"}})
			if row.absent {
				s.remove(checkStateCheck.ID)
				if states := derive(t, s.input()).Checks; slices.ContainsFunc(states, func(c CheckState) bool { return c.ID == row.check.ID }) {
					t.Errorf("the checks: %+v, want no state of %s", states, row.check.ID)
				}
				return
			}
			want := row.want
			want.ID = row.check.ID
			deriveAlone(t, s, want, row.statements)
		})
	}
}
