package api

import (
	"encoding/json"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// RouteProbe is one route-table row as a driver that walks the table (cmd/routeprobes) probes
// it: the object each wildcard of its pattern is filled with, once with the private project's own
// object of that kind and once with an id of that kind nobody holds, and the body a write sends
// with both fills; or, for a row that reads no row a caller's access decides, the private
// project's own objects alone, read beside a server that never held the project.
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
	// TestHook is whether the row is a test hook: one the server mounts only under
	// DISPATCH_TEST_HOOKS, which no deployment sets, so the driver leaves it out, naming why.
	TestHook bool `json:"test_hook,omitempty"`
	// NoProject is why the object the row's path names belongs to no project (noProjectWrites),
	// and empty for every other row. Such a row has no Probe; the driver holds it to its class's
	// refusal of every caller that is not open, sending Body.
	NoProject string `json:"no_project,omitempty"`
	// Classes is a GET row's read classes, as readSources gives them, and absent for a write. A
	// row whose only class is none reads no row a caller's access decides, so it answers every
	// caller alike, and the answer for the private project's own object can rightly differ from
	// the answer for an id nobody holds: the driver fills it with the private project's objects
	// and holds each caller's answer to the answer of a server that never held the project.
	Classes []readclass.Class `json:"classes,omitempty"`
}

// RouteProbes is every row of the route table, test hooks included, in table order, with its
// Probe: a GET row's from readSources, a write row's and its body from writeProbes. A row whose
// Probe no table gives answers a null Probe. internal/dispatch/api/cmd/routeprobes prints it as
// JSON for the driver, which therefore reads the table the server mounts rather than a list of
// its own.
func RouteProbes() []RouteProbe {
	deployed := map[string]bool{}
	for _, route := range (&server{}).routes() {
		deployed[route.Method+" "+route.Pattern] = true
	}
	table := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	probes := make([]RouteProbe, len(table))
	for index, route := range table {
		name := route.Method + " " + route.Pattern
		row := RouteProbe{Method: route.Method, Pattern: route.Pattern, TestHook: !deployed[name]}
		if entry, ok := readSources[name]; ok {
			row.Probe = probeMap(entry.Probe)
			row.Classes = entry.Classes
		}
		if probe, ok := writeProbes[name]; ok {
			row.Probe = probeMap(probe.Probe)
			if probe.Body != "" {
				row.Body = json.RawMessage(probe.Body)
			}
		}
		if write, ok := noProjectWrites[name]; ok {
			row.NoProject = write.Reason
			if write.Body != "" {
				row.Body = json.RawMessage(write.Body)
			}
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
