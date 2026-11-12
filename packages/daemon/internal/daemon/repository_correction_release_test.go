package daemon

// INFRA-3136 R3, the window between a correction and the pod it replaces: Dispatch moved the
// issue to another repository and the daemon's record followed it
// (record.Store.CorrectIssueRepository), while the pod still running is a clone of the
// repository the issue has left. The update round that replaces that pod opens on a later sweep
// (supervision.reconcilePodUpdates), so there is a window in which the record and the pod name
// two repositories, and these tests force a release into it — the one act that takes work off
// that pod's disk and stores it for the pod that comes next.
//
// Two things keep the work of the old repository out of the new clone, and each has a test here:
// the idle rule holds such a pod rather than releasing it (workflow.IdleHoldRepository), and the
// label of a bundle a release does save is the repository the released pod was built from, never
// the one the record holds now, so the store refuses it to every pod of the new repository
// (store.TakeWorkspaceBundleForRestore).

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// correctedRepo is the repository Dispatch moved the rig's issue to, and correctedBranch its
// default branch, which the pod that replaces the released one is built from. The rig's issue
// starts on acme/widgets (newIdleRig), which is what its pod was built from and what every
// commit on that pod's disk is in the history of.
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
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		corrected, err := r.issues.CorrectIssueRepository(ctx, tx, r.issue.Key, repo, branch, repo, time.Now().UTC())
		if err != nil {
			return err
		}
		if !corrected {
			t.Errorf("the record of %s was already on %s", r.issue.Key, repo)
		}
		return nil
	}); err != nil {
		t.Fatalf("correct the repository of %s onto %s: %v", r.issue.Key, repo, err)
	}
}

// bundleRepository is the repository the saved bundle of the issue is labelled with, which is
// what the store refuses a restore by (store.TakeWorkspaceBundleForRestore).
func (r *idleRig) bundleRepository(t *testing.T) string {
	t.Helper()
	var repo string
	if err := r.pool.QueryRow(context.Background(), `select repo from workspace_bundles where issue = $1`,
		r.issue.Key).Scan(&repo); err != nil {
		t.Fatalf("read the repository the saved bundle is labelled with: %v", err)
	}
	return repo
}

// The hold: a pod built from a repository the record no longer holds is not released, however
// idle it is. The release is forced into the window the correction opens — the record has moved
// and no update round is open on the claim yet — which is the release this issue's own class
// exists to stop: it would take the pod, store its unpushed work under the issue, and leave that
// work where no pod of the issue can ever take it back.
//
// The pod is the update round's to replace (supervision.reconcilePodUpdates, which opens a round
// naming both repositories), and the round carries every worker's unpushed work off the disk
// before the pod goes. The hold is what leaves the pod standing for it.
func TestRepositoryCorrectionHoldsTheIdlePodUntilTheRoundReplacesIt(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.podBuiltFrom(rig.issue.Repo)
	if got := rig.machine.Claim(); got.InPodRound() {
		t.Fatalf("the claim is in the pod round %q, want the window before any round opened", got.UpdateRound)
	}
	rig.corrects(t, correctedRepo, correctedBranch)

	rig.w.releaseIdlePod(ctx, rig.issue.Key)

	if rig.marked(t) {
		t.Fatal("the pass marked a release of a pod of the repository the issue has left: its work is work no pod of this issue can restore")
	}
	if got := rig.rows(t); len(got) != 0 {
		t.Fatalf("queued rows = %v, want none: the pod is held, not released", got)
	}
	records := rig.records(t)
	if len(records.Holds) != 1 || records.Holds[0].Reason != workflow.IdleHoldRepository.String() {
		t.Fatalf("holds = %+v, want the one %s", records.Holds, workflow.IdleHoldRepository)
	}
	// Nothing of the pod was touched: no bundle was taken off its disk, and the role that was
	// idle in it is still idle in it.
	rig.drain(t)
	if rig.bundled(t) {
		t.Fatal("a bundle was saved out of a pod the rule held")
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

// The label: a release that did happen saves the work under the repository the released pod was
// built from, never the one the record holds. The correction here commits after the rule read
// the pod and before the save's own row runs, which is the one window the hold above cannot
// close — the release was already marked — and the label is what makes it harmless.
//
// The take then refuses that bundle to the pod of the new repository rather than answering it
// `sent`: the work is commits of acme/widgets, and a clone of zeta/tools has none of them. A
// bundle labelled with the record's repository would have been sent into that clone, which is
// the failure this test holds shut.
func TestRepositoryCorrectionLabelsTheSavedBundleWithThePodsOwnRepository(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.podBuiltFrom(rig.issue.Repo)

	// The pod and the record agree, so the rule releases the pod.
	rig.w.releaseIdlePod(ctx, rig.issue.Key)
	if !rig.marked(t) {
		t.Fatal("the pass marked no release of an idle pod")
	}
	// Dispatch moves the issue between the mark and the save: the save's row is the first of the
	// release's rows and the claim still records the process the bundle is taken from.
	rig.corrects(t, correctedRepo, correctedBranch)

	rig.drain(t)

	if !rig.bundled(t) {
		t.Fatal("the release saved no bundle of the workspace's unpushed work")
	}
	if got := rig.bundleRepository(t); got != rig.issue.Repo {
		t.Fatalf("the saved bundle is labelled %q, want %q: the work is of the repository the released pod was a clone of", got, rig.issue.Repo)
	}
	// The pod of the new repository, created after the save: the store refuses it the work.
	bundle, outcome, err := rig.st.TakeWorkspaceBundleForRestore(ctx, rig.issue.Key, "pod-of-the-new-repository", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("take the saved bundle for the pod of the new repository: %v", err)
	}
	if outcome != runtime.WorkspaceBundleRefusedRepository {
		t.Fatalf("the pod of %s was answered %v, want the bundle refused for its repository", correctedRepo, outcome)
	}
	if len(bundle.Bundle) != 0 {
		t.Fatalf("the refused pod was answered %d bytes of bundle, want none", len(bundle.Bundle))
	}
	if bundle.Repo != rig.issue.Repo {
		t.Fatalf("the refusal names %q as the repository the work is of, want %q", bundle.Repo, rig.issue.Repo)
	}
}
