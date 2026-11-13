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
// name (TestThePodTheWorkCameOffIsAnsweredNothingAndSpendsNoAttempt).
const releasedPodUID = "pod-being-released"

// savedRepo is the repository every save of these tests was work of
// (workspace_bundles.repo). No issue of these tests is recorded in the issues table unless the
// test records one, so the take reads no repository to disagree with it and refuses nothing by
// it (TestABundleOfARepositoryTheIssueHasLeftIsRefusedAndKept).
const savedRepo = "acme/widgets"

// A released issue's bundle round-trips whole — its bytes, the commit its working copy sat on, the
// heads it carries, the bookmarks that pointed into it, the release that saved it and the pod its
// work came off — survives a second release replacing it, and goes when the restore is confirmed.
func TestAWorkspaceBundleRoundTripsAndIsDeletedOnConfirmation(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	// Every pod here is one the issue's wake made after the save it takes, which is the only
	// kind that is sent a bundle at all
	// (TestAPodNotNewerThanTheSaveSpendsTheAttemptAndClosesTheBundleToEveryLaterPod).
	replacement := at.Add(time.Minute)
	saved := runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 1024),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e", "side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:        at,
		PodUID:    releasedPodUID,
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-0", replacement); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", take, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-1", replacement)
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
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, again); err != nil {
		t.Fatal(err)
	}
	read, take, err = s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != "PACK newer" || read.Head != again.Head || read.Release != 12 {
		t.Fatalf("after a second release the bundle is %q at %s of release %d (%v, %v), want the newer one", read.Bundle, read.Head, read.Release, take, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the launcher confirmed the restore the bundle reads %v, %v; want none", take, err)
	}
	// A confirmation that arrives twice finishes rather than failing.
	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
	if err := s.PutWorkspaceBundle(ctx, "", 11, savedRepo, saved); err == nil {
		t.Fatal("a bundle saved under no issue was accepted")
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 0, savedRepo, saved); err == nil {
		t.Fatal("a bundle saved under no release was accepted")
	}
	// A save that cannot say which pod the work came off is refused: the row could not then
	// tell the pod being released from the pod that replaces it, and the released pod's own
	// relaunch would spend the attempt its replacement needs.
	noPod := saved
	noPod.PodUID = ""
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, noPod); err == nil {
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
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, first); err != nil {
		t.Fatal(err)
	}
	second := runtime.WorkspaceBundle{Bundle: []byte("PACK second"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-of-the-second-release"}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, second); err != nil {
		t.Fatal(err)
	}
	// The first release's restore is confirmed now, by a launcher that held its report through
	// the pod that saved the second.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 11); err != nil {
		t.Fatal(err)
	}
	kept, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(kept.Bundle) != "PACK second" || kept.Release != 12 {
		t.Fatalf("a report of release 11 left %q of release %d (%v, %v), want the second release's bundle kept",
			kept.Bundle, kept.Release, take, err)
	}
	// The second release's own confirmation drops it, and a report the daemon reads twice
	// finishes rather than failing.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("after the report of release 12 the bundle reads %v, %v; want none", take, err)
	}
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
}

// A bundle is sent to one pod's launcher once. The launcher's own record of the restore it ran
// is in the process that ran it, so a roles container that restarts comes back to the same disk
// with that record gone and the workspace hours of work past the save: a bundle sent there a
// second time would put the saved head and the saved bookmarks over that work. The daemon's
// record is the one that survives, and it is written in the read itself, before a slice leaves
// the daemon.
func TestABundleIsTakenForOneRestoreAttemptAndThenKept(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	// Each pod of this test is one the issue's wake made after the save it is answered for:
	// the rule that refuses an older one is
	// TestAPodNotNewerThanTheSaveSpendsTheAttemptAndClosesTheBundleToEveryLaterPod.
	replacement := at.Add(time.Minute)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	first, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-uid-1", replacement)
	if err != nil || take != runtime.WorkspaceBundleSend || string(first.Bundle) != string(saved.Bundle) {
		t.Fatalf("the pod that replaces the released one was answered %q (%v, %v), want the saved work", first.Bundle, take, err)
	}
	// The same pod again, which is its roles container restarting, and then the pod that
	// replaces it, and then a third after an eviction. Each is newer than the save, so the
	// spent attempt is the only thing refusing them.
	for i, pod := range []string{"pod-uid-1", "pod-uid-2", "pod-uid-3"} {
		created := replacement.Add(time.Duration(i) * time.Minute)
		again, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", pod, created)
		if err != nil || take != runtime.WorkspaceBundleNone {
			t.Fatalf("%s was answered %q (%v, %v) after the bundle's one attempt, want nothing to send", pod, again.Bundle, take, err)
		}
	}
	// The work is kept, and it holds the issue's pod until the issue's work is over.
	size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945")
	if err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the attempt the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
	}
	// The issue's next release saves work of its own, which has an attempt of its own.
	newer := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the next release"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-uid-3"}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, newer); err != nil {
		t.Fatal(err)
	}
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-uid-4", at.Add(time.Hour+time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(newer.Bundle) || read.Release != 12 {
		t.Fatalf("the pod after the next release was answered %q of release %d (%v, %v), want that release's own work", read.Bundle, read.Release, take, err)
	}
	// A send with no pod to name is refused: the record is what the rule rests on, and a row
	// marked by no pod names nothing an operator can read. A send that cannot say when its pod
	// was created is refused for the same reason: the rule has nothing to read.
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "", replacement); err == nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a take that named no pod was answered %v, %v; want it refused", take, err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-uid-5", time.Time{}); err == nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a take that said nothing of when its pod was created was answered %v, %v; want it refused", take, err)
	}
}

// The pod the work came off is answered nothing and spends nothing. Every start of every role
// takes the bundle for its pod, and a release stops the roles of the pod it is releasing: a role
// relaunched in that pod, between the save and the teardown that deletes the pod, is one of those
// starts. Its workspace is the one the work was taken from — it needs nothing put back — and were
// it to spend the attempt, the pod that replaces it, whose workspace holds only what was pushed,
// would be sent nothing while the row stayed and held the issue's pod
// (workflow.IdleHoldBundleUnrestored).
//
// It is the pod's uid that says so, not its clock: the save records the pod it was taken off
// (runtime.WorkspaceBundle.PodUID), so a node whose clock runs ahead of the API server's cannot
// make that pod look like the replacement, nor the replacement like it.
func TestThePodTheWorkCameOffIsAnsweredNothingAndSpendsNoAttempt(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 750_000_000, time.UTC)
	released := at.Add(-3 * time.Hour)
	saved := runtime.WorkspaceBundle{
		Bundle: []byte("PACK the released work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:   []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:     at,
		PodUID: releasedPodUID,
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	// Two relaunches in the pod being released: neither is answered, and neither spends the
	// attempt. The second is answered the same as the first, which is what says the first left
	// the row as it found it.
	for _, created := range []time.Time{released, at.Add(time.Minute)} {
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", releasedPodUID, created)
		if err != nil || take != runtime.WorkspaceBundleNone {
			t.Fatalf("the pod the work came off, asking as created %s against the save at %s, was answered %q (%v, %v); want nothing to send",
				created, at, got.Bundle, take, err)
		}
	}
	// The pod the wake made after the save takes the whole of it, attempt and all.
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-replacement", at.Add(time.Minute))
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(saved.Bundle) || read.Head != saved.Head ||
		!slices.Equal(read.Tips, saved.Tips) || read.Release != 11 {
		t.Fatalf("the pod created after the save was answered %q at %s of release %d with tips %v (%v, %v), want the saved work whole",
			read.Bundle, read.Head, read.Release, read.Tips, take, err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-replacement", at.Add(time.Minute)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("the replacement pod's roles container restarted and was answered %v, %v; want the spent attempt to refuse it", take, err)
	}

	// A pod's creationTimestamp has one-second resolution, so a pod created inside the save's
	// own second cannot be ordered against it and is taken as the newer one: refusing it would
	// cost a legitimate restore, and the pod being released is refused by its uid anyway.
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-of-the-same-second", at.Truncate(time.Second)); err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("a pod created inside the save's own second was answered %v, %v; want the save read to its second", take, err)
	}
}

// A pod that is not the one the work came off and was not created after the save spends the
// bundle's one attempt on a refusal, and no later pod is sent that bundle.
//
// Such a pod is one of two things, and both end the same way. It is a pod the release did not
// replace — one older than the save that is still standing — or it is the replacement itself
// under a node clock running ahead of the API server's, which stamps the save in the pod's own
// future. Either pod starts on what its workspace fetch provisioned and its roles work and push
// from there for as long as it lives, so the pod created after it holds their pushed work;
// a bundle the daemon kept open for that pod would be applied over it, moving the issue's
// bookmarks back behind work that was pushed after the save, which is the harm the one-attempt
// rule exists to stop.
//
// The bytes stay: nothing restored them, so the row holds the issue's pod
// (workflow.IdleHoldBundleUnrestored) and keeps the work for a person to recover.
func TestAPodNotNewerThanTheSaveSpendsTheAttemptAndClosesTheBundleToEveryLaterPod(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{
		Bundle:    []byte("PACK the released work"),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        at,
		PodUID:    releasedPodUID,
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	// The pod the teardown's launch made, stamped a minute before the save by a node clock that
	// runs ahead of the API server's.
	refused, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-skewed-replacement", at.Add(-time.Minute))
	if err != nil || take != runtime.WorkspaceBundleRefusedOlder {
		t.Fatalf("a pod created before the save was answered %v (%v), want it refused as not newer than the save", take, err)
	}
	if len(refused.Bundle) != 0 {
		t.Fatalf("the refused pod was answered %d bytes of bundle, want none", len(refused.Bundle))
	}
	if !refused.At.Equal(at) || refused.Release != 11 {
		t.Fatalf("the refusal reads the save as release %d at %s, want release 11 at %s: the warning names both", refused.Release, refused.At, at)
	}
	// Every pod after it is sent nothing, however new: the first of them is the pod an eviction
	// or an update round makes while the refused pod's roles are pushing, and the bundle would
	// land on a workspace provisioned from what they pushed.
	for _, pod := range []struct {
		uid     string
		created time.Time
	}{
		{"pod-after-an-eviction", at.Add(time.Hour)},
		{"pod-after-a-park-and-unpark", at.Add(2 * time.Hour)},
	} {
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", pod.uid, pod.created)
		if err != nil || take != runtime.WorkspaceBundleNone {
			t.Fatalf("%s, created %s after the save at %s, was answered %q (%v, %v); want nothing: the refusal was the bundle's one attempt",
				pod.uid, pod.created, at, got.Bundle, take, err)
		}
		if len(got.Bookmarks) != 0 {
			t.Fatalf("%s was answered the bookmarks %v, which a restore would move back over the work pushed since the save", pod.uid, got.Bookmarks)
		}
	}
	// The work is kept whole for a person to carry out of the database, and the row goes on
	// holding the issue's pod.
	size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945")
	if err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the refusal the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
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
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
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

// recordIssueOn records the issue on repo, as the daemon's record holds it: the one column the
// take reads of the issues table, with the rest of the row as a running root's.
func recordIssueOn(t *testing.T, s *Store, issue, repo string) {
	t.Helper()
	if _, err := s.Pool().Exec(context.Background(), `insert into issues
		(key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo)
		values ($1, $1, 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1, $2)
		on conflict (key) do update set repo = excluded.repo`, issue, repo); err != nil {
		t.Fatalf("record %s on the repository %s: %v", issue, repo, err)
	}
}

// bundleAttempt is what the row records of its one restore attempt: the pod it was spent on and
// what it did, both empty while nothing has asked.
func bundleAttempt(t *testing.T, s *Store, issue string) (pod, outcome string) {
	t.Helper()
	if err := s.Pool().QueryRow(context.Background(), `select coalesce(attempt_pod_uid, ''), coalesce(attempt_outcome, '')
		from workspace_bundles where issue = $1`, issue).Scan(&pod, &outcome); err != nil {
		t.Fatalf("read the restore attempt of the bundle of %s: %v", issue, err)
	}
	return pod, outcome
}

// refusedBundle is one row of workspace_bundles_refused: a bundle the take refused for its
// repository and moved out of the issue's one row, read back whole.
type refusedBundle struct {
	release            int64
	repo, head         string
	sourcePod, refused string
	bundle             []byte
	at                 time.Time
}

// refusedBundles is every bundle the store keeps of issue after refusing it for its repository,
// oldest refusal first. A row whose size_bytes is not its bundle's length fails the test, since
// the move is meant to carry the row as it was.
func refusedBundles(t *testing.T, s *Store, issue string) []refusedBundle {
	t.Helper()
	rows, err := s.Pool().Query(context.Background(), `select release_id, repo, head, source_pod_uid, refused_pod_uid, bundle, size_bytes, at
		from workspace_bundles_refused where issue = $1 order by id`, issue)
	if err != nil {
		t.Fatalf("read the refused bundles of %s: %v", issue, err)
	}
	defer rows.Close()
	var kept []refusedBundle
	for rows.Next() {
		var r refusedBundle
		var size int
		if err := rows.Scan(&r.release, &r.repo, &r.head, &r.sourcePod, &r.refused, &r.bundle, &size, &r.at); err != nil {
			t.Fatalf("read a refused bundle of %s: %v", issue, err)
		}
		if size != len(r.bundle) {
			t.Fatalf("the refused bundle of %s of release %d records %d bytes and holds %d", issue, r.release, size, len(r.bundle))
		}
		kept = append(kept, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the refused bundles of %s: %v", issue, err)
	}
	return kept
}

// A correction of the issue's repository in Dispatch moves the issue to another
// clone, and the work a release saved before it is work of the repository the issue has left.
// The pod asking is a clone of the repository the issue is on now, which holds none of the saved
// commits' bases, so the bundle is refused however new that pod is. The refusal is the bundle's
// one attempt, and the bundle moves whole to workspace_bundles_refused: the work is in the
// database alone, and the daemon writes on the issue where it is (internal/daemon's
// bundleRepositoryMessage).
func TestABundleOfARepositoryTheIssueHasLeftIsRefusedAndKept(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{
		Bundle:    []byte("PACK the work of the old repository"),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Bookmarks: map[string]string{"legion/INFRA-945": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        at,
		PodUID:    releasedPodUID,
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	// Dispatch corrected the issue's repository, and the record followed it
	// (record.(*Postgres).CorrectIssueRepository).
	recordIssueOn(t, s, "INFRA-945", "zeta/tools")

	// The pod the correction's own replacement made: created after the save, which is the pod
	// every other rule here sends the work to.
	refused, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-of-the-new-repository", at.Add(time.Minute))
	if err != nil || take != runtime.WorkspaceBundleRefusedRepository {
		t.Fatalf("a pod of the repository the issue moved to was answered %v (%v), want it refused for the bundle's repository", take, err)
	}
	if len(refused.Bundle) != 0 {
		t.Fatalf("the refused pod was answered %d bytes of bundle, want none", len(refused.Bundle))
	}
	if refused.Repo != savedRepo || refused.Head != saved.Head || !refused.At.Equal(at) {
		t.Fatalf("the refusal reads the save as %s at %s of %s, want %s at %s of %s: the message on the issue names all three",
			refused.Repo, refused.Head, refused.At, savedRepo, saved.Head, at)
	}
	kept := refusedBundles(t, s, "INFRA-945")
	if len(kept) != 1 {
		t.Fatalf("the store keeps %d refused bundles of the issue, want the one refused", len(kept))
	}
	if got := kept[0]; got.release != 11 || got.repo != savedRepo || got.head != saved.Head || got.sourcePod != releasedPodUID ||
		got.refused != "pod-of-the-new-repository" || !got.at.Equal(at) || !bytes.Equal(got.bundle, saved.Bundle) {
		t.Fatalf("the refused bundle is kept as release %d of %s at %s off %s, refused to %s, saved %s, %d bytes; want the row as it was saved, refused to the pod that asked",
			got.release, got.repo, got.head, got.sourcePod, got.refused, got.at, len(got.bundle))
	}
	// The attempt is spent: every pod after it is answered nothing at all.
	if got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-after-an-eviction", at.Add(time.Hour)); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("the pod after the refusal was answered %q (%v, %v); want nothing: the refusal was the bundle's one attempt", got.Bundle, take, err)
	}
	// The issue's one row is free for the next release's save: the refused work is not in it.
	if size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || held {
		t.Fatalf("after the refusal the issue's row holds %d bytes (held=%t, %v), want none: the refused bundle moved out", size, held, err)
	}
}

// The issue was told that the daemon keeps the refused work, so nothing a later release of the
// issue does may reach it. The issue's one row in workspace_bundles is what every release writes:
// the release of the issue's next pod saves its own work over that row, or deletes the row when it
// has nothing unpushed (daemon's outbox.bundleIssueWorkspace), and the end of the issue's work
// deletes it too (dropReleasedWork). A refused bundle left in that row was lost to the first of
// these. A second refusal of the same issue — Dispatch moves it back to the repository it left,
// and the next pod refuses the work of the repository between — is a row of its own beside the
// first, and the first is still not sent to that pod, although that pod is a clone of its
// repository again.
func TestARefusedBundleOutlivesEveryLaterSaveAndDeleteOfTheIssuesBundle(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	refused := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the old repository"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	// refuse saves the work of release 11 out of a pod of acme/widgets, moves the issue to
	// zeta/tools as a correction does, and has the pod of zeta/tools refuse it.
	refuse := func(t *testing.T, s *Store) {
		t.Helper()
		ctx := context.Background()
		if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, refused); err != nil {
			t.Fatal(err)
		}
		recordIssueOn(t, s, "INFRA-945", "zeta/tools")
		if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-of-zeta-tools", at.Add(time.Minute)); err != nil || take != runtime.WorkspaceBundleRefusedRepository {
			t.Fatalf("the pod of zeta/tools was answered %v (%v), want the bundle refused for its repository", take, err)
		}
	}
	// stillKept fails unless the bytes of the refused work are somewhere in the store: the issue's
	// row or the refused table, whichever holds them. Where they are is the other tests' to say;
	// this one says only that no later save or delete of the issue's bundle took them.
	stillKept := func(t *testing.T, s *Store) {
		t.Helper()
		var kept bool
		if err := s.Pool().QueryRow(context.Background(), `select exists (
			select 1 from workspace_bundles where issue = $1 and bundle = $2
			union all select 1 from workspace_bundles_refused where issue = $1 and bundle = $2)`,
			"INFRA-945", refused.Bundle).Scan(&kept); err != nil || !kept {
			t.Fatalf("the refused work of release 11 is in the store = %t (%v), want it kept", kept, err)
		}
	}

	t.Run("the next release saves work of its own", func(t *testing.T) {
		ctx := context.Background()
		s := migratedStore(t)
		refuse(t, s)
		own := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of zeta/tools"),
			Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(time.Hour), PodUID: "pod-of-zeta-tools"}
		if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, "zeta/tools", own); err != nil {
			t.Fatal(err)
		}
		stillKept(t, s)

		// Dispatch moves the issue back to acme/widgets. The pod after that is refused the work
		// of zeta/tools, which is kept beside the first, and is sent neither.
		recordIssueOn(t, s, "INFRA-945", savedRepo)
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-of-acme-widgets-again", at.Add(2*time.Hour))
		if err != nil || take != runtime.WorkspaceBundleRefusedRepository || len(got.Bundle) != 0 {
			t.Fatalf("the pod of acme/widgets again was answered %v with %d bytes (%v), want the work of zeta/tools refused and nothing sent", take, len(got.Bundle), err)
		}
		if got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-after-it", at.Add(3*time.Hour)); err != nil || take != runtime.WorkspaceBundleNone {
			t.Fatalf("the pod after it was answered %q (%v, %v), want nothing: a refused bundle is never offered again", got.Bundle, take, err)
		}
		kept := refusedBundles(t, s, "INFRA-945")
		if len(kept) != 2 || kept[1].release != 12 || kept[1].repo != "zeta/tools" || !bytes.Equal(kept[1].bundle, own.Bundle) {
			t.Fatalf("the store keeps %d refused bundles of the issue, want the two refusals, the second the work of zeta/tools", len(kept))
		}
		if kept[0].release != 11 || !bytes.Equal(kept[0].bundle, refused.Bundle) {
			t.Fatalf("the first refused bundle is release %d with %d bytes, want the work of release 11 kept as it was", kept[0].release, len(kept[0].bundle))
		}
	})
	t.Run("the next release saves nothing", func(t *testing.T) {
		s := migratedStore(t)
		refuse(t, s)
		if err := s.DeleteWorkspaceBundle(context.Background(), "INFRA-945"); err != nil {
			t.Fatal(err)
		}
		stillKept(t, s)
	})
}

// The repository refusal reads two names and refuses on the difference alone. A bundle of the
// repository the issue is recorded on now is the ordinary case and is sent, and so is a row
// saved before the repo column, whose repository nothing recorded: such a row behaves as it did
// before this release.
func TestABundleOfTheIssuesOwnRepositoryIsStillSent(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		repo string
	}{
		{name: "the repository the issue is recorded on", repo: savedRepo},
		{name: "a row saved before the repo column", repo: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
				Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
			if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, tc.repo, saved); err != nil {
				t.Fatal(err)
			}
			recordIssueOn(t, s, "INFRA-945", savedRepo)

			read, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-replacement", at.Add(time.Minute))
			if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(saved.Bundle) {
				t.Fatalf("the replacement pod was answered %q (%v, %v), want the saved work", read.Bundle, take, err)
			}
			if read.Repo != tc.repo {
				t.Fatalf("the bundle reads back as work of %q, want %q: the row answers the repository it was saved under", read.Repo, tc.repo)
			}
			if pod, outcome := bundleAttempt(t, s, "INFRA-945"); pod != "pod-replacement" || outcome != "sent" {
				t.Fatalf("the row records the attempt as %q by %s, want sent to the pod that asked", outcome, pod)
			}
		})
	}
}

// A bundle refused for its repository holds the issue's pod for nothing, and the idle rule reads
// that here: the refusal moved it out of workspace_bundles, where no release can write over it,
// and no restore is ever recorded for it, so a hold would never end
// (workflow.IdleHoldBundleUnrestored). A bundle refused as older is the opposite: it is still the
// issue's one row, its work could still be carried back by hand, and a release that overwrote it
// would take the only copy, so that row goes on holding the pod.
func TestABundleRefusedForItsRepositoryHoldsNoPod(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	for _, tc := range []struct {
		name     string
		repo     string
		created  time.Time
		want     runtime.WorkspaceBundleTake
		holdsPod bool
	}{
		{
			name: "refused for the repository the issue has left", repo: savedRepo, created: at.Add(time.Minute),
			want: runtime.WorkspaceBundleRefusedRepository, holdsPod: false,
		},
		{
			name: "refused as no newer than the save", repo: "zeta/tools", created: at.Add(-time.Minute),
			want: runtime.WorkspaceBundleRefusedOlder, holdsPod: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, tc.repo, saved); err != nil {
				t.Fatal(err)
			}
			// The issue is recorded on zeta/tools, which is the repository the first case's
			// bundle is not of and the second case's is.
			recordIssueOn(t, s, "INFRA-945", "zeta/tools")
			if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "INFRA-945", "pod-replacement", tc.created); err != nil || take != tc.want {
				t.Fatalf("the pod was answered %v (%v), want %v", take, err, tc.want)
			}
			var held bool
			if err := s.Tx(ctx, func(tx pgx.Tx) error {
				var err error
				held, err = workspaceBundleUnrestored(ctx, tx, "INFRA-945")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if held != tc.holdsPod {
				t.Fatalf("the refused bundle holds the issue's pod = %t, want %t", held, tc.holdsPod)
			}
			// The bytes stay either way: the refusal as older leaves them in the issue's row, with
			// the attempt recorded on it, and the refusal for the repository moves them.
			if tc.holdsPod {
				if _, outcome := bundleAttempt(t, s, "INFRA-945"); outcome != "refused_older" {
					t.Fatalf("the row records the attempt as %q, want refused_older", outcome)
				}
				if size, stored, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || !stored || size != len(saved.Bundle) {
					t.Fatalf("the store holds %d bytes (held=%t, %v), want the %d saved kept", size, stored, err, len(saved.Bundle))
				}
				return
			}
			if kept := refusedBundles(t, s, "INFRA-945"); len(kept) != 1 || !bytes.Equal(kept[0].bundle, saved.Bundle) {
				t.Fatalf("the store keeps %d refused bundles of the issue, want the one saved, whole", len(kept))
			}
		})
	}
}
