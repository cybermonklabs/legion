package pgmigrate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg"
)

// A deadline that cuts a migration's first attempt while it waits behind a holder ends with the
// runner's named error, naming that lock and its holder, and keeps the deadline: by then the watch
// has read what the migration waits for, so a cut that dropped it would say only that a context ran
// out, though the lock was contended. INFRA-3904, the tester's finding on #5806 (found by ApiCloseRows):
// a second session read pg_locks a second in as "1 granted, 1 waiting", and the runner's give-up named
// nothing. Twice: the runner's own LockTimeout with a deadline inside it, and a migration's own longer
// lock_timeout, as 20261120222733_drop_block_refs_to sets 120s, with a deadline inside that. Without the
// execWatched fix (a context error with wait != nil becoming a *LockTimeoutError) this fails on purpose.
func TestADeadlineThatCutsTheFirstContendedAttemptNamesTheLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sql      string
		deadline time.Duration
	}{
		{"the runner's own lock_timeout", "alter table things add column added int", 3 * time.Second},
		{"a migration's own longer lock_timeout", "set local lock_timeout = '120s'; alter table things add column added int", 8 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			url := testpg.URL(t)
			admin := connect(t, ctx, url)
			if _, err := admin.Exec(ctx, "create table things (kind text)"); err != nil {
				t.Fatal(err)
			}
			holderConn := connect(t, ctx, url)
			holder, err := holderConn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
			if _, err := holder.Exec(ctx, "lock table things in access share mode"); err != nil {
				t.Fatal(err)
			}
			runner := connect(t, ctx, url)
			bounded, cancel := context.WithTimeout(ctx, tc.deadline)
			defer cancel()
			bounded = WithRetryPolicy(bounded, DeadlineRetryPolicy())
			attempts := 0
			err = RetryLockTimeouts(bounded, "0001_added.up.sql", func(ctx context.Context) error {
				attempts++
				return pgx.BeginFunc(ctx, runner, func(tx pgx.Tx) error {
					return Exec(ctx, tx, Migration{Name: "0001_added.up.sql", SQL: tc.sql}, nil)
				})
			})
			if attempts != 1 {
				t.Errorf("made %d attempts, want 1: the deadline falls inside the first attempt's lock_timeout", attempts)
			}
			var named *LockTimeoutError
			if !errors.As(err, &named) {
				t.Fatalf("err = %v, want a *LockTimeoutError naming the lock the migration waited for when the deadline cut it", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("err = %v, want errors.Is(context.DeadlineExceeded) to hold: the deadline, not a lock_timeout, ended the attempt", err)
			}
			if named.Wait == nil || named.Wait.Object != "things" {
				t.Fatalf("the named error's wait is %+v, want the lock on things", named.Wait)
			}
			pid := holderConn.PgConn().PID()
			found := false
			for _, h := range named.Wait.Holders {
				found = found || h.PID == pid
			}
			if !found {
				t.Errorf("the named error's holders are %+v, want the holder's pid %d among them", named.Wait.Holders, pid)
			}
		})
	}
}
