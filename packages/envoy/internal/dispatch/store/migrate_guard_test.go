package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/testwait"
)

// Processes that boot at once against an empty database all migrate it. The version table is
// created under the runner's advisory lock, as the broker's runner creates its own; created before
// it, two `create table if not exists` race on the catalog and the loser exits on pg_type's unique
// index.
func TestMigrateFromSeveralProcessesAtOnceOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	first := openEmptyTestStore(t)
	const processes = 4
	stores := []*Store{first}
	for len(stores) < processes {
		another, err := Open(ctx, first.Pool.Config().ConnString())
		if err != nil {
			t.Fatalf("open another process's store: %v", err)
		}
		t.Cleanup(func() {
			testwait.Returns(t, "every connection of another process's pool to come back, so the pool closes", another.Pool.Close)
		})
		stores = append(stores, another)
	}
	set := fstest.MapFS{"migrations/0001_first.up.sql": {Data: []byte("create table guard_first (id integer)")}}
	start := make(chan struct{})
	// Each process sends its answer on a channel of its own, so the wait for it names the process
	// that has not returned.
	errs := make([]chan error, len(stores))
	for i, process := range stores {
		errs[i] = make(chan error, 1)
		go func() {
			<-start
			errs[i] <- process.migrate(ctx, set)
		}()
	}
	close(start)
	for i := range processes {
		if err := testwait.Receive(t, fmt.Sprintf("process %d of %d's migrate to return", i+1, processes), errs[i]); err != nil {
			t.Errorf("a process migrating at the same moment failed: %v", err)
		}
	}
	var recorded int
	if err := first.Pool.QueryRow(ctx, "select count(*) from schema_migrations where version = 1").Scan(&recorded); err != nil {
		t.Fatalf("count version 1: %v", err)
	}
	if recorded != 1 {
		t.Errorf("version 1 recorded %d times, want once", recorded)
	}
}

// A set in which two files share a version is refused whole, before the runner touches the
// database. The runner applies every version it has not recorded, so a set it accepted would apply
// the first file of the pair, pass over the second as already applied, and report success. Two
// timestamped files share a version only where one author's file carries the moment another's was
// created rather than its own; the refusal is the same, since the runner reads the identifier and
// not the name beside it.
func TestMigrateRefusesTwoMigrationsSharingAVersionBeforeApplyingAny(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  fstest.MapFS
		want []string
	}{
		{
			name: "two numbered migrations",
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":  {Data: []byte("create table guard_first (id integer)")},
				"migrations/0002_second.up.sql": {Data: []byte("create table guard_second (id integer)")},
				"migrations/0002_third.up.sql":  {Data: []byte("create table guard_third (id integer)")},
			},
			want: []string{"0002_second.up.sql", "0002_third.up.sql", "version 2"},
		},
		{
			name: "two timestamped migrations carrying one moment",
			set: fstest.MapFS{
				"migrations/0001_first.up.sql":       {Data: []byte("create table guard_first (id integer)")},
				"migrations/20261030120000_a.up.sql": {Data: []byte("create table guard_a (id integer)")},
				"migrations/20261030120000_b.up.sql": {Data: []byte("create table guard_b (id integer)")},
			},
			want: []string{"20261030120000_a.up.sql", "20261030120000_b.up.sql", "version 20261030120000"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEmptyTestStore(t)

			err := store.migrate(ctx, tc.set)
			left := existingTables(t, store, "guard_first", "guard_second", "guard_third", "guard_a", "guard_b", "schema_migrations")
			if err == nil {
				t.Errorf("migrate accepted two migrations sharing a version and reported success; the tables it left: %v", left)
			} else {
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("migrate error = %q, want it to name %q", err, want)
					}
				}
			}
			if len(left) != 0 {
				t.Errorf("the refused set left %v behind; want nothing applied, not even the version table", left)
			}
		})
	}
}

// A migration whose lock another transaction holds gives up once the runner's lock timeout runs
// out on every one of its attempts, naming itself, the lock it wanted and the session that held
// it, and applies nothing. Unbounded, it waits for as long as the holder lives, and every read and
// write of the table queues behind its request for that long.
//
// The store runs under production's Timeout on a fake clock: the error's lock and holder are what
// the lock watch read, and its reading must land inside the wait (watchReadsTheWait). Each give-up
// is a fact the runner reports: after each attempt but the last, the pause it arms on the store's
// clock once the attempt has timed out (awaitThePause), which the test moves past, and after the
// last, its error. A runner whose lock waits nothing bounds never arms that pause, and the test's
// wait for it gives up at the test binary's deadline less testwait's margin, naming it.
func TestMigrateBoundsTheWaitForALockAnotherTransactionHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openEmptyTestStore(t)
	fake := underLocking(store, 0)
	if _, err := store.Pool.Exec(ctx, "create table guard_held (id integer)"); err != nil {
		t.Fatalf("create the held table: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	// ACCESS SHARE is what any read of the table takes, and it conflicts with the ACCESS EXCLUSIVE
	// lock ALTER TABLE asks for.
	if _, err := held.Exec(ctx, "lock table guard_held in access share mode"); err != nil {
		t.Fatalf("hold the table: %v", err)
	}
	set := fstest.MapFS{
		"migrations/0001_widen_held.up.sql": {Data: []byte("alter table guard_held add column wider integer")},
	}

	migrated := make(chan error, 1)
	go func() { migrated <- store.migrate(ctx, set) }()
	everyAttemptTimesOut(t, store, fake, "guard_held", "AccessExclusiveLock")
	err = testwait.Receive(t, "migrate to give up after its last attempt", migrated)
	if err == nil {
		t.Fatal("migrate applied a migration whose lock another transaction held")
	}
	var lockTimeout *pgmigrate.LockTimeoutError
	if !errors.As(err, &lockTimeout) || !strings.Contains(err.Error(), fmt.Sprintf("after %d attempts:", pgmigrate.LockTimeoutAttempts)) {
		t.Fatalf("migrate = %v, want a *pgmigrate.LockTimeoutError after %d attempts: nothing bounds a migration's lock wait", err, pgmigrate.LockTimeoutAttempts)
	}
	if lockTimeout.Timeout != pgmigrate.LockTimeout {
		t.Errorf("the migration gave up under a lock_timeout of %v, want the store's, %v", lockTimeout.Timeout, pgmigrate.LockTimeout)
	}
	t.Logf("migrate gave up: %v", err)
	holderPID := strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	for _, want := range []string{"0001_widen_held.up.sql", "AccessExclusiveLock", "guard_held", "pid " + holderPID, "pg_stat_activity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migrate error = %q, want it to name %q", err, want)
		}
	}
	var widened bool
	if err := store.Pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns
		where table_name = 'guard_held' and column_name = 'wider')`).Scan(&widened); err != nil {
		t.Fatalf("look for the migration's column: %v", err)
	}
	var recorded bool
	if err := store.Pool.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = 1)").Scan(&recorded); err != nil {
		t.Fatalf("look for the migration's version: %v", err)
	}
	if widened || recorded {
		t.Errorf("the migration that gave up left its column (%t) or its version (%t) behind", widened, recorded)
	}
}

// existingTables reports which of names exist in the store's database.
func existingTables(t *testing.T, store *Store, names ...string) []string {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(),
		"select name from unnest($1::text[]) as name where to_regclass(name) is not null order by name", names)
	if err != nil {
		t.Fatalf("look for tables: %v", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read tables: %v", err)
	}
	return found
}
