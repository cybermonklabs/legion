package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A parent's move into awaiting_children drops the merge forward its record holds (awaitChildren),
// and names it to the role it was asked of only while it is the open ask of the head (openAsk)
// and not withdrawn: an ask that landed, an ask on a pull request that is closed, and an ask the
// implementer was told is no longer its work (record.MergeForward.Withdrawn, withdrawUnowed) are
// dropped with no message. Otherwise the implementer, told on the move out of implementing to
// merge nothing and push no merge for that head, would be told by the wait to finish that merge
// and push it; a planner the wait stops would be told to push nothing for a merge nobody owes;
// and a role would be told to push a merge onto a pull request that takes no push.
//
// Each case is LEGION-208, asked in implementing to merge the base forward onto the conflicting
// head, then stopped by the admission pass for an open child (StopForChildren). The withdrawals
// come on the move into accepting (`implementer acceptance:owner`) and on the implementer's move
// back into planning, the close in implementing. Each control is the same parent with the ask
// still open: the withdrawal mark taken off the record's ask, or no close. The control names the
// dropped merge forward to the implementer (dispatch://INFRA-3320 R10): in its phase-ended message
// where the stop ends its phase, and otherwise in a role message, with the planner's phase-ended
// message carrying it too.
func TestAMergeForwardNobodyOwesIsDroppedByAWaitOnChildrenUnnamed(t *testing.T) {
	t.Parallel()
	dropped := record.DroppedMergeForward{Head: conflictHead, Base: "main", Role: claim.RoleImplementer}
	withdrawnTo := func(t *testing.T, at phase.Phase, pool *pgxpool.Pool) {
		t.Helper()
		if got := issuePhase(t, pool); got != at {
			t.Fatalf("after the move the issue is in %s, want %s", got, at)
		}
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != conflictHead || mf.Landed != "" || mf.Asked() != claim.RoleImplementer || !mf.Withdrawn {
			t.Fatalf("the record's merge forward in %s = %+v, want the implementer's ask of %s, not landed and withdrawn", at, mf, conflictHead)
		}
	}
	intoAccepting := func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		pool := plannedConflictIssue(t, phase.Implementing, settledGreen("lint"), "implementer", "acceptance:owner")
		confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
		completesImplementingAt(t, pool)
		withdrawnTo(t, phase.Accepting, pool)
		return pool
	}
	backIntoPlanning := func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
		confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
		applyRefusingNothing(t, pool, readsAfter(2*time.Minute), intake.BackwardMove{Issue: "LEGION-208", Requester: claim.RoleImplementer,
			To: phase.Planning, Reason: "the plan misses a case"})
		withdrawnTo(t, phase.Planning, pool)
		return pool
	}
	askedInImplementing := func(t *testing.T) *pgxpool.Pool {
		t.Helper()
		pool := seedConflictIssue(t, phase.Implementing, "in_progress", settledGreen("lint"))
		confirmsConflict(t, pool, &record.BaseRules{Required: []string{"ci"}})
		if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf == nil || mf.Head != conflictHead || mf.Withdrawn {
			t.Fatalf("the record's merge forward = %+v, want the implementer's open ask of %s", mf, conflictHead)
		}
		return pool
	}
	for _, tc := range []struct {
		name  string
		pool  func(t *testing.T) *pgxpool.Pool
		at    phase.Phase
		ended claim.Role
		// named is whether the stop names the dropped merge forward: only in a control, whose ask
		// is still open.
		named bool
	}{
		{name: "the ask withdrawn on the move into accepting", pool: intoAccepting, at: phase.Accepting},
		{name: "control: the same with the withdrawal mark taken off", at: phase.Accepting, named: true, pool: func(t *testing.T) *pgxpool.Pool {
			t.Helper()
			return unwithdrawn(t, intoAccepting(t))
		}},
		{name: "the ask withdrawn on the implementer's move back into planning", pool: backIntoPlanning, at: phase.Planning, ended: claim.RolePlanner},
		{name: "control: the same with the withdrawal mark taken off", at: phase.Planning, ended: claim.RolePlanner, named: true, pool: func(t *testing.T) *pgxpool.Pool {
			t.Helper()
			return unwithdrawn(t, backIntoPlanning(t))
		}},
		{name: "the ask on a pull request that closed", at: phase.Implementing, ended: claim.RoleImplementer, pool: func(t *testing.T) *pgxpool.Pool {
			t.Helper()
			pool := askedInImplementing(t)
			applyRefusingNothing(t, pool, readsAfter(2*time.Minute), closedAt(conflictHead, firstConflictRead.Add(2*time.Minute)))
			if pr := recordedPR(t, pool, "LEGION-208"); pr.State != record.PullRequestClosed || pr.MergeForward == nil || pr.MergeForward.Head != conflictHead {
				t.Fatalf("after the close the pull request is %s with the ask %+v, want it closed with the ask kept", pr.State, pr.MergeForward)
			}
			return pool
		}},
		{name: "control: the same pull request open", pool: askedInImplementing, at: phase.Implementing, ended: claim.RoleImplementer, named: true},
	} {
		t.Run(string(tc.at)+": "+tc.name, func(t *testing.T) {
			t.Parallel()
			pool := tc.pool(t)
			messages := len(childrenRoleMessages(t, pool))
			ends := len(cwPhaseEnded(t, pool, "LEGION-208"))

			stopsForChild(t, pool)

			if got := issuePhase(t, pool); got != phase.AwaitingChildren {
				t.Fatalf("after the stop the issue is in %s, want awaiting_children", got)
			}
			if mf := recordedPR(t, pool, "LEGION-208").MergeForward; mf != nil {
				t.Errorf("the record's merge forward after the stop = %+v, want it cleared", mf)
			}
			var want []string
			var named *record.DroppedMergeForward
			if tc.named {
				named = &dropped
				if tc.ended != dropped.Role {
					want = []string{dropped.RoleMessageBody("LEGION-208")}
				}
			}
			if told := childrenRoleMessages(t, pool)[messages:]; !reflect.DeepEqual(told, want) {
				t.Errorf("the role messages the stop sent are %q, want %q", told, want)
			}
			ended := cwPhaseEnded(t, pool, "LEGION-208")[ends:]
			if tc.ended == "" {
				if len(ended) != 0 {
					t.Errorf("the phase-ended messages the stop sent are %+v, want none: it ends no worker's phase in %s", ended, tc.at)
				}
				return
			}
			if len(ended) != 1 || ended[0].Role != tc.ended || !reflect.DeepEqual(ended[0].MergeForward, named) {
				t.Errorf("the phase-ended messages the stop sent are %+v, want one to the %s naming the dropped merge forward %+v", ended, tc.ended, named)
			}
		})
	}
}

// unwithdrawn is pool with the withdrawal mark taken off the record's ask, the one field that
// stands between that ask and an open one nobody was told to leave.
func unwithdrawn(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	pr := recordedPR(t, pool, "LEGION-208")
	owed := *pr.MergeForward
	owed.Withdrawn = false
	pr.MergeForward = &owed
	seedRecord(t, pool, func(tx pgx.Tx) error { return record.NewStore().PutPullRequest(t.Context(), tx, pr) })
	return pool
}

// childrenRoleMessages is the body of every role message queued for LEGION-208, oldest first.
func childrenRoleMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var bodies []string
	for _, row := range issueOutboxOf(t, pool, "LEGION-208", string(record.OutboxKindRoleMessage)) {
		var message record.RoleMessage
		if err := json.Unmarshal(row.payload, &message); err != nil {
			t.Fatalf("decode role-message row %d: %v", row.id, err)
		}
		bodies = append(bodies, message.Body)
	}
	return bodies
}

// stopsForChild is the admission pass's stop of LEGION-208 for its open Dispatch child LEGION-209
// (StopForChildren), which moves it into awaiting_children.
func stopsForChild(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	engine := readsAfter(3 * time.Minute)
	if err := store.Transact(t.Context(), pool, func(ctx context.Context, tx pgx.Tx) error {
		issue, err := record.NewStore().Issue(ctx, tx, "LEGION-208")
		if err != nil {
			return err
		}
		moved, err := engine.StopForChildren(ctx, tx, *issue, []record.OpenChild{{Key: "LEGION-209", Status: "todo"}}, nil)
		if err == nil && !moved {
			return fmt.Errorf("the stop left LEGION-208 in %s, want it moved to awaiting_children", issue.Phase)
		}
		return err
	}); err != nil {
		t.Fatalf("the stop for LEGION-209: %v", err)
	}
}
