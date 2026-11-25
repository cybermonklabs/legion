package workflow

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A claim failed on OpenAI's cyber-policy refusal is relaunched by no start of the daemon's own
// (supervise.ErrHeldForCyberPolicy), so the worker-died notices that promise such a start say
// otherwise for it: a child's own architect is not started again at its child's next phase start,
// and a role that fails outside its issue's phase is not relaunched by that phase's next start
// (the checking role's notice is TestAProductionCheckRoleFailedForCyberPolicyIsPromisedNoLook's).
// Each names the operator's `legion claims retry`. A claim failed for another cause keeps the
// promise, which is the control: the same notice for a death still names the next phase start.
func TestAWorkerDiedForCyberPolicyPromisesNoRestartOfTheDaemonsOwn(t *testing.T) {
	root := "LEGION-208"
	liveRoot := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child",
		Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"}
	apply := func(t *testing.T, pool *pgxpool.Pool, role claim.Role, cause supervise.FailureCause) string {
		t.Helper()
		failed := intake.ClaimFailed{Issue: child.Key, Role: role, Cause: cause,
			Reason:  "a provider error that cannot clear ended a turn this claim's agent ran with no prompt of the daemon's: OpenAI's cyber_policy refusal of this session's content",
			Budgets: supervise.Budgets{}, Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 5, PromptRetires: 2}}
		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "worker-died-"+string(role), failed, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the %s's failed claim: %v", role, err)
		}
		rows := noticeRows(t, pool)
		if len(rows) != 1 {
			t.Fatalf("notice rows = %+v, want the one worker-died", rows)
		}
		return rows[0].(record.Notice).Reason
	}
	for _, tc := range []struct {
		name    string
		role    claim.Role
		cause   supervise.FailureCause
		promise string
	}{
		{"a child's own architect", claim.RoleArchitect, supervise.CauseCyberPolicy, ""},
		{"a role outside its issue's phase", claim.RoleReviewer, supervise.CauseCyberPolicy, ""},
		{"a child's own architect that died", claim.RoleArchitect, supervise.CauseDied, "the daemon starts it again at LEGION-209's next phase start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, liveRoot)
			seedIssue(t, pool, child)
			reason := apply(t, pool, tc.role, tc.cause)
			if tc.promise != "" {
				if !strings.Contains(reason, tc.promise) || strings.Contains(reason, "cyber_policy") {
					t.Fatalf("the worker-died says %q, want the daemon's own start promised: %q", reason, tc.promise)
				}
				return
			}
			for _, want := range []string{"cyber_policy", "legion claims retry", "--fresh-session"} {
				if !strings.Contains(reason, want) {
					t.Errorf("the worker-died says %q, want it to name %q", reason, want)
				}
			}
			if strings.Contains(reason, "starts it again") {
				t.Errorf("the worker-died says %q, which promises a start of the daemon's own that never comes", reason)
			}
		})
	}
}

// The checking role of a production check is relaunched by the look after a failed one, a start
// of the daemon's own, so its worker-died names that look (checkRoleFailedReason). A checking role
// failed on OpenAI's cyber-policy refusal is retried by no look, and its worker-died says so and
// names the operator's `legion claims retry`. The control is the same failure for a provider error
// that cannot clear, whose worker-died still names the look that retries the claim.
func TestAProductionCheckRoleFailedForCyberPolicyIsPromisedNoLook(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause supervise.FailureCause
		look  bool
	}{
		{"a cyber_policy refusal", supervise.CauseCyberPolicy, false},
		{"a provider error", supervise.CauseProviderError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := childCheckRig(t)
			rig.apply("tester-failed", intake.ClaimFailed{Issue: "LEGION-209", Role: claim.RoleTester, Cause: tc.cause,
				Reason:  "a provider error that cannot clear ended this claim's turn",
				Budgets: supervise.Budgets{LaunchFailures: 3}, Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 3, PromptRetires: 2}})
			notices := noticesOfKind(t, rig.pool, "LEGION-209", "worker-died")
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d worker-died notices of the ended look, want one", len(notices))
			}
			reason := notices[0].Reason
			if tc.look {
				if !strings.Contains(reason, "look 2 retries the claim at ") || strings.Contains(reason, "legion claims retry") {
					t.Fatalf("the worker-died says %q, want the look that retries the claim and no operator's retry", reason)
				}
				return
			}
			for _, want := range []string{"its cause is cyber_policy", "the check's next look does not retry the claim", "legion claims retry", "--fresh-session"} {
				if !strings.Contains(reason, want) {
					t.Errorf("the worker-died says %q, want it to name %q", reason, want)
				}
			}
			if strings.Contains(reason, "retries the claim at") {
				t.Errorf("the worker-died says %q, which promises a look that retries a claim no start of the daemon's own relaunches", reason)
			}
		})
	}
}
