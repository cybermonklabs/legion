// A fixture of TestNoStatementRunsOutsideTheHelper: the other ways a statement runs outside the
// helper. A transaction it did not open, begun on the connection or handed in by a caller;
// pgx.BeginFunc; a statement method taken as a value; pgx's own PgConn under a connection; a
// statement prepared on the connection and run on its PgConn; a type loaded by name, which runs a
// query under the login's own search_path; a caller's transaction used inside a function handed
// to the helper, which runs in the caller's unpinned transaction; and the raw network connection
// under a PgConn, on which any bytes of the protocol can be written. The check must name each line
// that carries a want comment, and no other.
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
		run := func(sql string) error {
			_, err := p.exec(ctx, sql)
			return err
		}
		return run("SELECT 1")
	})
}

func begun(ctx context.Context, conn *pgx.Conn) error {
	tx, err := conn.Begin(ctx) // want "conn.Begin (a method of *pgx.Conn)"
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT 1") // want "tx.Exec (a method of pgx.Tx)"
	return err
}

func beginFunc(ctx context.Context, conn *pgx.Conn) error {
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { // want "pgx.BeginFunc (a function of pgx)"
		_, err := tx.Exec(ctx, "SELECT 1") // want "tx.Exec (a method of pgx.Tx)"
		return err
	})
}

// Handed runs a statement in a transaction its caller hands it, which the helper did not open.
func Handed(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, "SELECT 1") // want "tx.Exec (a method of pgx.Tx)"
	return err
}

func value(conn *pgx.Conn) func(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return conn.Exec // want "conn.Exec (a method of *pgx.Conn)"
}

func underneath(ctx context.Context, conn *pgx.Conn) error {
	return conn.PgConn().Exec(ctx, "SELECT 1").Close() // want "conn.PgConn().Exec (a method of *pgconn.PgConn)"
}

func prepared(ctx context.Context, conn *pgx.Conn) error {
	statement, err := conn.Prepare(ctx, "", "SELECT 1") // want "conn.Prepare (a method of *pgx.Conn)"
	if err != nil {
		return err
	}
	_, err = conn.PgConn().ExecStatement(ctx, statement, nil, nil, nil).Close() // want "conn.PgConn().ExecStatement (a method of *pgconn.PgConn)"
	return err
}

func loaded(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.LoadType(ctx, "session_kind") // want "conn.LoadType (a method of *pgx.Conn)"
	return err
}

func callers(ctx context.Context, conn *pgx.Conn, caller pgx.Tx) error {
	return inTransaction(ctx, conn, true, func(pinned) error {
		_, err := caller.Exec(ctx, "SELECT 1") // want "caller.Exec (a method of pgx.Tx)"
		return err
	})
}

func raw(conn *pgx.Conn) error {
	_, err := conn.PgConn().Conn().Write([]byte{'Q'}) // want "conn.PgConn().Conn (a method of *pgconn.PgConn)"
	return err
}
