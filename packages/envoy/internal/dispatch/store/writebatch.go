package store

import (
	"context"
	"fmt"
	"math"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// WriteBatch is a transaction's write phase as its writers queue it: every write statement, in
// the order the transaction made its writes, sent to Postgres as one pipelined batch (Send), or,
// for a phase of thousands of rows, as a few in a row. The server runs the statements back to back
// and the client reads their results only once all of them have run, so no Go code of this
// process - no scheduling delay of the goroutine that sent them - comes between two of them while
// the transaction holds its transaction id, but for the round trip between two batches of such a
// phase.
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
	// builds holds, beside each statement queued, the function that builds its arguments when
	// Send reaches it (QueueBuilt), and nil for a statement queued with its arguments.
	builds []func() ([]any, error)
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
//
// The currval is a scalar subquery so that a statement finding the event by it reads one row of
// events through its primary key. currval is volatile, and Postgres compares a column with a
// volatile expression row by row, so `events.id = currval(...)` read every row of events, the
// whole table on each review and each ask opened. An uncorrelated subquery runs at most once, when
// the statement first needs its value, and that value can be an index key: for
// `events.id = (select currval(...))` it runs as the index scan of events_pkey starts, and the
// scan reads the one row. It is the same value, since nothing in the statement draws from the
// sequence.
const LatestEventID = `(select currval(pg_get_serial_sequence('events', 'id')))`

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
	return b.queue(what, nil, sql, args...)
}

// queue is Queue with the bounded wait the statement runs in (QueueWithinBound), nil for a
// statement queued bare. It sets the bound before Exec copies the Queued into the statement's
// callback, which reads the bound when the statement fails (failed).
func (b *WriteBatch) queue(what string, bound *batchBound, sql string, args ...any) Queued {
	queued := Queued{query: b.batch.Queue(sql, args...), what: what, bound: bound}
	b.builds = append(b.builds, nil)
	queued.Exec(nil)
	return queued
}

// QueueBuilt is Queue for a statement whose arguments build returns when Send reaches it, rather
// than now: a write of thousands of rows queues its statements with the rows they read, and the
// rows are held as arguments only for the part Send is sending, and for the statement after it,
// which Send builds to measure, and are dropped once that part has run. build reads the rows as
// they stand then, so they must not change after this call. Its error, wrapped as what failed,
// ends Send as a statement's own failure would: after the statements queued before it, whose
// own failure comes first.
func (b *WriteBatch) QueueBuilt(what, sql string, build func() ([]any, error)) Queued {
	queued := Queued{query: b.batch.Queue(sql), what: what}
	b.builds = append(b.builds, func() ([]any, error) {
		arguments, err := build()
		return arguments, queued.failed(err)
	})
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
	queued := b.queue(what, b.bound, sql, args...)
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

// Send sends every queued statement to q and reads their results in order, running each
// statement's callback as it reads its result. It returns the first failure: the first statement
// that failed (the server runs none after it) or the first callback that returned an error (whose
// later statements ran or were never sent, and are rolled back with the transaction the caller now
// abandons; on a pool they have committed with the batch), whichever comes first in the batch. The
// batch is empty again afterwards. An empty batch sends nothing.
//
// The statements go as one pipelined batch while their arguments come to sendBound or less, which
// is every write phase but the few that carry thousands of rows. A larger one goes as several
// batches in a row, each read before the next is sent (partEnd): pgx encodes the whole of a batch
// into one buffer before it sends any of it, beside each statement's own encoding of its
// arguments, so a version of 150,000 blocks held some 740 MB of encoded statements at once as one
// batch. In parts it holds one part's. A statement queued with QueueBuilt has its arguments built
// as its part is cut, and every statement's arguments are dropped once its part has run, so a
// write's rows are held as arguments only for the part that sends them and for the statement
// after it, which partEnd builds to measure. A build that fails sends the statements before it
// first, so their own failure, where one fails, is the one Send returns. The round trip between
// two parts is the one gap the order above allows, and it is taken only by a write phase whose
// statements run for seconds on their own. Sent on anything but a transaction (a pool), the batch
// goes as one batch whatever its size: it is the one transaction its statements run in
// (WriteBatch), which a second batch would not be part of.
func (b *WriteBatch) Send(ctx context.Context, q BatchSender) error {
	if b.batch.Len() == 0 {
		return nil
	}
	if b.bound != nil {
		b.bound.wait = lockWaitSetting(ctx)
	}
	queued, builds := b.batch.QueuedQueries, b.builds
	b.batch, b.builds, b.bound = pgx.Batch{}, nil, nil
	partBytes := sendBound
	if _, inTransaction := q.(pgx.Tx); !inTransaction {
		partBytes = math.MaxInt
	}
	// measured is each statement's argument bytes once its arguments are built, -1 before: partEnd
	// asks the size of the statement that starts the next part as well. On a pool, which cuts no
	// part, a statement's arguments are built and not counted, and its size is 0.
	measured := make([]int, len(queued))
	for index := range measured {
		measured[index] = -1
	}
	size := func(index int) (int, error) {
		if measured[index] >= 0 {
			return measured[index], nil
		}
		if build := builds[index]; build != nil {
			arguments, err := build()
			if err != nil {
				return 0, err
			}
			queued[index].Arguments = arguments
		}
		if partBytes == math.MaxInt {
			measured[index] = 0
			return 0, nil
		}
		bytes := 0
		for _, argument := range queued[index].Arguments {
			bytes += argumentBytes(argument)
		}
		measured[index] = bytes
		return bytes, nil
	}
	for start := 0; start < len(queued); {
		end, buildErr := partEnd(start, len(queued), partBytes, size)
		if end > start {
			batch := pgx.Batch{QueuedQueries: queued[start:end]}
			if err := q.SendBatch(ctx, &batch).Close(); err != nil {
				return err
			}
			for index := start; index < end; index++ {
				queued[index].Arguments, queued[index] = nil, nil
			}
		}
		if buildErr != nil {
			return buildErr
		}
		start = end
	}
	return nil
}

// sendBound is the most argument bytes (argumentBytes) one batch of a write phase carries, past
// which Send sends the rest as another batch. A statement whose own arguments pass it is a batch
// of its own.
const sendBound = 16 << 20

// partEnd is the end of the batch Send sends from statement start of count: as many statements
// as fit in bound together, and a statement past bound on its own. size gives a statement's
// argument bytes, and is asked once for each statement in order, the one past the batch included,
// which is the next batch's first. Where size fails, the end is that statement, so the batch is
// the statements before it, and the failure comes back beside it.
func partEnd(start, count, bound int, size func(index int) (int, error)) (int, error) {
	total := 0
	for index := start; index < count; index++ {
		bytes, err := size(index)
		if err != nil {
			return index, err
		}
		if index > start && total+bytes > bound {
			return index, nil
		}
		total += bytes
	}
	return count, nil
}

// argumentBytes is about how many bytes pgx sends for argument: a string's or a byte slice's
// length, an array's elements with the four bytes of length the wire puts before each, and eight
// for any other value. It reads the arrays of text and bytes the store's large writes bind, which
// is where a write phase's size is; a value pgx encodes as JSON from a map or a struct counts as
// one scalar, so a phase of many of those can pass sendBound before Send cuts it.
func argumentBytes(argument any) int {
	if bytes, known := knownArgumentBytes(argument, elementLengthBytes); known {
		return bytes
	}
	return valueBytes(reflect.ValueOf(argument))
}

// elementLengthBytes is the length the wire carries before each element of an array.
const elementLengthBytes = 4

// valueBytes is argumentBytes of a value of a kind knownArgumentBytes does not name.
func valueBytes(value reflect.Value) int {
	switch value.Kind() {
	case reflect.Invalid:
		return 0
	case reflect.String:
		return value.Len()
	case reflect.Pointer, reflect.Interface:
		if value.IsNil() {
			return 0
		}
		return valueBytes(value.Elem())
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return value.Len()
		}
		bytes := 0
		for index := range value.Len() {
			bytes += elementLengthBytes + valueBytes(value.Index(index))
		}
		return bytes
	default:
		return 8
	}
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
