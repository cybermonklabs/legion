package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// copied is whether the issue holds the other kind of saved work: a copy of its workspace taken
// while its pod was running, which holds no pod and which every path that drops or supersedes a
// release bundle drops with it.
func (r *idleRig) copied(t *testing.T) bool {
	t.Helper()
	_, held := runningCopyOf(t, r.st, r.issue.Key)
	return held
}

// copyWorkspace takes the copy the daemon takes while a pod runs, off the pod this rig's release
// takes its own bundle from, and answers it.
func (r *idleRig) copyWorkspace(t *testing.T) runtime.WorkspaceBundle {
	t.Helper()
	taken := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the cadence copied"), Head: "abc",
		Tips: []string{"abc"}, At: time.Now().UTC(), PodUID: releasedPodUID}
	if err := r.st.PutRunningWorkspaceCopy(context.Background(), r.issue.Key, taken); err != nil {
		t.Fatalf("take the running copy of %s: %v", r.issue.Key, err)
	}
	return taken
}

// A release of a workspace with nothing unpushed leaves the issue holding no saved work at all.
// It already deleted the bundle of whatever release came before — the next pod must never restore
// stale work — and the copy taken while this pod ran is the same stale work, taken minutes before
// the pod went quiet: every commit it holds has been pushed by now, so restoring it would move
// the issue's bookmarks back over what was pushed.
//
// That the release happens at all is half of what this reads: a running copy holds no pod, so an
// issue whose only saved work is one is released when it goes idle, where an unrestored release
// bundle would hold it (workflow.IdleHoldBundleUnrestored).
func TestAnIdleReleaseWithNothingUnpushedLeavesNoSavedWorkOfEitherKind(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	rig.copyWorkspace(t)
	// The shim finds the workspace with nothing unpushed, so the save stores nothing.
	rig.rt.bundle.Bundle = nil

	rig.w.releaseIdlePods(ctx)
	rig.drain(t)

	if _, suspended := rig.rt.reads(); suspended != 1 {
		t.Fatalf("Sandbox teardowns = %d, want the one this release made: a running copy holds no pod", suspended)
	}
	if rig.bundled(t) {
		t.Fatal("the release of a workspace with nothing unpushed left a bundle behind")
	}
	if rig.copied(t) {
		t.Fatal("the release of a workspace with nothing unpushed left the running copy behind")
	}
	// The pod the next wake makes is sent nothing of either kind.
	later := time.Now().UTC().Add(time.Hour)
	if restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-after-the-release", later); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the next pod was answered %+v (%v), want nothing of either kind", restore, err)
	}
}

// A release that does store a bundle drops the running copy too. The bundle is the newer copy of
// the same workspace — the shim takes it off the disk the running copy came off, while the roles
// are idle and before the release stops them — so from that save the copy is stale. Left behind
// it would be the newer-looking of the two for any pod after the one that restores the bundle,
// and that pod's workspace holds the descendants of the bundle's own work.
func TestAnIdleReleaseThatSavesABundleLeavesNoRunningCopy(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	taken := rig.copyWorkspace(t)

	rig.w.releaseIdlePods(ctx)
	rig.drain(t)

	if !rig.bundled(t) {
		t.Fatal("the release deleted the pod with no bundle saved")
	}
	if rig.copied(t) {
		t.Fatal("the release saved its bundle and left the running copy it supersedes behind")
	}
	// The pod that replaces the released one is sent the release's own bundle, and nothing of
	// the copy that was taken before it.
	restore, err := rig.st.TakeWorkspaceCopyForRestore(ctx, rig.issue.Key, "pod-that-replaces-it", time.Now().UTC().Add(time.Hour))
	saved := restore.ReleaseBundle
	if err != nil || saved.Take != runtime.WorkspaceBundleSend || string(saved.Bundle.Bundle) != string(rig.rt.bundle.Bundle) {
		t.Fatalf("the replacement pod was answered %q (%v, %v), want the %q the release saved", saved.Bundle.Bundle, saved.Take, err, rig.rt.bundle.Bundle)
	}
	if restore.Running.Take != runtime.WorkspaceBundleNone || string(saved.Bundle.Bundle) == string(taken.Bundle) {
		t.Fatalf("the replacement pod was answered the running copy %q (%v), want the release's own bundle alone", saved.Bundle.Bundle, restore.Running.Take)
	}
}

// An issue whose work is over — it is done, or its tree is closing — leaves no saved work of
// either kind: nothing will restore either copy and nothing should, since the next pod of that
// issue, if any, is a pod of work that was reopened over whatever was pushed.
func TestTheWorkOfAFinishedIssueLeavesNoReleaseBundleAndNoRunningCopy(t *testing.T) {
	rig := newIdleRig(t)
	ctx := context.Background()
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the release saved"), Head: "abc",
		At: time.Now().UTC(), PodUID: releasedPodUID}
	if err := rig.st.PutWorkspaceBundle(ctx, rig.issue.Key, 1, saved); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}
	// The pod that replaced the released one works on, and the cadence copies it: no copy is
	// stored off the pod the bundle came off once the bundle is saved (store.ErrReleaseBundleSaved).
	if err := rig.st.PutRunningWorkspaceCopy(ctx, rig.issue.Key, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work of the pod that replaced it"), Head: "abc", Tips: []string{"abc"},
		At: time.Now().UTC(), PodUID: "pod-that-replaced-it"}); err != nil {
		t.Fatalf("take the running copy of %s: %v", rig.issue.Key, err)
	}

	rig.runner.dropReleasedWork(ctx, rig.issue.Key)

	if rig.bundled(t) {
		t.Fatal("the finished issue kept the bundle of its release")
	}
	if rig.copied(t) {
		t.Fatal("the finished issue kept the copy taken while its pod ran")
	}
}

// A launcher holds the report of its restore until some connection carries it, so a report can
// reach a daemon that has restarted since the send, carrying nothing but the id the slices named.
// That id is what the keep is routed on, and it says which of the two tables the copy is in: the
// two draw from disjoint spaces (store.MinRunningWorkspaceCopyRestoreID). A report of one kind
// must leave the other kind's row alone — the issue can hold both, and each is the only copy of
// its own work. The routing is the store's own (store.KeepRestoredWorkspaceCopy), which is what
// the daemon hands the runtime (sandboxRuntime) and what the sandbox runtime's own restore tests
// drive their reports through.
func TestALateRestoreReportKeepsOnlyTheSavedWorkItsIdNames(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	st := outboxTreeStore(t, pool, "LEGION-208")
	const issue, release, restoring = "LEGION-208", int64(11), "pod-that-replaced-it"
	restored := st.KeepRestoredWorkspaceCopy

	// Whole seconds, which the store keeps exactly.
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the release saved"), Head: "abc",
		At: time.Now().UTC().Truncate(time.Second).Add(-time.Hour), PodUID: "pod-being-released"}
	if err := st.PutWorkspaceBundle(ctx, issue, release, saved); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}
	taken := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the cadence copied"), Head: "abc",
		At: saved.At.Add(time.Minute), PodUID: "pod-still-running"}
	if err := st.PutRunningWorkspaceCopy(ctx, issue, taken); err != nil {
		t.Fatalf("take the running copy: %v", err)
	}
	// The copy's own restore id, as the slices carry it to the launcher and the report brings it
	// back: above every id a release holds, which is what the routing reads.
	restore, err := st.TakeWorkspaceCopyForRestore(ctx, issue, restoring, time.Now().UTC().Add(time.Hour))
	sent := restore.Running.Bundle
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the pod that replaced the one the copy came off was answered %v (%v), want the copy", restore.Running.Take, err)
	}
	if sent.Release < store.MinRunningWorkspaceCopyRestoreID {
		t.Fatalf("the running copy's restore id is %d, want it at or above %d", sent.Release, store.MinRunningWorkspaceCopyRestoreID)
	}

	// The report of the running copy keeps that row as the restoring pod's own, and leaves the
	// release's bundle whole.
	if err := restored(ctx, issue, restoring, sent.Release); err != nil {
		t.Fatalf("keep the restored running copy: %v", err)
	}
	if again, err := st.TakeWorkspaceCopyForRestore(ctx, issue, restoring, time.Now().UTC().Add(time.Hour)); err != nil ||
		again.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the restoring pod asking again was answered %v of the running copy (%v), want nothing: the copy is its own", again.Running.Take, err)
	}
	if facts, held := runningCopyOf(t, st, issue); !held || facts.Size != taken.Size() {
		t.Fatalf("after the report of the running copy the store holds %+v, want its %d bytes kept", facts, taken.Size())
	}
	size, held, err := st.WorkspaceBundleSize(ctx, issue)
	if err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the report of the running copy the store holds %d bytes of the release bundle (held=%t, %v), want its %d untouched",
			size, held, err, len(saved.Bundle))
	}

	// A second cadence took another copy before the release's own report arrived, and that
	// report moves the bundle alone: the copy is the newer work and stays as it is.
	newer := taken
	newer.Bundle, newer.At = []byte("PACK the work the cadence copied next"), taken.At.Add(time.Minute)
	if err := st.PutRunningWorkspaceCopy(ctx, issue, newer); err != nil {
		t.Fatalf("take the running copy again: %v", err)
	}
	if err := restored(ctx, issue, restoring, release); err != nil {
		t.Fatalf("keep the restored release bundle: %v", err)
	}
	if _, held, err := st.WorkspaceBundleSize(ctx, issue); err != nil || held {
		t.Fatalf("after the report of the release the store holds the bundle = %t (%v), want it gone", held, err)
	}
	if facts, held := runningCopyOf(t, st, issue); !held || facts.Size != newer.Size() || !facts.At.Equal(newer.At) {
		t.Fatalf("after the report of the release the store holds the running copy %+v, want the newer copy of %d bytes taken %s kept",
			facts, newer.Size(), newer.At)
	}

	// A report of either kind that no row holds — a copy its issue has saved over, a release
	// whose bundle is already gone — changes nothing and fails nothing.
	if err := restored(ctx, issue, restoring, release); err != nil {
		t.Fatalf("a repeated report of the release: %v", err)
	}
	if err := restored(ctx, issue, restoring, sent.Release); err != nil {
		t.Fatalf("a report of a running copy the issue has replaced: %v", err)
	}
	if facts, held := runningCopyOf(t, st, issue); !held || facts.Size != newer.Size() || !facts.At.Equal(newer.At) {
		t.Fatalf("after the reports of rows the store holds the running copy %+v, want the newer copy as it was", facts)
	}
}
