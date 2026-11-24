package workflow

// A merge forward is asked at step as at the phases of Legion's own workers that read the head: an
// issue whose task runs a step of a role the deployment defines, with its head confirmed in
// conflict with its base, keeps its step, the implementer is asked to merge the base forward, and
// the role of that step is told to hold (record.MergeForwardPhases, record.MergeForwardReaches);
// the landing of the merge tells that role it landed. The control is the same task's tester step,
// whose tester is told the same way.
//
// The step of the type implementation is its agent's own wherever it runs, as implementing is the
// implementer's: in `coder tester`, coder a role the plan defines of that type, the coder's step
// runs as step, and a merge forward asked there is told, covered and dropped as one asked in
// implementing is (TestAMergeForwardAtTheImplementationStepAsksItsAgentAlone and the two tests
// after it).

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAMergeForwardAtAStepOfADeploymentRoleTellsThatRole(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		status string
		steps  []string
		worker claim.Role
	}{
		{name: "a step of a role the deployment defines", at: phase.Step, status: "in_progress", steps: []string{"implementer", "checker"}, worker: "checker"},
		{name: "control: a tester's step", at: phase.Testing, status: "testing", steps: []string{"implementer", "tester"}, worker: claim.RoleTester},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, tc.status, settledGreen("lint"))
			seedStepsPlan(t, pool, "LEGION-208", 1, tc.steps...)
			seedRecord(t, pool, func(tx pgx.Tx) error {
				return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
			})
			if tc.at == phase.Step {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.worker, Claim: claim.Token(string(tc.worker) + "-claim"), Entry: 1})
			}

			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, tc.at, tc.status, record.ConflictCheckNotGreen, tc.worker)

			asked := len(mergeForwardMessages(t, pool))
			landsMergeForward(t, pool)
			// The worker's phases row runs no step of the plan, so its claim is the role's own token
			// (record.WorkerClaim).
			want := []sent{{step: record.MergeForwardLanded, role: tc.worker, claim: seededClaim(t, tc.worker)}}
			if got := sentOf(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, want) {
				t.Fatalf("the landing queued %+v, want %+v", got, want)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the merge landed the issue is in %s, want it still in %s", got, tc.at)
			}
		})
	}
}

// coderTester writes the plan of LEGION-208, which seedConflictIssue seeded, whose task a has the
// steps `coder tester`, coder a role the plan defines of the type implementation, with that task
// running at step place.
func coderTester(t *testing.T, pool *pgxpool.Pool, place int) {
	t.Helper()
	seedStepsPlanDefining(t, pool, "LEGION-208", 1, map[string]plan.Type{"coder": plan.TypeImplementation}, "coder", "tester")
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", stepPlace(place), record.TaskRunning)
	})
}

// toldOf is the merge-forward message step to role about conflictHead onto main, with the issue in
// at, for a required check not green, on the claim of the agent role names on LEGION-208.
func toldOf(t *testing.T, role claim.Role, step record.MergeForwardStep, at phase.Phase) record.MergeForwardMessage {
	t.Helper()
	return record.MergeForwardMessage{Role: role, Claim: claim.Token(token(t, "LEGION-208", role)), Step: step, Head: conflictHead,
		Base: "main", Phase: at, PullRequest: 42, Why: record.ConflictCheckNotGreen}
}

// A confirmed conflict asked of while the task stands at its implementation step asks the agent of
// that step alone, as the implementer is asked in implementing: the ask says its own assignment
// and its completion stand, no hold goes to anyone, and the architect is told nothing, since the
// work the conflict needs is already where the task stands. The merge that agent pushes lands
// with no message to it. In `coder tester` that is the coder at its step, which runs as step. The
// control is the implementer of the default steps of a run with no plan rows, in implementing.
func TestAMergeForwardAtTheImplementationStepAsksItsAgentAlone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		plan  bool
		asked claim.Role
	}{
		{name: "coder tester, at the coder's step", at: phase.Step, plan: true, asked: "coder"},
		{name: "control: the default steps, in implementing", at: phase.Implementing, asked: claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, "in_progress", settledGreen("lint"))
			if tc.plan {
				coderTester(t, pool, 1)
			}
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			asked := mergeForwardMessages(t, pool)
			if want := []record.MergeForwardMessage{toldOf(t, tc.asked, record.MergeForwardAsk, tc.at)}; !reflect.DeepEqual(asked, want) {
				t.Fatalf("merge-forward messages %+v, want %+v: the ask alone", asked, want)
			}
			if notices := noticeKinds(t, pool, "LEGION-208"); len(notices) != 0 {
				t.Errorf("notices %v, want none: the work the conflict needs is already where the task stands", notices)
			}
			own := fmt.Sprintf("Your own assignment in %s stands, and so does its completion.", tc.at)
			if text := asked[0].Message("LEGION-208", recordedPR(t, pool, "LEGION-208")); !strings.Contains(text, own) {
				t.Errorf("the ask reads %q, want it to say %q", text, own)
			}

			landsMergeForward(t, pool)
			if landed := mergeForwardMessages(t, pool)[len(asked):]; len(landed) != 0 {
				t.Errorf("the landing queued %+v, want nothing: the agent that pushed the merge is told nothing of it", landed)
			}
		})
	}
}

// An ask made while the task stands at its implementation step covers that step alone: the
// completion of that step at the head the ask names, without the merge, moves the task on, and
// the step it moves to asks again, with the hold of that step's worker and the architect's one
// checks-red notice naming where the issue stays. In `coder tester` the coder's pass at its step
// moves the task to the tester's step, in testing. The control is the implementer's completion on
// the default steps, which moves the issue from implementing to testing.
func TestAnAskAtTheImplementationStepIsAskedAgainOnceTheTaskMovesOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		at    phase.Phase
		plan  bool
		asked claim.Role
		done  intake.HandoffComplete
	}{
		{name: "coder tester, the coder's pass", at: phase.Step, plan: true, asked: "coder",
			done: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: "coder", Claim: claim.Token(token(t, "LEGION-208", "coder")),
				Summary: "implemented", Verdict: "pass", Commit: conflictHead}},
		{name: "control: the default steps, the implementer's completion", at: phase.Implementing, asked: claim.RoleImplementer,
			done: intake.HandoffComplete{Generation: 1, Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim",
				Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, tc.at, "in_progress", settledGreen("lint"))
			if tc.plan {
				coderTester(t, pool, 1)
			}
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), tc.done)
			if got := issuePhase(t, pool); got != phase.Testing {
				t.Fatalf("after the completion at %s the issue is in %s, want testing", tc.at, got)
			}
			want := []record.MergeForwardMessage{toldOf(t, tc.asked, record.MergeForwardAsk, tc.at),
				toldOf(t, tc.asked, record.MergeForwardAsk, phase.Testing), toldOf(t, claim.RoleTester, record.MergeForwardHold, phase.Testing)}
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Errorf("merge-forward messages %+v, want %+v: testing asks again what the implementation step asked", got, want)
			}
			if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Phase != phase.Testing {
				t.Errorf("the record's merge forward = %+v, want the one asked in testing", asked)
			}
			if told := capNotices(t, pool, "checks-red"); told != 1 {
				t.Fatalf("%d checks-red notices, want one", told)
			}
			if _, stays := checksRedNotice(t, pool); stays != phase.Testing {
				t.Errorf("the checks-red notice names %s, want testing, the phase the issue stays in", stays)
			}
		})
	}
}

// A move into the task's implementation step drops the record's merge forward, as a move into
// implementing does: the agent of that step works the head there, and a head it pushes is read for
// a conflict of its own. In `coder tester` the tester's fail returns the task to the coder's step,
// which runs as step. The control is the tester's fail on the default steps, into implementing.
func TestAMoveIntoTheImplementationStepDropsTheMergeForward(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan bool
		back phase.Phase
	}{
		{name: "coder tester, the tester's fail", plan: true, back: phase.Step},
		{name: "control: the default steps, the tester's fail", back: phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := seedConflictIssue(t, phase.Testing, "testing", settledGreen("lint"))
			if tc.plan {
				coderTester(t, pool, 2)
			}
			confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
			if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked == nil || asked.Phase != phase.Testing {
				t.Fatalf("the record's merge forward = %+v, want the one asked in testing", asked)
			}
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleTester, Claim: "tester-claim", Summary: "tested", Verdict: "fail", Commit: conflictHead, HandoffRecord: "test-1", HandoffVersion: 1})
			if got := issuePhase(t, pool); got != tc.back {
				t.Fatalf("after the tester's fail the issue is in %s, want %s", got, tc.back)
			}
			if asked := recordedPR(t, pool, "LEGION-208").MergeForward; asked != nil {
				t.Fatalf("the record's merge forward = %+v, want none: the move into the implementation step drops it", asked)
			}
		})
	}
}
