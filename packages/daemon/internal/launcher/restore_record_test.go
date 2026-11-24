package launcher

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testbin"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The records the launcher keeps of its restore on the workspace's own disk (workspace.RecordRestore,
// workspace.MarkInUse), what a launcher restarted over that disk does with them, and what a start
// does when one of them cannot be written, removed or read.

// wantStartRefused sends g's launcher a start of generation whose workspace is dir and wants it
// refused with an answer that says each of words, and its child never run: the marker the child
// would publish is never written.
func (g *rig) wantStartRefused(generation uint64, dir string, words ...string) {
	g.t.Helper()
	marker := filepath.Join(g.t.TempDir(), "marker")
	command := g.childStart(generation, marker, nil)
	command.Env = append(command.Env, "LEGION_WORKSPACE="+dir)
	got := g.start(command)
	if got.OK {
		pid, _ := published(g.t, marker)
		g.t.Fatalf("generation %d's start in %s answered %#v, and its child %d ran; want the start refused", generation, dir, got, pid)
	}
	for _, want := range words {
		if !strings.Contains(got.Error, want) {
			g.t.Fatalf("generation %d's start answered %#v, want its refusal to say %q", generation, got, want)
		}
	}
	if _, err := os.Lstat(marker); !errors.Is(err, fs.ErrNotExist) {
		g.t.Fatalf("generation %d's marker %s is there (%v); want the child never ran", generation, marker, err)
	}
}

// heldWhole is a restore over dir, whose jj and git start starts, that holds every slice of a copy
// of release 11 sent under the sequence id: frames are those slices.
func heldWhole(t *testing.T, start procgroup.Starter, dir, id string) (*bundleRestore, []shimwire.LauncherBundle) {
	t.Helper()
	frames, err := shimwire.SplitWorkspaceBundle(id, 11, manySlices())
	if err != nil {
		t.Fatal(err)
	}
	r := newBundleRestore(start, dir)
	for _, frame := range frames {
		if got := r.receive(frame); !got.OK {
			t.Fatalf("slice %d answered %#v", frame.Index, got)
		}
	}
	return r, frames
}

// wantSettled wants r to owe the report of a restore of the sequence id of release 11, which the
// workspace at dir records no creation for, so the restore ended before it started anything, and
// the disk to hold that report as the restore's record, ended.
func wantSettled(t *testing.T, r *bundleRestore, dir, id string) {
	t.Helper()
	report := r.owed()
	if report == nil || report.ID != id || report.Release != 11 || report.OK || report.Error == "" {
		t.Fatalf("the launcher owes %+v, want the failed restore of sequence %q of release 11 with its reason", report, id)
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); !ok || err != nil || !record.Ended || record.Report != *report {
		t.Fatalf("the restore's record is %+v, %t, %v; want it ended with the report %+v", record, ok, err, *report)
	}
}

// A roles container that restarts after its restore ended, before any connection carried the
// report, comes back with a launcher process of its own over the same disk. That process holds
// nothing of the one before in memory, but the restore's record on the disk holds the report
// (workspace.RecordRestore), so it owes the same report, and it still refuses every slice of the
// bundle sent again, since the in-use record is there too. Once a connection has carried the
// report (sent) the record is gone, and the launcher after that owes nothing.
func TestALauncherRestartedAfterItsRestoreEndedStillOwesTheReportAndRefusesTheBundle(t *testing.T) {
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir}
	first, frames := heldWhole(t, refusingStarter(t), dir, "restore-restarted")
	// The workspace records no creation, so the restore ends before it starts anything, and its
	// report says why.
	if got, err := first.apply(context.Background(), env); got.Restored || got.Unrecorded != nil || err != nil {
		t.Fatalf("the first launcher's apply answered %+v, %v; want a restore that failed, recorded, and no error", got, err)
	}
	wantSettled(t, first, dir, frames[0].ID)
	report := first.owed()

	// The restarted container's launcher: a restore of its own over the same disk.
	restarted := newBundleRestore(refusingStarter(t), dir)
	if owed := restarted.owed(); owed == nil || *owed != *report {
		t.Fatalf("the restarted launcher owes %+v, want the first launcher's report %+v", owed, *report)
	}
	for _, frame := range frames {
		if got := restarted.receive(frame); got.OK || got.Held || got.Error != workspaceInUseRefusal {
			t.Fatalf("slice %d to the restarted launcher answered %#v; want it refused, since a restore has run on the workspace", frame.Index, got)
		}
	}
	if got, err := restarted.apply(context.Background(), env); got != (restoreResult{}) || err != nil {
		t.Fatalf("the restarted launcher's apply answered %+v, %v; want nothing and no error", got, err)
	}
	if err := restarted.sent(restarted.owed()); err != nil {
		t.Fatalf("the report's send = %v", err)
	}
	if owed := newBundleRestore(refusingStarter(t), dir).owed(); owed != nil {
		t.Fatalf("a launcher after a connection carried the report owes %+v, want nothing", owed)
	}
}

// A roles container killed in the middle of its restore leaves the in-use record and the
// restore's record begun and never ended (bundleRestore.apply). No role has run there, since none
// starts before a restore ends, but the workspace may hold part of the restore, and the work is
// in the daemon's database alone. So the launcher that comes back starts no role while no copy has
// been sent again, each start refused with why and with what clears it where the daemon sends no
// copy again: replacing the pod, resetting a kept volume, or deleting the record, named by its
// path. The begun record stays for the start that restores the copy. Deleting the record by hand
// clears the refusal for the same launcher: its next start reads the record again
// (bundleRestore.stillCutOff) and runs its role on the workspace as the cut-off restore left it,
// and the in-use rule still holds, so a copy sent after that start is refused as one to a
// workspace in use.
//
// The controls (the pull request records the runs): the refusal without what clears it, as
// before, which fails on the first remedy it does not name; and apply refusing on the cut-off
// reading it took at the launcher's start without reading the record again, as before, which
// refuses the start after the deletion.
func TestAStartOverARestoreCutOffWithNoCopySentAgainRunsNoRole(t *testing.T) {
	dir := newWorkspace(t)
	begun := workspace.RestoreRecord{Report: shimwire.LauncherRestored{ID: "restore-cut-off", Release: 11}}
	if err := workspace.RecordRestore(dir, begun); err != nil {
		t.Fatal(err)
	}
	if err := workspace.MarkInUse(dir); err != nil {
		t.Fatal(err)
	}
	g := newSessionRig(t, Config{Workspace: dir})
	// The daemon's retry is a new start, which finds the restore cut off as the first did.
	for _, generation := range []uint64{1, 2} {
		g.wantStartRefused(generation, dir, "restore of release 11", dir, "cut off", "may hold part of it",
			"no copy of the work has been sent again", "the roles do not start", "only while no role of the pod has registered",
			"until the pod is replaced", "on a kept volume once that volume is reset", workspace.RestoreRecordPath(dir)+" is deleted")
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); !ok || err != nil || record != begun {
		t.Fatalf("after the refused starts the restore's record is %+v, %t, %v; want it begun as it was, %+v", record, ok, err, begun)
	}
	if g.restored != nil {
		t.Fatalf("the launcher reported %#v, want no report of a restore that never ended", g.restored)
	}

	if err := os.Remove(workspace.RestoreRecordPath(dir)); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	command := g.childStart(3, marker, nil)
	command.Env = append(command.Env, "LEGION_WORKSPACE="+dir)
	if result := g.start(command); !result.OK {
		t.Fatalf("the start after the record was deleted by hand answered %#v, want the role started", result)
	}
	published(t, marker)
	frames, err := shimwire.SplitWorkspaceBundle("restore-after-the-deletion", 12, shimwire.WorkspaceBundle{Bundle: []byte("PACK the work"), Head: "f2f13454", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := g.answer(frames[0]).(shimwire.LauncherBundleResult); !ok || got.OK || got.Held || got.Error != workspaceInUseRefusal {
		t.Fatalf("a slice after the start the deletion let run answered %#v; want it refused as one to a workspace in use, %q", got, workspaceInUseRefusal)
	}
	if g.restored != nil {
		t.Fatalf("the launcher reported %#v, want no report: no restore ran", g.restored)
	}
	g.stop(3)
}

// damagedRecord writes a record of a restore that does not read on the workspace at dir.
func damagedRecord(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(workspace.RestoreRecordPath(dir), []byte("{not a record"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workspace.ReadRestoreRecord(dir); err == nil {
		t.Fatal("premise: the damaged record reads without an error")
	}
}

// A record of a restore that cannot be read says nothing of the roles, so the launcher reads the
// in-use record beside it (bundleRestore.read). On a workspace that holds none, no role has
// started there, and the record counts as a restore cut off, not as none: counted as none, the
// roles would start on a workspace no restore may have put the work back into. So the launcher
// takes the copy the daemon sends again and runs its restore, which ends at once here, since the
// workspace records no creation (wantSettled), and a start with no copy sent again runs no role
// and says why.
//
// The control (the pull request records the run): read counting every unreadable record as one on
// a workspace in use, which refuses the slice.
func TestAnUnreadableRestoreRecordOnAWorkspaceNoRoleStartedOnCountsAsARestoreCutOff(t *testing.T) {
	dir := newWorkspace(t)
	damagedRecord(t, dir)
	g := newSessionRig(t, Config{Workspace: dir})
	g.wantStartRefused(1, dir, "could not be read", dir, "no role has started on", "cut off",
		"no copy of the work has been sent again", "the roles do not start")
	if !strings.Contains(g.stderr.String(), "counts as a restore cut off") {
		t.Fatalf("the launcher's log does not say the unreadable record counts as a restore cut off:\n%s", g.stderr.String())
	}

	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-unreadable")
	if got, err := r.apply(context.Background(), []string{"LEGION_WORKSPACE=" + dir}); err != nil || got.Unrecorded != nil {
		t.Fatalf("the restore of the copy sent again answered %+v, %v; want it run and recorded", got, err)
	}
	wantSettled(t, r, dir, frames[0].ID)
}

// A record of a restore that cannot be read on a workspace marked in use proves nothing: on a kept
// volume it can be an earlier pod's, whose roles worked the disk after it, and a copy restored
// there would move their work back, while a role started there could run on a restore left half
// done. So the launcher refuses every slice and every role start, and says so and what clears it.
// The record removed by hand clears it: a slice after the removal reads the records again (fence),
// which clears the block, and is refused only as one to a workspace in use, as every slice is once
// a role may have worked there; and the next start runs its role on the workspace as it is.
//
// The control (the pull request records the run): read counting every unreadable record as a
// restore cut off, as before, which takes the slice.
func TestAnUnreadableRestoreRecordOnAWorkspaceInUseRefusesEverySliceAndEveryStart(t *testing.T) {
	dir := newWorkspace(t)
	if err := workspace.MarkInUse(dir); err != nil {
		t.Fatal(err)
	}
	damagedRecord(t, dir)
	frames, err := shimwire.SplitWorkspaceBundle("restore-unreadable", 12, shimwire.WorkspaceBundle{Bundle: []byte("PACK the work"), Head: "f2f13454", At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	unsafe := []string{"could not be read", dir, "the workspace is in use", "neither a restore nor a role start is safe", workspace.RestoreRecordPath(dir), "the roles do not start"}
	blocked := newBundleRestore(refusingStarter(t), dir)
	got := blocked.receive(frames[0])
	for _, want := range unsafe {
		if got.OK || !strings.Contains(got.Error, want) {
			t.Fatalf("a slice over an unreadable record on a workspace in use answered %#v; want it refused, saying %q", got, want)
		}
	}
	g := newSessionRig(t, Config{Workspace: dir})
	if !strings.Contains(g.stderr.String(), "neither a restore nor a role start is safe") {
		t.Fatalf("the launcher's log does not name the unreadable record:\n%s", g.stderr.String())
	}
	g.wantStartRefused(1, dir, unsafe...)

	if err := os.Remove(workspace.RestoreRecordPath(dir)); err != nil {
		t.Fatal(err)
	}
	if got := blocked.receive(frames[0]); got.OK || got.Error != workspaceInUseRefusal {
		t.Fatalf("a slice after the record was removed by hand answered %#v; want the block cleared and the slice refused as one to a workspace in use, %q", got, workspaceInUseRefusal)
	}
	if blocked.blocked != nil {
		t.Fatalf("after the record was removed by hand the restore is still blocked: %v", blocked.blocked)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	command := g.childStart(2, marker, nil)
	command.Env = append(command.Env, "LEGION_WORKSPACE="+dir)
	if result := g.start(command); !result.OK {
		t.Fatalf("the start after the record was removed by hand answered %#v, want the role started", result)
	}
	published(t, marker)
	g.stop(2)
}

// A start refused over a restore cut off has the next start read the records again
// (bundleRestore.stillCutOff), and a record that by then cannot be read, on a workspace marked in
// use, is a blocked workspace rather than a restore cut off: the mark there is the cut-off
// restore's own, but a record that does not read proves nothing of whose restore it was. So the
// next start is refused as one over a blocked workspace, naming the record, and runs no role.
//
// The control (the pull request records the run): stillCutOff without its arm of a blocked
// workspace, which lets the next start run its role.
func TestARestoreCutOffWhoseRecordIsThenDamagedRefusesTheNextStartAsBlocked(t *testing.T) {
	dir := newWorkspace(t)
	begun := workspace.RestoreRecord{Report: shimwire.LauncherRestored{ID: "restore-cut-off", Release: 11}}
	if err := workspace.RecordRestore(dir, begun); err != nil {
		t.Fatal(err)
	}
	if err := workspace.MarkInUse(dir); err != nil {
		t.Fatal(err)
	}
	g := newSessionRig(t, Config{Workspace: dir})
	g.wantStartRefused(1, dir, "restore of release 11", dir, "cut off", "no copy of the work has been sent again")

	damagedRecord(t, dir)
	g.wantStartRefused(2, dir, "could not be read", "neither a restore nor a role start is safe", workspace.RestoreRecordPath(dir))
}

// A start whose in-use mark fails after it wrote the mark — the record created, its directory's
// sync failed — leaves the restore's record begun on the disk, which is the order that keeps a
// killed container's disk reading as a restore cut off: the begun record before the in-use one.
// That mark is this process's own, written for the restore it began, so the next start does not
// read it as a role's use of the workspace: it runs the restore of the copy it holds, which ends at
// once here, since the workspace records no creation (wantSettled), and settles the record before
// any role starts, rather than start a role over a begun record and let the copy go.
//
// The controls (the pull request records the runs): inUse reading the mark while a restore of
// this process is begun, which starts the role with nothing restored; and apply writing the
// in-use mark before the begun record, which leaves no begun record after the failed mark.
func TestAStartWhoseInUseMarkFailsAfterItWroteTheMarkRestoresAtTheNextStart(t *testing.T) {
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir}
	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-marked")
	failing := true
	r.markInUse = func(dir string) error {
		if err := workspace.MarkInUse(dir); err != nil {
			return err
		}
		if failing {
			return errors.New("sync the in-use record's directory: input/output error")
		}
		return nil
	}
	if got, err := r.apply(context.Background(), env); err == nil || !strings.Contains(err.Error(), "mark the workspace") {
		t.Fatalf("the start whose mark failed answered %+v, %v; want its error", got, err)
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); !ok || err != nil || record.Ended || record.Report.ID != frames[0].ID {
		t.Fatalf("after the failed mark the restore's record is %+v, %t, %v; want the restore of %q begun", record, ok, err, frames[0].ID)
	}
	if inUse, err := workspace.InUse(dir); !inUse || err != nil {
		t.Fatalf("premise: after the failed mark InUse = %t, %v; want the mark's record there", inUse, err)
	}

	failing = false
	if got, err := r.apply(context.Background(), env); err != nil || got.Unrecorded != nil || got.Notice == "" {
		t.Fatalf("the next start answered %+v, %v; want the held copy's restore run, with its notice, and recorded", got, err)
	}
	wantSettled(t, r, dir, frames[0].ID)
}

// A start that cannot write the record of the restore it begins runs no role: a later launcher
// over the workspace could not tell a restore cut off from a role's work. The copy stays held and
// nothing is marked in use, so the next start, once the record can be written, runs its restore,
// which ends at once here, since the workspace records no creation (wantSettled), and settles the
// record.
func TestAStartWhoseBegunRecordCannotBeWrittenRunsNoRoleAndTheNextStartRestores(t *testing.T) {
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir}
	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-unwritten")
	failing := true
	r.recordRestore = func(dir string, record workspace.RestoreRecord) error {
		if failing {
			return errors.New("write the restore's record: no space left on device")
		}
		return workspace.RecordRestore(dir, record)
	}
	if got, err := r.apply(context.Background(), env); err == nil || !strings.Contains(err.Error(), "that the restore of release 11 begins") {
		t.Fatalf("the start whose begun record failed answered %+v, %v; want its error", got, err)
	}
	if r.body == nil || r.applied {
		t.Fatal("the failed start let the held copy go or counted the restore as applied")
	}
	if inUse, err := workspace.InUse(dir); inUse || err != nil {
		t.Fatalf("after the failed start InUse = %t, %v; want the workspace unmarked", inUse, err)
	}

	failing = false
	if got, err := r.apply(context.Background(), env); err != nil || got.Unrecorded != nil {
		t.Fatalf("the next start answered %+v, %v; want the held copy's restore run and recorded", got, err)
	}
	wantSettled(t, r, dir, frames[0].ID)
}

// A begun record that settled can neither replace with the outcome nor remove would read, to a
// launcher after this one, as a restore cut off with no role run beside it, so every start that
// meets it runs no role, and each tries again.
func TestARestoreRecordThatCanNeitherBeSettledNorRemovedRunsNoRole(t *testing.T) {
	dir := newWorkspace(t)
	env := []string{"LEGION_WORKSPACE=" + dir}
	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-unsettled")
	r.recordRestore = func(dir string, record workspace.RestoreRecord) error {
		if record.Ended {
			return errors.New("write the restore's record: no space left on device")
		}
		return workspace.RecordRestore(dir, record)
	}
	r.removeRecord = func(string) error { return errors.New("remove the restore's record: read-only file system") }
	for start := range 2 {
		if got, err := r.apply(context.Background(), env); err == nil || !strings.Contains(err.Error(), "still says it is under way") {
			t.Fatalf("start %d over the unsettled record answered %+v, %v; want its error", start+1, got, err)
		}
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); !ok || err != nil || record.Ended || record.Report.ID != frames[0].ID {
		t.Fatalf("the restore's record is %+v, %t, %v; want the restore of %q still begun", record, ok, err, frames[0].ID)
	}
}

// An outcome that cannot be recorded stops no role: the begun record is removed in its place, so
// no launcher after this one reads a restore under way, the report stays owed in memory, and the
// role's log says that a launcher restarted before a connection carries the report does not send
// it.
func TestAnOutcomeThatCannotBeRecordedStartsTheRolesAndKeepsTheReportInMemory(t *testing.T) {
	dir := newWorkspace(t)
	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-unrecorded")
	r.recordRestore = func(dir string, record workspace.RestoreRecord) error {
		if record.Ended {
			return errors.New("write the restore's record: no space left on device")
		}
		return workspace.RecordRestore(dir, record)
	}
	var log bytes.Buffer
	m := &manager{restore: r, stderr: &log}
	got, err := m.applyRestore(context.Background(), []string{"LEGION_WORKSPACE=" + dir})
	if err != nil || got.Unrecorded == nil {
		t.Fatalf("the start answered %+v, %v; want the role started with the outcome unrecorded", got, err)
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); ok || err != nil {
		t.Fatalf("the restore's record is %+v, %t, %v; want the begun record removed", record, ok, err)
	}
	if report := r.owed(); report == nil || report.ID != frames[0].ID {
		t.Fatalf("the launcher owes %+v, want the report of sequence %q", report, frames[0].ID)
	}
	for _, want := range []string{"the restore's report could not be recorded on the workspace's disk", "no space left on device"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("the role's log says %q, want it to say %q", log.String(), want)
		}
	}
}

// A report that reached the daemon is forgotten even where its record cannot be removed from the
// disk: the error is logged, and the record stays for a launcher restarted over that disk, which
// sends the report again.
func TestAReportWhoseRecordCannotBeRemovedIsForgottenAndTheErrorLogged(t *testing.T) {
	dir := newWorkspace(t)
	r, frames := heldWhole(t, refusingStarter(t), dir, "restore-unremoved")
	if _, err := r.apply(context.Background(), []string{"LEGION_WORKSPACE=" + dir}); err != nil {
		t.Fatal(err)
	}
	r.removeRecord = func(string) error { return errors.New("remove the restore's record: read-only file system") }
	var log, wire bytes.Buffer
	m := &manager{restore: r, stderr: &log, writer: shimwire.NewWriter(&wire)}
	m.reportRestore()
	if !strings.Contains(wire.String(), strconv.Quote(frames[0].ID)) {
		t.Fatalf("the connection carried %q, want the report of sequence %q", wire.String(), frames[0].ID)
	}
	if report := r.owed(); report != nil {
		t.Fatalf("after the report was carried the launcher owes %+v, want it forgotten", report)
	}
	for _, want := range []string{"could not be removed", "read-only file system"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("the role's log says %q, want it to say %q", log.String(), want)
		}
	}
	if record, ok, err := workspace.ReadRestoreRecord(dir); !ok || err != nil || !record.Ended {
		t.Fatalf("the restore's record is %+v, %t, %v; want it ended, for a restarted launcher to send", record, ok, err)
	}
}

// A roles container killed in the middle of its restore leaves what the restore had done so far:
// the in-use record, the restore's record begun, and its temporary branches, the git refs and,
// once `jj git import` has run, jj's bookmarks of them. The launcher that comes back takes the
// copy the daemon sends again, since no role has run there (fence), clears those branches
// (clearRestoreBranches) and restores the copy to its end: the workspace holds what a clean
// restore leaves, and the record holds the restore ended, the report the daemon is owed.
func TestARestoreCutOffInTheMiddleIsRunAgainToItsEndByTheNextLauncher(t *testing.T) {
	testbin.RequireJJFilters(t)
	f := newBundleFixture(t)
	f.workOne()
	saved := f.bundleWorkspaceOne()

	// What a clean restore leaves, in a pod's workspace of its own.
	clean := f.secondPod("ws-clean")
	if notice := f.restoreInto(clean, saved, 1); !strings.Contains(notice, "is restored") {
		t.Fatalf("the clean restore's notice is %q", notice)
	}
	want := f.restoredState(clean)

	ws := f.secondPod("ws-cut-off")
	frames, err := shimwire.SplitWorkspaceBundle("restore-"+ws, bundleRelease, saved)
	if err != nil {
		t.Fatal(err)
	}
	// The killed launcher's restore, as far as its `jj git import`: the records apply writes
	// before it, the unbundle, a temporary branch per saved head and jj's bookmarks of them.
	begun := shimwire.LauncherRestored{ID: frames[0].ID, Release: bundleRelease}
	if err := workspace.RecordRestore(ws, workspace.RestoreRecord{Report: begun}); err != nil {
		t.Fatal(err)
	}
	if err := workspace.MarkInUse(ws); err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(t.TempDir(), "saved.bundle")
	if err := os.WriteFile(body, saved.Bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	gitDir := f.GitDir(ws)
	f.Run(ws, "git", "--git-dir="+gitDir, "bundle", "unbundle", body)
	for index, tip := range saved.Tips {
		f.Run(ws, "git", "--git-dir="+gitDir, "update-ref", restoreRefHeadPrefix+strconv.Itoa(index), tip)
	}
	f.Run(ws, "jj", "git", "import", "-R", ws)
	if listed := f.Run(ws, "jj", "bookmark", "list", "-R", ws, "--ignore-working-copy", "-T", `if(remote, "", name ++ "\n")`); !strings.Contains(listed, restoreRefPrefix+"0") {
		t.Fatalf("premise: the killed restore's bookmarks are not in the workspace:\n%s", listed)
	}

	// The restarted container's launcher, sent the copy again.
	restarted := newBundleRestore(procgroup.NoReaper, ws)
	for _, frame := range frames {
		if got := restarted.receive(frame); !got.OK || got.Held != (frame.Index == len(frames)-1) {
			t.Fatalf("slice %d after the cut-off restore answered %#v; want it taken, since no role has run on the workspace", frame.Index, got)
		}
	}
	env := mergeEnv(os.Environ(), append(append([]string{}, f.Env...), "LEGION_WORKSPACE="+ws))
	got, err := restarted.apply(context.Background(), env)
	if err != nil || !got.Restored || got.Unrecorded != nil || !strings.Contains(got.Notice, "is restored") {
		t.Fatalf("the restore over the cut-off one answered %+v, %v; want the saved work restored and recorded", got, err)
	}
	f.requireRestored(ws, saved)
	if state := f.restoredState(ws); state != want {
		t.Fatalf("the restore over the cut-off one left the workspace\n%s\nwant it as a clean restore leaves one\n%s", state, want)
	}
	begun.OK = true
	ended := workspace.RestoreRecord{Report: begun, Ended: true}
	if record, ok, err := workspace.ReadRestoreRecord(ws); !ok || err != nil || record != ended {
		t.Fatalf("the restore's record is %+v, %t, %v; want %+v", record, ok, err, ended)
	}
}

// restoredState is what a restore leaves in workspace, as one string a test compares across two
// pods' workspaces: every commit jj shows but the working copies, with its description, every
// local bookmark with its commit, and this workspace's working-copy commit. The working copies are
// left out because each pod's clone has a default workspace whose working-copy commit is that
// clone's own; the restored workspace's is the last line.
func (f *bundleFixture) restoredState(workspace string) string {
	f.t.Helper()
	log := f.Run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "all() ~ working_copies()", "--no-graph",
		"-T", `commit_id ++ " " ++ description.first_line() ++ "\n"`)
	bookmarks := f.Run(workspace, "jj", "bookmark", "list", "-R", workspace, "--ignore-working-copy",
		"-T", `if(remote, "", name ++ " " ++ normal_target.commit_id() ++ "\n")`)
	head := f.Run(workspace, "jj", "log", "-R", workspace, "--ignore-working-copy", "-r", "@", "--no-graph", "-T", "commit_id")
	return "commits:\n" + log + "bookmarks:\n" + bookmarks + "working copy: " + head
}
