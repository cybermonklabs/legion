package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/store"
)

// A departure and a jump mark are written by two transactions that meet on one issue, and the
// case that matters is the one where they overlap: the route's write is in flight when the
// departure begins, or the departure is in flight when the route's write begins.
//
// Both halves are driven in the order a departure really runs, because an earlier pin of this
// file drove a bare `update issues` and certified a model the code does not use. Engine.leave
// (workflow/linger.go) clears the mark FIRST, through Store.ClearMergeJump, and writes the issue
// row after, and it runs inside the transaction that takes intake.SerializeFacts: every one of
// leave's four callers reaches it under that advisory lock — two as fact handlers, close_root
// through intake.CloseRoot, and LeaveUnslotted through an admission pass whose two entry points
// are both inside it. The lock is taken here by its own statement rather than by importing
// intake, which imports this package.
//
// What is asserted is the outcome rather than any mechanism, so the pin holds whatever shape the
// serialization takes and constrains no fix to one lock. Each half fails on its own defect: the
// first leaves a mark standing on an issue that departed, which a re-admission's merge would
// later spend against a decision nobody made about that merge; the second ends in a deadlock
// (40P01), which is two writers taking one pair of rows in opposite orders.
func TestADepartureAndAJumpMarkWriteLeaveNoMarkAndNoDeadlock(t *testing.T) {
	ctx := context.Background()
	setAt := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

	t.Run("the mark's write is in flight when the departure begins", func(t *testing.T) {
		st := migratedStore(t)
		records := NewStore()
		inTx(t, st, func(tx pgx.Tx) { mergeMarkIssue(t, records, tx, "LEGION-208") })

		mark, err := st.Pool().Begin(ctx)
		must(t, err)
		defer mark.Rollback(ctx)
		wrote, err := records.SetMergeJump(ctx, mark, MergeJump{Issue: "LEGION-208",
			Reason: "the queue-failure fix goes first", Session: "controller-1", SetAt: setAt})
		must(t, err)
		if !wrote {
			t.Fatal("the route's write answered that it wrote no mark for a running issue, want true")
		}

		// The departure begins while that write is uncommitted, in leave's own order and under
		// leave's own lock.
		leave, err := st.Pool().Begin(ctx)
		must(t, err)
		defer leave.Rollback(ctx)
		serializeFacts(t, leave)
		left := make(chan error, 1)
		go func() {
			if _, err := records.ClearMergeJump(ctx, leave, "LEGION-208"); err != nil {
				left <- err
				return
			}
			_, err := leave.Exec(ctx, "update issues set status = 'backlog' where key = $1", "LEGION-208")
			left <- err
		}()
		time.Sleep(300 * time.Millisecond)
		must(t, mark.Commit(ctx))
		if err := <-left; err != nil {
			t.Fatalf("the departure failed while the mark's write was in flight: %v", err)
		}
		must(t, leave.Commit(ctx))

		assertDepartedWithNoMark(t, st, "LEGION-208")
	})

	t.Run("the departure is in flight when the mark's write begins", func(t *testing.T) {
		st := migratedStore(t)
		records := NewStore()
		inTx(t, st, func(tx pgx.Tx) {
			mergeMarkIssue(t, records, tx, "LEGION-208")
			wrote, err := records.SetMergeJump(ctx, tx, MergeJump{Issue: "LEGION-208",
				Reason: "a mark the controller set earlier", Session: "controller-1", SetAt: setAt})
			must(t, err)
			if !wrote {
				t.Fatal("the seeded mark was not written")
			}
		})

		leave, err := st.Pool().Begin(ctx)
		must(t, err)
		defer leave.Rollback(ctx)
		serializeFacts(t, leave)
		if _, err := records.ClearMergeJump(ctx, leave, "LEGION-208"); err != nil {
			t.Fatalf("the departure's clear: %v", err)
		}

		// The controller's newer word arrives while that departure is in flight.
		marked := make(chan error, 1)
		answered := make(chan bool, 1)
		go func() {
			tx, err := st.Pool().Begin(ctx)
			if err != nil {
				marked <- err
				return
			}
			defer tx.Rollback(ctx)
			wrote, err := records.SetMergeJump(ctx, tx, MergeJump{Issue: "LEGION-208",
				Reason: "the controller's newer word", Session: "controller-2", SetAt: setAt})
			if err != nil {
				marked <- err
				return
			}
			answered <- wrote
			marked <- tx.Commit(ctx)
		}()
		time.Sleep(300 * time.Millisecond)

		if _, err := leave.Exec(ctx, "update issues set status = 'backlog' where key = $1", "LEGION-208"); err != nil {
			t.Fatalf("the departure's write of the issue row, while a mark's write waited: %v", err)
		}
		must(t, leave.Commit(ctx))
		if err := <-marked; err != nil {
			t.Fatalf("the mark's write failed against a departure in flight: %v", err)
		}
		if wrote := <-answered; wrote {
			t.Fatal("the route's write answered that it wrote a mark for an issue that had departed, want false")
		}

		assertDepartedWithNoMark(t, st, "LEGION-208")
	})
}

// serializeFacts takes intake.SerializeFacts' own lock (intake/facts.go), which every departure's
// transaction holds before it touches anything. It is written out rather than called because
// intake imports this package.
func serializeFacts(t *testing.T, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), "select pg_advisory_xact_lock(hashtext('legion.apply_fact'))"); err != nil {
		t.Fatalf("take the fact lock a departure runs under: %v", err)
	}
}

// assertDepartedWithNoMark reads what the two transactions left behind: the issue has departed,
// and it carries no jump mark.
func assertDepartedWithNoMark(t *testing.T, st *store.Store, key string) {
	t.Helper()
	ctx := context.Background()
	var status string
	if err := st.Pool().QueryRow(ctx, "select status from issues where key = $1", key).Scan(&status); err != nil {
		t.Fatalf("read the status of %s: %v", key, err)
	}
	if status != "backlog" {
		t.Fatalf("the status of %s = %q, want the backlog the departure wrote", key, status)
	}
	var marks int
	if err := st.Pool().QueryRow(ctx, "select count(*) from merge_jumps where issue = $1", key).Scan(&marks); err != nil {
		t.Fatalf("count the marks of %s: %v", key, err)
	}
	if marks != 0 {
		t.Fatalf("%s departed holding %d jump mark(s), want none: a mark left on a departed issue is "+
			"spent by whatever merge a re-admission of it reaches, against a decision nobody made", key, marks)
	}
}
