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
	"github.com/sjawhar/envoy/pgdsn"
)

// Store is the shared Dispatch database handle.
type Store struct {
	Pool *Pool
	// afterNonTransactionalStatement runs on the connection a marked migration was applied on,
	// once its statement has returned and before its version is recorded, and its error fails
	// that migration. Only a test sets it: the runner closes that connection before Migrate
	// returns, so this is the one place the session that applied the statement can be asked
	// whether it held a transaction around it. Nil, which every Store Open makes leaves it,
	// runs nothing.
	afterNonTransactionalStatement func(ctx context.Context, conn *pgx.Conn) error
}

// Every file in migrations, so the store's tests judge each one with pgmigrate.Load
// (TestTheEmbeddedMigrationsPassEveryCheckLoadMakes) and a start reads each one with
// pgmigrate.LoadBuilt; pgmigratetest.CheckEmbedsEveryFile states why the pattern is all:migrations
// and holds the tree to it.
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

// DSNSetting names the setting Dispatch's DSN comes from, as the store's refusals name it (pgdsn).
const DSNSetting = "DATABASE_URL"

// Open connects to databaseURL and verifies the database is reachable. The pool's size is
// sharedPoolSize: how many connections Dispatch may hold is a property of Dispatch, not of the
// string that names its database. Every connection it opens scans timestamptz in UTC
// (scanTimestampsInUTC) and gives back a large statement's encoded arguments when it returns to
// its pool (argumentReleaser), as do the pools Pool.separate copies from its configuration. A URL
// holding an "@" after its user part, where a password holding an "@" or a "/" leaves part of
// itself, is refused by DSNSetting alone before anything connects (pgdsn.Refusal), and every
// failure to parse, open or reach the database is named without the URL's password
// (pgdsn.ConnectError), since Dispatch logs it.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if err := pgdsn.Refusal(DSNSetting, databaseURL); err != nil {
		return nil, err
	}
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
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	config.MaxConns = sharedPoolSize
	config.AfterConnect = scanTimestampsInUTC
	config.ConnConfig.Tracer = withArgumentRelease(config.ConnConfig.Tracer)
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, pgdsn.ConnectError(DSNSetting, databaseURL, err)
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
		return false, pgdsn.ConnectError(DSNSetting, databaseURL, err)
	}
	_, declared := config.RuntimeParams[poolSizeParam]
	return declared, nil
}

// Migrate applies embedded migrations in version order. Unmarked files commit with their
// version record in one transaction; marked concurrent indexes run outside a transaction under
// the same advisory key until their version is recorded. Every wait for that key polls, for the
// reason lockMigrations gives. It refuses a set pgmigrate.LoadBuilt refuses before touching the
// database, refuses a database whose schema_migrations records a version under another file's
// identity before applying anything (prepareVersionTable), and bounds every migration's lock
// waits by LockTimeout. A marked migration whose statement is not one idempotent concurrent index
// build is refused at its turn, after the pending migrations before it have been applied
// (pgmigrate.LoadBuilt says why the start reads the set that way).
func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

// MigrateThrough is Migrate for the embedded migrations numbered version or below alone: it leaves
// the schema a release whose newest migration is version leaves, and a later Migrate applies the
// rest, as the next release's boot does over that deployment. It refuses a version no embedded
// migration carries, which names no release. Tests read a store as an earlier release left it
// through it (cmd/dispatch's supertag node tests).
func (s *Store) MigrateThrough(ctx context.Context, version int) error {
	return s.migrateThrough(ctx, migrationFiles, version)
}

// MigrationText is the whole text of the embedded forward migration numbered version, comments
// included, as the runner applies it. It refuses a version no embedded forward migration carries,
// and one that two carry. A caller that names an operator's remedy reads a migration's statement
// here rather than keeping a second copy of it (graphcensus' refusal without its indexes).
func MigrationText(version int) (string, error) {
	names, err := fs.Glob(migrationFiles, fmt.Sprintf("migrations/%d_*.up.sql", version))
	if err != nil {
		return "", fmt.Errorf("find migration %d: %w", version, err)
	}
	if len(names) != 1 {
		return "", fmt.Errorf("find migration %d: %d embedded forward migrations carry it, want one", version, len(names))
	}
	text, err := migrationFiles.ReadFile(names[0])
	if err != nil {
		return "", fmt.Errorf("read migration %d: %w", version, err)
	}
	return string(text), nil
}

// versionTable is the table this runner records each applied migration in.
const versionTable = "schema_migrations"

func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	return s.migrateThrough(ctx, fsys, math.MaxInt)
}

func (s *Store) migrateThrough(ctx context.Context, fsys fs.FS, version int) error {
	if s == nil || s.Pool == nil {
		return fmt.Errorf("migrate: store pool required")
	}
	migrations, err := pgmigrate.LoadBuilt(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	if version != math.MaxInt && !slices.ContainsFunc(migrations, func(m pgmigrate.Migration) bool { return m.Version == version }) {
		return fmt.Errorf("migrate through %d: no embedded migration carries that version, none applied", version)
	}
	if err := s.prepareVersionTable(ctx, migrations); err != nil {
		return err
	}
	for _, migration := range migrations {
		if migration.Version > version {
			break
		}
		if err := s.applyMigration(ctx, migrations, migration); err != nil {
			return fmt.Errorf("apply migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

// migrationLockKey is the advisory key every Dispatch runner takes, session-level, on a
// connection that serves nothing else while it is held: prepareVersionTable and
// applyMigrationOnce through inMigrationLockTx, applyNonTransactionalMigration on its own
// hijacked connection. The auto-deployer (deploy/scripts/autodeploy.sh, schema_applied) polls the
// same key before reading schema_migrations and reads only once a try of its own is granted, so a
// migration in flight when it starts is waited out only if it ends inside the deployer's
// 60-second deadline. That read ends within those 60 seconds, since each try's own timeout is
// the smaller of 60 seconds and the time the deadline leaves; a migration still holding the key
// then is not waited out at all - that read fails and the deployer refuses to deploy rather than
// read behind a held key. A change of key must change both:
// TestTheDeployerTakesTheMigrationKeyThisPackageDefines reads the script's own key and fails when
// only one of the two moves.
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
// (lockMigrations), begin, run fn, end the transaction, release the key, reset the connection's
// session so a migration's leftover setting does not reach the pool's next caller
// (unlockMigrations, pgmigrate.ResetSession, run after the unlock has answered true and before the
// release), and give the connection back - or, where the connection's lock state is unknown, close
// it out of the pool.
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

// unlockMigrations gives migrationLockKey and conn back through duty.FreeSessionLockThenReset,
// which returns conn to the pool only once Postgres answers true, so a connection that may still
// hold the key never serves another caller and holds every later boot's migrations behind it.
//
// It passes pgmigrate.ResetSession, which runs between the unlock and the release: a migration's
// own text can run a SET without LOCAL, and its stored code can call set_config(…, false), either
// of which would otherwise stay on this pooled connection for the pool's next caller. The reset
// runs DISCARD ALL, which also releases the session's advisory locks, so it must run after the
// unlock has answered true, not before, or pg_advisory_unlock would have nothing to free.
//
// duty.ErrSessionClosed is logged at INFO: a boot cancelled inside the transaction has already
// closed the connection, over its in-flight statement's failure or over BeginFunc's rollback on
// the cancelled context, and the session's end releases the key. Every other error is an ERROR,
// because the key may still be held by a session the unlock could not reach, or the reset left the
// session's state unknown.
func unlockMigrations(conn *pgxpool.Conn) {
	err := duty.FreeSessionLockThenReset(conn, pgmigrate.ResetSession, "select pg_advisory_unlock($1)", migrationLockKey)
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

// prepareVersionTable readies schema_migrations before the runner applies anything, in one
// transaction that inMigrationLockTx opens on the connection that holds the runner's advisory key,
// and judges every version it records against the set.
//
// It creates the table, as the broker's runner creates its own, with the columns that hold what a
// runner records of each applied migration (pgmigrate.IdentityColumns). Created outside the lock,
// two processes booting at once against an empty database race `create table if not exists` on
// the catalog, and the loser exits on pg_type's unique index.
//
// On a table an earlier runner created, it adds the identity columns the table lacks and, when
// widen is set, widens the version column of a table from before the versions became timestamps,
// in one ALTER TABLE (pgmigrate.EnsureIdentityColumns), so the table's ACCESS EXCLUSIVE lock is
// taken once, and only where a column is owed. Pin's lock_timeout bounds that lock's wait at
// pgmigrate.LockTimeout (5 s), and the lock is held to the commit of this short transaction:
// adding a nullable column with no default writes the catalog alone, and widening rewrites a table
// of a few hundred rows. Every statement an earlier runner sends on the table names its columns,
// so its prepared statements keep their result types when the columns are added, and a release
// before this one rolls beside it, its rows recording no name until this runner's next boot fills
// them.
//
// The column is a bigint because a migration added since the set stopped being numbered is named
// for the UTC moment it was created, 14 digits, which is past the 2147483647 an integer column
// holds. A database created before that change has the integer the statement used to create.
// It is widened only by a boot whose set holds such a version (widen), as the daemon widens its
// own in the first timestamped migration's transaction, and not by a boot that records nothing past
// an integer: the release before that change reads `max(version)` uncast, so a widening while its
// processes still serve fails that read once on every connection that prepared it ("cached plan
// must not change result type"). A release whose set first holds a timestamped migration is rolled
// out over processes of that one, which read the version cast.
//
// It then judges every row before the runner applies anything (pgmigrate.ReconcileRecorded, whose
// rule pgmigrate.Reconcile states): a refusal writes nothing and applies nothing. The fills commit
// with the table, and each is logged once they have (pgmigrate.LogFixes).
//
// The callback pins its transaction first (pgmigrate.Pin), as applyMigrationOnce's does, so every
// statement the runner sends on its version table runs under one set of values:
// session_authorization, role and search_path at the values that RESET gives them, under which
// schema_migrations and information_schema.columns resolve as the version check and the census
// resolve them, and lock_timeout at LockTimeout. A lock wait that outlasts it rolls the
// transaction back and tries it again, as a migration is tried
// (pgmigrate.RetryVersionTableLockTimeouts), so a boot that starts while a pg_dump or a census
// holds the table waits it out within that bound rather than fail on its first wait.
func (s *Store) prepareVersionTable(ctx context.Context, migrations []pgmigrate.Migration) error {
	widen := slices.ContainsFunc(migrations, func(m pgmigrate.Migration) bool { return m.Version > math.MaxInt32 })
	var fixes []pgmigrate.Fix
	err := pgmigrate.RetryVersionTableLockTimeouts(ctx, versionTable, func(ctx context.Context) error {
		fixes = nil
		return s.inMigrationLockTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := pgmigrate.Pin(ctx, tx); err != nil {
				return fmt.Errorf("pin the runner's settings: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				create table if not exists schema_migrations (
					version bigint primary key,
					applied_at timestamptz not null default now(),
					`+pgmigrate.IdentityColumns+`
				)
			`); err != nil {
				return fmt.Errorf("create schema migrations table: %w", err)
			}
			if err := pgmigrate.EnsureIdentityColumns(ctx, tx, versionTable, func(columns map[string]string) []string {
				if widen && columns["version"] == "integer" {
					return []string{"alter column version type bigint"}
				}
				return nil
			}); err != nil {
				return err
			}
			_, reconciled, err := pgmigrate.ReconcileRecorded(ctx, tx, versionTable, migrations)
			if err != nil {
				return pgmigrate.Labelled(versionTable, err)
			}
			fixes = reconciled
			return nil
		})
	})
	if err != nil {
		return err
	}
	pgmigrate.LogFixes(versionTable, fixes)
	return nil
}

// applyMigration applies one migration of migrations, the boot's whole set, trying it again while
// its lock wait outlasts lock_timeout, up to pgmigrate.LockTimeoutAttempts times, holding nothing
// between attempts. A concurrent build's retry first drops an INVALID index of the migration's own
// name the previous attempt left on the migration's own table, then builds the index again.
// A migration that times out on every attempt fails the runner and boot with it.
func (s *Store) applyMigration(ctx context.Context, migrations []pgmigrate.Migration, migration pgmigrate.Migration) error {
	return pgmigrate.RetryLockTimeouts(ctx, migration.Name, func(ctx context.Context) error {
		return s.applyMigrationOnce(ctx, migrations, migration)
	})
}

func (s *Store) applyMigrationOnce(ctx context.Context, migrations []pgmigrate.Migration, migration pgmigrate.Migration) error {
	if migration.NonTransactional {
		return s.applyNonTransactionalMigration(ctx, migrations, migration)
	}
	// A migration is a transaction like any other, so it is marked like any other: nothing it
	// runs may take a second pooled connection while it is open. The key is taken first, on the
	// connection the transaction then runs on (inMigrationLockTx), so the migration and its
	// version record still commit together, and it is released before RetryLockTimeouts pauses
	// between attempts.
	var fixes []pgmigrate.Fix
	err := s.inMigrationLockTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// The check below and the insert of the version name schema_migrations without its
		// schema. Pin gives the transaction the values that RESET gives session_authorization,
		// role and search_path, under which the census reads, whatever session the pooled
		// connection brings, so the check reads the version table that the census read, and bounds
		// it by LockTimeout. Exec pins the transaction again after the migration's statements, so
		// the insert runs under the same values whatever the migration's own stored code set.
		if err := pgmigrate.Pin(ctx, tx); err != nil {
			return fmt.Errorf("pin the runner's settings: %w", err)
		}
		// The key was released after prepareVersionTable judged the table, so another process may
		// have recorded any version since. RecheckRecorded judges this version's row where there is
		// one, and, before this migration applies, every row against the whole set again, so no
		// migration is applied while the table records a version the set refuses.
		applied, recorded, err := pgmigrate.RecheckRecorded(ctx, tx, versionTable, migrations, migration)
		if err != nil {
			return fmt.Errorf("check migration: %w", err)
		}
		fixes = recorded
		if applied {
			return nil
		}
		if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
			return fmt.Errorf("execute migration: %w", err)
		}
		return pgmigrate.Record(ctx, tx, versionTable, migration)
	})
	if err != nil {
		return err
	}
	pgmigrate.LogFixes(versionTable, fixes)
	return nil
}

// applyNonTransactionalMigration owns its connection so a cancellation or failed cleanup cannot
// return a session-level advisory lock to the pool. Session and transaction advisory locks on the
// same key conflict, so this excludes older runners and schema-version readers too. Closing the
// connection releases the lock only after the statement and its separate version record finish.
//
// This connection never goes back to the pool, so it needs no session reset: a migration's own SET
// or its stored code's set_config(…, false) dies with the connection that closeBounded closes, and
// reaches no later caller. A change that gave this connection back to the pool would have to reset
// its session first, as inMigrationLockTx does through unlockMigrations (pgmigrate.ResetSession),
// or that leftover would reach the pool's next caller.
//
// The key is taken by polling here too (lockMigrations), and this is the site whose build the
// polling exists for: it holds the key while CREATE INDEX CONCURRENTLY runs.
func (s *Store) applyNonTransactionalMigration(ctx context.Context, migrations []pgmigrate.Migration, migration pgmigrate.Migration) error {
	pooled, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	conn := pooled.Hijack()
	defer closeBounded(ctx, conn)
	if err := lockMigrations(ctx, conn); err != nil {
		return err
	}
	// The check below and the version insert name schema_migrations without its schema.
	// PinSession gives the session the values that RESET gives session_authorization, role and
	// search_path, under which the census reads, whatever session the pooled connection this one
	// was hijacked from brings, and bounds the check and the insert by LockTimeout, as Pin bounds
	// the transactional runner's. No restore follows: closeBounded closes the connection once the
	// migration is recorded, so no later caller meets the pinned session. ExecNonTransactional
	// gives the session back the values it found on return, which are these, so the insert after
	// it runs under them too, whatever the build's own code set.
	if err := pgmigrate.PinSession(ctx, conn); err != nil {
		return fmt.Errorf("pin the runner's settings: %w", err)
	}
	// As in applyMigrationOnce. Each fill commits as its own statement here, so it is logged at
	// once, whether or not the build then succeeds.
	applied, fixes, err := pgmigrate.RecheckRecorded(ctx, conn, versionTable, migrations, migration)
	if err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	pgmigrate.LogFixes(versionTable, fixes)
	if applied {
		return nil
	}
	if err := pgmigrate.ExecNonTransactional(ctx, conn, migration, s.Pool.Config().BeforeConnect); err != nil {
		return err
	}
	if s.afterNonTransactionalStatement != nil {
		if err := s.afterNonTransactionalStatement(ctx, conn); err != nil {
			return err
		}
	}
	return pgmigrate.Record(ctx, conn, versionTable, migration)
}
