package daemon

// The end of a pod's hold streaks when the project release deletes the pod.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A pod the release deletes leaves no standing hold behind: the idle pass, which ends a pod's
// hold streaks when it finds the pod gone, is held while the release's mark stands, so without the
// release's own end of them the state would name a hold of the deleted pod until the next boot
// (store.StandingPodHoldsTx, the state's `podHold`).
func TestAProjectReleaseEndsTheHoldOfThePodItDeletes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := outboxTreeStore(t, isolatedOutboxPool(t))
	token := mustClaimToken(t, "LEGION-208", claim.RoleImplementer)
	if err := st.PutClaim(ctx, supervise.Claim{Token: token, Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208",
		Role: claim.RoleImplementer, Generation: 1, Session: "ses_implementer", State: supervise.StateSuspended,
		SuspendedBy: supervise.SuspendedByReleased}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordPodHold(ctx, store.PodHold{Project: "legion", Issue: "LEGION-208", Reason: "turn", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	pods := &releasePods{sandboxes: []runtime.ProjectSandbox{{Name: "legion-legion-legion-208", Kind: runtime.ProjectSandboxIssue,
		Issue: "LEGION-208", Tree: "LEGION-208", PodUID: "uid-208", ConfiguredImage: true}}}
	r := testReleaser(ctx, &projectRelease{}, releasingStore{st}, sup, pods)
	if _, started, err := r.Start(ctx, time.Second, false); err != nil || !started {
		t.Fatalf("Start = started %v, %v; want the release recorded", started, err)
	}
	testwait.Eventually(t, "the release finished", func() bool {
		release, found, err := st.ReadProjectRelease(ctx, "legion")
		return err == nil && found && !release.FinishedAt.IsZero()
	})
	pods.mu.Lock()
	deleted := slices.Clone(pods.deleted)
	pods.mu.Unlock()
	if !slices.Equal(deleted, []string{"LEGION-208"}) {
		t.Fatalf("the release deleted the pods of %v, want LEGION-208's", deleted)
	}
	if holding, err := st.OpenPodHold(ctx, "legion", "LEGION-208"); err != nil || holding {
		t.Fatalf("a hold streak of the deleted pod of LEGION-208 runs = %t, %v; want none", holding, err)
	}
}
