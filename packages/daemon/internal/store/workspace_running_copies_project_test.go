package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// spawningProject is the token of a project that records none of the INFRA issues of these tests:
// a daemon whose operator spawned a claim of an issue copyProject records, on a database the two
// projects' daemons share. Its cadence copies the spawn's pod as it copies every claim of its own
// project (internal/daemon's runningCopyClaim).
const spawningProject = "other"

// requireRunningCopyAsItWas fails the test unless issue's running copy is want, byte for byte:
// its bundle, its size, its restore id, both its times, the pod it names and its attempt.
func requireRunningCopyAsItWas(t *testing.T, s *Store, issue, why string, want storedCopy) {
	t.Helper()
	got := storedRunningCopy(t, s, issue)
	if !bytes.Equal(got.bundle, want.bundle) || got.size != want.size || got.restoreID != want.restoreID ||
		!got.at.Equal(want.at) || !got.writtenAt.Equal(want.writtenAt) || got.sourcePod != want.sourcePod ||
		got.attemptPod != want.attemptPod {
		t.Fatalf("after %s the running copy of %s reads %q off %q under %d, taken at %s and written at %s, want it as it was: %q off %q under %d, taken at %s and written at %s",
			why, issue, got.bundle, got.sourcePod, got.restoreID, got.at, got.writtenAt,
			want.bundle, want.sourcePod, want.restoreID, want.at, want.writtenAt)
	}
	if rows := runningCopyRows(t, s); rows != 1 {
		t.Fatalf("after %s the table holds %d rows, want the one row of %s", why, rows, issue)
	}
}

// A project writes the running copy of an issue only where it records the issue. One issue has one
// running copy and the row names no project, so on a database two projects' daemons share, the
// copy the cadence of one takes off an operator's spawn of the other's issue would replace the
// owning project's running copy, and that project's next pod would be sent the spawn's work in
// place of its own. The save of such a copy is refused with ErrIssueNotRecorded and the owner's row
// stays as it was, byte for byte: neither its bytes nor its times move, even for a copy whose
// content equals the row's, which the save would otherwise answer by moving the row's times
// alone. The owning project's next copy is stored as before, and a save that names no project is
// refused, since whose copy it is rests on the project.
func TestASaveByAProjectThatDoesNotRecordTheIssueLeavesTheOwnersRunningCopy(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-1117"
	recordCopyIssues(t, s, issue)
	owners := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of INFRA's pod"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		Tips: []string{"da4f9f492e9f8349389c243743e0a000cd1c676f"}, Bookmarks: map[string]string{"side": "da4f9f492e9f8349389c243743e0a000cd1c676f"},
		PodUID: "pod-of-infra"}
	if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, copyBegun(t, s), owners); err != nil {
		t.Fatal(err)
	}
	stored := storedRunningCopy(t, s, issue)

	spawns := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of the spawn's pod"), Head: "0a8f0aee63b13682fefe975ba0cc51da6f2ba9e9",
		PodUID: "pod-of-the-spawn"}
	if err := s.PutRunningWorkspaceCopy(ctx, spawningProject, issue, copyBegun(t, s), spawns); !errors.Is(err, ErrIssueNotRecorded) {
		t.Fatalf("the save of the spawn's copy by a project that does not record %s was answered %v, want %v", issue, err, ErrIssueNotRecorded)
	}
	requireRunningCopyAsItWas(t, s, issue, "the spawn's copy", stored)
	// A copy whose content equals the row's would move its two times alone; it moves nothing.
	same := owners
	same.Bundle = bytes.Repeat([]byte{0xa5}, len(owners.Bundle))
	if err := s.PutRunningWorkspaceCopy(ctx, spawningProject, issue, copyBegun(t, s), same); !errors.Is(err, ErrIssueNotRecorded) {
		t.Fatalf("the save of a copy equal to the row by a project that does not record %s was answered %v, want %v", issue, err, ErrIssueNotRecorded)
	}
	requireRunningCopyAsItWas(t, s, issue, "a copy equal to the row", stored)
	if err := s.PutRunningWorkspaceCopy(ctx, "", issue, copyBegun(t, s), spawns); err == nil {
		t.Fatal("a save that named no project was accepted")
	}
	requireRunningCopyAsItWas(t, s, issue, "a save that named no project", stored)

	next := runtime.WorkspaceBundle{Bundle: []byte("PACK the next work of INFRA's pod"), Head: "c4", PodUID: "pod-of-infra"}
	if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, copyBegun(t, s), next); err != nil {
		t.Fatalf("the owning project's next copy was answered %v, want it stored", err)
	}
	if row := storedRunningCopy(t, s, issue); !bytes.Equal(row.bundle, next.Bundle) || row.sourcePod != next.PodUID {
		t.Fatalf("the running copy of %s is %q off %q, want the owning project's next copy %q", issue, row.bundle, row.sourcePod, next.Bundle)
	}
}

// The cadence's delete keeps the same rule as the save. A workspace of an operator's spawn that
// bundled to nothing says nothing of the work of the project that records the issue, so a delete
// by a project that does not record it is refused with ErrIssueNotRecorded and the owner's running
// copy stands, here although its take began before the delete's, which would otherwise drop it.
// The same delete by the owning project drops it, and a delete that names no project is refused.
func TestADeleteByAProjectThatDoesNotRecordTheIssueLeavesTheOwnersRunningCopy(t *testing.T) {
	ctx := context.Background()
	s := migratedStore(t)
	const issue = "INFRA-1117"
	recordCopyIssues(t, s, issue)
	owners := runtime.WorkspaceBundle{Bundle: []byte("PACK the work of INFRA's pod"), Head: "f2f134541b30dfa5160a5d1ce5e17b76c967c661",
		PodUID: "pod-of-infra"}
	if err := s.PutRunningWorkspaceCopy(ctx, copyProject, issue, copyBegun(t, s), owners); err != nil {
		t.Fatal(err)
	}
	stored := storedRunningCopy(t, s, issue)
	begun := copyBegun(t, s)
	if !begun.At().After(stored.at) {
		t.Fatalf("the delete's take began at %s, want it after the stored copy's at %s", begun.At(), stored.at)
	}

	if err := s.DeleteRunningWorkspaceCopy(ctx, spawningProject, issue, begun); !errors.Is(err, ErrIssueNotRecorded) {
		t.Fatalf("the delete of the spawn's empty copy by a project that does not record %s was answered %v, want %v", issue, err, ErrIssueNotRecorded)
	}
	requireRunningCopyAsItWas(t, s, issue, "the spawn's delete", stored)
	if err := s.DeleteRunningWorkspaceCopy(ctx, "", issue, begun); err == nil {
		t.Fatal("a delete that named no project was accepted")
	}
	requireRunningCopyAsItWas(t, s, issue, "a delete that named no project", stored)

	if err := s.DeleteRunningWorkspaceCopy(ctx, copyProject, issue, begun); err != nil {
		t.Fatalf("the owning project's delete was answered %v, want the copy dropped", err)
	}
	if row := heldCopyRow(t, s, runningCopyCopies, issue); row.held {
		t.Fatalf("after the owning project's delete the running copy of %s is %+v, want it dropped", issue, row)
	}
}
