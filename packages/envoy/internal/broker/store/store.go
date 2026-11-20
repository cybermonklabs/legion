// Package store owns the broker's Postgres pool and schema migrations. Its runner shares
// internal/dispatch/store's rules through internal/pgmigrate: the embedded set is refused whole,
// before anything is applied, when pgmigrate.Load refuses it, a database whose version table
// records a version its file does not accept is refused before anything is applied
// (pgmigrate.Reconcile), and every lock wait a migration makes is bounded by
// pgmigrate.LockTimeout. Migrate runs two transactions, each under the broker's
// pg_advisory_xact_lock, so concurrent callers serialize instead of racing on `create table if
// not exists`: a short one that gives an earlier runner's broker_schema_migrations the identity
// columns and commits, and one that judges every recorded row and applies every pending
// migration, recording each with what pgmigrate.Record records of it, and holds the key until it
// commits. Once they end, it resets the session of the connection they ran on before that
// connection goes back to the pool.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// Every file in migrations, so pgmigrate.Load judges each one; pgmigratetest.CheckEmbedsEveryFile
// states why the pattern is all:migrations and holds the tree to it.
//
//go:embed all:migrations
var migrationFiles embed.FS

type Store struct {
	Pool *pgxpool.Pool

	// mu guards listen, the one-connection pool Listener opens on first use, so two callers
	// cannot open two of them, and closed, so a dial after Close opens no pool nobody closes.
	mu     sync.Mutex
	closed bool
	listen *pgxpool.Pool
}

// TokenMinter mints the password one new connection signs in with, for the host:port it dials
// and the database user it signs in as.
type TokenMinter func(ctx context.Context, endpoint, user string) (string, error)

// Option configures the pool Open makes.
type Option func(*pgxpool.Config)

// WithTokenMinter signs every new connection in with a password mint makes for it, set in the
// pool's BeforeConnect, so a connection the pool opens an hour after startup brings a token as
// fresh as the first one's: an RDS IAM token is good for 15 minutes from its mint and is checked
// only at sign-in, so a connection outlives the token it signed in with but a new one cannot
// reuse it. The migration lock watch's own connection signs in the same way (Migrate).
func WithTokenMinter(mint TokenMinter) Option {
	return func(config *pgxpool.Config) {
		config.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			token, err := mint(ctx, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))), cc.User)
			if err != nil {
				return fmt.Errorf("mint the sign-in token for database user %s at %s: %w", cc.User, cc.Host, err)
			}
			cc.Password = token
			return nil
		}
	}
}

// Open opens the pool databaseURL names, configured by opts, and pings it.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	for _, opt := range opts {
		opt(config)
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open Postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Postgres: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// IsUniqueViolation reports whether err is Postgres's unique_violation (23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// listenerPoolSize bounds the pool Listener takes its connection from. One listener dials
// through it - the cross-pod secret-reread fan-out (internal/broker/reread) - and the connection
// is hijacked as soon as it is opened, so it leaves with its caller and the pool holds none
// between dials: what this bounds is dials in flight, not connections held.
const listenerPoolSize = 1

// Listener returns one connection of its own for LISTEN, from a one-connection pool opened from
// the shared pool's own configuration and hijacked, so it holds no connection of the shared pool
// and the caller owns, and closes, what it gets. The pool is opened on first use and kept, so
// every redial of a listener mints its own connection through it.
//
// Opening it from s.Pool.Config() rather than from the database URL is what keeps BeforeConnect -
// the RDS IAM token minter WithTokenMinter installs - minting a fresh token for every redial; a
// pgxpool.New on the raw URL would carry no minter and sign in with no password at all once the
// first token lapsed.
//
// What it costs: one extra Postgres connection per broker pod, outside the count pool_max_conns
// sizes, held for the life of the process, since a LISTEN lives on its connection and a pooled
// one would end the subscription when the pool recycled it. That connection also needs a
// session-mode path to Postgres: a transaction pooler hands each transaction whichever backend
// is free, which can carry no LISTEN. The deployment's path is a TLS tunnel that keeps each
// client connection its own session, not a pooler, so this holds there.
func (s *Store) Listener(ctx context.Context) (*pgx.Conn, error) {
	pool, err := s.listenerPool()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return conn.Hijack(), nil
}

// ErrClosed is a listener asked for after the store was closed.
var ErrClosed = errors.New("store is closed")

// listenerPool opens the listener pool on first use, under mu for the whole check-and-open.
func (s *Store) listenerPool() (*pgxpool.Pool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.listen != nil {
		return s.listen, nil
	}
	config := s.Pool.Config().Copy()
	config.MaxConns = listenerPoolSize
	// Both minimums, not just MinConns: a floor the shared pool's DSN set would be a target one
	// connection cannot reach, which pgxpool re-attempts every health-check period.
	config.MinConns = 0
	config.MinIdleConns = 0
	// Not the caller's context: this pool outlives the dial that opened it, and pgxpool's own
	// background work would end with that context.
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, fmt.Errorf("open the listener connection pool: %w", err)
	}
	s.listen = opened
	return opened, nil
}

// Close releases every connection the store holds: the listener pool first, then the shared one.
// A pool nobody closes is a pool whose health-check goroutine runs for the life of the process,
// so a store that ever dialled a listener leaks one goroutine - and the Postgres connection a
// redial is holding - unless its listener pool goes with it. A later dial is refused with
// ErrClosed rather than opening a pool this Close can no longer reach, and a second Close is a
// no-op, for the caller whose own teardown and a test's both run.
func (s *Store) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	listen := s.listen
	s.listen = nil
	s.mu.Unlock()
	if listen != nil {
		listen.Close()
	}
	s.Pool.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	return s.migrate(ctx, migrationFiles)
}

// versionTable is the table this runner records each applied migration in.
const versionTable = "broker_schema_migrations"

// migrate applies every pending migration of fsys in one transaction, on a connection it takes
// from the pool for itself (Acquire), and resets that connection's session before it gives it back
// (releaseMigrationConn).
//
// Each of its two transactions takes the broker's migration lock first (lockAndPin). The first
// gives a broker_schema_migrations an earlier runner created the identity columns
// (pgmigrate.EnsureIdentityColumns) and commits: its ALTER TABLE takes ACCESS EXCLUSIVE on the
// table, Pin's lock_timeout bounds its wait at pgmigrate.LockTimeout (5 s), and the lock is held to
// that short transaction's commit, so a request that reads the table (a release with the old rule
// for requests written before migration 0013 reads version 13's applied_at) waits for the catalog
// change alone. The catalog change commits whether or not the second transaction then refuses the
// rows: the columns are nullable, and the release before ignores them. Every statement an earlier
// runner sends on the table names its columns, so a pod of the release before this one serves
// beside it while the Deployment rolls, and its rows record no name. A lock wait that outlasts its
// bound rolls that transaction back and tries it again, as a migration is tried
// (pgmigrate.RetryVersionTableLockTimeouts), so a boot that starts while a pg_dump holds the table
// waits it out rather than fail on its first wait.
//
// The second creates the table with those columns where there is none, judges every row before it
// applies anything (pgmigrate.ReconcileRecorded, whose rule pgmigrate.Reconcile states: a refusal
// rolls the whole transaction back, nothing applied), and applies the pending migrations. It adds
// the columns again where they are missing, for one case alone: on a fresh database, a pod of the
// release before can create the table without them between the two transactions, and the ALTER
// then holds ACCESS EXCLUSIVE until this transaction commits, after the pending migrations. That
// pod serves at once, so its request's read of the table (version 13's applied_at, in a release
// with that old rule) waits behind the ALTER until this transaction commits, a wait the pending
// migrations bound. Each fill is logged once the transaction has committed (pgmigrate.LogFixes).
// A fresh database commits nothing, its table included, until every migration has applied.
func (s *Store) migrate(ctx context.Context, fsys fs.FS) error {
	migrations, err := pgmigrate.Load(fsys, "migrations")
	if err != nil {
		return fmt.Errorf("migrations refused, none applied: %w", err)
	}
	for _, migration := range migrations {
		if migration.NonTransactional {
			return fmt.Errorf("migrations refused, none applied: migration %s is non-transactional; the broker runner applies its whole set in one transaction", migration.Name)
		}
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer releaseMigrationConn(ctx, conn)
	if err := pgmigrate.RetryVersionTableLockTimeouts(ctx, versionTable, func(ctx context.Context) error {
		return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if err := lockAndPin(ctx, tx); err != nil {
				return err
			}
			return pgmigrate.EnsureIdentityColumns(ctx, tx, versionTable, nil)
		})
	}); err != nil {
		return err
	}
	var fixes []pgmigrate.Fix
	// BeginFunc commits when the callback returns nil and rolls the whole set back when it returns
	// an error, so a failed migration leaves none of the set applied.
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := lockAndPin(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `create table if not exists broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now(), `+pgmigrate.IdentityColumns+`)`); err != nil {
			return fmt.Errorf("create broker_schema_migrations: %w", err)
		}
		// The one window the comment above names: a release before this one created the table
		// between the two transactions.
		if err := pgmigrate.EnsureIdentityColumns(ctx, tx, versionTable, nil); err != nil {
			return err
		}
		// The advisory lock is held to the commit, so what the table records now is what it
		// records for every migration below.
		recorded, reconciled, err := pgmigrate.ReconcileRecorded(ctx, tx, versionTable, migrations)
		if err != nil {
			return pgmigrate.Labelled(versionTable, err)
		}
		fixes = reconciled
		applied := make(map[int]bool, len(recorded))
		for _, row := range recorded {
			applied[row.Version] = true
		}
		for _, migration := range migrations {
			if applied[migration.Version] {
				continue
			}
			if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
				return err
			}
			if err := pgmigrate.Record(ctx, tx, versionTable, migration); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	pgmigrate.LogFixes(versionTable, fixes)
	return nil
}

// lockAndPin takes the broker's migration lock in tx, a transaction-level advisory lock every
// broker boot takes, so concurrent boots serialize instead of racing on the version table, and
// then pins tx (pgmigrate.Pin): lock_timeout at LockTimeout, the text settings, and
// session_authorization, role and search_path at the values that RESET gives them, so every later
// statement of tx is bounded by LockTimeout and resolves broker_schema_migrations under those
// values, whatever session the pooled connection brings. Exec pins the transaction again after
// each migration's statements, so each later insert runs pinned too. The pin comes after the
// advisory lock and not before it, because lock_timeout bounds the wait for an advisory lock too:
// a pin before the lock would make a second broker process that starts while another one migrates
// give up after LockTimeout, where it must wait its turn.
func lockAndPin(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(8330001)); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if err := pgmigrate.Pin(ctx, tx); err != nil {
		return fmt.Errorf("pin the runner's settings: %w", err)
	}
	return nil
}

// CheckVersions reads what broker_schema_migrations records of each version against the
// migrations this binary embeds, and what the runner's next boot would do with each row
// (pgmigrate.CheckVersions), on one connection of the store's pool, which signs in as every pool
// connection does (an RDS IAM token from WithTokenMinter included). It reads in a read-only
// transaction it rolls back, and writes nothing.
func (s *Store) CheckVersions(ctx context.Context) (*pgmigrate.VersionCheck, error) {
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrations refused: %w", err)
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Release()
	return pgmigrate.CheckVersions(ctx, conn.Conn(), migrations, versionTable)
}

// releaseMigrationConn gives conn back to the pool once migrate's transaction has ended, after it
// resets conn's session with pgmigrate.ResetSession, which says why a migration's connection needs
// the reset. A migration's own text can run a SET without LOCAL, and its stored code can call
// set_config(…, false). Either one stays on the session past the commit, so without the reset the
// pool's next caller would meet it. The reset runs DISCARD ALL, which also releases the session's
// advisory locks. The broker's lock is a transaction lock (pg_advisory_xact_lock), which the commit
// or the rollback has already released, so no unlock has to run before the reset, as one does in
// Dispatch's runner. The store's connection options, such as a search_path in the database URL,
// are the values that the reset gives back.
//
// The reset and the close run on a context that a cancelled ctx does not cancel, bounded by
// pgmigrate.LockTimeout as the rest of the runner's waits are. A connection that is already closed,
// as pgx closes one whose context was cancelled during a statement, goes back to the pool, which
// destroys it. A connection whose reset fails carries a session that is unknown, so it is taken out
// of the pool (Hijack) and closed, never given back. migrate's own answer does not change: the set
// has committed or rolled back already, and the closed connection reaches no later caller. So the
// failed reset is logged at WARN.
func releaseMigrationConn(ctx context.Context, conn *pgxpool.Conn) {
	if conn.Conn().IsClosed() {
		conn.Release()
		return
	}
	resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
	defer resetCancel()
	err := pgmigrate.ResetSession(resetCtx, conn.Conn())
	if err == nil {
		conn.Release()
		return
	}
	slog.Warn("broker: the migration connection's session reset failed; the connection is closed, not given back to the pool", "error", err)
	closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), pgmigrate.LockTimeout)
	defer closeCancel()
	_ = conn.Hijack().Close(closeCtx)
}
