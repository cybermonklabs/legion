package workflow

// A merge forward that lands on an issue past its review asks the reviewer to read the merge only
// where the issue's task has a reviewer step (mergeForwardLanded). A task whose plan has none
// stands on no review of the old head, and no reviewer step runs for it, so nobody would ever
// approve the new head: the reviewer is asked nothing, and the merger goes on at the new head,
// told no wait for an approval, both by the landed message that reaches it in merging and by the
// task it is started with when it enters merging after the landing. The control is the same issue
// on a plan with a reviewer step, where the review is asked and both tell the merger to wait.

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// approvalWait is the words that hold the merger's merge to the reviewer's approval of the head a
// merge forward left, in the landed message (record.MergeForwardMessage) and in its task (task).
const approvalWait = "only once the reviewer's approval of"

// plannedConflictIssue is seedConflictIssue on a run with plan rows: the owner's plan whose steps
// are steps, with task a running at the place of at (its retro or its merge).
func plannedConflictIssue(t *testing.T, at phase.Phase, steps ...string) *pgxpool.Pool {
	t.Helper()
	pool := seedConflictIssue(t, at, "retro", settledGreen("lint"))
	seedStepsPlan(t, pool, "LEGION-208", 1, steps...)
	place := record.TaskPlace{Kind: record.PlaceRetro}
	if at == phase.Merging {
		place = record.TaskPlace{Kind: record.PlaceMerge}
	}
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().MoveTask(t.Context(), tx, "LEGION-208", 1, "a", place, record.TaskRunning)
	})
	return pool
}

// toldWho is each merge-forward message as the role it goes to and its step, in the order written.
func toldWho(messages []record.MergeForwardMessage) []string {
	told := make([]string, len(messages))
	for i, message := range messages {
		told[i] = string(message.Role) + " " + string(message.Step)
	}
	return told
}

func TestAMergeForwardAsksTheReviewerOnlyOfATaskWithAReviewerStep(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	for _, tc := range []struct {
		name   string
		steps  []string
		review bool
	}{
		{name: "a plan with no reviewer step", steps: []string{"implementer", "tester"}, review: false},
		{name: "control: a plan with a reviewer step", steps: []string{"implementer", "reviewer"}, review: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("the merger in merging", func(t *testing.T) {
				pool := plannedConflictIssue(t, phase.Merging, tc.steps...)
				confirmsConflict(t, pool, requiresCI)
				asked := len(mergeForwardMessages(t, pool))
				landsMergeForward(t, pool)

				got := mergeForwardMessages(t, pool)[asked:]
				want := []string{"merger landed"}
				if tc.review {
					want = append(want, "reviewer review")
				}
				if !reflect.DeepEqual(toldWho(got), want) {
					t.Fatalf("the landing queued %v, want %v", toldWho(got), want)
				}
				// The text is composed as the outbox composes it at delivery: from the pull request
				// as the record holds it (the daemon's mergeForwardMessage).
				recorded := recordedPR(t, pool, "LEGION-208")
				if told := got[0].Message("LEGION-208", recorded); strings.Contains(told, approvalWait) != tc.review {
					t.Fatalf("the merger was told %q; want a wait for the reviewer's approval: %t", told, tc.review)
				}
				if mf := recorded.MergeForward; mf == nil || mf.Landed != forwardHead || mf.ReviewAsked != tc.review {
					t.Fatalf("the record's merge forward = %+v, want it landed at %s with the review asked: %t", mf, forwardHead, tc.review)
				}
				if got := issuePhase(t, pool); got != phase.Merging {
					t.Fatalf("after the merge landed the issue is in %s, want merging", got)
				}
			})

			t.Run("a merger entering merging after the landing", func(t *testing.T) {
				pool := plannedConflictIssue(t, phase.Retro, tc.steps...)
				confirmsConflict(t, pool, requiresCI)
				asked := len(mergeForwardMessages(t, pool))
				landsMergeForward(t, pool)
				want := []string{}
				if tc.review {
					want = append(want, "reviewer review")
				}
				if got := toldWho(mergeForwardMessages(t, pool)[asked:]); !reflect.DeepEqual(got, want) {
					t.Fatalf("the landing in retro queued %v, want %v", got, want)
				}

				applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
					Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: forwardHead})
				if got := issuePhase(t, pool); got != phase.Merging {
					t.Fatalf("after retro's completion the issue is in %s, want merging", got)
				}
				if task := startTask(t, pool, claim.RoleMerger); task == "" || strings.Contains(task, approvalWait) != tc.review {
					t.Fatalf("the merger's task reads %q; want a wait for the reviewer's approval: %t", task, tc.review)
				}
			})
		})
	}
}
