package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

// beginThrough is a forwarder the compiler keeps as one function, as it keeps intake.ApplyFact
// and the daemon's other forwarders that are too large to inline: every caller's refusal passes
// through the same program counters here and in pgx.BeginFunc beneath it.
//
//go:noinline
func beginThrough(ctx context.Context, pool *Pool) error {
	return pgx.BeginFunc(ctx, pool, func(pgx.Tx) error { return nil })
}

//go:noinline
func askThroughFromA(ctx context.Context, pool *Pool) error { return beginThrough(ctx, pool) }

//go:noinline
func askThroughFromB(ctx context.Context, pool *Pool) error { return beginThrough(ctx, pool) }

// Two callers refused through one forwarder that is not inlined each log a line of their own. The
// frames nearest the guard — the guarded method, pgx.BeginFunc, the forwarder — are the same
// program counters on both paths, so only the frames beyond them tell the callers apart, and a log
// keyed on the few frames nearest the pool would let the first caller's refusal silence the
// second's. TestEachRefusingSiteLogsItsOwnLine passes under such a key, because the compiler
// inlines its forwarder into each of its two callers and so gives each copy a counter of its own;
// a forwarder the compiler keeps whole is the daemon's case. A caller that asks again logs nothing
// more.
func TestRefusalsThroughAForwarderTheCompilerKeepsWholeLogOneLineEach(t *testing.T) {
	store := storeWith(t, map[string]string{"pool_max_conns": "1"})
	pool := store.Pool()
	logged := captureLog(t)

	ctx := WithTransactionTracking(context.Background())
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "select 1"); err != nil {
			return err
		}
		asking, cancel := context.WithTimeout(ctx, refusalBound)
		defer cancel()
		for range 2 {
			if err := askThroughFromA(asking, pool); !errors.Is(err, ErrNestedAcquire) {
				t.Fatalf("caller A's transaction through the forwarder = %v, want %v", err, ErrNestedAcquire)
			}
			refusalsLogged(t, logged, 1)
		}
		if err := askThroughFromB(asking, pool); !errors.Is(err, ErrNestedAcquire) {
			t.Fatalf("caller B's transaction through the forwarder = %v, want %v", err, ErrNestedAcquire)
		}
		refusalsLogged(t, logged, 2)
		return nil
	}); err != nil {
		t.Fatalf("the holding transaction: %v", err)
	}
}
