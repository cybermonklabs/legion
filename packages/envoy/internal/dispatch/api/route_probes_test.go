package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestRouteProbesNameEveryRowAndFillEveryGETWildcard holds api.RouteProbes, which the
// c-private-unseen driver walks, to the route table: one entry per row, test hooks included, in
// the table's order; each GET row with the Probe readClasses gives it, filling exactly the
// pattern's wildcards; and a row no table gives a Probe read as null in the JSON the driver reads,
// so the driver can tell it from a row that names no wildcard ({}).
func TestRouteProbesNameEveryRowAndFillEveryGETWildcard(t *testing.T) {
	table := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	probes := RouteProbes()
	if len(probes) != len(table) {
		t.Fatalf("RouteProbes lists %d rows, the table %d", len(probes), len(table))
	}
	wildcard := regexp.MustCompile(`\{([^}]+)\}`)
	for index, route := range table {
		probe := probes[index]
		name := route.Method + " " + route.Pattern
		if probe.Method != route.Method || probe.Pattern != route.Pattern {
			t.Fatalf("RouteProbes row %d is %s %s, the table's %s", index, probe.Method, probe.Pattern, name)
		}
		if route.Method != http.MethodGet {
			continue
		}
		if probe.Probe == nil {
			t.Errorf("%s: no Probe, though readClasses classes every GET row", name)
			continue
		}
		var want []string
		for _, match := range wildcard.FindAllStringSubmatch(route.Pattern, -1) {
			want = append(want, match[1])
		}
		var got []string
		for filled := range probe.Probe {
			got = append(got, filled)
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the Probe fills %v, the pattern holds %v", name, got, want)
		}
	}
	for _, row := range probes {
		if row.Method == http.MethodGet && row.Pattern == "/api/v1/issues/{key}" && row.Probe["key"] != string(probeIssue) {
			t.Errorf("GET /api/v1/issues/{key}: the Probe fills {key} with %q, want %q", row.Probe["key"], probeIssue)
		}
	}

	none, err := json.Marshal(RouteProbe{Method: http.MethodPut, Pattern: "/api/v1/things/{id}"})
	if err != nil || !strings.Contains(string(none), `"probe":null`) {
		t.Errorf("a row with no Probe encodes as %s (%v), want \"probe\":null", none, err)
	}
	empty, err := json.Marshal(RouteProbe{Method: http.MethodGet, Pattern: "/api/v1/limits", Probe: probeMap(nil)})
	if err != nil || !strings.Contains(string(empty), `"probe":{}`) {
		t.Errorf("a row that names no wildcard encodes as %s (%v), want \"probe\":{}", empty, err)
	}
}
