package store

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// releasedPodUID is the pod every save of these tests took its work off: the pod the release is
// taking away, which the row records (workspace_bundles.source_pod_uid) and the take refuses by
// name (TestThePodTheWorkCameOffIsAnsweredNothingAndSpendsNoAttempt).
const releasedPodUID = "pod-being-released"

// savedRepo is the repository every save of these tests was work of
// (workspace_bundles.repo). The issue bundleStore records names no repository, and no other
// issue of these tests is recorded in the issues table unless the test records one, so the take
// reads no repository to disagree with it and refuses nothing by it
// (TestABundleOfARepositoryTheIssueHasLeftIsRefusedAndKept).
const savedRepo = "acme/widgets"

// noRenames is the rule every take of these tests decides a left repository by but the rename's
// own: a deployment's that configures no rename (ghrepo.Renames.Left), which compares names
// without regard to case.
var noRenames = ghrepo.Renames{}.Left

// bundleProject is the token of the project whose pods ask for INFRA-945's bundle in these tests,
// and bundleStore is a migrated store whose issues table records INFRA-945 under that project, as
// the release that saved a bundle of it leaves it: a bundle is a project's to take only where the
// project records its issue (TakeWorkspaceBundleForRestore).
const bundleProject = "infra"

func bundleStore(t *testing.T) *Store {
	t.Helper()
	s := migratedStore(t)
	if _, err := s.Pool().Exec(context.Background(), `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('INFRA-945', 'INFRA-945', 'INFRA', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record INFRA-945: %v", err)
	}
	return s
}

// A released issue's bundle round-trips whole — its bytes, the commit its working copy sat on, the
// heads it carries, the bookmarks that pointed into it, the release that saved it and the pod its
// work came off — survives a second release replacing it, and goes when the restore is confirmed.
func TestAWorkspaceBundleRoundTripsAndIsDeletedOnConfirmation(t *testing.T) {
	ctx := context.Background()
	s := bundleStore(t)
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
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-0", replacement, noRenames); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue never released reads %v, %v; want no bundle", take, err)
	}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-1", replacement, noRenames)
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
	read, take, err = s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != "PACK newer" || read.Head != again.Head || read.Release != 12 {
		t.Fatalf("after a second release the bundle is %q at %s of release %d (%v, %v), want the newer one", read.Bundle, read.Head, read.Release, take, err)
	}
	if len(read.Tips) != 0 || len(read.Bookmarks) != 0 {
		t.Fatalf("the newer bundle reads tips %v and bookmarks %v, want neither", read.Tips, read.Bookmarks)
	}

	if err := s.DeleteWorkspaceBundle(ctx, "INFRA-945"); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute), noRenames); err != nil || take != runtime.WorkspaceBundleNone {
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
	s := bundleStore(t)
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
	kept, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-2", at.Add(time.Hour+time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(kept.Bundle) != "PACK second" || kept.Release != 12 {
		t.Fatalf("a report of release 11 left %q of release %d (%v, %v), want the second release's bundle kept",
			kept.Bundle, kept.Release, take, err)
	}
	// The second release's own confirmation drops it, and a report the daemon reads twice
	// finishes rather than failing.
	if err := s.DeleteRestoredWorkspaceBundle(ctx, "INFRA-945", 12); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-3", at.Add(time.Hour+time.Minute), noRenames); err != nil || take != runtime.WorkspaceBundleNone {
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
	s := bundleStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	// Each pod of this test is one the issue's wake made after the save it is answered for:
	// the rule that refuses an older one is
	// TestAPodNotNewerThanTheSaveSpendsTheAttemptAndClosesTheBundleToEveryLaterPod.
	replacement := at.Add(time.Minute)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	first, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-uid-1", replacement, noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(first.Bundle) != string(saved.Bundle) {
		t.Fatalf("the pod that replaces the released one was answered %q (%v, %v), want the saved work", first.Bundle, take, err)
	}
	// The same pod again, which is its roles container restarting, and then the pod that
	// replaces it, and then a third after an eviction. Each is newer than the save, so the
	// spent attempt is the only thing refusing them.
	for i, pod := range []string{"pod-uid-1", "pod-uid-2", "pod-uid-3"} {
		created := replacement.Add(time.Duration(i) * time.Minute)
		again, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", pod, created, noRenames)
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
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-uid-4", at.Add(time.Hour+time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(newer.Bundle) || read.Release != 12 {
		t.Fatalf("the pod after the next release was answered %q of release %d (%v, %v), want that release's own work", read.Bundle, read.Release, take, err)
	}
	// A send with no pod to name is refused: the record is what the rule rests on, and a row
	// marked by no pod names nothing an operator can read. A send that cannot say when its pod
	// was created is refused for the same reason: the rule has nothing to read. So is a send
	// that names no project, since whose bundle it is rests on the project.
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "", replacement, noRenames); err == nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a take that named no pod was answered %v, %v; want it refused", take, err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-uid-5", time.Time{}, noRenames); err == nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a take that said nothing of when its pod was created was answered %v, %v; want it refused", take, err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, "", "INFRA-945", "pod-uid-5", replacement, noRenames); err == nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("a take that named no project was answered %v, %v; want it refused", take, err)
	}
}

// The one writer of a bundle is the save of an idle release, which an outbox row runs
// (internal/daemon's bundleIssueWorkspace), and a daemon's outbox runs only the rows of its own
// project's keys, of a release its idle rule marked over a record of its own project
// (readIdleView). So a bundle of an issue another project records was saved by that project's
// daemon, for its own pod, on a database the two daemons share. The operator's spawn makes a
// claim of that issue under this project's token, and a new pod of that claim is answered
// nothing and leaves the row and its one attempt as they were, for the other daemon's pod. A
// bundle of an issue this project records is taken as before.
func TestARestoreTakesNoBundleOfAnIssueAnotherProjectRecords(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	const otherIssue = "OTHER-8"
	if _, err := s.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ($1, $1, 'OTHER', 'root', 'implementing', 1, 'in_progress', 'A', 1)`, otherIssue); err != nil {
		t.Fatalf("record %s as OTHER's issue: %v", otherIssue, err)
	}
	others := runtime.WorkspaceBundle{Bundle: []byte("PACK of OTHER's pod"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: at, PodUID: "pod-other-released"}
	if err := s.PutWorkspaceBundle(ctx, otherIssue, 21, savedRepo, others); err != nil {
		t.Fatalf("save OTHER's bundle of %s: %v", otherIssue, err)
	}

	got, take, err := s.TakeWorkspaceBundleForRestore(ctx, "legion", otherIssue, "pod-of-legions-spawn", at.Add(time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleNone || len(got.Bundle) != 0 {
		t.Fatalf("legion's pod of %s was answered %q (%v, %v), want nothing: the bundle is OTHER's", otherIssue, got.Bundle, take, err)
	}
	var attempted bool
	if err := s.Pool().QueryRow(ctx, `select attempt_pod_uid is not null or attempt_at is not null or attempt_outcome is not null
		from workspace_bundles where issue = $1 and release_id = 21`, otherIssue).Scan(&attempted); err != nil {
		t.Fatalf("read OTHER's bundle of %s: %v", otherIssue, err)
	}
	if attempted {
		t.Fatalf("legion's pod of %s spent the attempt of OTHER's bundle, want the row untouched", otherIssue)
	}

	seedRunningRoot(t, s, 1)
	own := runtime.WorkspaceBundle{Bundle: []byte("PACK of legion's pod"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, rootIssue, 22, savedRepo, own); err != nil {
		t.Fatalf("save the bundle of %s: %v", rootIssue, err)
	}
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, "legion", rootIssue, "pod-replacement", at.Add(time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(own.Bundle) || read.Release != 22 {
		t.Fatalf("legion's pod of %s was answered %q of release %d (%v, %v), want its own release's work", rootIssue, read.Bundle, read.Release, take, err)
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
	s := bundleStore(t)
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
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", releasedPodUID, created, noRenames)
		if err != nil || take != runtime.WorkspaceBundleNone {
			t.Fatalf("the pod the work came off, asking as created %s against the save at %s, was answered %q (%v, %v); want nothing to send",
				created, at, got.Bundle, take, err)
		}
	}
	// The pod the wake made after the save takes the whole of it, attempt and all.
	read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", at.Add(time.Minute), noRenames)
	if err != nil || take != runtime.WorkspaceBundleSend || string(read.Bundle) != string(saved.Bundle) || read.Head != saved.Head ||
		!slices.Equal(read.Tips, saved.Tips) || read.Release != 11 {
		t.Fatalf("the pod created after the save was answered %q at %s of release %d with tips %v (%v, %v), want the saved work whole",
			read.Bundle, read.Head, read.Release, read.Tips, take, err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", at.Add(time.Minute), noRenames); err != nil || take != runtime.WorkspaceBundleNone {
		t.Fatalf("the replacement pod's roles container restarted and was answered %v, %v; want the spent attempt to refuse it", take, err)
	}

	// A pod's creationTimestamp has one-second resolution, so a pod created inside the save's
	// own second cannot be ordered against it and is taken as the newer one: refusing it would
	// cost a legitimate restore, and the pod being released is refused by its uid anyway.
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-of-the-same-second", at.Truncate(time.Second), noRenames); err != nil || take != runtime.WorkspaceBundleSend {
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
	s := bundleStore(t)
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
	refused, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-skewed-replacement", at.Add(-time.Minute), noRenames)
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
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", pod.uid, pod.created, noRenames)
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

// A pod whose Sandbox owns the issue's kept volume, and whose Sandbox was made before the save,
// mounts the volume the work was taken off: the release kept that Sandbox and its volume
// (runtime.IssueReleaser). That pod takes no saved work. The store drops the bundle in one
// statement that reads none of the body, and it answers the release and the time of the bundle it
// dropped, so the runtime can name them. The row is then gone, so the bundle no longer holds the
// issue's pod (workflow.IdleHoldBundleUnrestored).
//
// The drop keeps the take's three rules. The pod the work came off drops nothing: the attempt
// stays for the pod that replaces it. A bundle whose one attempt is spent is not dropped: its row
// is the record of that attempt. A bundle of an issue that the asking project does not record is
// not dropped: another project's daemon saved it for that daemon's own pod
// (TestARestoreTakesNoBundleOfAnIssueAnotherProjectRecords). A kept Sandbox made after the save
// holds a new volume, which a reset made, so its bundle stays for the take. A Sandbox's
// creationTimestamp has one-second resolution, so the save is read to its own second, and a
// Sandbox made in that second keeps the bundle too: a drop deletes the only copy of the work, and
// a restore the launcher refuses costs a notice.
func TestABundleAKeptVolumeHoldsIsDroppedWithoutItsBody(t *testing.T) {
	ctx := context.Background()
	s := bundleStore(t)
	at := time.Date(2026, 11, 2, 9, 30, 0, 750_000_000, time.UTC)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	// Kept Sandboxes made after the save and in the save's own second: each holds a new volume.
	for _, created := range []time.Time{at.Add(time.Hour), at.Truncate(time.Second)} {
		if held, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-of-a-new-volume", created, noRenames); err != nil || dropped {
			t.Fatalf("a pod of a kept Sandbox made %s, against the save at %s, dropped release %d (%t, %v); want the bundle kept for its take",
				created, at, held.Release, dropped, err)
		}
	}
	// The pod the work came off, a role relaunched in the pod being released, on the volume the
	// save was taken from.
	if held, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", releasedPodUID, at.Add(-time.Hour), noRenames); err != nil || dropped {
		t.Fatalf("the pod the work came off dropped release %d (%t, %v); want the bundle left for the pod that replaces it", held.Release, dropped, err)
	}
	// A pod of another project's daemon, of a kept Sandbox made before the save: INFRA-945 is a
	// record of INFRA's, so the bundle is not legion's to drop.
	if held, dropped, err := s.DropHeldWorkspaceBundle(ctx, "legion", "INFRA-945", "pod-of-legions-spawn", at.Add(-time.Hour), noRenames); err != nil || dropped {
		t.Fatalf("legion's pod of INFRA-945 dropped release %d (%t, %v); want nothing: the bundle is INFRA's", held.Release, dropped, err)
	}
	// A drop that names no project, no pod, or no creation of the pod's Sandbox, has nothing to
	// read the rule by and is refused: a zero creation would read as older than every save.
	if _, dropped, err := s.DropHeldWorkspaceBundle(ctx, "", "INFRA-945", "pod-after-the-release", at.Add(-time.Hour), noRenames); err == nil || dropped {
		t.Fatalf("a drop that named no project was answered %t, %v; want it refused", dropped, err)
	}
	if _, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "", at.Add(-time.Hour), noRenames); err == nil || dropped {
		t.Fatalf("a drop that named no pod was answered %t, %v; want it refused", dropped, err)
	}
	if _, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-after-the-release", time.Time{}, noRenames); err == nil || dropped {
		t.Fatalf("a drop that named no creation of the Sandbox was answered %t, %v; want it refused", dropped, err)
	}
	if size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the drops that held nothing the store holds %d bytes (held=%t, %v), want the %d saved", size, held, err, len(saved.Bundle))
	}

	// The next pod of the kept Sandbox, made in the second before the save's own. None of the
	// asks above spent the attempt, or this drop would find no row.
	held, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-after-the-release", at.Truncate(time.Second).Add(-time.Second), noRenames)
	if err != nil || !dropped {
		t.Fatalf("a pod of a kept Sandbox made before the save dropped nothing (%v); want the bundle its volume holds dropped", err)
	}
	if held.Release != 11 || !held.At.Equal(at) {
		t.Fatalf("the drop answered release %d saved at %s, want release 11 saved at %s", held.Release, held.At, at)
	}
	if len(held.Bundle) != 0 {
		t.Fatalf("the drop read %d bytes of the bundle, want none", len(held.Bundle))
	}
	if _, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || held {
		t.Fatalf("after the drop the store holds the bundle = %t (%v), want it deleted", held, err)
	}
	if again, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-after-the-release", at.Add(-time.Hour), noRenames); err != nil || dropped {
		t.Fatalf("a second drop answered release %d (%t, %v); want nothing to drop", again.Release, dropped, err)
	}

	// A bundle whose one attempt is spent stays, whatever volume the next pod mounts.
	if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 12, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-of-a-reset", at.Add(time.Minute), noRenames); err != nil || take != runtime.WorkspaceBundleSend {
		t.Fatalf("the pod of a reset was answered %v (%v), want the saved work", take, err)
	}
	if spent, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-after-the-reset", at.Add(-time.Hour), noRenames); err != nil || dropped {
		t.Fatalf("a bundle whose attempt was spent was dropped as release %d (%t, %v); want it kept", spent.Release, dropped, err)
	}
	if size, held, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || !held || size != len(saved.Bundle) {
		t.Fatalf("after the spent attempt the store holds %d bytes (held=%t, %v), want the %d saved kept", size, held, err, len(saved.Bundle))
	}

	// A bundle of an issue another project records, and one of an issue no project records: the
	// pod of this project's kept Sandbox, made before each save, drops neither, and each row stays
	// for the pod of the daemon that saved it.
	if _, err := s.Pool().Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('OTHER-8', 'OTHER-8', 'OTHER', 'root', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatalf("record OTHER-8 as OTHER's issue: %v", err)
	}
	for _, issue := range []string{"OTHER-8", "NOBODY-3"} {
		others := runtime.WorkspaceBundle{Bundle: []byte("PACK of another daemon's pod"), Head: saved.Head, At: at, PodUID: "pod-other-released"}
		if err := s.PutWorkspaceBundle(ctx, issue, 21, savedRepo, others); err != nil {
			t.Fatalf("save the bundle of %s: %v", issue, err)
		}
		if held, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, issue, "pod-of-infras-spawn", at.Add(-time.Hour), noRenames); err != nil || dropped {
			t.Fatalf("INFRA's pod of %s dropped release %d (%t, %v); want nothing: INFRA does not record %s", issue, held.Release, dropped, err, issue)
		}
		if size, held, err := s.WorkspaceBundleSize(ctx, issue); err != nil || !held || size != len(others.Bundle) {
			t.Fatalf("after INFRA's drop the store holds %d bytes of %s (held=%t, %v), want the %d saved kept", size, issue, held, err, len(others.Bundle))
		}
	}
}

// A kept volume is no reason to drop a bundle of a repository the issue has left. The volume
// holds that work, but in the workspace of the old repository: a workspace's directory names its
// repository (workspace.Location), so the pod of the issue's repository now provisions a workspace
// of its own beside it, and the volume goes at a reset or at the issue's close. So the drop of a
// kept Sandbox made before the save deletes nothing there, and spends no attempt: the take that
// follows refuses the bundle for its repository and moves it to workspace_bundles_refused, where
// the work stays (TestABundleOfARepositoryTheIssueHasLeftIsRefusedAndKept).
//
// The drop decides by the take's own rule (ghrepo.Renames.Left). A configured rename's two names
// are one repository, and so is a record with no repository and a bundle saved before the column,
// since an empty name is never a statement that the issue has left anything: each of those drops
// the bundle as before.
func TestAKeptVolumeDropsNoBundleOfARepositoryTheIssueHasLeft(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	keptSince := at.Add(-time.Hour)
	renamed := ghrepo.MustRenames(ghrepo.MustRename(savedRepo, "acme/gadgets")).Left
	for _, tc := range []struct {
		name      string
		saved     string
		recorded  string
		left      func(saved, recorded string) bool
		dropped   bool
		afterTake runtime.WorkspaceBundleTake
	}{
		{name: "the record names another repository", saved: savedRepo, recorded: "zeta/tools", left: noRenames,
			afterTake: runtime.WorkspaceBundleRefusedRepository},
		{name: "the record names the rename's other name", saved: savedRepo, recorded: "acme/gadgets", left: renamed, dropped: true},
		{name: "the record names no repository", saved: savedRepo, recorded: "", left: noRenames, dropped: true},
		{name: "the bundle was saved before the column", saved: "", recorded: "zeta/tools", left: noRenames, dropped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			recordIssueOn(t, s, "INFRA-945", tc.recorded)
			saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
				Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
			if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, tc.saved, saved); err != nil {
				t.Fatal(err)
			}
			held, dropped, err := s.DropHeldWorkspaceBundle(ctx, bundleProject, "INFRA-945", "pod-after-the-release", keptSince, tc.left)
			if err != nil || dropped != tc.dropped {
				t.Fatalf("a pod of a kept Sandbox made before the save, of an issue on %q with a bundle of %q, dropped %t (%v); want %t",
					tc.recorded, tc.saved, dropped, err, tc.dropped)
			}
			if tc.dropped {
				if held.Release != 11 {
					t.Fatalf("the drop answered release %d, want 11", held.Release)
				}
				if _, kept, err := s.WorkspaceBundleSize(ctx, "INFRA-945"); err != nil || kept {
					t.Fatalf("after the drop the store holds the bundle = %t (%v), want it deleted", kept, err)
				}
				return
			}
			// Nothing was dropped and no attempt was spent, so the take of the same pod is the
			// bundle's one attempt, and it refuses the bundle for its repository.
			if pod, outcome := bundleAttempt(t, s, "INFRA-945"); pod != "" || outcome != "" {
				t.Fatalf("the drop left the attempt %q spent on %q, want no attempt", outcome, pod)
			}
			if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-after-the-release", at.Add(time.Minute), tc.left); err != nil || take != tc.afterTake {
				t.Fatalf("the take after the drop was answered %v (%v), want %v", take, err, tc.afterTake)
			}
			kept := refusedBundles(t, s, "INFRA-945")
			if len(kept) != 1 || kept[0].release != 11 || kept[0].repo != tc.saved || !bytes.Equal(kept[0].bundle, saved.Bundle) {
				t.Fatalf("workspace_bundles_refused holds %+v, want the bundle of release 11 of %s whole", kept, tc.saved)
			}
		})
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

// A bundle refusal is one row per issue that the idle view reads, and it queues the rows that
// tell the worker only for the first refusal at a head: a second refusal of the same work tells
// nobody. A save that stores the work, and a delete of the issue's bundle, end it, so the next
// refusal tells the worker again.
func TestABundleRefusalTellsOnceAndEndsWithTheNextSaveOrDelete(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-945"
	tell := []IdleReleaseRow{{Kind: "role_message", Payload: []byte(`{"role":"legion-infra-infra-945-implementer","body":"push"}`)}}
	refuse := func() bool {
		t.Helper()
		told, err := s.RecordBundleRefusal(ctx, "infra", issue, tell)
		if err != nil {
			t.Fatal(err)
		}
		return told
	}
	refused := func() bool {
		t.Helper()
		var found bool
		if err := s.Tx(ctx, func(tx pgx.Tx) error {
			r, _, err := workspaceBundleRefusal(ctx, tx, issue)
			found = r != nil
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}
	messages := func() int {
		t.Helper()
		var n int
		if err := s.Pool().QueryRow(ctx, `select count(*) from outbox where issue = $1 and kind = 'role_message'`,
			issue).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if refused() {
		t.Fatal("an issue never refused reads a refusal")
	}
	if !refuse() || refuse() {
		t.Fatal("two refusals of the same work at the same head did not tell the worker exactly once")
	}
	if got := messages(); got != 1 {
		t.Fatalf("messages queued = %d, want 1", got)
	}
	if !refused() {
		t.Fatal("the refusal is not read back")
	}

	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, issue, 11, savedRepo, saved); err != nil {
		t.Fatal(err)
	}
	if refused() {
		t.Fatal("a save that stored the work left the refusal")
	}
	if !refuse() {
		t.Fatal("the first refusal after a stored save did not tell the worker")
	}
	if err := s.DeleteWorkspaceBundle(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if refused() {
		t.Fatal("a delete of the issue's bundle left the refusal")
	}
	if got := messages(); got != 2 {
		t.Fatalf("messages queued = %d, want 2", got)
	}
}

// recordIssueOn records the issue on repo under bundleProject's project, as the daemon's record
// holds it: the two columns the take reads of the issues table, with the rest of the row as a
// running root's.
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
	refused, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-of-the-new-repository", at.Add(time.Minute), noRenames)
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
	if got, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-after-an-eviction", at.Add(time.Hour), noRenames); err != nil || take != runtime.WorkspaceBundleNone {
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
		if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-of-zeta-tools", at.Add(time.Minute), noRenames); err != nil || take != runtime.WorkspaceBundleRefusedRepository {
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
		got, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-of-acme-widgets-again", at.Add(2*time.Hour), noRenames)
		if err != nil || take != runtime.WorkspaceBundleRefusedRepository || len(got.Bundle) != 0 {
			t.Fatalf("the pod of acme/widgets again was answered %v with %d bytes (%v), want the work of zeta/tools refused and nothing sent", take, len(got.Bundle), err)
		}
		if got, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-after-it", at.Add(3*time.Hour), noRenames); err != nil || take != runtime.WorkspaceBundleNone {
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

			read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", at.Add(time.Minute), noRenames)
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

// Whether the issue has left the bundle's repository is the daemon's one rule
// (ghrepo.Renames.Left), the rule a pod built from a repository is read by too, and not a
// comparison of the two names alone. A configured rename's two names are one repository: a
// bundle saved under the old name, of an issue whose record names the new one, is the work of
// the clone the pod holds and is sent. A record with no repository names nothing, which is an
// unknown and refuses nothing either. The same save with no rename configured is refused, which
// is what shows that the rename is what sent it.
func TestABundleOfARenamedRepositoryIsSent(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	renamed := ghrepo.MustRenames(ghrepo.MustRename(savedRepo, "acme/gadgets")).Left
	for _, tc := range []struct {
		name     string
		recorded string
		left     func(saved, recorded string) bool
		want     runtime.WorkspaceBundleTake
	}{
		{name: "the record names the rename's other name", recorded: "acme/gadgets", left: renamed, want: runtime.WorkspaceBundleSend},
		{name: "the record names no repository", recorded: "", left: noRenames, want: runtime.WorkspaceBundleSend},
		{name: "no rename is configured", recorded: "acme/gadgets", left: noRenames, want: runtime.WorkspaceBundleRefusedRepository},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
				Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
			if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
				t.Fatal(err)
			}
			recordIssueOn(t, s, "INFRA-945", tc.recorded)

			read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", at.Add(time.Minute), tc.left)
			if err != nil || take != tc.want {
				t.Fatalf("the replacement pod was answered %v (%v), want %v", take, err, tc.want)
			}
			if tc.want != runtime.WorkspaceBundleSend {
				if kept := refusedBundles(t, s, "INFRA-945"); len(kept) != 1 || !bytes.Equal(kept[0].bundle, saved.Bundle) {
					t.Fatalf("the store keeps %d refused bundles of the issue, want the one saved, whole", len(kept))
				}
				return
			}
			if string(read.Bundle) != string(saved.Bundle) || read.Repo != savedRepo {
				t.Fatalf("the replacement pod was answered %q of %q, want the saved work of %s", read.Bundle, read.Repo, savedRepo)
			}
			if pod, outcome := bundleAttempt(t, s, "INFRA-945"); pod != "pod-replacement" || outcome != "sent" {
				t.Fatalf("the row records the attempt as %q by %s, want sent to the pod that asked", outcome, pod)
			}
		})
	}
}

// The one-time rename pass writes a configured pair's `to` over its `from` on the saved bundle as
// on the record (record.RenameIssueRepository, workspace_bundles.repo among its tables), so a
// bundle saved under the old name is still the work of the issue's repository once the operator
// removes the pair from `repository_renames`: the take then compares the names with no pair at all
// (noRenames), and the bundle is sent. Control: a pass that wrote the record alone and left the
// bundle on the old name has the same take refuse the work as of a repository the issue left.
func TestABundleSavedBeforeTheRenamePassIsSentOnceThePairIsRemoved(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	const to = "acme/gadgets"
	for _, tc := range []struct {
		name   string
		rename func(t *testing.T, s *Store)
		want   runtime.WorkspaceBundleTake
	}{
		{name: "the pass", want: runtime.WorkspaceBundleSend, rename: func(t *testing.T, s *Store) {
			if err := s.Tx(context.Background(), func(tx pgx.Tx) error {
				_, err := record.RenameIssueRepository(context.Background(), tx, "INFRA-945", savedRepo, to)
				return err
			}); err != nil {
				t.Fatalf("the rename pass's write of INFRA-945: %v", err)
			}
		}},
		{name: "a pass that left the bundle", want: runtime.WorkspaceBundleRefusedRepository, rename: func(t *testing.T, s *Store) {
			recordIssueOn(t, s, "INFRA-945", to)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := migratedStore(t)
			saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the released work"),
				Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: releasedPodUID}
			if err := s.PutWorkspaceBundle(ctx, "INFRA-945", 11, savedRepo, saved); err != nil {
				t.Fatal(err)
			}
			recordIssueOn(t, s, "INFRA-945", savedRepo)
			tc.rename(t, s)

			read, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", at.Add(time.Minute), noRenames)
			if err != nil || take != tc.want {
				t.Fatalf("the pod after the pair's removal was answered %v (%v), want %v", take, err, tc.want)
			}
			if tc.want == runtime.WorkspaceBundleSend && (string(read.Bundle) != string(saved.Bundle) || read.Repo != to) {
				t.Fatalf("the pod was answered %q of %q, want the saved work of %s under the pair's new name", read.Bundle, read.Repo, savedRepo)
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
			if _, take, err := s.TakeWorkspaceBundleForRestore(ctx, bundleProject, "INFRA-945", "pod-replacement", tc.created, noRenames); err != nil || take != tc.want {
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
