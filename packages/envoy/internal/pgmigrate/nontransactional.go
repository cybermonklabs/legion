package pgmigrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	pgparser "github.com/wasilibs/go-pgquery/parser"
)

type indexStatement struct {
	Idxname     string `json:"idxname"`
	Concurrent  bool   `json:"concurrent"`
	IfNotExists bool   `json:"if_not_exists"`
	Relation    struct {
		Relname    string `json:"relname"`
		Schemaname string `json:"schemaname"`
	} `json:"relation"`
}

// concurrentIndex accepts exactly one idempotent concurrent index build. Keeping this mode
// narrow makes a successful statement safe to repeat if recording its version fails: unlike a
// transactional migration, the statement and its version cannot commit atomically.
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
	if !index.Concurrent || !index.IfNotExists || index.Idxname == "" || index.Relation.Relname == "" {
		return index, errors.New("non-transactional migration must be CREATE INDEX CONCURRENTLY IF NOT EXISTS")
	}
	return index, nil
}

// ExecNonTransactional runs a marked, single concurrent index build on an idle connection.
// Its caller must hold the runner's session-level advisory lock until the version is recorded,
// then close the connection: the build and that record cannot be one transaction. A failed build
// may leave an INVALID index; it is refused before IF NOT EXISTS can mistake it for success.
// The session lock_timeout is bounded exactly as Exec's transaction-local setting is, restored
// on return, and the same lock watch diagnoses failures. signIn has Exec's meaning.
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
	table := pgx.Identifier{index.Relation.Relname}
	if index.Relation.Schemaname != "" {
		table = pgx.Identifier{index.Relation.Schemaname, index.Relation.Relname}
	}
	var invalidSchema *string
	if err := conn.QueryRow(ctx, `select n.nspname from pg_index i
		join pg_class c on c.oid = i.indexrelid join pg_namespace n on n.oid = c.relnamespace
		where not i.indisvalid and c.relname = $2 and c.relnamespace =
			(select relnamespace from pg_class where oid = to_regclass($1))`, table.Sanitize(), index.Idxname).Scan(&invalidSchema); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("migration %s: check for INVALID index: %w", migration.Name, err)
	}
	if invalidSchema != nil {
		return fmt.Errorf("migration %s: index %s is INVALID after a failed concurrent build; investigate the failure, run DROP INDEX CONCURRENTLY %s outside a transaction, then retry the migration", migration.Name, index.Idxname, pgx.Identifier{*invalidSchema, index.Idxname}.Sanitize())
	}
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
	return execWatched(ctx, conn, migration, signIn)
}
