package pgmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	pgparser "github.com/wasilibs/go-pgquery/parser"
)

type indexStatement struct {
	Idxname     string `json:"idxname"`
	Concurrent  bool   `json:"concurrent"`
	IfNotExists bool   `json:"if_not_exists"`
	Unique      bool   `json:"unique"`
	Relation    struct {
		Relname    string `json:"relname"`
		Schemaname string `json:"schemaname"`
	} `json:"relation"`
}

// concurrentIndex accepts exactly one idempotent, non-unique concurrent index build. A UNIQUE
// build validates existing rows and must not receive this mode's census size exception. Keeping
// this mode idempotent makes a successful statement safe to repeat if recording its version
// fails: unlike a transactional migration, the statement and version cannot commit atomically.
func concurrentIndex(sql string) (index indexStatement, err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("cannot parse non-transactional migration")
		}
	}()
	tree, err := pgparser.ParseToJSON(sql)
	if err != nil {
		return index, fmt.Errorf("parse non-transactional migration: %w", err)
	}
	var parsed struct {
		Stmts []struct {
			Stmt map[string]json.RawMessage `json:"stmt"`
		} `json:"stmts"`
	}
	if err := json.Unmarshal([]byte(tree), &parsed); err != nil {
		return index, err
	}
	if len(parsed.Stmts) != 1 || len(parsed.Stmts[0].Stmt) != 1 {
		return index, errors.New("non-transactional migration must contain exactly one CREATE INDEX CONCURRENTLY IF NOT EXISTS statement")
	}
	body, ok := parsed.Stmts[0].Stmt["IndexStmt"]
	if !ok {
		return index, errors.New("non-transactional migration must be CREATE INDEX CONCURRENTLY IF NOT EXISTS")
	}
	if err := json.Unmarshal(body, &index); err != nil {
		return index, err
	}
	if index.Unique {
		return index, errors.New("non-transactional migration must create a non-unique index; UNIQUE builds cannot bypass the census size limit")
	}
	if !index.Concurrent || !index.IfNotExists || index.Idxname == "" || index.Relation.Relname == "" {
		return index, errors.New("non-transactional migration must be CREATE INDEX CONCURRENTLY IF NOT EXISTS")
	}
	return index, nil
}

// ExecNonTransactional runs a marked, single concurrent index build on an idle connection.
// Its caller must hold the runner's session-level advisory lock until the version is recorded,
// then close the connection: the build and that record cannot be one transaction. A build that
// failed, or one still running, leaves an INVALID index of the migration's own name on the
// migration's own table; the next attempt drops that leftover with DROP INDEX CONCURRENTLY and
// builds the index again, so a timed-out or killed build does not stop every later boot. A
// relation of that name that is not an index on the target table is refused instead, because the
// runner did not build it and IF NOT EXISTS would skip over it.
// The session lock_timeout is bounded exactly as Exec's transaction-local setting is, restored
// on return, and the same lock watch diagnoses failures, the drop's as well as the build's.
// signIn has Exec's meaning.
func ExecNonTransactional(ctx context.Context, conn *pgx.Conn, migration Migration, signIn func(context.Context, *pgx.ConnConfig) error) (err error) {
	if !migration.NonTransactional {
		return fmt.Errorf("migration %s is not marked non-transactional", migration.Name)
	}
	if conn.PgConn().TxStatus() != 'I' {
		return fmt.Errorf("migration %s must run outside a transaction", migration.Name)
	}
	index, err := concurrentIndex(migration.SQL)
	if err != nil {
		return fmt.Errorf("migration %s: %w", migration.Name, err)
	}
	// The settings are set before the leftover check so the drop below is bounded by
	// LockTimeout for every caller, whatever bound the caller's session carries (the server's
	// default is none at all).
	var previous, stringsSetting string
	if err := conn.QueryRow(ctx, "select current_setting('lock_timeout'), current_setting('standard_conforming_strings')").Scan(&previous, &stringsSetting); err != nil {
		return fmt.Errorf("migration %s: read session settings: %w", migration.Name, err)
	}
	if _, err := conn.Exec(ctx, "select set_config('lock_timeout', $1, false), set_config('standard_conforming_strings', 'on', false)", lockTimeoutSetting); err != nil {
		return fmt.Errorf("migration %s: set session settings: %w", migration.Name, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), diagnosisCleanupTimeout)
		defer cancel()
		if _, restore := conn.Exec(cleanup, "select set_config('lock_timeout', $1, false), set_config('standard_conforming_strings', $2, false)", previous, stringsSetting); restore != nil {
			err = errors.Join(err, fmt.Errorf("migration %s: restore lock_timeout: %w", migration.Name, restore))
		}
	}()
	if err := dropOwnLeftover(ctx, conn, migration, index, signIn); err != nil {
		return err
	}
	return execWatched(ctx, conn, migration, signIn)
}

// dropOwnLeftover reads the one relation of the index's name in the target table's schema and
// decides from the catalog alone. An INVALID index of that name on the target table is the
// runner's own leftover: it drops it, so the build that follows makes it again. A valid index of
// that name on that table is the index this migration builds, already built, and the build's
// IF NOT EXISTS skips over it. Any other relation of that name is one the runner did not build,
// and it is refused. An index of another name is neither dropped nor refused.
// The caller must have bounded the session's lock_timeout already: the drop takes locks too.
func dropOwnLeftover(ctx context.Context, conn *pgx.Conn, migration Migration, index indexStatement, signIn func(context.Context, *pgx.ConnConfig) error) error {
	table := pgx.Identifier{index.Relation.Relname}
	if index.Relation.Schemaname != "" {
		table = pgx.Identifier{index.Relation.Schemaname, index.Relation.Relname}
	}
	target := table.Sanitize()
	// At most one relation of the index's name exists in the target table's schema, since
	// Postgres keeps relation names distinct within a schema. A relation that is no index has
	// no pg_index row, so coalesce reads its indisvalid and its table as false.
	var schema, relkind string
	var valid, onTarget bool
	err := conn.QueryRow(ctx, `select n.nspname, c.relkind, coalesce(i.indisvalid, false),
			coalesce(i.indrelid = to_regclass($1), false)
		from pg_class c join pg_namespace n on n.oid = c.relnamespace
		left join pg_index i on i.indexrelid = c.oid
		where c.relname = $2 and c.relnamespace =
			(select relnamespace from pg_class where oid = to_regclass($1))`, target, index.Idxname).
		Scan(&schema, &relkind, &valid, &onTarget)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing of that name: the build has everything to do.
		return nil
	}
	if err != nil {
		return fmt.Errorf("migration %s: check for a relation named %s: %w", migration.Name, index.Idxname, err)
	}
	qualified := pgx.Identifier{schema, index.Idxname}.Sanitize()
	switch {
	case relkind != "i":
		// DROP INDEX CONCURRENTLY refuses a relation that is no index, so the remedy here is
		// the operator's own.
		return fmt.Errorf("migration %s: %s is a relation of relkind %q, not a plain index the runner can DROP INDEX CONCURRENTLY; the build's IF NOT EXISTS would skip over it and the index would never be built on %s. Rename or remove that relation, then retry the migration", migration.Name, qualified, relkind, target)
	case !onTarget:
		// On another table, VALID or INVALID alike: the runner did not build it, so it is not
		// this migration's to drop.
		validity := "an INVALID"
		if valid {
			validity = "a VALID"
		}
		return fmt.Errorf("migration %s: index %s is %s index on another table, so it is not this migration's to drop; the build's IF NOT EXISTS would skip over it and the index would never be built on %s. Rename it, or run DROP INDEX CONCURRENTLY %s outside a transaction if it is unwanted, then retry the migration", migration.Name, qualified, validity, target, qualified)
	case valid:
		// The index this migration builds, already built: the build skips it.
		return nil
	}
	slog.Warn("pgmigrate: dropping an INVALID index of this migration's own name, then building it again",
		"migration", migration.Name, "index", qualified, "table", target)
	drop := Migration{Name: migration.Name, SQL: "drop index concurrently if exists " + qualified, NonTransactional: true}
	// Alone on the connection: Postgres refuses DROP INDEX CONCURRENTLY inside a transaction
	// block. execWatched reports a drop cancelled by lock_timeout as a *LockTimeoutError, which
	// RetryLockTimeouts finds with errors.As through this wrapping and runs the whole attempt
	// again.
	if err := execWatched(ctx, conn, drop, signIn); err != nil {
		return fmt.Errorf("drop the INVALID index %s of this migration's own name, left by a build that failed or is still running: %w", qualified, err)
	}
	return nil
}
