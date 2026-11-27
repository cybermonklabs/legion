package testlimits

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A server that allows FullSizeServerConnections beyond its reserve gives no budget, so every
// process runs production's pool sizes; a smaller one gives each of MaxProcessesAtOnce processes its
// share beside SuiteHeadroom, rounded down; and one whose share is below the smallest pools is
// refused, naming the max_connections it needs. A Postgres at its default of 100, with the default
// reserve of 3, would leave each process (100 - 3 - 20) / 8 = 9, below store.MinBudget, so it is
// refused too.
func TestPoolBudgetIsEachProcessesShareOfTheServer(t *testing.T) {
	const reserve = postgresSuperuserReserve
	for _, row := range []struct {
		name           string
		maxConnections int
		want           int
	}{
		{"a server sized for production's pools", FullSizeServerConnections + reserve, 0},
		{"a larger one", FullSizeServerConnections + reserve + 100, 0},
		{"one connection short of production's pools", FullSizeServerConnections + reserve - 1, (FullSizeServerConnections - 1 - SuiteHeadroom) / MaxProcessesAtOnce},
		{"the smallest server the suite runs on", MinServerConnections() + reserve, store.MinBudget()},
	} {
		got, err := PoolBudget(row.maxConnections, reserve)
		if err != nil || got != row.want {
			t.Errorf("%s (max_connections=%d): PoolBudget = %d, %v; want %d", row.name, row.maxConnections, got, err, row.want)
		}
	}

	smallest := MinServerConnections() + reserve
	// 100 is Postgres's default max_connections.
	for _, maxConnections := range []int{smallest - 1, 100, 50, 0} {
		_, err := PoolBudget(maxConnections, reserve)
		if err == nil || !strings.Contains(err.Error(), "-c max_connections="+strconv.Itoa(smallest)+" ") {
			t.Errorf("max_connections=%d: PoolBudget's error = %v, want a refusal naming max_connections=%d", maxConnections, err, smallest)
		}
	}
}

// Every budget PoolBudget gives fits the server it was read from: MaxProcessesAtOnce processes at
// that budget - or at store.ConnectionsPerTestProcess, production's sizes and the test hooks'
// listener, where the budget is zero - the suite's own SuiteHeadroom and the superuser reserve
// together are never more than the server allows, and each process can scale its pools to a
// budget it is given (store.ScaledPoolSizes).
func TestEveryPoolBudgetFitsItsServer(t *testing.T) {
	for _, reserve := range []int{0, postgresSuperuserReserve, 10} {
		for maxConnections := MinServerConnections() + reserve; maxConnections <= FullSizeServerConnections+reserve; maxConnections++ {
			budget, err := PoolBudget(maxConnections, reserve)
			if err != nil {
				t.Fatalf("max_connections=%d, reserve %d: %v", maxConnections, reserve, err)
			}
			if opened := MaxProcessesAtOnce*processConnections(budget) + SuiteHeadroom + reserve; opened > maxConnections {
				t.Fatalf("max_connections=%d, reserve %d: a budget of %d opens %d with the suite and the reserve",
					maxConnections, reserve, budget, opened)
			}
			if budget == 0 {
				continue
			}
			// PoolBudget refuses a share below store.MinBudget, the least ScaledPoolSizes takes, so
			// this holds while the two agree on that floor; it is here to fail if they drift apart.
			if _, err := store.ScaledPoolSizes(budget); err != nil {
				t.Fatalf("max_connections=%d, reserve %d: a budget of %d: %v", maxConnections, reserve, budget, err)
			}
		}
	}
}

// processConnections is the most connections one Dispatch process a test starts opens under
// budget, a PoolBudget answer: the budget itself, or, for zero, production's sizes and the test
// hooks' listener (store.ConnectionsPerTestProcess).
func processConnections(budget int) int {
	if budget == 0 {
		return store.ConnectionsPerTestProcess
	}
	return budget
}

// A pod's test server - legion-testpg's max_connections, ServerConnections plus Postgres's
// default superuser reserve of 3 - is one connection per process short of production's sizes, the
// test hooks' listener counted, so PoolBudget scales each process there by that one connection:
// MaxProcessesAtOnce processes at the budget it gives, beside SuiteHeadroom and the reserve, fit
// the server. A CI lane's server, at the size .github/scripts/pg-connection-ceiling.sh computes
// (FullSizeServerConnections, which internal/dispatch/store/cmd/connections prints, plus the
// server's reserve), runs every process at production's sizes: PoolBudget answers 0 there.
//
// A threshold that did not count the hooks' listener would answer 0 at 983, and eight processes
// at production's sizes, 121 connections each with that listener, and the suite's 20 would need
// 988 of the 980 the server leaves beside its reserve.
func TestAPodsTestServerScalesEachProcessByTheHooksListener(t *testing.T) {
	const reserve = postgresSuperuserReserve
	pod := ServerConnections + reserve
	budget, err := PoolBudget(pod, reserve)
	if err != nil {
		t.Fatalf("max_connections=%d, reserve %d: %v", pod, reserve, err)
	}
	if opened := MaxProcessesAtOnce*processConnections(budget) + SuiteHeadroom + reserve; opened > pod {
		t.Fatalf("max_connections=%d, reserve %d: PoolBudget = %d, so %d processes open %d each, and with the suite's %d and the reserve that is %d, over the server's %d",
			pod, reserve, budget, MaxProcessesAtOnce, processConnections(budget), SuiteHeadroom, opened, pod)
	}
	if want := store.ConnectionsPerTestProcess - 1; budget != want {
		t.Errorf("max_connections=%d, reserve %d: PoolBudget = %d, want %d, production's sizes and the test hooks' listener less one connection",
			pod, reserve, budget, want)
	}
	ci := FullSizeServerConnections + reserve
	if budget, err := PoolBudget(ci, reserve); err != nil || budget != 0 {
		t.Errorf("max_connections=%d, the CI lanes' size with reserve %d: PoolBudget = %d, %v; want 0, production's pool sizes", ci, reserve, budget, err)
	}
}
