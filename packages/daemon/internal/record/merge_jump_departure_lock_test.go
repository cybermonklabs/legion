package record

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The jump mark's write is conditional on the issue still being one the daemon runs, and
// TestAJumpMarkIsNotWrittenForAnIssueWhoseDepartureCommitsFirst drives the departure that commits
// before the write's transaction begins. That case answers false with or without a row lock,
// because a plain read under READ COMMITTED already sees a committed departure.
//
// The case this one drives is the other half, and it is the half the `for update` exists for: a
// departure whose update commits while the write's own transaction is open, between its read of
// the issue and its commit. A plain re-read does not serialize against that at all — it reads a
// running issue, the departure commits, and the mark is inserted over it and outlives it, to be
// spent weeks later by whatever merge a re-admission of the issue reaches. Nothing else in the
// tree fails when the row lock is taken off: with `for update` deleted from
// Postgres.SetMergeJump, every test of internal/record and internal/api passes, measured at
// 271c19fe, so the lock had no reader of its own.
//
// What is asserted is the serialization, which is the whole of what the lock buys: while the
// mark's transaction is open, a departure of that issue cannot commit. It waits for the mark's
// transaction instead, and a departure that will not wait is refused (55P03, lock_not_available)
// rather than slipping underneath. The lock timeout is what makes the case deterministic: without
// the lock the update returns at once, and no wait of any length tells the two orderings apart.
func TestAJumpMarkWriteHoldsItsIssueAgainstADepartureCommittingUnderIt(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) { mergeMarkIssue(t, records, tx, "LEGION-208") })

	// The mark's own transaction, left open after the write, as the route's is between its
	// write and its commit.
	mark, err := st.Pool().Begin(ctx)
	must(t, err)
	defer mark.Rollback(ctx)
	wrote, err := records.SetMergeJump(ctx, mark, MergeJump{Issue: "LEGION-208",
		Reason: "the queue-failure fix goes first", Session: "controller-1",
		SetAt: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)})
	must(t, err)
	if !wrote {
		t.Fatal("the write answered that it wrote no mark for a running issue, want true")
	}

	// The departure, on a connection of its own, as the workflow's own leave runs it.
	departure, err := st.Pool().Begin(ctx)
	must(t, err)
	defer departure.Rollback(ctx)
	if _, err := departure.Exec(ctx, "set local lock_timeout = '2s'"); err != nil {
		t.Fatalf("set the departure's lock timeout: %v", err)
	}
	_, err = departure.Exec(ctx, "update issues set status = 'backlog' where key = $1", "LEGION-208")
	if err == nil {
		t.Fatal("the departure updated the issue while the mark's own transaction was open: " +
			"the write does not hold its issue, so a departure commits inside the write's window " +
			"and the mark outlives it")
	}
	var refusal *pgconn.PgError
	if !errors.As(err, &refusal) || refusal.Code != "55P03" {
		t.Fatalf("the departure failed with %v, want the lock timeout 55P03: the case is only "+
			"driven where the departure is refused because the mark's transaction holds the row", err)
	}
}
