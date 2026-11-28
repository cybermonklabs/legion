package admit

import (
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// priorityEngine is the real workflow engine these tests apply their facts through, which sees
// every fact before admission, as the daemon's own chain does.
func priorityEngine() *workflow.Engine {
	return workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps(),
		Clock: func() time.Time { return fixedNow }}, nil)
}

// A root admission records and a child its tree records each take the issue's own Dispatch
// priority from the event that first shows it, so the record carries the priority from its
// first write.
func TestTheRootAndTheChildRecordTheirOwnPriorityFromTheirFirstEvent(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := priorityEngine()
	seedSlotted(t, pool, "LEGION-1", "A")

	apply(t, pool, admission, "root-todo", intake.DispatchIssue{Key: "LEGION-5", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: "a P2 root", Rank: "B", Priority: record.P2, HandedOver: handed}, engine)
	if got := issue(t, pool, "LEGION-5"); got.Priority != record.P2 {
		t.Fatalf("the admitted root LEGION-5 carries %v, want P2, the priority its event showed", got.Priority)
	}
	apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: "LEGION-6", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "todo",
		Title: "a P0 child", Parent: "LEGION-1", Rank: "C", Priority: record.P0}, engine)
	child := issue(t, pool, "LEGION-6")
	if child.Tree != "LEGION-1" || child.Priority != record.P0 {
		t.Fatalf("the child LEGION-6 is recorded in tree %s at %v, want tree LEGION-1 at P0, the priority its event showed", child.Tree, child.Priority)
	}
}

// A person who changes a waiting issue's priority in Dispatch makes one issue.updated at the
// status, title and rank the record already holds, and the record takes the new priority from it
// at once, as it takes a re-rank (TestRankOnlyEventTakesTheFreeSlot), with no boot and no
// Reconcile: the next pass reads it. A priority taken off reads as none. The priority set is P1,
// so the issue keeps waiting at the full cap: a P0 would start outside it at that pass
// (TestAnIssueRaisedToP0WhileItWaitsStartsAtTheNextPass).
func TestAPriorityOnlyEventIsRecordedAtTheSameStatus(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := priorityEngine()
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-2", "B")

	apply(t, pool, admission, "set-p1", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-2", Rank: "B", Priority: record.P1, HandedOver: handed}, engine)
	if got := issue(t, pool, "LEGION-2"); got.Priority != record.P1 || got.Status != "todo" || got.Rank != "B" || got.LastDispatchSeq != 2 {
		t.Fatalf("the record after the priority event = %#v, want P1 at todo, rank B, sequence 2", got)
	}
	apply(t, pool, admission, "take-off", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "LEGION-2", Rank: "B", HandedOver: handed}, engine)
	if got := issue(t, pool, "LEGION-2"); got.Priority != record.NoPriority || got.LastDispatchSeq != 3 {
		t.Fatalf("the record after the priority was taken off carries %v at sequence %d, want none at 3", got.Priority, got.LastDispatchSeq)
	}
	apply(t, pool, admission, "old-priority", intake.DispatchIssue{Key: "LEGION-2", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-2", Rank: "B", Priority: record.P0, HandedOver: handed}, engine)
	if got := issue(t, pool, "LEGION-2"); got.Priority != record.NoPriority || got.LastDispatchSeq != 3 {
		t.Fatalf("the older fact replaced the newer null: %+v", got)
	}
}

// The boot listing carries each issue's priority too: an unrecorded root it admits is recorded
// with its priority, and a recorded root whose priority changed while the daemon was away takes
// the new one from a listing newer than its record.
func TestTheBootListingRecordsTheIssuesPriority(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-3", "C")
	p1, p3 := 1, 3

	reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
		{Key: "LEGION-3", Title: "LEGION-3", Status: "todo", Rank: "C", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Priority: &p1},
		{Key: "LEGION-4", Title: "a P3 root", Status: "todo", Rank: "D", HandedOver: handed, LastSeq: 2, ResolvedRepo: "acme/widgets", Priority: &p3},
	}, 2, 2, true)
	if got := issue(t, pool, "LEGION-3"); got.Priority != record.P1 || got.LastDispatchSeq != 2 {
		t.Fatalf("the recorded root LEGION-3 after the listing carries %v at sequence %d, want P1 at 2", got.Priority, got.LastDispatchSeq)
	}
	if got := issue(t, pool, "LEGION-4"); got.Priority != record.P3 {
		t.Fatalf("the root LEGION-4 the listing admitted carries %v, want P3", got.Priority)
	}
}

// A closed root's next run takes the priority on the todo that opens it.
func TestANewRunTakesThePriorityItsTodoShows(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := priorityEngine()
	const key = "LEGION-CLOSED"
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "closed",
		Tree: key, Phase: phase.Done, Generation: 1, Status: "done", DispatchStatus: "done", Rank: "A", Priority: record.P2, LastDispatchSeq: 1})

	apply(t, pool, admission, "readmit", intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "closed", Rank: "A", Priority: record.P0, HandedOver: handed}, engine)
	readmitted := issue(t, pool, key)
	if readmitted.Generation != 2 || readmitted.Priority != record.P0 {
		t.Fatalf("the re-admitted root is at generation %d carrying %v, want generation 2 at P0", readmitted.Generation, readmitted.Priority)
	}
}

// A priority change in Dispatch moves the issue in the waiting line at the daemon's next read of
// it, with no other action. Two roots wait under a full cap in board order; a P1 set on the one
// ranked lower puts it first at the pass the event runs, and taking the priority off puts it back
// behind its neighbour. Every entry here is a root that never ran, so the move is read inside one
// part of the line. The priority is P1 and not P0, which starts outside the cap rather than
// waiting first in the line (TestAnIssueRaisedToP0WhileItWaitsStartsAtTheNextPass).
func TestAPriorityChangeMovesTheIssueInTheWaitingLineAtTheNextRead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine := priorityEngine()
	seedSlotted(t, pool, "LEGION-1", "A")
	seedWaiting(t, pool, "LEGION-2", "B")
	seedWaiting(t, pool, "LEGION-3", "C")
	if got, want := waitingLine(t, pool, testProject, nil), []string{"LEGION-2", "LEGION-3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("the line before the priority event = %#v, want %#v", got, want)
	}

	apply(t, pool, admission, "set-p1", intake.DispatchIssue{Key: "LEGION-3", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "LEGION-3", Rank: "C", Priority: record.P1, HandedOver: handed}, engine)
	assertWaiting(t, pool, []string{"LEGION-3", "LEGION-2"})

	apply(t, pool, admission, "take-off", intake.DispatchIssue{Key: "LEGION-3", ResolvedRepo: "acme/widgets", Seq: 3, Type: "issue.updated", Status: "todo",
		Title: "LEGION-3", Rank: "C", HandedOver: handed}, engine)
	assertWaiting(t, pool, []string{"LEGION-2", "LEGION-3"})
}
