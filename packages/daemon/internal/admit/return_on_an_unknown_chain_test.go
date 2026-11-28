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

// seedReturning seeds child stopped at backlog in implementing under parent, with its implementer's
// phases row and idle claims, and applies a person's todo on it: a return to work (R20).
func seedReturning(t *testing.T, pool *pgxpool.Pool, admission *Admission, top, parent, child string) {
	t.Helper()
	stopped := chainRecord(child, parent, phase.Implementing, "backlog")
	stopped.Tree, stopped.LastDispatchSeq = top, 1
	putIssue(t, pool, stopped)
	spSeedPhase(t, pool, record.PhaseRow{Issue: child, Role: claim.RoleImplementer, Claim: "implementer", Entry: 3,
		EntryTask: "Continue " + child + ".", EntryRaisedAt: fixedNow.Add(-2 * time.Hour)})
	putWorkerClaim(t, pool, top, child, claim.RoleImplementer, 1, supervise.StateIdle, "")
	putWorkerClaim(t, pool, top, child, claim.RoleArchitect, 1, supervise.StateIdle, "")
	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: child, Parent: parent, Rank: child}, admission.engine)
}

// R16 of dispatch://INFRA-3320: "On an issue whose chain is unknown the daemon starts nothing ...
// An issue whose chain is unknown that is not at work is listed among the held issues with the
// reason chain_unknown" (the architect's ruling,
// dispatch://INFRA-4730/comment/317d3111-94a2-4777-81e9-ea15937a8c78: a stopped issue gave its slot
// back, so its return is not at work, and its worker start is a start). A child the daemon ran,
// stopped at backlog, comes back on a person's todo while its parent's row is missing from the
// copy: it takes no slot, no start of its worker can run, it stands outside the waiting line, and
// the held list names it chain_unknown, held by its parent. Once the copy holds the parent, the
// child starts.
func TestAReturnOnAnUnknownChainStartsNothing(t *testing.T) {
	const top, middle, child = "LEGION-660", "LEGION-661", "LEGION-662"
	pool := migratedPool(t)
	admission := quiet(t, 4)
	seedReturning(t, pool, admission, top, middle, child)
	if holdsSlot(t, pool, child) {
		t.Errorf("%s holds a slot, want none while its chain is unknown", child)
	}
	for _, start := range pwWorkerStarts(t, pool, child) {
		if start.claimable && start.task != "" {
			t.Errorf("%s has a worker start that can run (%+v), want none while its chain is unknown", child, start)
		}
	}
	line, held := passJudgement(t, pool, admission)
	if slices.Contains(line, child) {
		t.Errorf("the waiting line = %v, want no %s while its chain is unknown", line, child)
	}
	assertHeldBy(t, held, child, record.HeldChainUnknown, middle)
	inTx(t, pool, func(tx pgx.Tx) {
		putCopyTx(t, tx, record.Issue{Key: middle, Project: testProject, Status: "done"})
	})
	tick(t, pool, admission, "chain-known")
	if !holdsSlot(t, pool, child) {
		t.Errorf("%s holds no slot once its chain is known, want it started", child)
	}
}
