package store

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// TestADrainRolledBackLeavesItsWorkAndTwoDrainsAtOnceApplyItOnce: after the install, the tester
// records a result of CORE-1 at 1111… and CORE-1's #70 moves from 3333… to 2222…, so CORE-1 holds
// one pending result and #70 one record, which this test reads before it drains anything. A drain
// of CORE-1 that rolls back leaves both. Two drains of CORE-1 at once, and a third after them,
// apply them once: 1111… a candidate of #70, unverified, and 3333…, the head #70 left, a former
// head, with no pending result and no record left.
func TestADrainRolledBackLeavesItsWorkAndTwoDrainsAtOnceApplyItOnce(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	documents := installFixture(t, store)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, installResult("check_results"), "CORE-1", "CORE-1", documents["CORE-1"], installH1); err != nil {
		t.Fatalf("record the result at 1111…: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update pull_request_heads set head_sha = $1 where number = 70`, installH2); err != nil {
		t.Fatalf("move #70 to 2222…: %v", err)
	}
	queued := func() (pending, moves int) {
		t.Helper()
		if err := store.Pool.QueryRow(ctx, `
			select (select count(*) from pull_request_former_heads_pending), (select count(*) from pull_request_former_heads_moves)`).Scan(&pending, &moves); err != nil {
			t.Fatalf("count the queued work: %v", err)
		}
		return pending, moves
	}
	pending, moves := queued()
	if pending == 0 || moves == 0 {
		t.Fatalf("the writes queued %d pending results and %d records, want both: this test holds nothing to drain", pending, moves)
	}
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the drain that rolls back: %v", err)
	}
	// A failed drain returns its connection, so the store's close at the test's end waits for none.
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pull_request_former_heads_drain('CORE-1')`); err != nil {
		t.Fatalf("the drain that rolls back: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the drain back: %v", err)
	}
	if afterPending, afterMoves := queued(); afterPending != pending || afterMoves != moves {
		t.Fatalf("after a drain rolled back %d pending results and %d records are queued, want %d and %d", afterPending, afterMoves, pending, moves)
	}
	var drains sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		drains.Add(1)
		go func() {
			defer drains.Done()
			_, err := store.Pool.Exec(ctx, `select pull_request_former_heads_drain('CORE-1')`)
			failures <- err
		}()
	}
	drains.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("a drain of two at once: %v", err)
		}
	}
	want := []string{"acme/widgets#70@1111 unverified", "acme/widgets#70@3333"}
	if heads, _ := formerHeadsAndOwed(t, store); !slices.Equal(heads, want) {
		t.Fatalf("after two drains at once the former heads are %v, want %v", heads, want)
	}
	drainCandidates(t, store, "CORE-1")
	if heads, _ := formerHeadsAndOwed(t, store); !slices.Equal(heads, want) {
		t.Fatalf("a drain after them changed the former heads to %v, want %v", heads, want)
	}
	if afterPending, afterMoves := queued(); afterPending != 0 || afterMoves != 0 {
		t.Fatalf("after the drains %d pending results and %d records are queued, want none", afterPending, afterMoves)
	}
}
