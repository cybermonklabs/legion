package api

import "encoding/json"

// RouteProbe is one route-table row as e2e/acceptance/project-access.sh (c-private-unseen) probes
// it: the object each wildcard of its pattern is filled with, once with the private project's own
// object of that kind and once with an id of that kind nobody holds, and the body a write sends
// with both fills.
type RouteProbe struct {
	Method  string `json:"method"`
	Pattern string `json:"pattern"`
	// Probe maps each wildcard of Pattern to the kind of object that fills it (probeObjects), and
	// is empty for a row that names none. It is null for a row the tables give no Probe, which
	// the driver fails, naming the row, when its pattern names an object.
	Probe map[string]string `json:"probe"`
	// Body is the JSON a write sends with both fills; absent for a read and for a write that
	// sends none.
	Body json.RawMessage `json:"body,omitempty"`
}

// RouteProbes is every row of the route table, test hooks included, in table order, with its
// Probe: a GET row's from readClasses. A row whose Probe no table gives answers a null Probe.
// internal/dispatch/api/cmd/routeprobes prints it as JSON for the driver, which therefore reads
// the table the server mounts rather than a list of its own.
func RouteProbes() []RouteProbe {
	table := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	probes := make([]RouteProbe, len(table))
	for index, route := range table {
		row := RouteProbe{Method: route.Method, Pattern: route.Pattern}
		if entry, ok := readClasses[route.Method+" "+route.Pattern]; ok {
			row.Probe = probeMap(entry.Probe)
		}
		probes[index] = row
	}
	return probes
}

// probeMap is probe as RouteProbe spells it: never nil, so a row that names no wildcard reads {}
// and only a row with no Probe at all reads null.
func probeMap(probe readProbe) map[string]string {
	spelled := make(map[string]string, len(probe))
	for wildcard, object := range probe {
		spelled[wildcard] = string(object)
	}
	return spelled
}
