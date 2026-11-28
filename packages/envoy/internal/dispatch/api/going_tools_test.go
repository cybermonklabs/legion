// going_tools_test.go holds the tool tables (going_tools.go) to the route table: every route a
// person may call is a row of a tool or a route the bar leaves out for a reason, and every write
// tool states how it is undone, the class of change it is and the event its route appends.
package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going"
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
// a route of the table; every excluded route's reason is one of the eight kinds going_tools.go
// names, which holds of the route (goingExclusionProblem); a route whose declaration states the
// daemon's gate (routeDecl.DaemonWrites) is left out for a daemon gate's reason, and each route
// goingDaemonUngatedRoutes names for goingExcludedDaemonUngated. It fails naming each route that
// breaks one.
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
		if excluded {
			if problem := goingExclusionProblem(route, reason); problem != "" {
				t.Errorf("%s: %s", name, problem)
			}
		}
		if _, gated := goingDaemonGateReason(reason); route.DaemonWrites != nil && !gated {
			t.Errorf("%s: its declaration states DaemonWrites, whose gate refuses every session but the Legion daemon's, and goingExcludedRoutes does not leave it out as a route only the daemon writes (%q): give it goingExcludedDaemon or goingExcludedDaemonRefusing", name, reason)
		}
	}
	for name := range goingExcludedRoutes {
		if !served[name] {
			t.Errorf("goingExcludedRoutes names %s, which is no route of the table", name)
		}
	}
	for _, name := range goingDaemonUngatedRoutes {
		if reason := goingExcludedRoutes[name]; reason != goingExcludedDaemonUngated {
			t.Errorf("%s: goingDaemonUngatedRoutes names it, and goingExcludedRoutes gives it %q rather than goingExcludedDaemonUngated", name, reason)
		}
	}
	for name, tools := range called {
		if !served[name] {
			t.Errorf("%s: a row of %s, which is no route of the table", name, strings.Join(tools, ", "))
		}
	}
}

// goingErrorCode is the shape of the code of a refusal, such as STEP_WRITER.
var goingErrorCode = regexp.MustCompile(`^[A-Z]+(_[A-Z]+)*$`)

// goingDaemonUngatedRoutes is each route goingExcludedRoutes may leave out as only the Legion
// daemon's with no gate of the server's behind it (goingExcludedDaemonUngated): the design-gate
// and gated-step publications, which R12 names as routes only the daemon writes and whose
// declarations name the deployment's daemon as their one writer, and which state no daemon's
// gate, so the server refuses no other caller of them (the architect's ruling,
// dispatch://INFRA-1588/comment/37ca4bfe-e740-41d5-b7fd-8a6327917f49). Every other route left out
// as the daemon's states the gate.
var goingDaemonUngatedRoutes = []string{
	"PUT /api/v1/issues/{key}/design-gates",
	"PUT /api/v1/projects/{key}/gated-steps",
}

// goingDaemonGateReason is whether reason leaves a route out as one whose declaration states the
// daemon's gate - goingExcludedDaemon, or goingExcludedDaemonRefusing of a code - and that code,
// "" for goingExcludedDaemon, which names none.
func goingDaemonGateReason(reason string) (code string, gated bool) {
	if reason == goingExcludedDaemon {
		return "", true
	}
	code, refusing := strings.CutPrefix(reason, goingExcludedDaemon+" ")
	return code, refusing && goingErrorCode.MatchString(code)
}

// goingExclusionProblem is what is wrong with reason, the value goingExcludedRoutes gives route,
// "" for a value of one of the eight kinds going_tools.go names that holds of route: one of R12's
// five constants that hold of no declaration; a daemon gate's reason (goingExcludedDaemon, or
// goingExcludedDaemonRefusing of a code) for a route whose declaration states DaemonWrites, whose
// gate refuses with that code; goingExcludedDaemonUngated for a route goingDaemonUngatedRoutes
// names, which states no gate; a route reached through another tool, naming a tool the bar
// offers - one goingWriteTools or goingReadTools names and going.UnofferedTools does not leave
// out - and why (goingExcludedThrough); and goingExcludedNoPerson for a route whose declaration
// states ResultsScope, the route a CI run's results token writes.
func goingExclusionProblem(route apiRoute, reason string) string {
	switch reason {
	case goingExcludedCredential, goingExcludedAgentToken, goingExcludedSignIn, goingExcludedTestHook, goingExcludedBar:
		return ""
	case goingExcludedDaemonUngated:
		switch {
		case route.DaemonWrites != nil:
			return "goingExcludedRoutes says the server refuses no other caller of it, and its declaration states DaemonWrites, whose gate refuses every session but the Legion daemon's: give it goingExcludedDaemon or goingExcludedDaemonRefusing"
		case !slices.Contains(goingDaemonUngatedRoutes, route.Method+" "+route.Pattern):
			return "goingExcludedRoutes says only the Legion daemon writes it with no gate of the server's, and it is none of the publications goingDaemonUngatedRoutes names: state DaemonWrites in its declaration and give it goingExcludedDaemon, or leave it out for another kind"
		}
		return ""
	case goingExcludedNoPerson:
		if !route.ResultsScope {
			return "goingExcludedRoutes says no person may call it, and its declaration does not state ResultsScope, so a person's credential reaches its handler"
		}
		return ""
	}
	if code, gated := goingDaemonGateReason(reason); gated {
		switch {
		case route.DaemonWrites == nil && slices.Contains(goingDaemonUngatedRoutes, route.Method+" "+route.Pattern):
			return "goingExcludedRoutes says every other caller of it is refused, and its declaration states no DaemonWrites, so the server refuses no other caller: give it goingExcludedDaemonUngated"
		case route.DaemonWrites == nil:
			return "goingExcludedRoutes says only the Legion daemon writes it and every other caller is refused, and its declaration does not state DaemonWrites, so a person's credential reaches its handler"
		case code != "" && code != route.DaemonWrites.code():
			return fmt.Sprintf("goingExcludedRoutes says the daemon's gate refuses every other caller %s, and the gate its declaration states (DaemonWrites) refuses them %s", code, route.DaemonWrites.code())
		}
		return ""
	}
	if named, through := strings.CutPrefix(reason, goingExcludedThroughWords); through {
		tool, why, _ := strings.Cut(named, ": ")
		_, writes := goingWriteTools[tool]
		_, reads := goingReadTools[tool]
		_, unoffered := going.UnofferedTools[tool]
		switch {
		case strings.TrimSpace(why) == "":
			return fmt.Sprintf("goingExcludedRoutes says another tool meets its need (%q), and does not say which tool and why: build it with goingExcludedThrough", reason)
		case !writes && !reads:
			return fmt.Sprintf("goingExcludedRoutes says %s meets its need, which neither goingWriteTools nor goingReadTools names", tool)
		case unoffered:
			return fmt.Sprintf("goingExcludedRoutes says %s meets its need, which going.UnofferedTools leaves out of the bar", tool)
		}
		return ""
	}
	return fmt.Sprintf("goingExcludedRoutes leaves it out for %q, which is none of the eight kinds of reason going_tools.go names: give it a kind's constant or constructor", reason)
}

// TestEveryPersonRouteHasATool: a route a person may call is one a run can read or write
// through, or one the bar leaves out by name and reason, so a route added later with no tool
// fails here.
func TestEveryPersonRouteHasATool(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestGoingToolReadRouteIsTheRowTheServerPicks: GoingReadRoute names the row a read tool's call
// reads, as the server picks it, with nothing sent: dispatch_read by what its reference names (a
// block's, written as a dispatch:// reference or a dashboard URL under the server's own address,
// reads the reference graph), a bound read by its spec's binding; and it names none for a call the
// server refuses before it picks a row, a write tool, and a tool the server has not.
func TestGoingToolReadRouteIsTheRowTheServerPicks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tool string
		arguments  map[string]any
		route      string
	}{
		{"a block's reference", "dispatch_read", map[string]any{"ref": "dispatch://DSP-1/spec#b-x"}, "GET /api/v1/references"},
		{"a block's dashboard URL", "dispatch_read", map[string]any{"ref": testServerURL + "/issues/DSP-1/spec#b-x"}, "GET /api/v1/references"},
		{"an issue's reference", "dispatch_read", map[string]any{"ref": "dispatch://DSP-1"}, "GET /api/v1/issues/{key}"},
		{"an issue", "dispatch_read", map[string]any{"issue": "DSP-1"}, "GET /api/v1/issues/{key}"},
		{"an issue's references", "dispatch_read_issue_references", map[string]any{"issue": "DSP-1"}, "GET /api/v1/issues/{key}/references"},
		{"a document's references", "dispatch_read_document_references", map[string]any{"document": "0190c7d2-1111-7222-8333-444444444444"},
			"GET /api/v1/artifacts/{id}/references"},
		{"a search", "dispatch_search", map[string]any{"query": "quernstone"}, "GET /api/v1/search"},
		{"a read that names nothing", "dispatch_read", map[string]any{}, ""},
		{"a bound read missing its path's argument", "dispatch_read_issue_references", map[string]any{}, ""},
		{"a write", "dispatch_comment", map[string]any{"issue": "DSP-1", "body": "hi"}, ""},
		{"no tool of the server", "summon_dragon", map[string]any{}, ""},
	} {
		route, ok := GoingReadRoute(test.tool, test.arguments, testOrigins)
		if route != test.route || ok != (test.route != "") {
			t.Errorf("%s: %s %v reads %q (%v), want %q", test.name, test.tool, test.arguments, route, ok, test.route)
		}
	}
}
