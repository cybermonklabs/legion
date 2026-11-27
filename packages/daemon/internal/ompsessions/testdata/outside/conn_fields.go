// A fixture of TestNoStatementRunsOutsideTheHelper: a Conn whose connection code outside the
// package reaches with no method of Conn, through a field it exports, or one it embeds, whose
// methods it promotes even where the field's own name is unexported. The check must name each
// line that carries a want comment, and no other.
package fixture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type conn = pgx.Conn

type Conn struct {
	Raw   *pgx.Conn // want "Conn.Raw (an exported or embedded field of Conn, which code outside the package reaches)"
	*conn           // want "Conn.conn (an exported or embedded field of Conn, which code outside the package reaches)"
}

func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	raw, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Conn{Raw: raw}, nil
}

type pinned struct{ tx pgx.Tx }

func (p pinned) exec(ctx context.Context, sql string) (pgconn.CommandTag, error) {
	return p.tx.Exec(ctx, sql)
}

func inTransaction(ctx context.Context, conn *Conn, readOnly bool, fn func(pinned) error) error {
	if status := conn.Raw.PgConn().TxStatus(); !conn.Raw.IsClosed() && status != 'I' {
		return errors.New("open a transaction on a connection already in a transaction")
	}
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	return pgx.BeginTxFunc(ctx, conn.Raw, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog, pg_temp"); err != nil {
			return err
		}
		return fn(pinned{tx})
	})
}

func inside(ctx context.Context, conn *Conn) error {
	return inTransaction(ctx, conn, false, func(p pinned) error {
		_, err := p.exec(ctx, "SELECT 1")
		return err
	})
}
