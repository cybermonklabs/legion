package workflow

// A merge forward is asked at step as at the phases of Legion's own workers that read the head: an
// issue whose task runs a step of a role the deployment defines, with its head confirmed in
// conflict with its base, keeps its step, the implementer is asked to merge the base forward, and
// the role of that step is told to hold (classify.MergeForwardStands over the task's line, on which
// that step stands as step; record.MergeForwardReaches); the landing of the merge tells that role
// it landed. The control is the same task's tester step, whose tester is told the same way.

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
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
				return seedMove(t.Context(), tx, "LEGION-208", 1, "a", record.TaskPlace{Kind: record.PlaceStep, Position: 2}, record.TaskRunning)
			})
			if tc.at == phase.Step {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.worker, Claim: claim.Token(string(tc.worker) + "-claim"), Entry: 1})
			}

			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, tc.at, tc.status, record.ConflictCheckNotGreen, tc.worker)

			asked := len(mergeForwardMessages(t, pool))
			landsMergeForward(t, pool)
			want := []string{string(tc.worker) + " landed"}
			if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, want) {
				t.Fatalf("the landing queued %v, want %v", got, want)
			}
			if got := issuePhase(t, pool); got != tc.at {
				t.Fatalf("after the merge landed the issue is in %s, want it still in %s", got, tc.at)
			}
		})
	}
}
