package pgmigrate

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/testpg"
)

// If the deadline cuts the first attempt while it waits for a lock the watch has read,
// the error names the lock and its holder. The error also wraps the context deadline error.
// This applies with both the default lock_timeout and a longer timeout set by the migration.
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
			runner := connect(t, ctx, url)
			if _, err := runner.Exec(ctx, "create table things (kind text)"); err != nil {
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
			if !slices.ContainsFunc(named.Wait.Holders, func(h Session) bool { return h.PID == pid }) {
				t.Errorf("the named error's holders are %+v, want the holder's pid %d among them", named.Wait.Holders, pid)
			}
		})
	}
}
