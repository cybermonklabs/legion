package store

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// runningPodUID is the pod every copy of these tests was taken off: the pod whose roles are still
// working, which the row records (workspace_running_copies.source_pod_uid) and the take refuses by
// name (TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttemptReadToItsSecond).
const runningPodUID = "pod-still-running"

// copyRow is what one of the two tables holds of an issue's copy, read off the row itself without
// its bytes: the store serves no such read, since nothing in the daemon decides on these facts but
// the restore's own transaction, and a test asks what that transaction left.
type copyRow struct {
	held       bool
	size       int
	at         time.Time
	attemptPod string
	outcome    string
}

// heldCopyRow reads issue's row in the table copies names, held false when there is none.
func heldCopyRow(t *testing.T, s *Store, copies copyTable, issue string) copyRow {
	t.Helper()
	row := copyRow{held: true}
	err := s.Pool().QueryRow(context.Background(),
		`select size_bytes, at, coalesce(attempt_pod_uid, ''), coalesce(attempt_outcome, '') from `+copies.table+` where issue = $1`, issue).
		Scan(&row.size, &row.at, &row.attemptPod, &row.outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return copyRow{}
	}
	if err != nil {
		t.Fatalf("read the %s of %s: %v", copies.kind, issue, err)
	}
	return row
}

// storedCopy is the row behind an issue's running copy, read whole. The store answers its facts
// without its bytes and its bytes only to a pod it is sent to, which is the point of both methods;
// what the save rewrote and what it left is a question about the row itself, so these tests read
// it directly.
type storedCopy struct {
	bundle     []byte
	size       int
	restoreID  int64
	at         time.Time
	sourcePod  string
	attemptPod string
}

// storedRunningCopy reads issue's row, and fails the test when the issue holds none.
func storedRunningCopy(t *testing.T, s *Store, issue string) storedCopy {
	t.Helper()
	var row storedCopy
	if err := s.Pool().QueryRow(context.Background(),
		`select bundle, size_bytes, restore_id, at, source_pod_uid, coalesce(attempt_pod_uid, '')
			from workspace_running_copies where issue = $1`, issue).
		Scan(&row.bundle, &row.size, &row.restoreID, &row.at, &row.sourcePod, &row.attemptPod); err != nil {
		t.Fatalf("read the stored running copy of %s: %v", issue, err)
	}
	return row
}

// runningCopyRows is how many rows the table holds, which is what says a cadence taking a copy
// every few minutes grows nothing.
func runningCopyRows(t *testing.T, s *Store) int {
	t.Helper()
	var rows int
	if err := s.Pool().QueryRow(context.Background(), `select count(*) from workspace_running_copies`).Scan(&rows); err != nil {
		t.Fatalf("count the stored running copies: %v", err)
	}
	return rows
}

// A copy taken while the pod runs round-trips whole — its bytes, the commit the working copy sat
// on, the heads it carries, the bookmarks that pointed into it, the pod its work came off and the
// restore id the table gave it — one issue keeps one row however many copies its cadence takes,
// and the confirmation of a restore keeps the copy its id names, and no other, as the restoring
// pod's own.
func TestTheRunningCopyOfAWorkspaceBundleRoundTripsAndLeavesOneRowPerIssue(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	// Every pod answered here was created after the copy it is answered, which is the only kind
	// a copy is sent to at all
	// (TestEitherSavedCopyOfAWorkspaceBundleHasOneRestoreAttemptReadToItsSecond).
	replacement := at.Add(time.Minute)
	taken := runtime.WorkspaceBundle{
		Bundle:    bytes.Repeat([]byte{0, 1, 2, 3, 0xff}, 1024),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f", "f2f134541b30dfa5160a5d1ce5e17b76c967c661"},
		Bookmarks: map[string]string{"legion/INFRA-1117": "dffe88ae2f793e13436cef3921adc988433fbd4e", "side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		At:        at,
		PodUID:    runningPodUID,
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-0", replacement); err != nil || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("an issue whose cadence has taken nothing reads %v, %v; want no copy", restore.Running.Take, err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); row.held {
		t.Fatalf("an issue whose cadence has taken nothing holds a copy: %+v", row)
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", taken); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); !row.held || row.size != taken.Size() || !row.at.Equal(at) {
		t.Fatalf("the copy reads %+v, want the %d bytes saved at %s", row, taken.Size(), at)
	}
	stored := storedRunningCopy(t, s, "INFRA-1117")
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-1", replacement)
	read := restore.Running.Bundle
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("read the running copy: %v, %v", restore.Running.Take, err)
	}
	if !bytes.Equal(read.Bundle, taken.Bundle) || read.Size() != taken.Size() {
		t.Fatalf("the copy read back is %d bytes, want the %d taken", read.Size(), taken.Size())
	}
	if read.Head != taken.Head || !slices.Equal(read.Tips, taken.Tips) || !maps.Equal(read.Bookmarks, taken.Bookmarks) {
		t.Fatalf("the copy read back is %+v, want %+v", read, taken)
	}
	if !read.At.Equal(at) {
		t.Fatalf("the copy was taken at %s, want %s", read.At, at)
	}
	if read.PodUID != runningPodUID {
		t.Fatalf("the copy reads back as the work of pod %q, want the %q it was taken off", read.PodUID, runningPodUID)
	}
	// The restore id is the row's own, and the restore sends it to the launcher as the release
	// of every slice: it says which of the issue's two copies a report confirms.
	if read.Release != stored.restoreID {
		t.Fatalf("the copy reads back under restore id %d, want the %d the row holds", read.Release, stored.restoreID)
	}

	// The cadence goes on taking copies of the same issue, each replacing the one before it: a
	// copy of an older minute must never be restored in place of a newer one, and a table that
	// grew a row every five minutes would hold the whole of each pod's day.
	var ids []int64
	for i, work := range []string{"PACK the next five minutes", "PACK the five after that"} {
		next := runtime.WorkspaceBundle{Bundle: []byte(work), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e" + string(rune('0'+i)),
			At: at.Add(time.Duration(i+1) * 5 * time.Minute), PodUID: runningPodUID}
		if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", next); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, storedRunningCopy(t, s, "INFRA-1117").restoreID)
	}
	if rows := runningCopyRows(t, s); rows != 1 {
		t.Fatalf("three copies of one issue left %d rows, want the one row the issue has", rows)
	}
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-2", at.Add(time.Hour))
	newest := restore.Running.Bundle
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend || string(newest.Bundle) != "PACK the five after that" {
		t.Fatalf("the pod after the cadence was answered %q (%v, %v), want the newest copy", newest.Bundle, restore.Running.Take, err)
	}
	if len(newest.Tips) != 0 || len(newest.Bookmarks) != 0 {
		t.Fatalf("the newest copy reads tips %v and bookmarks %v, want neither", newest.Tips, newest.Bookmarks)
	}
	if ids[0] == stored.restoreID || ids[1] == ids[0] || newest.Release != ids[1] {
		t.Fatalf("the copies read back under ids %d, %v and %d, want each save to take an id of its own",
			stored.restoreID, ids, newest.Release)
	}

	// A launcher holds its report until some connection carries it, so a confirmation of a copy
	// the cadence has already replaced reaches the daemon with nothing but its id: it changes
	// nothing, and the newer work stays as it was for the issue's next pod. Another issue's copy
	// is no more its business.
	other := runtime.WorkspaceBundle{Bundle: []byte("PACK another issue's work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: at, PodUID: "pod-of-another-issue"}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", other); err != nil {
		t.Fatal(err)
	}
	current := storedRunningCopy(t, s, "INFRA-1117")
	for _, id := range []int64{stored.restoreID, ids[0]} {
		if err := s.KeepRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", "pod-1", id); err != nil {
			t.Fatal(err)
		}
	}
	if row := storedRunningCopy(t, s, "INFRA-1117"); row.restoreID != current.restoreID || row.sourcePod != current.sourcePod ||
		row.attemptPod != current.attemptPod || !row.at.Equal(current.at) {
		t.Fatalf("a report of a copy the cadence replaced left %+v, want the newer copy %+v as it was", row, current)
	}
	if row := storedRunningCopy(t, s, "INFRA-2760"); row.sourcePod != other.PodUID {
		t.Fatalf("a report of one issue's copy rebound another issue's to pod %q", row.sourcePod)
	}
	// The report of the copy pod-2 was sent keeps it as pod-2's own: the same work, named off
	// that pod, under an id of its own, stamped at the confirmation, with no attempt spent.
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", "pod-2", ids[1]); err != nil {
		t.Fatal(err)
	}
	kept := storedRunningCopy(t, s, "INFRA-1117")
	if !bytes.Equal(kept.bundle, current.bundle) || kept.sourcePod != "pod-2" || kept.attemptPod != "" ||
		kept.restoreID == ids[1] || !kept.at.After(current.at) {
		t.Fatalf("the confirmed copy reads %+v, want %q kept off pod-2 under a new id, stamped after %s, with no attempt",
			kept, current.bundle, current.at)
	}
	// A report the daemon read twice names an id the row no longer holds, and changes nothing.
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", "pod-2", ids[1]); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
	if row := storedRunningCopy(t, s, "INFRA-1117"); row.restoreID != kept.restoreID || !row.at.Equal(kept.at) {
		t.Fatalf("a repeated confirmation moved the kept copy to %+v, want %+v", row, kept)
	}
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", "", kept.restoreID); err == nil {
		t.Fatal("a confirmation naming no pod was accepted")
	}
	// pod-2 is answered nothing of its own copy, and the pod after it is sent the work.
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-2", at.Add(time.Hour)); err != nil || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("pod-2 asking again was answered %v (%v), want nothing", restore.Running.Take, err)
	}
	restore, err = s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-3", time.Now().UTC().Add(time.Hour))
	if err != nil || restore.Running.Take != runtime.WorkspaceBundleSend || !bytes.Equal(restore.Running.Bundle.Bundle, kept.bundle) ||
		restore.Running.Bundle.Release != kept.restoreID {
		t.Fatalf("the pod after pod-2 was answered %v of %q under %d (%v), want the kept copy sent under %d",
			restore.Running.Take, restore.Running.Bundle.Bundle, restore.Running.Bundle.Release, err, kept.restoreID)
	}
	// The cadence's own delete, of a workspace that bundled to nothing, drops a kept copy as it
	// drops any other, and a delete of an issue holding nothing finishes.
	for _, issue := range []string{"INFRA-1117", "INFRA-2760"} {
		if err := s.DeleteRunningWorkspaceCopy(ctx, issue); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteRunningWorkspaceCopy(ctx, "INFRA-2760"); err != nil {
		t.Fatalf("a repeated delete: %v", err)
	}
	if rows := runningCopyRows(t, s); rows != 0 {
		t.Fatalf("the table holds %d rows after both copies went, want none", rows)
	}

	if err := s.PutRunningWorkspaceCopy(ctx, "", taken); err == nil {
		t.Fatal("a copy saved under no issue was accepted")
	}
	// A copy that cannot say which pod the work is on is refused: the row could not then tell
	// the pod still working from the pod that replaces it, and the take refuses by that uid.
	noPod := taken
	noPod.PodUID = ""
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", noPod); err == nil {
		t.Fatal("a copy saved without the pod its work came off was accepted")
	}
	// A copy of no bytes restores nothing, and a row holding one would be answered to a pod in
	// place of the release bundle it should have had.
	empty := taken
	empty.Bundle = nil
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", empty); err == nil {
		t.Fatal("a copy carrying no work was accepted")
	}
}

// A restore report is read off the session of one issue's launcher, and the restore id is all it
// says of the copy it restored. A report whose id names another issue's running copy — a launcher
// that is wrong, or one a role of its pod has taken over — changes nothing: that copy is the other
// issue's work, and the pod that restores it is that issue's next one. The same id reported by the
// launcher of the copy's own issue keeps it as that launcher's pod's own.
func TestARestoreReportNamingAnotherIssuesRunningCopyChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	other := runtime.WorkspaceBundle{Bundle: []byte("PACK another issue's work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: at, PodUID: "pod-of-another-issue"}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", other); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-2760", "pod-of-INFRA-2760", at.Add(time.Minute)); err != nil ||
		restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the next pod of INFRA-2760 was answered %v (%v), want its copy sent", restore.Running.Take, err)
	}
	stored := storedRunningCopy(t, s, "INFRA-2760")
	if err := s.KeepRestoredWorkspaceCopy(ctx, "INFRA-1117", "pod-of-INFRA-1117", stored.restoreID); err != nil {
		t.Fatal(err)
	}
	if row := storedRunningCopy(t, s, "INFRA-2760"); row.restoreID != stored.restoreID || row.sourcePod != other.PodUID || row.size != other.Size() {
		t.Fatalf("a report of INFRA-1117's launcher naming INFRA-2760's copy %d left %+v, want that copy as it was", stored.restoreID, row)
	}
	if err := s.KeepRestoredWorkspaceCopy(ctx, "INFRA-2760", "pod-of-INFRA-2760", stored.restoreID); err != nil {
		t.Fatal(err)
	}
	if row := storedRunningCopy(t, s, "INFRA-2760"); row.sourcePod != "pod-of-INFRA-2760" || row.restoreID == stored.restoreID {
		t.Fatalf("the report of the copy's own issue left %+v, want it kept as the reporting pod's copy under a new id", row)
	}
}

// A confirmation keeps only a running copy whose one attempt was a send to the confirming pod. Any
// other launcher of the issue reports work its pod does not hold — the pod the copy was refused
// to, or one it was never sent to — and such a report keeps nothing: the row keeps its source, its
// restore id, its time and its attempt, so it is never named off a pod whose disk lacks the work.
func TestARestoreReportFromAPodNotSentTheRunningCopyKeepsNothing(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-1117"
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	unchanged := func(why string, want storedCopy) {
		t.Helper()
		if row := storedRunningCopy(t, s, issue); row.restoreID != want.restoreID || row.sourcePod != want.sourcePod ||
			row.attemptPod != want.attemptPod || !row.at.Equal(want.at) {
			t.Fatalf("after %s the running copy reads %+v, want it as it was: %+v", why, row, want)
		}
	}
	if err := s.PutRunningWorkspaceCopy(ctx, issue, runtime.WorkspaceBundle{Bundle: []byte("PACK the first copy"),
		Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661", At: at, PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	// A push the daemon recorded after the copy refuses it to the pod that asks, which spends its
	// attempt.
	recordPush(t, s, issue, "c0", "c1", at.Add(time.Minute))
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-refused-the-copy", at.Add(time.Hour)); err != nil ||
		restore.Running.Take != runtime.WorkspaceBundleRefusedPushed {
		t.Fatalf("the pod after the push was answered %v (%v), want the copy refused", restore.Running.Take, err)
	}
	refused := storedRunningCopy(t, s, issue)
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, issue, "pod-refused-the-copy", refused.restoreID); err != nil {
		t.Fatal(err)
	}
	unchanged("a report of the pod the copy was refused to", refused)

	// The pod's next copy is taken after the push and sent to pod-two.
	if err := s.PutRunningWorkspaceCopy(ctx, issue, runtime.WorkspaceBundle{Bundle: []byte("PACK the copy after the push"),
		Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9", At: at.Add(2 * time.Minute), PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	if restore, err := s.TakeWorkspaceCopyForRestore(ctx, issue, "pod-two", at.Add(time.Hour)); err != nil ||
		restore.Running.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("pod-two was answered %v (%v), want the copy after the push sent", restore.Running.Take, err)
	}
	sent := storedRunningCopy(t, s, issue)
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, issue, "another-pod-of-the-issue", sent.restoreID); err != nil {
		t.Fatal(err)
	}
	unchanged("a report of a pod the copy was not sent to", sent)

	// The report of the pod it was sent to keeps it as that pod's own.
	if err := s.KeepRestoredRunningWorkspaceCopy(ctx, issue, "pod-two", sent.restoreID); err != nil {
		t.Fatal(err)
	}
	if row := storedRunningCopy(t, s, issue); row.sourcePod != "pod-two" || row.restoreID == sent.restoreID || row.attemptPod != "" {
		t.Fatalf("after pod-two's report the running copy reads %+v, want it kept off pod-two under a new id with no attempt", row)
	}
}

// A running copy holds no pod. The work it carries is on the disk it was taken off, so an issue
// that holds one is still released when it is idle: the idle rule reads the release bundle's table
// alone (workspaceBundleUnrestored, workflow.IdleHoldBundleUnrestored), which is the whole reason
// the copy lives in a table of its own. Were it a row of workspace_bundles, every working issue
// would hold its pod for ever, since no pod restores a copy while the pod it came off still runs.
func TestARunningCopyLeavesTheIssueReleasableWhileItIsIdle(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	now := time.Now().UTC()

	if err := st.PutRunningWorkspaceCopy(ctx, rootIssue, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work of a running pod"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: now, PodUID: runningPodUID}); err != nil {
		t.Fatalf("save a running copy of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); view.BundleUnrestored {
		t.Fatalf("view = %+v, want an issue holding a running copy to be releasable when it is idle", view)
	}

	// The control: the flag is read, and it is the release bundle that sets it. An idle release
	// of this pod stores that bundle, and until a pod puts it back the issue is held. The save
	// drops the running copy as it stores the bundle (PutWorkspaceBundle); the pod that replaces
	// the released one is sent the bundle, and a pod that came back beside it works on and is
	// copied.
	if err := st.PutWorkspaceBundle(ctx, rootIssue, 7, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work of the release"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		At: now, PodUID: releasedPodUID}); err != nil {
		t.Fatalf("save a release bundle of %s: %v", rootIssue, err)
	}
	if restore, err := st.TakeWorkspaceCopyForRestore(ctx, rootIssue, "pod-that-restored-it", now.Add(time.Hour)); err != nil ||
		restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend {
		t.Fatalf("the pod after the release was answered %v (%v), want the bundle sent", restore.ReleaseBundle.Take, err)
	}
	if err := st.PutRunningWorkspaceCopy(ctx, rootIssue, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work after the release"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: now.Add(time.Minute), PodUID: runningPodUID}); err != nil {
		t.Fatalf("save a running copy of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); !view.BundleUnrestored {
		t.Fatalf("view = %+v, want the unrestored release bundle to hold the pod", view)
	}

	// The restore of that release bundle lifts the hold, and the running copy is still there as
	// it was: it is the newer work, taken off another pod after the bundle, so the confirmation
	// keeps it in place of the bundle's (KeepRestoredWorkspaceBundle).
	if err := st.KeepRestoredWorkspaceBundle(ctx, rootIssue, "pod-that-restored-it", 7); err != nil {
		t.Fatal(err)
	}
	if view := idleViewOf(t, st, rootIssue); view.BundleUnrestored {
		t.Fatalf("view = %+v, want the confirmed release bundle gone from it", view)
	}
	if row := storedRunningCopy(t, st, rootIssue); string(row.bundle) != "PACK the work after the release" || row.sourcePod != runningPodUID {
		t.Fatalf("after the release bundle's confirmation the running copy holds %q off %q, want the newer copy as it was", row.bundle, row.sourcePod)
	}
}

// Once an idle release has saved its bundle, that bundle is the last copy of the pod it came off,
// and no running copy taken off that pod afterwards is stored. The release's save runs before its
// role stops, so the pod's roles are live after it, and a push the daemon records in that window
// asks them for a copy: stored, that copy would be the later of the two by the clock, the next
// pod would be offered it in the bundle's place, and the bundle's row would hold the issue's pod
// from then on. The write refuses it with a refusal of its own, which the daemon tells apart from
// a save that failed, and the bundle is what the next pod is sent. A copy off the next pod is
// stored once that pod has asked, and a late copy off the released pod is still refused then, so
// it cannot take the place of the next pod's own work.
func TestARunningCopyOffThePodAReleaseBundleCameOffIsRefusedAndOneOffTheNextPodIsStored(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	saved := runtime.WorkspaceBundle{Bundle: []byte("PACK the work the release saved"), Head: "c1",
		Tips: []string{"c1"}, At: at, PodUID: releasedPodUID}
	if err := s.PutWorkspaceBundle(ctx, "INFRA-1117", 7, saved); err != nil {
		t.Fatal(err)
	}

	// The copy a push in the release's window asks of the pod's roles, two minutes after the save.
	window := runtime.WorkspaceBundle{Bundle: []byte("PACK the released pod's work after the save"), Head: "c1",
		Tips: []string{"c1"}, At: at.Add(2 * time.Minute), PodUID: releasedPodUID}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", window); !errors.Is(err, ErrReleaseBundleSaved) {
		t.Fatalf("a copy off the pod the release bundle came off was answered %v, want %v", err, ErrReleaseBundleSaved)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); row.held {
		t.Fatalf("the refused copy was stored: %+v", row)
	}
	// The read the cadence makes before it asks a shim answers the same of that pod and of no
	// other, nor of the pod of another issue.
	for _, c := range []struct {
		issue, pod string
		want       bool
	}{
		{"INFRA-1117", releasedPodUID, true},
		{"INFRA-1117", "pod-that-replaces-it", false},
		{"INFRA-1117", "", false},
		{"INFRA-2760", releasedPodUID, false},
	} {
		if got, err := s.ReleaseBundleSavedOff(ctx, c.issue, c.pod); err != nil || got != c.want {
			t.Fatalf("whether the release bundle of %s came off pod %q read %t (%v), want %t", c.issue, c.pod, got, err, c.want)
		}
	}

	// The pod that replaces the released one asks, and is sent the bundle: the refused copy is
	// nowhere to be offered in its place.
	restore, err := s.TakeWorkspaceCopyForRestore(ctx, "INFRA-1117", "pod-that-replaces-it", at.Add(20*time.Minute))
	if err != nil || restore.ReleaseBundle.Take != runtime.WorkspaceBundleSend || restore.Running.Take != runtime.WorkspaceBundleNone {
		t.Fatalf("the next pod was answered %+v (%v), want the release bundle sent and no running copy", restore, err)
	}
	// Its own copies are stored.
	next := runtime.WorkspaceBundle{Bundle: []byte("PACK the next pod's work"), Head: "c4",
		Tips: []string{"c4"}, At: at.Add(30 * time.Minute), PodUID: "pod-that-replaces-it"}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", next); err != nil {
		t.Fatalf("a copy off the pod that replaced the released one was answered %v, want it stored", err)
	}
	// A copy off the released pod that reaches the store only now is refused all the same, and
	// leaves the next pod's copy where it is.
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", window); !errors.Is(err, ErrReleaseBundleSaved) {
		t.Fatalf("a late copy off the released pod was answered %v, want %v", err, ErrReleaseBundleSaved)
	}
	if row := storedRunningCopy(t, s, "INFRA-1117"); row.sourcePod != next.PodUID || !bytes.Equal(row.bundle, next.Bundle) {
		t.Fatalf("the running copy is %q off %q, want the next pod's %q", row.bundle, row.sourcePod, next.Bundle)
	}
}

// A copy whose content equals the stored row's moves the time alone. A turn that read the
// workspace and wrote nothing in it ends with the work the copy before it already holds, and
// rewriting a multi-megabyte row every five minutes to store those bytes again is waste. The bytes
// the second save carries differ from the stored ones here — a git bundle is built afresh each
// time — so reading the first save's bytes back is what says the row was not rewritten.
func TestASaveOfTheSameRunningCopyMovesOnlyItsTimeAndRewritesNoBundleBytes(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	at := time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC)
	taken := runtime.WorkspaceBundle{
		Bundle:    []byte("PACK the work as the first copy carried it"),
		Head:      "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips:      []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"},
		Bookmarks: map[string]string{"legion/INFRA-1117": "dffe88ae2f793e13436cef3921adc988433fbd4e"},
		At:        at,
		PodUID:    runningPodUID,
	}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", taken); err != nil {
		t.Fatal(err)
	}
	stored := storedRunningCopy(t, s, "INFRA-1117")

	// Five minutes on, the same workspace: the same head, the same tips, the same bookmarks and
	// a bundle of the same length, whose bytes are not the stored ones, since a git bundle of
	// one workspace is built afresh and is not byte-for-byte the bundle before it.
	same := taken
	same.At = at.Add(5 * time.Minute)
	same.Bundle = bytes.Repeat([]byte{0xa5}, len(taken.Bundle))
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", same); err != nil {
		t.Fatal(err)
	}
	unchanged := storedRunningCopy(t, s, "INFRA-1117")
	if !bytes.Equal(unchanged.bundle, taken.Bundle) {
		t.Fatalf("the row holds %q, want the %q the first copy wrote: a save of the same content rewrites no bytes", unchanged.bundle, taken.Bundle)
	}
	if unchanged.size != stored.size || unchanged.restoreID != stored.restoreID || unchanged.sourcePod != stored.sourcePod {
		t.Fatalf("the row reads %d bytes under id %d off pod %q, want the stored %d, %d and %q untouched",
			unchanged.size, unchanged.restoreID, unchanged.sourcePod, stored.size, stored.restoreID, stored.sourcePod)
	}
	if !unchanged.at.Equal(same.At) {
		t.Fatalf("the copy reads as taken at %s, want the %s of the save that found it unchanged", unchanged.at, same.At)
	}

	// Work the roles did since: the head moves, so the row is written whole and takes an id of
	// its own, since it is another copy and a report of the one it replaced deletes nothing.
	moved := taken
	moved.At = at.Add(10 * time.Minute)
	moved.Head = "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9"
	moved.Bundle = []byte("PACK the work of ten minutes later")
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", moved); err != nil {
		t.Fatal(err)
	}
	rewritten := storedRunningCopy(t, s, "INFRA-1117")
	if !bytes.Equal(rewritten.bundle, moved.Bundle) || rewritten.size != moved.Size() || !rewritten.at.Equal(moved.At) {
		t.Fatalf("the row holds %q of %d bytes taken at %s, want the copy whose head moved", rewritten.bundle, rewritten.size, rewritten.at)
	}
	if rewritten.restoreID == stored.restoreID {
		t.Fatalf("a copy of other work kept the id %d of the copy it replaced", rewritten.restoreID)
	}

	// A workspace that comes back unchanged on the pod that replaced the first is not the same
	// copy: the row's source pod is what the take refuses by, so the row is written whole and
	// names the pod the work is on now.
	elsewhere := moved
	elsewhere.At = at.Add(15 * time.Minute)
	elsewhere.PodUID = "pod-replacement"
	elsewhere.Bundle = []byte("PACK the work of the replacement pod")
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-1117", elsewhere); err != nil {
		t.Fatal(err)
	}
	onTheNewPod := storedRunningCopy(t, s, "INFRA-1117")
	if onTheNewPod.sourcePod != "pod-replacement" || !bytes.Equal(onTheNewPod.bundle, elsewhere.Bundle) ||
		onTheNewPod.restoreID == rewritten.restoreID {
		t.Fatalf("a copy of the same work off pod %q reads as %q off %q under id %d, want the row written whole for the new pod",
			elsewhere.PodUID, onTheNewPod.bundle, onTheNewPod.sourcePod, onTheNewPod.restoreID)
	}
}

// The ids this table issues are above every id issue_releases holds, and the schema keeps them so.
// A launcher holds its restore report until some connection carries it, so one can reach a daemon
// that has restarted since the send with nothing but the id to route it on: an id below the
// boundary names a release bundle, one at or above it names a running copy. The launcher refuses a
// slice whose release is not positive (shimwire.LauncherBundle.Validate), so both spaces are
// positive and the boundary is the only thing telling them apart.
func TestRunningCopyRestoreIdsSitAboveEveryIdAnIssueReleaseHolds(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	openRunningTree(t, st)
	markRelease(t, st, 30, 31, 32, 0)
	var releaseID int64
	if err := st.Pool().QueryRow(ctx, `select id from issue_releases where project = 'legion' and issue = $1`, rootIssue).Scan(&releaseID); err != nil {
		t.Fatalf("read the id of the marked release: %v", err)
	}
	if err := st.PutRunningWorkspaceCopy(ctx, rootIssue, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work of a running pod"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: time.Now().UTC(), PodUID: runningPodUID}); err != nil {
		t.Fatal(err)
	}
	restoreID := storedRunningCopy(t, st, rootIssue).restoreID
	if restoreID <= 0 {
		t.Fatalf("the copy took restore id %d: a launcher refuses a slice whose release is not positive", restoreID)
	}
	if restoreID < MinRunningWorkspaceCopyRestoreID || restoreID <= releaseID {
		t.Fatalf("the copy took restore id %d against the release id %d, want it above %d",
			restoreID, releaseID, MinRunningWorkspaceCopyRestoreID)
	}
	if releaseID >= MinRunningWorkspaceCopyRestoreID {
		t.Fatalf("a release took id %d, which is a running copy's space", releaseID)
	}

	// The schema is what keeps the spaces apart, not the distance between them: an id a release
	// could only reach by issuing 2^62 of them is refused by name.
	_, err := st.Pool().Exec(ctx, `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, id)
		overriding system value values ('legion', $1, 1, 1, 1, 1, 0, 'architect', $2)`,
		childIssue, MinRunningWorkspaceCopyRestoreID)
	if err == nil {
		t.Fatalf("a release took the id %d, which names a running copy to every restore report", MinRunningWorkspaceCopyRestoreID)
	}
	if !strings.Contains(err.Error(), "issue_releases_id_below_running_copy_restore_ids") {
		t.Fatalf("a release id in the copies' space was refused by something other than the constraint that holds them apart: %v", err)
	}
	// The id below it is a release's own, so the constraint refuses nothing a release needs.
	if _, err := st.Pool().Exec(ctx, `insert into issue_releases
		(project, issue, generation, first_row, boundary_row, last_row, started_row, role, id)
		overriding system value values ('legion', $1, 1, 1, 1, 1, 0, 'architect', $2)`,
		childIssue, MinRunningWorkspaceCopyRestoreID-1); err != nil {
		t.Fatalf("the highest id a release may hold was refused: %v", err)
	}
}
