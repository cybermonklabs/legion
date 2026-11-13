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
// and the confirmation of a restore deletes the copy its id names and no other.
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
	// the cadence has already replaced reaches the daemon with nothing but its id: it deletes
	// that copy, which is gone, and leaves the newer work for the issue's next pod. Another
	// issue's copy is no more its business.
	other := runtime.WorkspaceBundle{Bundle: []byte("PACK another issue's work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: at, PodUID: "pod-of-another-issue"}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", other); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{stored.restoreID, ids[0]} {
		if err := s.DeleteRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", id); err != nil {
			t.Fatal(err)
		}
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); !row.held {
		t.Fatal("a report of a copy the cadence replaced took the newer copy")
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-2760"); !row.held {
		t.Fatal("a report of one issue's copy took another issue's")
	}
	if err := s.DeleteRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", ids[1]); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-1117"); row.held {
		t.Fatalf("the copy the report named is still held: %+v", row)
	}
	// A report the daemon read twice finishes, and so does a delete of an issue holding nothing.
	if err := s.DeleteRestoredRunningWorkspaceCopy(ctx, "INFRA-1117", ids[1]); err != nil {
		t.Fatalf("a repeated confirmation: %v", err)
	}
	if err := s.DeleteRunningWorkspaceCopy(ctx, "INFRA-2760"); err != nil {
		t.Fatal(err)
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
// that is wrong, or one a role of its pod has taken over — deletes nothing: that copy is the other
// issue's work, and the pod that restores it is that issue's next one. The same id reported by the
// launcher of the copy's own issue drops it.
func TestARestoreReportNamingAnotherIssuesRunningCopyDeletesNothing(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	other := runtime.WorkspaceBundle{Bundle: []byte("PACK another issue's work"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC), PodUID: "pod-of-another-issue"}
	if err := s.PutRunningWorkspaceCopy(ctx, "INFRA-2760", other); err != nil {
		t.Fatal(err)
	}
	id := storedRunningCopy(t, s, "INFRA-2760").restoreID
	if err := s.DeleteRestoredWorkspaceCopy(ctx, "INFRA-1117", id); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-2760"); !row.held || row.size != other.Size() {
		t.Fatalf("a report of INFRA-1117's launcher naming INFRA-2760's copy %d left %+v, want that copy whole", id, row)
	}
	if err := s.DeleteRestoredWorkspaceCopy(ctx, "INFRA-2760", id); err != nil {
		t.Fatal(err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, "INFRA-2760"); row.held {
		t.Fatalf("the report of the copy's own issue left %+v, want it dropped", row)
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
	// drops the running copy as it stores the bundle (PutWorkspaceBundle), and the pod whose
	// release a wake then cancelled works on and is copied again.
	if err := st.PutWorkspaceBundle(ctx, rootIssue, 7, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work of the release"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		At: now, PodUID: releasedPodUID}); err != nil {
		t.Fatalf("save a release bundle of %s: %v", rootIssue, err)
	}
	if err := st.PutRunningWorkspaceCopy(ctx, rootIssue, runtime.WorkspaceBundle{
		Bundle: []byte("PACK the work after the release"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		At: now.Add(time.Minute), PodUID: runningPodUID}); err != nil {
		t.Fatalf("save a running copy of %s: %v", rootIssue, err)
	}
	if view := idleViewOf(t, st, rootIssue); !view.BundleUnrestored {
		t.Fatalf("view = %+v, want the unrestored release bundle to hold the pod", view)
	}

	// The restore of that release bundle lifts the hold, and the running copy is still there:
	// the confirmation of one copy's restore deletes that copy and no other.
	if err := st.DeleteRestoredWorkspaceBundle(ctx, rootIssue, 7); err != nil {
		t.Fatal(err)
	}
	if view := idleViewOf(t, st, rootIssue); view.BundleUnrestored {
		t.Fatalf("view = %+v, want the confirmed release bundle gone from it", view)
	}
	if row := heldCopyRow(t, st, runningCopyCopies, rootIssue); !row.held {
		t.Fatal("the running copy went with the release bundle's confirmation")
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
