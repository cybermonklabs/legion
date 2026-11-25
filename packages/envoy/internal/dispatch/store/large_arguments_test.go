package store

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
// half hour pgxpool keeps it. The pool is the health probe's, which openSeparateLocked opens at
// one connection from a copy of the shared pool's configuration, as it opens every separate
// pool: the statement runs on the connection the pool then holds idle, the pool must keep that
// connection rather than drop the arguments by destroying it, and a copy that lost the shared
// pool's tracer fails here.
func TestAReleasedConnectionKeepsNoLargeArguments(t *testing.T) {
	for _, row := range largeArgumentRows {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			pool, err := openTestStore(t).Pool.healthPool()
			if err != nil {
				t.Fatalf("open the health probe's pool: %v", err)
			}
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

// runStatement runs statement with args on pool: on its own, or batched as the last query of a
// batch after a statement of one argument, so the batch's last build is the large one and nothing
// after it in the batch overwrites what it encoded.
func runStatement(ctx context.Context, pool *pgxpool.Pool, batched bool, statement string, args ...any) error {
	if !batched {
		_, err := pool.Exec(ctx, statement, args...)
		return err
	}
	var batch pgx.Batch
	batch.Queue("select $1::int", 1)
	batch.Queue(statement, args...)
	return pool.SendBatch(ctx, &batch).Close()
}

// runWithLargeArgument runs statement on pool with an 8 MiB text, passed as argument makes it, as
// its second argument, and keeps no copy of that argument once it returns, so what stays live is
// what the connection keeps.
func runWithLargeArgument(ctx context.Context, t *testing.T, pool *pgxpool.Pool, batched bool, statement string, argument func(text string) any) {
	t.Helper()
	if err := runStatement(ctx, pool, batched, statement, "a", argument(strings.Repeat("x", 8<<20)), 0); err != nil {
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
