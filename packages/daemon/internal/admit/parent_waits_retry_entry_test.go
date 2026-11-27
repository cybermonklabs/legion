package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// TestParentWaitsHoldsTheWorkOfAnEntryRaisedAfterARetryOfTheOneBefore: the task of a retry is
// kept on the entry it was given for (record.PhaseRow.Given, GivenFor) and no longer. The
// implementer is retried on entry 3 and completes implementing. The tester's start raises the
// issue's next entry. The tester's failure then raises the entry after it for the implementer,
// with the work of that failure. The issue has one counter for its entries (workflow's enter).
// When a child then opens with the implementer in the middle of that entry, the admission pass
// holds that entry's work. It does not hold the task of the retry of
// entry 3, and the return starts implementing on it (dispatch://INFRA-3320 R9 and R12).
func TestParentWaitsHoldsTheWorkOfAnEntryRaisedAfterARetryOfTheOneBefore(t *testing.T) {
	pool := migratedPool(t)
	admission := pwAdmission(t, 2)
	spSeedParent(t, pool, spParent, phase.Implementing, true, nil)
	pwPullRequest(t, pool, spParent, record.PullRequestOpen)
	token := pwRoleToken(t, spParent, claim.RoleImplementer)
	spSeedPhase(t, pool, record.PhaseRow{Issue: spParent, Role: claim.RoleImplementer, Claim: token, Entry: 3,
		EntryTask: pwTask(spParent, phase.Implementing), EntryRaisedAt: fixedNow.Add(-time.Hour)})
	pwApplied(t, pool, admission, "supervisor", "failed", intake.ClaimFailed{Issue: spParent, Role: claim.RoleImplementer,
		Reason: "the launch budget ran out"})
	pwApplied(t, pool, admission, "api", "retry", intake.RetryOrEscalate{Issue: spParent, Decision: intake.RetryDecision,
		Reason: "the runner is back"})
	retry := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
	if retry.Entry != 3 || !strings.Contains(retry.Task, "Reason: the runner is back.") {
		t.Fatalf("the retry's start = %+v; want the retry's own task on entry 3", retry)
	}
	putWorkerClaim(t, pool, spParent, spParent, claim.RoleImplementer, 1, supervise.StateIdle, "")
	pwDeliver(t, pool, spParent, token, retry)

	pwApplied(t, pool, admission, "api", "implemented", intake.HandoffComplete{Generation: 1, Issue: spParent,
		Role: claim.RoleImplementer, Claim: token, Summary: "implemented after the retry", Commit: "head-42",
		HandoffRecord: "implement-2", HandoffVersion: 1})
	if got := issue(t, pool, spParent); got.Phase != phase.Testing {
		t.Fatalf("the parent after the implementer's completion = phase %s, want testing", got.Phase)
	}
	tester := pwRoleToken(t, spParent, claim.RoleTester)
	putWorkerClaim(t, pool, spParent, spParent, claim.RoleTester, 1, supervise.StateIdle, "")
	tested := pnWorkerStart(t, pool, spParent, claim.RoleTester)
	if tested.Entry <= retry.Entry {
		t.Fatalf("the tester's start = %+v; want an entry above the implementer's entry %d: the issue has one counter for its entries",
			tested, retry.Entry)
	}
	pwDeliver(t, pool, spParent, tester, tested)
	pwApplied(t, pool, admission, "api", "failed", intake.HandoffComplete{Generation: 1, Issue: spParent, Role: claim.RoleTester,
		Claim: tester, Summary: "a check failed", Verdict: "fail", Commit: "head-42",
		HandoffRecord: "test-1", HandoffVersion: 1})
	if got := issue(t, pool, spParent); got.Phase != phase.Implementing {
		t.Fatalf("the parent after the tester's failure = phase %s, want implementing", got.Phase)
	}
	fixed := pnWorkerStart(t, pool, spParent, claim.RoleImplementer)
	if fixed.Entry <= tested.Entry || strings.Contains(fixed.Task, "the runner is back") {
		t.Fatalf("the implementer's start after the tester's failure = %+v; want an entry above the tester's entry %d, on the work of that failure",
			fixed, tested.Entry)
	}
	// The turn that took the retry ended with the implementer's completion. That completion retires
	// the claim's pending delivery (store's RetireDelivery). The lane then delivers the entry the
	// tester's failure raised.
	if _, err := pool.Exec(context.Background(), `delete from pending_task_deliveries where claim_token = $1 and delivery_id = 'outbox:delivered'`,
		string(token)); err != nil {
		t.Fatalf("retire the retry's delivery as the end of the turn that took it does: %v", err)
	}
	pwDeliver(t, pool, spParent, token, fixed)
	seedMirrorChild(t, pool, spChild, "todo", spParent)

	spPass(t, pool, admission)

	wait := pwHeld(t, pool, spParent, phase.Implementing, "the admission pass stopped the implementer's implementing phase", spChild+" (todo)")
	if wait.Task != fixed.Task {
		t.Fatalf("the held task = %q, want the work entry %d was raised with, %q, not the task of the retry of entry 3, %q",
			wait.Task, fixed.Entry, fixed.Task, retry.Task)
	}

	pnChildMerged(t, pool, spChild, spParent, 51)
	spPass(t, pool, admission)

	if back := issue(t, pool, spParent); back.Phase != phase.Implementing || back.ChildrenWait != nil {
		t.Fatalf("the parent after its child is done = phase %s, held start %+v; want implementing, the start it held", back.Phase, back.ChildrenWait)
	}
	work := fixed.Task + " " + pnMergedChildren(spParent, pnMerged(spChild, 51)) + "."
	if starts := pwWorkerStarts(t, pool, spParent); len(starts) != 1 || starts[0].role != claim.RoleImplementer || starts[0].task != work {
		t.Fatalf("starts of %s = %+v, want one implementer start on entry %d's work %q", spParent, starts, fixed.Entry, work)
	}
}
