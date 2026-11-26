package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The pass lists GitHub before it applies what it found, and the record can change in between, so
// the workflow tells the architect a pull request is missing only while the issue still waits for
// one at the completion the pass named: in implementing, its implementer's handoff at that commit,
// and no open or merged pull request recorded. A pull request recorded closed is not an open one;
// one recorded merged told the architect (pr-merged), and a completion moves the issue on it.
func TestAMissingPullRequestIsToldOnlyWhileTheCompletionWaitsForOne(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		phase    phase.Phase
		handoff  string
		recorded record.PullRequestState
		told     bool
	}{
		{"the completion waits with no pull request", phase.Implementing, "handoff", "", true},
		{"the completion waits with its pull request closed", phase.Implementing, "handoff", record.PullRequestClosed, true},
		{"an open pull request is recorded since", phase.Implementing, "handoff", record.PullRequestOpen, false},
		{"its pull request merged since", phase.Implementing, "handoff", record.PullRequestMerged, false},
		{"the implementer completed again at another commit", phase.Implementing, "later", "", false},
		{"the issue moved on", phase.Testing, "handoff", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
				Title: "root", Phase: tc.phase, Generation: 1, Status: "in_progress", Rank: "U"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim", HandoffCommit: tc.handoff})
			if tc.recorded != "" {
				seedPR(t, pool, record.PullRequest{State: tc.recorded, Issue: "LEGION-208", Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "handoff", Failing: []string{}})
			}
			fact := intake.PullRequestMissing{Issue: "LEGION-208", Repo: "sjawhar/legion", Branch: "legion/LEGION-208", HandoffCommit: "handoff"}
			if _, err := intake.ApplyFact(context.Background(), pool, "github", "missing", fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("apply the missing pull request: %v", err)
			}
			if told := slices.Contains(noticeKinds(t, pool, "LEGION-208"), "pull-request-missing"); told != tc.told {
				t.Fatalf("pull-request-missing told: %v, want %v (notices %v)", told, tc.told, noticeKinds(t, pool, "LEGION-208"))
			}
		})
	}
}

// The notice names the topic of the implementer to tell: the claim that works the implementer's
// step, here key a's agent, `…-checker`, which the implementer's row names on its step, and not
// a topic spelled from the role, which reaches the implementer's own claim.
func TestAMissingPullRequestNamesTheTopicOfTheImplementersStepWorker(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION",
		Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})
	const keyed = "legion-legion-legion-208-checker"
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: keyed, StepTask: "a", StepPlace: 1, Entry: 1, HandoffCommit: "handoff"})
	fact := intake.PullRequestMissing{Issue: "LEGION-208", Repo: "sjawhar/legion", Branch: "legion/LEGION-208", HandoffCommit: "handoff"}
	if _, err := intake.ApplyFact(context.Background(), pool, "github", "missing", fact, testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("apply the missing pull request: %v", err)
	}
	var reason string
	if err := pool.QueryRow(context.Background(), `select payload->>'reason' from outbox where kind = 'notice' and payload->>'kind' = 'pull-request-missing'`).Scan(&reason); err != nil {
		t.Fatalf("read the notice: %v", err)
	}
	if want := "envoy_publish to `notifications.role." + keyed + "`"; !strings.Contains(reason, want) {
		t.Fatalf("the notice says %q, want it naming %q", reason, want)
	}
}
