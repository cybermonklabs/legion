package workflow

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// oneStartIssue is the issue this file's transition stands on.
const oneStartIssue = "LEGION-208"

// oneStartRow is one supervise start row of an issue: what it asks for, and whether a lane may
// run it at the moment it is read (record.Store.DueRows, the one predicate the outbox claims by).
type oneStartRow struct {
	id        int64
	role      claim.Role
	task      string
	stands    phase.Phase
	needsSlot bool
	claimable bool
}

// oneStartRowsOf is every supervise start row of issue, oldest first, each read against the
// outbox's own due predicate at now. It reads the table directly rather than through any helper
// of this package's other start tests, so what it reports is the rows as the transition left
// them.
func oneStartRowsOf(t *testing.T, pool *pgxpool.Pool, issue string, now time.Time) []oneStartRow {
	t.Helper()
	var rows []oneStartRow
	seedRecord(t, pool, func(tx pgx.Tx) error {
		due, err := record.NewStore().DueRows(t.Context(), tx, "LEGION", issue, []record.OutboxKind{record.OutboxKindSupervise}, now)
		if err != nil {
			return err
		}
		claimable := make(map[int64]struct{}, len(due))
		for _, row := range due {
			claimable[row.ID] = struct{}{}
		}
		all, err := tx.Query(t.Context(), `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce(payload->>'phase', ''), coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			return err
		}
		defer all.Close()
		for all.Next() {
			var row oneStartRow
			if err := all.Scan(&row.id, &row.role, &row.task, &row.stands, &row.needsSlot); err != nil {
				return err
			}
			_, row.claimable = claimable[row.id]
			rows = append(rows, row)
		}
		return all.Err()
	})
	return rows
}

// A start of a phase worker is one outbox row, and that row carries the worker's task.
//
// The planner's completion gives the implementer its first task of the run, so every start row
// the issue holds afterwards is that start. The row names the phase the task is for, waits for
// the issue's admission slot while the issue holds none, and is the one row a lane may run once
// a slot names the issue: the worker comes back with its work rather than ahead of it.
//
// The assertion that shows the behaviour before this change is the first one, that the issue
// holds exactly one start row: the start used to travel as two (StartOrOwe), a task-less
// relaunch row that was claimable the moment it was written and a second row that carried the
// task and waited for the slot. The third assertion reads the same fault from the other side —
// with no slot for the issue, the task-less row of the pair was claimable, so the implementer's
// process started, and its whole first turn could run with no task to do (INFRA-1573).
func TestAStartIsOneRowThatCarriesItsTask(t *testing.T) {
	pool := migratedPool(t)
	seedIssue(t, pool, record.Issue{Key: oneStartIssue, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: oneStartIssue, Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "U"})
	seedTreeLifecycle(t, pool, oneStartIssue)
	seedPhase(t, pool, record.PhaseRow{Issue: oneStartIssue, Role: claim.RolePlanner, Claim: "plan-claim"})
	engine := testEngine(config.DesignGateRootIssues, nil)
	now := engine.now()

	applyRefusingNothing(t, pool, engine, intake.HandoffComplete{Generation: 1, Issue: oneStartIssue, Role: claim.RolePlanner,
		Claim: "plan-claim", HandoffRecord: "plan", HandoffVersion: 1})

	starts := oneStartRowsOf(t, pool, oneStartIssue, now)
	if len(starts) != 1 {
		t.Fatalf("the start rows of %s = %+v, want exactly one: a phase worker's start is one row", oneStartIssue, starts)
	}
	start := starts[0]
	if start.role != claim.RoleImplementer || start.task == "" || start.stands != phase.Implementing {
		t.Fatalf("the start row = %+v, want the implementer's, carrying a task and the %s phase it is for", start, phase.Implementing)
	}
	if !start.needsSlot || start.claimable {
		t.Fatalf("the start row = %+v, want it held for the issue's admission slot: the cap counts the issues at work, and this row puts a worker to work", start)
	}

	seedSlot(t, pool, record.Slot{Issue: oneStartIssue, Index: 0, AdmittedAt: now})

	after := oneStartRowsOf(t, pool, oneStartIssue, now)
	if len(after) != 1 || after[0].id != start.id || !after[0].claimable {
		t.Fatalf("the start rows once a slot names %s = %+v, want the one row %d, now runnable: the slot is the whole of what it waited for",
			oneStartIssue, after, start.id)
	}
	if after[0].task != start.task {
		t.Fatalf("the runnable row carries task %q, want the %q it was written with", after[0].task, start.task)
	}
}
