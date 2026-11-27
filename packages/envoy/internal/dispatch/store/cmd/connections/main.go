// Command connections prints testlimits.ServerConnections: the connections a Dispatch test lane's
// Postgres must allow beyond its superuser reserve (superuser_reserved_connections).
//
// .github/scripts/pg-connection-ceiling.sh runs it to check a CI lane's server against the code,
// and adds only that server's own superuser reserve, so no workflow holds the number.
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
	fmt.Println(testlimits.ServerConnections)
}
