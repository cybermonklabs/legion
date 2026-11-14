package admit

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// rowsOf is every outbox row queued for key, decoded, so a test can say the daemon wrote it
// nothing at all rather than nothing of one kind.
func rowsOf(t *testing.T, pool *pgxpool.Pool, key string) []effect {
	t.Helper()
	var got []effect
	for _, row := range effects(t, pool) {
		if row.issue == key {
			got = append(got, row)
		}
	}
	return got
}

// The daemon starts nothing a person stopped: C5 of dispatch://INFRA-3107. A member of a tree
// parked to backlog, a root whose record says the Legion label is off and a member of a closed
// tree no architect has released get no route out of where they stand — one admission pass writes
// no Dispatch status for any of them, moves no record of them and makes a tree root of none of
// them — and each stands in admission.held with the one word for why. A person's own act is what
// moves such an issue, which is read here as the one act C4 gives: the live tree's architect
// releases the closed tree's member, and that record moves while the other two stand.
//
// The rule for a root whose label a person took off is read as it stands today: the root leaves
// the waiting line, and the tree running under it is not stopped — its member at work keeps its
// slot, its record and every claim it holds, because taking the label off is a pause the label
// ends (admit's unslotted, which this change leaves exactly as it was).
func TestNothingAPersonStoppedIsStarted(t *testing.T) {
	const parkedTree, parkedMember = "LEGION-700", "LEGION-701"
	const labelOffRoot, labelOffMember = "LEGION-710", "LEGION-711"
	const closedTreeOfItsOwn, closedMember = "LEGION-720", "LEGION-721"
	pool := migratedPool(t)
	// Two slots, both held: the live tree's root and the member at work under the label-off root,
	// so a record this pass moved would have to take a slot that is not there.
	admission := quiet(t, 2)
	seedOpenTree(t, pool, liveTree)

	// A tree a person parked to backlog, with the member the park left behind at admitted.
	parked := closedRoot(parkedTree, 1)
	parked.Status, parked.DispatchStatus = "backlog", "backlog"
	putIssue(t, pool, parked)
	seedLifecycle(t, pool, parkedTree)
	putIssue(t, pool, treeMember(parkedMember, parkedTree, parkedTree, "B", 1))

	// A root of its own tree whose record says the Legion label is off, standing at admitted,
	// with its tree running under it: a member mid-phase, holding the slot it started on.
	putIssue(t, pool, record.Issue{Key: labelOffRoot, Repo: closedTreeRepo, DefaultBranch: closedTreeBranch, GitHubRepo: closedTreeRepo,
		Project: testProject, Title: labelOffRoot, Tree: labelOffRoot, Phase: phase.Admitted, Generation: 1,
		Status: "todo", DispatchStatus: "todo", Rank: "C", HandedOver: false, LastDispatchSeq: 1})
	seedLifecycle(t, pool, labelOffRoot)
	atWork := treeMember(labelOffMember, labelOffRoot, labelOffRoot, "C1", 1)
	atWork.Phase, atWork.Status, atWork.DispatchStatus = phase.Implementing, "in_progress", "in_progress"
	putIssue(t, pool, atWork)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(t.Context(), tx, record.Slot{Issue: labelOffMember, Index: 1, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("seed the slot of the member at work: %v", err)
		}
	})

	// A tree whose root is done with no linger left to run, holding a member at admitted that no
	// architect has released.
	putIssue(t, pool, closedRoot(closedTreeOfItsOwn, 3))
	putIssue(t, pool, treeMember(closedMember, closedTreeOfItsOwn, closedTreeOfItsOwn, "D", 3))

	stopped := []string{parkedMember, labelOffRoot, closedMember}
	before := snapshot(t, pool, append(stopped, labelOffMember)...)
	slotsBefore := slots(t, pool)
	// The lifecycle rows as they stand: the two trees that ran have one each, and the two
	// members have none. A pass that made a tree root of any of these would open one.
	lifecyclesBefore := map[string]int{}
	for _, key := range stopped {
		lifecyclesBefore[key] = lifecycleRows(t, pool, key)
	}

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})

	if got := snapshot(t, pool, append(stopped, labelOffMember)...); !reflect.DeepEqual(got, before) {
		t.Fatalf("the records after the pass = %#v, want them as they stood, %#v: the daemon starts nothing a person stopped", got, before)
	}
	for _, key := range stopped {
		if rows := rowsOf(t, pool, key); len(rows) != 0 {
			t.Fatalf("the pass queued %#v for %s, want nothing at all: no Dispatch status and no start", rows, key)
		}
		if got := lifecycleRows(t, pool, key); got != lifecyclesBefore[key] {
			t.Fatalf("%s has %d tree lifecycles, want the %d it had: the pass makes a tree root of none of them", key, got, lifecyclesBefore[key])
		}
	}
	for key, want := range map[string]record.HeldReason{
		parkedMember: record.HeldTreeParked,
		labelOffRoot: record.HeldLabelOff,
		closedMember: record.HeldTreeClosed,
	} {
		assertHeld(t, pool, admission, want, key)
	}
	// The tree running under the root whose label is off is not stopped: its member keeps the
	// record and the slot it was started on, and the only row the pass queues for it is the start
	// that brings its role back — nothing suspends it, closes its tree or writes its status.
	for _, row := range rowsOf(t, pool, labelOffMember) {
		request, start := row.payload.(record.SuperviseRequest)
		if !start || request.Op != "start" {
			t.Fatalf("the pass queued %#v for %s, want nothing that stops it: taking the label off a root stops no tree running under it", row.payload, labelOffMember)
		}
	}
	if got := slots(t, pool); !reflect.DeepEqual(got, slotsBefore) {
		t.Fatalf("slots after the pass = %#v, want %#v", got, slotsBefore)
	}
	assertWaiting(t, pool, nil)

	// The one act that moves any of them: the live tree's architect takes the closed tree's
	// member. The two a person stopped stand where they stood.
	if refusal := releaseInto(t, pool, admission, "release", liveTree,
		intake.AdoptedChild{Issue: closedMember, Parent: liveTree}); refusal != nil {
		t.Fatalf("the release of %s = %#v, want it accepted", closedMember, refusal)
	}
	if got := issue(t, pool, closedMember); got.Tree != liveTree || got.Phase != phase.Admitted || got.Generation != before[closedMember].Generation+1 {
		t.Fatalf("%s = %#v, want it admitted in the tree of %s at generation %d", closedMember, got, liveTree, before[closedMember].Generation+1)
	}
	for _, key := range []string{parkedMember, labelOffRoot, labelOffMember} {
		if got := issue(t, pool, key); !reflect.DeepEqual(got, before[key]) {
			t.Fatalf("%s after the release = %#v, want it as it stood, %#v", key, got, before[key])
		}
	}
	assertHeld(t, pool, admission, record.HeldTreeParked, parkedMember)
	assertHeld(t, pool, admission, record.HeldLabelOff, labelOffRoot)
	if writes := dispatchWrites(t, pool); writes != 0 {
		t.Fatalf("Dispatch writes = %d, want none: C5 writes no status for a held issue, and the adoption writes the record alone", writes)
	}
}
