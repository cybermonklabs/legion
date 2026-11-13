// Command connections prints what a Dispatch test lane's Postgres must allow: the connections
// the most processes any one test starts may open together (testlimits.RequiredConnections,
// store.ConnectionsPerProcess per process).
//
// It exists so that a CI lane sizes its server from the code rather than from a number written in
// a workflow (.github/scripts/pg-connection-ceiling.sh). The per-process ceiling has climbed 105,
// 108 and 112 as pools were added, and each climb left every literal elsewhere wrong and silent.
package main

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/testlimits"
)

func main() {
	fmt.Println(testlimits.RequiredConnections)
}
