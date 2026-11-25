package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/checkstatetest"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Only a check.state_changed event moves the tester: a result.recorded event, here a fail of k1
// at the head where k1 reads yes from the tester, is no state, and changes nothing the daemon
// holds. The tester's completion then moves the issue to reviewing on the states it holds.
func TestOnlyTheStateEventMovesTheTester(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))

	fixture := checkstatetest.ReadFixture(t)
	recorded, err := json.Marshal(map[string]any{
		"id": 9001, "issue_key": "LEGION-208", "seq": 1500, "type": "result.recorded", "notify": false,
		"created_at": "2026-11-21T06:00:00Z", "actor": fixture.Actor, "payload": map[string]any{
			"result_id": "7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "issue": "LEGION-208", "check_issue": "LEGION-208",
			"check_artifact_id": "0a3f6c1e-5b2d-4e8a-9c7f-1d2e3f4a5b6c", "check_block_id": "k1", "check_version": 1,
			"when": "before_merge", "verdict": "fail", "role": "tester", "commit_sha": headOne,
		},
	})
	if err != nil {
		t.Fatalf("encode the result.recorded event: %v", err)
	}
	fact, _, err := intake.DecodeDispatchEvent("LEGION", "LEGION-208", recorded)
	if err != nil {
		t.Fatalf("decode the result.recorded event: %v", err)
	}
	if fact != nil {
		if result, err := intake.ApplyFact(context.Background(), r.pool, "dispatch", "dispatch-event:LEGION-208:1500", fact, r.engine, admissionStub{}); err != nil || result.Refusal != nil {
			t.Fatalf("apply the result.recorded event: %+v, %v", result.Refusal, err)
		}
	}
	if k1 := r.held(headOne).States["k1"]; k1.State != "yes" || k1.Seq != checkstatetest.Seq(1) {
		t.Fatalf("after the recorded fail the daemon holds k1 at %s as %+v, want yes at the sequence of its own event, %d", headOne, k1, checkstatetest.Seq(1))
	}
	assertPhase(t, r.pool, phase.Testing)

	r.complete(headOne, 2)
	assertPhase(t, r.pool, phase.Reviewing)
}

// The move reads the states at the head the tester's record names, not at the head of the pull
// request on record: k1 and k2 read yes from the tester at H1, and k2 reads no at H2, where the
// pull request now stands. A record of H1 goes to reviewing, and one of H2 goes back to
// implementing naming k2.
func TestTheMoveReadsTheStatesAtTheTestedHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tested string
		want   phase.Phase
	}{
		{"the test record names H1", headOne, phase.Reviewing},
		{"the test record names H2", headTwo, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newStatesRig(t, phase.Testing)
			seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208",
				HeadSHA: headTwo, CheckedHead: headTwo, Failing: []string{}})
			r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
			r.states(headTwo, 3, passOf("k1", "tester"), ofSet(stateOf("k2", "no", "fail"), "k1", "k2"))
			r.complete(tc.tested, 4)
			assertPhase(t, r.pool, tc.want)
			if tc.want == phase.Implementing {
				r.movedBackFailing(headTwo, failingOf("k2", "no", "fail"))
			}
		})
	}
}

// The pull request moves from H1 to H2 while the tester tests H1, and the tester's results at H1
// after the move decide the move. The daemon applies the events Dispatch published in each case,
// in their order and with their serials, as Envoy's TestAFormerHeadOfAnOpenPullRequestKeepsItsStates
// wrote them (checkstatetest.AfterAMove), with the pull request on record at H2, and the tester
// completes naming H1 with its record's serial the last of them, as the handoff's fence gives it.
// The implementer passed k1 and k2 at H1 before the move, so the tester's fail of k2 at H1 after it
// makes k2 not known there (unknown for pass_and_fail) and sends the issue back naming k2; passes
// of k1 and k2 at H1 after the move, where nobody passed before it, send the issue to review.
// Where Dispatch publishes nothing at a head the pull request has left, the move reads the states
// of H1 from before the move: the fail case goes to review on the implementer's passes, and the
// pass case goes back on no result.
func TestAResultAtTheTestedHeadAfterAMoveDecidesTheMove(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published string
		want      phase.Phase
	}{
		{"a fail of k2 at H1 after the move", "fail_after_the_move", phase.Implementing},
		{"passes of k1 and k2 at H1 after the move", "passes_after_the_move", phase.Reviewing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := checkstatetest.AfterAMove(t, tc.published)
			tested, moved := events[0].Head, ""
			for _, event := range events {
				if event.Head != tested {
					moved = event.Head
					break
				}
			}
			if moved == "" {
				t.Fatalf("the events %+v never name a head but %s, want the move to a second head", events, tested)
			}
			r := newStatesRig(t, phase.Testing)
			seedPR(t, r.pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: statesRepo, Number: 42, Branch: "legion/LEGION-208",
				HeadSHA: moved, CheckedHead: moved, Failing: []string{}})
			for _, event := range events {
				r.states(event.Head, event.Serial, intake.DispatchCheckState{Check: event.Check, State: event.State, Reason: event.Reason,
					Roles: event.Roles, CICase: event.CICase, Checks: event.Checks})
			}
			r.complete(tested, events[len(events)-1].Serial)
			assertPhase(t, r.pool, tc.want)
			if tc.want == phase.Implementing {
				r.movedBackFailing(tested, failingOf("k2", "unknown", "pass_and_fail"))
			}
		})
	}
}

// A tester's completion whose test record named no head holds the move and reads no state: the
// head the tester tested is the record's alone, and the pull request's head, where every check
// here reads yes, can be one the tester never tested. The completion tells the architect so in its
// one notice, and a later state event at the pull request's head moves nothing either. The
// tester's completion again, naming a new record of the head it tested, decides the move, as the
// notice says. (The completion route refuses a completion naming no commit, so only a row the
// engine was handed directly, never one a plugin sent, can name none.)
func TestATestRecordNamingNoHeadHoldsTheMove(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
	r.complete("", 2)
	r.waitsFor("", "tested_head")
	r.states(headOne, 3, ofSet(passOf("k1", "tester", "implementer"), "k1", "k2"))
	r.waitsFor("", "tested_head")

	if result, err := intake.ApplyFact(context.Background(), r.pool, "api", "test-again", intake.HandoffComplete{
		Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "Every check passes.",
		Commit: headOne, HandoffRecord: "test-2", HandoffVersion: 2, StatesSerial: 3,
	}, r.engine, admissionStub{}); err != nil || result.Refusal != nil {
		t.Fatalf("apply the tester's completion naming %s: %+v, %v", headOne, result.Refusal, err)
	}
	assertPhase(t, r.pool, phase.Reviewing)
}

// Neither the reviewer the pass starts nor the architect told of either move reads a verdict of
// the tester: the pass starts the reviewer once, its task built from the tester's phase row, which
// holds none (record.PhaseRow), and the architect's phase-finished notice carries the tester's
// summary and no verdict key. On a fail the notice carries the check that does not pass.
func TestTheReviewerAndTheArchitectGetNoTesterVerdict(t *testing.T) {
	t.Run("a pass", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		r.states(headOne, 1, passOf("k1", "tester"))
		r.complete(headOne, 1)
		assertPhase(t, r.pool, phase.Reviewing)
		if starts := roleStarts(t, r.pool, claim.RoleReviewer); starts != 1 {
			t.Fatalf("the pass started the reviewer %d times, want once", starts)
		}
		notices := r.finished()
		if len(notices) != 1 || notices[0]["role"] != "tester" || notices[0]["phase"] != "testing" || notices[0]["summary"] != "Every check passes." {
			t.Fatalf("phase-finished notices = %v, want the tester's one of testing with its summary", notices)
		}
		if _, carries := notices[0]["verdict"]; carries {
			t.Fatalf("the tester's phase-finished notice %v carries a verdict", notices[0])
		}
	})
	t.Run("a fail", func(t *testing.T) {
		r := newStatesRig(t, phase.Testing)
		r.states(headOne, 1, passOf("k1", "tester"), stateOf("k2", "no", "fail"))
		r.complete(headOne, 2)
		r.movedBackFailing(headOne, failingOf("k2", "no", "fail"))
		notices := r.finished()
		if len(notices) != 1 || notices[0]["role"] != "tester" || notices[0]["phase"] != "testing" || notices[0]["summary"] != "Every check passes." {
			t.Fatalf("phase-finished notices = %v, want the tester's one of testing with its summary", notices)
		}
		if _, carries := notices[0]["verdict"]; carries {
			t.Fatalf("the tester's phase-finished notice %v carries a verdict", notices[0])
		}
	})
}

// A fail state that arrives while the tester has not completed is held and starts nothing: the
// issue stays in testing, and no implementer is started.
func TestAFailStateBeforeTheTesterCompletesStartsNothing(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1, stateOf("k1", "no", "fail"))
	if k1 := r.held(headOne).States["k1"]; k1.State != "no" || k1.Reason != "fail" {
		t.Fatalf("the daemon holds k1 at %s as %+v, want no for fail", headOne, k1)
	}
	assertPhase(t, r.pool, phase.Testing)
	if task := startTask(t, r.pool, claim.RoleImplementer); task != "" || r.starts() != 0 {
		t.Fatalf("the early fail started a worker (the implementer's task %q), want none", task)
	}
	if notices := r.finished(); len(notices) != 0 {
		t.Fatalf("the early fail wrote %v, want no notice", notices)
	}
}

// Each step of the type testing among the steps the issue runs passes when each check it reads
// reads yes at its head, and its run ends passed; a check that reads no ends it failed, returning
// the work to the step a failed step returns to. A check that a CI job runs, k3 here, is left out
// of the testing step, since the end of its task holds it, so a fail of k3 fails no testing step.
// The issue here has no plan rows, so it walks the
// steps of the task default on today's line (todaysWalk).
func TestATestingStepPassesWhenItsChecksPass(t *testing.T) {
	w := todaysWalk()
	implementing, _ := w.PlaceOf(phase.Implementing)
	tested := 0
	k3Failed := []intake.DispatchCheckState{passOf("k1", "tester"), passOf("k2", "tester"), inCI(stateOf("k3", "no", "fail", "ci"), "failure")}
	for i, step := range w.Steps {
		if step.Type != plan.TypeTesting {
			continue
		}
		p, place := w.Line[i], placeOf("step", i+1)
		tested++
		for _, tc := range []struct {
			name   string
			states []intake.DispatchCheckState
			want   phase.Phase
			result string
			back   *int
		}{
			{"each check reads yes", []intake.DispatchCheckState{passOf("k1", "tester"), passOf("k2", "tester")}, phase.Reviewing, string(ResultPassed), nil},
			{"a check that a CI job runs reads no", k3Failed, phase.Reviewing, string(ResultPassed), nil},
			{"a check reads no", []intake.DispatchCheckState{passOf("k1", "tester"), stateOf("k2", "no", "fail")}, phase.Implementing, string(ResultFailed), &implementing.Position},
		} {
			t.Run(fmt.Sprintf("step %d (%s): %s", place.Place, p, tc.name), func(t *testing.T) {
				pool := migratedPool(t)
				engine := stepEngine(nil)
				stepRoot(t, pool, phase.Implementing)
				move(t, pool, engine, "LEGION-208", TriggerImplementationReady, "", record.PhaseRow{})
				assertPhase(t, pool, p)
				applyStates(t, pool, engine, "LEGION-208", statesRepo, headOne, 1, tc.states...)
				if result, err := intake.ApplyFact(context.Background(), pool, "api", "tested", intake.HandoffComplete{
					Generation: 1, Issue: "LEGION-208", Role: claim.RoleTester, Claim: "test-claim", Summary: "tested",
					Commit: headOne, HandoffRecord: "test-1", HandoffVersion: 1, StatesSerial: int64(len(tc.states)),
				}, engine, admissionStub{}); err != nil || result.Refusal != nil {
					t.Fatalf("apply the tester's completion: %+v, %v", result.Refusal, err)
				}
				assertPhase(t, pool, tc.want)
				_, end := runOf(stepWrites(t, pool, "LEGION-208"), place.Kind, place.Place)
				assertEnd(t, end, place.Kind, place.Place, 1, tc.result, tc.back)
			})
		}
	}
	if tested == 0 {
		t.Fatalf("the steps %v hold no step of the type testing, so nothing was tested", w.Steps)
	}
}

// An issue with no before_merge check goes to reviewing when its tester completes: the first
// event of its head is the set form, with no check.
func TestAnIssueWithNoCheckGoesToReview(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1)
	if held := r.held(headOne); held.Checks == nil || len(held.Checks) != 0 {
		t.Fatalf("the daemon holds the set %v at %s, want the empty set", held.Checks, headOne)
	}
	r.complete(headOne, 1)
	assertPhase(t, r.pool, phase.Reviewing)
}

// The first events at a new head can be check events alone, with no set form: each carries the
// head's whole set, so the daemon holds the set from them, and the tester's completion moves on
// them.
func TestTheFirstEventsOfAHeadGiveItsSet(t *testing.T) {
	r := newStatesRig(t, phase.Testing)
	r.states(headOne, 1, passOf("k1", "tester"), passOf("k2", "tester"))
	if held := r.held(headOne); !reflect.DeepEqual(held.Checks, []string{"k1", "k2"}) {
		t.Fatalf("the daemon holds the set %v at %s, want [k1 k2] from the check events", held.Checks, headOne)
	}
	r.complete(headOne, 2)
	assertPhase(t, r.pool, phase.Reviewing)
}
