package store

import (
	"context"
	"fmt"
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
// A batch is sent in a transaction (Send), where a callback's error abandons the transaction and
// so undoes every statement of the batch, or on the pool as a transaction of its own that commits
// at the batch's end (SendCommitting), where nothing can undo it: such a batch may read an answer
// only through Observe, and every other callback is refused before anything is sent.
type WriteBatch struct {
	batch pgx.Batch
	// reads is the read each statement queued so far makes of its result, in statement order.
	reads []*queuedRead
	// builds holds, beside each statement queued, the function that builds its arguments when
	// Send reaches it (QueueBuilt), and nil for a statement queued with its arguments.
	builds []func() ([]any, error)
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
	read  *queuedRead
}

// Queue adds sql with args after every statement queued so far. A failure of the statement itself
// comes back from Send wrapped as what failed ("what: err"); an empty what returns it unchanged,
// as a statement run on its own would.
func (b *WriteBatch) Queue(what, sql string, args ...any) Queued {
	b.builds = append(b.builds, nil)
	return b.queue(what, sql, args)
}

// QueueBuilt is Queue for a statement whose arguments build returns when Send reaches it, rather
// than now: a write of thousands of rows queues its statements with the rows they read, and the
// rows are held as arguments only for the part Send is sending, and for the statement after it,
// which Send builds to measure, and are dropped once that part has run. build reads the rows as
// they stand then, so they must not change after this call. Its error, wrapped as what failed,
// ends Send as a statement's own failure would: after the statements queued before it, whose
// own failure comes first.
func (b *WriteBatch) QueueBuilt(what, sql string, build func() ([]any, error)) Queued {
	queued := b.queue(what, sql, nil)
	b.builds = append(b.builds, func() ([]any, error) {
		arguments, err := build()
		return arguments, queued.failed(err)
	})
	return queued
}

// queue adds sql with args after every statement queued so far, with the read it makes of its
// result (reads), which starts as an Exec's that drops it. Its caller adds its build (builds).
func (b *WriteBatch) queue(what, sql string, args []any) Queued {
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

// Send sends every queued statement to tx and reads their results in order, running each
// statement's callback as it reads its result. It returns the first failure: the first statement
// that failed (the server runs none after it) or the first callback that returned an error (whose
// later statements ran or were never sent, and are rolled back with the transaction the caller now
// abandons), whichever comes first in the batch. An empty batch sends nothing. A batch that
// queued an Observe is refused before anything is sent, and keeps its statements: an observed read
// is one whose error undoes nothing, which in a transaction it would. Any other batch is empty
// again afterwards.
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
// statements run for seconds on their own.
func (b *WriteBatch) Send(ctx context.Context, tx pgx.Tx) error {
	if b.batch.Len() == 0 {
		return nil
	}
	if err := b.refuse(readObserved, "observes a statement's answer, which only a batch that commits at its end may (SendCommitting)"); err != nil {
		return err
	}
	queued, builds := b.batch.QueuedQueries, b.builds
	b.batch, b.builds, b.reads = pgx.Batch{}, nil, nil
	// measured is each statement's argument bytes once its arguments are built, -1 before: partEnd
	// asks the size of the statement that starts the next part as well.
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
		bytes := 0
		for _, argument := range queued[index].Arguments {
			bytes += argumentBytes(argument)
		}
		measured[index] = bytes
		return bytes, nil
	}
	for start := 0; start < len(queued); {
		end, buildErr := partEnd(start, len(queued), sendBound, size)
		if end > start {
			batch := pgx.Batch{QueuedQueries: queued[start:end]}
			if err := tx.SendBatch(ctx, &batch).Close(); err != nil {
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

// SendCommitting sends every queued statement to pool in one batch, outside any transaction: the
// server runs the batch as one implicit transaction, which commits at the batch's end (its Sync)
// whatever the client reads of it afterwards. So every statement commits or none does, and no
// callback can undo the batch once it is sent: before anything is sent, it refuses a batch that
// queued a QueryRow, a Query or an Exec callback, whose error would read as a refusal of a write
// that has already committed. A statement's answer is read through Observe alone. It returns the
// first failure: a statement that failed, which leaves nothing of the batch committed, or an
// observed read that failed, which leaves all of it committed. It sends the batch whole, never in
// Send's parts, since each part would commit on its own, and builds the arguments of every
// statement queued with QueueBuilt before it sends any: a build that fails sends nothing. A
// refused batch keeps its statements, any other is empty again afterwards, and an empty batch
// sends nothing.
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
	queued, builds := b.batch.QueuedQueries, b.builds
	b.batch, b.builds, b.reads = pgx.Batch{}, nil, nil
	for index, build := range builds {
		if build == nil {
			continue
		}
		arguments, err := build()
		if err != nil {
			return err
		}
		queued[index].Arguments = arguments
	}
	batch := pgx.Batch{QueuedQueries: queued}
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
