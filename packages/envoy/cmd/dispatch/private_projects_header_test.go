package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/readclass"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// wildcard is a pattern's wildcard, which a request fills with NOPE.
var wildcard = regexp.MustCompile(`\{[^}]*\}`)

// requestOf is the method and path a request to pattern makes: a pattern with no method is a GET,
// and every wildcard reads NOPE.
func requestOf(pattern string) (method, path string) {
	method, path = http.MethodGet, pattern
	if before, after, found := strings.Cut(pattern, " "); found {
		method, path = before, after
	}
	return method, wildcard.ReplaceAllString(path, "NOPE")
}

// Every Dispatch HTTP response, errors and /healthz included, carries Dispatch-Private-Projects:
// 1 when DISPATCH_PRIVATE_PROJECTS is non-empty or the database holds a private project, and 0
// otherwise. The process's handler is built as main builds it - the router, routes.Process's
// mounts, then serveHandler's wrap - and every route the router serves is requested with no
// credential: each row of the route index, each pattern readclass.Handle mounted in the process,
// and both sockets once more as upgrades: /ws/live's 101 carries it, and /ws/doc/NOPE, a room no
// document holds, is refused before any upgrade (refuseHiddenRoom), so that refusal carries it.
func TestEveryResponseSaysWhetherAnyProjectIsPrivate(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name, default_assignee) values ('PRIV', 'Private', 'alice@example.com')`); err != nil {
		t.Fatalf("seed the project PRIV: %v", err)
	}
	cookie, err := http.ParseSetCookie(auth.IssueSessionCookie("alice@example.com", 0, testSigningKey, true))
	if err != nil {
		t.Fatalf("issue alice's session cookie: %v", err)
	}
	for _, state := range []struct {
		name       string
		list       []string
		makePublic bool
		want       string
	}{
		{"the list [PRIV]", []string{"PRIV"}, true, "1"},
		{"an empty list and no private project", nil, true, "0"},
		{"an empty list and PRIV private", nil, false, "1"},
	} {
		t.Run(state.name, func(t *testing.T) {
			visibility := "private"
			if state.makePublic {
				visibility = "public"
			}
			if _, err := database.Pool.Exec(ctx, `update projects set visibility = $1 where key = 'PRIV'`, visibility); err != nil {
				t.Fatalf("make PRIV %s: %v", visibility, err)
			}
			privateState, err := startPrivateProjectsState(t.Context(), database.Pool, state.list)
			if err != nil {
				t.Fatalf("the first read of the private projects: %v", err)
			}
			broker := events.NewBroker()
			router := routerFor(t, resolveWith(t, nil), routes.AppContextOptions{
				Store: database, Events: broker, Docs: docs.New(docs.Deps{Store: database, Events: broker}),
			})
			served := serveHandler(routes.Process(router, healthzHandler(database, nil, nil, nil, "", nil, issuers{}, privateState, nil)), privateState)
			server := httptest.NewServer(served)
			t.Cleanup(server.Close)
			client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			ask := func(method, path string) *http.Response {
				t.Helper()
				request, err := http.NewRequest(method, server.URL+path, nil)
				if err != nil {
					t.Fatalf("build %s %s: %v", method, path, err)
				}
				response, err := client.Do(request)
				if err != nil {
					t.Fatalf("%s %s: %v", method, path, err)
				}
				_ = response.Body.Close()
				return response
			}

			requests := map[string]bool{}
			var body struct {
				Routes []struct{ Method, Path string } `json:"routes"`
			}
			// The route index needs a credential, as every route but sign-in, health and the web
			// app does; the routes it names are then requested with none.
			indexRequest, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1", nil)
			if err != nil {
				t.Fatalf("build GET /api/v1: %v", err)
			}
			indexRequest.Header.Set("Authorization", "Bearer agent-token")
			index, err := client.Do(indexRequest)
			if err != nil {
				t.Fatalf("GET /api/v1: %v", err)
			}
			if err := json.NewDecoder(index.Body).Decode(&body); err != nil || len(body.Routes) == 0 {
				t.Fatalf("read the route index (%d): %v, %d routes", index.StatusCode, err, len(body.Routes))
			}
			_ = index.Body.Close()
			for _, row := range body.Routes {
				requests[row.Method+" "+row.Path] = true
			}
			for _, pattern := range readclass.AllPatterns() {
				requests[pattern] = true
			}
			for _, required := range []string{"GET /healthz", "GET /livez", "GET /metrics", "GET /ws/live", "GET /ws/doc/{room}"} {
				if !requests[required] {
					t.Fatalf("the walk holds no request for %s; it walks %d", required, len(requests))
				}
			}
			for _, pattern := range slices.Sorted(func(yield func(string) bool) {
				for pattern := range requests {
					if !yield(pattern) {
						return
					}
				}
			}) {
				method, path := requestOf(pattern)
				if got := ask(method, path).Header.Get(access.PrivateProjectsHeader); got != state.want {
					t.Errorf("%s %s answered %s: %q, want %q", method, path, access.PrivateProjectsHeader, got, state.want)
				}
			}

			socket := "ws" + strings.TrimPrefix(server.URL, "http")
			live, upgraded, err := gws.DefaultDialer.Dial(socket+"/ws/live", http.Header{"Authorization": {"Bearer agent-token"}})
			if err != nil {
				t.Fatalf("open /ws/live: %v (%v)", err, upgraded)
			}
			_ = live.Close()
			if got := upgraded.Header.Get(access.PrivateProjectsHeader); upgraded.StatusCode != http.StatusSwitchingProtocols || got != state.want {
				t.Errorf("/ws/live's upgrade answered %d with %s %q, want 101 and %q", upgraded.StatusCode, access.PrivateProjectsHeader, got, state.want)
			}
			document, answered, err := gws.DefaultDialer.Dial(socket+"/ws/doc/NOPE", http.Header{"Cookie": {cookie.String()}})
			if err == nil {
				_ = document.Close()
			}
			if answered == nil {
				t.Fatalf("/ws/doc/NOPE as an upgrade answered nothing: %v", err)
			}
			if got := answered.Header.Get(access.PrivateProjectsHeader); got != state.want {
				t.Errorf("/ws/doc/NOPE as an upgrade answered %d with %s %q, want %q", answered.StatusCode, access.PrivateProjectsHeader, got, state.want)
			}
		})
	}
}

// Every call of newHTTPServer takes, as its handler, a variable assigned from serveHandler(…),
// and no other http.Server is built in cmd/dispatch but newHTTPServer's own and pprof.go's, the
// localhost listener that serves no Dispatch response. So no route can be registered outside the
// response middleware without this test naming it. A walk that finds no newHTTPServer call, or
// not its http.Server, read nothing it can vouch for, and fails as such.
func TestTheServerServesOnlyTheWrappedHandler(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	set := token.NewFileSet()
	var outside []string
	var serves, servers int
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(set, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		wrapped := map[string]bool{}
		ast.Inspect(file, func(node ast.Node) bool {
			if assign, ok := node.(*ast.AssignStmt); ok && len(assign.Lhs) == 1 && len(assign.Rhs) == 1 {
				if call, ok := assign.Rhs[0].(*ast.CallExpr); ok && calls(call, "serveHandler") {
					if variable, ok := assign.Lhs[0].(*ast.Ident); ok {
						wrapped[variable.Name] = true
					}
				}
			}
			return true
		})
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				if calls(node, "newHTTPServer") {
					serves++
					if variable, ok := node.Args[0].(*ast.Ident); !ok || !wrapped[variable.Name] {
						outside = append(outside, set.Position(node.Pos()).String()+": newHTTPServer serves a handler no serveHandler call assigned")
					}
				}
			case *ast.CompositeLit:
				if selector, ok := node.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Server" {
					if pkg, ok := selector.X.(*ast.Ident); ok && pkg.Name == "http" {
						servers++
						if !allowedServer(file, name, node) {
							outside = append(outside, set.Position(node.Pos()).String()+": an http.Server outside newHTTPServer")
						}
					}
				}
			}
			return true
		})
	}
	if serves == 0 || servers == 0 {
		t.Fatalf("the walk of %d files found %d newHTTPServer calls and %d http.Server literals: a renamed or moved server leaves it nothing to check", len(files), serves, servers)
	}
	if len(outside) > 0 {
		t.Fatalf("a Dispatch response could leave without Dispatch-Private-Projects:\n%s", strings.Join(outside, "\n"))
	}
}

// calls reports whether call calls the function named name.
func calls(call *ast.CallExpr, name string) bool {
	function, ok := call.Fun.(*ast.Ident)
	return ok && function.Name == name
}

// allowedServer reports whether server, an http.Server literal of file name, is one of the two
// this package may build: newHTTPServer's, which serves the wrapped handler, and pprof.go's,
// which listens on localhost alone and serves no Dispatch response.
func allowedServer(file *ast.File, name string, server *ast.CompositeLit) bool {
	if name == "pprof.go" {
		return true
	}
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "newHTTPServer" &&
			function.Pos() <= server.Pos() && server.End() <= function.End() {
			return true
		}
	}
	return false
}

// failingQuerier answers every read with an error, as a database that went away does.
type failingQuerier struct{}

var errReadFailed = errors.New("the database went away")

func (failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errReadFailed
}

func (failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return failedRow{} }

type failedRow struct{}

func (failedRow) Scan(...any) error { return errReadFailed }

// A state's later read that fails answers 1, whatever it read before: the closed direction, which
// a reader of the header takes as a deployment holding a private project.
func TestAFailedReadSaysPrivate(t *testing.T) {
	database := storetest.Open(t)
	state, err := startPrivateProjectsState(t.Context(), database.Pool, nil)
	if err != nil {
		t.Fatalf("the first read over no private project: %v", err)
	}
	if got := state.header(); got != "0" {
		t.Fatalf("a state over no private project answers %q, want 0", got)
	}
	state.querier = failingQuerier{}
	state.read(context.Background())
	if got := state.header(); got != "1" {
		t.Fatalf("a state whose read failed answers %q, want 1", got)
	}
}

// The state's first read, at boot, does not fail open: a read that fails answers its error and no
// state, so main logs "dispatch: read the private projects" at ERROR and exits 1, as it does when
// the database will not open, and no 30 s loop runs.
func TestTheFirstPrivateReadFailingStopsTheBoot(t *testing.T) {
	for _, list := range [][]string{nil, {"PRIV"}} {
		state, err := startPrivateProjectsState(t.Context(), failingQuerier{}, list)
		if !errors.Is(err, errReadFailed) || state != nil {
			t.Errorf("the first read with the list %v over a database whose read fails = %v, %v; want the read's error and no state", list, state, err)
		}
	}
}
