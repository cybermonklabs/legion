package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

const removedReachNoticesVersion = 20261116082001

// removedControllerNotices is every controller notice an earlier build of the release that
// reports claims out of reach queued and this release no longer has.
var removedControllerNotices = []string{"unreachable-claims", "relaunch-ready", "relaunch-guard-held",
	"relaunch-not-back", "unreachable-exit", "envoy-no-live-session", "worker-locked-out", "claim-out-of-reach"}

// earlierBuildsDatabase is a database an earlier build of the release that reports claims out of
// reach ran against, migrated through every version below the cleanup: its kind check admitted the
// two reach kinds, it held the table of the project's notice marks, and its outbox holds a row of
// each reach kind, a notice row and a controller_notice row of each removed notice, and one notice
// row and one controller_notice row of a notice this release still sends.
func earlierBuildsDatabase(t *testing.T) (*Store, []migrations.Migration) {
	t.Helper()
	ctx := context.Background()
	store := emptyStore(t)
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	var below []migrations.Migration
	for _, migration := range all {
		if migration.Version < removedReachNoticesVersion {
			below = append(below, migration)
		}
	}
	if _, err := store.migrate(ctx, below); err != nil {
		t.Fatalf("migrate the versions below the cleanup: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `alter table outbox drop constraint outbox_kind_check;
		create table reach_project_notices (project text primary key)`); err != nil {
		t.Fatalf("shape the earlier build's database: %v", err)
	}
	queue := func(kind, notice string) {
		if _, err := store.pool.Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
			values ($1, 'LEGION-208', jsonb_build_object('kind', $2::text), 0, now(), '')`, kind, notice); err != nil {
			t.Fatalf("queue a %s row of %q: %v", kind, notice, err)
		}
	}
	queue("reach_notice", "claim-out-of-reach")
	queue("reach_controller_notice", "unreachable-claims")
	queue("notice", "claim-out-of-reach")
	for _, notice := range removedControllerNotices {
		queue("controller_notice", notice)
	}
	queue("notice", "phase-finished")
	queue("controller_notice", "project-paused")
	return store, all
}

// A database an earlier build ran against migrates through the cleanup and every later version,
// the later lists of outbox_kind_check among them, and keeps only the rows of notices this release
// sends. The control is the same database migrated through every later version but the cleanup:
// the first later kind list is refused over the reach rows (SQLSTATE 23514), which is the boot the
// cleanup keeps from failing.
func TestTheCleanupLetsTheLaterKindChecksRunAndKeepsOnlyLiveNotices(t *testing.T) {
	ctx := context.Background()

	control, all := earlierBuildsDatabase(t)
	var withoutCleanup []migrations.Migration
	for _, migration := range all {
		if migration.Version != removedReachNoticesVersion {
			withoutCleanup = append(withoutCleanup, migration)
		}
	}
	var refused *pgconn.PgError
	if _, err := control.migrate(ctx, withoutCleanup); !errors.As(err, &refused) || refused.Code != "23514" {
		t.Fatalf("the later versions without the cleanup = %v, want a later kind list refused over the reach rows (23514)", err)
	}

	store, all := earlierBuildsDatabase(t)
	applied, err := store.Migrate(ctx)
	if err != nil {
		t.Fatalf("migrate through the cleanup and every later version: %v", err)
	}
	later := 0
	for _, migration := range all {
		if migration.Version >= removedReachNoticesVersion {
			later++
		}
	}
	if applied != later {
		t.Fatalf("the upgrade applied %d migrations, want the cleanup and every later version, %d", applied, later)
	}
	rows, err := store.pool.Query(ctx, `select kind, payload->>'kind' from outbox order by id`)
	if err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	defer rows.Close()
	var kept [][2]string
	for rows.Next() {
		var row [2]string
		if err := rows.Scan(&row[0], &row[1]); err != nil {
			t.Fatalf("read an outbox row: %v", err)
		}
		kept = append(kept, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the outbox: %v", err)
	}
	if want := [][2]string{{"notice", "phase-finished"}, {"controller_notice", "project-paused"}}; len(kept) != 2 || kept[0] != want[0] || kept[1] != want[1] {
		t.Fatalf("after the upgrade the outbox holds %v, want only %v", kept, want)
	}
	var table bool
	if err := store.pool.QueryRow(ctx, `select to_regclass('reach_project_notices') is not null`).Scan(&table); err != nil || table {
		t.Fatalf("after the upgrade reach_project_notices exists %t (%v), want it dropped", table, err)
	}
}
