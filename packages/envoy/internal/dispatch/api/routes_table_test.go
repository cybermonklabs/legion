package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/routefile"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

func TestRouteIndexListsEveryRouteSortedByPathThenMethod(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	index := decodeBody[routeIndex](t, response)
	if index.Docs != "skills/dispatch/SKILL.md" {
		t.Fatalf("docs = %q", index.Docs)
	}
	if !sort.SliceIsSorted(index.Routes, func(left, right int) bool {
		if index.Routes[left].Path != index.Routes[right].Path {
			return index.Routes[left].Path < index.Routes[right].Path
		}
		return index.Routes[left].Method < index.Routes[right].Method
	}) {
		t.Fatalf("routes are not sorted by path then method: %+v", index.Routes)
	}
	found := false
	for _, entry := range index.Routes {
		if entry.Method == http.MethodGet && entry.Path == "/api/v1/issues/{key}" {
			found = true
			if entry.Auth != "any" {
				t.Fatalf("GET /api/v1/issues/{key} auth = %q, want any", entry.Auth)
			}
			if entry.Description == "" {
				t.Fatal("GET /api/v1/issues/{key} has no description")
			}
		}
	}
	if !found {
		t.Fatalf("GET /api/v1/issues/{key} missing from the index: %+v", index.Routes)
	}
	if len(index.Routes) != len((&server{}).routes()) {
		t.Fatalf("index lists %d routes, table has %d", len(index.Routes), len((&server{}).routes()))
	}
}

// WriteRouteIndex is what `envoy-dispatch routes` prints and the docs site's API reference is
// generated from: every row of the table, as GET /api/v1 serves it, and no test hook.
func TestWriteRouteIndexCarriesEveryRouteTheTableHas(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := WriteRouteIndex(&output); err != nil {
		t.Fatal(err)
	}
	var printed routeIndex
	if err := json.Unmarshal(output.Bytes(), &printed); err != nil {
		t.Fatalf("decode %q: %v", output.String(), err)
	}
	table := (&server{}).routes()
	if len(printed.Routes) != len(table) {
		t.Fatalf("printed %d routes, table has %d", len(printed.Routes), len(table))
	}
	byKey := make(map[string]routeIndexEntry, len(printed.Routes))
	for _, entry := range printed.Routes {
		byKey[entry.Method+" "+entry.Path] = entry
	}
	for _, route := range table {
		entry, ok := byKey[route.Method+" "+route.Pattern]
		if !ok {
			t.Errorf("%s %s missing from the printed index", route.Method, route.Pattern)
			continue
		}
		if entry.Auth != route.Auth || entry.Description != route.Description {
			t.Errorf("%s %s printed as auth %q, description %q; the table says %q, %q", route.Method, route.Pattern, entry.Auth, entry.Description, route.Auth, route.Description)
		}
	}
	for _, entry := range printed.Routes {
		if strings.Contains(entry.Path, "/_test/") {
			t.Errorf("printed index carries test hook %s %s", entry.Method, entry.Path)
		}
	}

	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if served := response.Body.String(); served != output.String() {
		t.Fatalf("printed index differs from GET /api/v1:\nprinted %s\nserved  %s", output.String(), served)
	}
}

// The table is the registration: every row is mounted, every row is described, and no row is
// listed twice.
//
// No count is pinned. A count is one number two branches cannot both be right about: a branch
// that adds a route updates it, and the base that added one first then makes that branch's
// merge red for a reason that is not the branch's change
// (docs/solutions/github/a-base-gate-arrives-in-the-merge-and-the-job-your-branch-alone-adds-is-the-line-it-refuses-pr2517.md:63-67
// records that failure: "routes() has 147 rows, want 146: the base had added a route"). A route
// added or removed is a file added or removed, which a diff shows by its name, and two branches
// that each add one merge two new files.
func TestRoutesTablePinsEveryRegisteredRoute(t *testing.T) {
	t.Parallel()
	routes := (&server{}).routes()
	seen := make(map[string]struct{}, len(routes))
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("route %q listed twice", key)
		}
		seen[key] = struct{}{}
		if route.Description == "" {
			t.Fatalf("route %q has no description", key)
		}
		if route.routeDecl.Handler == nil {
			t.Fatalf("route %q has no handler", key)
		}
		switch route.Auth {
		case authPublic, authAny, authBearer:
		default:
			t.Fatalf("route %q has auth %q", key, route.Auth)
		}
	}
	if got := len((&server{deps: Deps{TestHooksEnabled: true}}).routes()); got != len(routes)+15 {
		t.Fatalf("test hooks add fifteen routes: got %d, want %d", got, len(routes)+15)
	}
}

// declaredRoute is the declaration of one route, by "<method> <pattern>", test hooks included.
// Only a test reads one route out of the registry by name: the package itself reads the whole
// registry (routes()).
func declaredRoute(name string) (routeDecl, bool) {
	for _, route := range routeRegistry {
		if route.Method+" "+route.Pattern == name {
			return route, true
		}
	}
	return routeDecl{}, false
}

// duplicateRoutes names, sorted, every "<method> <pattern>" that more than one of routes
// declares. Each route has one file, so a second declaration of one is a file copied and left
// unchanged, or two changes that each added the route under a name of their own; either way the
// table would hold two rows of one route, and ServeMux refuses the second. The walk is a test's:
// the file-name rule is what stops the second declaration from being written
// (TestEachRouteIsDeclaredInTheFileItsNameNames), and this is what fails where one was.
func duplicateRoutes(routes []routeDecl) []string {
	seen := make(map[string]int, len(routes))
	for _, route := range routes {
		seen[route.Method+" "+route.Pattern]++
	}
	var duplicates []string
	for name, count := range seen {
		if count > 1 {
			duplicates = append(duplicates, fmt.Sprintf("%s is declared %d times", name, count))
		}
	}
	sort.Strings(duplicates)
	return duplicates
}

// No route is declared twice: a route file copied and left unchanged, or two branches that each
// added one route under a file name of their own, would leave two declarations of one method and
// pattern, which merge with no conflict since they are two files. The walk is over the registry
// itself, test hooks included, because routes() and the index each hold whatever the registry
// holds. The sub-test is the negative control: the same walk over the registry with one
// declaration planted a second time names it, and names nothing else.
func TestNoRouteIsDeclaredTwice(t *testing.T) {
	t.Parallel()
	if len(routeRegistry) == 0 {
		t.Fatal("the registry holds no route, so this walk read nothing")
	}
	if duplicates := duplicateRoutes(routeRegistry); len(duplicates) > 0 {
		t.Fatalf("routes declared more than once:\n%s", strings.Join(duplicates, "\n"))
	}

	t.Run("a route declared a second time fails this walk", func(t *testing.T) {
		planted, declared := declaredRoute("POST /api/v1/issues/{key}/comments")
		if !declared {
			t.Fatal("no route declares POST /api/v1/issues/{key}/comments")
		}
		planted.Description = "a second declaration, written in another file"
		duplicates := duplicateRoutes(append(slices.Clone(routeRegistry), planted))
		want := []string{"POST /api/v1/issues/{key}/comments is declared 2 times"}
		if !slices.Equal(duplicates, want) {
			t.Fatalf("the walk answered %q, want %q", duplicates, want)
		}
	})
}

// Each route is declared in the file routefile.Name gives its method and pattern, and that file
// declares that route alone. The name is what makes two branches that add one route meet as one
// file added twice, which git refuses to merge, rather than as two files that merge cleanly and
// leave the route declared twice. The walk reads the package's own source, so a file whose name
// nobody held to the rule fails here rather than at a merge.
func TestEachRouteIsDeclaredInTheFileItsNameNames(t *testing.T) {
	t.Parallel()
	declaredIn := map[string][]string{}
	for _, file := range goFilesOfThisPackage(t) {
		name := filepath.Base(file)
		if !routefile.Is(name) {
			continue
		}
		for _, route := range routesDeclaredIn(t, file) {
			declaredIn[name] = append(declaredIn[name], route)
		}
	}
	if len(declaredIn) != len(routeRegistry) {
		t.Fatalf("%d route files declare routes and the registry holds %d", len(declaredIn), len(routeRegistry))
	}
	for _, route := range routeRegistry {
		want, err := routefile.Name(route.Method, route.Pattern)
		if err != nil {
			t.Errorf("%s %s: %v", route.Method, route.Pattern, err)
			continue
		}
		declared := declaredIn[want]
		if len(declared) != 1 || declared[0] != route.Method+" "+route.Pattern {
			t.Errorf("%s %s is declared in %v, want %s alone", route.Method, route.Pattern, declared, want)
		}
	}
}

// routesDeclaredIn is every "<method> <pattern>" a file of this package declares, read from its
// source: the Method and Pattern of each declareRoute(routeDecl{...}) it holds.
func routesDeclaredIn(t *testing.T, file string) []string {
	t.Helper()
	var routes []string
	ast.Inspect(parseAPIFile(t, file), func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if fun, named := call.Fun.(*ast.Ident); !named || fun.Name != "declareRoute" || len(call.Args) != 1 {
			return true
		}
		literal, isLiteral := call.Args[0].(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		var method, pattern string
		for _, element := range literal.Elts {
			pair, keyed := element.(*ast.KeyValueExpr)
			key, named := pair.Key.(*ast.Ident)
			if !keyed || !named {
				continue
			}
			switch key.Name {
			case "Method":
				selector, isSelector := pair.Value.(*ast.SelectorExpr)
				if !isSelector {
					t.Fatalf("%s: its Method is no http.Method constant", file)
				}
				method = strings.ToUpper(strings.TrimPrefix(selector.Sel.Name, "Method"))
			case "Pattern":
				switch value := pair.Value.(type) {
				case *ast.BasicLit:
					unquoted, err := strconv.Unquote(value.Value)
					if err != nil {
						t.Fatalf("%s: %v", file, err)
					}
					pattern = unquoted
				case *ast.Ident:
					// A pattern another file declares a constant for, such as goingCataloguePath.
					pattern = patternOfConstant(t, value.Name)
				default:
					t.Fatalf("%s: its Pattern is neither a string nor a constant", file)
				}
			}
		}
		if method != "" && pattern != "" {
			routes = append(routes, method+" "+pattern)
		}
		return false
	})
	return routes
}

// patternOfConstant is the value of a package-level string constant a route's Pattern names.
func patternOfConstant(t *testing.T, name string) string {
	t.Helper()
	for _, file := range goFilesOfThisPackage(t) {
		for _, declared := range parseAPIFile(t, file).Decls {
			general, isGeneral := declared.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				for index, ident := range value.Names {
					if ident.Name != name || index >= len(value.Values) {
						continue
					}
					literal, isLiteral := value.Values[index].(*ast.BasicLit)
					if !isLiteral {
						t.Fatalf("the constant %s is no string literal", name)
					}
					unquoted, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					return unquoted
				}
			}
		}
	}
	t.Fatalf("no file of this package declares the constant %s", name)
	return ""
}

// The Inbox route's description states the rule the person a question reaches is resolved by —
// the person it was handed to, then the issue's assignee, then the project's default assignee —
// and the four descriptions that rule narrowed no longer carry what it replaced. This is a test
// of its own because the two route tests above read every description and assert no word of
// one: the index test reads one description alone and the pin only holds each row to having one,
// so nothing else fails when a description outlives the code it describes.
func TestTheInboxRouteDescriptionStatesTheRule(t *testing.T) {
	t.Parallel()
	descriptions := map[string]string{}
	for _, route := range (&server{}).routes() {
		descriptions[route.Method+" "+route.Pattern] = route.Description
	}
	for _, want := range []struct{ route, says string }{
		// The three steps, in the order the read resolves them.
		{"GET /api/v1/inbox", "handed to"},
		{"GET /api/v1/inbox", "the issue's assignee"},
		{"GET /api/v1/inbox", "the project's default assignee"},
		// A create names the person, and says what happens when it names nobody.
		{"POST /api/v1/projects", "default_assignee"},
		{"POST /api/v1/projects", "ASSIGNEE_NOT_ALLOWED"},
		// The settings list holds every project, not only the ones that named somebody.
		{"GET /api/v1/settings/project-default-assignees", "every project"},
		// The write moves a default and refuses to clear it.
		{"PUT /api/v1/projects/{key}/default-assignee", "cannot be cleared"},
	} {
		description, mounted := descriptions[want.route]
		if !mounted {
			t.Fatalf("the table mounts no %s", want.route)
		}
		if !strings.Contains(description, want.says) {
			t.Errorf("%s does not say %q: %s", want.route, want.says, description)
		}
	}
	for _, stale := range []struct{ route, says string }{
		// A null no longer clears a default, so the write's description cannot offer one.
		{"PUT /api/v1/projects/{key}/default-assignee", "null to clear"},
		// Every project names a default, so the list leaves none out.
		{"GET /api/v1/settings/project-default-assignees", "that names a default assignee"},
		// The create chain's last step always lands, so it carries no qualification.
		{"POST /api/v1/issues", "still names a person who has signed in"},
		// The inbox resolves three steps and stops at none of them.
		{"GET /api/v1/inbox", "until somebody hands it on"},
	} {
		description, mounted := descriptions[stale.route]
		if !mounted {
			t.Fatalf("the table mounts no %s", stale.route)
		}
		if strings.Contains(description, stale.says) {
			t.Errorf("%s still says %q, which the three-step rule made false: %s", stale.route, stale.says, description)
		}
	}
}

// TestEveryIDRouteIsNamed: every route whose pattern carries {id} or {block_id} says in its ID
// what that id names — a kind whose prefix is resolved into it and the 404 that route answers for
// an id it does not hold, or a reason it resolves none — and no route whose pattern carries no id
// states one. The sub-tests are the negative control, as TestEveryMutatingRouteIsClassed's are:
// the same walk over a fixture holding a route that states no ID, one that resolves nothing and
// says why not at once, one that resolves an id without naming that 404, and one that states an
// ID with no id to name, fails on each.
func TestEveryIDRouteIsNamed(t *testing.T) {
	t.Parallel()
	routes := (&server{deps: Deps{TestHooksEnabled: true}}).routes()
	if undecided := undecidedIDRoutes(routes); len(undecided) > 0 {
		t.Fatalf("%d route(s) whose id the prefix layer cannot decide:\n%s", len(undecided), strings.Join(undecided, "\n"))
	}
	withID := func(pattern string, entry *idRoute) apiRoute {
		return apiRoute{routeDecl: routeDecl{Method: http.MethodGet, Pattern: pattern, Auth: authAny, ID: entry}}
	}

	t.Run("a route that states no ID fails this walk", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{withID("/api/v1/widgets", nil), withID("/api/v1/widgets/{id}", nil)})
		if len(undecided) != 1 || !strings.HasPrefix(undecided[0], "GET /api/v1/widgets/{id}:") {
			t.Fatalf("the walk answered %q, want the unnamed id route named and the other left alone", undecided)
		}
	})

	t.Run("a row that resolves nothing and gives no reason fails it too", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{withID("/api/v1/widgets/{block_id}", &idRoute{})})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "gives no reason") {
			t.Fatalf("the walk answered %q, want the reasonless row named", undecided)
		}
	})

	t.Run("a row that resolves a kind and gives a reason fails it too", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{
			withID("/api/v1/widgets/{id}", &idRoute{Kind: "ask", NotFound: "NOT_FOUND", Reason: "and also not"}),
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "a reason not to") {
			t.Fatalf("the walk answered %q, want the contradictory row named", undecided)
		}
	})

	t.Run("a row that resolves an id and names no 404 of its own fails it too", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{withID("/api/v1/widgets/{id}", &idRoute{Kind: "ask"})})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "what 404 the route answers") {
			t.Fatalf("the walk answered %q, want the row with no not-found answer named", undecided)
		}
	})

	t.Run("a row that resolves a block and names no 404 for it fails it too", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{
			withID("/api/v1/widgets/{id}/blocks/{block_id}", &idRoute{Kind: "artifact", NotFound: "ARTIFACT_NOT_FOUND", Blocks: true}),
		})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "the document does not hold") {
			t.Fatalf("the walk answered %q, want the row with no block not-found answer named", undecided)
		}
	})

	t.Run("a route with no id that states an ID fails it too", func(t *testing.T) {
		undecided := undecidedIDRoutes([]apiRoute{withID("/api/v1/widgets", &idRoute{Reason: "a widget id"})})
		if len(undecided) != 1 || !strings.Contains(undecided[0], "its pattern carries no id") {
			t.Fatalf("the walk answered %q, want the route with no id to name named", undecided)
		}
	})

	t.Run("every row names a kind the prefix resolver reads and a 404 that is one", func(t *testing.T) {
		for _, route := range routes {
			if route.ID == nil {
				continue
			}
			name, entry := route.Method+" "+route.Pattern, *route.ID
			switch entry.Kind {
			case "", "ask", "comment", "message", "artifact":
			default:
				t.Errorf("%s states the kind %q, which refs.ResolvePrefix does not read", name, entry.Kind)
			}
			if entry.WithinIssue && !strings.Contains(name, "{key}") {
				t.Errorf("%s looks within the issue its pattern names, and it names none", name)
			}
			if entry.Blocks && !strings.Contains(name, "{block_id}") {
				t.Errorf("%s reads a block, and its pattern carries no {block_id}", name)
			}
			if strings.Contains(name, "{block_id}") && !entry.Blocks {
				t.Errorf("%s leaves its {block_id} unresolved", name)
			}
			for column, code := range map[string]string{"NotFound": entry.NotFound, "BlockNotFound": entry.BlockNotFound} {
				if code != "" && code != notFoundNone && !strings.HasSuffix(code, "NOT_FOUND") {
					t.Errorf("%s states the %s code %q, which is no not-found code", name, column, code)
				}
			}
		}
	})
}

// routeProbePath fills a mux pattern's wildcards with well-formed placeholders so the request
// reaches the handler's auth gate rather than a pattern mismatch.
func routeProbePath(pattern string) string {
	replacements := []struct{ from, to string }{
		{"/projects/{key}", "/projects/TEST"},
		{"/issues/{key}", "/issues/TEST-1"},
		{"/me/issues/{key}", "/me/issues/TEST-1"},
		{"{id}", "00000000-0000-0000-0000-000000000001"},
		{"{session_id}", "s1"},
		{"{block_id}", "b1"},
		{"{login}", "alice"},
		{"{owner}", "owner"},
		{"{repo}", "repo"},
		{"{slug}", "spec"},
		{"{number}", "1"},
		{"{attempt}", "1"},
		{"{phase}", "plan"},
	}
	path := pattern
	for _, replacement := range replacements {
		path = strings.ReplaceAll(path, replacement.from, replacement.to)
	}
	return path
}

// routeProbeBody is the JSON body a probe sends. Every mutation that decodes before it
// authenticates accepts `actor` (a bearer's self-identification). Two kinds of route need more:
// a review route refuses a body without a valid state (400 INVALID_REVIEW) before it reads the
// document, so its probe is an approval, and a reply route answers REPLY_FORBIDDEN to a malformed
// reply as well as to the wrong caller, so its probe must be a complete reply to reach the caller
// check.
func routeProbeBody(route apiRoute) string {
	switch route.Method + " " + route.Pattern {
	case "POST /api/v1/artifacts/{id}/reviews", "POST /api/v1/artifacts/{id}/versions/{number}/reviews":
		return `{"state":"approved"}`
	case "POST /api/v1/messages/{id}/reply", "POST /api/v1/comments/{id}/reply":
		return `{"actor":{"kind":"session","id":"s1"},"attempt":1,"body":"probe"}`
	}
	return `{"actor":{"kind":"session","id":"s1"}}`
}

// authRefusal reports whether a response is the server turning the caller away for what its
// credential is: 401, or a 403 whose code names the credential (OAUTH_SCOPE_REQUIRED is a
// credential that holds none of the row's scopes; HANDOFF_FORBIDDEN is a cookie caller on the
// bearer-only handoff write). Other 4xx/5xx are the handler proceeding past the gate on
// placeholder input.
func authRefusal(response *httptest.ResponseRecorder) (bool, string) {
	switch response.Code {
	case http.StatusUnauthorized:
		return true, "401"
	case http.StatusForbidden:
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		switch body.Code {
		case codeScopeRequired, "REPLY_FORBIDDEN", "ACCEPT_FORBIDDEN", "HANDOFF_FORBIDDEN":
			return true, "403 " + body.Code
		}
	}
	return false, ""
}

// personalTokenHolding mints a personal agent token of alice's on the test store holding exactly
// the scopes named, and answers its plaintext: the credential a scoped row admits.
func personalTokenHolding(t *testing.T, database *store.Store, scopes ...string) string {
	t.Helper()
	return personalTokenOwnedBy(t, database, "alice", scopes...)
}

// personalTokenOwnedBy mints one person's personal agent token holding exactly the scopes named.
// The row is written straight to the table rather than minted through POST
// /api/v1/me/agent-tokens, which is itself one of the rows under test and needs a credential of
// this kind to call. The owner is a parameter because two owners' tokens are what tells a route
// comparing whole identities from one comparing kind and id alone: every personal token resolves
// with an empty id (agent_tokens.go), so only the owner tells them apart.
func personalTokenOwnedBy(t *testing.T, database *store.Store, owner string, scopes ...string) string {
	t.Helper()
	plaintext, hash, err := newAgentToken()
	if err != nil {
		t.Fatalf("mint a personal token: %v", err)
	}
	if scopes == nil {
		// A caller that names no scope wants a token holding none, and the column is not null:
		// an empty set, as the shared token's credential holds, rather than a null one.
		scopes = []string{}
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into agent_tokens (owner, name, token_hash, prefix, scopes)
		values ($1, $2, $3, $4, $5)
	`, owner, "probe", hash[:], plaintext[len("dsp_"):len("dsp_")+agentTokenPrefixLength], scopes); err != nil {
		t.Fatalf("insert a personal token: %v", err)
	}
	return plaintext
}

// Every row's auth cell is checked against what its handler does to a cookie caller, a bearer,
// and an anonymous request, so GET /api/v1 cannot describe a route the server refuses (or
// admits) differently. A row that names OAuth scopes is checked against three bearers: the
// deployment's shared token, which holds no scope and is refused OAUTH_SCOPE_REQUIRED; a
// person's own token holding exactly the scopes the row declares, which is admitted; and, on a
// row that names `approve`, a token of that person holding `person` alone, which is refused.
// That last probe is what holds the gate to the table: without it a row the table declares
// `approve` whose gate asked only for `person` would pass every test here.
func TestRoutesTableAuthCellsMatchHandlers(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	personal := personalTokenHolding(t, database, model.ScopePerson, model.ScopeApprove)
	personOnly := personalTokenHolding(t, database, model.ScopePerson)
	// A probe ends on its route's answer, waited for through the shared wait, whose one bound is
	// the test binary's deadline: a fixed deadline on the request ended a probe whose token read a
	// loaded machine held past it, and the route then answered 5xx where it would have refused.
	probe := func(t *testing.T, route apiRoute, credential string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(route.Method, routeProbePath(route.Pattern), bytes.NewReader([]byte(routeProbeBody(route))))
		request.Header.Set("Content-Type", "application/json")
		switch credential {
		case "cookie":
			request.Header.Set("X-Dispatch-User", "alice")
		case "anonymous":
		case "bearer":
			request.Header.Set("Authorization", "Bearer agent-token")
		default:
			// A personal token's plaintext, presented as the person's own credential.
			request.Header.Set("Authorization", "Bearer "+credential)
		}
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			answered <- response
		}()
		return testwait.Receive(t, fmt.Sprintf("the %s caller's probe of %s %s to answer", credential, route.Method, route.Pattern), answered)
	}
	expectAdmitted := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(t, route, credential)
		if refused, why := authRefusal(response); refused {
			t.Fatalf("%s %s is %q but refuses a %s caller with %s: %s", route.Method, route.Pattern, route.Auth, credential, why, response.Body.String())
		}
	}
	expectRefused := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(t, route, credential)
		if refused, _ := authRefusal(response); !refused {
			t.Fatalf("%s %s is %q but admits a %s caller: %d %s", route.Method, route.Pattern, route.Auth, credential, response.Code, response.Body.String())
		}
	}
	expectScopeRefusal := func(t *testing.T, route apiRoute, credential string) {
		t.Helper()
		response := probe(t, route, credential)
		refused, why := authRefusal(response)
		if !refused || why != "403 "+codeScopeRequired {
			t.Fatalf("%s %s needs %v but answers a %s caller %d %s", route.Method, route.Pattern, route.OAuthScopes, credential, response.Code, response.Body.String())
		}
	}
	for _, route := range (&server{}).routes() {
		t.Run(route.Method+" "+route.Pattern, func(t *testing.T) {
			switch route.Auth {
			case authPublic:
				expectAdmitted(t, route, "anonymous")
			case authAny:
				expectRefused(t, route, "anonymous")
				expectAdmitted(t, route, "cookie")
				if len(route.OAuthScopes) > 0 {
					expectScopeRefusal(t, route, "bearer")
					if slices.Contains(route.OAuthScopes, model.ScopeApprove) {
						expectScopeRefusal(t, route, personOnly)
					}
					expectAdmitted(t, route, personal)
					return
				}
				expectAdmitted(t, route, "bearer")
			case authBearer:
				expectRefused(t, route, "anonymous")
				expectRefused(t, route, "cookie")
				expectAdmitted(t, route, "bearer")
			}
		})
	}
}

// Every route Dispatch refused a bearer token on before INFRA-1017 names the `person` scope now,
// and no other route declares a scope: the routes that declare one are exactly those, read from
// the declarations alone with nothing mounted. The one exception is `PATCH /api/v1/comments/{id}`,
// which declares no scope since INFRA-1523: an agent's comment has an author of kind session, so a
// person scope would shut every author of one out of its own edit, and the route holds the caller
// to that author instead (editComment).
func TestEveryRouteThatWasABrowsersAloneNamesThePersonScope(t *testing.T) {
	t.Parallel()
	browserOnly := []string{
		"GET /api/v1/settings/project-default-assignees",
		"PUT /api/v1/projects/{key}/default-assignee",
		// Turning a project's proven-sessions setting off is INFRA-4764's own row, and the
		// person's as the default assignee above is: it weakens a check, which no credential the
		// deployment configured may do.
		"DELETE /api/v1/projects/{key}/proven-sessions",
		"GET /api/v1/me/agent-tokens",
		"POST /api/v1/me/agent-tokens",
		"DELETE /api/v1/me/agent-tokens/{id}",
		"GET /api/v1/users",
		// A person's own page is no older row either: it reads what one person wrote and what
		// reached them, so a credential that proves no person has no business on it.
		"GET /api/v1/people/{login}/nodes",
		"GET /api/v1/issues/{key}/subscribers",
		"DELETE /api/v1/issues/{key}/subscribers/{session_id}",
		"GET /api/v1/inbox",
		"GET /api/v1/me/answers",
		"GET /api/v1/me/replies",
		"PUT /api/v1/me/threads/{id}/read",
		// The Inbox's Mark all read is no older row either: it writes the person's own read
		// marks, so it is theirs as the two rows above it are.
		"POST /api/v1/me/inbox/read",
		// The Dismiss beside it writes the person's own dismissal marks, so it is theirs too.
		"POST /api/v1/me/inbox/dismiss",
		"GET /api/v1/agents/{session_id}/messages",
		// Which sessions Dispatch holds a record of is this change's own row: it answers only
		// what the conversation read above already tells a caller, so it admits the callers
		// that read admits and needs its scope.
		"GET /api/v1/agents/known",
		// The conversation's files are this change's own row: they are part of the conversation
		// the read above shows, so the list admits the callers that read admits and needs its
		// scope.
		"GET /api/v1/agents/{session_id}/artifacts",
		"POST /api/v1/agents/{session_id}/messages",
		"POST /api/v1/broadcasts",
		"GET /api/v1/broadcasts",
		"GET /api/v1/broadcasts/{id}",
		"POST /api/v1/asks/{id}/answer",
		// A decline is the person's own word that they will not answer, which nobody may give
		// for them: no agent and no deployment credential declines a question on their behalf.
		"POST /api/v1/asks/{id}/decline",
		// Handing an ask to another person is this change's own row, and it is the person's as
		// the answer above it is: Dispatch takes who hands the question on from the credential,
		// and a credential the deployment configured is nobody to hand one on.
		"PUT /api/v1/asks/{id}/asked-of",
		"POST /api/v1/comments/{id}/reopen",
		"POST /api/v1/comments/{id}/accept",
		"POST /api/v1/comments/{id}/reject",
		"GET /api/v1/artifacts/{id}/subscribers",
		"DELETE /api/v1/artifacts/{id}/subscribers/{session_id}",
		"POST /api/v1/artifacts/{id}/rebuild",
		"POST /api/v1/artifacts/{id}/reviews",
		// The review that names the version its caller read is this change's own row, and the
		// person's as the review above it is: it records the same review in the person's name.
		"POST /api/v1/artifacts/{id}/versions/{number}/reviews",
		// The title correction is this change's own row: a document's title is what every
		// reader sees it by, so the person whose credential it is corrects one.
		"PATCH /api/v1/artifacts/{id}",
		// The removal is this change's own row: a document made by mistake is removed by the
		// person whose credential it is, as that person's title correction is.
		"DELETE /api/v1/artifacts/{id}",
		"GET /api/v1/me/state",
		"PUT /api/v1/me/issues/{key}/state",
		"GET /api/v1/me/agents/state",
		"PUT /api/v1/me/agents/{session_id}/state",
		"GET /api/v1/me/documents/{id}/read",
		"PUT /api/v1/me/documents/{id}/read",
		"POST /api/v1/artifacts/{id}/blocks/{block_id}/keep",
		"PUT /api/v1/me/asks/{id}/snooze",
		"DELETE /api/v1/me/asks/{id}/snooze",
		"GET /api/v1/credential-requests",
		"POST /api/v1/credential-requests/{id}/approve",
		"POST /api/v1/credential-requests/{id}/deny",
		// The dismissal of an expired request is this change's own row, and the person's for the
		// reason the decisions above it are: Dispatch names the caller as the one who dismissed.
		"POST /api/v1/credential-requests/{id}/dismiss",
		"POST /api/v1/credential-requests/machine-lookup",
		"GET /api/v1/credential-grants",
		"POST /api/v1/credential-grants/{id}/revoke",
		"GET /api/v1/machine-logins",
		"POST /api/v1/machine-logins/{id}/revoke",
		// The sign-in rows and the capabilities read are this change's own, and are the
		// browser's alone for the reason the credential rows above them are: each names the
		// calling person, whom Dispatch takes from the credential and never from the request.
		"GET /api/v1/signins",
		"POST /api/v1/signins/{id}/revoke",
		"GET /api/v1/credentials/capabilities",
		"POST /api/v1/agents/{session_id}/stream/_test/publish",
		// The scope edit is the one route that was no row at all before INFRA-1017: it is the
		// person's own, as the mint and the revoke beside it are.
		"PATCH /api/v1/me/agent-tokens/{id}",
	}
	for _, name := range browserOnly {
		route, declared := declaredRoute(name)
		if !declared || !slices.Contains(route.OAuthScopes, model.ScopePerson) {
			t.Errorf("%s names %v (declared=%t), want the %s scope among them", name, route.OAuthScopes, declared, model.ScopePerson)
		}
	}
	scoped := 0
	for _, route := range routeRegistry {
		if len(route.OAuthScopes) > 0 {
			scoped++
		}
	}
	if scoped != len(browserOnly) {
		t.Errorf("%d routes declare OAuth scopes, want exactly the %d above", scoped, len(browserOnly))
	}
}
