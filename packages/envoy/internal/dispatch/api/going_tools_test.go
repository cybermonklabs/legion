// going_tools_test.go holds the tool tables (going_tools.go) to the route table: every route a
// person may call is a row of a tool or a route the bar leaves out for a reason, and every write
// tool states how it is undone, the class of change it is and the event its route appends.
package api

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// goingCalledRoutes is, by "<METHOD> <pattern>", the tools whose rows call each route: every row
// of goingWriteTools and goingReadTools, a tool whose spec binds its routes contributing its
// bindings (goingToolRows).
func goingCalledRoutes(t *testing.T) map[string][]string {
	t.Helper()
	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	called := map[string][]string{}
	add := func(tool string, rows []goingToolRoute) {
		for _, row := range rows {
			name := row.Method + " " + row.Pattern
			if !slices.Contains(called[name], tool) {
				called[name] = append(called[name], tool)
			}
		}
	}
	for tool, entry := range goingWriteTools {
		rows, _ := goingToolRows(specs[tool], entry.Routes, entry.UndoRoutes)
		add(tool, rows)
	}
	for tool, entry := range goingReadTools {
		rows := entry.Routes
		if spec, stated := specs[tool]; stated {
			rows, _ = goingToolRows(spec, entry.Routes, nil)
		}
		add(tool, rows)
	}
	return called
}

// goingRouteInventory holds the route table, its test hooks in, to the tool tables: every route
// whose auth class a person's credential passes is a row of a tool or a route
// goingExcludedRoutes names, and never both; every excluded route and every row a tool names is
// a route of the table. It fails naming each route that breaks one.
func goingRouteInventory(t *testing.T) {
	t.Helper()
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	called := goingCalledRoutes(t)
	served := make(map[string]bool, len(routes))
	for _, route := range routes {
		name := route.Method + " " + route.Pattern
		served[name] = true
		switch route.Auth {
		case authPublic, authAny, authBearer:
			// A person's credential passes each: a public route needs none, a sign-in or a
			// personal agent token is any credential, and that token is a bearer.
		default:
			t.Errorf("%s: its auth %q is no class this inventory decides whether a person passes", name, route.Auth)
			continue
		}
		tools := called[name]
		reason, excluded := goingExcludedRoutes[name]
		switch {
		case len(tools) == 0 && !excluded:
			t.Errorf("%s: a person may call it, and no tool calls it and goingExcludedRoutes does not name it", name)
		case len(tools) > 0 && excluded:
			t.Errorf("%s: %s call it, and goingExcludedRoutes leaves it out (%s)", name, strings.Join(tools, ", "), reason)
		}
	}
	for name := range goingExcludedRoutes {
		if !served[name] {
			t.Errorf("goingExcludedRoutes names %s, which is no route of the table", name)
		}
	}
	for name, tools := range called {
		if !served[name] {
			t.Errorf("%s: a row of %s, which is no route of the table", name, strings.Join(tools, ", "))
		}
	}
}

// TestEveryPersonRouteHasATool: a route a person may call is one a run can read or write
// through, or one the bar leaves out by name and reason, so a route added later with no tool
// fails here.
func TestEveryPersonRouteHasATool(t *testing.T) {
	goingRouteInventory(t)
	called := goingCalledRoutes(t)
	t.Logf("%d routes a tool calls, %d excluded", len(called), len(goingExcludedRoutes))
}

// goingEventType is the shape of an event type (model.Event.Type): a noun and a verb, each lower
// case words joined by underscores, such as issue.updated or settings.repo_project.updated.
var goingEventType = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)+$`)

// TestEveryWriteToolNamesItsUndoKindClassAndEvent: each write tool states one of the four undos,
// one of the classes, and its event, an event type or goingNoEvent for a route that appends none;
// a reverse undoes through routes of the table.
func TestEveryWriteToolNamesItsUndoKindClassAndEvent(t *testing.T) {
	mutating := mutatingRoutes((&server{}).routes())
	specs := make(map[string]contracts.DispatchTool, len(contracts.DispatchTools))
	for _, spec := range contracts.DispatchTools {
		specs[spec.Name] = spec
	}
	names := make([]string, 0, len(goingWriteTools))
	for name := range goingWriteTools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tool := goingWriteTools[name]
		switch tool.Undo {
		case goingUndoReverse, goingUndoWriteBack, goingUndoCloseCreated, goingUndoNotUndone:
		default:
			t.Errorf("%s names no undo kind of the four (%q)", name, tool.Undo)
		}
		if !slices.Contains(goingWriteClasses, tool.Class) {
			t.Errorf("%s names no class of %v (%q)", name, goingWriteClasses, tool.Class)
		}
		if tool.Event != goingNoEvent && !goingEventType.MatchString(tool.Event) {
			t.Errorf("%s names no event (%q): name the event type its route appends, or goingNoEvent", name, tool.Event)
		}
		if tool.Undo != goingUndoReverse {
			continue
		}
		rows, _ := goingToolRows(specs[name], tool.Routes, tool.UndoRoutes)
		if len(rows) == 0 {
			t.Errorf("%s is reversed, and names no row", name)
		}
		for _, row := range rows {
			if len(row.UndoRoutes) == 0 {
				t.Errorf("%s: %s %s is reversed through no route", name, row.Method, row.Pattern)
			}
			for _, reverse := range row.UndoRoutes {
				if !mutating[reverse] {
					t.Errorf("%s is reversed through %s, which is no route of the table", name, reverse)
				}
			}
		}
	}
}

// TestABoundToolCallsTheRouteItsSpecBinds: a tool whose spec binds its routes is called through
// that binding with no translation written for it - a write fills its path and sends its body
// arguments as a JSON body, and a read sends its query arguments, a list as one parameter per
// item - and a version that is a number fills its path as an argument that is a text does.
func TestABoundToolCallsTheRouteItsSpecBinds(t *testing.T) {
	catalogue, err := BuildGoingCatalogue()
	if err != nil {
		t.Fatalf("build the catalogue: %v", err)
	}

	t.Run("a write", func(t *testing.T) {
		doer := &countingDoer{}
		caller := GoingCaller{Catalogue: catalogue, BaseURL: "https://dispatch.invalid", Doer: doer}
		arguments := map[string]any{"document": "01234567-0000-4000-8000-000000000001", "name": "design.md"}
		if _, err := caller.Do(context.Background(), "dispatch_rename_document", arguments); err != nil {
			t.Fatalf("rename a document: %v", err)
		}
		sent := doer.requests[0]
		body, _ := io.ReadAll(sent.Body)
		if sent.Method != http.MethodPatch || sent.URL.Path != "/api/v1/artifacts/01234567-0000-4000-8000-000000000001" || string(body) != `{"name":"design.md"}` {
			t.Fatalf("the caller sent %s %s %s", sent.Method, sent.URL.Path, body)
		}
	})

	t.Run("a read", func(t *testing.T) {
		var spec contracts.DispatchTool
		for _, tool := range contracts.DispatchTools {
			if tool.Name == "dispatch_find_related" {
				spec = tool
			}
		}
		rows, bound := goingToolRows(spec, goingReadTools["dispatch_find_related"].Routes, nil)
		if !bound || len(rows) != 1 {
			t.Fatalf("dispatch_find_related's rows are %v", rows)
		}
		arguments := map[string]any{"project": "DSP", "title": "Renamed issues", "exclude_issues": []any{"DSP-1", "DSP-2"}, "keywords_only": true}
		call, err := rows[0].Translate(context.Background(), goingReader{}, goingCallInput{arguments: arguments, path: map[string]string{"key": "DSP"}})
		if err != nil {
			t.Fatalf("translate the read: %v", err)
		}
		requests, err := call.requests(GoingRoute{Method: rows[0].Method, Pattern: rows[0].Pattern})
		if err != nil {
			t.Fatalf("build the read: %v", err)
		}
		read := requests[0]
		want := url.Values{"title": {"Renamed issues"}, "exclude_issues": {"DSP-1", "DSP-2"}, "keywords_only": {"true"}}
		if read.method != http.MethodGet || read.path != "/api/v1/projects/DSP/related" || read.query.Encode() != want.Encode() || read.body != nil {
			t.Fatalf("the read is %s %s?%s with body %q", read.method, read.path, read.query.Encode(), read.body)
		}
	})

	t.Run("a number fills a path", func(t *testing.T) {
		action, _ := catalogue.Lookup("dispatch_review")
		_, values, chosen := chooseGoingRoute(action, map[string]any{"issue": "DSP-1", "version": float64(3), "verdict": "approved"}, "")
		if !chosen || values["number"] != "3" {
			t.Fatalf("a review of version 3 chose %v with %v", chosen, values)
		}
	})
}
