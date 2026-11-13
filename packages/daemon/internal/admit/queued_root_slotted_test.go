package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root another tree handed to Legion, holding the row it took in the tree it left, stands in
// the waiting line and its promotion opens the tree of its own, taking no second row.
//
// The line drops a queued root that holds a row (record.Waiting's second loop), because a root
// the daemon has already started and a person has set back to todo is a queued root too, and
// promoting it again would start a second architect. The carried root is the other half of that
// branch and must not be dropped: such a root keeps the row it carries until it is done or
// leaves the workflow (record.KeepsSlot), even while it waits on another issue (the release for
// a wait leaves a queued root out), so a root arriving from another tree in accepting carries the
// row of the tree it left while the tree of its own has still to be opened. Dropped from the
// line it is promoted by nothing, and the release keeps its row (record.NeedsSlot is true of it,
// since a queued root makes a live tree of its own), and the repair of a row whose worker has no
// task passes it over (it reads a running phase, and this one stands in todo at accepting with
// no worker of its own). So it would wait on this tick and on every tick after it, with nothing
// to end the wait.
//
// The cap is 2 and the one row held is this root's own, so a free row stands throughout: a pass
// that leaves the tree closed cannot be the cap doing it.
func TestACarriedRootHoldingItsRowStandsInTheLineAndOpensItsTree(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const carried = "LEGION-200"

	carriedRoot(t, pool, carried, phase.Accepting, "B")
	takeSlot(t, pool, carried)

	assertWaiting(t, pool, []string{carried})

	tick(t, pool, admission, "tick")

	if !treeOpen(t, pool, carried) {
		t.Fatalf("the tree of the carried root %s is closed after a pass with a row free: a root holding the row of the tree it "+
			"left is in the line, and its promotion opens the tree of its own", carried)
	}
	if got := issue(t, pool, carried); got.Status != "in_progress" {
		t.Fatalf("the carried root = %#v, want in_progress: its promotion writes the status of a tree that runs", got)
	}
	// Its promotion takes no second row: it holds one already (admit's slotFor).
	assertSlots(t, pool, []record.Slot{{Issue: carried, Index: 0, AdmittedAt: fixedNow}})

	// The pass after it leaves everything as it stands: the promotion cleared the mark that said
	// the root arrived from another tree, so it queues no more, and its one row is a row it keeps.
	tick(t, pool, admission, "tick-again")

	assertSlots(t, pool, []record.Slot{{Issue: carried, Index: 0, AdmittedAt: fixedNow}})
	if got := issue(t, pool, carried); got.Status != "in_progress" || got.HandedOverFrom != "" {
		t.Fatalf("the carried root = %#v after a second pass, want it still in_progress with the mark of its arrival cleared", got)
	}
	assertWaiting(t, pool, nil)
}

// A root the daemon has already started, which a person set back to todo, stays out of the
// waiting line and is promoted no second time.
//
// It is a queued root by the record — it stands at admitted in todo, its design gate has never
// opened and it carries no run — and only the row it holds keeps it out of the line. Its
// architect is writing the spec, and a second promotion would open an open tree and start a
// second architect. The one mark that tells it from a root that arrived from another tree is
// HandedOverFrom, which is empty here, so the line's skip of a queued root holding a row reads
// that mark and this root keeps the skip.
func TestAStartedRootSetBackToTodoHoldingItsRowIsNotPromotedAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const root = "LEGION-300"

	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a root the daemon started", Phase: phase.Admitted,
		Generation: 1, Status: "todo", Rank: "A", HandedOver: true})
	seedMirror(t, pool, root, "todo")
	seedLifecycle(t, pool, root)
	takeSlot(t, pool, root)

	assertWaiting(t, pool, nil)

	tick(t, pool, admission, "tick")

	assertWaiting(t, pool, nil)
	got := issue(t, pool, root)
	if got.Status != "todo" || got.Phase != phase.Admitted {
		t.Fatalf("the started root = %#v after the pass, want it left at admitted in todo: its architect is writing the spec, "+
			"and nothing of this pass promotes a root the daemon has already started", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root, Index: 0, AdmittedAt: fixedNow}})
	var architectStarts int
	for _, queued := range effects(t, pool) {
		request, ok := queued.payload.(record.SuperviseRequest)
		if ok && queued.issue == root && request.Op == "start" && request.Role == claim.RoleArchitect {
			architectStarts++
		}
	}
	if architectStarts != 0 {
		t.Fatalf("the pass queued %d architect starts for %s, want none: its architect is already writing the spec, and a second "+
			"start would put two of them on one root", architectStarts, root)
	}
}
