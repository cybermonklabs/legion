package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A write phase past the send bound goes as several batches (partEnd): every statement once and
// in its order, each batch within the bound unless it is one statement past it alone, and a batch
// cut only where the next statement would take it past the bound.
func TestPartsKeepEveryStatementInOrderWithinTheBound(t *testing.T) {
	const bound = 8
	sizes := []int{3, 4, 2, 9, 1, 1, 12, 5, 8, 0}
	size := func(index int) (int, error) { return sizes[index], nil }
	sum := func(from, to int) int {
		total := 0
		for _, bytes := range sizes[from:to] {
			total += bytes
		}
		return total
	}

	var ends []int
	for start := 0; start < len(sizes); {
		end, err := partEnd(start, len(sizes), bound, size)
		if err != nil {
			t.Fatalf("cut the part from statement %d: %v", start, err)
		}
		if end <= start {
			t.Fatalf("the part from statement %d ends at %d, sending nothing", start, end)
		}
		if total := sum(start, end); total > bound && end-start > 1 {
			t.Errorf("statements %d to %d carry %d bytes, past the bound of %d", start, end-1, total, bound)
		}
		if end < len(sizes) && sum(start, end+1) <= bound {
			t.Errorf("the part from statement %d ends at %d although statement %d fits under the bound of %d", start, end, end, bound)
		}
		ends = append(ends, end)
		start = end
	}
	if len(ends) < 2 || ends[len(ends)-1] != len(sizes) {
		t.Fatalf("the parts end at %v, want the phase cut and every one of its %d statements sent", ends, len(sizes))
	}
}

// A statement whose arguments fail to build ends the part at itself, so Send sends the statements
// queued before it first and their own failure comes before the build's.
func TestAFailedBuildEndsThePartAtItsStatement(t *testing.T) {
	failed := errors.New("encode the arguments")
	size := func(index int) (int, error) {
		if index == 2 {
			return 0, failed
		}
		return 1, nil
	}
	if end, err := partEnd(0, 4, 8, size); end != 2 || !errors.Is(err, failed) {
		t.Fatalf("the part ends at %d with %v, want it to end at the failed statement 2 with its failure", end, err)
	}
	if end, err := partEnd(2, 4, 8, size); end != 2 || !errors.Is(err, failed) {
		t.Fatalf("a part starting at the failed statement ends at %d with %v, want 2 and its failure, so nothing of it is sent", end, err)
	}
}

// A write phase whose arguments pass the send bound goes to Postgres as several batches in a row
// (Send), and those batches together run every statement of the phase once and in the order it
// was queued, whether the statement was queued with its arguments or with a build of them: no
// statement is dropped or sent twice where one part ends and the next begins. Each statement's
// callback runs once, in that same order. The test's connection records each batch it sends
// (sentBatches), so the test sees the phase go in more than one part, and names the statement
// each later part begins with: the one after the last statement of the part before it.
//
// Control: change `start = end` in Send to `start = end + 1`, so each part after the first
// starts one statement late, and this fails, naming statements 3, 7 and 11, the first of each
// later part, as missing from the rows.
func TestAWritePhasePastTheSendBoundRunsEveryStatementInOrder(t *testing.T) {
	ctx := context.Background()
	config, err := pgx.ParseConfig(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse the test database's URL: %v", err)
	}
	sent := &sentBatches{}
	config.Tracer = sent
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the test database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `create temporary table parted_send (
		position bigserial primary key,
		statement int not null,
		bytes int not null
	)`); err != nil {
		t.Fatalf("create the table the write phase inserts into: %v", err)
	}

	// Each statement carries a payload a little over a quarter of the bound, so no part holds
	// more than three statements. Every statement shares the one payload string.
	const statements = 12
	payload := strings.Repeat("x", sendBound/4+1)

	// Each statement inserts a row naming its place in the phase, its first argument.
	const insert = `insert into parted_send (statement, bytes) values ($1, length($2::text))`
	var batch WriteBatch
	var ran []int
	for index := range statements {
		var queued Queued
		if index%2 == 0 {
			queued = batch.Queue("insert a statement's row", insert, index, payload)
		} else {
			queued = batch.QueueBuilt("insert a statement's row", insert, func() ([]any, error) {
				return []any{index, payload}, nil
			})
		}
		queued.Exec(func(pgconn.CommandTag) error {
			ran = append(ran, index)
			return nil
		})
	}
	if err := batch.Send(ctx, tx); err != nil {
		t.Fatalf("send the write phase: %v", err)
	}
	if len(sent.batches) < 2 {
		t.Fatalf("the write phase went as the batches %v, want more than one", sent.batches)
	}

	rows, err := tx.Query(ctx, `select statement, bytes from parted_send order by position`)
	if err != nil {
		t.Fatalf("read the rows the write phase inserted: %v", err)
	}
	defer rows.Close()
	var inserted []int
	for rows.Next() {
		var statement, bytes int
		if err := rows.Scan(&statement, &bytes); err != nil {
			t.Fatalf("scan a row the write phase inserted: %v", err)
		}
		if bytes != len(payload) {
			t.Errorf("statement %d inserted a payload of %d bytes, want %d", statement, bytes, len(payload))
		}
		inserted = append(inserted, statement)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the rows the write phase inserted: %v", err)
	}

	want := make([]int, statements)
	for index := range want {
		want[index] = index
	}
	if !slices.Equal(inserted, want) {
		t.Errorf("the table holds rows, in insert order, of statements %v, want %v, each once and in order", inserted, want)
	}
	if !slices.Equal(ran, want) {
		t.Errorf("the callbacks ran for statements %v, want %v, each once and in order", ran, want)
	}
	// A part ends where the next begins, so the statement after each batch's last begins a later
	// part, unless that batch ends the phase.
	var missing []int
	for _, carried := range sent.batches {
		if first := carried[len(carried)-1] + 1; first < statements && !slices.Contains(inserted, first) {
			missing = append(missing, first)
		}
	}
	if len(missing) > 0 {
		t.Errorf("statements %v begin the parts after the batches %v, but the table holds no row of them", missing, sent.batches)
	}
}

// sentBatches is a pgx tracer that records, for each batch its connection sends, the statements
// the batch carried, each by its first argument.
type sentBatches struct{ batches [][]int }

func (s *sentBatches) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	var carried []int
	for _, query := range data.Batch.QueuedQueries {
		statement, _ := query.Arguments[0].(int)
		carried = append(carried, statement)
	}
	s.batches = append(s.batches, carried)
	return ctx
}

func (*sentBatches) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (*sentBatches) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// TraceQueryStart and TraceQueryEnd make sentBatches a pgx.QueryTracer, the type of
// pgx.ConnConfig's Tracer, which pgx hands each batch when it is a pgx.BatchTracer too.
func (*sentBatches) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (*sentBatches) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
