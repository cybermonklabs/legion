// A fixture of TestNoStatementRunsOutsideTheHelper: pinned and Conn declared as aliases, pinned of a
// type declared under a name of its own, whose methods are pinned's, and Conn of a struct type
// written out, to which a value of any type of its shape is assignable. A pinned built under either
// of its names, and a Conn built as any type of its shape, or one given a connection through such a
// type's field, are named as they are where each is declared as a type of its own. Conn being that
// struct type itself, each value of it outside ConnectURL is a value of Conn's struct type, which
// no reader can tell from one built there, so each is named too. The check must name each line
// that carries a want comment, and no other.
package fixture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Conn = struct{ c *pgx.Conn }

func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &Conn{c: conn}, nil
}

type transaction struct{ tx pgx.Tx }

type pinned = transaction

func (p transaction) exec(ctx context.Context, sql string) (pgconn.CommandTag, error) {
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

func forged(ctx context.Context, caller pgx.Tx) error {
	_, err := pinned{tx: caller}.exec(ctx, "SELECT 1") // want "pinned{…} (a pinned transaction built outside inTransaction)"
	return err
}

func forgedUnderItsOwnName(ctx context.Context, caller pgx.Tx) error {
	_, err := transaction{tx: caller}.exec(ctx, "SELECT 1") // want "transaction{…} (a pinned transaction built outside inTransaction)"
	return err
}

type looseTransaction struct{ tx pgx.Tx }

func converted(ctx context.Context, loose looseTransaction) error {
	_, err := transaction(loose).exec(ctx, "SELECT 1") // want "transaction(loose) (a pinned transaction built outside inTransaction)"
	return err
}

func built(raw *pgx.Conn) *Conn {
	return &Conn{c: raw} // want "Conn{…} (a Conn built outside ConnectURL)"
}

func writtenOut(raw *pgx.Conn) *Conn {
	return &struct{ c *pgx.Conn }{c: raw} // want "struct{c *pgx.Conn}{…} (a Conn built outside ConnectURL)"
}

type looseConnection struct{ c *pgx.Conn }

func assigned(raw *pgx.Conn) *Conn {
	var conn Conn = looseConnection{c: raw} // want "looseConnection{…} (a Conn built outside ConnectURL)"
	return &conn                            // want "conn (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
}

func fielded(raw *pgx.Conn) *Conn {
	var loose looseConnection
	loose.c = raw // want "loose.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
	var conn Conn = loose
	return &conn // want "conn (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
}
