// A fixture of TestNoStatementRunsOutsideTheHelper: a pinned or a Conn built with no composite
// literal and no conversion, from a value of the struct type it is declared with, written out,
// which becomes one wherever one is wanted: returned as one, handed to a function as one, or
// assigned whole to one. A value of a type of its own with the same shape needs a conversion, and
// is passed on here as itself, as a pinned and a Conn are. The check must name each line that
// carries a want comment, and no other.
package fixture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Conn struct{ c *pgx.Conn }

func (c *Conn) Close(ctx context.Context) error { return c.c.Close(ctx) }

func (c *Conn) IsClosed() bool { return c.c.IsClosed() }

func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Conn{c: conn}, nil
}

type pinned struct{ tx pgx.Tx }

func (p pinned) exec(ctx context.Context, sql string) (pgconn.CommandTag, error) {
	return p.tx.Exec(ctx, sql)
}

func inTransaction(ctx context.Context, conn *Conn, readOnly bool, fn func(pinned) error) error {
	if status := conn.c.PgConn().TxStatus(); !conn.c.IsClosed() && status != 'I' {
		return errors.New("open a transaction on a connection already in a transaction")
	}
	access := pgx.ReadWrite
	if readOnly {
		access = pgx.ReadOnly
	}
	return pgx.BeginTxFunc(ctx, conn.c, pgx.TxOptions{AccessMode: access}, func(tx pgx.Tx) error {
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

type looseTransaction struct{ tx pgx.Tx }

func returned(raw struct{ tx pgx.Tx }) pinned {
	return raw // want "raw (a value of pinned's struct type, which becomes a pinned with no conversion, outside inTransaction)"
}

func forged(ctx context.Context, caller pgx.Tx) error {
	_, err := returned(looseTransaction{caller}).exec(ctx, "SELECT 1")
	return err
}

func run(ctx context.Context, p pinned) error {
	_, err := p.exec(ctx, "SELECT 1")
	return err
}

func handed(ctx context.Context, raw struct{ tx pgx.Tx }) error {
	return run(ctx, raw) // want "raw (a value of pinned's struct type, which becomes a pinned with no conversion, outside inTransaction)"
}

func assignedWhole(raw struct{ tx pgx.Tx }) *pinned {
	p := new(pinned)
	*p = raw // want "raw (a value of pinned's struct type, which becomes a pinned with no conversion, outside inTransaction)"
	return p
}

func passedOn(ctx context.Context, p pinned, loose looseTransaction) (pinned, looseTransaction, error) {
	return p, loose, run(ctx, p)
}

type looseConnection struct{ c *pgx.Conn }

func returnedConn(raw struct{ c *pgx.Conn }) Conn {
	return raw // want "raw (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
}

func closeConn(ctx context.Context, conn Conn) error {
	return conn.Close(ctx)
}

func handedConn(ctx context.Context, raw struct{ c *pgx.Conn }) error {
	return closeConn(ctx, raw) // want "raw (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
}

func assignedWholeConn(raw struct{ c *pgx.Conn }) *Conn {
	conn := new(Conn)
	*conn = raw // want "raw (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
	return conn
}

func passedOnConn(ctx context.Context, conn Conn, loose looseConnection) (Conn, looseConnection, error) {
	return conn, loose, closeConn(ctx, conn)
}
