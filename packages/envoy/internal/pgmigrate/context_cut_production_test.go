package pgmigrate

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// A boot stopped while a migration waits behind a holder the lock watch has read, as SIGTERM
// cancels Dispatch's boot context (cmd/dispatch's signal.NotifyContext), fails at once under the
// production policy, the one a context with no policy follows: one attempt, an error that names
// the lock and its holder, errors.Is still finding context.Canceled, and a cause that is the
// caller's cancel rather than a lock_timeout that never ran out.
func TestACancelDuringAWaitTheWatchReadNamesTheLockUnderTheProductionPolicy(t *testing.T) {
	t.Parallel()
	rig := newContextCutRig(t)
	cut, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	done := make(chan error, 1)
	go func() {
		done <- RetryLockTimeouts(cut, "0001_added.up.sql", func(ctx context.Context) error {
			attempts++
			return pgx.BeginFunc(ctx, rig.runner, func(tx pgx.Tx) error {
				return Exec(ctx, tx, Migration{Name: "0001_added.up.sql", SQL: "alter table things add column added int"}, nil)
			})
		})
	}()
	rig.watchReadSince(t, rig.runnerWaits(t))
	cancel()
	err := <-done
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1: a cancel is never tried again", attempts)
	}
	var named *LockTimeoutError
	if !errors.As(err, &named) {
		t.Fatalf("err = %v, want a *LockTimeoutError naming the lock the migration waited for when the boot was stopped", err)
	}
	if named.Wait == nil || named.Wait.Object != "things" {
		t.Fatalf("the named error's wait is %+v, want the lock on things", named.Wait)
	}
	pid := rig.holderConn.PgConn().PID()
	if !slices.ContainsFunc(named.Wait.Holders, func(h Session) bool { return h.PID == pid }) {
		t.Errorf("the named error's holders are %+v, want the holder's pid %d among them", named.Wait.Holders, pid)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(context.Canceled) to hold: the cancel, not a lock_timeout, ended the wait", err)
	}
	if strings.Contains(err.Error(), "lock_timeout ran out") {
		t.Errorf("err = %v\nwant it not to say its lock_timeout ran out: the caller's cancel ended the wait", err)
	}
}
