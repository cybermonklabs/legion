package record

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// owedStartRow is a worker start that waits for its issue's admission slot, as a transition
// writes one (workflow.StartOrOwe): the row the waiting line orders on and the repair finishes.
func owedStartRow(t *testing.T, issue string, task string) OutboxRow {
	t.Helper()
	row, err := NewOutboxRow(issue, SuperviseRequest{Op: "start", Tree: issue, Role: claim.RoleImplementer,
		Phase: phase.Implementing, Generation: 1, Task: task, NeedsSlot: true}, time.Now().UTC())
	if err != nil {
		t.Fatalf("build the owed start of %s: %v", issue, err)
	}
	return row
}

// A lease that has lapsed is not a lane holding the row. The outbox's own due predicate reads it
// that way — a row is due again once lease_until has passed — and only a finished delivery takes
// the row away, so a start whose lane died is still the start its issue is owed.
//
// Both readers of that row have to agree with the outbox about it. record.Store.OwedStarts is
// what puts the issue in the waiting line's first part, and record.Store.EnqueueStart is what
// takes the row out when a second transition before the slot writes its own start in its place;
// a row neither of them can see is a start nothing delivers and nothing takes out: the issue
// stands in no part of the line, and admission's repair (finishFencedStarts) walks the same
// read.
func TestALapsedLeaseLeavesTheStartOwedAndItsRowTakenOutByTheNextStart(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-208"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
		must(t, records.Enqueue(ctx, tx, owedStartRow(t, issue, "Implement the change.")))
		if _, err := tx.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() - interval '1 hour'
			where issue = $1 and kind = 'supervise'`, issue); err != nil {
			t.Fatalf("lapse the lease: %v", err)
		}

		owed, err := records.OwedStarts(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("list the owed starts: %v", err)
		}
		if len(owed) != 1 || owed[0].Issue != issue {
			t.Fatalf("the starts owed = %#v, want the one whose lease has lapsed: no lane holds it, so its issue waits in the line", owed)
		}

		must(t, records.EnqueueStart(ctx, tx, owedStartRow(t, issue, "Implement the change again.")))
		var tasks []string
		if err := tx.QueryRow(ctx, `select array_agg(payload->>'task' order by created_at, id) from outbox
			where issue = $1 and kind = 'supervise'`, issue).Scan(&tasks); err != nil {
			t.Fatalf("read the owed tasks: %v", err)
		}
		if want := []string{"Implement the change again."}; !slices.Equal(tasks, want) {
			t.Fatalf("the tasks the issue owes = %q, want %q: EnqueueStart left the lapsed row alone, and a second transition before the slot writes the task the worker will be given in its place",
				tasks, want)
		}
	})
}

// The other half of the same rule: a row a lane takes while the start is written must keep the
// task the lane is about to deliver. EnqueueStart picks the row in a sub-select, which reads the
// row as it stood before the lane's claim, and so tests the lease again in the delete's own
// where clause: without that test the delete would land on a row the lane now holds, the lane
// would deliver the earlier task while the phases row already names the later entry, and the
// worker's completion of it would be refused.
func TestEnqueueStartLeavesARowALaneTakesWhileItRuns(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-208"
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
		must(t, records.Enqueue(ctx, tx, owedStartRow(t, issue, "Implement the change.")))
	})

	// The lane's claim, in a transaction of its own that has not committed yet.
	lane, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin the lane's transaction: %v", err)
	}
	if _, err := lane.Exec(ctx, `update outbox set lease_token = 'lane', lease_until = now() + interval '1 hour'
		where issue = $1 and kind = 'supervise'`, issue); err != nil {
		t.Fatalf("take the lease: %v", err)
	}

	// The transition's start, which reads the row before that claim and writes after it.
	done := make(chan error, 1)
	go func() {
		tx, err := st.Pool().Begin(ctx)
		if err != nil {
			done <- err
			return
		}
		if err := records.EnqueueStart(ctx, tx, owedStartRow(t, issue, "Implement the change again.")); err != nil {
			_ = tx.Rollback(ctx)
			done <- err
			return
		}
		done <- tx.Commit(ctx)
	}()
	// The start reaches the lane's row lock and waits there; the lane then commits.
	time.Sleep(500 * time.Millisecond)
	if err := lane.Commit(ctx); err != nil {
		t.Fatalf("commit the lane's claim: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("write the start of %s: %v", issue, err)
	}
	var task string
	if err := st.Pool().QueryRow(ctx, `select payload->>'task' from outbox
		where issue = $1 and kind = 'supervise' and lease_token = 'lane'`, issue).Scan(&task); err != nil {
		t.Fatalf("read the leased row's task: %v", err)
	}
	if task != "Implement the change." {
		t.Fatalf("the leased row's task = %q, want the task the lane is delivering", task)
	}
	var tasks []string
	if err := st.Pool().QueryRow(ctx, `select array_agg(payload->>'task' order by created_at, id) from outbox
		where issue = $1 and kind = 'supervise'`, issue).Scan(&tasks); err != nil {
		t.Fatalf("read the issue's starts: %v", err)
	}
	if want := []string{"Implement the change.", "Implement the change again."}; !slices.Equal(tasks, want) {
		t.Fatalf("the issue's starts, oldest first, = %q, want %q: a row a lane holds is left where it is and the caller's row is written beside it",
			tasks, want)
	}
}

// An issue owing two starts has the older of the two taken out, however their ids run.
//
// It owes two when a transition met the first under a live lease and wrote its own beside it
// (workflow.StartOrOwe), and that first lease then lapsed. Each start is a row of its own
// carrying the taken-out row's creation time, so an issue's rows no longer run in creation order
// by id: a start written over the older row takes an id above the younger one's. Chosen by id,
// the younger row would be taken out next and the older wait on carrying a task nothing replaces
// again, which is the row the waiting line puts the issue in the line by (Waiting, OwedStarts)
// and the row admission's repair reads first.
func TestEnqueueStartTakesTheOlderOfTwoOwedRows(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const issue = "LEGION-208"
	older := owedStartRow(t, issue, "Implement the change.")
	older.CreatedAt = time.Now().UTC().Add(-time.Hour)
	younger := owedStartRow(t, issue, "Implement what the red asked for.")
	younger.CreatedAt = older.CreatedAt.Add(time.Minute)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
		must(t, records.Enqueue(ctx, tx, older))
		must(t, records.Enqueue(ctx, tx, younger))

		// The first start takes the older row and leaves an id above the younger's.
		must(t, records.EnqueueStart(ctx, tx, owedStartRow(t, issue, "Implement it again.")))

		// The second must take that row, the oldest row the issue owes, and not the younger one
		// standing below it in id.
		must(t, records.EnqueueStart(ctx, tx, owedStartRow(t, issue, "Implement it once more.")))

		owed, err := records.OwedStarts(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("list the owed starts: %v", err)
		}
		if len(owed) != 2 {
			t.Fatalf("the starts owed = %#v, want the two the issue owes: a start takes out the row it stands in for, never adds one", owed)
		}
		var tasks []string
		if err := tx.QueryRow(ctx, `select array_agg(payload->>'task' order by created_at, id) from outbox
			where issue = $1 and kind = 'supervise'`, issue).Scan(&tasks); err != nil {
			t.Fatalf("read the owed tasks: %v", err)
		}
		if want := []string{"Implement it once more.", "Implement what the red asked for."}; !slices.Equal(tasks, want) {
			t.Fatalf("the tasks the issue owes, oldest first, = %q, want %q: both starts took the oldest row, and the younger one is untouched",
				tasks, want)
		}
	})
}
