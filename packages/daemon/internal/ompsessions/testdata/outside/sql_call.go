// A fixture of TestNoStatementRunsOutsideTheHelper: a statement through database/sql outside the
// helper, whose receiver is no pgx type. The check must name each line that carries a want
// comment, and no other.
package fixture

import (
	"context"
	"database/sql"
)

type pinned struct{ tx *sql.Tx }

func (p pinned) queryRow(ctx context.Context, query string) *sql.Row {
	return p.tx.QueryRowContext(ctx, query)
}

func inTransaction(ctx context.Context, db *sql.DB, fn func(pinned) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
		return err
	}
	if err := fn(pinned{tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func inside(ctx context.Context, db *sql.DB) (n int, err error) {
	err = inTransaction(ctx, db, func(p pinned) error {
		return p.queryRow(ctx, "SELECT 1").Scan(&n)
	})
	return n, err
}

func outside(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT 1") // want "db.QueryContext (a method of *sql.DB)"
	if err != nil {
		return err
	}
	return rows.Close()
}
