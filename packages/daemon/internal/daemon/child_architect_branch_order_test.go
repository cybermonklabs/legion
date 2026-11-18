package daemon

import (
	"net/http"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// No role of an issue starts before its issue's branch exists (record's outboxInOrder holds a
// start behind its issue's older branch row, and gateOpened queues the child's architect start
// after the branch row for that reason). A child architect's first start that waited while its
// tree lingered keeps that order when the tree runs again: the re-admission queues a branch row of
// its own for the child, newer than the waiting start, and while GitHub has not created the
// branch the waiting start must not create and launch the child's architect.
func TestAWaitingChildArchitectStartWaitsForTheReadmissionsBranch(t *testing.T) {
	rig := newChildArchitectRig(t)
	token := mustClaimToken(t, doorChild, claim.RoleArchitect)
	parkTheRootBeforeTheChildsRowsRun(rig)

	// GitHub fails every create of a ref from now on, as an outage does, so the branch row retries.
	gh := newBranchGitHub(t, nil, func() (int, string) { return http.StatusBadGateway, `{"message":"bad gateway"}` })
	rig.runner.githubAPI = gh.url
	reopenTheRoot(rig)

	branchRows := func() int {
		var n int
		if err := rig.pool.QueryRow(rig.ctx, `select count(*) from outbox where issue = $1 and kind = 'issue_branch'`, doorChild).Scan(&n); err != nil {
			t.Fatalf("count the branch rows of %s: %v", doorChild, err)
		}
		return n
	}
	if branchRows() == 0 {
		t.Fatal("the re-admission queued no branch row of the child; the scenario does not hold")
	}
	attempts := func() int {
		var n int
		if err := rig.pool.QueryRow(rig.ctx, `select coalesce(max(attempts), -1) from outbox where issue = $1 and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'architect'`, doorChild).Scan(&n); err != nil {
			t.Fatalf("read the attempts of the child's architect start: %v", err)
		}
		return n
	}
	before := attempts()
	testwait.Eventually(t, "another attempt of the waiting start of the child's architect once its tree runs again", func() bool {
		rig.drain()
		now := attempts()
		return now == -1 || now > before
	})
	if _, found := rig.sup.Machine(token); found && branchRows() > 0 {
		t.Fatalf("the child's architect claim was created and launched while the re-admission's branch row of %s still waits (GitHub created %d refs), want the start held until the branch exists",
			doorChild, len(gh.created()))
	}
}
