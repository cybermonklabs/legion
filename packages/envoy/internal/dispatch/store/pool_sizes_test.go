package store

import (
	"context"
	"strings"
	"testing"
)

// PoolSizes holds every pool ConnectionsPerProcess counts beyond the ones every process opens at
// the same size, the health probe's and the registry's pools of one connection each and the
// listener connections (listenerConnections): a pool added to the sum and not here would open at
// production's size in a test process whatever its budget.
func TestPoolSizesHoldEveryPoolConnectionsPerProcessCounts(t *testing.T) {
	if got := productionPoolSizes.total() + healthPoolSize + registryPoolSize + listenerConnections; got != ConnectionsPerProcess {
		t.Fatalf("production's PoolSizes and the unscaled pools hold %d connections, ConnectionsPerProcess %d", got, ConnectionsPerProcess)
	}
}

// A budget scales every pool between its minimum and its production size and uses the whole
// budget, the test hooks' listener counted; a budget that holds production's sizes gets exactly
// them; and one below the minimums is refused. A Postgres at its default of 100 connections gives
// each process 19 (testlimits.PoolBudget): every pool at its minimum, and the one connection left
// over to the shared pool, which gives up the most of production's size.
func TestScaledPoolSizesUseTheBudgetAndKeepEveryMinimum(t *testing.T) {
	production := ConnectionsPerTestProcess
	for budget := MinBudget(); budget <= production+10; budget++ {
		sizes, err := ScaledPoolSizes(budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		scaled, floor, ceiling := sizes.fields(), minimumPoolSizes.fields(), productionPoolSizes.fields()
		for i := range scaled {
			if *scaled[i] < *floor[i] || *scaled[i] > *ceiling[i] {
				t.Fatalf("budget %d: %+v holds a pool outside its minimum %+v and its production size %+v", budget, sizes, minimumPoolSizes, productionPoolSizes)
			}
		}
		switch opened := sizes.total() + unscaledConnections; {
		case budget >= production && sizes != productionPoolSizes:
			t.Fatalf("budget %d holds production's sizes and got %+v", budget, sizes)
		case budget < production && opened != budget:
			t.Fatalf("budget %d: %+v opens %d", budget, sizes, opened)
		}
	}

	if sizes, err := ScaledPoolSizes(19); err != nil || sizes != (PoolSizes{Shared: 2, Rooms: 1, RoomAppends: 1, Leases: 3, Outbox: 1, Compaction: 1, TextTrees: 1, Horizon: 1}) {
		t.Errorf("budget 19: %+v, %v; want every pool at its minimum and the shared pool at 2", sizes, err)
	}
	if _, err := ScaledPoolSizes(MinBudget() - 1); err == nil || !strings.Contains(err.Error(), "store.MinBudget") {
		t.Errorf("budget %d, one below MinBudget: err = %v, want the refusal naming store.MinBudget", MinBudget()-1, err)
	}
}

// OpenWithin opens the shared pool and every separate pool at the sizes its budget gives, and the
// lease pool's callers are still held to LeasePoolSize, the wiring a deployment runs, whatever
// connections the budget left them.
func TestOpenWithinSizesEveryPoolToItsBudget(t *testing.T) {
	const budget = 19
	want, err := ScaledPoolSizes(budget)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenWithin(context.Background(), testDatabaseURL(t), MinBudget()-1); err == nil {
		t.Errorf("OpenWithin a budget of %d, below MinBudget, opened", MinBudget()-1)
	}
	database, err := OpenWithin(context.Background(), testDatabaseURL(t), budget)
	if err != nil {
		t.Fatalf("open within a budget of %d: %v", budget, err)
	}
	t.Cleanup(database.Pool.Close)
	pool := database.Pool
	if pool.Sizes() != want {
		t.Errorf("Sizes = %+v, want %+v", pool.Sizes(), want)
	}

	rooms, err := pool.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	appends, err := pool.RoomAppends()
	if err != nil {
		t.Fatal(err)
	}
	callers, err := pool.Leases()
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := pool.Outbox()
	if err != nil {
		t.Fatal(err)
	}
	compaction, err := pool.Compaction()
	if err != nil {
		t.Fatal(err)
	}
	textTrees, err := pool.TextTrees()
	if err != nil {
		t.Fatal(err)
	}
	horizon, err := pool.Horizon()
	if err != nil {
		t.Fatal(err)
	}
	for name, sized := range map[string]struct{ got, want int32 }{
		"shared":       {pool.Config().MaxConns, want.Shared},
		"rooms":        {rooms.Config().MaxConns, want.Rooms},
		"room appends": {appends.Config().MaxConns, want.RoomAppends},
		"leases":       {callers.pool.Config().MaxConns, want.Leases},
		"outbox":       {outbox.Config().MaxConns, want.Outbox},
		"compaction":   {compaction.Config().MaxConns, want.Compaction},
		"text trees":   {textTrees.Config().MaxConns, want.TextTrees},
		"horizon":      {horizon.Config().MaxConns, want.Horizon},
	} {
		if sized.got != sized.want {
			t.Errorf("the %s pool holds at most %d connections, want %d", name, sized.got, sized.want)
		}
	}

	reserveLeaseCallers(t, callers, LeasePoolSize)
	if err := callers.Complete(); err != nil {
		t.Errorf("%d callers of a lease pool scaled to %d: %v, want the wiring check to pass", LeasePoolSize, want.Leases, err)
	}
}
