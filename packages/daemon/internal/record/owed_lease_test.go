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
// what puts the issue in the waiting line's first part, and ReplaceOwedStart is what a second
// transition before the slot replaces the task with; a row neither of them can see is a start
// nothing delivers and nothing re-places: the issue stands in no part of the line, and
// admission's repair (finishFencedStarts) walks the same read.
func TestALapsedLeaseLeavesTheStartOwedAndReplaceable(t *testing.T) {
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

		replaced, err := records.ReplaceOwedStart(ctx, tx, owedStartRow(t, issue, "Implement the change again."))
		if err != nil {
			t.Fatalf("replace the owed start: %v", err)
		}
		if !replaced {
			t.Fatalf("ReplaceOwedStart left the lapsed row alone: a second transition before the slot replaces the task the worker will be given")
		}
	})
}

// The other half of the same rule: a row a lane takes while the replace runs must keep the task
// the lane is about to deliver. The replace chooses the row in a sub-select, which reads the row
// as it stood before the lane's claim, and then writes by id alone, so the write lands on a row
// the lane now holds: the lane delivers the earlier task while the record carries the later
// assignment, and the worker's completion of it is refused.
func TestReplaceOwedStartLeavesARowALaneTakesWhileItRuns(t *testing.T) {
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

	// The transition's replace, which reads the row before that claim and writes after it.
	type outcome struct {
		replaced bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		tx, err := st.Pool().Begin(ctx)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		replaced, err := records.ReplaceOwedStart(ctx, tx, owedStartRow(t, issue, "Implement the change again."))
		if err != nil {
			_ = tx.Rollback(ctx)
			done <- outcome{err: err}
			return
		}
		done <- outcome{replaced: replaced, err: tx.Commit(ctx)}
	}()
	// The replace reaches the lane's row lock and waits there; the lane then commits.
	time.Sleep(500 * time.Millisecond)
	if err := lane.Commit(ctx); err != nil {
		t.Fatalf("commit the lane's claim: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("replace the owed start: %v", got.err)
	}
	if got.replaced {
		t.Fatalf("ReplaceOwedStart overwrote the task of a row the lane took while it ran: the lane delivers the earlier task under the later assignment")
	}
	var task string
	if err := st.Pool().QueryRow(ctx, `select payload->>'task' from outbox where issue = $1 and kind = 'supervise'`, issue).Scan(&task); err != nil {
		t.Fatalf("read the leased row's task: %v", err)
	}
	if task != "Implement the change." {
		t.Fatalf("the leased row's task = %q, want the task the lane is delivering", task)
	}
}

// An issue owing two starts has the older of the two replaced, however their ids run.
//
// It owes two when a transition met the first under a live lease and enqueued its own
// (workflow.StartOrOwe), and that first lease then lapsed. Each replacement is a row of its own
// carrying the replaced row's creation time, so an issue's rows no longer run in creation order
// by id: a replacement of the older row takes an id above the younger one's. Chosen by id, the
// younger row would be replaced next and the older wait on carrying a task nothing replaces
// again, which is the row the waiting line puts the issue in the line by (Waiting, OwedStarts)
// and the row admission's repair reads first.
func TestReplaceOwedStartTakesTheOlderOfTwoOwedRows(t *testing.T) {
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

		// The first replacement takes the older row and leaves an id above the younger's.
		replaced, err := records.ReplaceOwedStart(ctx, tx, owedStartRow(t, issue, "Implement it again."))
		if err != nil {
			t.Fatalf("replace the older owed start: %v", err)
		}
		if !replaced {
			t.Fatalf("ReplaceOwedStart replaced neither row")
		}

		// The second must take that replacement, the oldest row the issue owes, and not the
		// younger one standing below it in id.
		if replaced, err = records.ReplaceOwedStart(ctx, tx, owedStartRow(t, issue, "Implement it once more.")); err != nil {
			t.Fatalf("replace the owed start a second time: %v", err)
		}
		if !replaced {
			t.Fatalf("ReplaceOwedStart replaced nothing the second time")
		}

		owed, err := records.OwedStarts(ctx, tx, "LEGION")
		if err != nil {
			t.Fatalf("list the owed starts: %v", err)
		}
		if len(owed) != 2 {
			t.Fatalf("the starts owed = %#v, want the two the issue owes: a replacement replaces a row, never adds one", owed)
		}
		var tasks []string
		if err := tx.QueryRow(ctx, `select array_agg(payload->>'task' order by created_at, id) from outbox
			where issue = $1 and kind = 'supervise'`, issue).Scan(&tasks); err != nil {
			t.Fatalf("read the owed tasks: %v", err)
		}
		if want := []string{"Implement it once more.", "Implement what the red asked for."}; !slices.Equal(tasks, want) {
			t.Fatalf("the tasks the issue owes, oldest first, = %q, want %q: both replacements took the oldest row, and the younger one is untouched",
				tasks, want)
		}
	})
}
