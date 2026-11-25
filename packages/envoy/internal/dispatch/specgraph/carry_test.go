package specgraph

import (
	"slices"
	"testing"
	"time"
)

// The rule that replaces The Spec's R8: "A result counts only at the commit that it read", and "A
// failing example that a check's test caught (The Spec R9) counts at another commit only while no
// file that the test reads changed". R18: "At each commit, Dispatch derives four ties". A failing
// example of a code check, and a result of the test of a program check of production, at commit A
// counts at the subject's commit B only when the check's tie is known at A and at B and no path
// that changed between them is covered by the tie at A or by the tie at B. Each row is
// TestCheckState's base with subject c2, whose tree is c1's with docs/x.md changed, with a pass of
// C at c2, so that c1's caught failing example decides for C, and with one more change: narrow is
// C's tie with no entry of testdata.
func TestAResultCarriesByTheTiesOfBothCommits(t *testing.T) {
	t.Parallel()
	const deleted = "packages/envoy/cmd/listener/testdata/a.json" // a file of c1, under testdata
	narrow, err := NewTie(slices.DeleteFunc(slices.Clone(checkStateTie), func(p string) bool { return p == "packages/envoy/cmd/listener/testdata/**" }))
	if err != nil {
		t.Fatal(err)
	}
	code := func(change func(c *checkCase)) func(c *checkCase) {
		return func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			c.Results = append(c.Results, checkRun("c2", OutcomePass, checkStateRun.Add(time.Hour)))
			change(c)
		}
	}
	program := func(change func(c *checkCase)) func(c *checkCase) {
		return func(c *checkCase) {
			c.productionProgram("docs/x.md", "c1")
			change(c)
		}
	}
	q := checkStateProductionProgram.ID
	for _, row := range []struct {
		name   string
		change func(c *checkCase)
		want   CheckState // ID "" is C
	}{
		{name: "control: no path that changed is in either tie", change: code(func(*checkCase) {}),
			want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "control: the ties differ, and no path that changed is in either", change: code(func(c *checkCase) { c.Ties["c2"][c.check] = narrow }),
			want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "(a) c2 deletes a file that only the tie at c1, where the test caught the fault, covers", change: code(func(c *checkCase) {
			delete(c.files2(), deleted)
			c.Ties["c2"][c.check] = narrow
		}), want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "control: c2 deletes a file that both ties cover", change: code(func(c *checkCase) { delete(c.files2(), deleted) }),
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "c2 adds a file that only the tie at c2 covers", change: code(func(c *checkCase) {
			c.files2()["packages/envoy/cmd/listener/testdata/b.json"] = "c2"
			c.Ties["c1"][c.check] = narrow
		}), want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "(c) c1 has no ties", change: code(func(c *checkCase) { delete(c.Ties, "c1") }),
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "(d) the check has a tie at c2 only", change: code(func(c *checkCase) { delete(c.Ties["c1"], c.check) }),
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "the check has a tie at c1 only", change: code(func(c *checkCase) { delete(c.Ties["c2"], c.check) }),
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		// A caller that knows no tie, or has no history, says so with an empty value, which the entry
		// takes (TestTheDerivationRefusesAnInputThatIsNotWhole), and no failing example counts at
		// another commit with it; a history that holds no commit holds none that the fault was
		// planted on, so that record cannot be placed.
		{name: "an empty map of ties: no tie is known", change: code(func(c *checkCase) { c.Ties = CommitTies{} }),
			want: CheckState{State: "unknown", Reason: "no_failing_example"}},
		{name: "a FixtureHistory of no commit: the history holds neither commit", change: code(func(c *checkCase) { c.History = FixtureHistory{Commits: []FixtureCommit{}} }),
			want: CheckState{State: "unknown", Reason: "planted_on_outside_history", Detail: "c1"}},
		{name: "(b) a program check of production: c2 deletes a file that only the tie at c1, where its test caught the fault, covers",
			change: program(func(c *checkCase) {
				delete(c.files2(), deleted)
				c.Ties["c2"][q] = narrow
			}), want: CheckState{ID: q, State: "unknown", Reason: "no_failing_example"}},
		{name: "control (b): the ties of the program check differ, and no path that changed is in either",
			change: program(func(c *checkCase) { c.Ties["c2"][q] = narrow }),
			want:   CheckState{ID: q, State: "true", Reason: "passed", From: "d2"}},
		{name: "(b), a run: the test of the program check ran at c1, and c2 deletes a file that only the tie at c1 covers",
			change: program(func(c *checkCase) {
				c.testRunAt(q, "c1")
				delete(c.files2(), deleted)
				c.Ties["c2"][q] = narrow
			}), want: CheckState{ID: q, State: "unknown", Reason: "subject_moved"}},
		{name: "control (b), a run: the test of the program check ran at c1, and no path that changed is in either tie",
			change: program(func(c *checkCase) {
				c.testRunAt(q, "c1")
				c.Ties["c2"][q] = narrow
			}), want: CheckState{ID: q, State: "true", Reason: "passed", From: "d2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			row.change(s)
			want := row.want
			if want.ID == "" {
				want.ID = checkStateCheck.ID
			}
			states := checkStates(t, s.input())
			if i := slices.IndexFunc(states, func(c CheckState) bool { return c.ID == want.ID }); i < 0 || states[i] != want {
				t.Errorf("deriveChecks = %+v\nwant %+v", states, want)
			}
		})
	}
}

// A result counts only at the commit that it read, and a run of a CI job only at the commit that
// the job tested. So a run of a check of the code counts at its own commit alone, also at a later
// commit where no file that the check's tie covers changed; a run of the check's job that ended
// there with no result of the check is no fault of CI at the later commit; and a program, a query
// and an agent of the code each read their runs so. Each row is TestCheckState's base, a pass of
// C's job at c1 and a failing example planted on c1, with the row's change at the subject c2, whose
// tree is c1's with docs/x.md changed, a file that no tie covers. Each control holds a record of
// c2 itself, which counts there.
func TestARunCountsOnlyAtTheCommitItRead(t *testing.T) {
	t.Parallel()
	atC2 := func(change func(c *checkCase)) func(c *checkCase) {
		return func(c *checkCase) {
			c.commit("c2", "c1", "docs/x.md")
			c.Subject.Commit = "c2"
			change(c)
		}
	}
	ended := func(commit, leg string) JobRun {
		return JobRun{Job: "go-checks", Leg: leg, Commit: commit, Ended: true}
	}
	later := checkStateRun.Add(time.Hour)
	q, k := checkStateCodeQuery.ID, checkStateCodeAgent.ID
	for _, row := range []struct {
		name   string
		change func(c *checkCase)
		want   CheckState // ID "" is C
	}{
		{name: "a pass at c1", change: atC2(func(*checkCase) {}),
			want: CheckState{State: "unknown", Reason: "subject_moved"}},
		{name: "control: a pass at c1 and a pass at c2", change: atC2(func(c *checkCase) {
			c.Results = append(c.Results, checkRun("c2", OutcomePass, later))
		}), want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "a failure at c1 and a later pass at c2", change: atC2(func(c *checkCase) {
			c.Results[0].Outcome = OutcomeFail
			c.Results = append(c.Results, checkRun("c2", OutcomePass, later))
		}), want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "a run of the job at c1 that ended with no result in leg arm, and a pass at c2 in leg linux", change: atC2(func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.Results = append(c.Results, legRun("c2", "linux", 1, OutcomePass, later))
			c.JobRuns = []JobRun{ended("c1", "linux"), ended("c1", "arm"), ended("c2", "linux")}
		}), want: CheckState{State: "true", Reason: "passed", From: "c2"}},
		{name: "control: the run of the job at c2 ended with no result in leg arm", change: atC2(func(c *checkCase) {
			c.Results[0].Leg = "linux"
			c.Results = append(c.Results, legRun("c2", "linux", 1, OutcomePass, later))
			c.JobRuns = []JobRun{ended("c1", "linux"), ended("c2", "linux"), ended("c2", "arm")}
		}), want: CheckState{State: "unknown", Reason: "ci_fault", Detail: "arm"}},
		{name: "no run of C, and a run of the job at c1 that ended", change: atC2(func(c *checkCase) {
			c.Results = c.Results[1:]
			c.JobRuns = []JobRun{ended("c1", "")}
		}), want: CheckState{State: "unknown", Reason: "no_result"}},
		{name: "control: no run of C, and a run of the job at c2 that ended", change: atC2(func(c *checkCase) {
			c.Results = c.Results[1:]
			c.JobRuns = []JobRun{ended("c2", "")}
		}), want: CheckState{State: "unknown", Reason: "ci_fault"}},
		{name: "a query of the code: a pass at c1", change: func(c *checkCase) { c.codeResults(checkStateCodeQuery, "c1") },
			want: CheckState{ID: q, State: "unknown", Reason: "subject_moved"}},
		{name: "control: a query of the code with a pass at c2", change: func(c *checkCase) { c.codeResults(checkStateCodeQuery, "c2") },
			want: CheckState{ID: q, State: "true", Reason: "passed", From: "c2"}},
		{name: "an agent of the code: a pass at c1", change: func(c *checkCase) { c.codeResults(checkStateCodeAgent, "c1") },
			want: CheckState{ID: k, State: "unknown", Reason: "subject_moved"}},
		{name: "control: an agent of the code with a pass at c2", change: func(c *checkCase) { c.codeResults(checkStateCodeAgent, "c2") },
			want: CheckState{ID: k, State: "true", Reason: "passed", From: "c2"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			row.change(s)
			want := row.want
			if want.ID == "" {
				want.ID = checkStateCheck.ID
			}
			states := checkStates(t, s.input())
			if i := slices.IndexFunc(states, func(c CheckState) bool { return c.ID == want.ID }); i < 0 || states[i] != want {
				t.Errorf("deriveChecks = %+v\nwant %+v", states, want)
			}
		})
	}
}

// A failing example keeps the rule on files that a run does not have: one that a check's test
// caught counts at another commit while no file that the test reads changed between the two, by
// the check's tie at each commit, and stops counting where such a file changed. Each row is
// TestCheckState's base, a pass of C's job at c1 and a failing example planted on c1, with Q, a
// query of the code with C's tie, and a failing example of Q at c1; c2, whose tree is c1's with
// docs/x.md changed, a file that no tie covers; c3, whose tree is c2's with checkStateFaultPath
// changed, a file of the tie; and a pass of each check at the row's subject, so that the failing
// example decides there whatever the rule for runs at another commit.
func TestAFailingExampleStillCountsWhereNoTiedFileChanged(t *testing.T) {
	t.Parallel()
	q := checkStateCodeQuery
	counts := func(commit string) []CheckState {
		return []CheckState{
			{ID: checkStateCheck.ID, State: "true", Reason: "passed", From: commit},
			{ID: q.ID, State: "true", Reason: "passed", From: commit},
		}
	}
	for _, row := range []struct {
		subject string
		want    []CheckState
	}{
		{"c1", counts("c1")},
		{"c2", counts("c2")},
		{"c3", []CheckState{
			{ID: checkStateCheck.ID, State: "unknown", Reason: "no_failing_example"},
			{ID: q.ID, State: "unknown", Reason: "no_failing_example"},
		}},
	} {
		t.Run("subject "+row.subject, func(t *testing.T) {
			t.Parallel()
			s := newCheckCase(t)
			s.add(q)
			s.tieLikeC(q.ID)
			s.commit("c2", "c1", "docs/x.md")
			s.commit("c3", "c2", checkStateFaultPath)
			s.Results = append(s.Results, Result{Check: q.ID, Kind: ResultFailingExample, Outcome: OutcomeFail, Commit: "c1", At: checkStateRun.Add(5 * time.Minute)})
			at := checkStateRun.Add(time.Hour)
			if row.subject != "c1" {
				s.Results = append(s.Results, checkRun(row.subject, OutcomePass, at))
			}
			s.Results = append(s.Results, Result{Check: q.ID, Kind: ResultRun, Outcome: OutcomePass, Commit: row.subject, At: at})
			s.Subject.Commit = row.subject
			states := checkStates(t, s.input())
			for _, want := range row.want {
				if !slices.Contains(states, want) {
					t.Errorf("deriveChecks = %+v\nwant %+v among them", states, want)
				}
			}
		})
	}
}

// files2 is the tree of commit c2 of the case, which a row changes in place.
func (s *checkCase) files2() map[string]string {
	s.t.Helper()
	i := slices.IndexFunc(s.history.Commits, func(c FixtureCommit) bool { return c.ID == "c2" })
	if i < 0 {
		s.t.Fatal("the history holds no commit c2")
	}
	return s.history.Commits[i].Files
}

// testRunAt makes commit the commit of the test of each run of the check id.
func (s *checkCase) testRunAt(id, commit string) {
	for i := range s.Results {
		if s.Results[i].Check == id && s.Results[i].Kind == ResultRun {
			s.Results[i].TestCommit = commit
		}
	}
}
