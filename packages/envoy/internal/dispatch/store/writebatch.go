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
type WriteBatch struct {
	batch pgx.Batch
	// builds holds, beside each statement queued, the function that builds its arguments when
	// Send reaches it (QueueBuilt), and nil for a statement queued with its arguments.
	builds []func() ([]any, error)
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
}

// Queue adds sql with args after every statement queued so far. A failure of the statement itself
// comes back from Send wrapped as what failed ("what: err"); an empty what returns it unchanged,
// as a statement run on its own would.
func (b *WriteBatch) Queue(what, sql string, args ...any) Queued {
	queued := Queued{query: b.batch.Queue(sql, args...), what: what}
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

// failed is err, the statement's own failure, wrapped as what failed.
func (q Queued) failed(err error) error {
	if err == nil || q.what == "" {
		return err
	}
	return fmt.Errorf("%s: %w", q.what, err)
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

// Send sends every queued statement to tx and reads their results in order, running each
// statement's callback as it reads its result. It returns the first failure: the first statement
// that failed (the server runs none after it) or the first callback that returned an error (whose
// later statements ran or were never sent, and are rolled back with the transaction the caller now
// abandons), whichever comes first in the batch. The batch is empty again afterwards. An empty
// batch sends nothing.
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
	queued, builds := b.batch.QueuedQueries, b.builds
	b.batch, b.builds = pgx.Batch{}, nil
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
	switch value := argument.(type) {
	case nil:
		return 0
	case string:
		return len(value)
	case []byte:
		return len(value)
	case []string:
		bytes := 0
		for _, element := range value {
			bytes += elementLengthBytes + len(element)
		}
		return bytes
	case [][]byte:
		bytes := 0
		for _, element := range value {
			bytes += elementLengthBytes + len(element)
		}
		return bytes
	}
	return valueBytes(reflect.ValueOf(argument))
}

// elementLengthBytes is the length the wire carries before each element of an array.
const elementLengthBytes = 4

// valueBytes is argumentBytes of a value of a type its switch does not name.
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
