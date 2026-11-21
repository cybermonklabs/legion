package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
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

// And to what every broker migration locks, applied in order to an empty database
// (pgmigratetest.CheckTouchedTablesAgainstLocks).
func TestTouchedTablesOfEveryMigrationAreTheTablesItLocks(t *testing.T) {
	ctx := context.Background()
	s := emptyDatabaseStore(t)
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
	s := emptyDatabaseStore(t)
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
	s := emptyDatabaseStore(t)
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
	lockTimeout := pgmigrate.LockTimeout
	ctx := context.Background()
	s := emptyDatabaseStore(t)
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

	bound := 6 * lockTimeout
	bounded, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	started := time.Now()
	err = s.migrate(bounded, set)
	waited := time.Since(started)
	if bounded.Err() != nil {
		t.Fatalf("migrate waited %s for the held lock, until the test's own %s bound cancelled it (%v): nothing bounds a migration's lock wait", waited.Round(time.Millisecond), bound, err)
	}
	if err == nil {
		t.Fatalf("migrate applied a migration whose lock another transaction held, after %s", waited.Round(time.Millisecond))
	}
	if waited < lockTimeout || waited > lockTimeout+10*time.Second {
		t.Errorf("migrate gave up after %s, want about the %s lock timeout", waited.Round(time.Millisecond), lockTimeout)
	}
	t.Logf("migrate gave up after %s: %v", waited.Round(time.Millisecond), err)
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

// The runner pins each of its transactions (pgmigrate.Locking.Pin) right after its advisory lock,
// so the statements it runs on broker_schema_migrations before the first migration's own pin are
// bounded by LockTimeout too. Here another connection holds ACCESS EXCLUSIVE on an existing
// broker_schema_migrations in an open transaction. On PostgreSQL 18.6, create table if not exists
// takes no lock on a table that exists, and neither does the read of its columns from
// information_schema, so the statement that waits is the first that touches the table:
//
//   - on a table an earlier runner created, the ALTER TABLE that adds the identity columns, in the
//     runner's first transaction, under ACCESS EXCLUSIVE. That transaction is tried again on a
//     timeout (pgmigrate.Locking.RetryVersionTableLockTimeouts), so a holder that stays fails the
//     boot after LockTimeoutAttempts waits, and one that ends after the first, as a pg_dump does,
//     lets the boot go on and apply its migration;
//   - on a table this runner created, the read of every recorded row, in its second transaction,
//     under ACCESS SHARE, which is not tried again: that transaction applies every migration.
//
// A boot that gives up fails with a lock-timeout error (SQLSTATE 55P03) rather than wait for as
// long as the holder lives, and the error does not read as a refusal of the recorded rows.
//
// Control: remove the Pin call from lockAndPin, and migrate waits until the test's own deadline
// cancels it. Remove the retry, and the third row fails on the first attempt's timeout.
//
// Serial: its assertion is a time - migrate gives up within ten seconds of each row's bound, one
// LockTimeout or every attempt's and the pauses between them.
func TestMigrateBoundsTheWaitOfItsFirstVersionTableStatement(t *testing.T) {
	lockTimeout := pgmigrate.LockTimeout
	attempts := time.Duration(pgmigrate.LockTimeoutAttempts)
	retried := attempts*lockTimeout + (attempts-1)*pgmigrate.LockTimeoutRetryPause
	earlier := "create table broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now())"
	for name, tc := range map[string]struct {
		table   string
		wait    time.Duration
		release bool
	}{
		"an earlier runner's table":                  {table: earlier, wait: retried},
		"this runner's table":                        {table: "create table broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now(), " + pgmigrate.IdentityColumns + ")", wait: lockTimeout},
		"a holder that ends after the first attempt": {table: earlier, release: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := emptyDatabaseStore(t)
			if _, err := s.Pool.Exec(ctx, tc.table); err != nil {
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
			if _, err := held.Exec(ctx, "lock table broker_schema_migrations in access exclusive mode"); err != nil {
				t.Fatalf("hold the version table: %v", err)
			}
			released := make(chan struct{})
			if tc.release {
				go func() {
					defer close(released)
					time.Sleep(lockTimeout + pgmigrate.LockTimeoutRetryPause/2)
					_ = held.Rollback(context.Background())
				}()
			} else {
				close(released)
				defer held.Rollback(ctx)
			}
			defer func() { <-released }()
			set := fstest.MapFS{
				"migrations/0001_guard_first.up.sql": {Data: []byte("create table guard_first (id integer)")},
			}

			bound := 6 * lockTimeout
			bounded, cancel := context.WithTimeout(ctx, bound)
			defer cancel()
			started := time.Now()
			err = s.migrate(bounded, set)
			waited := time.Since(started)
			if bounded.Err() != nil {
				t.Fatalf("migrate waited %s for the held version table, until the test's own %s bound cancelled it (%v): nothing bounds the runner's first statement on the version table", waited.Round(time.Millisecond), bound, err)
			}
			if tc.release {
				if err != nil {
					t.Fatalf("migrate returned %v after %s; want the attempt after the holder ended to go through", err, waited.Round(time.Millisecond))
				}
				if waited < lockTimeout {
					t.Errorf("migrate returned after %s; want it to have waited out the first attempt (%s)", waited.Round(time.Millisecond), lockTimeout)
				}
				if left := existingTables(t, s, "guard_first"); len(left) != 1 {
					t.Errorf("the boot that went on did not apply its migration: tables %v", left)
				}
				return
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("migrate returned %v after %s; want a lock-timeout error (SQLSTATE 55P03)", err, waited.Round(time.Millisecond))
			}
			if waited < tc.wait || waited > tc.wait+10*time.Second {
				t.Errorf("migrate gave up after %s, want about %s", waited.Round(time.Millisecond), tc.wait)
			}
			t.Logf("migrate gave up after %s: %v", waited.Round(time.Millisecond), err)
			if pgmigrate.IsRefusal(err) || strings.Contains(err.Error(), "migrations refused") {
				t.Errorf("migrate = %v; a lock timeout reads as a refusal of the recorded rows, whose remedy is a reset", err)
			}
			if left := existingTables(t, s, "guard_first"); len(left) != 0 {
				t.Errorf("the run that gave up committed %v; want its whole transaction rolled back", left)
			}
		})
	}
}

// On a broker_schema_migrations an earlier runner created, the ALTER TABLE that adds the identity
// columns takes ACCESS EXCLUSIVE on the table, and a request's read of the table
// (requests.writtenBeforeMigration13) queues behind that lock. The runner commits the ALTER in a
// short transaction of its own, so the lock is gone before any pending migration runs: here the
// pending migration records, while it runs, how many ACCESS EXCLUSIVE locks on the table are
// granted, and finds none. The control is a table this runner created, which needs no ALTER and
// reads none either; with the ALTER back in the run's one transaction, the first row reads 1.
func TestMigrateReleasesTheVersionTablesAlterLockBeforeItsMigrations(t *testing.T) {
	for name, table := range map[string]string{
		"an earlier runner's table":    "create table broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now())",
		"control: this runner's table": "create table broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now(), " + pgmigrate.IdentityColumns + ")",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := emptyDatabaseStore(t)
			if _, err := s.Pool.Exec(ctx, table); err != nil {
				t.Fatalf("create the version table: %v", err)
			}
			set := fstest.MapFS{"migrations/0001_lock_probe.up.sql": {Data: []byte(
				"create table lock_probe as select count(*) as held from pg_locks where relation = 'broker_schema_migrations'::regclass and mode = 'AccessExclusiveLock' and granted")}}
			if err := s.migrate(ctx, set); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			var held int
			if err := s.Pool.QueryRow(ctx, "select held from lock_probe").Scan(&held); err != nil {
				t.Fatalf("read the probe: %v", err)
			}
			if held != 0 {
				t.Errorf("while the pending migration ran, %d ACCESS EXCLUSIVE locks on broker_schema_migrations were granted; want 0: the ALTER's lock is held through the migrations", held)
			}
		})
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
// did for 288 on a 288-CPU box. The store's database is the test's own (emptyDatabaseStore) and
// its URL names no search_path, so the reset gives back the server's default, which a new
// connection reads too; the schema the migration creates goes with that database. The control
// runs the same SETs in a committed transaction on a connection of its own and reads them back:
// the SETs survive a commit, so the row shows that the reset clears them.
//
// Control: remove the pgmigrate.ResetSession call from releaseMigrationConn, so the connection
// goes back as it is, and this fails: a pooled connection reads 77s and the row's schema.
func TestMigrateResetsTheSessionSettingsAMigrationLeftOnItsConnection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := emptyDatabaseStore(t, "pool_max_conns=4")
	schema := "broker_migrate_leak_" + randomSuffix(t)
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

	acquire, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
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

// testDatabaseURL is the BROKER_TEST_DATABASE_URL server the rows below make their databases on,
// skipping t when it is unset. The external test package reaches its databases through
// storetest; these rows cannot, since they drive the unexported migrate and the embedded set,
// and storetest imports this package, so they name the variable themselves.
func testDatabaseURL(t *testing.T) string {
	url := os.Getenv("BROKER_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("BROKER_TEST_DATABASE_URL must be set to run Postgres store tests")
	}
	return url
}

// emptyDatabaseStore opens a store on a database of its own on the BROKER_TEST_DATABASE_URL
// server, created empty and dropped when t ends, with params, extra connection parameters as
// key=value (e.g. "pool_max_conns=4"), added to the URL. A test that migrates a set of its own
// needs one: a database already carrying the broker's real versions would pass over the test
// set's numbers as applied. It is a database rather than a schema of a shared one because Migrate
// takes its advisory lock database-wide, so two rows migrating their own schemas of one database
// would still queue on each other, and because a row here holds a table lock until its own
// migration gives up (dispatch://INFRA-1075).
func emptyDatabaseStore(t *testing.T, params ...string) *Store {
	t.Helper()
	raw := testDatabaseURL(t)
	// A file copy of template1 (docs/testing.md, "A database a Go test creates").
	database := "broker_migrate_test_" + randomSuffix(t)
	adminExec(t, raw, "create database "+database+" strategy file_copy")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse BROKER_TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + database
	query := parsed.Query()
	for _, param := range params {
		key, value, ok := strings.Cut(param, "=")
		if !ok {
			t.Fatalf("connection parameter %q is not key=value", param)
		}
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	s, err := Open(context.Background(), parsed.String())
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	// One cleanup, so the order is on the page: the pool closes, then the database is dropped.
	// pgxpool.Close returns once it has asked its connections to go, and the server has not
	// always finished closing the backend by then, so the drop is tried again for a moment
	// rather than forced: a drop that terminates another session needs a privilege the server
	// need not grant the test's role, and there is nothing here to terminate anyway.
	t.Cleanup(func() {
		s.Pool.Close()
		var err error
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if err = adminTry(raw, "drop database "+database); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("drop database %s: %v", database, err)
				return
			}
		}
	})
	return s
}

// adminTry runs statement on the BROKER_TEST_DATABASE_URL server and answers its error, for a
// caller that retries rather than failing t at the first one.
func adminTry(databaseURL, statement string) error {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, statement)
	return err
}

// randomSuffix is 16 random hex digits, for a name of the test's own on a server that other tests
// share.
func randomSuffix(t *testing.T) string {
	t.Helper()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random name: %v", err)
	}
	return hex.EncodeToString(suffix[:])
}

func adminExec(t *testing.T, databaseURL, statement string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to BROKER_TEST_DATABASE_URL: %v", err)
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
