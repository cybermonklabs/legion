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
// A batch is sent in a transaction (Send), where a callback's error abandons the transaction and
// so undoes every statement of the batch, or on the pool as a transaction of its own that commits
// at the batch's end (SendCommitting), where nothing can undo it: such a batch may read an answer
// only through Observe, and every other callback is refused before anything is sent.
type WriteBatch struct {
	batch pgx.Batch
	// reads is the read each statement queued so far makes of its result, in statement order.
	reads []*queuedRead
}

// queuedRead is how a statement's result is read: dropped, handed to an Exec callback, handed to
// a QueryRow or Query callback, whose error refuses the write, or observed (Observe).
type queuedRead int

const (
	readDropped queuedRead = iota
	readExec
	readRefusing
	readObserved
)

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
	read  *queuedRead
}

// Queue adds sql with args after every statement queued so far. A failure of the statement itself
// comes back from Send wrapped as what failed ("what: err"); an empty what returns it unchanged,
// as a statement run on its own would.
func (b *WriteBatch) Queue(what, sql string, args ...any) Queued {
	read := new(queuedRead)
	b.reads = append(b.reads, read)
	queued := Queued{query: b.batch.Queue(sql, args...), what: what, read: read}
	queued.Exec(nil)
	return queued
}

// failed is err, the statement's own failure, wrapped as what failed.
func (q Queued) failed(err error) error {
	if err == nil || q.what == "" {
		return err
	}
	return fmt.Errorf("%s: %w", q.what, err)
}

// Exec hands the statement's command tag to fn once it has run; nil drops it. fn's error stops the
// batch's reading and is what Send returns, unchanged. A batch that commits at its end
// (SendCommitting) takes no fn.
func (q Queued) Exec(fn func(pgconn.CommandTag) error) {
	*q.read = readDropped
	if fn != nil {
		*q.read = readExec
	}
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
// what failed. A batch that commits at its end (SendCommitting) takes no Query or QueryRow: their
// error refuses the write, which there nothing can undo.
func (q Queued) Query(read func(pgx.Rows) error) {
	q.readRows(readRefusing, read)
}

// Observe hands the rows of a statement of a batch that commits at its end (SendCommitting) to
// read once it has run: the answer of a statement that has committed, or will once the batch
// ends. read's error, and a failure the rows report, mean only that this answer was lost, never
// that the write must not stand: the batch's statements commit together all the same, and the
// caller answers the error as it answers an answer the network lost after the commit. A batch
// sent in a transaction (Send) takes no Observe, so a read whose error must undo the write is
// never written as one.
func (q Queued) Observe(read func(pgx.Rows) error) {
	q.readRows(readObserved, read)
}

// readRows hands the statement's rows to read once it has run, read as kind.
func (q Queued) readRows(kind queuedRead, read func(pgx.Rows) error) {
	*q.read = kind
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

// Send sends every queued statement to tx in one batch and reads their results in order, running
// each statement's callback as it reads its result. It returns the first failure: the first
// statement that failed (the server runs none after it) or the first callback that returned an
// error (whose later statements ran, and are rolled back with the transaction the caller now
// abandons), whichever comes first in the batch. The batch is empty again afterwards. An empty
// batch sends nothing. A batch that queued an Observe is refused before anything is sent: an
// observed read is one whose error undoes nothing, which in a transaction it would.
func (b *WriteBatch) Send(ctx context.Context, tx pgx.Tx) error {
	if b.batch.Len() == 0 {
		return nil
	}
	if err := b.refuse(readObserved, "observes a statement's answer, which only a batch that commits at its end may (SendCommitting)"); err != nil {
		return err
	}
	batch := b.batch
	b.batch, b.reads = pgx.Batch{}, nil
	return tx.SendBatch(ctx, &batch).Close()
}

// SendCommitting sends every queued statement to pool in one batch, outside any transaction: the
// server runs the batch as one implicit transaction, which commits at the batch's end (its Sync)
// whatever the client reads of it afterwards. So every statement commits or none does, and no
// callback can undo the batch once it is sent: before anything is sent, it refuses a batch that
// queued a QueryRow, a Query or an Exec callback, whose error would read as a refusal of a write
// that has already committed. A statement's answer is read through Observe alone. It returns the
// first failure: a statement that failed, which leaves nothing of the batch committed, or an
// observed read that failed, which leaves all of it committed. The batch is empty again afterwards
// it is sent, and an empty batch sends nothing.
func (b *WriteBatch) SendCommitting(ctx context.Context, pool *Pool) error {
	if b.batch.Len() == 0 {
		return nil
	}
	if err := b.refuse(readRefusing, "reads a statement's answer with QueryRow or Query, whose error would refuse a write that commits at the batch's end"); err != nil {
		return err
	}
	if err := b.refuse(readExec, "hands a statement's command tag to an Exec callback, whose error would refuse a write that commits at the batch's end"); err != nil {
		return err
	}
	batch := b.batch
	b.batch, b.reads = pgx.Batch{}, nil
	return pool.SendBatch(ctx, &batch).Close()
}

// refuse answers an error naming the first statement queued so far whose result is read as kind,
// and nil where none is.
func (b *WriteBatch) refuse(kind queuedRead, why string) error {
	for index, read := range b.reads {
		if *read == kind {
			return fmt.Errorf("store: statement %d of the batch %s", index+1, why)
		}
	}
	return nil
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

// RunCommitting runs write's statements on pool as one batch that commits at its end
// (SendCommitting): a write of its own transaction whose statements are all known before any of
// them runs. A write that fails while it queues sends nothing, since whatever it queued before
// would commit.
func RunCommitting(ctx context.Context, pool *Pool, write func(*WriteBatch) error) error {
	var batch WriteBatch
	if err := write(&batch); err != nil {
		return err
	}
	return batch.SendCommitting(ctx, pool)
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
