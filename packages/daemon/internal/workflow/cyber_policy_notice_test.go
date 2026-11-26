package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// cyberPolicyFailure is the sentence the supervisor writes for a claim that failed on OpenAI's
// cyber-policy refusal of a turn that ran no prompt of the daemon's.
const cyberPolicyFailure = "a provider error that cannot clear ended a turn this claim's agent ran with no prompt of the daemon's: OpenAI's cyber_policy refusal of this session's content"

// wantNoDaemonMechanics fails the case where reason, a notice an agent reads, names how the daemon
// treats a cyber-policy refusal or who brings the claim's agent back: an agent acts on a failure
// the same way whatever failed it (noStartOfItsOwn).
func wantNoDaemonMechanics(t *testing.T, reason string) {
	t.Helper()
	for _, mechanic := range []string{"legion claims retry", "--fresh-session", "new session", "operator"} {
		if strings.Contains(reason, mechanic) {
			t.Errorf("the notice says %q, want it to name no daemon mechanics (%q)", reason, mechanic)
		}
	}
}

// A claim failed on OpenAI's cyber-policy refusal is started again by no start of the daemon's own
// (supervise.ErrHeldForCyberPolicy), so the worker-died notices that promise such a start say in
// its place that the role does not start again by itself: a child's own architect is not started
// again at its child's next phase start, and a role that fails outside its issue's phase is not
// started again by that phase's next start (the checking role's notice is
// TestAProductionCheckRoleFailedForCyberPolicyIsPromisedNoLook's). Neither names the cause's
// mechanics or a command. The held phase's notice promises nothing, so it says nothing more than
// for any failure: the architect retries or escalates, and its retry starts the agent in a new
// session by itself. A claim failed for another cause keeps the promise, which is the control.
func TestAWorkerDiedForCyberPolicyPromisesNoRestartOfTheDaemonsOwn(t *testing.T) {
	root := "LEGION-208"
	liveRoot := record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Project: "LEGION", Title: "root",
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"}
	child := record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: root, Parent: &root, Project: "LEGION", Title: "child",
		Phase: phase.Testing, Generation: 1, Status: "in_progress", Rank: "V"}
	apply := func(t *testing.T, pool *pgxpool.Pool, role claim.Role, cause supervise.FailureCause) string {
		t.Helper()
		failed := intake.ClaimFailed{Issue: child.Key, Role: role, Cause: cause, Reason: cyberPolicyFailure,
			Budgets: supervise.Budgets{}, Limits: supervise.Limits{LaunchFailures: 3, PromptFailures: 5, PromptRetires: 2}}
		if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "worker-died-"+string(role), failed, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
			t.Fatalf("ApplyFact the %s's failed claim: %v", role, err)
		}
		notices := noticesOfKind(t, pool, child.Key, "worker-died")
		if len(notices) != 1 {
			t.Fatalf("worker-died notices = %+v, want one", notices)
		}
		return notices[0].Reason
	}
	const noStart = "it does not start again by itself"
	for _, tc := range []struct {
		name  string
		role  claim.Role
		cause supervise.FailureCause
		want  string
	}{
		{"a child's own architect", claim.RoleArchitect, supervise.CauseCyberPolicy, noStart},
		{"a role outside its issue's phase", claim.RoleReviewer, supervise.CauseCyberPolicy, noStart},
		{"the held phase's worker", claim.RoleTester, supervise.CauseCyberPolicy, ""},
		{"a child's own architect that died", claim.RoleArchitect, supervise.CauseDied, "the daemon starts it again at LEGION-209's next phase start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, liveRoot)
			seedIssue(t, pool, child)
			reason := apply(t, pool, tc.role, tc.cause)
			wantNoDaemonMechanics(t, reason)
			if tc.want == "" {
				if reason != "" {
					t.Errorf("the held phase's worker-died says %q, want nothing beyond what any failure's says", reason)
				}
				if got := storedIssue(t, pool, child.Key); got.Phase != phase.Held {
					t.Errorf("the child stands in %s, want it held for its architect", got.Phase)
				}
				return
			}
			if !strings.Contains(reason, tc.want) {
				t.Fatalf("the worker-died says %q, want %q", reason, tc.want)
			}
			if tc.cause == supervise.CauseCyberPolicy && strings.Contains(reason, "starts it again") {
				t.Errorf("the worker-died says %q, which promises a start of the daemon's own that never comes", reason)
			}
			if tc.cause != supervise.CauseCyberPolicy && strings.Contains(reason, noStart) {
				t.Errorf("the worker-died of a death says %q, want the daemon's own start promised", reason)
			}
		})
	}
}

// The checking role of a production check is started again by the look after a failed one, a
// start of the daemon's own, so its worker-died names that look (checkRoleFailedReason). A
// checking role failed on OpenAI's cyber-policy refusal is retried by no look, and its worker-died
// says that it does not start again by itself, and nothing of the daemon's mechanics. The control
// is the same failure for a provider error that cannot clear, whose worker-died still names the
// look that retries the claim.
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
			wantNoDaemonMechanics(t, reason)
			if tc.look {
				if !strings.Contains(reason, "look 2 retries the claim at ") || strings.Contains(reason, "does not start again") {
					t.Fatalf("the worker-died says %q, want the look that retries the claim", reason)
				}
				return
			}
			if !strings.Contains(reason, "it does not start again by itself") {
				t.Errorf("the worker-died says %q, want it to say the role does not start again by itself", reason)
			}
			if strings.Contains(reason, "retries the claim at") {
				t.Errorf("the worker-died says %q, which promises a look that retries a claim no start of the daemon's own starts", reason)
			}
		})
	}
}

// A person's todo on a held child that a stop took out of Legion's work returns it to work and
// releases its hold (ReturnToWork, releaseHeldRoot): the held phase's worker is started at a new
// phase entry, as every return's worker is, and that start is the daemon's own, so the start of
// the planner held for OpenAI's cyber-policy refusal carries no decision
// (record.SuperviseRequest.Decided) and the outbox refuses it. The refusal tells the workflow of
// the failure again, built from the claim as it stands (intake.ClaimFailedOf, the daemon outbox's
// heldStartRefused): the child is held at that phase and its architect is told, as for any
// failure, and the architect's retry of the held phase is then a decided start, which starts the
// agent in a new session (supervise's freshSession). Without the refusal's fact the child would
// stand in the phase with no worker and nobody told.
func TestAHeldChildsTodoHoldsItAgainAtTheHeldWorkersPhaseAndTellsItsArchitect(t *testing.T) {
	pool := migratedPool(t)
	approved := 1
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION",
		Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 3})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-208", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 1, ApprovedVersion: &approved})
	parent := "LEGION-208"
	const child = "LEGION-209"
	seedIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION",
		Title: "child", Parent: &parent, Phase: phase.Held, Hold: &record.Hold{From: phase.Planning}, Generation: 1, Status: "backlog", DispatchStatus: "backlog",
		Rank: "V", LastDispatchSeq: 4})
	seedPhase(t, pool, record.PhaseRow{Issue: child, Role: claim.RolePlanner, Claim: "child-planner", Rounds: 1})
	engine := testEngine(config.DesignGateRootIssues, nil)

	applyRefusingNothing(t, pool, engine, intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 5, Type: "issue.updated", Status: "todo",
		Title: "child", Parent: parent, Rank: "V"})
	if got := storedIssue(t, pool, child); got.Phase != phase.Planning || got.Hold != nil || got.Generation != 1 {
		t.Fatalf("the child after its todo stands in %s (%+v) at generation %d, want its hold released into planning at generation 1: the held planner's phase is the one this case is about",
			got.Phase, got.Hold, got.Generation)
	}
	start := lastStartOf(t, pool, child, claim.RolePlanner)
	if start.Decided || start.Phase != phase.Planning {
		t.Fatalf("the return's planner start = %+v, want a start of planning that is the daemon's own: nobody decided to start the held planner again", start)
	}

	// The outbox's refusal of that start, for a planner held for OpenAI's cyber-policy refusal.
	held := supervise.Claim{Issue: child, Role: claim.RolePlanner, State: supervise.StateFailed, FailureCause: supervise.CauseCyberPolicy,
		Failure: cyberPolicyFailure, Budgets: supervise.Budgets{}}
	if _, err := intake.ApplyFact(context.Background(), pool, "outbox", "start-held:planner:1",
		intake.ClaimFailedOf(held, supervise.Limits{LaunchFailures: 3, PromptFailures: 5, PromptRetires: 2}), engine, admissionStub{}); err != nil {
		t.Fatalf("apply the refused start's failure: %v", err)
	}
	if got := storedIssue(t, pool, child); got.Phase != phase.Held || got.Hold == nil || got.Hold.From != phase.Planning {
		t.Fatalf("the child after the refused start = %s (%+v), want it held from planning for its architect", got.Phase, got.Hold)
	}
	heldNotices := noticesOfKind(t, pool, child, "held")
	died := noticesOfKind(t, pool, child, "worker-died")
	if len(heldNotices) != 1 || len(died) != 1 || died[0].Role != claim.RolePlanner {
		t.Fatalf("the child's architect was told held %+v and worker-died %+v, want one of each for the planner", heldNotices, died)
	}
	wantNoDaemonMechanics(t, died[0].Reason)

	// The architect answers as for any held phase: retry_or_escalate's retry, a decided start.
	applyRefusingNothing(t, pool, engine, intake.RetryOrEscalate{Issue: child, Decision: intake.RetryDecision})
	if retry := lastStartOf(t, pool, child, claim.RolePlanner); !retry.Decided {
		t.Fatalf("the architect's retry of the held planning = %+v, want it decided, the start that starts the held planner's agent again", retry)
	}
}
