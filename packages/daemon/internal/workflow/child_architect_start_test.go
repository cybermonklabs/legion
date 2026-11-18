package workflow

import (
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// heldRow is one row the outbox holds, decoded, with the issue it is queued on.
type heldRow struct {
	issue   string
	payload record.OutboxPayload
}

// queuedRows is every row the outbox holds, decoded, in the order they were written.
func queuedRows(t *testing.T, pool *pgxpool.Pool) []heldRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select id, kind, issue, payload from outbox order by id")
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var queued []heldRow
	for rows.Next() {
		var row record.OutboxRow
		if err := rows.Scan(&row.ID, &row.Kind, &row.Issue, &row.Payload); err != nil {
			t.Fatalf("scan an outbox row: %v", err)
		}
		payload, err := record.DecodeOutboxPayload(row)
		if err != nil {
			t.Fatalf("decode outbox row %d: %v", row.ID, err)
		}
		queued = append(queued, heldRow{issue: row.Issue, payload: payload})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the outbox: %v", err)
	}
	return queued
}

// startsOf is the index in queued of every start of role, in order.
func startsOf(queued []heldRow, role claim.Role) []int {
	var at []int
	for i, row := range queued {
		if start, ok := row.payload.(record.SuperviseRequest); ok && start.Op == "start" && start.Role == role {
			at = append(at, i)
		}
	}
	return at
}

// branchRowsOf is the index in queued of every row that creates an issue's branch, in order.
func branchRowsOf(queued []heldRow) []int {
	var at []int
	for i, row := range queued {
		if _, ok := row.payload.(record.IssueBranch); ok {
			at = append(at, i)
		}
	}
	return at
}

// A child's first phase start, which admission's promotion makes when the waiting line gives the
// child its slot (Engine.StartFirstPhase), starts the child's own architect beside its planner. The
// outbox holds, on the child and in this order, the row that creates the child's branch, the
// start of the child's architect, and the planner's start, so the outbox, which runs an issue's
// starts oldest first, starts the architect before the worker it owns, and both after the branch
// exists. The architect's start is the plain one (childArchitectStart): no phase, no task, no
// entry and no slot mark, so it waits for no slot. A root's first phase start starts no architect:
// admission started the root's with its tree.
func TestAChildsFirstPhaseStartQueuesItsArchitectBeforeItsPlanner(t *testing.T) {
	t.Run("a child", func(t *testing.T) {
		pool := migratedPool(t)
		seedHeldTree(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: gateDoc, LatestVersion: 3, ApprovedVersion: new(3)})
		seedAdmittedChild(t, pool, "LEGION-209")

		admissionPass(t, pool, testEngine(config.DesignGateRootIssues, nil))
		if got := phaseOf(t, pool, "LEGION-209"); got != phase.Planning {
			t.Fatalf("the child = %s after the line reached it, want planning", got)
		}
		queued := queuedRows(t, pool)
		for _, row := range queued {
			if row.issue != "LEGION-209" {
				t.Fatalf("the first phase start queued %#v on %s; want every row on the child LEGION-209", row.payload, row.issue)
			}
		}
		branch, architect, planner := branchRowsOf(queued), startsOf(queued, claim.RoleArchitect), startsOf(queued, claim.RolePlanner)
		if len(branch) != 1 || len(architect) != 1 || len(planner) != 1 {
			t.Fatalf("the outbox holds branch rows at %v, architect starts at %v and planner starts at %v of %#v; want one of each",
				branch, architect, planner, queued)
		}
		if !(branch[0] < architect[0] && architect[0] < planner[0]) {
			t.Fatalf("the branch row is row %d, the architect's start row %d and the planner's start row %d of %#v; "+
				"want the branch row, then the architect's start, then the planner's", branch[0], architect[0], planner[0], queued)
		}
		want := record.SuperviseRequest{Op: "start", Tree: "LEGION-208", Role: claim.RoleArchitect, Generation: 4}
		if got := queued[architect[0]].payload; !reflect.DeepEqual(got, want) {
			t.Fatalf("the child's architect start = %#v; want the plain start %#v, with no phase, task, entry or slot mark", got, want)
		}
		if got := queued[planner[0]].payload.(record.SuperviseRequest); got.Phase != phase.Planning || got.Task == "" || !got.NeedsSlot {
			t.Fatalf("the planner's start = %#v; want the planning task, waiting for the child's slot", got)
		}
	})

	t.Run("a root", func(t *testing.T) {
		pool := migratedPool(t)
		seedGatedTree(t, pool)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4)})

		admissionPass(t, pool, testEngine(config.DesignGateRootIssues, nil))
		if got := phaseOf(t, pool, gatedRoot); got != phase.Planning {
			t.Fatalf("the root = %s once its gate is open, want planning", got)
		}
		queued := queuedRows(t, pool)
		if got := startsOf(queued, claim.RoleArchitect); len(got) != 0 {
			t.Fatalf("the root's first phase start queued architect starts at %v of %#v; want none", got, queued)
		}
		if branch, planner := branchRowsOf(queued), startsOf(queued, claim.RolePlanner); len(branch) != 1 || len(planner) != 1 || branch[0] > planner[0] {
			t.Fatalf("the outbox holds branch rows at %v and planner starts at %v of %#v; want the root's branch row, then its planner's start",
				branch, planner, queued)
		}
	})
}
