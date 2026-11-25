package workflow

import (
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// One merge of one head has one asked role at a time. An ask of the implementer made in
// implementing covers implementing alone, so an implementer that completes at the conflicting head
// without the merge leaves the merge to the next phase. Where no step of the task runs before the
// merge, the move into the retro asks nobody: the merge comes next, so the implementer is told
// once that the merge is no longer its work (record.MergeForwardWithdrawn) and its ask is
// delivered no more (leftMergeForwardAt). The move into merging then asks the merger, with no
// second withdrawal, and the merger waits for no landing. An implementer that pushes a merge after
// that pushes a new head, which the merger's ask follows as any landing does: the merger is told
// the old head and the new one. The control is an ask of the implementer made after implementing,
// in reviewing, which the implementer keeps into merging: the merger is held, the implementer is
// told nothing more, and its ask is still delivered.
func TestTheMergersAskEndsTheImplementersAskOfTheSameHead(t *testing.T) {
	t.Parallel()
	requiresCI := &record.BaseRules{Required: []string{"ci"}}
	left := func(t *testing.T, pool *pgxpool.Pool, role claim.Role) string {
		t.Helper()
		pr := recordedPR(t, pool, "LEGION-208")
		return leftMergeForwardAt(storedIssue(t, pool, "LEGION-208"), false, &pr, role, record.MergeForwardAsk, conflictHead)
	}
	retroCompletes := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		applyRefusingNothing(t, pool, readsAfter(150*time.Second), intake.HandoffComplete{Generation: 1, Issue: "LEGION-208",
			Role: claim.RoleImplementer, Claim: "implement-claim", Summary: "retro done", Commit: conflictHead})
		if got := issuePhase(t, pool); got != phase.Merging {
			t.Fatalf("after the retro's completion the issue is in %s, want merging", got)
		}
	}

	t.Run("an ask made in implementing, then the moves into the retro and merging", func(t *testing.T) {
		pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer")
		confirmsConflict(t, pool, requiresCI)
		completesImplementingAt(t, pool)
		if got := issuePhase(t, pool); got != phase.Retro {
			t.Fatalf("after the implementer's completion the issue is in %s, want retro", got)
		}
		why := record.ConflictCheckNotGreen
		want := []record.MergeForwardMessage{implementerAsked(phase.Implementing, why), withdrawnIn(phase.Retro, why)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Errorf("after the move into the retro the merge-forward messages are %+v, want %+v", got, want)
		}
		if why := left(t, pool, claim.RoleImplementer); why == "" {
			t.Errorf("in the retro the implementer's ask of %s is still delivered, want it ended: the merge comes next", conflictHead)
		}

		retroCompletes(t, pool)
		want = append(want, mergerAsked())
		asked := mergeForwardMessages(t, pool)
		if !reflect.DeepEqual(asked, want) {
			t.Errorf("after the move into merging the merge-forward messages are %+v, want %+v: the merger is asked, with no second withdrawal",
				asked, want)
		}
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleMerger || mf.Head != conflictHead {
			t.Errorf("the record's merge forward = %+v, want the merger's ask of %s", mf, conflictHead)
		}
		if why := left(t, pool, claim.RoleMerger); why != "" {
			t.Errorf("the merger's ask is undelivered (%s), want it delivered", why)
		}

		// The implementer pushes its merge anyway: a new head, which the merger's ask follows.
		landsMergeForward(t, pool)
		if got := toldWho(mergeForwardMessages(t, pool)[len(asked):]); !reflect.DeepEqual(got, []string{"merger landed"}) {
			t.Errorf("the implementer's push after the merger was asked queued %v, want the merger told the new head", got)
		}
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Asked() != claim.RoleMerger || mf.Landed != forwardHead {
			t.Errorf("the record's merge forward = %+v, want the merger's ask landed at %s", mf, forwardHead)
		}
	})

	t.Run("control: an ask made in reviewing, kept into merging", func(t *testing.T) {
		pool := seedConflictIssue(t, phase.Retro, "retro", askedInReviewing())
		confirmsConflict(t, pool, requiresCI)
		retroCompletes(t, pool)
		want := []record.MergeForwardMessage{heldIn(claim.RoleMerger, phase.Merging, record.ConflictCheckNotGreen)}
		if got := mergeForwardMessages(t, pool); !reflect.DeepEqual(got, want) {
			t.Errorf("after the move into merging the merge-forward messages are %+v, want the merger's one hold %+v", got, want)
		}
		if why := left(t, pool, claim.RoleImplementer); why != "" {
			t.Errorf("the implementer's ask is undelivered (%s), want it delivered: the merge is still the implementer's", why)
		}
	})
}
