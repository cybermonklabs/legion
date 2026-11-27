// Command routeprobes prints api.RouteProbes as one JSON array: every row of Dispatch's route
// table with the Probe a driver fills its wildcards from, so the rows a driver walks and the
// objects it fills them with are the server's own tables, never a list in the driver.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sjawhar/envoy/internal/dispatch/api"
)

func main() {
	if err := json.NewEncoder(os.Stdout).Encode(api.RouteProbes()); err != nil {
		fmt.Fprintf(os.Stderr, "routeprobes: %v\n", err)
		os.Exit(1)
	}
}
