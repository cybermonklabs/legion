package workflow

// What a worker-died notice says of a failed claim on the two paths that said nothing at all: the
// tree architect's, and the held phase's. Each now carries the sentence the supervisor wrote when
// it gave up on the claim and the pod end the claim's process was last observed to have died of,
// which is the cause no role chose — an eviction for the node's disk above all (INFRA-2009).

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// evictedPodEnd is a claim's stored pod end as the runtime read it off an evicted pod
// (supervise.Claim.PodEnd, runtime.Observation.PodEnd).
const evictedPodEnd = "Evicted: The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."

// budgetRanOut is the sentence the supervisor writes when a claim's relaunches run out
// (supervise's fail, Claim.Failure).
const budgetRanOut = "deaths with work outstanding ran out"

// The tree architect's own claim failing is told to that architect and to the controller, and the
// notice now names both of the things the supervisor knows. The negative control is the code at
// the merge base, where this path writes a kind, a role and a phase and no reason at all.
func TestATreeArchitectsFailedClaimNamesWhatEndedIt(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "architect-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleArchitect, Reason: budgetRanOut, PodEnd: evictedPodEnd},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices for the architect, want one", len(notices))
	}
	for _, want := range []string{budgetRanOut, evictedPodEnd} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the tree architect's worker-died does not name %q: %q", want, notices[0].Reason)
		}
	}
}

// A pod that died with no pod-level reason leaves the claim a sentence and no pod end, and the
// tree architect's worker-died still names that sentence: the notice carries no failure record,
// so its reason is the only place the architect and the controller read why the claim failed.
// The negative control is deathReason answering "" for a fact with no pod end, which left this
// notice saying nothing at all.
func TestATreeArchitectsFailedClaimNamesTheSentenceWithNoPodEnd(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "architect-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleArchitect, Reason: budgetRanOut, Cause: supervise.CauseDied},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the architect's failed claim: %v", err)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices for the architect, want one", len(notices))
	}
	if notices[0].Reason != budgetRanOut {
		t.Errorf("the tree architect's worker-died reads %q, want the supervisor's sentence %q", notices[0].Reason, budgetRanOut)
	}
	if notices[0].Failure != nil {
		t.Errorf("the tree architect's worker-died carries the failure record %+v, want none: its reason is where the sentence goes", notices[0].Failure)
	}
}

// The held path is the other one that said nothing: the role that works the issue's phase died,
// the phase is held for its architect to decide, and the worker-died beside that hold now names
// what ended the worker. The negative control is the same code at the merge base.
func TestAHeldPhasesWorkerDiedNamesWhatEndedIt(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "implementer-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: budgetRanOut, PodEnd: evictedPodEnd},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the implementer's failed claim: %v", err)
	}
	var held phase.Phase
	if err := pool.QueryRow(context.Background(), "select phase from issues where key = 'LEGION-208'").Scan(&held); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if held != phase.Held {
		t.Fatalf("the issue stands at %s, want it held for its architect to decide", held)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices, want one beside the hold", len(notices))
	}
	for _, want := range []string{budgetRanOut, evictedPodEnd} {
		if !strings.Contains(notices[0].Reason, want) {
			t.Errorf("the held phase's worker-died does not name %q: %q", want, notices[0].Reason)
		}
	}
}

// The held path keeps from repeating the sentence its failure record already carries: with a
// cause and no pod end, the worker-died's failure record names the sentence in its detail and
// the reason is empty. A held phase whose fact names no cause — a failure recorded before the
// daemon kept one, replayed at a boot — carries no failure record at all, so its reason is the
// sentence, as the tree architect's is.
func TestAHeldPhasesWorkerDiedNamesTheSentenceOnceWithNoPodEnd(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cause      supervise.FailureCause
		wantReason string
	}{
		{"a fact with a cause, whose failure record carries the sentence", supervise.CauseDied, ""},
		{"a fact with no cause, which carries no failure record", "", budgetRanOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})

			if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "implementer-failed",
				intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: budgetRanOut, Cause: tc.cause},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the implementer's failed claim: %v", err)
			}
			notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d worker-died notices, want one beside the hold", len(notices))
			}
			if notices[0].Reason != tc.wantReason {
				t.Errorf("the held phase's worker-died reads %q, want %q", notices[0].Reason, tc.wantReason)
			}
			detail := ""
			if notices[0].Failure != nil {
				detail = notices[0].Failure.Detail
			}
			if said := strings.Count(notices[0].Reason+"\n"+detail, budgetRanOut); said != 1 {
				t.Errorf("the held phase's worker-died names the sentence %d times across its reason %q and its failure detail %q, want once",
					said, notices[0].Reason, detail)
			}
		})
	}
}

// The path that already builds a reason keeps the one it builds: a role that does not work the
// issue's phase is told exactly that, and nothing of the supervisor's sentence or the pod's end
// displaces it. What that reason says is what the architect must act on, and it is more than
// either of those facts is.
func TestARoleThatWorksNoPhaseKeepsTheReasonItsPathBuilds(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})

	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "tester-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleTester, Reason: budgetRanOut, PodEnd: evictedPodEnd},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the tester's failed claim: %v", err)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices, want one", len(notices))
	}
	if !strings.Contains(notices[0].Reason, "does not work LEGION-208's phase implementing") {
		t.Fatalf("the notice of a role that works no phase reads %q, want the reason that path builds", notices[0].Reason)
	}
}
