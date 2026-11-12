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
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// treeOpen says whether key's tree lifecycle stands open under the workflow's authority, which
// is what a promotion that opens a tree writes.
func treeOpen(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatal(err)
	}
	var open bool
	inTx(t, pool, func(tx pgx.Tx) {
		lifecycles, err := record.NewStore().TreeLifecycles(context.Background(), tx, project, []string{key})
		if err != nil {
			t.Fatalf("read the tree lifecycles: %v", err)
		}
		life, found := lifecycles[key]
		open = found && life.Authority == treelifecycle.AuthorityWorkflow
	})
	return open
}

// carriedRoot records key as a root another tree handed to Legion, carrying the phase it ran
// there, with a tree of its own still to be opened.
func carriedRoot(t *testing.T, pool *pgxpool.Pool, key string, carries phase.Phase, rank string) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: key, Tree: key, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root", Phase: carries,
		Generation: 2, Status: "todo", Rank: rank, HandedOver: true, HandedOverFrom: "LEGION-700"})
	seedMirror(t, pool, key, "todo")
}

// A handed-over root that carries accepting takes a row, because an issue keeps its row from
// the moment the daemon starts it until it is done, leaves the workflow or waits only for its
// merge, and accepting is none of those (record.KeepsSlot). So the cap does hold such a root
// back: its tree is opened on the pass that finds a row free, and not before.
//
// The fixture: the cap's one row is held by a tree already at work, a released child of that
// tree stands at admitted waiting for its own first phase, and the handed-over root ranks
// behind that child. Both want a row and the cap has none.
func TestAHandedOverRootCarryingAcceptingWaitsForAFreeRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)

	// The live tree that holds the cap's one row, its design gate open and its own first phase
	// behind it, with one released child whose first phase has still to start.
	seedSlotted(t, pool, "LEGION-100", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: new(1)})
	acceptingRoot(t, pool, "LEGION-100")
	putIssue(t, pool, record.Issue{Key: "LEGION-101", Tree: "LEGION-100", Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a released child", Phase: phase.Admitted,
		Generation: 1, Status: "todo", Rank: "A1", HandedOver: true})
	carriedRoot(t, pool, "LEGION-200", phase.Accepting, "B")

	// Both candidates are in the line, the child first by its tree root's rank.
	assertWaiting(t, pool, []string{"LEGION-101", "LEGION-200"})

	tick(t, pool, admission, "tick")

	if holders := slots(t, pool); len(holders) != 1 || holders[0].Issue != "LEGION-100" {
		t.Fatalf("rows = %#v, want the cap's one row still held by LEGION-100: neither candidate can be given one", holders)
	}
	if treeOpen(t, pool, "LEGION-200") {
		t.Fatalf("the tree of the handed-over root LEGION-200 is open while the cap is full: a root carrying accepting takes a row, so the cap holds it back")
	}
	if carried := issue(t, pool, "LEGION-200"); carried.Status != "todo" {
		t.Fatalf("the handed-over root's status = %q, want todo: nothing of it has been promoted", carried.Status)
	}

	// The tree holding the row closes, which frees it and takes its child out of the line. The
	// carried root is what is left, and the next pass opens its tree on that row.
	apply(t, pool, admission, "root-closed", intake.DispatchIssue{Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "LEGION-100", Rank: "A", HandedOver: true}, admission.engine)

	if !treeOpen(t, pool, "LEGION-200") {
		t.Fatalf("the tree of the handed-over root LEGION-200 is still closed after a row freed, though it stands alone in the line")
	}
	if carried := issue(t, pool, "LEGION-200"); carried.Status != "in_progress" {
		t.Fatalf("the handed-over root's status = %q, want in_progress: its promotion writes the status of a tree that runs", carried.Status)
	}
	if holders := slots(t, pool); len(holders) != 1 || holders[0].Issue != "LEGION-200" {
		t.Fatalf("rows = %#v, want the one freed row now held by the carried root", holders)
	}
}

// A handed-over root that carries awaiting_merge takes no row: an issue waiting only for its
// merge keeps none (record.KeepsSlot), and the grant reads the phase the entry carries
// (admit's slotFor), which for a root whose tree is opened is the phase its record carries.
// Holding it back would bound nothing and leave its tree closed, so it opens at once with the
// cap full, and the pass after it deletes nothing. What its promotion then starts of the run it
// carries is the opening's own business (admit's startMidPhaseMembers), and takes no row here
// either.
func TestAHandedOverRootCarryingAwaitingMergeTakesNoRow(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)

	// The cap's one row, held by a tree already at work.
	seedSlotted(t, pool, "LEGION-100", "A")
	carriedRoot(t, pool, "LEGION-200", phase.AwaitingMerge, "B")
	assertWaiting(t, pool, []string{"LEGION-200"})

	tick(t, pool, admission, "tick")

	if !treeOpen(t, pool, "LEGION-200") {
		t.Fatalf("the tree of the handed-over root LEGION-200 is not open after the pass: a candidate whose promotion leaves it in a phase that keeps no row is promoted whatever the cap")
	}
	if carried := issue(t, pool, "LEGION-200"); carried.Status != "in_progress" {
		t.Fatalf("the handed-over root = %#v, want in_progress: its promotion writes the status of a tree that runs", carried)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})

	// The pass after it leaves the rows as they are: there is no row of the carried root to
	// delete, and the cap's one row belongs to an issue that keeps it.
	tick(t, pool, admission, "tick-again")

	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
}
