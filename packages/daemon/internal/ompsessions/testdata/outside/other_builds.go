// A fixture of TestNoStatementRunsOutsideTheHelper: a pinned or a Conn built where no composite
// literal or conversion names its type. An element of a composite literal of pointers, whose &T
// the literal elides; a pointer converted to a pointer to one; a struct type of its shape written
// out, whose values are assignable to it, built by a composite literal or from its zero value with
// its field set, and then assigned to one; and a composite literal or a conversion of a type
// parameter it satisfies. A composite literal of a type of its own with the same shape builds
// none, nor does one of a type parameter it does not satisfy, nor a conversion of a type parameter
// whose constraint names no type (any, comparable), which converts only a value of its own type,
// nor nil converted to a pointer to one. The check must name each line that carries a want
// comment, and no other.
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

func elided(ctx context.Context, caller pgx.Tx) error {
	transactions := []*pinned{{tx: caller}} // want "&pinned{…} (a pinned transaction built outside inTransaction)"
	_, err := transactions[0].exec(ctx, "SELECT 1")
	return err
}

func pointerConverted(ctx context.Context, loose *looseTransaction) error {
	_, err := (*pinned)(loose).exec(ctx, "SELECT 1") // want "(*pinned)(loose) (a pinned transaction built outside inTransaction)"
	return err
}

func writtenOut(ctx context.Context, caller pgx.Tx) error {
	var p pinned = struct{ tx pgx.Tx }{caller} // want "struct{tx pgx.Tx}{…} (a pinned transaction built outside inTransaction)"
	_, err := p.exec(ctx, "SELECT 1")
	return err
}

func writtenOutAndFielded(ctx context.Context, caller pgx.Tx) error {
	var shaped struct{ tx pgx.Tx }
	shaped.tx = caller    // want "shaped.tx (the transaction a pinned holds, reached outside pinned's methods)"
	var p pinned = shaped // want "shaped (a value of pinned's struct type, which becomes a pinned with no conversion, outside inTransaction)"
	_, err := p.exec(ctx, "SELECT 1")
	return err
}

func generic[T ~struct{ tx pgx.Tx }](caller pgx.Tx) T {
	return T{tx: caller} // want "T{…} (a pinned transaction built outside inTransaction)"
}

func genericConverted[T ~struct{ tx pgx.Tx }](loose looseTransaction) T {
	return T(loose) // want "T(loose) (a pinned transaction built outside inTransaction)"
}

func genericPointer[T interface{ *pinned }](loose *looseTransaction) T {
	return T(loose) // want "T(loose) (a pinned transaction built outside inTransaction)"
}

type looseConnection struct{ c *pgx.Conn }

func elidedConn(raw *pgx.Conn) *Conn {
	return []*Conn{{c: raw}}[0] // want "&Conn{…} (a Conn built outside ConnectURL)"
}

func pointerConvertedConn(loose *looseConnection) *Conn {
	return (*Conn)(loose) // want "(*Conn)(loose) (a Conn built outside ConnectURL)"
}

func writtenOutConn(raw *pgx.Conn) Conn {
	return struct{ c *pgx.Conn }{raw} // want "struct{c *pgx.Conn}{…} (a Conn built outside ConnectURL)"
}

func writtenOutAndFieldedConn(raw *pgx.Conn) Conn {
	var shaped struct{ c *pgx.Conn }
	shaped.c = raw // want "shaped.c (the connection a Conn holds, reached outside inTransaction, ConnectURL and Conn's Close and IsClosed)"
	return shaped  // want "shaped (a value of Conn's struct type, which becomes a Conn with no conversion, outside ConnectURL)"
}

func genericConn[T ~struct{ c *pgx.Conn }](raw *pgx.Conn) T {
	return T{c: raw} // want "T{…} (a Conn built outside ConnectURL)"
}

func genericConvertedConn[T ~struct{ c *pgx.Conn }](loose looseConnection) T {
	return T(loose) // want "T(loose) (a Conn built outside ConnectURL)"
}

type holder struct{ tx pgx.Tx }

func ownShapes(caller pgx.Tx, raw *pgx.Conn) ([]*holder, looseConnection) {
	return []*holder{{tx: caller}}, looseConnection{c: raw}
}

func otherParameter[T ~struct{ n int }]() T {
	return T{n: 1}
}

func identity[T any](v T) T {
	return T(v)
}

func identityComparable[T comparable](v T) T {
	return T(v)
}

var _ any = (*Conn)(nil)

var _ any = (*pinned)(nil)
