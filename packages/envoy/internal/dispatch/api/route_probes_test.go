package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// TestRouteProbesNameEveryRowAndFillEveryGETWildcard holds api.RouteProbes, which the
// c-private-unseen driver walks, to the route table: one entry per row, test hooks included, in
// the table's order; each GET row with the Probe readClasses gives it, filling exactly the
// pattern's wildcards; and a row no table gives a Probe read as null in the JSON the driver reads,
// so the driver can tell it from a row that names no wildcard ({}).
func TestRouteProbesNameEveryRowAndFillEveryGETWildcard(t *testing.T) {
	t.Parallel()
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

// TestRouteProbesNameEachGETRowsReadClasses holds the JSON cmd/routeprobes prints, which the
// c-private-unseen driver reads, to readClasses: each GET row names its read classes in the
// table's order, and a write names none. The driver compares a GET row whose only class is none
// beside the twin, filled with PRIV's objects, rather than beside a missing object, since such a
// row answers every caller alike and its answer for PRIV's object can differ from its answer for
// an id nobody holds: the App-installation route, while it was of class none, answered
// installed:true for PRIV's repository and installed:false for a repository nobody holds, to
// whoever asked. The driver reads the class as the word none, so the JSON must spell it so.
func TestRouteProbesNameEachGETRowsReadClasses(t *testing.T) {
	t.Parallel()
	printed, err := json.Marshal(RouteProbes())
	if err != nil {
		t.Fatalf("encode RouteProbes: %v", err)
	}
	var rows []struct {
		Method  string    `json:"method"`
		Pattern string    `json:"pattern"`
		Classes *[]string `json:"classes"`
	}
	if err := json.Unmarshal(printed, &rows); err != nil {
		t.Fatalf("decode RouteProbes' JSON: %v", err)
	}
	classed, wrong := 0, []string{}
	for _, row := range rows {
		name := row.Method + " " + row.Pattern
		entry, read := readSources[name]
		var want []string
		for _, class := range entry.Classes {
			want = append(want, string(class))
		}
		switch {
		case read && row.Classes == nil:
			wrong = append(wrong, fmt.Sprintf("%s names no classes, readClasses %v", name, want))
		case read && !slices.Equal(*row.Classes, want):
			wrong = append(wrong, fmt.Sprintf("%s names classes %v, readClasses %v", name, *row.Classes, want))
		case !read && row.Classes != nil:
			wrong = append(wrong, fmt.Sprintf("%s, which readClasses does not class, names classes %v", name, *row.Classes))
		case read:
			classed++
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d of %d rows name their read classes otherwise than readClasses, the first %s",
			len(wrong), len(rows), strings.Join(wrong[:min(3, len(wrong))], "; "))
	}
	if classed == 0 {
		t.Fatalf("no row of %d names its read classes, so the rule above held of nothing", len(rows))
	}
	// The spelling the driver compares a row's classes to (project-access-walk.sh's walk_rows).
	for _, row := range rows {
		if row.Method == http.MethodGet && row.Pattern == "/api/v1/limits" {
			if row.Classes == nil || !slices.Equal(*row.Classes, []string{"none"}) || readclass.None != "none" {
				t.Errorf("GET %s names classes %v, want [none], the word the driver reads", row.Pattern, row.Classes)
			}
			return
		}
	}
	t.Fatalf("RouteProbes holds no GET /api/v1/limits, which the check above reads")
}
