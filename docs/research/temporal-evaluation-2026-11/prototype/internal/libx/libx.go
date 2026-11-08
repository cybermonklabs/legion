// Package libx holds what the two embedded-library products of the evaluation share:
// go-workflows (experiments 13, 14 and 15) and durabletask-go (16, 17 and 18).
//
// Both are Go libraries rather than servers: the workflow runs inside the
// application's own process and its state lives in the application's own PostgreSQL,
// in tables the library creates for itself. That is the whole reason they are measured
// here, so the experiments read those tables directly with SQL: what a fact says is
// what a row says.
//
// Each run owns a schema of its own inside one evaluation database, and drops that
// schema when it ends, so a run leaves nothing behind and two runs never collide.
package libx

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	// The database/sql driver these connections use is "pgx", registered by this
	// blank import. It is go-workflows' own PostgreSQL backend, which imports
	// jackc/pgx/v5/stdlib: this evaluation already depends on go-workflows directly,
	// while importing jackc/pgx here would add a third direct dependency, which
	// INFRA-1546 pins against.
	_ "github.com/cschleiden/go-workflows/backend/postgres"
)

// EvalDatabase is the database the library experiments run in. The pod's PostgreSQL
// role may create databases but may not create a schema in the server's own `postgres`
// database, so the experiments cannot put their schemas there. The first run creates
// this database, the role that created it owns it, and every run creates and drops its
// own schema inside it. This database, once created, stays empty between runs.
const EvalDatabase = "legion_golib_eval"

// connectTimeout bounds opening a connection, so a run against an unreachable
// PostgreSQL fails with its own message rather than hanging.
const connectTimeout = 15 * time.Second

// DatabaseURL is the URL of the evaluation database: the DSN the command line was
// given, pointed at EvalDatabase instead of whatever database it named, and with the
// given schema as the connection's search path when one is named.
func DatabaseURL(dsn, schema string) (string, error) {
	u, err := parseDSN(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + EvalDatabase
	if schema != "" {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// parseDSN checks that a DSN is a PostgreSQL URL this package can point elsewhere.
func parseDSN(dsn string) (*url.URL, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("-postgres-dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("-postgres-dsn %q is not a URL: %w", dsn, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return nil, fmt.Errorf("-postgres-dsn %q must be a postgres:// URL, not %q", dsn, u.Scheme)
	}
	return u, nil
}

// SchemaName is a schema name no other run of this prototype uses.
func SchemaName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1_000_000)
}

// Logger is the logger the experiments give the libraries under test: warnings and
// errors on stderr, so the library's chatter never reaches the measured lines on
// stdout.
func Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// EnsureDatabase creates the evaluation database if it is not there yet, connecting to
// the database the DSN itself names. It is safe to call from several runs at once: a
// concurrent creation is reported by PostgreSQL and treated as success.
func EnsureDatabase(ctx context.Context, dsn string) error {
	u, err := parseDSN(dsn)
	if err != nil {
		return err
	}
	admin, err := open(ctx, u.String())
	if err != nil {
		return err
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, EvalDatabase).Scan(&exists); err != nil {
		return fmt.Errorf("looking for database %s: %w", EvalDatabase, err)
	}
	if exists {
		return nil
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, Quote(EvalDatabase))); err != nil {
		// 42P04 is duplicate_database: another run created it between the two
		// statements, which is exactly what this function wanted.
		if strings.Contains(err.Error(), "42P04") || strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return fmt.Errorf("create database %s: %w", EvalDatabase, err)
	}
	return nil
}

// Open connects to the evaluation database with the run's schema as the search path.
// Every unqualified table a library creates or reads then lands in that schema.
func Open(ctx context.Context, dsn, schema string) (*sql.DB, error) {
	databaseURL, err := DatabaseURL(dsn, schema)
	if err != nil {
		return nil, err
	}
	return open(ctx, databaseURL)
}

func open(ctx context.Context, databaseURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("reach PostgreSQL: %w", err)
	}
	return db, nil
}

// CreateSchema makes the run's own schema, which is where the library under test puts
// its tables.
func CreateSchema(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, Quote(schema))); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	return nil
}

// DropSchema removes the run's schema and every table the library put in it, so the
// next run starts on an empty database.
func DropSchema(ctx context.Context, db *sql.DB, schema string) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`DROP SCHEMA IF EXISTS %s CASCADE`, Quote(schema))); err != nil {
		return fmt.Errorf("drop schema %s: %w", schema, err)
	}
	return nil
}

// Quote makes an identifier safe to interpolate. The experiments build their own
// schema names, so this guards against a name that cannot be an identifier rather than
// against an attacker.
func Quote(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
