package store

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// largeArgumentRows is each way a store write passes a large argument, as a statement that takes
// three arguments and reads the second, as a row's write takes its body among other columns. A
// connection that ran one with a large second argument still keeps that argument after a later
// statement of one argument (argumentReleaser), so a release that overwrote only one argument
// would leave it held. The block-text writes bind their texts as a slice of pointers to strings,
// a NULL where a block has no text, and queue that statement in a write phase's batch
// (refs/blocks.go, WriteBatch.Send), which TraceBatchStart marks rather than TraceQueryStart.
var largeArgumentRows = []struct {
	name      string
	statement string
	argument  func(text string) any
	batched   bool
}{
	{"a string", "select length($1::text) + length($2::text) + $3::int", func(text string) any { return text }, false},
	{"a slice of pointers to strings", "select length($1::text) + cardinality($2::text[]) + $3::int",
		func(text string) any { return []*string{&text, nil} }, false},
	{"a slice of pointers to strings, last in a batch", "select length($1::text) + cardinality($2::text[]) + $3::int",
		func(text string) any { return []*string{&text, nil} }, true},
}

// A pooled connection keeps no statement's large arguments once it is back in its pool. pgx keeps
// a statement's encoded arguments on its connection until a later statement overwrites them
// (argumentReleaser), and an idle connection of the shared pool held its last large write for the
// half hour pgxpool keeps it. The pool is the health probe's, which newSeparate opens at one
// connection from a copy of the shared pool's configuration, as it opens every separate
// pool: the statement runs on the connection the pool then holds idle, the pool must keep that
// connection rather than drop the arguments by destroying it, and a copy that lost the shared
// pool's tracer fails here.
func TestAReleasedConnectionKeepsNoLargeArguments(t *testing.T) {
	for _, row := range largeArgumentRows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			health, err := openTestStore(t).Pool.healthPool()
			if err != nil {
				t.Fatalf("open the health probe's pool: %v", err)
			}
			pool := health.Pool
			if size := pool.Config().MaxConns; size != 1 {
				t.Fatalf("the health probe's pool holds %d connections, want the one this test reads", size)
			}
			// The connection is dialled and the statement prepared before the first
			// measurement, so neither counts as growth.
			if err := runStatement(ctx, pool, row.batched, row.statement, "a", row.argument("b"), 0); err != nil {
				t.Fatalf("run the statement with small arguments: %v", err)
			}
			made := pool.Stat().NewConnsCount()

			before := liveHeap()
			runWithLargeArgument(ctx, t, pool, row.batched, row.statement, row.argument)
			after := liveHeap()

			if stat := pool.Stat(); stat.IdleConns() != 1 || stat.NewConnsCount() != made {
				t.Fatalf("after the release the pool holds %d idle connections and made %d new ones, want its one connection kept",
					stat.IdleConns(), stat.NewConnsCount()-made)
			}
			if grown := int64(after) - int64(before); grown > 2<<20 {
				t.Fatalf("the live heap is %.1f MiB above its level before the statement once its connection is back in the pool, want within 2 MiB",
					float64(grown)/(1<<20))
			}
		})
	}
}

// largeArgumentCallers is each way a caller takes the connection a large write runs on, through
// the store.Pool wrapper it calls: on the request pool, a statement of its own, a write's
// transaction (Pool.Begin), a read pinned to one connection, whose connection goes back only at
// the pin's end (PinConnection), and one of a pinned read's side-by-side reads
// (ForConcurrentRead); on the background pool, a statement of its own and a transaction. pool is
// the pool the caller runs on; start is the caller's context, what the statement runs on, and the
// end that gives the connection back.
var largeArgumentCallers = []struct {
	name  string
	pool  func(t *testing.T, database *Store) *Pool
	start func(ctx context.Context, t *testing.T, pool *Pool) (context.Context, statementRunner, func())
}{
	{"the request pool, a statement of its own", requestPoolOf,
		func(ctx context.Context, _ *testing.T, pool *Pool) (context.Context, statementRunner, func()) {
			return WithTransactionTracking(ctx), pool, func() {}
		}},
	{"the request pool, a write's transaction", requestPoolOf, inTransaction},
	{"the request pool, a pinned read", requestPoolOf,
		func(ctx context.Context, _ *testing.T, pool *Pool) (context.Context, statementRunner, func()) {
			pinned, end := PinConnection(ctx)
			return pinned, pool, end
		}},
	{"the request pool, a pinned read's concurrent read", requestPoolOf,
		func(ctx context.Context, t *testing.T, pool *Pool) (context.Context, statementRunner, func()) {
			pinned, end := PinConnection(ctx)
			read, err := ForConcurrentRead(pinned)
			if err != nil {
				t.Fatalf("split a concurrent read off the pinned read: %v", err)
			}
			return read, pool, end
		}},
	{"the background pool, a statement of its own", backgroundPoolOf,
		func(ctx context.Context, _ *testing.T, pool *Pool) (context.Context, statementRunner, func()) {
			return WithTransactionTracking(ctx), pool, func() {}
		}},
	{"the background pool, a transaction", backgroundPoolOf, inTransaction},
}

// requestPoolOf is database's request pool, the one Open configures.
func requestPoolOf(_ *testing.T, database *Store) *Pool { return database.Pool }

// backgroundPoolOf is database's background pool, which newSeparate opens from a copy of the
// request pool's configuration, as it opens every pool this package adds.
func backgroundPoolOf(t *testing.T, database *Store) *Pool {
	background, err := database.Pool.Background()
	if err != nil {
		t.Fatalf("open the background pool: %v", err)
	}
	return background
}

// inTransaction starts a transaction on pool, which its end commits.
func inTransaction(ctx context.Context, t *testing.T, pool *Pool) (context.Context, statementRunner, func()) {
	tracked := WithTransactionTracking(ctx)
	tx, err := pool.Begin(tracked)
	if err != nil {
		t.Fatalf("begin a transaction: %v", err)
	}
	return tracked, tx, func() {
		if err := tx.Commit(tracked); err != nil {
			t.Fatalf("commit the transaction: %v", err)
		}
	}
}

// The release holds on the request pool and on every pool this package adds, not only on the
// health probe's above. The request pool is Open's configuration, its acquire tracer composed over
// the release (configurePool keeps it as acquireTracer.inner), and every separate pool configures
// a copy of that configuration again (newSeparate), the background pool among them: a
// configurePool that dropped the tracer it composed with, or a pool whose copy lost it, would keep
// a large write's arguments on that pool alone. Each caller runs the statement through the wrapper
// its code calls (largeArgumentCallers) and hands its connection back before the heap is read, and
// the pool must keep that connection rather than drop the arguments by destroying it.
func TestTheRequestAndBackgroundPoolsKeepNoLargeArguments(t *testing.T) {
	database := openTestStore(t)
	for _, caller := range largeArgumentCallers {
		for _, row := range largeArgumentRows {
			t.Run(caller.name+"/"+row.name, func(t *testing.T) {
				ctx := context.Background()
				pool := caller.pool(t, database)
				call := func(run func(ctx context.Context, on statementRunner)) {
					callCtx, on, end := caller.start(ctx, t, pool)
					run(callCtx, on)
					end()
				}
				// The connection is dialled and the statement prepared before the first
				// measurement, so neither counts as growth.
				call(func(ctx context.Context, on statementRunner) {
					if err := runStatement(ctx, on, row.batched, row.statement, "a", row.argument("b"), 0); err != nil {
						t.Fatalf("run the statement with small arguments: %v", err)
					}
				})
				stat := pool.Stat()
				made, total := stat.NewConnsCount(), stat.TotalConns()

				before := liveHeap()
				call(func(ctx context.Context, on statementRunner) {
					runWithLargeArgument(ctx, t, on, row.batched, row.statement, row.argument)
				})
				after := liveHeap()

				if stat := pool.Stat(); stat.NewConnsCount() != made || stat.TotalConns() != total {
					t.Fatalf("after the release the pool holds %d connections, had %d, and made %d new ones, want every connection kept",
						stat.TotalConns(), total, stat.NewConnsCount()-made)
				}
				if grown := int64(after) - int64(before); grown > 2<<20 {
					t.Fatalf("the live heap is %.1f MiB above its level before the statement once its connection is back in the pool, want within 2 MiB",
						float64(grown)/(1<<20))
				}
			})
		}
	}
}

// statementRunner is what runStatement runs a statement on: a pgx pool, a store.Pool wrapper or a
// transaction.
type statementRunner interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults
}

// runStatement runs statement with args on on: on its own, or batched as the last query of a
// batch after a statement of one argument, so the batch's last build is the large one and nothing
// after it in the batch overwrites what it encoded.
func runStatement(ctx context.Context, on statementRunner, batched bool, statement string, args ...any) error {
	if !batched {
		_, err := on.Exec(ctx, statement, args...)
		return err
	}
	var batch pgx.Batch
	batch.Queue("select $1::int", 1)
	batch.Queue(statement, args...)
	return on.SendBatch(ctx, &batch).Close()
}

// runWithLargeArgument runs statement on on with an 8 MiB text, passed as argument makes it, as
// its second argument, and keeps no copy of that argument once it returns, so what stays live is
// what the connection keeps.
func runWithLargeArgument(ctx context.Context, t *testing.T, on statementRunner, batched bool, statement string, argument func(text string) any) {
	t.Helper()
	if err := runStatement(ctx, on, batched, statement, "a", argument(strings.Repeat("x", 8<<20)), 0); err != nil {
		t.Fatalf("run the statement with an 8 MiB argument: %v", err)
	}
}

// liveHeap is the bytes of heap still live after two collections.
func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}
