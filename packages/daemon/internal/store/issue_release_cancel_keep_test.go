// The workspace copy a cancelled idle release keeps: a cancel after the save stored its bundle
// keeps that bundle as the issue's running copy, a running copy newer than the bundle stays as it
// is, and a cancel after a save that failed keeps the running copy the save did not replace. The
// release's fences, its mark and the cancel's resumes are tested in issue_release_test.go, whose
// helpers these tests share.
package store

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A release cancelled after its save stored the bundle — a wake's or a delivery's cancel, the
// usual one — keeps that bundle as the issue's running copy, in the cancel's own transaction with
// the resumes and the mark. The save dropped the copy the cadence had taken (PutWorkspaceBundle),
// no copy off the pod is stored while the bundle stands, and the resumed roles come up Ready and
// take no copy until a woken turn ends, so the bundle is the one copy of the pod's work outside
// it: a cancel that dropped it would leave an eviction of the pod in that window nothing to
// restore. The kept copy still names the pod it came off, the pod the roles resume in, which is
// answered nothing of it, and the pod after that one is sent the work. The bundle's row, which
// would hold the issue's pod, is gone.
//
// One transaction is what makes that true of a cancel that fails too: a resume the store cannot
// queue leaves the mark standing and the bundle with it, and writes no running copy, so the row's
// retry runs the whole cancel again.
func TestACancelAfterTheSaveStoredItsBundleKeepsItAsTheResumedPodsRunningCopy(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	// The copy the cadence took while the pod worked, before it went quiet.
	taken := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the last turn"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", PodUID: "pod-being-released"}
	if err := st.PutRunningWorkspaceCopy(ctx, "legion", rootIssue, copyBegun(t, st), taken); err != nil {
		t.Fatalf("take the running copy: %v", err)
	}
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	var release int64
	if err := st.Pool().QueryRow(ctx, `select id from issue_releases where issue = $1`, rootIssue).Scan(&release); err != nil {
		t.Fatalf("read the id of the release of %s: %v", rootIssue, err)
	}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the cancelled release"),
		Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", Tips: []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"side": "da4f9f492e9f8349389c243743e0a000cd1c676f"}, PodUID: "pod-being-released"}
	if err := st.PutWorkspaceBundle(ctx, rootIssue, release, copyBegun(t, st), saved); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}
	savedAt := heldCopyRow(t, st, releaseBundleCopies, rootIssue).writtenAt

	// A wake cancels the release, and the resume it queues cannot be built.
	refused := errors.New("the resume could not be encoded")
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, func(claim.Role, uint64) (IdleReleaseRow, error) {
		return IdleReleaseRow{}, refused
	}, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); !errors.Is(err, refused) {
		t.Fatalf("a cancel whose resume could not be built = %v, want %v", err, refused)
	}
	if row := heldCopyRow(t, st, releaseBundleCopies, rootIssue); !row.held {
		t.Fatal("after a cancel that failed the store holds no bundle, want it kept with the mark")
	}
	if row := heldCopyRow(t, st, runningCopyCopies, rootIssue); row.held {
		t.Fatalf("a cancel that failed left the running copy %+v, want none written", row)
	}

	cancelled, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()})
	if resumed := cancelled.Resumed; err != nil || len(resumed) != 1 || resumed[0] != claim.RoleArchitect {
		t.Fatalf("the cancel resumed %v (%v), want the one claim the release suspended", resumed, err)
	}
	if row := heldCopyRow(t, st, releaseBundleCopies, rootIssue); row.held {
		t.Fatalf("after the cancel the store holds the bundle %+v, whose row holds the issue's pod; want it gone with the mark", row)
	}
	kept := storedRunningCopy(t, st, rootIssue)
	if !bytes.Equal(kept.bundle, saved.Bundle) || kept.sourcePod != saved.PodUID || kept.attemptPod != "" ||
		!kept.at.After(savedAt) || !kept.writtenAt.Equal(kept.at) {
		t.Fatalf("after the cancel the running copy is %q off pod %q taken %s and written %s with an attempt by %q, want the bundle's work off %q, both its times the cancel's, after its save was written at %s, with no attempt",
			kept.bundle, kept.sourcePod, kept.at, kept.writtenAt, kept.attemptPod, saved.PodUID, savedAt)
	}

	// The pod the roles resume in restarts its roles container: its own disk holds the work.
	restore, err := st.TakeWorkspaceCopyForRestore(ctx, "legion", rootIssue, "pod-being-released")
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod the roles resume in was answered %v of a release bundle and %v of a running copy (%v), want nothing",
			restore.ReleaseBundle.Take, restore.Running.Take, err)
	}
	// An eviction of that pod before any woken turn brings a pod that is sent the work.
	restore, err = st.TakeWorkspaceCopyForRestore(ctx, "legion", rootIssue, "pod-after-an-eviction")
	sent := restore.Running.Bundle
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend || !bytes.Equal(sent.Bundle, saved.Bundle) ||
		sent.Head != saved.Head || !slices.Equal(sent.Tips, saved.Tips) || !maps.Equal(sent.Bookmarks, saved.Bookmarks) ||
		sent.Release != kept.restoreID {
		t.Fatalf("the pod after an eviction was answered %v: %q at %s with tips %v and bookmarks %v under id %d (%v), want the release's work sent under id %d",
			restore.Running.Take, sent.Bundle, sent.Head, sent.Tips, sent.Bookmarks, sent.Release, err, kept.restoreID)
	}
}

// A running copy newer than the cancelled release's bundle stays as it is, and the cancel drops
// the bundle's row alone: the newer work wins. Such a copy can only have come off another pod,
// since no copy off the pod a bundle came off is stored while the bundle stands — a pod that came
// back without spending the bundle's attempt and worked on.
func TestACancelLeavesARunningCopyNewerThanItsReleasesBundleAsItIs(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)
	var release int64
	if err := st.Pool().QueryRow(ctx, `select id from issue_releases where issue = $1`, rootIssue).Scan(&release); err != nil {
		t.Fatalf("read the id of the release of %s: %v", rootIssue, err)
	}
	if err := st.PutWorkspaceBundle(ctx, rootIssue, release, copyBegun(t, st), runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the cancelled release"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", PodUID: "pod-being-released"}); err != nil {
		t.Fatalf("save the release's bundle: %v", err)
	}
	newer := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of a pod that worked on"),
		Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", PodUID: "pod-that-worked-on"}
	if err := st.PutRunningWorkspaceCopy(ctx, "legion", rootIssue, copyBegun(t, st), newer); err != nil {
		t.Fatalf("take the newer running copy: %v", err)
	}
	before := storedRunningCopy(t, st, rootIssue)

	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if row := heldCopyRow(t, st, releaseBundleCopies, rootIssue); row.held {
		t.Fatalf("after the cancel the store holds the bundle %+v, want it gone with the mark", row)
	}
	after := storedRunningCopy(t, st, rootIssue)
	if after.restoreID != before.restoreID || after.sourcePod != newer.PodUID || !bytes.Equal(after.bundle, newer.Bundle) ||
		!after.at.Equal(before.at) || !after.writtenAt.Equal(before.writtenAt) {
		t.Fatalf("the newer running copy reads %d: %q off %q taken %s, want %d: %q off %q taken %s as it was",
			after.restoreID, after.bundle, after.sourcePod, after.at, before.restoreID, newer.Bundle, newer.PodUID, before.at)
	}
}

// A release whose save failed before it replaced the issue's running copy — a bundler that could
// not build the bundle, a bundle too large to carry — is cancelled, and the cancel keeps that
// copy. The pod goes on idle with its roles resumed, and the copy is the only copy of its work
// outside it: an eviction of that idle pod brings a pod that is sent the copy, where a cancel that
// deleted it would leave that pod nothing to restore. The pod the copy came off, the one the
// roles resume in, is answered nothing of it.
func TestACancelledReleaseKeepsTheRunningCopyItsFailedSaveLeft(t *testing.T) {
	const first, boundary, last = 18, 20, 22
	st := migratedStore(t)
	ctx := context.Background()
	openRunningTree(t, st)
	// The copy the turn's end took before the pod went quiet.
	taken := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the last turn"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", PodUID: "pod-being-released"}
	if err := st.PutRunningWorkspaceCopy(ctx, "legion", rootIssue, copyBegun(t, st), taken); err != nil {
		t.Fatalf("take the running copy: %v", err)
	}
	markRelease(t, st, first, boundary, last, 0, claim.RoleArchitect)
	releasedClaim(t, st, claim.RoleArchitect)

	// The save stored nothing, and the release is cancelled.
	if _, err := st.CancelIssueRelease(ctx, "legion", rootIssue, resumeRow, record.OutOfWorkflow, ReleaseCancelPause{At: time.Now()}); err != nil {
		t.Fatalf("carry the cancel out: %v", err)
	}
	if releaseMarked(t, st) {
		t.Fatal("the cancel left the release marked")
	}
	if row := heldCopyRow(t, st, runningCopyCopies, rootIssue); !row.held || row.size != taken.Size() {
		t.Fatalf("after the cancel the store holds the running copy %+v, want the %d bytes the failed save left", row, taken.Size())
	}
	restore, err := st.TakeWorkspaceCopyForRestore(ctx, "legion", rootIssue, "pod-being-released")
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod the roles resume in was answered %v of its own copy (%v), want nothing", restore.Running.Take, err)
	}
	restore, err = st.TakeWorkspaceCopyForRestore(ctx, "legion", rootIssue, "pod-after-an-eviction")
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend || string(restore.Running.Bundle.Bundle) != string(taken.Bundle) {
		t.Fatalf("the pod after an eviction was answered %v of %q (%v), want the copy sent", restore.Running.Take, restore.Running.Bundle.Bundle, err)
	}
}
