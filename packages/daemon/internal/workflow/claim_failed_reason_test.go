package workflow

// What a worker-died notice says of a failed claim on the two paths that said nothing at all: the
// tree architect's, and the held phase's. Each now carries the sentence the supervisor wrote when
// it gave up on the claim, and the pod end the claim's process was last observed to have died of,
// which is the cause no role chose — an eviction for the node's disk above all (INFRA-2009) — or,
// where there is none, the runtime's account of the process's last death.

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

// evictionMessage is the kubelet's message on a pod it evicted for its node's disk.
const evictionMessage = "The node was low on resource: ephemeral-storage. Container roles was using 48Gi, which exceeds its request of 0."

// evictedPodEnd is a claim's stored pod end as the runtime read it off an evicted pod
// (supervise.Claim.PodEnd, runtime.Observation.PodEnd).
const evictedPodEnd = "Evicted: " + evictionMessage

// budgetRanOut is the sentence the supervisor writes when a claim's relaunches run out
// (supervise's fail, Claim.Failure).
const budgetRanOut = "deaths with work outstanding ran out"

// roleLogTail is the tail of the log the runtime quotes at the end of a death's detail
// (sandbox's roleLog): the role's own last lines with the launcher's prefix dropped, then the
// container's own under the line that introduces them.
const roleLogTail = "   Compiling widgets v0.4.1 (/legion/workspaces/acme/widgets/legion-208)\n" +
	"error: failed to write /legion/scratch/target/debug/deps/libwidgets-3f2a9c.rlib: No space left on device (os error 28)\n" +
	"error: could not compile `widgets` (lib) due to 1 previous error\n" +
	"the container's own last lines, no role's:\n" +
	"volume trim: the volume is 97.2% full (used 50331648000 B, free 1073741824 B): at or above the high mark 85%, sweeping to 70%"

// evictedDeath is a claim's stored account of its process's last death when the cluster evicted
// its pod, in the shape the runtime writes it (supervise.Claim.LastDeathCause): supervise's
// deathCause puts the verdict before the detail sandbox's Runtime.ended writes, which is the pod
// and its phase, the pod's reason and the kubelet's message, the container's end, and role's
// last lines of the log. Neither function is reachable from this package, so the shape is
// copied from the two.
func evictedDeath(role claim.Role) string {
	return "gone: pod legion-legion-208 (uid 8a1f) Failed: " + evictedPodEnd +
		": container roles terminated (Error, exit code 137); last lines of " + string(role) + ":\n" + roleLogTail
}

// oomKilledDeath is a claim's stored account of a death whose pod gives no reason of its own,
// in the shape the runtime writes it: the pod still runs and its container ended, which the
// Sandbox runtime observes with no pod end (sandbox's observation of a running pod's terminated
// container), so the claim holds none beside it.
func oomKilledDeath(role claim.Role) string {
	return "gone: pod legion-legion-208 (uid 8a1f) container roles terminated (OOMKilled, exit code 137); last lines of " +
		string(role) + ":\n" + roleLogTail
}

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

// A held phase whose fact names a cause and a pod end still names the sentence once: the failure
// record's detail carries it, so the reason carries only the pod end, which the record has no
// field for. The runtime's account of the death is the record's own `death`, so the reason does
// not repeat that either. The negative control is deathReason putting the sentence before the
// pod end whatever the notice carried beside it, which named the sentence twice.
func TestAHeldPhasesWorkerDiedNamesTheSentenceOnceWithAPodEnd(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U"})

	death := evictedDeath(claim.RoleImplementer)
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "implementer-failed",
		intake.ClaimFailed{Issue: "LEGION-208", Role: claim.RoleImplementer, Reason: budgetRanOut, PodEnd: evictedPodEnd,
			Death: death, Cause: supervise.CauseDied},
		testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
		t.Fatalf("ApplyFact the implementer's failed claim: %v", err)
	}
	notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
	if len(notices) != 1 {
		t.Fatalf("the outbox holds %d worker-died notices, want one beside the hold", len(notices))
	}
	failure := notices[0].Failure
	if failure == nil {
		t.Fatalf("the held phase's worker-died carries no failure record, want one: its fact names a cause")
	}
	if !strings.Contains(notices[0].Reason, evictedPodEnd) {
		t.Errorf("the held phase's worker-died reads %q, want the pod end the failure record has no field for", notices[0].Reason)
	}
	if said := strings.Count(notices[0].Reason+"\n"+failure.Detail, budgetRanOut); said != 1 {
		t.Errorf("the held phase's worker-died names the sentence %d times across its reason %q and its failure detail %q, want once",
			said, notices[0].Reason, failure.Detail)
	}
	if said := strings.Count(notices[0].Reason+"\n"+failure.Death, death); said != 1 {
		t.Errorf("the held phase's worker-died names the death %d times across its reason %q and its failure death %q, want once",
			said, notices[0].Reason, failure.Death)
	}
}

// A death whose pod gives no reason of its own leaves the claim no pod end, and the runtime's own
// account of it is what the daemon last observed. The two notices with no failure record — the
// tree architect's, and a held phase's whose fact names no cause — name it in their reason beside
// the sentence, since nothing else in either notice carries it, and name its verdict once: the
// account begins with it, and the reason reads on from "the process was last found". The
// negative controls are deathReason reading only the sentence and the pod end, which left both
// reasons the sentence alone, and deathReason writing "the process was last found gone: " before
// the account, which read `gone: gone:`.
func TestAWorkerDiedWithNoFailureRecordNamesTheLastObservedDeath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  claim.Role
		phase phase.Phase
	}{
		{"the tree architect's", claim.RoleArchitect, phase.Planning},
		{"a held phase's whose fact names no cause", claim.RoleImplementer, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.phase, Generation: 1, Status: "in_progress", Rank: "U"})

			death := oomKilledDeath(tc.role)
			if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "claim-failed",
				intake.ClaimFailed{Issue: "LEGION-208", Role: tc.role, Reason: budgetRanOut, Death: death},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the failed claim: %v", err)
			}
			notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d worker-died notices, want one", len(notices))
			}
			if notices[0].Failure != nil {
				t.Fatalf("the worker-died carries the failure record %+v, want none: its reason is where the death goes", notices[0].Failure)
			}
			if strings.Contains(notices[0].Reason, "gone: gone:") {
				t.Errorf("the worker-died reads %q, which names the verdict twice", notices[0].Reason)
			}
			if want := budgetRanOut + "; the process was last found " + death; notices[0].Reason != want {
				t.Errorf("the worker-died reads %q, want %q", notices[0].Reason, want)
			}
		})
	}
}

// A pod the cluster evicted leaves the claim both a pod end and the runtime's account of the
// death, and that account is the runtime's whole detail: the pod's reason and the kubelet's
// message, which are the pod end, then the container's end and a tail of the role's log over
// many lines. The two notices with no failure record name the sentence and the pod end alone,
// so the kubelet's message is read once, on one line, with nothing of the log. The negative
// control is deathReason naming the death whatever the pod end held, which put the message in
// the reason twice and the pod end after the log.
func TestAWorkerDiedWithNoFailureRecordNamesAnEvictionOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  claim.Role
		phase phase.Phase
	}{
		{"the tree architect's", claim.RoleArchitect, phase.Planning},
		{"a held phase's whose fact names no cause", claim.RoleImplementer, phase.Implementing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: "LEGION-208", Project: "LEGION", Title: "root", Phase: tc.phase, Generation: 1, Status: "in_progress", Rank: "U"})

			if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "claim-failed",
				intake.ClaimFailed{Issue: "LEGION-208", Role: tc.role, Reason: budgetRanOut, PodEnd: evictedPodEnd, Death: evictedDeath(tc.role)},
				testEngine(config.DesignGateRootIssues, nil), admissionStub{}); err != nil {
				t.Fatalf("ApplyFact the failed claim: %v", err)
			}
			notices := noticesOfKind(t, pool, "LEGION-208", "worker-died")
			if len(notices) != 1 {
				t.Fatalf("the outbox holds %d worker-died notices, want one", len(notices))
			}
			if notices[0].Failure != nil {
				t.Fatalf("the worker-died carries the failure record %+v, want none: its reason is where the pod end goes", notices[0].Failure)
			}
			reason := notices[0].Reason
			if said := strings.Count(reason, evictionMessage); said != 1 {
				t.Errorf("the worker-died names the kubelet's message %d times, want once: %q", said, reason)
			}
			if strings.Contains(reason, "\n") {
				t.Errorf("the worker-died reads over %d lines, want one: %q", strings.Count(reason, "\n")+1, reason)
			}
			if want := budgetRanOut + "; the pod ended: " + evictedPodEnd; reason != want {
				t.Errorf("the worker-died reads %q, want %q", reason, want)
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
