package store

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// largeArgumentStatement takes three arguments and reads the second, as a row's write takes its
// body among other columns. A connection that ran it with a large second argument still keeps that
// argument after a later statement of one argument (argumentReleaser), so a release that
// overwrote only one argument would leave it held.
const largeArgumentStatement = "select length($1::text) + length($2::text) + $3::int"

// A pooled connection keeps no statement's large arguments once it is back in its pool. pgx keeps
// a statement's encoded arguments on its connection until a later statement overwrites them
// (argumentReleaser), and an idle connection of the shared pool held its last large write for the
// half hour pgxpool keeps it. The pool is the shared pool's configuration at one connection, as
// Pool.separate opens its pools, so the statement runs on the connection the pool then holds idle,
// and the pool must keep that connection rather than drop the arguments by destroying it.
func TestAReleasedConnectionKeepsNoLargeArguments(t *testing.T) {
	ctx := context.Background()
	config := openTestStore(t).Pool.Config().Copy()
	config.MaxConns = 1
	opened, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open a one-connection pool: %v", err)
	}
	pool := NewPool(opened)
	t.Cleanup(pool.Close)
	// The connection is dialled and the statement prepared before the first measurement, so
	// neither counts as growth.
	if _, err := pool.Exec(ctx, largeArgumentStatement, "a", "b", 0); err != nil {
		t.Fatalf("run the statement with small arguments: %v", err)
	}
	made := pool.Stat().NewConnsCount()

	before := liveHeap()
	runWithLargeArgument(ctx, t, pool)
	after := liveHeap()

	if stat := pool.Stat(); stat.IdleConns() != 1 || stat.NewConnsCount() != made {
		t.Fatalf("after the release the pool holds %d idle connections and made %d new ones, want its one connection kept",
			stat.IdleConns(), stat.NewConnsCount()-made)
	}
	if grown := int64(after) - int64(before); grown > 2<<20 {
		t.Fatalf("the live heap is %.1f MiB above its level before the statement once its connection is back in the pool, want within 2 MiB",
			float64(grown)/(1<<20))
	}
}

// runWithLargeArgument runs largeArgumentStatement on pool with an 8 MiB second argument, and keeps
// no copy of that argument once it returns, so what stays live is what the connection keeps.
func runWithLargeArgument(ctx context.Context, t *testing.T, pool *Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, largeArgumentStatement, "a", strings.Repeat("x", 8<<20), 0); err != nil {
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
