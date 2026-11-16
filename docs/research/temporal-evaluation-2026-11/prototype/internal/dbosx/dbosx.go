// Package dbosx holds what the DBOS experiments of the evaluation share: where their
// system database lives, how a process opens one, and how the driver reads DBOS's own
// tables with SQL.
//
// DBOS Transact is a library, not a server: the workflow state lives in the
// application's own PostgreSQL, in a schema DBOS migrates for itself. That is the whole
// reason it is measured here, so the experiments read that schema directly rather than
// only through the SDK's accessors: what a fact says is what a row says.
package dbosx

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/dbos-inc/dbos-transact-golang/dbos/internals"
)

// EvalDatabase is the database the DBOS experiments run in. The pod's PostgreSQL role
// may create databases but may not create a schema in the server's own `postgres`
// database, so the experiments cannot put their schemas there. DBOS creates this
// database itself the first time a context opens it, and the role that created it owns
// it, so every run can create and drop its own schema inside it. Each run drops its
// schema and leaves nothing else behind; this database, once created, stays empty
// between runs.
const EvalDatabase = "legion_dbos_eval"

// DatabaseURL is the system-database URL the experiments open: the DSN the command line
// was given, pointed at EvalDatabase instead of whatever database it named.
func DatabaseURL(dsn string) (string, error) {
	if strings.TrimSpace(dsn) == "" {
		return "", fmt.Errorf("-postgres-dsn is empty")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("-postgres-dsn %q is not a URL: %w", dsn, err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("-postgres-dsn %q must be a postgres:// URL, not %q", dsn, u.Scheme)
	}
	u.Path = "/" + EvalDatabase
	return u.String(), nil
}

// SchemaName is a schema name no other run of this prototype uses.
func SchemaName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, os.Getpid(), time.Now().UnixNano()%1_000_000)
}

// Options is one process's DBOS system-database connection.
type Options struct {
	// DSN is the command line's -postgres-dsn.
	DSN string
	// Schema is the system-database schema this run owns. Every DBOS table of the run
	// lives in it, and the run drops it at the end.
	Schema string
	// AppName is the application the workflows belong to.
	AppName string
	// AppVersion is the application version, pinned rather than computed. DBOS
	// otherwise derives it from the running binary's hash, which would give the driver
	// and the worker two different versions of the same workflow: recovery and dequeue
	// both filter on it, so nothing either one started would ever reach the other.
	AppVersion string
	// ExecutorID names this process to DBOS. A workflow is bound to the executor that
	// claimed it, and a process recovers at launch the pending workflows of its own
	// executor id, so a replacement process must carry the id of the one it replaces.
	ExecutorID string
	// SkipMigrations verifies the schema instead of creating it, for a process that
	// opens a schema another process already migrated.
	SkipMigrations bool
}

// Logger is the logger the experiments give DBOS: warnings and errors on stderr, so the
// library's chatter never reaches the measured lines on stdout.
func Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// Open creates a DBOS context on the evaluation database. It does not launch it: a
// context that is never launched recovers nothing, dequeues nothing and runs nothing,
// which is what the driver wants from the handle it observes with.
func Open(ctx context.Context, o Options) (dbos.Context, error) {
	databaseURL, err := DatabaseURL(o.DSN)
	if err != nil {
		return nil, err
	}
	if o.Schema == "" {
		return nil, fmt.Errorf("dbosx.Open: a schema is required")
	}
	dbosCtx, err := dbos.NewContext(ctx, dbos.Config{
		AppName:            o.AppName,
		DatabaseURL:        databaseURL,
		DatabaseSchema:     o.Schema,
		ApplicationVersion: o.AppVersion,
		ExecutorID:         o.ExecutorID,
		SkipMigrations:     o.SkipMigrations,
		Logger:             Logger(),
	})
	if err != nil {
		return nil, fmt.Errorf("open DBOS on %s schema %s: %w", EvalDatabase, o.Schema, err)
	}
	return dbosCtx, nil
}

// Pool is the connection pool DBOS itself uses for its system database, which is how
// the experiments read its tables with SQL. It comes from the runtime view of the
// context, the same view the DBOS commercial module is given.
func Pool(ctx dbos.Context) (dbos.Pool, error) {
	runtime, err := internals.RuntimeOf(ctx)
	if err != nil {
		return nil, fmt.Errorf("dbosx.Pool: %w", err)
	}
	return runtime.SystemDB().Pool(), nil
}

// Decode turns a value as DBOS stored it -- its serialized form, which is what a SELECT
// returns -- back into what the workflow wrote, so a fact can print both.
func Decode(ctx dbos.Context, value, serialization string) string {
	decoded, err := DecodeAny(ctx, value, serialization)
	if err != nil {
		return fmt.Sprintf("undecodable: %v", err)
	}
	return fmt.Sprintf("%v", decoded)
}

// DecodeAny is Decode, giving back the value itself rather than its printed form, for
// a caller that has to look at what the value is.
func DecodeAny(ctx dbos.Context, value, serialization string) (any, error) {
	runtime, err := internals.RuntimeOf(ctx)
	if err != nil {
		return nil, fmt.Errorf("dbosx.DecodeAny: %w", err)
	}
	return runtime.DecodeStoredValue(value, serialization)
}
