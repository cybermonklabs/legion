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
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-0", replacement); err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", restore.ReleaseBundle.Take, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, saved); err != nil {
		t.Fatal(err)
	}
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-1", replacement)
	read := restore.ReleaseBundle.Bundle
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("read the saved bundle: %v, %v", restore.ReleaseBundle.Take, err)
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
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	read = restore.ReleaseBundle.Bundle
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend || string(read.Bundle) != "PACK newer" || read.Head != again.Head || read.Release != 12 {
		t.Fatalf("after a second release the bundle is %q at %s of release %d (%v, %v), want the newer one", read.Bundle, read.Head, read.Release, restore.ReleaseBundle.Take, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the launcher confirmed the restore the bundle reads %v, %v; want none", restore.ReleaseBundle.Take, err)
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
// reach the daemon after the issue's next release has saved a bundle of its own. The keep names
// the release it confirms: the bundle of that release moves to the running copy, and a bundle the
// issue's newer release saved stays where it is, since the work in it is on no disk any more — the
// teardown took the one it came off. The late report changes nothing at all: no running copy is
// written from it either, since the work it names is gone and the newer bundle is the issue's.
func TestARestoreConfirmationKeepsOnlyTheBundleOfTheReleaseItNames(t *testing.T) {
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
	if err := s.KeepRestoredWorkspaceBundle(ctx, "INFRA-945", "pod-1", 11); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-945"); row.held {
		t.Fatalf("a report of release 11 wrote the running copy %+v, want nothing written", row)
	}
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	if kept := restore.ReleaseBundle; err != nil || kept.Take != runtime.WorkspaceBundleSend || string(kept.Bundle.Bundle) != "PACK second" || kept.Bundle.Release != 12 {
		t.Fatalf("a report of release 11 left %q of release %d (%v, %v), want the second release's bundle kept",
			kept.Bundle.Bundle, kept.Bundle.Release, kept.Take, err)
	}
	// The second release's own confirmation moves it out of the bundle table, and a report the
	// daemon reads twice finishes rather than failing.
	if err := s.KeepRestoredWorkspaceBundle(ctx, "INFRA-945", "pod-2", 12); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the report of release 12 the bundle reads %v, %v; want none", restore.ReleaseBundle.Take, err)
	}
	if err := s.KeepRestoredWorkspaceBundle(ctx, "INFRA-945", "pod-2", 12); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
	// A report that names no pod cannot say whose copy the work is now, and is refused.
	if err := s.KeepRestoredWorkspaceBundle(ctx, "INFRA-945", "", 12); err == nil {
		t.Fatal("a confirmation naming no pod was accepted")
	}
}

// The idle rule releases no pod of an issue whose stored bundle no pod has restored, and the row's
// own presence is that record: it is there from the save until the launcher of the pod it was sent
// to reports the restore.
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
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-945", "pod-that-restored-it", saved.At.Add(time.Minute)); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the pod after the release was answered %v (%v), want the bundle sent", restore.ReleaseBundle.Take, err)
	}
	if err := s.KeepRestoredWorkspaceBundle(ctx, "INFRA-945", "pod-that-restored-it", 11); err != nil {
		t.Fatal(err)
	}
	if unrestored() {
		t.Fatal("an issue whose restore was confirmed still reads as holding work no pod has restored")
	}
}

// A confirmation keeps only a bundle whose one attempt was a send to the confirming pod. Any other
// launcher of the issue reports work its pod does not hold — the pod the bundle was refused to, or
// one it was never sent to — and such a report keeps nothing: no running copy is written from it,
// and the bundle's row stays, with the hold it puts on the issue's pod, since no pod's disk holds
// that work.
func TestARestoreConfirmationFromAPodNotSentTheBundleKeepsNothingAndLiftsNoHold(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-945"
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	held := func(why string) {
		t.Helper()
		var unrestored bool
		if err := s.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			unrestored, err = workspaceBundleUnrestored(ctx, tx, issue)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !unrestored {
			t.Fatalf("after %s the bundle reads as restored, which lifts the hold on the issue's pod", why)
		}
		if row := heldCopyRow(t, s, runningCopyCopies, issue); row.held {
			t.Fatalf("after %s the issue holds the running copy %+v, want none written", why, row)
		}
	}
	if err := s.PutWorkspaceBundle(ctx, issue, 11, runtime.WorkspaceBundle{Bundle: []byte("PACK the first release"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}); err != nil {
		t.Fatal(err)
	}
	// A pod not created after the save is refused the bundle, which spends its attempt.
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-older-than-the-save", at.Add(-time.Minute)); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleRefusedOlder {
		t.Fatalf("a pod older than the save was answered %v (%v), want the bundle refused", restore.ReleaseBundle.Take, err)
	}
	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "pod-older-than-the-save", 11); err != nil {
		t.Fatal(err)
	}
	held("a report of the pod the bundle was refused to")

	// The issue's next release saves a bundle of its own, which is sent to pod-two.
	if err := s.PutWorkspaceBundle(ctx, issue, 12, runtime.WorkspaceBundle{Bundle: []byte("PACK the second release"),
		Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-of-the-second-release"}); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-two", at.Add(2*time.Hour)); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("pod-two was answered %v (%v), want the second release's bundle sent", restore.ReleaseBundle.Take, err)
	}
	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "another-pod-of-the-issue", 12); err != nil {
		t.Fatal(err)
	}
	held("a report of a pod the bundle was not sent to")

	// The report of the pod it was sent to keeps it.
	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "pod-two", 12); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, issue); row.held {
		t.Fatalf("after pod-two's report the store holds the bundle %+v, want it kept as pod-two's running copy", row)
	}
	if row := storedRunningCopy(t, s, issue); string(row.bundle) != "PACK the second release" || row.sourcePod != "pod-two" {
		t.Fatalf("after pod-two's report the running copy is %q off %q, want the second release's work off pod-two", row.bundle, row.sourcePod)
	}
}

// A confirmed restore of a release bundle keeps its work as the running copy of the pod that
// restored it, in the transaction that deletes the bundle's row. From the confirmation on the work
// is on that pod's disk alone, and a pod that does nothing takes no copy of its own, so a second
// end of it before any turn would otherwise take the work with it. As that pod's copy the work is
// answered nothing to the pod itself and sent whole to the pod after it; it is stamped at the
// confirmation rather than at the save; and it replaces the older running copy the bundle
// superseded. The delete and the write are one transaction: a write that fails leaves the bundle
// standing, and with it the hold on the issue's pod, rather than lifting the hold over work no row
// holds any more. The kept copy goes the way every running copy goes, here with an idle release of
// the restoring pod, whose save drops it.
func TestAConfirmedReleaseBundleIsKeptAsTheRestoringPodsRunningCopyInOneTransaction(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-945"
	// Whole seconds, which the store keeps exactly.
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	saved := runtime.WorkspaceBundle{Bundle: bytes.Repeat([]byte("PACK the release's work "), 64),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", Tips: []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"side": "da4f9f492e9f8349389c243743e0a000cd1c676f"}, At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, issue, 11, saved); err != nil {
		t.Fatal(err)
	}
	// A copy off a pod an eviction ended, taken a minute before the bundle: the older work.
	older := runtime.WorkspaceBundle{Bundle: []byte("PACK an older copy"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		At: at.Add(-time.Minute), PodUID: "pod-an-eviction-ended"}
	if err := s.PutRunningWorkspaceCopy(ctx, issue, older); err != nil {
		t.Fatal(err)
	}
	superseded := storedRunningCopy(t, s, issue)
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-two", at.Add(time.Minute))
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend || restore.Running.Take != runtime.WorkspaceBundleSuperseded {
		t.Fatalf("pod-two was answered %v of the bundle and %v of the running copy (%v), want the bundle sent and the older copy superseded",
			restore.ReleaseBundle.Take, restore.Running.Take, err)
	}

	// The running copy's table refuses every write for a moment, so the keep fails halfway.
	if _, err := s.Pool().Exec(ctx, `create function refuse_running_copy() returns trigger language plpgsql
		as $$ begin raise exception 'the running copy table refuses this write'; end $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool().Exec(ctx, `create trigger refuse_running_copy before insert or update on workspace_running_copies
		for each row execute function refuse_running_copy()`); err != nil {
		t.Fatal(err)
	}
	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "pod-two", 11); err == nil {
		t.Fatal("a confirmation whose running copy could not be written succeeded")
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, issue); !row.held {
		t.Fatal("a confirmation that could not keep the work deleted the bundle, lifting the hold over work no row holds")
	}
	if row := storedRunningCopy(t, s, issue); row.restoreID != superseded.restoreID || !bytes.Equal(row.bundle, older.Bundle) {
		t.Fatalf("a confirmation that failed left the running copy %d of %q, want the copy %d it found", row.restoreID, row.bundle, superseded.restoreID)
	}
	if _, err := s.Pool().Exec(ctx, `drop trigger refuse_running_copy on workspace_running_copies`); err != nil {
		t.Fatal(err)
	}

	// The confirmation run again keeps the work.
	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "pod-two", 11); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, issue); row.held {
		t.Fatalf("after the confirmation the store holds the bundle %+v, want it gone and its hold lifted", row)
	}
	kept := storedRunningCopy(t, s, issue)
	if !bytes.Equal(kept.bundle, saved.Bundle) || kept.size != saved.Size() || kept.sourcePod != "pod-two" || kept.attemptPod != "" {
		t.Fatalf("the running copy is %d bytes off pod %q with an attempt by %q, want the bundle's %d bytes off pod-two with no attempt",
			kept.size, kept.sourcePod, kept.attemptPod, saved.Size())
	}
	if kept.restoreID == superseded.restoreID {
		t.Fatalf("the kept copy took over the restore id %d of the copy it replaced, want one of its own", kept.restoreID)
	}
	if !kept.at.After(saved.At) {
		t.Fatalf("the kept copy is stamped %s, want the confirmation's time, after the save at %s", kept.at, saved.At)
	}

	// The pod that restored the work is answered nothing of it: its own workspace holds it.
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-two", at.Add(time.Minute))
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleNone || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("pod-two asking again was answered %v and %v (%v), want nothing", restore.ReleaseBundle.Take, restore.Running.Take, err)
	}
	if row := storedRunningCopy(t, s, issue); row.attemptPod != "" {
		t.Fatalf("pod-two's own ask spent the attempt of its kept copy on %q", row.attemptPod)
	}
	// The pod after it, made by a second end before any turn of pod-two, is sent the work whole.
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-three", time.Now().UTC().Add(time.Hour))
	sent := restore.Running.Bundle
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend || !bytes.Equal(sent.Bundle, saved.Bundle) ||
		sent.Head != saved.Head || !slices.Equal(sent.Tips, saved.Tips) || !maps.Equal(sent.Bookmarks, saved.Bookmarks) ||
		sent.Release != kept.restoreID || sent.PodUID != "pod-two" {
		t.Fatalf("pod-three was answered %v: %d bytes at %s with tips %v and bookmarks %v under id %d off %q (%v), want the bundle's work under id %d off pod-two",
			restore.Running.Take, len(sent.Bundle), sent.Head, sent.Tips, sent.Bookmarks, sent.Release, sent.PodUID, err, kept.restoreID)
	}

	// An idle release of pod-two saves its workspace as a bundle of its own and drops the kept
	// copy in the same transaction.
	if err := s.PutWorkspaceBundle(ctx, issue, 12, runtime.WorkspaceBundle{Bundle: []byte("PACK pod-two's release"),
		Head: saved.Head, At: time.Now().UTC(), PodUID: "pod-two"}); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, issue); row.held {
		t.Fatalf("the idle release of pod-two left the kept copy %+v", row)
	}
}

// A running copy the restore's own rule reads as newer than the confirmed bundle stays as it is:
// the newer work wins. Such a copy came off another pod after the bundle's save — a pod that came
// back without restoring the bundle and worked on — and the bundle's row goes all the same, since
// the restore it confirms is done.
func TestAConfirmedReleaseBundleLeavesANewerRunningCopyAsItIs(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-945"
	at := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the release's work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, issue, 11, saved); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-two", at.Add(time.Minute)); err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("pod-two was answered %v (%v), want the bundle sent", restore.ReleaseBundle.Take, err)
	}
	newer := runtime.WorkspaceBundle{Bundle: []byte("PACK a newer copy"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		At: at.Add(2 * time.Minute), PodUID: "pod-that-worked-on"}
	if err := s.PutRunningWorkspaceCopy(ctx, issue, newer); err != nil {
		t.Fatal(err)
	}
	before := storedRunningCopy(t, s, issue)

	if err := s.KeepRestoredWorkspaceBundle(ctx, issue, "pod-two", 11); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, releaseBundleCopies, issue); row.held {
		t.Fatalf("after the confirmation the store holds the bundle %+v, want it gone", row)
	}
	after := storedRunningCopy(t, s, issue)
	if after.restoreID != before.restoreID || after.sourcePod != newer.PodUID || !bytes.Equal(after.bundle, newer.Bundle) || !after.at.Equal(newer.At) {
		t.Fatalf("the newer running copy reads %d off %q taken %s, want %d off %q taken %s as it was",
			after.restoreID, after.sourcePod, after.at, before.restoreID, newer.PodUID, newer.At)
	}
}
