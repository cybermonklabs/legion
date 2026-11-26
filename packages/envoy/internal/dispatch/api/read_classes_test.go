package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// TestEveryGETRouteHasAReadClass (c-read-class): every GET row of the route table the server
// mounts, test hooks included, has a read class, and every entry of readClasses names a GET row
// it serves. The walk is undecidedReads, the one register refuses to mount a table over, so this
// test and the server's start refuse the same set. Each entry also says why its rows are of
// those classes, and its Probe names the seeded object a driver fills each wildcard of the
// pattern with, exactly the pattern's wildcards.
//
// The sub-tests are the walk's negative controls: a GET row with no entry, and an entry that
// names no row, each fail it, named.
func TestEveryGETRouteHasAReadClass(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	if undecided := undecidedReads(routes, readClasses); len(undecided) > 0 {
		t.Fatalf("%d GET row(s) or entries the read check cannot decide:\n%s", len(undecided), strings.Join(undecided, "\n"))
	}

	hooks := testHookRows()
	getRows := 0
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		if route.Method != http.MethodGet {
			if len(route.Reads) > 0 {
				t.Errorf("%s writes, and carries the read classes %q, which only a GET row takes", name, route.Reads)
			}
			continue
		}
		getRows++
		entry := readClasses[name]
		if !slices.Equal(route.Reads, entry.Classes) {
			t.Errorf("%s carries the read classes %q, and readClasses gives it %q", name, route.Reads, entry.Classes)
		}
		for _, problem := range entry.problems(route.Pattern, hooks[name]) {
			t.Errorf("%s: %s", name, problem)
		}
	}
	// A walk that read no GET row read nothing, which is no pass.
	if getRows == 0 {
		t.Fatal("the route table holds no GET row, so no read class was checked")
	}

	t.Run("a GET row with no read class fails the walk", func(t *testing.T) {
		undecided := undecidedReads([]apiRoute{
			{routeRow: routeRow{Method: http.MethodPost, Pattern: "/api/v1/widgets", Auth: authAny}},
			{routeRow: routeRow{Method: http.MethodGet, Pattern: "/api/v1/widgets", Auth: authAny}},
		}, readClasses)
		if len(undecided) != 1 || !strings.HasPrefix(undecided[0], "GET /api/v1/widgets:") {
			t.Fatalf("the walk answered %q, want the unclassed GET named and the POST left alone", undecided)
		}
	})

	t.Run("an entry that names no row fails it too", func(t *testing.T) {
		classes := maps.Clone(readClasses)
		classes["GET /api/v1/widgets"] = reads("widgets", nil, readclass.Project)
		undecided := undecidedReads(routes, classes)
		if len(undecided) != 1 || !strings.HasPrefix(undecided[0], "GET /api/v1/widgets:") {
			t.Fatalf("the walk answered %q, want the entry that names no row named", undecided)
		}
	})

	t.Run("the server's start refuses the same set", func(t *testing.T) {
		saved := readClasses
		t.Cleanup(func() { readClasses = saved })
		readClasses = maps.Clone(saved)
		delete(readClasses, "GET /api/v1/issues/{key}")
		defer func() {
			refusal, _ := recover().(string)
			if !strings.Contains(refusal, "GET /api/v1/issues/{key}: no read class") {
				t.Fatalf("register over a table with an unclassed GET row answered %q, want a panic naming it", refusal)
			}
		}()
		register(http.NewServeMux(), t.Context(), Deps{})
	})
}

// testHookRows names every row the table holds only for a deployment that turns its test hooks on.
func testHookRows() map[string]bool {
	served := map[string]bool{}
	for _, route := range (&server{}).routes() {
		served[route.Method+" "+route.Pattern] = true
	}
	hooks := map[string]bool{}
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		if name := route.Method + " " + route.Pattern; !served[name] {
			hooks[name] = true
		}
	}
	return hooks
}

// problems names what is wrong with one entry of the GET row at pattern: no class, a class twice,
// none or testHook beside another class, testHook on a row that is no test hook or a test hook
// without it, no reason, and a Probe that does not fill exactly the pattern's wildcards with known
// objects.
func (e readEntry) problems(pattern string, testHook bool) []string {
	var problems []string
	if len(e.Classes) == 0 {
		problems = append(problems, "no read class")
	}
	seen := map[readclass.Class]bool{}
	for _, class := range e.Classes {
		if seen[class] {
			problems = append(problems, fmt.Sprintf("the read class %q twice", class))
		}
		seen[class] = true
		if !slices.Contains(readclass.Classes, class) {
			problems = append(problems, fmt.Sprintf("the read class %q, which is none of readclass.Classes", class))
		}
	}
	for _, alone := range []readclass.Class{readclass.None, readclass.TestHook} {
		if seen[alone] && len(e.Classes) > 1 {
			problems = append(problems, fmt.Sprintf("%q beside other classes %q: it is a read of nothing else", alone, e.Classes))
		}
	}
	if seen[readclass.TestHook] != testHook {
		problems = append(problems, fmt.Sprintf("testHook is the class of exactly the test hooks' rows; this row is a test hook: %t", testHook))
	}
	if strings.TrimSpace(e.Reason) == "" {
		problems = append(problems, "no Reason")
	}
	wildcards := map[string]bool{}
	for _, match := range patternWildcard.FindAllStringSubmatch(pattern, -1) {
		wildcards[match[1]] = true
		if _, filled := e.Probe[match[1]]; !filled {
			problems = append(problems, fmt.Sprintf("a Probe that does not fill the wildcard {%s}", match[1]))
		}
	}
	for wildcard, object := range e.Probe {
		if !wildcards[wildcard] {
			problems = append(problems, fmt.Sprintf("a Probe that fills {%s}, which the pattern does not hold", wildcard))
		}
		if !slices.Contains(probeObjects, object) {
			problems = append(problems, fmt.Sprintf("a Probe that fills {%s} with %q, which is none of probeObjects", wildcard, object))
		}
	}
	slices.Sort(problems)
	return problems
}
