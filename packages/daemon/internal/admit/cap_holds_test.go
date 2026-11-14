package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// What the cap can and cannot hold back, now that an issue keeps its row from the moment the
// daemon starts it until its work is past its review (record.KeepsSlot). An issue that holds a
// row starts its next phase whatever admission_cap says, because its promotion takes no second
// row (admit's slotFor); an issue that holds none takes one when the daemon starts it, and
// waits in the line while the project is at its cap.

// capMember records key under root in stands, with the Dispatch status a member in that phase
// shows. It writes no row: what each case here is about is whether the issue holds one.
func capMember(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase, status, rank string) {
	t.Helper()
	parent := root
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: key, Tree: root, Parent: &parent, Phase: stands, Generation: 1,
		Status: status, DispatchStatus: status, Rank: rank, LastDispatchSeq: 2})
}

// acceptingRoot moves a seeded root to accepting, where a parent whose own phases are behind it
// waits while its children work. Its work stands past its review there, so the pass's release
// takes back the row it took at its admission (record.KeepsSlot), and its own first phase is no
// longer in the waiting line.
func acceptingRoot(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	root := issue(t, pool, key)
	root.Phase, root.Status, root.DispatchStatus = phase.Accepting, "needs_review", "needs_review"
	putIssue(t, pool, root)
}

// oweMemberStart queues the start of key's worker in the phase it stands in, as the transition
// into that phase does (workflow.StartOrOwe): the one row that carries the task, which the
// outbox holds unclaimable until key holds a row. stands is the phase the row names, which is
// the issue's own except where a case is about a row the issue has moved past.
func oweMemberStart(t *testing.T, pool *pgxpool.Pool, root, key string, stands phase.Phase) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, record.SuperviseRequest{Op: "start",
			Tree: root, Role: phase.RoleFor(stands), Generation: 1, Phase: stands, Task: "Continue " + key + ".",
			Entry: 1}, fixedNow); err != nil {
			t.Fatalf("owe %s a start: %v", key, err)
		}
	})
}

// claimableStart says whether a lane may now run a start of issue that carries a task, which is
// what says the next phase's worker has been given its work.
func claimableStart(t *testing.T, pool *pgxpool.Pool, issue string) bool {
	t.Helper()
	for _, start := range startsOf(t, pool, issue) {
		if start.task != "" && start.claimable {
			return true
		}
	}
	return false
}

// closeTree closes key as Dispatch's own close does, which frees the row it held.
func closeTree(t *testing.T, pool *pgxpool.Pool, admission *Admission, id, key, rank string) {
	t.Helper()
	apply(t, pool, admission, id, intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.closed", Status: "done", Title: key, Rank: rank, HandedOver: true}, admission.engine)
}

// A root holds the row it took at its admission while its architect writes the design, so the
// opening of its design gate starts its own first phase on that row. The cap is full and holds
// nothing back: the promotion writes no row at all.
func TestAnOpenedGateStartsTheFirstPhaseWithNoNewRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-1", LatestVersion: 1})
	before := slots(t, pool)

	applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: record.EventMark{Seq: 4, At: reviewedAt}})

	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Planning {
		t.Fatalf("the root at its gate's opening = %#v, want its first phase started on the row it already held", got)
	}
	assertSlots(t, pool, before)
}

// A worker of a row-holding issue finishes its phase while the cap is full, and the next
// phase's start is delivered with no row free: the row the issue already holds is what releases
// that start (record.SuperviseRequest.NeedsSlot), so the line is never asked for one.
func TestAFinishedPhaseStartsTheNextOneWithNoNewRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlotted(t, pool, "LEGION-100", "A")
	capMember(t, pool, "LEGION-100", "LEGION-101", phase.Implementing, "in_progress", "A1")
	takeSlot(t, pool, "LEGION-101")
	// A root that never ran, waiting in line, so the cap is full of issues the daemon has
	// started and the line is not empty: a pass that handed out a row would hand it to this one.
	seedWaiting(t, pool, "LEGION-200", "B")

	// The implementer completes and the transition into testing owes the tester its start.
	member := issue(t, pool, "LEGION-101")
	member.Phase, member.Status, member.DispatchStatus = phase.Testing, "testing", "testing"
	putIssue(t, pool, member)
	oweMemberStart(t, pool, "LEGION-100", "LEGION-101", phase.Testing)

	tick(t, pool, admission, "tick")

	if !claimableStart(t, pool, "LEGION-101") {
		t.Fatalf("the starts of LEGION-101 = %#v, want its tester's task claimable: the row it holds releases the start",
			startsOf(t, pool, "LEGION-101"))
	}
	assertRowHolders(t, pool, "LEGION-100", "LEGION-101")
	assertWaiting(t, pool, []string{"LEGION-200"})
}

// A child the architect released holds no row, so it takes one when the daemon starts it and
// waits in the line while the project is at its cap. The pass that finds a row free starts it.
//
// The cap is one row and the second tree holds it: the root above the child stands in
// accepting, where an issue holds none (record.KeepsSlot), so the row that frees is that
// second tree's and no other.
func TestAReleasedChildWaitsForAFreeRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-1", LatestVersion: 1, ApprovedVersion: new(1)})
	acceptingRoot(t, pool, "LEGION-100")
	capMember(t, pool, "LEGION-100", "LEGION-101", phase.Admitted, "todo", "A1")
	// The second tree, whose row fills the cap and whose close frees it.
	seedSlotted(t, pool, "LEGION-200", "B")

	tick(t, pool, admission, "tick")

	if got := issue(t, pool, "LEGION-101"); got.Phase != phase.Admitted {
		t.Fatalf("the released child under a full cap = %#v, want it still at admitted", got)
	}
	assertRowHolders(t, pool, "LEGION-200")
	assertWaiting(t, pool, []string{"LEGION-101"})

	closeTree(t, pool, admission, "second-closed", "LEGION-200", "B")

	if got := issue(t, pool, "LEGION-101"); got.Phase != phase.Planning {
		t.Fatalf("the released child once a row freed = %#v, want it planning", got)
	}
	assertRowHolders(t, pool, "LEGION-101")
	assertWaiting(t, pool, nil)
}

// An issue standing in a phase that keeps a row while holding none, which is what every issue
// this release finds standing past its review becomes the moment its work comes back: no row is
// written back for it, so it waits in the line once, at its next start, and from then on it
// keeps the row it was given.
func TestAnIssueThatHoldsNoRowWaitsOnceAfterTheChange(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	seedSlotted(t, pool, "LEGION-100", "A")
	// The issue this change leaves in accepting with no row of its own.
	capMember(t, pool, "LEGION-100", "LEGION-101", phase.Accepting, "needs_review", "A1")
	seedSlotted(t, pool, "LEGION-200", "B")

	// Its architect sends the work back to implementing, where a worker writes the change
	// again, and its implementer is owed the start that waits for a row.
	member := issue(t, pool, "LEGION-101")
	member.Phase, member.Status, member.DispatchStatus = phase.Implementing, "in_progress", "in_progress"
	putIssue(t, pool, member)
	oweMemberStart(t, pool, "LEGION-100", "LEGION-101", phase.Implementing)

	tick(t, pool, admission, "cap-full")

	if claimableStart(t, pool, "LEGION-101") {
		t.Fatalf("the starts of LEGION-101 = %#v, want none claimable: it holds no row and the cap has none to give",
			startsOf(t, pool, "LEGION-101"))
	}
	assertRowHolders(t, pool, "LEGION-100", "LEGION-200")
	assertWaiting(t, pool, []string{"LEGION-101"})

	closeTree(t, pool, admission, "second-closed", "LEGION-200", "B")

	if !claimableStart(t, pool, "LEGION-101") {
		t.Fatalf("the starts of LEGION-101 = %#v, want its implementer's task claimable once a row freed",
			startsOf(t, pool, "LEGION-101"))
	}
	assertRowHolders(t, pool, "LEGION-100", "LEGION-101")

	// From then on it keeps that row: every phase up to its review keeps one.
	back := issue(t, pool, "LEGION-101")
	back.Phase, back.Status, back.DispatchStatus = phase.Reviewing, "needs_review", "needs_review"
	putIssue(t, pool, back)

	tick(t, pool, admission, "back-in-reviewing")

	assertRowHolders(t, pool, "LEGION-100", "LEGION-101")
}

// An issue in merging has a worker with work of its own: its merger merges the head it read,
// which is what ends the phase, so nothing of that phase ever waits on a person. The pass that
// sends a role the work its phase entry names therefore sends its merger that work, where before
// this release it passed over a merger whose own READY a closed design gate held (admit's
// resumeSlottedWorkers, record.AtWork). The merger runs on no row: merging stands past the
// issue's review, so the repair reaches it holding none (record.KeepsSlot).
func TestAMergingIssueStartsItsMerger(t *testing.T) {
	const root, key = "LEGION-820", "LEGION-821"
	pool := migratedPool(t)
	seedTreeAndMember(t, pool, root, key, phase.Merging, 1, false)
	// A merger whose session the daemon suspended holds no task it still works, and whose claim
	// was queued no task of the entry its phases row names, which is the difference this pass
	// closes.
	token := putWorkerClaim(t, pool, root, key, claim.RoleMerger, 1, supervise.StateSuspended, supervise.SuspendedByDaemon)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutPhase(context.Background(), tx, record.PhaseRow{Issue: key,
			Role: claim.RoleMerger, Claim: token, Entry: seededEntry, EntryTask: seededEntryTask(key),
			EntryRaisedAt: fixedNow}); err != nil {
			t.Fatalf("record the merger's phases row of %s: %v", key, err)
		}
	})

	tick(t, pool, quiet(t, 1), "tick")

	if !claimableStart(t, pool, key) {
		t.Fatalf("no merger of %s was started; starts = %#v", key, startsOf(t, pool, key))
	}
	if holdsSlot(t, pool, key) {
		t.Fatalf("rows = %#v, want %s holding none: its work stands past its review", slots(t, pool), key)
	}
}

// An issue sent back from its merge holds no row — its work stood past its review, which holds
// none — so it waits in the line. It waits among the issues whose work came back, oldest
// first, which is the line's first part, ahead of an issue whose first phase has yet to start,
// and it takes the next row that frees. The cap is one row, the second tree holds it, and the
// root above both members stands in accepting holding none.
func TestAnIssueSentBackFromItsMergeStandsAtTheFrontOfTheLine(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-1", LatestVersion: 1, ApprovedVersion: new(1)})
	acceptingRoot(t, pool, "LEGION-100")
	// The issue whose merge sent it back, holding no row, and a sibling whose first phase has
	// yet to start, which ranks ahead of it on the board.
	capMember(t, pool, "LEGION-100", "LEGION-101", phase.Implementing, "in_progress", "A1")
	oweMemberStart(t, pool, "LEGION-100", "LEGION-101", phase.Implementing)
	capMember(t, pool, "LEGION-100", "LEGION-102", phase.Admitted, "todo", "A0")
	seedSlotted(t, pool, "LEGION-200", "B")

	assertWaiting(t, pool, []string{"LEGION-101", "LEGION-102"})

	tick(t, pool, admission, "cap-full")

	assertRowHolders(t, pool, "LEGION-200")
	assertWaiting(t, pool, []string{"LEGION-101", "LEGION-102"})

	closeTree(t, pool, admission, "second-closed", "LEGION-200", "B")

	assertRowHolders(t, pool, "LEGION-101")
	if !claimableStart(t, pool, "LEGION-101") {
		t.Fatalf("the starts of LEGION-101 = %#v, want its implementer's task claimable on the row it took",
			startsOf(t, pool, "LEGION-101"))
	}
	assertWaiting(t, pool, []string{"LEGION-102"})
}

// Why the line's first part needs no term for an issue in held or in awaiting_person, though
// both keep a row now: admit's finishFencedStarts runs before the line is judged, and it ends
// an owed start whose phase is not the issue's own. An issue whose worker died in a phase the
// hold took it out of owes nothing by the time the line is judged, so the line offers it no row
// and no worker of it starts — even with a row free.
func TestAHeldIssueOwesNoStartSoTheLineOffersItNoRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)
	seedSlotted(t, pool, "LEGION-100", "A")
	// The member stands in held, where the hold put it; the start its implementer was owed names
	// the phase it stood in before, which the fence repair ends.
	capMember(t, pool, "LEGION-100", "LEGION-101", phase.Held, "in_progress", "A1")
	oweMemberStart(t, pool, "LEGION-100", "LEGION-101", phase.Implementing)
	if owed := owedStarts(t, pool, "LEGION-101"); len(owed) != 1 {
		t.Fatalf("the starts LEGION-101 owes before the pass = %#v, want the one its fixture wrote", owed)
	}

	tick(t, pool, admission, "tick")

	if owed := owedStarts(t, pool, "LEGION-101"); len(owed) != 0 {
		t.Fatalf("the starts LEGION-101 owes after the pass = %#v, want none: the fence repair ends a start whose phase the issue has moved past", owed)
	}
	if claimableStart(t, pool, "LEGION-101") {
		t.Fatalf("the starts of LEGION-101 = %#v, want no worker started for an issue standing in held", startsOf(t, pool, "LEGION-101"))
	}
	if holdsSlot(t, pool, "LEGION-101") {
		t.Fatalf("rows = %#v, want none for LEGION-101: it owes no start, so the line never offers it one", slots(t, pool))
	}
	assertWaiting(t, pool, nil)
}
