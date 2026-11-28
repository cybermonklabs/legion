package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
)

// A status row the daemon queues after another of the same issue still finds the board where the
// daemon last saw it when the row before it never runs. Two moves of an issue are applied in facts
// of their own before the outbox runs: implementing to testing, and then, at a new phase entry, to
// reviewing. Each is counted on the record (R16 of dispatch://INFRA-3320) and queues its status
// row through record.EnqueueFenced, as every writer of a row queues one, while the record still
// sees Dispatch at in_progress. The outbox then runs the rows oldest first. The first row is of
// the phase entry before and finishes unwritten (record.LateAt), so the board still shows
// in_progress: neither the testing the second row observed nor the needs_review it writes, but the
// status its record last saw Dispatch show when it was queued (record.StatusWrite.DispatchStatus),
// so the second row writes. A move of the board that no event the daemon applied carried, a todo
// here, is one made after the rows were queued, and both leave it standing. The control runs the
// first row at its own entry, before the second move: it writes testing, and the second row writes
// needs_review over it.
func TestAStatusRowQueuedAfterOneTheEntryFenceRefusesStillWrites(t *testing.T) {
	t.Parallel()
	const key = "LEGION-301"
	for _, tc := range []struct {
		name string
		// firstRunsFirst runs the status rows between the two moves, so the first runs at its own
		// phase entry; otherwise both run after the second move.
		firstRunsFirst bool
		// unseen is a status the board is moved to before the rows run that no event the daemon
		// applied carried, "" for none.
		unseen string
		// want is the status the board shows once every row has run.
		want string
	}{
		{name: "the first row is refused at the second move's entry, and the second writes", want: "needs_review"},
		{name: "a move no event the daemon applied carried stands", unseen: "todo", want: "todo"},
		{name: "control: the first row runs at its own entry before the second move", firstRunsFirst: true, want: "needs_review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				return records.PutIssue(ctx, tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Tree: key, Project: "LEGION", Title: "issue", Phase: phase.Implementing, Generation: 1, Status: "in_progress",
					DispatchStatus: "in_progress", Rank: "A", LastDispatchSeq: 1, HandedOver: true})
			}); err != nil {
				t.Fatalf("seed %s: %v", key, err)
			}
			board := &boardDispatch{status: "in_progress"}
			runner := &outbox{pool: pool, records: records, dispatch: board, dispatchProject: "LEGION", log: quietLogger()}
			// move applies, in a fact of its own, the issue's move to the phase at status, counted
			// on the record, and queues its status row over the status the record counted before
			// it. A role given entry raises the issue's phase entry by starting that role there.
			move := func(id string, to phase.Phase, status string, role claim.Role, entry int64) {
				t.Helper()
				moved := applyFunc(func(ctx context.Context, tx pgx.Tx, _ intake.Fact) (intake.Result, error) {
					issue, err := records.Issue(ctx, tx, key)
					if err != nil {
						return intake.Result{}, err
					}
					if role != "" {
						if err := records.PutPhase(ctx, tx, record.PhaseRow{Issue: key, Role: role, Entry: entry, EntryRaisedAt: now}); err != nil {
							return intake.Result{}, err
						}
					}
					counted := issue.Status
					issue.Phase, issue.Status = to, status
					if err := records.PutIssue(ctx, tx, *issue); err != nil {
						return intake.Result{}, err
					}
					return intake.Result{}, record.EnqueueFenced(ctx, tx, records, key, record.OrdinaryStatusWrite(status, counted, ""), now)
				})
				if result, err := intake.ApplyFact(ctx, pool, "test", id, intake.ControllerTick{}, moved); err != nil || result.Refusal != nil {
					t.Fatalf("apply %s = %+v, %v; want it taken", id, result.Refusal, err)
				}
			}
			// writeStatuses runs each status row of the issue the outbox holds, oldest first, and
			// finishes it, as the outbox's status lane does.
			writeStatuses := func() {
				t.Helper()
				var rows []record.OutboxRow
				if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					rows, err = records.PendingStatusWrites(ctx, tx, "LEGION")
					return err
				}); err != nil {
					t.Fatalf("read the status rows: %v", err)
				}
				for _, row := range rows {
					if err := runner.execute(ctx, row); err != nil {
						t.Fatalf("run the status row %d of %s: %v", row.ID, key, err)
					}
					if _, err := pool.Exec(ctx, "delete from outbox where id = $1", row.ID); err != nil {
						t.Fatalf("finish the status row %d: %v", row.ID, err)
					}
				}
			}

			move("to-testing", phase.Testing, "testing", claim.RoleTester, 1)
			if tc.firstRunsFirst {
				writeStatuses()
				if board.status != "testing" {
					t.Fatalf("Dispatch reads %s as %q once the first row ran at its own entry, want testing", key, board.status)
				}
			}
			move("to-reviewing", phase.Reviewing, "needs_review", claim.RoleReviewer, 1)
			var payloads [][]byte
			if err := store.Transact(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, "select payload from outbox where kind = 'dispatch_status' and issue = $1 order by id", key)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var payload []byte
					if err := rows.Scan(&payload); err != nil {
						return err
					}
					payloads = append(payloads, payload)
				}
				return rows.Err()
			}); err != nil {
				t.Fatalf("read the status rows of %s: %v", key, err)
			}
			var last record.StatusWrite
			if len(payloads) > 0 {
				if err := json.Unmarshal(payloads[len(payloads)-1], &last); err != nil {
					t.Fatalf("decode the last status row of %s: %v", key, err)
				}
			}
			if last.Status != "needs_review" || last.ObservedStatus != "testing" || last.DispatchStatus != "in_progress" {
				t.Fatalf("the last status row of %s = %+v, want needs_review over the testing its record counted, queued while the record saw Dispatch at in_progress", key, last)
			}
			if tc.unseen != "" {
				board.status = tc.unseen
			}
			writeStatuses()
			if board.status != tc.want {
				t.Fatalf("Dispatch reads %s as %q once every status row ran, want %q", key, board.status, tc.want)
			}
		})
	}
}
