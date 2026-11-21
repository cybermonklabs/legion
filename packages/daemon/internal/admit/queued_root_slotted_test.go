package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root a hand-over of the release before carried in, in a phase short of its review, stands in
// the waiting line by its own facts (record.CarriedIn: its carried generation is the one it stands
// at), and its promotion takes a row, writes it in progress and starts the worker of the phase it
// carries at a new entry (workflow.Engine.StartCarriedRun). A person's todo on it afterwards
// queues nothing: it holds its row, and a root holding a row is never a queued root
// (record.AwaitsPromotion).
func TestACarriedRootIsStartedInItsPhaseAndATodoAfterwardsQueuesNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	const carried = "LEGION-200"

	carriedRoot(t, pool, carried, phase.Implementing, "B")

	assertWaiting(t, pool, []string{carried})

	tick(t, pool, admission, "tick")

	if !issueOpen(t, pool, carried) {
		t.Fatalf("the carried root %s holds no open lifecycle after a pass with a row free: its promotion starts it", carried)
	}
	if got := issue(t, pool, carried); got.Status != "in_progress" {
		t.Fatalf("the carried root = %#v, want in_progress: its promotion writes the status of an issue at work", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: carried, Index: 0, AdmittedAt: fixedNow}})
	if starts := phaseStarts(t, pool, carried, claim.RoleImplementer, phase.Implementing); starts != 1 {
		t.Fatalf("the promotion queued %d starts of the implementer of %s, want the one that resumes the phase it carries", starts, carried)
	}
	if starts := architectStarts(t, pool, carried); starts != 1 {
		t.Fatalf("the promotion queued %d starts of the architect of %s, want 1", starts, carried)
	}

	putIssue(t, pool, func() record.Issue {
		got := issue(t, pool, carried)
		got.Status, got.DispatchStatus = "todo", "todo"
		return got
	}())
	tick(t, pool, admission, "tick-after-todo")

	assertWaiting(t, pool, nil)
	if starts := architectStarts(t, pool, carried); starts != 1 {
		t.Fatalf("a person's todo on the started carried root queued %d architect starts in all, want the promotion's one alone", starts)
	}
}

// A root a hand-over of the release before carried in, in a phase past its review, holds no row:
// no pass grants one in a phase that keeps none (record.KeepsSlot). It is a queued root by its own
// facts (record.CarriedIn), and its promotion starts it whatever the cap says, which is the whole of
// what the rule costs such a root.
func TestACarriedRootPastItsReviewHoldsNoRowAndStillOpensItsTree(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const carried = "LEGION-200"

	// The cap's one row, held by a tree already at work, so no row is free for the carried root.
	seedSlotted(t, pool, "LEGION-100", "A")
	carriedRoot(t, pool, carried, phase.Accepting, "B")

	assertWaiting(t, pool, []string{carried})

	tick(t, pool, admission, "tick")

	if !issueOpen(t, pool, carried) {
		t.Fatalf("the tree of the carried root %s is closed after the pass: a candidate standing past its review takes no row, "+
			"so the cap holds it back for nothing", carried)
	}
	if got := issue(t, pool, carried); got.Status != "in_progress" {
		t.Fatalf("the carried root = %#v, want in_progress: its promotion writes the status of a tree that runs", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	assertWaiting(t, pool, nil)
}

// A root the daemon has already started, which a person set back to todo, stays out of the
// waiting line and is promoted no second time.
//
// It is a queued root by the record — it stands at admitted in todo, its design gate has never
// opened and it carries no run — and only the row it holds keeps it out of the line. Its
// architect is writing the spec, and a second promotion would start a second architect. A root
// that holds a row is never a queued root (record.AwaitsPromotion).
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
