package admit

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A stopped issue that returns to work under an R19 or R16 hold (ancestry, root_awaiting_person,
// or chain_unknown) stays out of the waiting line and takes no slot while the hold stands, and
// enters the line and takes a slot — making its owed worker start claimable — as soon as the hold
// ends (the parent returns to work, the top issue leaves awaiting_person, or the missing chain row
// appears in the copy).
func TestAReturningIssueStartsOnceItsHoldClears(t *testing.T) {
	const top, middle, child = "LEGION-660", "LEGION-661", "LEGION-662"
	for _, tc := range []struct {
		name   string
		parent string
		reason record.HeldReason
		holder string
		seed   func(t *testing.T, pool *pgxpool.Pool)
		clear  func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{
			name:   "ancestry: parent returns to work",
			parent: middle,
			reason: record.HeldAncestry,
			holder: top,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				seedRunning(t, pool, top, "")
				stoppedParent := chainRecord(middle, top, phase.Implementing, "backlog")
				stoppedParent.Tree, stoppedParent.LastDispatchSeq = top, 1
				putIssue(t, pool, stoppedParent)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "parent-todo", intake.DispatchIssue{Key: middle, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
					Status: "todo", Title: middle, Parent: top, Rank: middle}, admission.engine)
			},
		},
		{
			name:   "root_awaiting_person: top issue leaves awaiting_person",
			parent: top,
			reason: record.HeldRootAwaitingPerson,
			holder: top,
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				// No open design gate: an open gate answers the question wait on the first pass
				// (workflow's answerOpenGates), so top stays in awaiting_person until answered.
				putIssue(t, pool, quietRootRecord(top, top, phase.AwaitingPerson))
				takeSlot(t, pool, top)
				seedLifecycle(t, pool, top)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				answerWait(t, pool, top, record.AnsweredByAsk, "ask-"+top)
				tick(t, pool, admission, "top-answered")
			},
		},
		{
			name:   "chain_unknown: parent row arrives in the copy",
			parent: middle,
			reason: record.HeldChainUnknown,
			holder: middle,
			seed:   func(*testing.T, *pgxpool.Pool) {},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				inTx(t, pool, func(tx pgx.Tx) {
					putCopyTx(t, tx, record.Issue{Key: middle, Project: testProject, Status: "done"})
				})
				tick(t, pool, admission, "chain-known")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 4)
			tc.seed(t, pool)

			stopped := chainRecord(child, tc.parent, phase.Implementing, "backlog")
			stopped.Tree, stopped.LastDispatchSeq = top, 1
			putIssue(t, pool, stopped)
			spSeedPhase(t, pool, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
				EntryTask: "Continue " + child + ".", EntryRaisedAt: fixedNow.Add(-2 * time.Hour)})
			putWorkerClaim(t, pool, top, child, claim.RoleImplementer, 1, supervise.StateIdle, "")
			putWorkerClaim(t, pool, top, child, claim.RoleArchitect, 1, supervise.StateIdle, "")

			apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
				Status: "todo", Title: child, Parent: tc.parent, Rank: child}, admission.engine)

			if holdsSlot(t, pool, child) {
				t.Fatalf("%s holds a slot while %s holds it, want none", child, tc.reason)
			}
			for _, start := range pwWorkerStarts(t, pool, child) {
				if start.claimable && start.task != "" {
					t.Fatalf("%s has a claimable worker start while %s holds it (%+v), want none", child, tc.reason, start)
				}
			}
			line, held := passJudgement(t, pool, admission)
			if slices.Contains(line, child) {
				t.Fatalf("the waiting line = %v, want no %s while %s holds it", line, child, tc.reason)
			}
			assertHeldBy(t, held, child, tc.reason, tc.holder)

			tc.clear(t, pool, admission)

			if !holdsSlot(t, pool, child) {
				t.Fatalf("%s holds no slot after %s cleared, want it started", child, tc.reason)
			}
			claimable := false
			for _, start := range pwWorkerStarts(t, pool, child) {
				if start.role == claim.RoleImplementer && start.claimable && start.task != "" {
					claimable = true
				}
			}
			if !claimable {
				t.Fatalf("worker starts of %s after %s cleared = %+v, want a claimable implementer start", child, tc.reason, pwWorkerStarts(t, pool, child))
			}
			_, heldAfter := passJudgement(t, pool, admission)
			if row, still := heldAfter[child]; still {
				t.Fatalf("%s is still held as %+v after %s cleared, want it active", child, row, tc.reason)
			}
		})
	}
}
