// Command connections prints testlimits.FullSizeServerConnections: the connections a Dispatch test
// lane's Postgres must allow beyond its superuser reserve (superuser_reserved_connections) for every
// Dispatch process its tests start to run at production's pool sizes, each counted with the test
// hooks' listener a process holds under DISPATCH_TEST_HOOKS=1. The memory harness starts its
// processes without the hooks, so for them the number is an upper bound.
//
// .github/scripts/pg-connection-ceiling.sh runs it to check a CI lane's server against the code,
// and adds only that server's own superuser reserve, so no workflow holds the number. A lane so
// sized tests the pools production runs; a smaller server would have the tests scale them
// (testlimits.PoolBudget).
//
// With -process it prints store.ConnectionsPerProcess instead: the most connections one Dispatch
// server process opens. packages/pi-legion/scripts/skill-scenarios/rig.sh sizes each concurrent
// run's share of its batch's Postgres by it, so that script holds no number either.
package main

import (
	"flag"
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

func main() {
	process := flag.Bool("process", false, "print store.ConnectionsPerProcess, the most connections one Dispatch server process opens")
	flag.Parse()
	if *process {
		fmt.Println(store.ConnectionsPerProcess)
		return
	}
	fmt.Println(testlimits.FullSizeServerConnections)
}
