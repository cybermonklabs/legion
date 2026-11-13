package store

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// releasedPodUID is the pod every save of these tests took its work off: the pod the release is
// taking away, which the row records (workspace_bundles.source_pod_uid) and the take refuses by
// name (TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttemptReadToItsSecond).
const releasedPodUID = "pod-being-released"

// A released issue's bundle round-trips whole — its bytes, the commit its working copy sat on, the
// heads it carries, the bookmarks that pointed into it, the release that saved it and the pod its
// work came off — survives a second release replacing it, and goes when the restore is confirmed.
func TestAWorkspaceBundleRoundTripsAndIsDeletedOnConfirmation(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	// Every pod here is one the issue's wake made after the save it takes, which is the only
	// kind that is sent a bundle at all
	// (TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttemptReadToItsSecond).
	replacement := at.Add(time.Minute)
	saved := runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 1024),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e", "side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:        at,
		PodUID:    releasedPodUID,
	}
	if _, take, err := takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-0", replacement); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", take, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	read, take, err := takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-1", replacement)
	if err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("read the saved bundle: %v, %v", take, err)
	}
	if !bytes.Equal(read.Bundle, saved.Bundle) || read.Size() != saved.Size() {
		t.Fatalf("the bundle read back is %d bytes, want the %d saved", read.Size(), saved.Size())
	}
	if read.Head != saved.Head || !slices.Equal(read.Tips, saved.Tips) || !maps.Equal(read.Bookmarks, saved.Bookmarks) {
		t.Fatalf("the bundle read back is %+v, want %+v", read, saved)
	}
	if !read.At.Equal(at) {
		t.Fatalf("the bundle was taken at %s, want %s", read.At, at)
	}
	if read.Release != 11 {
		t.Fatalf("the bundle reads back as release %d, want the 11 that saved it", read.Release)
	}
	if read.PodUID != releasedPodUID {
		t.Fatalf("the bundle reads back as the work of pod %q, want the %q it was taken off", read.PodUID, releasedPodUID)
	}

	// A second release of the same issue replaces the row: the older work must not be restored
	// in place of the newer, and the new bundle has had no restore attempt, whatever became of
	// the one the row it replaced had.
	again := runtime.WorkspaceBundle{Bundle: []byte("PACK newer"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-of-the-second-release"}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, again); err != nil {
		t.Fatal(err)
	}
	read, take, err = takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != "PACK newer" || read.Head != again.Head || read.Release != 12 {
		t.Fatalf("after a second release the bundle is %q at %s of release %d (%v, %v), want the newer one", read.Bundle, read.Head, read.Release, take, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if _, take, err := takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the launcher confirmed the restore the bundle reads %v, %v; want none", take, err)
	}
	// A confirmation that arrives twice finishes rather than failing.
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
	if err := s.PutWorkspaceBundle(ctx, "", 11, saved); err == nil {
		t.Fatal("a bundle saved under no issue was accepted")
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 0, saved); err == nil {
		t.Fatal("a bundle saved under no release was accepted")
	}
	// A save that cannot say which pod the work came off is refused: the row could not then
	// tell the pod being released from the pod that replaces it, and the released pod's own
	// relaunch would spend the attempt its replacement needs.
	noPod := saved
	noPod.PodUID = ""
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, noPod); err == nil {
		t.Fatal("a bundle saved without the pod its work came off was accepted")
	}
}

// A launcher holds its restore report until some connection carries it, so a confirmation can
// reach the daemon after the issue's next release has saved a bundle of its own. The delete names
// the release it confirms: the bundle of that release goes, and a bundle the issue's newer release
// saved stays, since the work in it is on no disk any more — the teardown took the one it came off.
func TestARestoreConfirmationDeletesOnlyTheBundleOfTheReleaseItNames(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	first := runtime.WorkspaceBundle{Bundle: []byte("PACK first"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, first); err != nil {
		t.Fatal(err)
	}
	second := runtime.WorkspaceBundle{Bundle: []byte("PACK second"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-of-the-second-release"}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, second); err != nil {
		t.Fatal(err)
	}
	// The first release's restore is confirmed now, by a launcher that held its report through
	// the pod that saved the second.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 11); err != nil {
		t.Fatal(err)
	}
	kept, take, err := takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(kept.Bundle) != "PACK second" || kept.Release != 12 {
		t.Fatalf("a report of release 11 left %q of release %d (%v, %v), want the second release's bundle kept",
			kept.Bundle, kept.Release, take, err)
	}
	// The second release's own confirmation drops it, and a report the daemon reads twice
	// finishes rather than failing.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatal(err)
	}
	if _, take, err := takeOf(ctx, s, runtime.WorkspaceCopyReleaseBundle, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the report of release 12 the bundle reads %v, %v; want none", take, err)
	}
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
}

// The idle rule releases no pod of an issue whose stored bundle no pod has restored, and the row's
// own presence is that record: it is there from the save until a launcher's report deletes it.
func TestAStoredBundleReadsAsUnrestoredUntilItsConfirmation(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	unrestored := func() bool {
		t.Helper()
		var held bool
		if err := s.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			held, err = workspaceBundleUnrestored(ctx, tx, "INFRA-945")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return held
	}
	if unrestored() {
		t.Fatal("an issue that was never released reads as holding work no pod has restored")
	}
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	if !unrestored() {
		t.Fatal("an issue whose release saved a bundle reads as holding nothing")
	}
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 11); err != nil {
		t.Fatal(err)
	}
	if unrestored() {
		t.Fatal("an issue whose restore was confirmed still reads as holding work no pod has restored")
	}
}
