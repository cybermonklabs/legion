package record

import (
	"maps"
	"slices"
	"testing"
)

// ciVerdictRow is one state of a check and what the rule of a CI check reads of it.
type ciVerdictRow struct {
	name  string
	state CheckState
	held  bool
	want  CIState
}

// ciVerdictRows is every case of the rule of a CI check: each case of a job that has not ended at
// the head waits, a job that passed passes, and a job that ended with any other answer, a word
// Dispatch writes that the daemon does not know among them, fails. A check that no CI job runs
// has no CI verdict, and a check with no state held is not known to be one.
func ciVerdictRows() []ciVerdictRow {
	state := func(answer, reason, ciCase string) CheckState {
		return CheckState{Check: "k1", State: answer, Reason: reason, CICase: ciCase, Roles: []string{"ci"}}
	}
	rows := []ciVerdictRow{}
	for _, ciCase := range []string{"absent", "not_completed", "not_started", "no_head", "github_unreadable"} {
		rows = append(rows, ciVerdictRow{ciCase + " with no", state("no", "no_result", ciCase), true, CIRunning})
	}
	rows = append(rows,
		ciVerdictRow{"not_completed with yes", state("yes", "", "not_completed"), true, CIRunning},
		ciVerdictRow{"success with yes", state("yes", "", "success"), true, CIPassed},
	)
	for _, ciCase := range []string{"failure", "timed_out", "cancelled", "skipped", "neutral"} {
		rows = append(rows,
			ciVerdictRow{ciCase + " with no", state("no", "fail", ciCase), true, CIFailed},
			ciVerdictRow{ciCase + " with unknown", state("unknown", "pass_and_fail", ciCase), true, CIFailed},
		)
	}
	return append(rows,
		ciVerdictRow{"job_recorded with yes", state("yes", "", "job_recorded"), true, CIPassed},
		ciVerdictRow{"job_recorded with no", state("no", "fail", "job_recorded"), true, CIFailed},
		ciVerdictRow{"an unknown word startup_failure with unknown", state("unknown", "pass_and_fail", "startup_failure"), true, CIFailed},
		ciVerdictRow{"no ci_case: no CI job runs the check", state("no", "fail", ""), true, CINotCI},
		ciVerdictRow{"no state held", CheckState{}, false, CIUnknownKind},
	)
}

// The rule of a check that a CI job runs reads each state the daemon holds, one row a case. The
// control is the same table under a copy of the rule that does not wait on not_completed: its row
// with no then reads CIFailed and its row with yes CIPassed, so the table fails, which the
// subtest asserts.
func TestCIVerdictOfEachCase(t *testing.T) {
	for _, row := range ciVerdictRows() {
		t.Run(row.name, func(t *testing.T) {
			if got := CIVerdict(row.state, row.held); got != row.want {
				t.Fatalf("CIVerdict(%+v, %t) = %d, want %d", row.state, row.held, got, row.want)
			}
		})
	}

	t.Run("the control: a rule that does not wait on not_completed fails the table", func(t *testing.T) {
		waits := slices.DeleteFunc(slices.Clone(ciWaitCases), func(ciCase string) bool { return ciCase == "not_completed" })
		if len(waits) != len(ciWaitCases)-1 {
			t.Fatalf("the wait cases %v hold no not_completed to take out", ciWaitCases)
		}
		missed := map[string]CIState{}
		for _, row := range ciVerdictRows() {
			if got := ciVerdictWaiting(row.state, row.held, waits); got != row.want {
				missed[row.name] = got
			}
		}
		if want := map[string]CIState{"not_completed with no": CIFailed, "not_completed with yes": CIPassed}; !maps.Equal(missed, want) {
			t.Fatalf("the rows the copy misses, with what each reads = %v, want %v", missed, want)
		}
	})
}

// What of the CI at a head has not passed is each check of the head's set that a CI job may run
// and that has not passed there, in the set's order: one whose job runs, one whose job failed and
// one with no state held. A check that passed, one that no CI job runs whatever it reads, and a
// state of a check outside the set name nothing. A set held empty and a set not held name nothing
// either.
func TestCIWaitsNamesEachCheckThatHasNotPassedAtTheHead(t *testing.T) {
	state := func(check, answer, ciCase string) CheckState {
		return CheckState{Check: check, State: answer, CICase: ciCase}
	}
	head := HeadStates{
		Checks: []string{"k-failed", "k-passed", "k-unit", "k-unstated", "k-running"},
		States: map[string]CheckState{
			"k-failed":  state("k-failed", "no", "failure"),
			"k-passed":  state("k-passed", "yes", "success"),
			"k-unit":    state("k-unit", "no", ""),
			"k-running": state("k-running", "no", CINotCompleted),
			"k-outside": state("k-outside", "no", "failure"),
		},
	}
	if got, want := CIWaits(head), []string{"k-failed", "k-unstated", "k-running"}; !slices.Equal(got, want) {
		t.Fatalf("CIWaits = %v, want %v", got, want)
	}
	for name, held := range map[string]HeadStates{
		"a set held empty": {Checks: []string{}, States: map[string]CheckState{}},
		"no set held":      {States: map[string]CheckState{"k-failed": state("k-failed", "no", "failure")}},
	} {
		if got := CIWaits(held); got != nil {
			t.Errorf("CIWaits of %s = %v, want nothing", name, got)
		}
	}
}
