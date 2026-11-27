// A fixture of TestNoStatementRunsOutsideTheHelper: the connection a Conn holds, which ConnectURL
// alone builds, and only inTransaction, ConnectURL, and Conn's Close and IsClosed reach. Each shape
// that would carry a raw connection to another package's code needs one: a tuple assigned to that
// package's interface, a tuple returned as it, a struct field of its type, or an argument of its
// function. Outside those bodies the only ways to one are a driver function called outside
// ConnectURL and the field reached, and the check names both. It names a Conn built outside
// ConnectURL, and the field set on one made by new, too. The check must name each line that
// carries a want comment, and no other.
package fixture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/legion/daemon/internal/store"
)

type Conn struct{ c *pgx.Conn }

func (c *Conn) Close(ctx context.Context) error { return c.c.Close(ctx) }

func (c *Conn) IsClosed() bool { return c.c.IsClosed() }

// Raw is a method of Conn that hands its connection out.
func (c *Conn) Raw() *pgx.Conn {
	return c.c // want "c.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
}

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

func open(ctx context.Context, dsn string) (*pgx.Conn, error) {
	return pgx.Connect(ctx, dsn) // want "pgx.Connect (a function of pgx)"
}

func tupleAssigned(ctx context.Context, dsn string) (err error) {
	var q store.TxPool
	q, err = open(ctx, dsn)
	if err != nil {
		return err
	}
	return store.Transact(ctx, q, func(context.Context, pgx.Tx) error { return nil })
}

func tupleReturned(ctx context.Context, dsn string) (store.TxPool, error) {
	return open(ctx, dsn)
}

type carrier struct{ q store.TxPool }

func carried(conn *Conn) carrier {
	return carrier{q: conn.c} // want "conn.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
}

func handed(ctx context.Context, conn *Conn) error {
	return store.Transact(ctx, conn.c, func(context.Context, pgx.Tx) error { return nil }) // want "conn.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
}

func built(raw *pgx.Conn) *Conn {
	return &Conn{c: raw} // want "Conn{…} (a Conn built outside ConnectURL)"
}

func allocated(raw *pgx.Conn) *Conn {
	conn := new(Conn)
	conn.c = raw // want "conn.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
	return conn
}
