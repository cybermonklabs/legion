package workflow

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A CI settlement tells the architect, the worker of the issue's phase and the implementer what CI
// reported, one role message each, in every phase a worker holds, and starts no worker and moves
// nothing: the tester reads no CI, the implementer learns of a failed check a CI job runs from its
// own message (tellCIFailure), and the reviewer and the architect read CI's state as their prompts
// say. The test reads to whom each message goes and what the outbox queued beside it, never a word
// of a message. The wake of a released role by a settlement is TestACISettlementWakesTheReleasedPhaseWorker's.
func TestACISettlementTellsEachRoleWhatCIReportedAndStartsNothing(t *testing.T) {
	for _, tc := range []struct {
		from   phase.Phase
		status string
		worker claim.Role
		// told are the roles the settlement tells, in the order tellChecks addresses them.
		told []claim.Role
	}{
		{phase.Implementing, "in_progress", claim.RoleImplementer, []claim.Role{claim.RoleArchitect, claim.RoleImplementer}},
		{phase.Testing, "testing", claim.RoleTester, []claim.Role{claim.RoleArchitect, claim.RoleTester, claim.RoleImplementer}},
		{phase.Reviewing, "needs_review", claim.RoleReviewer, []claim.Role{claim.RoleArchitect, claim.RoleReviewer, claim.RoleImplementer}},
	} {
		t.Run(string(tc.from), func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.from, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			if tc.worker != claim.RoleImplementer {
				seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: tc.worker, Claim: claim.Token(string(tc.worker) + "-claim")})
			}
			applyRefusingNothing(t, pool, mergingEngine(), settledAt("head", 1, "unit"))
			want := make([]string, 0, len(tc.told))
			for _, role := range tc.told {
				want = append(want, roleToken(t, role))
			}
			if got := roleMessageTopics(t, pool, "LEGION-208"); !slices.Equal(got, want) {
				t.Fatalf("the settlement told %v, want one role message to each of %v", got, want)
			}
			assertOutboxCount(t, pool, "supervise", 0)
			if got := issuePhase(t, pool); got != tc.from {
				t.Fatalf("after the settlement the issue is in %s, want it left in %s", got, tc.from)
			}
		})
	}
}

// roleMessageTopics is the role each role message the outbox queued for issue goes to, in order.
func roleMessageTopics(t *testing.T, pool *pgxpool.Pool, issue string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select payload->>'role' from outbox where kind = 'role_message' and issue = $1 order by id", issue)
	if err != nil {
		t.Fatalf("read the role messages: %v", err)
	}
	defer rows.Close()
	var roles []string
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			t.Fatalf("read a role message: %v", err)
		}
		roles = append(roles, role)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the role messages: %v", err)
	}
	return roles
}
