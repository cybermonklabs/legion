package workflow

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The rule of a passing check is judgeStates' alone: a check passes when it reads yes at the tested
// head, whatever roles its results came from. A check of the head's set with no state, and a CI run
// still going with no result, wait, since neither is a state the check ends at; every other state
// is conclusive, and one that is not yes fails and is named with why, a state Dispatch could not
// read among them. A state of a check outside the set is no part of the judgement, and no set held
// is no judgement at all.
func TestJudgeStates(t *testing.T) {
	state := func(check, answer, reason, ciCase string, roles ...string) record.CheckState {
		return record.CheckState{Check: check, State: answer, Reason: reason, CICase: ciCase, Roles: roles}
	}
	held := func(checks []string, states ...record.CheckState) record.HeadStates {
		head := record.HeadStates{Checks: checks, States: map[string]record.CheckState{}}
		for _, s := range states {
			head.States[s.Check] = s
		}
		return head
	}
	for _, tc := range []struct {
		name string
		head record.HeadStates
		want testingJudgement
	}{
		{"all yes from tester", held([]string{"k1", "k2"}, state("k1", "yes", "", "", "tester"), state("k2", "yes", "", "", "implementer", "tester")),
			testingJudgement{}},
		{"yes from implementer only", held([]string{"k1"}, state("k1", "yes", "", "", "implementer")),
			testingJudgement{}},
		{"no for fail", held([]string{"k1"}, state("k1", "no", "fail", "")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "no", "fail")}}},
		{"unknown for pass_and_fail", held([]string{"k1"}, state("k1", "unknown", "pass_and_fail", "")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "unknown", "pass_and_fail")}}},
		// Dispatch published a state it could not compute: a held, conclusive state that is not
		// yes, so the check does not pass, named with its reason.
		{"read_failed", held([]string{"k1"}, state("k1", "unknown", "read_failed", "")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "unknown", "read_failed")}}},
		{"a check of the set with no state", held([]string{"k1", "k2"}, state("k1", "yes", "", "", "tester")),
			testingJudgement{Waiting: []string{"k2"}}},
		{"a state of a check outside the set", held([]string{"k1"}, state("k1", "yes", "", "", "tester"), state("k9", "no", "fail", "")),
			testingJudgement{}},
		{"yes from ci", held([]string{"k1"}, state("k1", "yes", "", "success", "ci")),
			testingJudgement{}},
		{"no for no_result with a CI run going on", held([]string{"k1"}, state("k1", "no", "no_result", "not_completed")),
			testingJudgement{Waiting: []string{"k1"}}},
		{"unknown for pass_and_fail with a CI run going on", held([]string{"k1"}, state("k1", "unknown", "pass_and_fail", "not_completed", "ci")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "unknown", "pass_and_fail")}}},
		{"no for fail with a CI run going on", held([]string{"k1"}, state("k1", "no", "fail", "not_completed")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "no", "fail")}}},
		{"a fail beside a state Dispatch could not read", held([]string{"k1", "k2"}, state("k1", "no", "fail", ""), state("k2", "unknown", "read_failed", "")),
			testingJudgement{Failing: []record.FailingCheck{failingOf("k1", "no", "fail"), failingOf("k2", "unknown", "read_failed")}}},
		{"a fail beside a check with no state", held([]string{"k1", "k2"}, state("k1", "no", "fail", "")),
			testingJudgement{Waiting: []string{"k2"}, Failing: []record.FailingCheck{failingOf("k1", "no", "fail")}}},
		{"a fail beside a CI run that goes on with no result", held([]string{"k1", "k2"}, state("k1", "no", "fail", ""), state("k2", "no", "no_result", "not_completed")),
			testingJudgement{Waiting: []string{"k2"}, Failing: []record.FailingCheck{failingOf("k1", "no", "fail")}}},
		{"a head with no check", held([]string{}), testingJudgement{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, judged := judgeStates(tc.head)
			if !judged || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("judgeStates = %+v, %t; want %+v, judged", got, judged, tc.want)
			}
		})
	}
	t.Run("no set held", func(t *testing.T) {
		if got, judged := judgeStates(record.HeadStates{States: map[string]record.CheckState{"k1": state("k1", "yes", "", "", "tester")}}); judged {
			t.Fatalf("judgeStates of a head with no set = %+v, judged; want no judgement", got)
		}
	})
}

// Each pair of a state of the issue and an event the testing move meets, each its own row. A
// completion that waits tells the architect once, in one phase-finished notice carrying what it
// waits for as fields; a later event that still leaves it waiting tells nothing more. The move
// decides only once every check of the set holds a conclusive state, so a fail waits while another
// check has no state or a CI run with no result goes on, and the notice of a move back carries every
// check that does not pass, a state Dispatch could not read among them.
func TestTheTestingMoveStateTable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		given func(r *statesRig)
		event func(r *statesRig)
		then  func(r *statesRig)
	}{
		{
			name: "testing, tester not completed | a state of k1 at the tested head | held, nothing moves", at: phase.Testing,
			event: func(r *statesRig) { r.states(headOne, 1, passOf("k1", "tester")) },
			then: func(r *statesRig) {
				if k1 := r.held(headOne).States["k1"]; k1.State != "yes" {
					r.t.Fatalf("the daemon holds k1 as %+v, want yes", k1)
				}
				assertPhase(r.t, r.pool, phase.Testing)
				if r.starts() != 0 || len(r.finished()) != 0 {
					r.t.Fatalf("a state before the completion started %d workers and wrote %v", r.starts(), r.finished())
				}
			},
		},
		{
			name: "testing, tester completed with serial 7, the daemon holds serials through 6 | the completion | waits for the fence, one notice", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"), passOf("k1", "tester", "implementer"),
					passOf("k2", "tester", "implementer"), passOf("k1", "tester"), passOf("k2", "tester"))
			},
			event: func(r *statesRig) { r.complete(headOne, 7) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "fence") },
		},
		{
			name: "testing, tester completed with serial 7, all states yes, serial 7 not held | the event of serial 7, yes from tester | reviewing", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"), passOf("k1", "tester", "implementer"),
					passOf("k2", "tester", "implementer"), passOf("k1", "tester"), passOf("k2", "tester"))
				r.complete(headOne, 7)
			},
			event: func(r *statesRig) { r.states(headOne, 7, ofSet(passOf("k1", "tester"), "k1", "k2")) },
			then:  func(r *statesRig) { assertPhase(r.t, r.pool, phase.Reviewing) },
		},
		{
			name: "testing, tester completed with serial 7, serials 5 and 7 held, 6 not | the completion | waits for the fence", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 5, passOf("k1", "tester"))
				r.states(headOne, 7, ofSet(passOf("k2", "tester"), "k1", "k2"))
			},
			event: func(r *statesRig) { r.complete(headOne, 7) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "fence") },
		},
		{
			name: "testing, tester completed, no set held | the completion | waits, one notice", at: phase.Testing,
			given: func(r *statesRig) { r.states(headTwo, 1, passOf("k1", "tester")) },
			event: func(r *statesRig) { r.complete(headOne, 1) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "set") },
		},
		{
			name: "testing, tester completed, k1 no for no_result, ci_case not_completed | the completion | waits", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, inCI(stateOf("k1", "no", "no_result"), "not_completed"), passOf("k2", "tester"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "checks", "k1") },
		},
		{
			name: "testing, tester completed, k1 no for no_result, ci_case not_completed | the state of k1, yes from ci | reviewing", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, inCI(stateOf("k1", "no", "no_result"), "not_completed"), passOf("k2", "tester"))
				r.complete(headOne, 2)
			},
			event: func(r *statesRig) { r.states(headOne, 3, ofSet(inCI(passOf("k1", "ci"), "success"), "k1", "k2")) },
			then:  func(r *statesRig) { assertPhase(r.t, r.pool, phase.Reviewing) },
		},
		{
			name: "testing, tester completed, k1 unknown for pass_and_fail, ci_case not_completed | the completion | implementing, the task names k1", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, inCI(stateOf("k1", "unknown", "pass_and_fail", "ci"), "not_completed"), passOf("k2", "tester"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then:  func(r *statesRig) { r.movedBackFailing(headOne, failingOf("k1", "unknown", "pass_and_fail")) },
		},
		{
			name: "testing, tester completed, k1 no for fail, ci_case not_completed | the completion | implementing, the task names k1", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, inCI(stateOf("k1", "no", "fail"), "not_completed"), passOf("k2", "tester"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then:  func(r *statesRig) { r.movedBackFailing(headOne, failingOf("k1", "no", "fail")) },
		},
		{
			name: "testing, tester completed, k2 has no state | a state of k1 | waits, no second notice", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(intake.DispatchCheckState{}, "k1", "k2"))
				r.complete(headOne, 1)
			},
			event: func(r *statesRig) { r.states(headOne, 2, ofSet(passOf("k1", "tester"), "k1", "k2")) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "checks", "k1", "k2") },
		},
		{
			name: "testing, tester completed, k2 has no state | the state of k2, yes from tester | reviewing", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(passOf("k1", "tester"), "k1", "k2"))
				r.complete(headOne, 1)
			},
			event: func(r *statesRig) { r.states(headOne, 2, ofSet(passOf("k2", "tester"), "k1", "k2")) },
			then:  func(r *statesRig) { assertPhase(r.t, r.pool, phase.Reviewing) },
		},
		// Dispatch could not compute k1's state and published that: a held, conclusive state that is
		// not yes, so the issue goes back naming k1 with its reason, and waits for nothing.
		{
			name: "testing, tester completed, k1 unknown for read_failed | the completion | implementing, the task names k1 with read_failed", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, stateOf("k1", "unknown", "read_failed"), passOf("k2", "tester"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then:  func(r *statesRig) { r.movedBackFailing(headOne, failingOf("k1", "unknown", "read_failed")) },
		},
		{
			name: "testing, tester completed, k1 no for fail, k2 has no state | the completion | waits, the notice names k2", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(stateOf("k1", "no", "fail"), "k1", "k2"))
			},
			event: func(r *statesRig) { r.complete(headOne, 1) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "checks", "k2") },
		},
		{
			name: "testing, tester completed, k1 no for fail, k2 has no state | the state of k2, yes from tester | implementing, the task names k1 alone", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(stateOf("k1", "no", "fail"), "k1", "k2"))
				r.complete(headOne, 1)
			},
			event: func(r *statesRig) { r.states(headOne, 2, ofSet(passOf("k2", "tester"), "k1", "k2")) },
			then:  func(r *statesRig) { r.movedBackFailing(headOne, failingOf("k1", "no", "fail")) },
		},
		{
			name: "testing, tester completed, k1 no for fail, k2 no for no_result, ci_case not_completed | the completion | waits", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, stateOf("k1", "no", "fail"), inCI(stateOf("k2", "no", "no_result"), "not_completed"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then:  func(r *statesRig) { r.waitsFor(headOne, "checks", "k2") },
		},
		{
			name: "testing, tester completed | a state of k1, no for fail | implementing, the task names k1", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(passOf("k2", "tester"), "k1", "k2"))
				r.complete(headOne, 1)
			},
			event: func(r *statesRig) { r.states(headOne, 2, ofSet(stateOf("k1", "no", "fail"), "k1", "k2")) },
			then:  func(r *statesRig) { r.movedBackFailing(headOne, failingOf("k1", "no", "fail")) },
		},
		{
			name: "testing, tester completed | a state at another head | held, nothing moves", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, ofSet(passOf("k1", "tester"), "k1", "k2"))
				r.complete(headOne, 1)
			},
			event: func(r *statesRig) { r.states(headTwo, 2, ofSet(passOf("k2", "tester"), "k1", "k2")) },
			then: func(r *statesRig) {
				if k2 := r.held(headTwo).States["k2"]; k2.State != "yes" {
					r.t.Fatalf("the daemon holds k2 at %s as %+v, want yes", headTwo, k2)
				}
				r.waitsFor(headOne, "checks", "k2")
			},
		},
		{
			name: "reviewing | a state of k1, no for fail | held, nothing moves", at: phase.Reviewing,
			given: func(r *statesRig) {
				seedPhase(r.t, r.pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", HandoffCommit: headOne,
					HandoffRecord: "test-1", HandoffVersion: 1, LastHandoff: "test-1", Summary: "Every check passes."})
			},
			event: func(r *statesRig) { r.states(headOne, 1, stateOf("k1", "no", "fail")) },
			then: func(r *statesRig) {
				if k1 := r.held(headOne).States["k1"]; k1.State != "no" {
					r.t.Fatalf("the daemon holds k1 as %+v, want no", k1)
				}
				assertPhase(r.t, r.pool, phase.Reviewing)
				if r.starts() != 0 || len(r.finished()) != 0 {
					r.t.Fatalf("a state in reviewing started %d workers and wrote %v", r.starts(), r.finished())
				}
			},
		},
		{
			name: "testing, tester completed | a state with a sequence below the one held | nothing changes", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 2, ofSet(passOf("k1", "tester"), "k1", "k2"))
				r.complete(headOne, 2)
			},
			event: func(r *statesRig) { r.states(headOne, 1, ofSet(stateOf("k1", "no", "fail"), "k1", "k2")) },
			then: func(r *statesRig) {
				if k1 := r.held(headOne).States["k1"]; k1.State != "yes" || k1.Seq != checkstatetest.Seq(2) {
					r.t.Fatalf("the daemon holds k1 as %+v, want yes at sequence %d", k1, checkstatetest.Seq(2))
				}
				r.waitsFor(headOne, "checks", "k2")
			},
		},
		{
			name: "testing, tester completed, k1 no for fail, k2 unknown for read_failed | the completion | implementing, the task names k1 and k2", at: phase.Testing,
			given: func(r *statesRig) {
				r.states(headOne, 1, stateOf("k1", "no", "fail"), stateOf("k2", "unknown", "read_failed"))
			},
			event: func(r *statesRig) { r.complete(headOne, 2) },
			then: func(r *statesRig) {
				r.movedBackFailing(headOne, failingOf("k1", "no", "fail"), failingOf("k2", "unknown", "read_failed"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStatesRig(t, tc.at)
			if tc.given != nil {
				tc.given(r)
			}
			tc.event(r)
			tc.then(r)
		})
	}
}
