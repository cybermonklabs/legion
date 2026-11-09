// Package store owns Dispatch's Postgres connection and schema migrations.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/duty"
	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Store is the shared Dispatch database handle.
type Store struct {
	Pool *Pool
	// Locking is the discipline Migrate applies each migration under: how long it waits for a
	// lock, how many attempts it makes of one whose wait outlasts that, and how long it holds
	// nothing between them. The zero value is production's (pgmigrate.DefaultLocking); the tests
	// of the runner itself set their own short, so that proving the bound, the retry and the
	// give-up costs a fraction of a second rather than three five-second waits.
	Locking pgmigrate.Locking
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

// Option configures the pool Open makes.
type Option func(*pgxpool.Config)

// WithMaxConns bounds the shared pool at conns rather than at sharedPoolSize. It is for the test
// harness, which holds one store per test database in one process
// (internal/dispatch/store/storetest) and would otherwise ask the one Postgres its packages share
// for sharedPoolSize connections per store, more than that server's own limit allows at once.
// Nothing a deployment sets reaches it: how many connections Dispatch holds is a property of
// Dispatch, picked in code (Open, sharedPoolSize).
func WithMaxConns(conns int32) Option {
	return func(config *pgxpool.Config) { config.MaxConns = conns }
}

// Open connects to databaseURL and verifies the database is reachable. The pool's size is
// sharedPoolSize unless an option replaces it: how many connections Dispatch may hold is a
// property of Dispatch, not of the string that names its database. Every connection it opens
// scans timestamptz in UTC (scanTimestampsInUTC), as do the pools Pool.separate copies from its
// configuration.
func Open(ctx context.Context, databaseURL string, options ...Option) (*Store, error) {
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
	for _, option := range options {
		option(config)
	}
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
// the same advisory key until their version is recorded. Every wait for that key polls, for the
// reason lockMigrations gives. It refuses a set pgmigrate.Load refuses before touching the
// database, and bounds every migration's lock waits by s.Locking's timeout,
// pgmigrate.LockTimeout unless a caller set its own.
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

// MigrationsFingerprint is pgmigrate.Fingerprint of the set Migrate applies, and the error
// Migrate would refuse that set with. The test harness names the template database every test's
// database is cloned from after it (internal/dispatch/store/storetest), so the test processes of
// one run share one migrated template; nothing in production reads it.
func MigrationsFingerprint() (string, error) {
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		return "", fmt.Errorf("migrations refused, none applied: %w", err)
	}
	return pgmigrate.Fingerprint(migrations), nil
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

// migrationLockKey is the advisory key every Dispatch runner takes, session-level, on a
// connection that serves nothing else while it is held: createVersionTable and
// applyMigrationOnce through inMigrationLockTx, applyNonTransactionalMigration on its own
// hijacked connection. The auto-deployer (deploy/scripts/autodeploy.sh, schema_applied) polls the
// same key before reading schema_migrations and reads only once a try of its own is granted, so a
// migration in flight when it starts is waited out only if it ends inside the deployer's
// 60-second deadline; a migration still holding the key then is not waited out at all - that read
// fails and the deployer refuses to deploy rather than read behind a held key. A change of key
// must change both: TestTheDeployerTakesTheMigrationKeyThisPackageDefines reads the script's own
// key and fails when only one of the two moves.
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
func lockMigrations(ctx context.Context, conn *pgx.Conn) error {
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

// inMigrationLockTx runs fn under migrationLockKey, inside a transaction of its own on the
// connection that holds the key, and owns the whole sequence around it in one fixed order:
// acquire a pooled connection, mark the context as holding it, take the key by polling
// (lockMigrations), begin, run fn, end the transaction, release the key and give the connection
// back - or, where the connection's lock state is unknown, close it out of the pool.
//
// fn follows pgx.BeginFunc's rule: nil commits, an error rolls back.
func (s *Store) inMigrationLockTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// From here the caller holds a connection of this pool, so the pool refuses it a second one
	// exactly as it refuses one asked for inside a transaction (ErrNestedAcquire). The mark goes
	// on after the acquisition, which would otherwise refuse itself.
	ctx, unmark := HoldsConnection(ctx)
	defer unmark()
	if err := lockMigrations(ctx, conn.Conn()); err != nil {
		// This connection may hold the key: a server-side error can arrive after the grant - a
		// pg_cancel_backend caught at the executor's next interrupt check, say - and the statement
		// that failed is then the one that took it. Unlocking instead would not settle that, since
		// pg_advisory_unlock answers false both on the session that never took the key, which is
		// the ordinary failure here, and on the one whose unlock did not land, and the answer a
		// connection must not go back on is the same false. So this closes on every failure, which
		// ends the session and the key with it, and costs one connection on a path whose boot has
		// failed anyway. (A cancelled Go context needs none of this - pgx closes the socket - but
		// it cannot be told apart from the rest here either.)
		closeBounded(ctx, conn.Hijack())
		return err
	}
	defer unlockMigrations(conn)
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return fn(ctx, tx) })
}

// unlockMigrations gives migrationLockKey and conn back through duty.FreeSessionLock, which
// returns conn to the pool only once Postgres answers true, so a connection that may still hold
// the key never serves another caller and holds every later boot's migrations behind it.
//
// duty.ErrSessionClosed is logged at INFO: a boot cancelled inside the transaction has already
// closed the connection, over its in-flight statement's failure or over BeginFunc's rollback on
// the cancelled context, and the session's end releases the key. Every other error is an ERROR,
// because the key may still be held by a session the unlock could not reach.
func unlockMigrations(conn *pgxpool.Conn) {
	err := duty.FreeSessionLock(conn, "select pg_advisory_unlock($1)", migrationLockKey)
	switch {
	case err == nil:
	case errors.Is(err, duty.ErrSessionClosed):
		slog.Info("dispatch: the migration advisory lock's connection was closed before the unlock; its session ended the key",
			"key", migrationLockKey, "error", err)
	default:
		slog.Error("dispatch: the migration advisory lock was not released; its connection leaves the pool",
			"key", migrationLockKey, "error", err)
	}
}

// closeBounded closes conn on a context a cancelled ctx does not cancel, bounded like the rest of
// this runner's waits (pgmigrate.LockTimeout). Its two callers are this runner's own: the failed
// lock, which hands over the connection it hijacks out of the pool, and the concurrent build,
// which owns its connection.
func closeBounded(ctx context.Context, conn *pgx.Conn) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
	defer cancel()
	_ = conn.Close(cleanup)
}

// createVersionTable creates schema_migrations under the runner's advisory lock
// (inMigrationLockTx), as the broker's runner creates its own, and, when widen is set, widens
// the version column of a database that has the table from before the versions became
// timestamps. Created outside the lock, two processes booting at once against an empty database
// race `create table if not exists` on the catalog, and the loser exits on pg_type's unique index.
//
// The table and its widening commit in one transaction, the one inMigrationLockTx opens on the
// connection that holds the key.
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
	return s.inMigrationLockTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			create table if not exists schema_migrations (
				version bigint primary key,
				applied_at timestamptz not null default now()
			)
		`); err != nil {
			return fmt.Errorf("create schema migrations table: %w", err)
		}
		if !widen {
			return nil
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
		return nil
	})
}

// applyMigration applies one migration, trying it again while its lock wait outlasts
// lock_timeout, up to s.Locking's attempts, holding nothing between attempts
// (pgmigrate.Locking.RetryLockTimeouts). A concurrent build's retry first drops an INVALID
// index of the migration's own name the previous attempt left on the migration's own table,
// then builds the index again. A migration that times out on every attempt fails the runner
// and boot with it.
func (s *Store) applyMigration(ctx context.Context, migration pgmigrate.Migration) error {
	return s.Locking.RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		return s.applyMigrationOnce(ctx, migration)
	})
}

func (s *Store) applyMigrationOnce(ctx context.Context, migration pgmigrate.Migration) error {
	if migration.NonTransactional {
		return s.applyNonTransactionalMigration(ctx, migration)
	}
	// A migration is a transaction like any other, so it is marked like any other: nothing it
	// runs may take a second pooled connection while it is open. The key is taken first, on the
	// connection the transaction then runs on (inMigrationLockTx), so the migration and its
	// version record still commit together, and it is released before RetryLockTimeouts pauses
	// between attempts.
	return s.inMigrationLockTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var applied bool
		// The parameter is cast, here and in the insert below, because pgx encodes it for the
		// column it finds: a database whose column this boot is about to widen, or one widened by
		// a boot whose statement this connection has not prepared again, would otherwise refuse a
		// 14-digit version as too large for an int4.
		if err := tx.QueryRow(ctx, "select exists(select 1 from schema_migrations where version = $1::bigint)", migration.Version).Scan(&applied); err != nil {
			return fmt.Errorf("check migration: %w", err)
		}
		if applied {
			return nil
		}
		if err := s.Locking.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
			return fmt.Errorf("execute migration: %w", err)
		}
		if _, err := tx.Exec(ctx, "insert into schema_migrations (version) values ($1::bigint)", migration.Version); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
		return nil
	})
}

// applyNonTransactionalMigration owns its connection so a cancellation or failed cleanup cannot
// return a session-level advisory lock to the pool. Session and transaction advisory locks on the
// same key conflict, so this excludes older runners and schema-version readers too. Closing the
// connection releases the lock only after the statement and its separate version record finish.
//
// The key is taken by polling here too (lockMigrations), and this is the site whose build the
// polling exists for: it holds the key while CREATE INDEX CONCURRENTLY runs.
func (s *Store) applyNonTransactionalMigration(ctx context.Context, migration pgmigrate.Migration) error {
	pooled, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer closeBounded(ctx, conn)
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
