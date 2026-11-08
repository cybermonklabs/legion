// Package store owns Dispatch's Postgres connection and schema migrations.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Store is the shared Dispatch database handle.
type Store struct {
	Pool *Pool
}

// Every file in migrations, so pgmigrate.Load judges each one; pgmigratetest.CheckEmbedsEveryFile
// states why the pattern is all:migrations and holds the tree to it.
//
//go:embed all:migrations
var migrationFiles embed.FS

// poolSizeParam is the connection-string parameter Open refuses. Open fixes MaxConns at
// sharedPoolSize, so a connection string that asks for a pool size is asking for something
// that will not happen, and ignoring it silently would leave the operator tuning a number
// nobody reads.
//
// The other pool parameters are not refused because they are not ignored: the shared pool
// keeps whatever minimums the connection string sets, and only the rooms and health pools
// override them, deliberately and per pool (Pool.separate).
const poolSizeParam = "pool_max_conns"

// Open connects to databaseURL and verifies the database is reachable. The pool's size is
// sharedPoolSize: how many connections Dispatch may hold is a property of Dispatch, not of the
// string that names its database. Every connection it opens scans timestamptz in UTC
// (scanTimestampsInUTC), as do the pools Pool.separate copies from its configuration.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	declared, err := declaresPoolSize(databaseURL)
	if err != nil {
		return nil, err
	}
	if declared {
		return nil, fmt.Errorf(
			"DATABASE_URL sets %s, which Dispatch does not honour: the pool is fixed at %d in code (store.sharedPoolSize). Remove the parameter",
			poolSizeParam, sharedPoolSize)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse Postgres URL: %w", err)
	}
	config.MaxConns = sharedPoolSize
	config.AfterConnect = scanTimestampsInUTC
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &Store{Pool: NewPool(pool)}, nil
}

// scanTimestampsInUTC makes every timestamptz a connection scans come back in UTC, whatever zone
// the host runs in. pgx builds a scanned timestamptz with time.Unix, which carries time.Local,
// and time.Time marshals JSON in the zone it carries - so one instant left Dispatch as
// "2026-10-02T00:00:01.12Z" from a UTC host and "2026-10-01T17:00:01.12-07:00" from a Pacific
// one, and every reader that compares a timestamp as a string (the dashboard's read-through
// mark, a client ordering replies written in the same second) read two different servers. Only
// the zone the value carries changes; the instant does not. The connection's own session
// timezone is Postgres's and is untouched, so what a timestamptz is compared with and stored as
// is unchanged too.
//
// Registered per connection rather than in pgtype's default map, which is shared with every
// other pgx user in the process, and for the array and range types over timestamptz as well:
// each holds the element type as it was registered when the map was built, so re-registering the
// element alone would leave a timestamptz[] scanning in the host's zone.
func scanTimestampsInUTC(_ context.Context, conn *pgx.Conn) error {
	types := conn.TypeMap()
	timestamptz := &pgtype.Type{
		Name: "timestamptz", OID: pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	}
	tstzrange := &pgtype.Type{
		Name: "tstzrange", OID: pgtype.TstzrangeOID, Codec: &pgtype.RangeCodec{ElementType: timestamptz},
	}
	types.RegisterTypes([]*pgtype.Type{
		timestamptz,
		tstzrange,
		{Name: "_timestamptz", OID: pgtype.TimestamptzArrayOID, Codec: &pgtype.ArrayCodec{ElementType: timestamptz}},
		{Name: "_tstzrange", OID: pgtype.TstzrangeArrayOID, Codec: &pgtype.ArrayCodec{ElementType: tstzrange}},
		{Name: "tstzmultirange", OID: pgtype.TstzmultirangeOID, Codec: &pgtype.MultirangeCodec{ElementType: tstzrange}},
	})
	return nil
}

// declaresPoolSize reports whether databaseURL carries poolSizeParam. It asks pgx.ParseConfig
// rather than pgxpool.ParseConfig or the raw string. pgxpool is the layer that consumes the
// parameter - it folds it into MaxConns and deletes it from RuntimeParams - so its parse
// cannot answer the question, while the layer underneath it leaves the parameter there.
// Scanning the string instead would answer a different question: keyword/value values may be
// quoted or backslash-escaped and may contain the parameter's own name, and a keyword may be
// separated from its "=" by whitespace, so a scanner refuses valid passwords and misses
// "pool_max_conns = 4", which pgx honours.
func declaresPoolSize(databaseURL string) (bool, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return false, fmt.Errorf("parse Postgres URL: %w", err)
	}
	_, declared := config.RuntimeParams[poolSizeParam]
	return declared, nil
}

// Migrate applies embedded migrations in version order. Unmarked files commit with their
// version record in one transaction; marked concurrent indexes run outside a transaction under
// the same advisory key until their version is recorded. Every one of the three waits for that
// key polls pg_try_advisory_lock from here rather than waiting for it inside Postgres
// (lockMigrations), and holds it session-level on a connection that serves nothing else while it
// is held. It refuses a set pgmigrate.Load refuses before touching the database and bounds every
// migration's lock waits by LockTimeout.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	if s == nil || s.Pool == nil {
		return fmt.Errorf("migrate: store pool required")
	}
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	// Only a set holding a version past an integer widens the column: see createVersionTable.
	widen := slices.ContainsFunc(migrations, func(m pgmigrate.Migration) bool { return m.Version > math.MaxInt32 })
	if err := s.createVersionTable(ctx, widen); err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := s.applyMigration(ctx, migration); err != nil {
			return fmt.Errorf("apply migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

// migrationLockKey is the advisory key every Dispatch runner takes: session-level, at each of the
// three places a boot waits for it (createVersionTable, applyMigrationOnce and
// applyNonTransactionalMigration), on a connection of its own that goes back to the pool only
// once the key is released. The auto-deployer (deploy/scripts/autodeploy.sh, schema_version)
// takes it too before reading schema_migrations, so an in-flight migration ends first; a change
// of key changes both.
const migrationLockKey int64 = 8150001

// migrationLockFirstPause and migrationLockMaxPause bound the sleep lockMigrations takes between
// two tries for the key. The first is short enough that a boot queued behind a migration of a few
// hundred milliseconds starts within a tick of it, and the backoff keeps a boot queued behind a
// concurrent index build, which runs for minutes, from asking the database twenty times a second
// for all of them.
const (
	migrationLockFirstPause = 50 * time.Millisecond
	migrationLockMaxPause   = time.Second
)

// migrationLockConn is all lockMigrations needs of a connection: a session of its own that runs
// one statement at a time. The pooled connection the two transactional sites hold the key on and
// the hijacked one a concurrent build owns both satisfy it.
type migrationLockConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockMigrations takes migrationLockKey on conn, session-level, by polling: one
// pg_try_advisory_lock statement, and, while another runner holds the key, a sleep with nothing
// open on the connection before the next try. The wait is unbounded, as a boot's wait for a
// migration it must not run twice has to be; it ends when the key is granted or when ctx does,
// and a boot that waits at all says so in the log, because a silent unbounded wait tells an
// operator nothing.
//
// It cannot be the single blocking pg_advisory_lock call it replaces, inside a transaction or
// outside one. A backend blocked on a lock keeps the snapshot its statement took, and CREATE
// INDEX CONCURRENTLY and DROP INDEX CONCURRENTLY wait for every transaction holding a snapshot
// older than the phase they are in - so a runner blocked on this key while the holder builds a
// concurrent index waits for the holder and the holder waits for it, which PostgreSQL 18.6
// reports as a deadlock (SQLSTATE 40P01) and both boots fail on. Polling from the client is what
// breaks the cycle: between two tries the waiter has no statement and no snapshot open, so the
// holder's build sees it out at once.
func lockMigrations(ctx context.Context, conn migrationLockConn) error {
	started := time.Now()
	pause := migrationLockFirstPause
	waited := false
	for {
		var granted bool
		if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1)", migrationLockKey).Scan(&granted); err != nil {
			return fmt.Errorf("lock migrations: %w", err)
		}
		if granted {
			if waited {
				slog.Info("dispatch: the migration advisory lock was granted after a wait",
					"key", migrationLockKey, "waited", time.Since(started).String())
			}
			return nil
		}
		if !waited {
			waited = true
			slog.Info("dispatch: another runner holds the migration advisory lock; waiting for it",
				"key", migrationLockKey)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		pause = min(2*pause, migrationLockMaxPause)
	}
}

// lockMigrationsOnPooledConnection marks ctx as holding one of the shared pool's connections,
// takes that connection and holds migrationLockKey on it until the returned release runs. The
// release unlocks the key and gives the connection back, or keeps the connection out of the pool
// when the unlock did not answer true (unlockMigrations), and clears the mark; the caller runs it
// on every path. It hands back the marked context because the work the caller then does - its
// transaction, every statement in it - belongs on this same connection and under this mark.
func (s *Store) lockMigrationsOnPooledConnection(ctx context.Context) (context.Context, *pgxpool.Conn, func(), error) {
	ctx = WithTransactionTracking(ctx)
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return ctx, nil, nil, err
	}
	// From here the caller holds a connection of this pool, so the pool refuses it a second one
	// exactly as it refuses one asked for inside a transaction (ErrNestedAcquire).
	unmark := hold(ctx)
	if err := lockMigrations(ctx, conn); err != nil {
		conn.Release()
		unmark()
		return ctx, nil, nil, err
	}
	return ctx, conn, func() {
		unlockMigrations(ctx, conn)
		unmark()
	}, nil
}

// unlockMigrations releases migrationLockKey on conn and gives conn back to the pool. When the
// release errors or answers false the connection is taken out of the pool and closed instead:
// a connection that may still hold a session-level key must never serve another caller, who would
// hand the key on to a request and hold every later boot's migrations behind it. Closing it
// releases the key with the session. Like the concurrent builds' cleanup it runs on a context a
// cancelled ctx does not cancel, so a cancelled boot still gives back what it took.
func unlockMigrations(ctx context.Context, conn *pgxpool.Conn) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
	defer cancel()
	var released bool
	err := conn.QueryRow(cleanup, "select pg_advisory_unlock($1)", migrationLockKey).Scan(&released)
	if err == nil && released {
		conn.Release()
		return
	}
	slog.Error("dispatch: the migration advisory lock was not released; its connection leaves the pool",
		"key", migrationLockKey, "released", released, "error", err)
	_ = conn.Hijack().Close(cleanup)
}

// createVersionTable creates schema_migrations under the runner's advisory lock - taken by
// polling, session-level, on a connection of its own (lockMigrations), and released before that
// connection goes back to the pool - as the broker's runner creates its own, and, when widen is
// set, widens the version column of a database that has the table from before the versions became
// timestamps. Created outside the lock, two processes booting at once against an empty database
// race `create table if not exists` on the catalog, and the loser exits on pg_type's unique index.
//
// The table and its widening still commit in one transaction, opened on the connection that holds
// the key.
//
// The column is a bigint because a migration added since the set stopped being numbered is named
// for the UTC moment it was created, 14 digits, which is past the 2147483647 an integer column
// holds. A database created before this change has the integer the statement below used to create.
// It is widened only by a boot whose set holds such a version (widen), as the daemon widens its
// own in the first timestamped migration's transaction, and not by a boot that records nothing past
// an integer: the release before this change reads `max(version)` uncast, so a widening while its
// processes still serve fails that read once on every connection that prepared it ("cached plan
// must not change result type"). A release whose set first holds a timestamped migration is rolled
// out over processes of this one, which read the version cast. The type is read before the ALTER
// TABLE, which on a column already bigint rewrites nothing but still takes ACCESS EXCLUSIVE.
func (s *Store) createVersionTable(ctx context.Context, widen bool) error {
	ctx, conn, release, err := s.lockMigrationsOnPooledConnection(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		create table if not exists schema_migrations (
			version bigint primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		return fmt.Errorf("create schema migrations table: %w", err)
	}
	if !widen {
		return tx.Commit(ctx)
	}
	var dataType string
	if err := tx.QueryRow(ctx, `select data_type from information_schema.columns
		where table_schema = current_schema() and table_name = 'schema_migrations' and column_name = 'version'`).Scan(&dataType); err != nil {
		return fmt.Errorf("read the type of schema_migrations.version: %w", err)
	}
	if dataType == "integer" {
		if _, err := tx.Exec(ctx, "alter table schema_migrations alter column version type bigint"); err != nil {
			return fmt.Errorf("widen schema_migrations.version to bigint: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// applyMigration applies one migration, trying it again while its lock wait outlasts
// lock_timeout, up to pgmigrate.LockTimeoutAttempts times, holding nothing between attempts.
// A concurrent build's retry first drops an INVALID index of the migration's own name the
// previous attempt left on the migration's own table, then builds the index again.
// A migration that times out on every attempt fails the runner and boot with it.
func (s *Store) applyMigration(ctx context.Context, migration pgmigrate.Migration) error {
	return pgmigrate.RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		return s.applyMigrationOnce(ctx, migration)
	})
}

func (s *Store) applyMigrationOnce(ctx context.Context, migration pgmigrate.Migration) error {
	if migration.NonTransactional {
		return s.applyNonTransactionalMigration(ctx, migration)
	}
	// A migration is a transaction like any other, so it is marked like any other: nothing it
	// runs may take a second pooled connection while it is open. The key is taken first, by
	// polling, on the connection the transaction then runs on, so the migration and its version
	// record still commit together under a lock no concurrent index build can deadlock with
	// (lockMigrations), and it is released before RetryLockTimeouts pauses between attempts.
	ctx, conn, release, err := s.lockMigrationsOnPooledConnection(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var applied bool
	// The parameter is cast, here and in the insert below, because pgx encodes it for the column
	// it finds: a database whose column this boot is about to widen, or one widened by a boot
	// whose statement this connection has not prepared again, would otherwise refuse a 14-digit
	// version as too large for an int4.
	if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1::bigint)", migration.Version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	if applied {
		return tx.Commit(ctx)
	}
	if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
		return fmt.Errorf("execute migration: %w", err)
	}
	if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1::bigint)", migration.Version); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return tx.Commit(ctx)
}

// applyNonTransactionalMigration owns its connection so a cancellation or failed cleanup cannot
// return a session-level advisory lock to the pool. Session and transaction advisory locks on the
// same key conflict, so this excludes older runners and schema-version readers too. Closing the
// connection releases the lock only after the statement and its separate version record finish.
//
// The key is taken by polling (lockMigrations) rather than waited for: this is the site that
// holds it while CREATE INDEX CONCURRENTLY runs, and a runner blocked on the key keeps a snapshot
// the build waits for, so a blocking wait here is the deadlock the polling exists to avoid.
func (s *Store) applyNonTransactionalMigration(ctx context.Context, migration pgmigrate.Migration) error {
	pooled, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	if err := lockMigrations(ctx, conn); err != nil {
		return err
	}
	var applied bool
	if err := conn.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1::bigint)", migration.Version).Scan(&applied); err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	if applied {
		return nil
	}
	// Keep the version insert bounded too, just as the transactional runner's SET LOCAL
	// remains in force until commit. ExecNonTransactional restores this same session bound.
	if _, err := conn.Exec(ctx, "select set_config('lock_timeout', $1, false)", pgmigrate.LockTimeout.String()); err != nil {
		return fmt.Errorf("bound migration version lock waits: %w", err)
	}
	if err := pgmigrate.ExecNonTransactional(ctx, conn, migration, s.Pool.Config().BeforeConnect); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, "insert into schema_migrations (version) values ($1::bigint)", migration.Version); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}
	return nil
}
