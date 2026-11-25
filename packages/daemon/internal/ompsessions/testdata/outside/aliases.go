// A fixture of TestNoStatementRunsOutsideTheHelper: pinned, Conn and a driver type under the names
// aliases give them. A method of pinned or of Conn declared through an alias of it is its method,
// and a pinned or a Conn built through an alias of it, by a composite literal or a conversion, is
// built as one is under its own name, which the conversions under its own name show. A driver
// type's method called through an alias of the type is the method of the type that declares it.
// The check must name each line that carries a want comment, and no other.
package fixture

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Conn struct{ c *pgx.Conn }

type connection = Conn

func (c *connection) Close(ctx context.Context) error { return c.c.Close(ctx) }

func (c *connection) IsClosed() bool { return c.c.IsClosed() }

func ConnectURL(ctx context.Context, dsn string) (*Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &connection{c: conn}, nil
}

type pinned struct{ tx pgx.Tx }

type transaction = pinned

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
		return fn(transaction{tx})
	})
}

func inside(ctx context.Context, conn *Conn) error {
	return inTransaction(ctx, conn, false, func(p transaction) error {
		_, err := p.exec(ctx, "SELECT 1")
		return err
	})
}

type looseTransaction struct{ tx pgx.Tx }

func forged(ctx context.Context, caller pgx.Tx) error {
	_, err := transaction{tx: caller}.exec(ctx, "SELECT 1") // want "transaction{…} (a pinned transaction built outside inTransaction)"
	return err
}

func converted(ctx context.Context, loose looseTransaction) error {
	_, err := pinned(loose).exec(ctx, "SELECT 1") // want "pinned(loose) (a pinned transaction built outside inTransaction)"
	return err
}

func convertedThroughAlias(ctx context.Context, loose looseTransaction) error {
	_, err := transaction(loose).exec(ctx, "SELECT 1") // want "transaction(loose) (a pinned transaction built outside inTransaction)"
	return err
}

type looseConnection struct{ c *pgx.Conn }

func built(raw *pgx.Conn) *Conn {
	return &connection{c: raw} // want "connection{…} (a Conn built outside ConnectURL)"
}

func convertedConn(loose looseConnection) Conn {
	return Conn(loose) // want "Conn(loose) (a Conn built outside ConnectURL)"
}

func convertedConnThroughAlias(loose looseConnection) Conn {
	return connection(loose) // want "connection(loose) (a Conn built outside ConnectURL)"
}

type driverTx = pgx.Tx

func throughDriverAlias(ctx context.Context, caller driverTx) error {
	if _, err := caller.Exec(ctx, "SELECT 1"); err != nil { // want "caller.Exec (a method of pgx.Tx)"
		return err
	}
	return caller.Rollback(ctx)
}
