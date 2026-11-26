package workflow

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Each door that writes done, one case for each row of the plan's table of doors (D16), by the
// names there: an issue whose pull request merged closes only by its checks or by a person. Door 9,
// admission's release of an unslotted root, is admission's (internal/admit), and doors 11 and 12
// write nothing new.
func TestEachDoorToDone(t *testing.T) {
	t.Parallel()
	for _, door := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"1/TestAMergeInMergingDoesNotCloseWhileACheckHasNoPass", TestAMergeInMergingDoesNotCloseWhileACheckHasNoPass},
		{"2/TestAMergeInRetroWaitsWhenTheIssueReachesMerging", TestAMergeInRetroWaitsWhenTheIssueReachesMerging},
		{"3/the last child leaves, and the root closes", TestTheLastChildsLeaveQueuesARead},
		{"4/TestARestartBetweenTheMergeAndTheFirstReadLosesNothing", TestARestartBetweenTheMergeAndTheFirstReadLosesNothing},
		{"4/TestTheFirstBootMovesAMergedIssueInMergingIntoVerifying", TestTheFirstBootMovesAMergedIssueInMergingIntoVerifying},
		{"5/TestAMergeInAParkedTreeWaitsForItsChecks", TestAMergeInAParkedTreeWaitsForItsChecks},
		{"5/TestAMemberOfALingeringTreeClosesOnItsChecks", TestAMemberOfALingeringTreeClosesOnItsChecks},
		{"6/TestAPersonsDoneClosesAndSaysWhatWaited", TestAPersonsDoneClosesAndSaysWhatWaited},
		{"7/TestAnArchitectsDoneOnAMergedChildIsSetBack", TestAnArchitectsDoneOnAMergedChildIsSetBack},
		{"7/TestAnArchitectsDoneOnAnUnmergedChildDropsIt", TestAnArchitectsDoneOnAnUnmergedChildDropsIt},
		{"8/TestCloseRootRefusesARootWhosePullRequestMerged", TestCloseRootRefusesARootWhosePullRequestMerged},
		{"10/TestTheStatusRepairWritesTestingForVerifying", TestTheStatusRepairWritesTestingForVerifying},
	} {
		t.Run(door.name, door.run)
	}
}

// A restart between the merge and the first read loses nothing: the boot keeps the read the merge
// queued and queues no second one, the issue stays in verifying, and the read closes it when it
// comes.
func TestARestartBetweenTheMergeAndTheFirstReadLosesNothing(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.fact(intake.DaemonBooted{Boot: 2})
	rig.stands(waitRoot, phase.Verifying, "testing")
	if got := rig.rows(waitRoot, "close_read"); got != 1 {
		t.Fatalf("close_read rows after the boot = %d, want the one the merge queued", got)
	}
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("done status writes after the boot = %d, want none", got)
	}
	rig.read(waitRoot, answerOf(waitRoot, nil, passing("c-after", waitRootMerge)))
	closedOnChecks(t, rig, waitRoot)
}

// A merge in a tree a person parked stands its member in verifying: the member has not closed, so
// nobody is told child-closed, and its checks are read as in a running tree.
func TestAMergeInAParkedTreeWaitsForItsChecks(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Implementing, "in_progress")
	rig.seedChild(waitChild, phase.Merging, "retro")
	rig.fact(intake.DispatchIssue{Key: waitRoot, Seq: 2, Type: "issue.updated", Status: "backlog", Title: "root", Rank: "U"})
	if !rig.issue(waitRoot).Lingers() {
		t.Fatalf("the parked root does not linger")
	}
	rig.mergeChild()
	rig.stands(waitChild, phase.Verifying, "testing")
	if got := noticesOfKind(t, rig.pool, waitChild, "child-closed"); len(got) != 0 {
		t.Fatalf("child-closed notices = %+v, want none: the member waits for its checks", got)
	}
	if got := rig.rows(waitChild, "close_read"); got != 1 {
		t.Fatalf("close_read rows of the member = %d, want one", got)
	}
}

// A person's done closes an issue that waits in verifying, as a person's close does. The close's
// one message names what waited, each open defect is marked closed by the person, and an open
// production check goes on as for any closed issue.
func TestAPersonsDoneClosesAndSaysWhatWaited(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, claim.RoleTester)
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.seedCheck(waitRoot, waitGeneration, waitRootMerge, "")
	rig.read(waitRoot, answerOf(waitRoot, nil, noResult("c-after", waitRootMerge)))
	defect := rig.seedDefect(waitRoot, "the retro found the cache key unchecked")

	rig.fact(intake.DispatchIssue{Key: waitRoot, ResolvedRepo: waitRepo, Seq: 2, Type: "issue.closed", Status: "done", Title: "root", Rank: "U"})
	// The record's status is admission's to write (its recordObservation), which this rig stubs.
	if got := issueAt(t, rig.pool, waitRoot); got != phase.Done {
		t.Fatalf("%s stands at %s after a person's done, want done", waitRoot, got)
	}
	messages := messageBodies(t, rig.pool)
	if len(messages) != 1 || !strings.Contains(messages[0], "c-after") || !strings.Contains(messages[0], "production check") {
		t.Fatalf("messages = %q, want one naming the check and the production check the close waited for", messages)
	}
	var reason string
	if err := rig.pool.QueryRow(t.Context(), "select withdraw_reason from defects where id = $1", defect).Scan(&reason); err != nil || reason != "closed_by_person" {
		t.Fatalf("the defect's withdrawal = %q, %v; want closed_by_person", reason, err)
	}
	if check := productionCheck(t, rig.pool, waitRoot); check == nil || !check.Open() {
		t.Fatalf("the production check = %+v, want it open still", check)
	}
	if rig.issue(waitRoot).ClosedOnChecksAt != nil {
		t.Fatalf("a person's close is marked as a close on the checks")
	}
}

// The owning architect's done on a child whose pull request merged is set back, as a session's
// write is, and the architect is told what the close waits for: such a child closes by its checks.
func TestAnArchitectsDoneOnAMergedChildIsSetBack(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Implementing, "in_progress")
	rig.seedChild(waitChild, phase.Merging, "retro")
	seedArchitectClaim(t, rig.pool, waitRoot)
	rig.mergeChild()
	rig.fact(intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.closed", Status: "done", Title: waitChild, Parent: waitRoot, Rank: "V",
		ActorSession: "session-architect"})
	rig.stands(waitChild, phase.Verifying, "testing")
	if got := rig.statusWrites(waitChild, "testing"); got != 2 {
		t.Fatalf("the child's testing writes = %d, want the entry's and the set-back's", got)
	}
	notices := noticesOfKind(t, rig.pool, waitChild, "close-waits")
	if len(notices) != 2 || !strings.Contains(notices[1].Reason, "set your done") {
		t.Fatalf("close-waits notices = %+v, want the entry's and the set-back's", notices)
	}
}

// The owning architect's done on a child with no merged pull request takes the child out of the
// workflow, as before.
func TestAnArchitectsDoneOnAnUnmergedChildDropsIt(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Implementing, "in_progress")
	rig.seedChild(waitChild, phase.Implementing, "in_progress")
	seedArchitectClaim(t, rig.pool, waitRoot)
	rig.fact(intake.DispatchIssue{Key: waitChild, Seq: 2, Type: "issue.closed", Status: "done", Title: waitChild, Parent: waitRoot, Rank: "V",
		ActorSession: "session-architect"})
	if got := rig.issue(waitChild).Phase; got != phase.Done {
		t.Fatalf("the unmerged child stands at %s, want done", got)
	}
}

// close_root refuses a root whose pull request merged, CLOSE_AFTER_MERGE: such a root closes by
// its checks or a person. Here its next change waits at admitted.
func TestCloseRootRefusesARootWhosePullRequestMerged(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Admitted, "todo")
	seedRecord(t, rig.pool, func(tx pgx.Tx) error {
		_, err := record.NewStore().PutMergeSnapshot(t.Context(), tx, waitRoot, record.Merge{Number: waitRootNumber, Head: headOne, SHA: waitRootMerge,
			Base: "main", Repo: waitRepo, MergedAt: waitMergedAt})
		return err
	})
	result := rig.fact(intake.CloseRoot{Issue: waitRoot, Reason: "moot", Generation: waitGeneration})
	if code := refusalCode(result); code != "CLOSE_AFTER_MERGE" {
		t.Fatalf("close_root's answer = %q, want CLOSE_AFTER_MERGE", code)
	}
	rig.stands(waitRoot, phase.Admitted, "todo")
}

// The status repair writes testing for an issue in verifying whose status write was lost, and
// never done.
func TestTheStatusRepairWritesTestingForVerifying(t *testing.T) {
	t.Parallel()
	rig := newWaitRig(t, "")
	rig.seedRoot(phase.Merging, "retro")
	rig.mergeRoot()
	rig.ranOut(waitRoot, "dispatch_status")
	rig.fact(intake.ControllerTick{})
	if got := rig.statusWrites(waitRoot, "testing"); got != 1 || rig.firstID(waitRoot, "dispatch_status", "observedStatus", "retro") == 0 {
		t.Fatalf("the repair's testing writes = %d, want one over retro", got)
	}
	if got := rig.statusWrites(waitRoot, "done"); got != 0 {
		t.Fatalf("the repair's done writes = %d, want none", got)
	}
}
