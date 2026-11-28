package admit

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// rootsNextChange raises root's own generation as its next change does, and leaves its tree's run
// where it stands: PutIssue writes a record of the same tree with the tree_generation the store
// holds. It returns the root as the store reads it back, its architect still on the tree's run
// (record.Issue.ArchitectGeneration).
func rootsNextChange(t *testing.T, pool *pgxpool.Pool, root string) record.Issue {
	t.Helper()
	before := readRoot(t, pool, root)
	raised := before
	raised.Generation++
	putIssue(t, pool, raised)
	after := readRoot(t, pool, root)
	if after.Generation != before.Generation+1 || after.ArchitectGeneration() != before.ArchitectGeneration() {
		t.Fatalf("%s after its next change stands at generation %d with its architect on run %d, want generation %d on run %d",
			root, after.Generation, after.ArchitectGeneration(), before.Generation+1, before.ArchitectGeneration())
	}
	return after
}

// readRoot is root's record as the store holds it.
func readRoot(t *testing.T, pool *pgxpool.Pool, root string) record.Issue {
	t.Helper()
	var issue *record.Issue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if issue, err = record.NewStore().Issue(context.Background(), tx, root); err != nil {
			t.Fatalf("read %s: %v", root, err)
		}
	})
	if issue == nil {
		t.Fatalf("%s is not recorded", root)
	}
	return *issue
}

// queueRootArchitect queues request, a supervise row of root's architect, on root.
func queueRootArchitect(t *testing.T, pool *pgxpool.Pool, root string, request record.SuperviseRequest) {
	t.Helper()
	request.Tree, request.Role = root, claim.RoleArchitect
	inTx(t, pool, func(tx pgx.Tx) {
		row, err := record.NewOutboxRow(root, request, fixedNow)
		if err != nil {
			t.Fatalf("build the %s of the architect of %s at generation %d: %v", request.Op, root, request.Generation, err)
		}
		if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
			t.Fatalf("queue the %s of the architect of %s at generation %d: %v", request.Op, root, request.Generation, err)
		}
	})
}

// treeRunStart is the start of a root's architect of the entry its phases row names, on run, as
// the sweep's send writes it (workflow.Engine.SendArchitectStart).
func treeRunStart(root string, run uint64) record.SuperviseRequest {
	return record.SuperviseRequest{Op: "start", Generation: run, Task: seededArchitectTask(root), Entry: seededEntry}
}

// strandedNow is what the boot's report of the claims nothing will wake names now
// (workflow.Stranded).
func strandedNow(t *testing.T, pool *pgxpool.Pool) []supervise.Claim {
	t.Helper()
	claims := storedClaims(t, pool)
	var named []supervise.Claim
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if named, err = workflow.Stranded(context.Background(), tx, record.NewStore(), testProject, nil, claims, nil, fixedNow); err != nil {
			t.Fatalf("read the stranded claims: %v", err)
		}
	})
	return named
}

// startsAt names each architect start by the generation it serves and the entry it carries.
func startsAt(starts []record.SuperviseRequest) []string {
	named := make([]string, len(starts))
	for i, start := range starts {
		named[i] = fmt.Sprintf("generation %d, entry %d", start.Generation, start.Entry)
	}
	return named
}

// A root's next change raises the root's own generation and leaves its tree's run, which its
// architect runs on, so the sweep owes that architect no start for it. The sweep sends a root's
// architect the entry its phases row names only where no start of the role is queued
// (workflow.OwedEntry, ArchitectStartPending), and it reads the queued starts from the architect's
// run (readSlotHolders, record.Store.RoleRuns). A start of the tree's run queued before the change
// and not yet run is such a start, and the sweep writes none beside it.
func TestTheSweepWritesNoSecondStartForARootArchitectWhoseStartOfTheTreesRunIsQueuedAfterItsRootsNextChange(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 1)
	const root = "ACME-1"
	fixture := architectWakeRoot(root)
	fixture.generation, fixture.claimed = 1, false
	seedArchitectWakeRoot(t, pool, fixture)
	queueRootArchitect(t, pool, root, treeRunStart(root, 1))
	raised := rootsNextChange(t, pool, root)

	applySweep(t, pool, admission, "after-the-next-change")
	if starts := architectStartsOf(t, pool, root); len(starts) != 1 {
		t.Fatalf("%s holds the architect starts %v after the sweep. Want only the one already queued on the tree's run %d. "+
			"The root stands at generation %d, and a run read at that generation misses the queued start",
			root, startsAt(starts), raised.ArchitectGeneration(), raised.Generation)
	}
}

// The boot's report of the claims nothing will wake leaves out a failed root architect whose start
// is queued: StartFor counts it, from the architect's run (workflow.Stranded, record.Store.RoleRuns).
// After the root's next change that run is still the tree's run, so a start of it queued before
// the change is the start that wakes the claim, and the report names nothing.
func TestStrandedLeavesOutAFailedRootArchitectWhoseStartOfTheTreesRunIsQueuedAfterItsRootsNextChange(t *testing.T) {
	pool := migratedPool(t)
	const root = "ACME-2"
	fixture := architectWakeRoot(root)
	fixture.generation, fixture.state, fixture.task = 1, supervise.StateFailed, false
	seedArchitectWakeRoot(t, pool, fixture)
	queueRootArchitect(t, pool, root, treeRunStart(root, 1))
	raised := rootsNextChange(t, pool, root)

	if named := strandedNow(t, pool); len(named) != 0 {
		t.Fatalf("Stranded names %+v. Want nothing: the start of the tree's run %d is queued. "+
			"The root stands at generation %d, and a run read at that generation misses the queued start",
			named, raised.ArchitectGeneration(), raised.Generation)
	}
}

// A stop of a root's architect serves the root's own generation (the outbox's supervise fence
// holds every supervise row but an architect's start to it), and one queued after a start undoes
// that start (workflow's actingStop). So the run of a root's architect after the root's next change
// holds its starts of the tree's run and its stops of the root's own generation together. A failed
// architect whose queued start a newer suspend undoes is one nothing will wake, and the report
// names it.
func TestStrandedNamesAFailedRootArchitectWhoseStartOfTheTreesRunAStopOfTheRootsGenerationUndoes(t *testing.T) {
	pool := migratedPool(t)
	const root = "ACME-3"
	fixture := architectWakeRoot(root)
	fixture.generation, fixture.state, fixture.task = 1, supervise.StateFailed, false
	seedArchitectWakeRoot(t, pool, fixture)
	queueRootArchitect(t, pool, root, treeRunStart(root, 1))
	raised := rootsNextChange(t, pool, root)
	queueRootArchitect(t, pool, root, record.SuperviseRequest{Op: "suspend", Generation: raised.Generation,
		Reason: "the root's own run stops its architect"})

	named := strandedNow(t, pool)
	if token := architectToken(t, root); len(named) != 1 || named[0].Token != token {
		t.Fatalf("Stranded names %+v. Want the failed architect %s: the suspend of the root's generation %d undoes the start of the tree's run %d queued before it",
			named, token, raised.Generation, raised.ArchitectGeneration())
	}
}
