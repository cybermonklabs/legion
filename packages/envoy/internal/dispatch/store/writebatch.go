package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WriteBatch is a transaction's write phase as its writers queue it: every write statement, in
// the order the transaction made its writes, sent to Postgres as one pipelined batch (Send). The
// server runs the statements back to back and the client reads their results only once all of
// them have run, so no Go code of this process - no scheduling delay of the goroutine that sent
// them - comes between two of them while the transaction holds its transaction id.
//
// A statement's arguments are therefore known before the batch is sent. A value a later statement
// needs from an earlier one - an appended event's sequence or id, a row's timestamp - is derived in
// the later statement's SQL from what the earlier one left (the owner row's last_seq, the events
// id sequence's currval), never read back in Go between them. A value the caller needs once the
// batch has run - an event's id, a returned timestamp - is read by the statement's callback (Exec,
// QueryRow, Query), which runs as Send reads that statement's result, in statement order.
//
// Sent on a pool rather than on a transaction, a batch is a write of its own: the server runs its
// statements as one transaction, which commits once the last of them has run and rolls back whole
// when one fails, so a row a statement locks stays locked for the statements after it. A write
// that was one statement on the pool is sent so when it must wait on a row in a bounded wait
// (QueueWithinBound): the bound is a setting of the transaction the batch is, which ends with it,
// and the write is still one round trip, as the statement alone was.
type WriteBatch struct {
	batch pgx.Batch
	// bound is the bounded wait every statement queued within the bound waits in
	// (QueueWithinBound), nil until the first: Send sets it, and the statements that set
	// lock_timeout to it and the Queued values of the statements they bound share it.
	bound *batchBound
}

// batchBound is the bound a WriteBatch's bounded waits wait under, as lock_timeout's value: the
// bound of the context the batch is sent under (lockWaitSetting), which Send sets before it sends
// the batch. The statement that sets lock_timeout to it takes wait's address as its argument, and
// pgx reads an argument as it sends the batch, so the statement sets the bound Send chose.
type batchBound struct {
	wait string
}

// LatestEventID is the SQL expression for the id of the event this session appended last: the
// events id sequence's currval, which the event's insert set (events.Broker.QueueAppend). A
// statement of a write phase that records the id of the event appended just before it - the
// references a write stamps with its event, an ask.opened payload's own id - names it so, rather
// than reading the id back in Go between the two statements. Nothing between the two may append
// another event.
const LatestEventID = `currval(pg_get_serial_sequence('events', 'id'))`

// Queued is one statement of a WriteBatch. Unless one of its methods says otherwise, its result is
// read as an Exec's and dropped.
type Queued struct {
	query *pgx.QueuedQuery
	what  string
	// bound is the bounded wait the statement runs in (QueueWithinBound), nil for one queued
	// bare.
	bound *batchBound
}

// Queue adds sql with args after every statement queued so far. A failure of the statement itself
// comes back from Send wrapped as what failed ("what: err"); an empty what returns it unchanged,
// as a statement run on its own would.
func (b *WriteBatch) Queue(what, sql string, args ...any) Queued {
	queued := Queued{query: b.batch.Queue(sql, args...), what: what}
	queued.Exec(nil)
	return queued
}

// failed is err, the statement's own failure, wrapped as what failed: a statement queued within a
// bound that ran it out (ranOutItsBound) named ErrPoolBusy first (heldPastItsBound).
func (q Queued) failed(err error) error {
	if q.bound != nil && ranOutItsBound(err) {
		err = heldPastItsBound(q.bound.wait, err)
	}
	if err == nil || q.what == "" {
		return err
	}
	return fmt.Errorf("%s: %w", q.what, err)
}

// QueueWithinBound queues sql with args, a statement that takes a row lock another transaction
// may hold - a locking read, or a write of the row itself - in a row lock's bounded wait, under
// the bound LockRowsWithinBound waits under (lockWait, or the bound a test put on the context Send
// is given: batchBound): the transaction's own lock_timeout kept, lock_timeout set to the bound,
// the statement, and the kept value set again (boundedWaitKeep, boundedWaitBound,
// boundedWaitRestore). Its result is read as a statement Queue queued is (the Queued it returns).
// An admitted write has no deadline, so a write phase that queued the statement bare would wait on
// a held row for as long as its holder held it. A wait that runs the bound out fails the batch with
// ErrPoolBusy, which a request answers 503 DATABASE_BUSY with Retry-After; nothing queued after it
// runs, and the transaction - the one a failed write phase abandons, or the batch's own when it is
// sent on a pool - rolls back with everything it wrote. So it takes no savepoint, as no row lock's
// wait does: nothing is left to use. Any other failure of the statement comes back wrapped as what
// failed, as Queue's does.
func (b *WriteBatch) QueueWithinBound(what, sql string, args ...any) Queued {
	if b.bound == nil {
		b.bound = &batchBound{}
	}
	b.Queue("", boundedWaitKeep)
	b.Queue("", boundedWaitBound, &b.bound.wait)
	queued := Queued{query: b.batch.Queue(sql, args...), what: what, bound: b.bound}
	queued.Exec(nil)
	b.Queue("", boundedWaitRestore)
	return queued
}

// Exec hands the statement's command tag to fn once it has run; nil drops it. fn's error stops the
// batch's reading and is what Send returns, unchanged.
func (q Queued) Exec(fn func(pgconn.CommandTag) error) {
	q.query.Fn = func(results pgx.BatchResults) error {
		tag, err := results.Exec()
		if err != nil {
			return q.failed(err)
		}
		if fn == nil {
			return nil
		}
		return fn(tag)
	}
}

// QueryRow hands the statement's first row to scan once it has run, as tx.QueryRow would: Scan
// answers pgx.ErrNoRows when it returned none, and the statement's own failure wrapped as what
// failed. scan's error stops the batch's reading and is what Send returns, unchanged.
func (q Queued) QueryRow(scan func(pgx.Row) error) {
	q.Query(func(rows pgx.Rows) error {
		return scan(batchRow{rows: rows, queued: q})
	})
}

// Query hands the statement's rows to read once it has run. read's error stops the batch's reading
// and is what Send returns, unchanged; a failure the rows report once read returned is wrapped as
// what failed.
func (q Queued) Query(read func(pgx.Rows) error) {
	q.query.Fn = func(results pgx.BatchResults) error {
		rows, err := results.Query()
		if err != nil {
			return q.failed(err)
		}
		defer rows.Close()
		if err := read(rows); err != nil {
			return err
		}
		rows.Close()
		return q.failed(rows.Err())
	}
}

// batchRow is a queued statement's first row, read as pgx's own row of tx.QueryRow reads it.
type batchRow struct {
	rows   pgx.Rows
	queued Queued
}

func (r batchRow) Scan(dest ...any) error {
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return r.queued.failed(err)
		}
		return pgx.ErrNoRows
	}
	if err := r.rows.Scan(dest...); err != nil {
		return r.queued.failed(err)
	}
	r.rows.Close()
	return r.queued.failed(r.rows.Err())
}

// BatchSender is what a WriteBatch is sent on: the transaction whose write phase it is, or a pool,
// on which the batch is a write of its own (WriteBatch).
type BatchSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Send sends every queued statement to q in one batch and reads their results in order, running
// each statement's callback as it reads its result. It returns the first failure: the first
// statement that failed (the server runs none after it) or the first callback that returned an
// error (whose later statements ran, and are rolled back with the transaction the caller now
// abandons; on a pool they have committed with the batch), whichever comes first in the batch. The
// batch is empty again afterwards. An empty batch sends nothing.
func (b *WriteBatch) Send(ctx context.Context, q BatchSender) error {
	if b.batch.Len() == 0 {
		return nil
	}
	if b.bound != nil {
		b.bound.wait = lockWaitSetting(ctx)
	}
	batch := b.batch
	b.batch, b.bound = pgx.Batch{}, nil
	return q.SendBatch(ctx, &batch).Close()
}

// RunWrite runs write's statements in tx at once, as a batch of their own (Send): what a write
// step does in a transaction that queues no write phase.
func RunWrite(ctx context.Context, tx pgx.Tx, write func(*WriteBatch) error) error {
	var batch WriteBatch
	if err := write(&batch); err != nil {
		return batch.SendFailed(ctx, tx, err)
	}
	return batch.Send(ctx, tx)
}

// SendFailed ends a batch whose building failed with err partway: it sends what was queued before
// the failure and returns the first failure among those statements, else err. Run one at a time,
// those statements would have run before the write that failed was reached, so one of them failing
// is the first failure of the write phase, which the caller answers as it always has.
func (b *WriteBatch) SendFailed(ctx context.Context, tx pgx.Tx, err error) error {
	if sendErr := b.Send(ctx, tx); sendErr != nil {
		return sendErr
	}
	return err
}
