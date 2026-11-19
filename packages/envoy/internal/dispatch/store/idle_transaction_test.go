package store

import (
	"context"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

// A pool opened with BoundIdleTransactions has Postgres end a transaction that sends nothing for
// IdleTransactionBound between two of its statements, and the locks it held go with it: a
// transaction that takes an advisory lock and then waits, as one of a process stopped inside a
// write does, gives the lock up IdleTransactionBound later, and its next statement fails. The
// control is a pool opened without the option, whose idle transaction keeps the lock past the
// bound, so the row fails if the server ends idle transactions of its own accord.
func TestABoundPoolsIdleTransactionGivesItsLocksUp(t *testing.T) {
	ctx := context.Background()
	databaseURL := testDatabaseURL(t)
	// How long past the bound the row waits for the lock: the server checks the timeout on a
	// timer of its own, and the poll below reads every 20 ms.
	const slack = 2 * time.Second
	for _, row := range []struct {
		name    string
		options []OpenOption
		setting string
		ends    bool
	}{
		{"bounded", []OpenOption{BoundIdleTransactions()}, strconv.FormatInt(IdleTransactionBound.Milliseconds(), 10), true},
		{"unbounded", nil, "0", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			idle, err := Open(ctx, databaseURL, row.options...)
			if err != nil {
				t.Fatalf("open the pool: %v", err)
			}
			t.Cleanup(idle.Pool.Close)
			var setting string
			if err := idle.Pool.QueryRow(ctx, `select setting from pg_settings where name = 'idle_in_transaction_session_timeout'`).Scan(&setting); err != nil {
				t.Fatalf("read the pool's idle_in_transaction_session_timeout: %v", err)
			}
			if setting != row.setting {
				t.Fatalf("the pool's connections carry idle_in_transaction_session_timeout %s ms, want %s", setting, row.setting)
			}
			other := openTestStore(t)
			key := rand.Int64()
			tx, err := idle.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin the idle transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, key); err != nil {
				t.Fatalf("take the lock: %v", err)
			}
			idleFrom := time.Now()
			var taken time.Time
			for deadline := idleFrom.Add(IdleTransactionBound + slack); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				var got bool
				if err := other.Pool.QueryRow(ctx, `select pg_try_advisory_xact_lock($1)`, key).Scan(&got); err != nil {
					t.Fatalf("try the lock from another connection: %v", err)
				}
				if got {
					taken = time.Now()
					break
				}
			}
			_, next := tx.Exec(ctx, `select 1`)
			switch {
			case row.ends && taken.IsZero():
				t.Fatalf("another connection could not take the lock %v after the transaction went idle, past the %v bound", IdleTransactionBound+slack, IdleTransactionBound)
			case row.ends && taken.Sub(idleFrom) < IdleTransactionBound:
				t.Fatalf("another connection took the lock %v after the transaction went idle, before the %v bound", taken.Sub(idleFrom), IdleTransactionBound)
			case row.ends && next == nil:
				t.Fatalf("the idle transaction's next statement succeeded after Postgres ended its session")
			case !row.ends && !taken.IsZero():
				t.Fatalf("another connection took the lock %v after an unbounded transaction went idle: the server ends idle transactions of its own accord, so this row proves nothing", taken.Sub(idleFrom))
			case !row.ends && next != nil:
				t.Fatalf("the unbounded idle transaction's next statement failed: %v", next)
			}
			if row.ends {
				t.Logf("another connection took the lock %v after the transaction went idle; the transaction's next statement answered %v", taken.Sub(idleFrom).Round(time.Millisecond), next)
			}
		})
	}
}
