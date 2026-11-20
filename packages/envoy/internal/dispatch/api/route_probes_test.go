package api

import (
	"encoding/json"
	"maps"
	"net/http"
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
	for index, route := range table {
		probe := probes[index]
		name := route.Method + " " + route.Pattern
		if probe.Method != route.Method || probe.Pattern != route.Pattern {
			t.Fatalf("RouteProbes row %d is %s %s, the table's %s", index, probe.Method, probe.Pattern, name)
		}
		_, written := writeProbes[name]
		if route.Method != http.MethodGet && !written {
			continue
		}
		if probe.Probe == nil {
			t.Errorf("%s: no Probe, though readClasses classes every GET row and writeProbes names this one", name)
			continue
		}
		if written && string(probe.Body) != writeProbes[name].Body {
			t.Errorf("%s: the body is %s, writeProbes's %s", name, probe.Body, writeProbes[name].Body)
		}
		var want []string
		for _, match := range patternWildcard.FindAllStringSubmatch(route.Pattern, -1) {
			want = append(want, match[1])
		}
		got := slices.Sorted(maps.Keys(probe.Probe))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: the Probe fills %v, the pattern holds %v", name, got, want)
		}
	}
	spotRow := func(pattern string) RouteProbe {
		t.Helper()
		index := slices.IndexFunc(probes, func(row RouteProbe) bool { return row.Method == http.MethodGet && row.Pattern == pattern })
		if index < 0 {
			t.Fatalf("RouteProbes holds no GET %s, which the spot checks below read", pattern)
		}
		return probes[index]
	}
	issue := spotRow("/api/v1/issues/{key}")
	if issue.Probe["key"] != string(probeIssue) {
		t.Errorf("GET /api/v1/issues/{key}: the Probe fills {key} with %q, want %q", issue.Probe["key"], probeIssue)
	}
	if issue.TestHook {
		t.Errorf("GET /api/v1/issues/{key} is marked a test hook, though every deployment mounts it")
	}
	if !spotRow("/api/v1/artifacts/_test/rooms").TestHook {
		t.Errorf("GET /api/v1/artifacts/_test/rooms is not marked a test hook, though only DISPATCH_TEST_HOOKS mounts it")
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
