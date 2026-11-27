// A fixture of TestNoStatementRunsOutsideTheHelper: reflect and unsafe, through which code builds
// a pinned holding a caller's transaction with no composite literal and no conversion the check
// reads, or sets the field of one with no selector. The package's code imports neither, so the
// check names each import, and the other imports go unnamed. The check must name each line that
// carries a want comment, and no other.
package fixture

import (
	"context"
	"reflect" // want "the import of reflect (through which code builds a pinned or a Conn, or reaches a field of one, where the check cannot see)"
	"unsafe"  // want "the import of unsafe (through which code builds a pinned or a Conn, or reaches a field of one, where the check cannot see)"

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

type looseTransaction struct{ tx pgx.Tx }

func reflected(ctx context.Context, caller pgx.Tx) error {
	p := reflect.New(reflect.TypeFor[pinned]())
	p.Elem().Set(reflect.ValueOf(looseTransaction{caller}).Convert(p.Elem().Type()))
	_, err := p.Elem().Interface().(pinned).exec(ctx, "SELECT 1")
	return err
}

func throughUnsafe(ctx context.Context, caller pgx.Tx) error {
	var p pinned
	*(*pgx.Tx)(unsafe.Pointer(&p)) = caller
	_, err := p.exec(ctx, "SELECT 1")
	return err
}
