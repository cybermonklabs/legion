package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
	"github.com/sjawhar/envoy/internal/testpg/testdb"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The broker's set is numbered 1 to N with none missing; pgmigratetest.CheckNumberedOneToN says
// why.
func TestMigrationSetIsNumberedOneToN(t *testing.T) {
	if err := pgmigratetest.CheckNumberedOneToN(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// Every file in the broker's migrations directory reaches the runner;
// pgmigratetest.CheckEmbedsEveryFile says why that needs a test.
func TestEveryMigrationFileIsEmbedded(t *testing.T) {
	if err := pgmigratetest.CheckEmbedsEveryFile(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The census's textual reading of which tables a migration touches is held to the broker's set
// too: every name it finds is a table the set itself creates.
func TestTouchedTablesOfEveryMigrationAreTablesTheSetCreates(t *testing.T) {
	if err := pgmigratetest.CheckTouchedTablesAreKnown(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// And to what every broker migration locks, applied in order to an empty schema
// (pgmigratetest.CheckTouchedTablesAgainstLocks).
func TestTouchedTablesOfEveryMigrationAreTheTablesItLocks(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t)
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if err := pgmigratetest.CheckTouchedTablesAgainstLocks(ctx, conn, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The broker's runner applies its whole set in one transaction, so a set it accepted with two
// files numbered 2 would apply the first, pass over the second as already applied, and commit.
func TestMigrateRefusesTwoMigrationsSharingAVersionBeforeApplyingAny(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t)
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  {Data: []byte("create table guard_first (id integer)")},
		"migrations/0002_second.up.sql": {Data: []byte("create table guard_second (id integer)")},
		"migrations/0002_third.up.sql":  {Data: []byte("create table guard_third (id integer)")},
	}

	err := s.migrate(ctx, set)
	left := existingTables(t, s, "guard_first", "guard_second", "guard_third", "broker_schema_migrations")
	if err == nil {
		t.Errorf("migrate accepted two migrations numbered 2 and reported success; the tables it left: %v", left)
	} else {
		for _, want := range []string{"0002_second.up.sql", "0002_third.up.sql", "version 2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("migrate error = %q, want it to name %q", err, want)
			}
		}
	}
	if len(left) != 0 {
		t.Errorf("the refused set left %v behind; want nothing applied, not even the version table", left)
	}
}

func TestMigrateRefusesNonTransactionalFilesBeforeApplyingAnything(t *testing.T) {
	s := emptySchemaStore(t)
	set := fstest.MapFS{
		"migrations/0001_first.up.sql": {Data: []byte("create table guard_first (id integer)")},
		"migrations/0002_index.up.sql": {Data: []byte("-- pgmigrate:non-transactional\ncreate index concurrently if not exists guard_index on guard_first (id)")},
	}
	err := s.migrate(context.Background(), set)
	if err == nil || !strings.Contains(err.Error(), "0002_index.up.sql is non-transactional") {
		t.Fatalf("migrate = %v, want explicit unsupported-mode refusal", err)
	}
	if left := existingTables(t, s, "guard_first", "broker_schema_migrations"); len(left) != 0 {
		t.Fatalf("refused set applied %v", left)
	}
}

// A broker migration whose lock another transaction holds gives up once the lock timeout runs
// out, naming itself, the lock and its holder, rather than waiting for as long as the holder lives
// with every broker request on that table queued behind it.
func TestMigrateBoundsTheWaitForALockAnotherTransactionHolds(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t)
	if _, err := s.Pool.Exec(ctx, "create table guard_held (id integer)"); err != nil {
		t.Fatalf("create the held table: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, "lock table guard_held in access share mode"); err != nil {
		t.Fatalf("hold the table: %v", err)
	}
	set := fstest.MapFS{
		"migrations/0001_widen_held.up.sql": {Data: []byte("alter table guard_held add column wider integer")},
	}

	bounded := testwait.Context(t)
	err = s.migrate(bounded, set)
	if bounded.Err() != nil {
		t.Fatalf("migrate waited for the held lock until the shared wait's own bound cancelled it (%v): nothing bounds a migration's lock wait", err)
	}
	if err == nil {
		t.Fatal("migrate applied a migration whose lock another transaction held")
	}
	// Postgres's own lock_timeout cancelled it, the one Exec sets to pgmigrate.LockTimeout
	// before every migration: a wait that ended any other way — the holder letting go, a
	// statement timeout, this test's context — is not the bound this row is about.
	var lockTimeout *pgmigrate.LockTimeoutError
	if !errors.As(err, &lockTimeout) {
		t.Fatalf("migrate gave up with %v; want a *pgmigrate.LockTimeoutError, Postgres cancelling it at the %s lock_timeout Exec sets", err, pgmigrate.LockTimeout)
	}
	holderPID := strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	for _, want := range []string{"0001_widen_held.up.sql", "AccessExclusiveLock", "guard_held", "pid " + holderPID, "pg_stat_activity"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migrate error = %q, want it to name %q", err, want)
		}
	}
	if left := existingTables(t, s, "broker_schema_migrations"); len(left) != 0 {
		t.Errorf("the migration that gave up committed %v; want its whole transaction rolled back", left)
	}
}

// The runner pins its transaction (pgmigrate.Pin) right after its advisory lock, so the statements
// it runs before the first migration's own pin are bounded by LockTimeout too: the create table of
// broker_schema_migrations and the first applied check. Here another connection holds ACCESS
// EXCLUSIVE on an existing broker_schema_migrations in an open transaction. The first applied
// check is the statement that waits: on PostgreSQL 18.6, create table if not exists takes no lock
// on a table that exists, and the check reads the table under ACCESS SHARE. migrate gives up after
// LockTimeout with a lock-timeout error (SQLSTATE 55P03) rather than wait for as long as the holder
// lives.
//
// Control: remove the pgmigrate.Pin call after the advisory lock in migrate, and migrate waits
// until the shared wait's own bound cancels it.
func TestMigrateBoundsTheWaitOfItsFirstAppliedCheck(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t)
	if _, err := s.Pool.Exec(ctx, "create table broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now())"); err != nil {
		t.Fatalf("create the version table: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect the holder: %v", err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the holder's transaction: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, "lock table broker_schema_migrations in access exclusive mode"); err != nil {
		t.Fatalf("hold the version table: %v", err)
	}
	set := fstest.MapFS{
		"migrations/0001_guard_first.up.sql": {Data: []byte("create table guard_first (id integer)")},
	}

	bounded := testwait.Context(t)
	started := time.Now()
	err = s.migrate(bounded, set)
	waited := time.Since(started)
	if bounded.Err() != nil {
		t.Fatalf("migrate waited %s for the held version table, until the shared wait's own bound cancelled it (%v): nothing bounds the runner's first applied check", waited.Round(time.Millisecond), err)
	}
	// Postgres's own lock_timeout cancelled it, the one Pin sets to pgmigrate.LockTimeout right
	// after the runner's advisory lock: a wait that ended any other way — the holder letting go, a
	// statement timeout, this test's context — is not the bound this row is about.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("migrate returned %v after %s; want a lock-timeout error (SQLSTATE 55P03), Postgres cancelling it at the %s lock_timeout Pin sets", err, waited.Round(time.Millisecond), pgmigrate.LockTimeout)
	}
	// And that lock_timeout is LockTimeout and no shorter. A loaded machine only makes the wait
	// longer, so this check cannot fail for load. A check that the wait ended soon after
	// LockTimeout could, so the shared wait's bound above is what holds the wait from above.
	if waited < pgmigrate.LockTimeout {
		t.Errorf("migrate gave up after %s, before the %s lock timeout: the first applied check ran under a shorter lock_timeout", waited.Round(time.Millisecond), pgmigrate.LockTimeout)
	}
	if left := existingTables(t, s, "guard_first"); len(left) != 0 {
		t.Errorf("the run that gave up committed %v; want its whole transaction rolled back", left)
	}
}

// A migration whose own text runs a session SET, one without LOCAL, leaves that setting on its
// connection past the commit. The runner resets that connection's session before it goes back to
// the pool (releaseMigrationConn, pgmigrate.ResetSession), so after migrate returns no connection
// the pool can give carries the migration's statement_timeout or search_path. The test holds as
// many connections as the pool can give at once (its MaxConns) and reads each, because the
// migration ran on one of them. The pool is capped at 4, which is what pgxpool's default
// (max(4, NumCPU)) gives on CI's 4-CPU runner: left at that default on a many-CPU machine, the
// pool asks for more connections than a Postgres at its default max_connections (100) has, as it
// did for 288 on a 288-CPU box. The store's database is the test's own (emptySchemaStore), and no
// connection option sets its search_path, so the reset gives back the server's default, which a
// new connection reads too. The control runs the same SETs in a committed transaction on a
// connection of its own and reads them back: the SETs survive a commit, so the row shows that the
// reset clears them.
//
// Control: remove the pgmigrate.ResetSession call from releaseMigrationConn, so the connection
// goes back as it is, and this fails: a pooled connection reads 77s and the row's schema.
func TestMigrateResetsTheSessionSettingsAMigrationLeftOnItsConnection(t *testing.T) {
	ctx := context.Background()
	s := emptySchemaStore(t, "pool_max_conns=4")
	// The schema is the row's own: the database it lives in is dropped when the row ends.
	schema := "broker_migrate_leak"
	leaked := schema + ", public"
	set := fstest.MapFS{
		"migrations/0001_schema.up.sql": {Data: []byte("create schema " + schema)},
		"migrations/0002_leak.up.sql":   {Data: []byte("set statement_timeout = '77s'; set search_path = " + leaked)},
	}
	if err := s.migrate(ctx, set); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	const read = "select current_setting('statement_timeout'), current_setting('search_path')"

	// A new connection reads the values that the reset gives, which every pooled connection must
	// read after migrate.
	fresh, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("connect a new connection: %v", err)
	}
	t.Cleanup(func() { fresh.Close(context.Background()) })
	var wantTimeout, wantSearchPath string
	if err := fresh.QueryRow(ctx, read).Scan(&wantTimeout, &wantSearchPath); err != nil {
		t.Fatalf("read a new connection's settings: %v", err)
	}
	if wantTimeout == "77s" || wantSearchPath == leaked {
		t.Fatalf("a new connection already reads the migration's settings (%q, %q), so the row proves nothing", wantTimeout, wantSearchPath)
	}

	acquire := testwait.Context(t)
	held := make([]*pgxpool.Conn, 0, s.Pool.Config().MaxConns)
	t.Cleanup(func() {
		for _, conn := range held {
			conn.Release()
		}
	})
	for i := range s.Pool.Config().MaxConns {
		conn, err := s.Pool.Acquire(acquire)
		if err != nil {
			t.Fatalf("acquire pooled connection %d of %d: %v", i+1, s.Pool.Config().MaxConns, err)
		}
		held = append(held, conn)
	}
	for i, conn := range held {
		var timeout, searchPath string
		if err := conn.QueryRow(ctx, read).Scan(&timeout, &searchPath); err != nil {
			t.Fatalf("read pooled connection %d's settings: %v", i+1, err)
		}
		if timeout != wantTimeout || searchPath != wantSearchPath {
			t.Errorf("pooled connection %d reads statement_timeout %q and search_path %q; want %q and %q, the values the reset gives: the migration's session SET reached the pool", i+1, timeout, searchPath, wantTimeout, wantSearchPath)
		}
	}

	control, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatalf("control: connect: %v", err)
	}
	t.Cleanup(func() { control.Close(context.Background()) })
	if _, err := control.Exec(ctx, "begin; set statement_timeout = '77s'; set search_path = "+leaked+"; commit"); err != nil {
		t.Fatalf("control: run the SETs in a committed transaction: %v", err)
	}
	var controlTimeout, controlSearchPath string
	if err := control.QueryRow(ctx, read).Scan(&controlTimeout, &controlSearchPath); err != nil {
		t.Fatalf("control: read the settings: %v", err)
	}
	if controlTimeout != "77s" || controlSearchPath != leaked {
		t.Fatalf("control: after a committed SET the connection reads %q and %q; want 77s and %q: the SETs do not survive a commit, so the row proves nothing", controlTimeout, controlSearchPath, leaked)
	}
}

// emptySchemaStore opens a store on an empty database of the test's own (ownDatabase), whose
// public schema records no version yet, with params, extra connection parameters as key=value
// (e.g. "pool_max_conns=4"), added to the URL. A test that migrates a set of its own needs one: a
// schema that already recorded the broker's real versions would pass over the test set's numbers
// as applied.
func emptySchemaStore(t *testing.T, params ...string) *Store {
	t.Helper()
	databaseURL := ownDatabase(t)
	for _, param := range params {
		key, value, ok := strings.Cut(param, "=")
		if !ok {
			t.Fatalf("connection parameter %q is not key=value", param)
		}
		databaseURL = testdb.WithParameter(t, databaseURL, key, value)
	}
	s, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(s.Pool.Close)
	return s
}

func adminExec(t *testing.T, databaseURL, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("%s: %v", statement, err)
	}
}

// existingTables reports which of names exist on the store's search path.
func existingTables(t *testing.T, s *Store, names ...string) []string {
	t.Helper()
	rows, err := s.Pool.Query(context.Background(),
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
