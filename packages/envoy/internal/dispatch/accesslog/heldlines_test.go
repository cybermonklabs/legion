package accesslog_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/githubapi"
	"github.com/sjawhar/envoy/internal/dispatch/githubpath"
	"github.com/sjawhar/envoy/internal/dispatch/heldpath"
	"github.com/sjawhar/envoy/internal/logtest"
)

// fixtureMux mounts each fixture route behind its access-log line, as Dispatch mounts its own: the
// table's rows by their patterns, the GitHub proxy under its prefix and the unmatched route as the
// catch-all of /api/.
func fixtureMux() *http.ServeMux {
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{}")) }
	for _, route := range []string{
		"GET /api/v1/projects/{key}/artifacts/{slug}",
		"GET /api/v1/issues/{key}/artifacts/{slug}",
		"PUT /api/v1/projects/{key}/architecture-sources/{owner}/{repo}",
		"GET /api/v1/agents/{session_id}/artifacts/{slug}",
		"GET /api/v1/agents/{session_id}/artifacts/{slug}/versions/{number}",
		"GET /api/v1/issues/{key}",
		"GET /api/v1/asks/{id}",
		"GET /ws/doc/{room}",
	} {
		_, pattern, _ := strings.Cut(route, " ")
		mux.HandleFunc(route, accesslog.HandlerFunc(pattern, answer))
	}
	mux.HandleFunc(githubpath.Prefix, accesslog.HandlerFunc(githubpath.Prefix, func(w http.ResponseWriter, r *http.Request) {
		githubapi.ProxyREST(w, r, nil)
	}))
	mux.HandleFunc("/api/", accesslog.HandlerFunc(heldpath.Unmatched, func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, http.StatusNotFound, map[string]string{"code": "NOT_FOUND", "error": "no route for " + r.Method + " " + r.URL.Path})
	}))
	return mux
}

// fixtureRequests are the requests whose lines testdata/lines.before holds.
var fixtureRequests = []string{
	"GET /api/v1/projects/PUB/artifacts/r14-plan",
	"GET /api/v1/issues/PUB-7/artifacts/spec",
	"PUT /api/v1/projects/PUB/architecture-sources/acme/r14-repo",
	"GET /api/v1/agents/session-0123456789abcdef/artifacts/r14-plan",
	"GET /api/v1/agents/session-0123456789abcdef/artifacts/r14-plan/versions/2",
	"GET /api/v1/issues/a%2Fb%20c?q=hidden-query",
	"GET /api/v1/asks/0e5f0c1e-6b1d-4c43-9a51-3c1f0b8b5d2e",
	"GET /ws/doc/7d3c9a62-2f4e-4b8a-9f1c-1e2d3c4b5a69",
	"GET /api/github/rest/r14-plan",
	"GET /api/github/rest/repos/acme/r14-repo/contents/r14-plan",
	"GET /api/github/rest/repos/acme/r14-repo/pulls/7",
	"GET /api/github/rest/repos/acme/r14-repo/actions/runs?head_sha=0123456789abcdef0123456789abcdef01234567",
	"GET /api/v1/no-such-route/r14-plan",
	"GET /api/v1/projects/PUB/artifacts/" + strings.Repeat("x", 300),
}

// fixtureLines serves each request through mux and answers the access-log lines it wrote, one a
// request, with the time dropped and elapsed_ms written as 0, the two fields a run cannot fix.
func fixtureLines(t *testing.T, mux http.Handler, requests []string) []string {
	t.Helper()
	logged := &logtest.Buffer{}
	logtest.SetDefault(t, slog.NewTextHandler(logged, &slog.HandlerOptions{ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		switch attr.Key {
		case slog.TimeKey:
			return slog.Attr{}
		case "elapsed_ms":
			return slog.Int64("elapsed_ms", 0)
		}
		return attr
	}}))
	for _, request := range requests {
		method, target, _ := strings.Cut(request, " ")
		served := httptest.NewRequest(method, target, nil)
		served.Header.Set("User-Agent", "dispatch-cli/1.0")
		served.Header.Set("X-Forwarded-For", "203.0.113.9")
		mux.ServeHTTP(httptest.NewRecorder(), served)
	}
	var lines []string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, `msg="`+accesslog.Message+`"`) {
			lines = append(lines, line)
		}
	}
	if len(lines) != len(requests) {
		t.Fatalf("%d requests wrote %d access-log lines:\n%s", len(requests), len(lines), strings.Join(lines, "\n"))
	}
	return lines
}

// usePrivate makes keys the private set until the test ends, known or not.
func usePrivate(t *testing.T, known bool, keys ...string) {
	t.Helper()
	set := heldpath.NewPrivateSet(keys...)
	heldpath.Use(func() (heldpath.PrivateSet, bool) { return set, known })
	t.Cleanup(func() { heldpath.Use(nil) })
}

// With no private project and the private projects read, every line is the line the merge base
// wrote for the same request (R9): testdata/lines.before holds those, written by this test's own
// requests at the merge base, cybermonklabs/legion 0d3786da53.
func TestTheLineIsTodaysWithNoPrivateProject(t *testing.T) {
	usePrivate(t, true)
	got := fixtureLines(t, fixtureMux(), fixtureRequests)
	before, err := os.ReadFile("testdata/lines.before")
	if err != nil {
		t.Fatalf("read the merge base's lines: %v", err)
	}
	want := strings.Split(strings.TrimSuffix(string(before), "\n"), "\n")
	if len(want) != len(got) {
		t.Fatalf("%d lines, the merge base wrote %d", len(got), len(want))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("request %q writes\n%s\nwhere the merge base wrote\n%s", fixtureRequests[index], got[index], want[index])
		}
	}
}

// A proxy suffix none of the proxy's path classes reads is any text a caller wrote, so while a
// project is private, or the private projects are not known, the line writes it as {path}; one
// a class reads holds a repository's owner and name, a number or a commit, identifiers, and is
// written whole. The proxy answers each as it does today.
func TestAProxySuffixNoClassReadsIsHidden(t *testing.T) {
	hidden := []string{
		"GET /api/github/rest/r14-plan",
		"GET /api/github/rest/repos/acme/r14-repo/contents/r14-plan",
	}
	read := "GET /api/github/rest/repos/acme/r14-repo/pulls/7"
	for _, state := range []struct {
		name  string
		known bool
		keys  []string
		held  bool
	}{
		{name: "PRIV private", known: true, keys: []string{"PRIV"}, held: true},
		{name: "the set not known", known: false, held: true},
		{name: "no project private", known: true},
	} {
		t.Run(state.name, func(t *testing.T) {
			usePrivate(t, state.known, state.keys...)
			mux := fixtureMux()
			lines := fixtureLines(t, mux, append(slices.Clone(hidden), read))
			for index, request := range hidden {
				want := " path=" + strings.TrimPrefix(request, "GET ") + " "
				if state.held {
					want = " path=/api/github/rest/{path} "
				}
				if !strings.Contains(lines[index], want) || (state.held && strings.Contains(lines[index], "r14-plan")) {
					t.Errorf("%s writes %s, want%s", request, lines[index], want)
				}
				answer := httptest.NewRecorder()
				mux.ServeHTTP(answer, httptest.NewRequest(http.MethodGet, strings.TrimPrefix(request, "GET "), nil))
				if answer.Code != http.StatusNotFound || !strings.Contains(answer.Body.String(), `"GITHUB_PATH_REFUSED"`) {
					t.Errorf("%s is answered %d %s, want 404 GITHUB_PATH_REFUSED", request, answer.Code, answer.Body.String())
				}
			}
			if want := " path=" + strings.TrimPrefix(read, "GET ") + " "; !strings.Contains(lines[len(hidden)], want) {
				t.Errorf("%s writes %s, want%s", read, lines[len(hidden)], want)
			}
		})
	}
}

// The walk reads Dispatch's source as written, with no types: every route an access-log line is
// mounted under, and every log call, of each non-test Go file under the directories it is given.
type walk struct {
	// module is the module's directory, which an import path under modulePath names a directory of.
	module string
	fset   *token.FileSet
	// packages holds each walked directory's files.
	packages map[string][]*ast.File
	// findings are what the walk refuses, each as `<file>:<line>: <what>`.
	findings []string
	// counts are what it read, so a walk that read nothing cannot pass.
	routes, logCalls int
	// parameters is each parameter the routes name, with how many routes name it.
	parameters map[string]int
}

const modulePath = "github.com/sjawhar/envoy"

// accesslogPath and heldpathPath are the import paths of the two packages a call is judged by.
const (
	accesslogPath = modulePath + "/internal/dispatch/accesslog"
	heldpathPath  = modulePath + "/internal/dispatch/heldpath"
)

// newWalk parses every non-test Go file under each root, leaving out testdata directories, which
// no build reads, unless a root is one itself.
func newWalk(t *testing.T, module string, roots ...string) *walk {
	t.Helper()
	w := &walk{module: module, fset: token.NewFileSet(), packages: map[string][]*ast.File{}}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" && path != root {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(w.fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			directory, _ := filepath.Abs(filepath.Dir(path))
			w.packages[directory] = append(w.packages[directory], file)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return w
}

// imports names each import of file by the name the file calls it.
func imports(file *ast.File) map[string]string {
	named := map[string]string{}
	for _, spec := range file.Imports {
		path, _ := strconv.Unquote(spec.Path.Value)
		name := path[strings.LastIndexByte(path, '/')+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		named[name] = path
	}
	return named
}

// calls reports whether call is a call of function in the package at path, as file imports it.
func calls(file *ast.File, call *ast.CallExpr, path string, functions ...string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && imports(file)[pkg.Name] == path && (len(functions) == 0 || slices.Contains(functions, selector.Sel.Name))
}

func (w *walk) refuse(at token.Pos, format string, args ...any) {
	position := w.fset.Position(at)
	w.findings = append(w.findings, position.Filename+":"+strconv.Itoa(position.Line)+": "+fmt.Sprintf(format, args...))
}

// constant answers the string values the constant name of the package in directory may hold: one
// for a package constant, and each one a function's own constants of that name hold.
func (w *walk) constant(directory, name string) []string {
	var values []string
	for _, file := range w.packages[directory] {
		ast.Inspect(file, func(node ast.Node) bool {
			declaration, ok := node.(*ast.GenDecl)
			if !ok || declaration.Tok != token.CONST {
				return true
			}
			for _, spec := range declaration.Specs {
				value := spec.(*ast.ValueSpec)
				for index, ident := range value.Names {
					if ident.Name == name && index < len(value.Values) {
						values = append(values, w.strings(directory, file, value.Values[index])...)
					}
				}
			}
			return true
		})
	}
	return values
}

// strings answers the string values expression may hold, read in file of the package in
// directory, or nil when the walk cannot read it.
func (w *walk) strings(directory string, file *ast.File, expression ast.Expr) []string {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		if value, err := strconv.Unquote(expression.Value); err == nil && expression.Kind == token.STRING {
			return []string{value}
		}
	case *ast.Ident:
		return w.constant(directory, expression.Name)
	case *ast.SelectorExpr:
		if pkg, ok := expression.X.(*ast.Ident); ok {
			if path, imported := imports(file)[pkg.Name]; imported && strings.HasPrefix(path, modulePath+"/") {
				return w.constant(filepath.Join(w.module, strings.TrimPrefix(path, modulePath+"/")), expression.Sel.Name)
			}
		}
	case *ast.ParenExpr:
		return w.strings(directory, file, expression.X)
	case *ast.BinaryExpr:
		if expression.Op != token.ADD {
			return nil
		}
		var joined []string
		for _, left := range w.strings(directory, file, expression.X) {
			for _, right := range w.strings(directory, file, expression.Y) {
				joined = append(joined, left+right)
			}
		}
		return joined
	}
	return nil
}

// patterns answers every value the Pattern field takes in the package in directory: in each
// composite literal of a struct type the package declares with that field, written by name or in
// its place, the element literals of a slice or array of such a struct included.
func (w *walk) patterns(directory string) []string {
	// at is each struct type's place of its Pattern field among its fields, as a literal lists them.
	at := map[string]int{}
	for _, file := range w.packages[directory] {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if structure, ok := spec.Type.(*ast.StructType); ok {
				place := 0
				for _, field := range structure.Fields.List {
					if len(field.Names) == 0 {
						// An embedded field is one place.
						place++
						continue
					}
					for _, name := range field.Names {
						if name.Name == "Pattern" {
							at[spec.Name.Name] = place
						}
						place++
					}
				}
			}
			return true
		})
	}
	var values []string
	var literal func(file *ast.File, lit *ast.CompositeLit, elided string)
	literal = func(file *ast.File, lit *ast.CompositeLit, elided string) {
		typeName, element := elided, ""
		switch shape := lit.Type.(type) {
		case *ast.Ident:
			typeName = shape.Name
		case *ast.ArrayType:
			typeName = ""
			if ident, ok := shape.Elt.(*ast.Ident); ok {
				element = ident.Name
			}
		}
		place, holds := at[typeName]
		for index, item := range lit.Elts {
			value := item
			if pair, ok := item.(*ast.KeyValueExpr); ok {
				value = pair.Value
				if key, ok := pair.Key.(*ast.Ident); ok && holds && key.Name == "Pattern" {
					values = append(values, w.strings(directory, file, value)...)
				}
			} else if holds && index == place {
				values = append(values, w.strings(directory, file, value)...)
			}
			if inner, ok := value.(*ast.CompositeLit); ok {
				literal(file, inner, element)
			}
		}
	}
	for _, file := range w.packages[directory] {
		ast.Inspect(file, func(node ast.Node) bool {
			if lit, ok := node.(*ast.CompositeLit); ok {
				literal(file, lit, "")
				return false
			}
			return true
		})
	}
	return values
}

// checkRoute refuses a route that names a parameter the path rule does not class, unless it is
// one of the two routes that are no pattern of names: the GitHub proxy's prefix and the unmatched
// route.
func (w *walk) checkRoute(at token.Pos, route string) {
	w.routes++
	if route == githubpath.Prefix || route == heldpath.Unmatched {
		return
	}
	if !strings.HasPrefix(route, "/") {
		w.refuse(at, "route %q is no pattern", route)
		return
	}
	for rest := route; ; {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			w.refuse(at, "route %q has an unclosed parameter", route)
			return
		}
		name := strings.TrimSuffix(rest[open+1:open+end], "...")
		if name != "$" {
			if w.parameters == nil {
				w.parameters = map[string]int{}
			}
			w.parameters[name]++
		}
		if _, classed := heldpath.Parameter(name); name != "$" && !classed {
			w.refuse(at, "route %q names {%s}, which heldpath.Parameter does not class", route, name)
		}
		rest = rest[open+end+1:]
	}
}

// requestPathReads are the fields and methods of a request, and of its URL, that hold the path or
// the query a caller wrote.
var requestPathReads = []string{"Path", "RawPath", "EscapedPath", "RequestURI", "String", "RawQuery"}

// loggerMethods are the *slog.Logger methods that write a record or carry attributes into one.
var loggerMethods = []string{"Debug", "Info", "Warn", "Error", "Log", "LogAttrs", "DebugContext", "InfoContext", "WarnContext", "ErrorContext", "With"}

// readsRequestPath answers the first read of a request's path or query in expression outside a
// call of heldpath.Of, or nil.
func readsRequestPath(file *ast.File, expression ast.Node) ast.Node {
	var found ast.Node
	ast.Inspect(expression, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok && calls(file, call, heldpathPath, "Of") {
			return false
		}
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if inner, ok := selector.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "URL" && slices.Contains(requestPathReads, selector.Sel.Name) {
			found = node
		} else if selector.Sel.Name == "RequestURI" {
			found = node
		}
		return found == nil
	})
	return found
}

// check walks every file: each access-log mount's route, and each log call's arguments.
func (w *walk) check(t *testing.T) {
	t.Helper()
	for directory, files := range w.packages {
		for _, file := range files {
			named := imports(file)
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if calls(file, call, accesslogPath, "Handler", "HandlerFunc") && len(call.Args) > 0 {
					route := call.Args[0]
					var routes []string
					if selector, ok := route.(*ast.SelectorExpr); ok && selector.Sel.Name == "Pattern" {
						if pkg, ok := selector.X.(*ast.Ident); !ok || named[pkg.Name] == "" {
							routes = w.patterns(directory)
						}
					}
					if routes == nil {
						routes = w.strings(directory, file, route)
					}
					if len(routes) == 0 {
						w.refuse(call.Pos(), "an access-log route the walk cannot read")
					}
					for _, value := range routes {
						w.checkRoute(call.Pos(), value)
					}
				}
				logs := calls(file, call, "log/slog") || calls(file, call, "log")
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && !logs && slices.Contains(loggerMethods, selector.Sel.Name) {
					if pkg, ok := selector.X.(*ast.Ident); !ok || named[pkg.Name] == "" {
						logs = true
					}
				}
				if logs {
					w.logCalls++
					for _, argument := range call.Args {
						if read := readsRequestPath(file, argument); read != nil {
							w.refuse(read.Pos(), "a log call writes a request's path or query other than through heldpath.Of")
						}
					}
				}
				return true
			})
		}
	}
}

// Every route an access-log line is mounted under, written as a literal or as the Pattern field
// of the table it is mounted from in a loop, names only parameters the path rule classes, or is
// the GitHub proxy's prefix or the unmatched route; and no log call of Dispatch reads a request's
// path or query but through heldpath.Of. A value built from the path before the call, as the
// proxy's target is, and an error that carries one, as githubapp's are, are outside what a walk
// with no types follows: TestTheProxysWarningsHoldTheCallersQuery holds those.
func TestEveryRouteAndPathIsHeld(t *testing.T) {
	module, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := newWalk(t, module, filepath.Join(module, "internal", "dispatch"), filepath.Join(module, "cmd", "dispatch"))
	dispatch.check(t)
	// A walk that read no route or no log call has proved nothing: Dispatch mounts more than a
	// hundred routes and logs in hundreds of places, so a count this low is a walk gone blind.
	if dispatch.routes < 100 || dispatch.logCalls < 100 {
		t.Fatalf("UNKNOWN: the walk read %d routes and %d log calls of %d packages; it cannot see Dispatch's source", dispatch.routes, dispatch.logCalls, len(dispatch.packages))
	}
	t.Logf("the walk read %d routes and %d log calls in %d packages; the routes name %v", dispatch.routes, dispatch.logCalls, len(dispatch.packages), dispatch.parameters)
	for _, finding := range dispatch.findings {
		t.Error(finding)
	}

	// The control: the same walk over testdata/unheld names its two routes with an unclassed
	// parameter and its log call of a request's path.
	control := newWalk(t, module, filepath.Join("testdata", "unheld"))
	control.check(t)
	want := []string{
		`route "/api/v1/x/{label}" names {label}`,
		`route "/api/v1/y/{label}/z" names {label}`,
		"a log call writes a request's path or query",
	}
	if len(control.findings) != len(want) {
		t.Fatalf("the control's findings are\n%s\nwant one naming each of\n%s", strings.Join(control.findings, "\n"), strings.Join(want, "\n"))
	}
	for _, finding := range want {
		if !slices.ContainsFunc(control.findings, func(got string) bool { return strings.Contains(got, finding) }) {
			t.Errorf("the control's findings\n%s\nname nothing of %q", strings.Join(control.findings, "\n"), finding)
		}
	}
}
