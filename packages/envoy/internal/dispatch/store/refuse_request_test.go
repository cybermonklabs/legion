package store

import (
	"context"
	"errors"
	"testing"
)

// A request refused (RefuseRequest) runs no statement more, and takes no connection for one: a
// call on the request pool, a call on a read split off it (ForConcurrentRead) before the refusal,
// a transaction's begin, and its commit (CommitRefusal) are each refused with the refusal as it was
// given, while the request's context goes on. The first refusal stands. A request with none runs
// its statements as before, and a context the pool cannot see is refused nothing.
func TestARefusedRequestRunsNoStatementMore(t *testing.T) {
	pool := smallRequestPool(t, 2, testAcquireBound)
	ctx := WithTransactionTracking(context.Background())
	split, err := ForConcurrentRead(ctx)
	if err != nil {
		t.Fatalf("split a read off the request: %v", err)
	}
	if _, err := pool.Exec(ctx, "select 1"); err != nil {
		t.Fatalf("a statement before the refusal: %v", err)
	}
	refusal := errors.New("the units the request asked back were refused")
	RefuseRequest(ctx, refusal)
	RefuseRequest(ctx, errors.New("a later refusal"))
	acquired := pool.Stat().AcquireCount()

	if _, err := pool.Exec(ctx, "select 1"); !errors.Is(err, refusal) {
		t.Errorf("a statement after the refusal: %v, want the refusal", err)
	}
	if err := pool.QueryRow(split, "select 1").Scan(new(int)); !errors.Is(err, refusal) {
		t.Errorf("a statement of a read split off the request: %v, want the refusal", err)
	}
	if tx, err := pool.Begin(ctx); !errors.Is(err, refusal) {
		if tx != nil {
			_ = tx.Rollback(context.Background())
		}
		t.Errorf("a transaction's begin after the refusal: %v, want the refusal", err)
	}
	if err := CommitRefusal(ctx); !errors.Is(err, refusal) {
		t.Errorf("a commit after the refusal: %v, want the refusal", err)
	}
	if got := pool.Stat().AcquireCount(); got != acquired {
		t.Errorf("the refused request took %d connections, want none", got-acquired)
	}
	if ctx.Err() != nil {
		t.Errorf("the refused request's context ended (%v), want it live", ctx.Err())
	}

	untracked := context.Background()
	RefuseRequest(untracked, refusal)
	if _, err := pool.Exec(untracked, "select 1"); err != nil {
		t.Errorf("a statement on a context the pool cannot see: %v, want it run", err)
	}
	if err := CommitRefusal(WithTransactionTracking(context.Background())); err != nil {
		t.Errorf("the commit of a request with no refusal: %v, want none", err)
	}
}
