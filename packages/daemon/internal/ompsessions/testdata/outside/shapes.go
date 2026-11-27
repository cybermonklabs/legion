// A fixture of TestNoStatementRunsOutsideTheHelper: the Go shapes that carry a transaction the
// helper did not pin into a function handed to it, or a driver handle past the check. A local
// copy of the caller's transaction, a range variable over callers' transactions, and a struct
// field built from one; a pgx connection held in an interface the package declares, and the same
// through a type parameter that interface constrains, or one an interface written in its place
// constrains; a pinned transaction built outside the helper; and a large object's Close, which
// runs a statement (select lo_close) although a handle's Close runs none. The check must name each
// line that carries a want comment, and no other.
package fixture

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type pinned struct{ tx pgx.Tx }

func (p pinned) exec(ctx context.Context, sql string) (pgconn.CommandTag, error) {
	return p.tx.Exec(ctx, sql)
}

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

func inside(ctx context.Context, conn *pgx.Conn) error {
	return inTransaction(ctx, conn, false, func(p pinned) error {
		_, err := p.exec(ctx, "SELECT 1")
		return err
	})
}

func copied(ctx context.Context, conn *pgx.Conn, caller pgx.Tx) error {
	return inTransaction(ctx, conn, true, func(pinned) error {
		t := caller
		_, err := t.Exec(ctx, "SELECT 1") // want "t.Exec (a method of pgx.Tx)"
		return err
	})
}

func ranged(ctx context.Context, conn *pgx.Conn, callers []pgx.Tx) error {
	return inTransaction(ctx, conn, true, func(pinned) error {
		for _, t := range callers {
			if _, err := t.Exec(ctx, "SELECT 1"); err != nil { // want "t.Exec (a method of pgx.Tx)"
				return err
			}
		}
		return nil
	})
}

type holder struct{ tx pgx.Tx }

func fielded(ctx context.Context, conn *pgx.Conn, caller pgx.Tx) error {
	return inTransaction(ctx, conn, true, func(pinned) error {
		h := holder{tx: caller}
		_, err := h.tx.Exec(ctx, "SELECT 1") // want "h.tx.Exec (a method of pgx.Tx)"
		return err
	})
}

type execer interface { // want "an interface a driver handle implements"
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func throughInterface(ctx context.Context, conn *pgx.Conn) error {
	var e execer = conn
	_, err := e.Exec(ctx, "SELECT 1")
	return err
}

func throughTypeParameter[E execer](ctx context.Context, e E) error {
	_, err := e.Exec(ctx, "SELECT 1")
	return err
}

func throughWrittenConstraint[E interface { // want "an interface a driver handle implements"
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}](ctx context.Context, e E) error {
	_, err := e.Exec(ctx, "SELECT 1")
	return err
}

func forged(ctx context.Context, caller pgx.Tx) error {
	_, err := pinned{tx: caller}.exec(ctx, "SELECT 1") // want "pinned{…} (a pinned transaction built outside inTransaction)"
	return err
}

func largeObject(object *pgx.LargeObject) error {
	return object.Close() // want "object.Close (a method of *pgx.LargeObject)"
}

func zeroValue(ctx context.Context, caller pgx.Tx) error {
	var p pinned
	p.tx = caller // want "p.tx (the transaction a pinned holds, reached outside pinned's methods)"
	_, err := p.exec(ctx, "SELECT 1")
	return err
}

func allocated(ctx context.Context, caller pgx.Tx) error {
	p := new(pinned)
	p.tx = caller // want "p.tx (the transaction a pinned holds, reached outside pinned's methods)"
	_, err := p.exec(ctx, "SELECT 1")
	return err
}
