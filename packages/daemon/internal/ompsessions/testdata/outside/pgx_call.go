// A fixture of TestNoStatementRunsOutsideTheHelper: a statement on a pgx connection outside the
// helper. The check must name each line that carries a want comment, and no other.
package fixture

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type pinned struct{ tx pgx.Tx }

func (p pinned) queryRow(ctx context.Context, sql string) pgx.Row { return p.tx.QueryRow(ctx, sql) }

func inTransaction(ctx context.Context, conn *pgx.Conn, readOnly bool, fn func(pinned) error) error {
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	return pgx.BeginTxFunc(ctx, conn, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
			return err
		}
		return fn(pinned{tx})
	})
}

func inside(ctx context.Context, conn *pgx.Conn) (n int, err error) {
	err = inTransaction(ctx, conn, true, func(p pinned) error {
		return p.queryRow(ctx, "SELECT 1").Scan(&n)
	})
	return n, err
}

func outside(ctx context.Context, conn *pgx.Conn) (n int, err error) {
	err = conn.QueryRow(ctx, "SELECT 1").Scan(&n) // want "conn.QueryRow (a method of *pgx.Conn)"
	return n, err
}
