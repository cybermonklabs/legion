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
// are steps, with task a running at the place of at (its first step, its retro or its merge), the
// Dispatch status of at, and pr as the record holds the pull request.
func plannedConflictIssue(t *testing.T, at phase.Phase, pr record.PullRequest, steps ...string) *pgxpool.Pool {
	t.Helper()
	pool := seedConflictIssue(t, at, PhaseStatus(at), pr)
	seedStepsPlan(t, pool, "LEGION-208", 1, steps...)
	place := record.TaskPlace{Kind: record.PlaceRetro}
	switch at {
	case phase.Implementing:
		place = record.TaskPlace{Kind: record.PlaceStep, Position: 1}
	case phase.Merging:
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
		last   phase.Phase // the phase of the plan's last step
		review bool
	}{
		{name: "a plan with no reviewer step", steps: []string{"implementer", "tester"}, last: phase.Testing, review: false},
		{name: "control: a plan with a reviewer step", steps: []string{"implementer", "reviewer"}, last: phase.Reviewing, review: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("the merger in merging", func(t *testing.T) {
				pool := plannedConflictIssue(t, phase.Merging, settledGreen("lint"), tc.steps...)
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

			// Past the review a conflict waits for the merger (cybermonklabs/legion#4979), so a merge
			// forward lands in retro only where the implementer was asked in the plan's last step,
			// while a required check had no green result, and its merge lands once the issue is in
			// retro.
			t.Run("a merger entering merging after the landing", func(t *testing.T) {
				pr := settledGreen("lint")
				pr.MergeForward = &record.MergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer, Phase: tc.last,
					Why: record.ConflictCheckNotGreen, AskedAt: firstConflictRead}
				pool := plannedConflictIssue(t, phase.Retro, pr, tc.steps...)
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

// An ask of the implementer made in implementing covers implementing alone, so the move out of
// implementing asks again where the next phase comes before the review (mergeForwardOnEntry).
// Past the review the conflict waits for the merger (cybermonklabs/legion#4979). An owner's plan
// with no tester and no reviewer step, `implementer` or `implementer acceptance:owner`, takes the
// implementer's completion at the conflicting head without the merge straight into retro or
// accepting: that move asks nothing, holds nobody and tells the architect nothing, the ask of
// implementing stays on the record, and the move into merging asks the merger. The control,
// `implementer tester`, asks again in testing, with the tester's hold and the checks-red notice.
func TestAnAskOfImplementingIsAskedAgainOnlyBeforeTheReview(t *testing.T) {
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	ask := func(role claim.Role, step record.MergeForwardStep, at phase.Phase, why record.ConflictWhy) record.MergeForwardMessage {
		return record.MergeForwardMessage{Role: role, Step: step, Head: conflictHead, Base: "main", Phase: at, PullRequest: 42, Why: why}
	}
	inImplementing := ask(claim.RoleImplementer, record.MergeForwardAsk, phase.Implementing, record.ConflictCheckNotGreen)
	for _, tc := range []struct {
		name  string
		steps []string
		next  phase.Phase
		// again is what the move out of implementing queues.
		again []record.MergeForwardMessage
	}{
		{name: "the plan implementer", steps: []string{"implementer"}, next: phase.Retro},
		{name: "the plan implementer acceptance:owner", steps: []string{"implementer", "acceptance:owner"}, next: phase.Accepting},
		{name: "control: the plan implementer tester", steps: []string{"implementer", "tester"}, next: phase.Testing,
			again: []record.MergeForwardMessage{ask(claim.RoleImplementer, record.MergeForwardAsk, phase.Testing, record.ConflictCheckNotGreen),
				ask(claim.RoleTester, record.MergeForwardHold, phase.Testing, record.ConflictCheckNotGreen)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), tc.steps...)
			confirmsConflict(t, pool, requiresCI)
			askedOnce(t, pool, phase.Implementing, "in_progress", record.ConflictCheckNotGreen, "")
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "implemented", Commit: conflictHead, HandoffRecord: "implement-1", HandoffVersion: 1})
			if got := issuePhase(t, pool); got != tc.next {
				t.Fatalf("after the implementer's completion the issue is in %s, want %s", got, tc.next)
			}
			want := append([]record.MergeForwardMessage{inImplementing}, tc.again...)
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the move into %s the merge-forward messages are %+v, want %+v", tc.next, got, want)
			}
			told := 0
			if len(tc.again) > 0 {
				told = 1
			}
			if got := capNotices(t, pool, "checks-red"); got != told {
				t.Fatalf("after the move into %s the architect has %d checks-red notices, want %d", tc.next, got, told)
			}
			if len(tc.again) > 0 {
				return
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Role != claim.RoleImplementer || mf.Phase != phase.Implementing {
				t.Fatalf("the record's merge forward = %+v, want the implementer's ask of implementing", mf)
			}

			if tc.next == phase.Accepting {
				engine := readsAfter(3 * time.Minute)
				applyRefusingNothing(t, pool, engine, rootsSignOff(t, pool, engine, "LEGION-208"))
				if got := issuePhase(t, pool); got != phase.Retro {
					t.Fatalf("after the owner's sign-off the issue is in %s, want retro", got)
				}
			}
			applyRefusingNothing(t, pool, readsAfter(4*time.Minute), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
				Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
			if got := issuePhase(t, pool); got != phase.Merging {
				t.Fatalf("after retro's completion the issue is in %s, want merging", got)
			}
			want = append(want, ask(claim.RoleMerger, record.MergeForwardAsk, phase.Merging, record.ConflictMergeNext))
			if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
				t.Fatalf("after the move into merging the merge-forward messages are %+v, want %+v: the merger is asked", got, want)
			}
		})
	}
}
