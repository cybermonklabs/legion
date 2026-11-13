// Command connections prints store.ConnectionsPerProcess, the most connections one Dispatch
// server process opens to Postgres.
//
// It exists so that a CI lane sizes its Postgres from the code rather than from a number written
// in a workflow: a lane that starts N processes needs N times this, beside Postgres's reserved
// superuser slots (.github/scripts/pg-connection-ceiling.sh). The ceiling has climbed 105, 108
// and 112 as pools were added, and each climb left every literal elsewhere wrong and silent.
package main

import (
	"fmt"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func main() {
	fmt.Println(store.ConnectionsPerProcess)
}
