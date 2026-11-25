package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
)

// Under a cut, the database itself holds each statement of the pool to reading. Bind refuses a
// statement that is not a read, by its first word. But a read can call a function that writes,
// such as nextval, and Bind cannot see that. So every statement under a cut runs in a read-only
// transaction, and PostgreSQL refuses each write in it (SQLSTATE 25006). A transaction that Begin
// or BeginTx opens under a cut is read-only (Pool.begin). A statement of Query, QueryRow or Exec
// under a cut runs in a read-only transaction of its own, on one pooled connection, and so does
// a batch. The pool ends that transaction with a rollback when the statement or the batch is
// done (endRead).
//
// A statement that Bind changes holds its cut in its text, so each new cut makes each of its
// statements a new text. The pool sends such a statement with pgx.QueryExecModeCacheDescribe
// (boundArgs), so it leaves no prepared statement on its connection. pgx has no mode for one
// batch: it sends a batch in the mode of the connection's configuration, which is fixed when the
// pool opens the connection. So the pool sends the statements of a batch under a cut one at a
// time, each with its own mode (cutBatchResults).
//
// This costs round trips. A statement of Query, QueryRow or Exec under a cut takes two more than
// at the present: the begin and the rollback of its transaction. A batch under a cut takes one
// round trip for each of its statements, where pgx sends a whole batch in one. A batch that the
// pool sends takes two more for its own transaction; a batch of a transaction runs in that
// transaction. A described statement that the connection has not seen takes one more round trip
// for its description, at a cut as at the present.

// readAtCut is the options of each transaction that the pool opens of its own under a cut.
var readAtCut = pgx.TxOptions{AccessMode: pgx.ReadOnly}

// boundArgs is args for bound, the statement that Bind made of sql: args themselves where Bind
// changed nothing, else pgx.QueryExecModeCacheDescribe in front of them. A bound statement holds
// its cut in its text, so each new cut makes each of its statements a new text. pgx's default
// mode prepares each new text as a named statement of the connection and keeps 512 of them: each
// cut would leave a large plan of each statement on the server and push the present statements
// out of the cache, which must then be prepared again. A described statement is cached on the
// client alone, and one seen before takes one round trip as a prepared one does. A mode that the
// caller passes comes after this one and wins. A batch under a cut sends each of its statements
// with these arguments too (bindBatch).
func boundArgs(sql, bound string, args []any) []any {
	if bound == sql {
		return args
	}
	return append([]any{pgx.QueryExecModeCacheDescribe}, args...)
}

// refuseAtCut is the refusal of method when ctx carries a cut, or nil when it carries none.
func refuseAtCut(ctx context.Context, method string) error {
	if cut, ok := asof.CutFrom(ctx); ok {
		return &RefusedAtCutError{Method: method, Cut: cut}
	}
	return nil
}

// queryAtCut is Query under cut: sql bound to cut, in a read-only transaction of its own. The
// rows hold the transaction's connection, and the caller counts as holding it, until they close,
// as at the present (trackedRows). Their close ends the transaction.
func (p *Pool) queryAtCut(ctx context.Context, cut asof.Cut, sql string, args []any) (pgx.Rows, error) {
	bound, err := asof.Bind(sql, cut)
	if err != nil {
		return nil, err
	}
	tx, err := p.pool.BeginTx(ctx, readAtCut)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, bound, boundArgs(sql, bound, args)...)
	if err != nil {
		endRead(ctx, tx)
		return nil, err
	}
	release := hold(ctx)
	return &trackedRows{Rows: rows, release: func() {
		endRead(ctx, tx)
		if release != nil {
			release()
		}
	}}, nil
}

// queryRowAtCut is QueryRow under cut: sql bound to cut, in a read-only transaction of its own,
// which the row's Scan ends (cutRow).
func (p *Pool) queryRowAtCut(ctx context.Context, cut asof.Cut, sql string, args []any) pgx.Row {
	bound, err := asof.Bind(sql, cut)
	if err != nil {
		return errRow{err: err}
	}
	tx, err := p.pool.BeginTx(ctx, readAtCut)
	if err != nil {
		return errRow{err: err}
	}
	return cutRow{ctx: ctx, row: tx.QueryRow(ctx, bound, boundArgs(sql, bound, args)...), tx: tx}
}

// execAtCut is Exec under cut: sql bound to cut, in a read-only transaction of its own.
func (p *Pool) execAtCut(ctx context.Context, cut asof.Cut, sql string, args []any) (pgconn.CommandTag, error) {
	bound, err := asof.Bind(sql, cut)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tx, err := p.pool.BeginTx(ctx, readAtCut)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer endRead(ctx, tx)
	return tx.Exec(ctx, bound, boundArgs(sql, bound, args)...)
}

// sendBatchAtCut is SendBatch under cut: each statement of batch bound to cut, sent one at a time
// in a read-only transaction of its own, which Close ends (cutBatchResults). An empty batch sends
// nothing, so it goes to pgx as it is and opens no transaction.
func (p *Pool) sendBatchAtCut(ctx context.Context, cut asof.Cut, batch *pgx.Batch) pgx.BatchResults {
	if batch.Len() == 0 {
		return p.pool.SendBatch(ctx, batch)
	}
	statements, err := bindBatch(batch, cut)
	if err != nil {
		return errBatchResults{err: err}
	}
	tx, err := p.pool.BeginTx(ctx, readAtCut)
	if err != nil {
		return errBatchResults{err: err}
	}
	return &cutBatchResults{ctx: ctx, tx: tx, statements: statements, own: true}
}

// endRead ends tx, a read-only transaction that the pool opened of its own under a cut, and gives
// its connection back to the pool. The transaction has nothing to commit, so it rolls back. The
// rollback's error is not returned: what the statement read stands, and a rollback that fails
// closes its connection (pgx), so no connection stays inside the transaction. A second end finds
// the transaction closed and sends nothing.
func endRead(ctx context.Context, tx pgx.Tx) {
	_ = tx.Rollback(ctx)
}

// cutRow is the row of QueryRow under a cut. Its Scan ends the statement's own transaction once
// pgx's row has given the connection back to it.
type cutRow struct {
	ctx context.Context
	row pgx.Row
	tx  pgx.Tx
}

func (r cutRow) Scan(dest ...any) error {
	defer endRead(r.ctx, r.tx)
	return r.row.Scan(dest...)
}

// cutTx is a transaction opened under a cut. It binds each statement that it sends to that cut,
// whatever context the statement's call names, and so does each savepoint that it opens. The pool
// opens it read-only (Pool.begin), so its savepoints are read-only too. It refuses each way to
// send SQL that it cannot bind (RefusedAtCutError). Commit and Rollback are the transaction's
// own, so a trackedTx inside it still clears its hold mark and takes its BeforeCommit step.
type cutTx struct {
	pgx.Tx
	cut asof.Cut
}

func (t *cutTx) Begin(ctx context.Context) (pgx.Tx, error) {
	savepoint, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &cutTx{Tx: savepoint, cut: t.cut}, nil
}

func (t *cutTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	bound, err := asof.Bind(sql, t.cut)
	if err != nil {
		return nil, err
	}
	return t.Tx.Query(ctx, bound, boundArgs(sql, bound, args)...)
}

func (t *cutTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	bound, err := asof.Bind(sql, t.cut)
	if err != nil {
		return errRow{err: err}
	}
	return t.Tx.QueryRow(ctx, bound, boundArgs(sql, bound, args)...)
}

func (t *cutTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	bound, err := asof.Bind(sql, t.cut)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return t.Tx.Exec(ctx, bound, boundArgs(sql, bound, args)...)
}

// SendBatch sends each statement of batch bound to the cut, one at a time, in this transaction
// (cutBatchResults).
func (t *cutTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	statements, err := bindBatch(batch, t.cut)
	if err != nil {
		return errBatchResults{err: err}
	}
	return &cutBatchResults{ctx: ctx, tx: t.Tx, statements: statements}
}

func (t *cutTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, &RefusedAtCutError{Method: "Tx.CopyFrom", Cut: t.cut}
}

func (t *cutTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, &RefusedAtCutError{Method: "Tx.Prepare", Cut: t.cut}
}

// LargeObjects panics with its refusal: it returns no error.
func (t *cutTx) LargeObjects() pgx.LargeObjects {
	panic(&RefusedAtCutError{Method: "Tx.LargeObjects", Cut: t.cut})
}

// Conn panics with its refusal: it returns no error.
func (t *cutTx) Conn() *pgx.Conn {
	panic(&RefusedAtCutError{Method: "Tx.Conn", Cut: t.cut})
}

// cutStatement is one statement of a batch under a cut: its SQL bound to the cut, its arguments
// with the mode of a bound statement (boundArgs), and the callback that the caller queued with it.
type cutStatement struct {
	sql  string
	args []any
	fn   func(pgx.BatchResults) error
}

// bindBatch is each statement that batch queues, bound to cut (asof.Bind), and leaves the caller's
// batch as the caller wrote it. Bind's refusal of one statement refuses the batch, before any
// statement is sent. A batch that begins its own transaction (pgx.Batch.BeginTx) queues a begin
// first, which Bind refuses.
func bindBatch(batch *pgx.Batch, cut asof.Cut) ([]cutStatement, error) {
	statements := make([]cutStatement, len(batch.QueuedQueries))
	for index, queued := range batch.QueuedQueries {
		bound, err := asof.Bind(queued.SQL, cut)
		if err != nil {
			return nil, err
		}
		statements[index] = cutStatement{sql: bound, args: boundArgs(queued.SQL, bound, queued.Arguments), fn: queued.Fn}
	}
	return statements, nil
}

// cutBatchResults is a batch under a cut, sent one statement at a time in tx, on tx's one
// connection: Exec, Query and QueryRow each send the next statement and read its result. It
// answers as a batch that pgx sends answers (pgx.BatchResults). The first error of a statement,
// or of the rows of a statement, stops the batch: each later read returns that error and sends
// nothing. Close runs the callbacks of the statements not read yet, in order (pgx.QueuedQuery.Fn),
// or sends the statement where it has none, until one fails, and returns the batch's first error.
type cutBatchResults struct {
	ctx        context.Context
	tx         pgx.Tx
	statements []cutStatement
	// own is whether tx is the batch's own read-only transaction, which Close ends. A batch of a
	// transaction under a cut runs in that transaction and leaves it open.
	own bool
	// next is the index of the next statement to send.
	next int
	// rows is the rows of the last statement that Query or QueryRow sent, which must close before
	// the connection can send the next statement.
	rows   pgx.Rows
	err    error
	closed bool
}

// statement is the next statement to send, once the rows of the statement before it have closed.
// It answers the batch's error instead, when a statement or its rows have failed.
func (b *cutBatchResults) statement() (cutStatement, error) {
	if b.err != nil {
		return cutStatement{}, b.err
	}
	if b.closed {
		return cutStatement{}, errors.New("batch already closed")
	}
	if b.rows != nil {
		b.rows.Close()
		b.err = b.rows.Err()
		b.rows = nil
		if b.err != nil {
			return cutStatement{}, b.err
		}
	}
	if b.next == len(b.statements) {
		return cutStatement{}, errors.New("no more results in batch")
	}
	statement := b.statements[b.next]
	b.next++
	return statement, nil
}

func (b *cutBatchResults) Exec() (pgconn.CommandTag, error) {
	statement, err := b.statement()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := b.tx.Exec(b.ctx, statement.sql, statement.args...)
	if err != nil {
		b.err = err
	}
	return tag, err
}

func (b *cutBatchResults) Query() (pgx.Rows, error) {
	statement, err := b.statement()
	if err != nil {
		return failedRows(err), err
	}
	rows, err := b.tx.Query(b.ctx, statement.sql, statement.args...)
	if err != nil {
		b.err = err
		return failedRows(err), err
	}
	b.rows = rows
	return rows, nil
}

// QueryRow reads the first row of the next statement as pgx's row of QueryRow reads it, with the
// row reader of a write batch's statement (batchRow), which wraps no error here.
func (b *cutBatchResults) QueryRow() pgx.Row {
	rows, _ := b.Query()
	return batchRow{rows: rows}
}

func (b *cutBatchResults) Close() error {
	if b.closed {
		return b.err
	}
	for b.err == nil && b.next < len(b.statements) {
		if fn := b.statements[b.next].fn; fn != nil {
			if err := fn(b); err != nil {
				b.err = err
			}
		} else {
			_, _ = b.Exec()
		}
	}
	if b.rows != nil {
		b.rows.Close()
		if b.err == nil {
			b.err = b.rows.Err()
		}
		b.rows = nil
	}
	b.closed = true
	if b.own {
		endRead(b.ctx, b.tx)
	}
	return b.err
}

// Tx is nil: a batch under a cut begins no transaction of its own (bindBatch).
func (b *cutBatchResults) Tx() pgx.Tx { return nil }

// failedRows is rows that carry err alone, as the rows of a failed pgx batch do: Next is false,
// and Err and Scan return err.
func failedRows(err error) pgx.Rows {
	rows, _ := pgx.FailedBatchResults(&pgx.Batch{}, err).Query()
	return rows
}

// RefusedAtCutError is a method that a read at a cut cannot use. The pool refuses it under a
// context that carries a cut (asof.WithCut), before it sends anything. The pool refuses Acquire,
// AcquireFunc, Listener, Dedicated and CopyFrom: a connection that the pool hands out sends
// statements that nothing binds, and a copy writes. It refuses a transaction whose begin query the
// caller writes (pgx.TxOptions.BeginQuery), because that query sets its own access mode, and the
// pool opens each transaction under a cut read-only. A transaction opened under a cut refuses its
// CopyFrom, Prepare, LargeObjects and Conn for the same reasons; LargeObjects and Conn return no
// error, so they panic with this error.
type RefusedAtCutError struct {
	// Method names the refused method: "Acquire", "AcquireFunc", "Listener", "Dedicated",
	// "CopyFrom", "BeginTx with BeginQuery", "Tx.CopyFrom", "Tx.Prepare", "Tx.LargeObjects" or
	// "Tx.Conn".
	Method string
	Cut    asof.Cut
}

func (e *RefusedAtCutError) Error() string {
	return "store: " + e.Method + " is refused under the cut " + e.Cut.String() +
		": a read at a cut sends only the statements that asof.Bind binds"
}
