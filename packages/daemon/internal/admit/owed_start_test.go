package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// midPhaseStart is one supervise start row of an issue, with whether a lane may run it now
// (record.Store.DueRows, the predicate the outbox claims by).
type midPhaseStart struct {
	role      claim.Role
	task      string
	needsSlot bool
	claimable bool
}

// startsOf is every supervise start row issue holds, oldest first.
func startsOf(t *testing.T, pool *pgxpool.Pool, issue string) []midPhaseStart {
	t.Helper()
	var rows []midPhaseStart
	inTx(t, pool, func(tx pgx.Tx) {
		ctx := context.Background()
		due, err := record.NewStore().DueRows(ctx, tx, testProject, issue, []record.OutboxKind{record.OutboxKindSupervise}, fixedNow)
		if err != nil {
			t.Fatalf("list what a lane may run for %s: %v", issue, err)
		}
		runnable := make(map[int64]struct{}, len(due))
		for _, row := range due {
			runnable[row.ID] = struct{}{}
		}
		all, err := tx.Query(ctx, `select id, coalesce(payload->>'role', ''), coalesce(payload->>'task', ''),
			coalesce(payload->>'needsSlot', '') = 'true'
			from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' order by id`, issue)
		if err != nil {
			t.Fatalf("list the starts of %s: %v", issue, err)
		}
		defer all.Close()
		for all.Next() {
			var id int64
			var row midPhaseStart
			if err := all.Scan(&id, &row.role, &row.task, &row.needsSlot); err != nil {
				t.Fatalf("scan a start of %s: %v", issue, err)
			}
			_, row.claimable = runnable[id]
			rows = append(rows, row)
		}
		if err := all.Err(); err != nil {
			t.Fatalf("read the starts of %s: %v", issue, err)
		}
	})
	return rows
}

// entryOf is the phase entry role's phases row of issue is on and the work that entry names.
func entryOf(t *testing.T, pool *pgxpool.Pool, issue string, role claim.Role) (int64, string) {
	t.Helper()
	var entry int64
	var task string
	if err := pool.QueryRow(context.Background(), `select entry, entry_task from phases where issue = $1 and role = $2`,
		issue, string(role)).Scan(&entry, &task); err != nil {
		t.Fatalf("read the %s's entry on %s: %v", role, issue, err)
	}
	return entry, task
}
