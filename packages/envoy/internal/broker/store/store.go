// Package store owns the broker's Postgres pool and schema migrations. Its runner shares
// internal/dispatch/store's rules through internal/pgmigrate: the embedded set is refused whole,
// before anything is applied, when pgmigrate.Load refuses it, and every lock wait a migration
// makes is bounded by pgmigrate.LockTimeout. Migrate holds a broker-specific
// pg_advisory_xact_lock for the whole run, so concurrent callers serialize instead of racing on
// `create table if not exists`, and applies every pending migration inside one shared
// transaction, recording each in broker_schema_migrations. Once that transaction ends, it resets
// the session of the connection it ran on before that connection goes back to the pool.
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
	"github.com/sjawhar/envoy/pgsetting"
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

// Open opens the pool databaseURL (BROKER_DATABASE_URL) names, configured by opts, and pings it.
// It reads the URL through pgsetting: one whose user part pgx may read apart is refused, and one
// pgx cannot parse, each named by its length, and a failed ping names no user, since pgx's errors
// would print a piece of the user part, or the user, which can be a token.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*Store, error) {
	config, err := pgsetting.ParsePoolConfig("BROKER_DATABASE_URL", databaseURL)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("ping Postgres: %w", pgsetting.ConnectFailure(err))
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

// migrate applies every pending migration of fsys in one transaction, on a connection it takes
// from the pool for itself (Acquire), and resets that connection's session before it gives it back
// (releaseMigrationConn).
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
	// BeginFunc commits when the callback returns nil and rolls the whole set back when it returns
	// an error, so a failed migration leaves none of the set applied.
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(8330001)); err != nil {
			return fmt.Errorf("lock migrations: %w", err)
		}
		// Pin gives the transaction, until it ends, lock_timeout at LockTimeout, the text settings,
		// and session_authorization, role and search_path at the values that RESET gives them. So
		// the create table and the first applied check below are bounded by LockTimeout and resolve
		// broker_schema_migrations under those values, whatever session the pooled connection
		// brings. Exec pins the transaction again after each migration's statements, so each later
		// check and insert runs pinned too. The pin comes after the advisory lock and not before it,
		// because lock_timeout bounds the wait for an advisory lock too. A pin before the lock would
		// make a second broker process that starts while another one migrates give up after
		// LockTimeout, where it must wait its turn.
		if err := pgmigrate.Pin(ctx, tx); err != nil {
			return fmt.Errorf("pin the runner's settings: %w", err)
		}
		if _, err := tx.Exec(ctx, `create table if not exists broker_schema_migrations (version integer primary key, applied_at timestamptz not null default now())`); err != nil {
			return fmt.Errorf("create broker_schema_migrations: %w", err)
		}
		for _, migration := range migrations {
			var applied bool
			if err := tx.QueryRow(ctx, `select exists(select 1 from broker_schema_migrations where version=$1)`, migration.Version).Scan(&applied); err != nil {
				return fmt.Errorf("check whether migration %s is applied: %w", migration.Name, err)
			}
			if applied {
				continue
			}
			if err := pgmigrate.Exec(ctx, tx, migration, s.Pool.Config().BeforeConnect); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `insert into broker_schema_migrations (version) values ($1)`, migration.Version); err != nil {
				return err
			}
		}
		return nil
	})
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
