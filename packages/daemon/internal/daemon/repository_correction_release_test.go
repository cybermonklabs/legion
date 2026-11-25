package daemon

// The window between a correction and the pod it replaces: Dispatch moved the
// issue to another repository and the daemon's record followed it
// (record.Store.CorrectIssueRepository), while the pod still running is a clone of the
// repository the issue has left. The update round that replaces that pod opens on a later sweep
// (supervision.reconcilePodUpdates), so there is a window in which the record and the pod name
// two repositories, and these tests force a release into it. The idle rule holds such a pod
// rather than releasing it (workflow.IdleHoldRepository): the pod is the update round's to
// replace.

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// correctedRepo is the repository Dispatch moved the rig's issue to, and correctedBranch its
// default branch, which the pod the update round builds to replace the held one is built from.
// The rig's issue starts on acme/widgets (newIdleRig), which is what its pod was built from.
const (
	correctedRepo   = "zeta/tools"
	correctedBranch = "main"
)

// podBuiltFrom scripts the repository the issue's pod was built from, as the Kubernetes runtime
// answers it from the pod's own workspace fetch (runtime.Runtime.PodRepository): one repository
// for the whole issue, since every role of it runs in that one pod and works the one workspace
// the fetch cloned.
func (r *idleRig) podBuiltFrom(repo string) {
	r.rt.ScriptPodRepository(r.token, repo)
}

// corrects moves the issue's record onto repo, as the workflow's own pass does when Dispatch
// names another repository for the issue (workflow.(*Engine).ApplyRepositoryChange). Nothing of
// the pod changes: the clone, the branch and the gh owner directories on its disk are still the
// repository the issue has left, until an update round replaces it.
func (r *idleRig) corrects(t *testing.T, repo, branch string) {
	t.Helper()
	correctRecord(t, r.pool, r.issues, r.issue.Key, repo, branch, time.Now().UTC())
}

// The hold: a pod built from a repository the record no longer holds is not released, however
// idle it is. The release is forced into the window the correction opens — the record has moved
// and no update round is open on the claim yet.
//
// The pod is the update round's to replace (supervision.reconcilePodUpdates, which opens a round
// naming both repositories), and the round asks every worker to push its work before the pod goes.
// The hold is what leaves the pod standing for it.
func TestRepositoryCorrectionHoldsTheIdlePodUntilTheRoundReplacesIt(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.podBuiltFrom(rig.issue.Repo)
	if got := rig.machine.Claim(); got.InPodRound() {
		t.Fatalf("the claim is in the pod round %q, want the window before any round opened", got.UpdateRound)
	}
	rig.corrects(t, correctedRepo, correctedBranch)

	// What the pass queued, not every row of the issue: the seeded idle implementer's own write
	// queued its role-holder publication before any pass (queuedBy says why).
	queued := rig.queuedBy(t, func() { rig.w.releaseIdlePod(ctx, rig.issue.Key) })

	if rig.marked(t) {
		t.Fatal("the pass marked a release of a pod of the repository the issue has left: the update round replaces it")
	}
	if len(queued) != 0 {
		t.Fatalf("queued rows = %v, want none: the pod is held, not released", queued)
	}
	records := rig.records(t)
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldRepository.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldRepository)
	}
	// Nothing of the pod was touched: no workspace was read through it, no Sandbox was suspended,
	// and the role that was idle in it is still idle in it.
	rig.drain(t)
	if read, suspended := rig.rt.reads(); len(read) != 0 || suspended != 0 {
		t.Fatalf("workspace reads = %v and Sandbox teardowns = %d, want none of a pod the rule held", read, suspended)
	}
	if got := rig.machine.Claim(); got.State != supervise.StateIdle {
		t.Fatalf("the implementer = %s by %s, want it left idle in the pod the rule held", got.State, got.SuspendedBy)
	}
}

// A pod on the repository its record holds is released as it always was: the hold names a pod
// that has drifted and nothing else, and a deployment where no issue has moved releases every
// idle pod of it.
func TestRepositoryCorrectionHoldsNoPodThatAgreesWithItsRecord(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.podBuiltFrom(rig.issue.Repo)

	rig.w.releaseIdlePod(ctx, rig.issue.Key)

	if !rig.marked(t) {
		t.Fatal("the pass marked no release of an idle pod built from the repository its record holds")
	}
}
