package api

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

// v1OnlyRoutesList is the frozen list of the routes /api/v1 alone serves (its header says what it
// holds and how a row is added to it).
const v1OnlyRoutesList = "testdata/v1-only-routes.txt"

// v2RouteBound is R2's bound: the /api/v2 routes that answer the 184 rows of c8a44f97 the parent
// maps - every row of its routes.txt but the INFRA-979 tree's five - number at most 137.
const v2RouteBound = 137

// readV1OnlyRoutes reads the frozen list: by "<METHOD> <path>", whether the row is one other work
// added after c8a44f97, a line ending in a ` # ` note naming the pull request that brought it.
func readV1OnlyRoutes(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(v1OnlyRoutesList)
	if err != nil {
		t.Fatal(err)
	}
	frozen := map[string]bool{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, note, later := strings.Cut(line, " # ")
		if later && strings.TrimSpace(note) == "" {
			t.Fatalf("%s: %q ends in a note that names nothing", v1OnlyRoutesList, line)
		}
		if _, twice := frozen[name]; twice {
			t.Fatalf("%s lists %s twice", v1OnlyRoutesList, name)
		}
		frozen[name] = later
	}
	return frozen
}

// v1OnlyRowsOutside names every row of rows - the routes /api/v1 alone serves - that the frozen
// list does not hold, and every line of the list that is no such row, sorted.
func v1OnlyRowsOutside(rows []v1Row, frozen map[string]bool) []string {
	var outside []string
	held := map[string]bool{}
	for _, row := range rows {
		name := row.Method + " " + row.Pattern
		held[name] = true
		if _, listed := frozen[name]; !listed {
			outside = append(outside, name+": /api/v1 alone serves it, and "+v1OnlyRoutesList+" does not hold it; write it as a routeRow of its group's v2_<group>.go, which both versions serve, or /api/v2 alone (Versions: onlyV2) where it declares a Query or a Body")
		}
	}
	for name := range frozen {
		if !held[name] {
			outside = append(outside, name+": "+v1OnlyRoutesList+" holds it, and it is no row of v1Rows")
		}
	}
	slices.Sort(outside)
	return outside
}

// successorRoute is the /api/v2 route a successor names, by method and version-free pattern.
func successorRoute(next successor) string {
	return next.Method + " " + next.Pattern
}

// successorsOfC8a44f97 is the distinct /api/v2 routes the rows of c8a44f97 among rows name: the
// rows the frozen list holds with no note.
func successorsOfC8a44f97(rows []v1Row, frozen map[string]bool) map[string]bool {
	named := map[string]bool{}
	for _, row := range rows {
		if later, listed := frozen[row.Method+" "+row.Pattern]; listed && !later {
			named[successorRoute(row.Successor)] = true
		}
	}
	return named
}

// successorBoundFault is R2's bound over rows: an error naming the count when the rows of
// c8a44f97 name more than v2RouteBound distinct /api/v2 routes.
func successorBoundFault(rows []v1Row, frozen map[string]bool) error {
	if named := len(successorsOfC8a44f97(rows, frozen)); named > v2RouteBound {
		return fmt.Errorf("the rows of c8a44f97 name %d distinct /api/v2 routes, over R2's bound of %d", named, v2RouteBound)
	}
	return nil
}

// handlerResolvedSuccessors are the rows whose successor's path needs a value the v1 request does
// not carry, which the v1 handler resolves while it answers (decision 9 of the plan of
// dispatch://INFRA-4071): rows 6, 17, 89, 91, 92, 161, 164 and 165 of the route map, which need
// an artifact id or an issue key, and row 178, which answers one link per project it lists.
var handlerResolvedSuccessors = map[string]string{
	"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}": "id",
	"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep":                 "key",
	"POST /api/v1/issues/{key}/artifacts/{slug}/edits":                   "id",
	"POST /api/v1/issues/{key}/artifacts/{slug}/versions":                "id",
	"GET /api/v1/issues/{key}/artifacts/{slug}/versions/{number}":        "id",
	"POST /api/v1/projects/{key}/artifacts/{slug}/edits":                 "id",
	"POST /api/v1/projects/{key}/artifacts/{slug}/versions":              "id",
	"GET /api/v1/projects/{key}/artifacts/{slug}/versions/{number}":      "id",
	"GET /api/v1/settings/architecture-sources":                          "key",
}

var wildcardName = regexp.MustCompile(`\{([a-z_]+)\}`)

// wildcardsOf is the wildcards of a pattern, by name.
func wildcardsOf(pattern string) []string {
	var names []string
	for _, match := range wildcardName.FindAllStringSubmatch(pattern, -1) {
		names = append(names, match[1])
	}
	return names
}

// TestEveryV1RowNamesItsV2Route holds the map the table records (R2, c-map): every row /api/v1
// alone serves names the /api/v2 route that answers it, or its deletion with the route that
// answers for it; the INFRA-979 tree's rows are served in both versions instead; every value the
// successor's path needs is one the v1 request or its handler gives; the rows of c8a44f97 name at
// most 137 distinct /api/v2 routes, which a table that names 138 fails; and every successor is a
// route /api/v2 serves (successors-are-mounted).
func TestEveryV1RowNamesItsV2Route(t *testing.T) {
	served := &server{}
	rows := served.v1Rows()
	routes := served.routes()
	frozen := readV1OnlyRoutes(t)
	mounted := map[string]apiRoute{}
	for _, route := range routes {
		mounted[route.Method+" "+route.Pattern] = route
	}
	infra979 := served.v2Infra979Rows()
	ofINFRA979 := map[string]bool{}
	for _, row := range infra979 {
		ofINFRA979[row.Method+" "+versionPrefix(apiV1)+row.Pattern] = true
	}

	t.Run("every-row-names-its-successor", func(t *testing.T) {
		for _, row := range rows {
			name, next := row.Method+" "+row.Pattern, row.Successor
			switch {
			case ofINFRA979[name]:
				t.Errorf("%s is a row of the INFRA-979 tree, served in both versions by its routeRow, and no v1Row", name)
			case next.Method == "":
				t.Errorf("%s names no /api/v2 route", name)
			case strings.HasPrefix(next.Pattern, "/api/"):
				t.Errorf("%s names its successor %s with a version prefix; a successor's pattern is version-free", name, successorRoute(next))
			case next.Pattern != "" && !strings.HasPrefix(next.Pattern, "/"):
				t.Errorf("%s names its successor %q, which is no path", name, successorRoute(next))
			}
		}
		for _, row := range infra979 {
			name := row.Method + " " + versionPrefix(apiV1) + row.Pattern
			for _, version := range []apiVersion{apiV1, apiV2} {
				at := row.Method + " " + versionPrefix(version) + row.Pattern
				route, ok := mounted[at]
				if !ok || !route.columns || !route.OfINFRA979 {
					t.Errorf("%s is a row of the INFRA-979 tree, and the table serves no %s of its routeRow", name, at)
				}
			}
		}
	})

	t.Run("every-successor-path-value-is-filled", func(t *testing.T) {
		for _, row := range rows {
			name, next := row.Method+" "+row.Pattern, row.Successor
			v1Wildcards := wildcardsOf(row.Pattern)
			var resolved []string
			for _, wildcard := range wildcardsOf(next.Pattern) {
				if _, bound := next.Path[wildcard]; bound || slices.Contains(v1Wildcards, wildcard) {
					continue
				}
				resolved = append(resolved, wildcard)
			}
			for wildcard := range next.Path {
				if !slices.Contains(wildcardsOf(next.Pattern), wildcard) {
					t.Errorf("%s binds {%s}, which its successor %s does not hold", name, wildcard, successorRoute(next))
				}
			}
			want, resolves := handlerResolvedSuccessors[name]
			switch {
			case !resolves && len(resolved) > 0:
				t.Errorf("%s names %s, whose {%s} neither the v1 path nor a binding fills", name, successorRoute(next), strings.Join(resolved, "}, {"))
			case resolves && !slices.Equal(resolved, []string{want}):
				t.Errorf("%s names %s, which leaves %v for its handler to resolve; decision 9 has it resolve {%s}", name, successorRoute(next), resolved, want)
			}
		}
	})

	t.Run("the-rows-of-c8a44f97-name-at-most-137-routes", func(t *testing.T) {
		var ofC8a44f97, later int
		for _, row := range rows {
			if isLater, listed := frozen[row.Method+" "+row.Pattern]; listed && !isLater {
				ofC8a44f97++
			}
		}
		for _, isLater := range frozen {
			if isLater {
				later++
			}
		}
		every := map[string]bool{}
		for _, row := range rows {
			every[successorRoute(row.Successor)] = true
		}
		t.Logf("v1 rows, which /api/v1 alone serves: %d (%d of c8a44f97, %d other work added after it); rows of the INFRA-979 tree, served in both versions: %d",
			len(rows), ofC8a44f97, later, len(infra979))
		t.Logf("routes /api/v1 serves: %d; routes /api/v2 serves: %d", len(servedAt(apiV1, routes)), len(servedAt(apiV2, routes)))
		t.Logf("distinct /api/v2 routes the %d rows of c8a44f97 name: %d (R2's bound: %d); the routes every v1 row names: %d",
			ofC8a44f97, len(successorsOfC8a44f97(rows, frozen)), v2RouteBound, len(every))
		if ofC8a44f97 != 184 {
			t.Errorf("v1Rows holds %d rows of c8a44f97, and the parent maps 184", ofC8a44f97)
		}
		if err := successorBoundFault(rows, frozen); err != nil {
			t.Error(err)
		}
	})

	t.Run("a-table-whose-rows-name-138-routes-fails-the-bound", func(t *testing.T) {
		// A scratch copy of the table in which rows of c8a44f97 that share a successor with a row
		// before them each name a route of their own, until the rows name one route more than R2
		// allows.
		scratch := slices.Clone(rows)
		seen := map[string]bool{}
		for index, row := range scratch {
			if len(successorsOfC8a44f97(scratch, frozen)) > v2RouteBound {
				break
			}
			if later, listed := frozen[row.Method+" "+row.Pattern]; !listed || later {
				continue
			}
			if route := successorRoute(row.Successor); seen[route] {
				scratch[index].Successor = successorIs(row.Successor.Method, fmt.Sprintf("/made-up-routes/%d", index))
			} else {
				seen[route] = true
			}
		}
		if named := len(successorsOfC8a44f97(scratch, frozen)); named != v2RouteBound+1 {
			t.Fatalf("the scratch table's rows name %d routes, want %d", named, v2RouteBound+1)
		}
		err := successorBoundFault(scratch, frozen)
		if err == nil || !strings.Contains(err.Error(), "138") {
			t.Fatalf("the bound answered %v for rows that name 138 routes, want them refused", err)
		}
	})

	// Every successor is a route the table mounts at /api/v2, which only the mount of a routeRow
	// is: a row of the v1 table is mounted at its /api/v1 pattern alone (v1Route).
	t.Run("successors-are-mounted", func(t *testing.T) {
		var unmounted []string
		for _, row := range rows {
			at := row.Successor.Method + " /api/v2" + row.Successor.Pattern
			if _, ok := mounted[at]; !ok {
				unmounted = append(unmounted, row.Method+" "+row.Pattern+" -> "+at)
			}
		}
		if len(unmounted) > 0 {
			t.Errorf("%d v1 rows name a /api/v2 route the table does not serve:\n%s", len(unmounted), strings.Join(unmounted, "\n"))
		}
	})
}

// TestAV1OnlyRowOutsideTheFrozenListFails: a row /api/v1 alone serves is one of c8a44f97's, or one
// other work added before /api/v2's change merged, which the frozen list holds (R2, c-map). A row
// added after it is a routeRow, which both versions serve; a made-up v1Row the list does not hold
// fails the check whatever successor it names, and so does a line of the list that is no row.
func TestAV1OnlyRowOutsideTheFrozenListFails(t *testing.T) {
	frozen := readV1OnlyRoutes(t)
	rows := (&server{}).v1Rows()
	if outside := v1OnlyRowsOutside(rows, frozen); len(outside) > 0 {
		t.Fatalf("the v1 rows and %s disagree:\n%s", v1OnlyRoutesList, strings.Join(outside, "\n"))
	}

	for _, next := range []successor{
		{},
		successorIs(http.MethodGet, "/widgets"),
		deletedFor(http.MethodGet, "/issues?widget={widget}"),
		successorIs(http.MethodGet, ""),
	} {
		scratch := append(slices.Clone(rows), v1Row{http.MethodGet, "/api/v1/widgets", authAny, "A made-up row /api/v1 alone serves.", nil, next})
		outside := v1OnlyRowsOutside(scratch, frozen)
		if len(outside) != 1 || !strings.HasPrefix(outside[0], "GET /api/v1/widgets:") {
			t.Errorf("a made-up v1 row naming %+v was answered %q, want it refused alone", next, outside)
		}
	}

	stale := maps.Clone(frozen)
	stale["GET /api/v1/widgets"] = false
	if outside := v1OnlyRowsOutside(rows, stale); len(outside) != 1 || !strings.Contains(outside[0], "no row of v1Rows") {
		t.Errorf("a line of the list no row serves was answered %q, want it named", outside)
	}
}

// madeUpRow is a routeRow no group declares, well formed: a read of widgets.
func madeUpRow() routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/widgets", Auth: authAny, Description: "A made-up row.",
		Handler:  func(http.ResponseWriter, *http.Request) {},
		Resource: "widgets", Kind: kindCollection, List: listBounds{Default: 50, Max: 200},
		Shape: routeShape{Status: http.StatusOK, Response: []string{}},
	}
}

// A row a later change adds says nothing about versions and is served in both, at /api/v1 and at
// /api/v2 (R2: "a row added after this change is served in both versions by one declaration");
// one that names onlyV2 is served at /api/v2 alone, as a row that declares a Query or a Body must
// be (TestABothVersionRowThatDeclaresARequestRefusesToMount). Each mount carries the row's columns.
func TestARowWithNoVersionsIsServedInBothVersions(t *testing.T) {
	mounts := func(row routeRow) []string {
		var got []string
		for _, route := range mountRow(row) {
			if route.Resource != row.Resource || route.List != row.List || !route.columns {
				t.Errorf("%s %s carries %+v, not the row's columns", route.Method, route.Pattern, route.routeRow)
			}
			got = append(got, fmt.Sprintf("%s %s %s", versionPrefix(route.Version), route.Method, route.Pattern))
		}
		return got
	}

	both := madeUpRow()
	if got, want := mounts(both), []string{"/api/v1 GET /api/v1/widgets", "/api/v2 GET /api/v2/widgets"}; !slices.Equal(got, want) {
		t.Errorf("a row with no Versions is mounted as %q, want %q", got, want)
	}
	v2 := madeUpRow()
	v2.Versions = onlyV2
	if got, want := mounts(v2), []string{"/api/v2 GET /api/v2/widgets"}; !slices.Equal(got, want) {
		t.Errorf("a row that names onlyV2 is mounted as %q, want %q", got, want)
	}
}

// A routeRow states every fact a v1 row's side maps state, in its own columns, and a mount whose
// column is missing refuses to mount where a v1 row's missing side-map entry does (register): a
// write with no write class, a route with no shape, an id wildcard with no id facts.
func TestARowMissingAColumnRefusesToMount(t *testing.T) {
	served := &server{}
	write := madeUpRow()
	write.Method, write.Pattern = http.MethodPost, "/widgets/{id}/paints"
	write.Body = requestBody{Kind: bodyJSON, Type: struct {
		Colour string `json:"colour"`
	}{}}
	write.Shape = routeShape{Status: http.StatusCreated, Response: struct{}{}}
	write.Scope, write.ProjectOf = scopeDeployment, projectPathNone
	write.IDs = &idRoute{Reason: "a made-up widget's id"}
	routes := mountRow(write)
	if faults := slices.Concat(served.undecidableWrites(routes), shapelessRoutes(routes), undecidedIDRoutes(routes)); len(faults) > 0 {
		t.Fatalf("a row whose columns say everything was refused: %q", faults)
	}

	for _, missing := range []struct {
		column string
		strip  func(*routeRow)
		guard  func([]apiRoute) []string
		says   string
	}{
		{"Scope", func(row *routeRow) { row.Scope = "" }, served.undecidableWrites, "no write class (its Scope column)"},
		{"ProjectOf", func(row *routeRow) { row.Scope = scopeProject }, served.undecidableWrites, "names no object"},
		{"Shape", func(row *routeRow) { row.Shape = routeShape{} }, shapelessRoutes, "no shape"},
		{"Query", func(row *routeRow) { row.Shape.Params = struct{}{} }, shapelessRoutes, "declares a request"},
		{"IDs", func(row *routeRow) { row.IDs = nil }, undecidedIDRoutes, "its IDs column says neither"},
	} {
		row := write
		missing.strip(&row)
		faults := missing.guard(mountRow(row))
		if len(faults) != 2 || !strings.Contains(faults[0], missing.says) || !strings.Contains(faults[1], missing.says) {
			t.Errorf("a row missing its %s column was answered %q, want both its mounts refused saying %q", missing.column, faults, missing.says)
		}
	}
}

// conditionCodeConflicts names every /api/v2 route that declares a refusal condition with another
// code than a route before it declared it with: one condition is one code across the table
// (decision 4 of the plan of dispatch://INFRA-4071).
func conditionCodeConflicts(routes []apiRoute) []string {
	declared := map[apicode.Condition]apicode.Code{}
	declaredBy := map[apicode.Condition]string{}
	var conflicts []string
	for _, route := range routes {
		if route.Version != apiV2 {
			continue
		}
		name := route.Method + " " + route.Pattern
		for _, refusal := range route.Refusals {
			code, seen := declared[refusal.Condition]
			switch {
			case !seen:
				declared[refusal.Condition], declaredBy[refusal.Condition] = refusal.Code, name
			case code != refusal.Code:
				conflicts = append(conflicts, fmt.Sprintf("%s answers %q with %s, and %s answers it with %s",
					name, refusal.Condition, refusal.Code, declaredBy[refusal.Condition], code))
			}
		}
	}
	return conflicts
}

// TestOneConditionOneCode: over every /api/v2 route's declared refusals, one condition is never
// declared with two codes, so a client reads one code for one fault whichever route it calls; a
// scratch row that declares a second code for a condition another row declares fails it.
func TestOneConditionOneCode(t *testing.T) {
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	declared := 0
	for _, route := range servedAt(apiV2, routes) {
		declared += len(route.Refusals)
	}
	t.Logf("the /api/v2 routes declare %d refusals", declared)
	if conflicts := conditionCodeConflicts(routes); len(conflicts) > 0 {
		t.Fatalf("a condition is declared with two codes:\n%s", strings.Join(conflicts, "\n"))
	}

	first, second := madeUpRow(), madeUpRow()
	first.Versions, first.Refusals = onlyV2, []apicode.Refusal{{Condition: apicode.IssueMissing, Code: apicode.IssueNotFound}}
	second.Versions, second.Pattern = onlyV2, "/widgets/{id}"
	second.Refusals = []apicode.Refusal{{Condition: apicode.IssueMissing, Code: apicode.NotFound}}
	scratch := slices.Concat(routes, mountRow(first), mountRow(second))
	conflicts := conditionCodeConflicts(scratch)
	if len(conflicts) != 1 || !strings.HasPrefix(conflicts[0], "GET /api/v2/widgets/{id} answers") {
		t.Fatalf("a row declaring a second code for a declared condition was answered %q, want it named", conflicts)
	}
}
