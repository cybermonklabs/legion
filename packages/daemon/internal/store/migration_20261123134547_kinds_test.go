package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// laterKind stands for an outbox kind another migration adds to outbox_kind_check, one that sorts
// above the fix migration on main or lands on a database before it does: main's merge_holds_changed,
// pending_basis_seed and reparent_seed are three such kinds today.
const laterKind = "merge_holds_changed"

// extendKindCheck adds kinds to outbox_kind_check as a migration of another writer does, keeping
// every kind the check holds: the database a real upgrade finds, where that migration ran first.
func extendKindCheck(t *testing.T, store *Store, kinds ...string) {
	t.Helper()
	ctx := context.Background()
	var held []string
	if err := store.Pool().QueryRow(ctx, `select array_agg(m[1]) from pg_constraint c,
		regexp_matches(pg_get_constraintdef(c.oid), '''([a-z0-9_]+)''', 'g') as m
		where c.conrelid = 'outbox'::regclass and c.conname = 'outbox_kind_check'`).Scan(&held); err != nil || len(held) == 0 {
		t.Fatalf("read the kinds of outbox_kind_check = %v, %v; want the list an earlier migration wrote", held, err)
	}
	list := ""
	for _, kind := range append(held, kinds...) {
		if list != "" {
			list += ", "
		}
		list += "'" + kind + "'"
	}
	if _, err := store.Pool().Exec(ctx, `alter table outbox drop constraint outbox_kind_check;
		alter table outbox add constraint outbox_kind_check check (kind in (`+list+`))`); err != nil {
		t.Fatalf("add %v to outbox_kind_check: %v", kinds, err)
	}
}

// queueKind queues one outbox row of kind for issue, and answers the error the insert met.
func queueKind(store *Store, kind, issue string) error {
	_, err := store.Pool().Exec(context.Background(), `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
		values ($1, $2, '{}'::jsonb, 0, now(), '')`, kind, issue)
	return err
}

// The fix migration's kind check is one list that other migrations write too, so it must keep every
// kind the database's check holds, whichever migration ran first. A database another writer's
// migration already reached holds a kind of that writer's (laterKind) and a row of it waits in the
// outbox, as on a database main has migrated: the fix migration applies there, the row stays, both
// that kind and the fix's own kinds are taken after it, and its way back takes out the fix's kinds
// alone. Its red is the list the migration wrote whole from 20261119203047_design_news's, which
// named no kind added since: on such a database the migration failed with SQLSTATE 23514 on the
// waiting row, and the daemon could not start.
func TestTheFixMigrationKeepsEveryKindAnotherMigrationAdded(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, fixBesideStepsVersion-1)
	extendKindCheck(t, store, laterKind)
	if err := queueKind(store, laterKind, "LEGION-208"); err != nil {
		t.Fatalf("queue a %s row before the fix migration: %v", laterKind, err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			t.Fatalf("the fix migration on a database whose check holds %s failed with SQLSTATE %s: %v", laterKind, pgErr.Code, err)
		}
		t.Fatalf("the fix migration on a database whose check holds %s: %v", laterKind, err)
	}

	if got := rowsAsText(t, store, `select kind from outbox order by id`); len(got) != 1 || got[0] != laterKind {
		t.Fatalf("the outbox after the fix migration = %v, want the waiting %s row", got, laterKind)
	}
	for _, kind := range []string{laterKind, "fix_job_read", "ci_news", "beside_branches_delete"} {
		if err := queueKind(store, kind, "LEGION-208"); err != nil {
			t.Errorf("queue a %s row after the fix migration: %v", kind, err)
		}
	}

	if _, err := store.Pool().Exec(ctx, `delete from outbox`); err != nil {
		t.Fatalf("empty the outbox before the way back: %v", err)
	}
	runFixBesideStepsWayBack(t, store, wayBackOf(t, fixBesideStepsVersion, fixBesideStepsRelease))
	if err := queueKind(store, laterKind, "LEGION-208"); err != nil {
		t.Errorf("queue a %s row after the way back: %v, want it taken: the way back takes out the fix's kinds alone", laterKind, err)
	}
	for _, kind := range []string{"fix_job_read", "ci_news", "beside_branches_delete"} {
		var pgErr *pgconn.PgError
		if err := queueKind(store, kind, "LEGION-208"); !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("queue a %s row after the way back = %v, want the check's refusal, SQLSTATE 23514", kind, err)
		}
	}
}
