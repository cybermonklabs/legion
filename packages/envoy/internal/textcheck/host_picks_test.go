package textcheck

//ci:reads packages/envoy/internal/dispatch/routes/*.go !**/*_test.go TestNoCodeOutsideTheOriginTablePicksByHost goFiles parses ../dispatch/routes's own Go files, its test files aside

import (
	"go/ast"
	"go/token"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// hostPickPackages are the packages whose code answers a request by the address it came to, from
// this directory: Dispatch's router, its API, and the broker token check the API mounts.
var hostPickPackages = []string{"../dispatch/routes", "../dispatch/api", "../ciphertoken"}

// originTable is the router's one table of the origins it picks among by a request's Host
// (routes.hostOrigins): its methods are the one place a request's Host is read.
const (
	originTablePackage = "../dispatch/routes"
	originTable        = "hostOrigins"
)

// hostPickExclusions are the declarations outside the table that read a request's Host, each with
// why it stays out of it.
var hostPickExclusions = map[string]string{
	"../dispatch/routes/devsignin.go requireHost": "the development sign-in fence answers 421 to every Host but the dashboard origin's host and port and picks no origin, " +
		"and boot refuses DISPATCH_SERVED_ORIGINS and DISPATCH_FORMER_ORIGINS beside DISPATCH_DEV_SIGNIN, so no request it lets through came to another address",
}

// A request is answered by the address it came to through one table, routes.hostOrigins: the
// canonical origin, the served and the former addresses, each matched as the table matches it
// (weborigin.SameHost for a served address, weborigin.SameHostName for a former one). A function
// of the router, the API or the broker token check that reads a request's Host itself compares it
// with a configured origin outside that table, and can answer a served or a former address as the
// table does not. Each one fails here, named by its file and declaration, except the ones
// hostPickExclusions names with their reason.
func TestNoCodeOutsideTheOriginTablePicksByHost(t *testing.T) {
	reads, read, err := hostReads(hostPickPackages...)
	if err != nil {
		t.Fatal(err)
	}
	if read == 0 {
		t.Fatal("read no Go file: the check cannot see the router, the API or the broker token check")
	}
	var picks []string
	excluded := map[string]bool{}
	for _, found := range reads {
		file, declaration, _ := strings.Cut(found, " ")
		switch {
		case path.Dir(file) == originTablePackage &&
			(strings.HasPrefix(declaration, "("+originTable+").") || strings.HasPrefix(declaration, "(*"+originTable+").")):
		case hostPickExclusions[found] != "":
			excluded[found] = true
		default:
			picks = append(picks, found)
		}
	}
	if len(picks) > 0 {
		t.Errorf("%d declarations read a request's Host outside the origin table (routes.%s):\n\t%s\n"+
			"pick the request's origin with a method of routes.%s, which reads the canonical, served and former addresses as one",
			len(picks), originTable, strings.Join(picks, "\n\t"), originTable)
	}
	for member, reason := range hostPickExclusions {
		if !excluded[member] {
			t.Errorf("hostPickExclusions names %s (%s), which reads no request's Host now: take it off the list", member, reason)
		}
	}

	// The check sees a read of a request's Host whatever name the file imports net/http under, in a
	// method and in a function literal, and passes over the Host of a URL, which is no request's.
	t.Run("the check sees what it guards", func(t *testing.T) {
		probe := "package probe\n\nimport web \"net/http\"\n\ntype hostOrigins struct{}\n\n" +
			"func (hostOrigins) pick(r *web.Request) string { return r.Host }\n\n" +
			"func fence(next web.Handler) web.Handler {\n" +
			"\treturn web.HandlerFunc(func(w web.ResponseWriter, req *web.Request) {\n\t\t_ = req.Host\n\t\tnext.ServeHTTP(w, req)\n\t})\n}\n\n" +
			"func target(r *web.Request) string { return r.URL.Host }\n"
		file := writeFile(t, "probe.go", probe)
		got, read, err := hostReads(filepath.Dir(file))
		if err != nil {
			t.Fatal(err)
		}
		at := filepath.ToSlash(file)
		if want := []string{at + " (hostOrigins).pick", at + " fence"}; read != 1 || !slices.Equal(got, want) {
			t.Errorf("the check read %d files and found %q, want 1 file and %q", read, got, want)
		}
	})
}

// hostReads reads the non-test Go files of each of directories, not of the directories below it
// (goFiles), and returns, sorted, each top-level declaration (declName) that reads the Host of a
// request it is handed, as "<path> <declaration>": a selector .Host on a parameter of type
// *http.Request, of the declaration or of a function literal in it, whatever name its file imports
// net/http under. It counts the Go files it read.
func hostReads(directories ...string) (reads []string, read int, err error) {
	read, err = goFiles(directories, true, func(at string, file *ast.File, _ *token.FileSet) {
		http := importNames(file, "net/http")
		for _, decl := range file.Decls {
			if readsRequestHost(decl, http) {
				reads = append(reads, filepath.ToSlash(at)+" "+declName(decl))
			}
		}
	})
	if err != nil {
		return nil, read, err
	}
	slices.Sort(reads)
	return reads, read, nil
}

// readsRequestHost reports whether decl reads .Host of a parameter, of decl or of a function
// literal in it, whose type is *http.Request: http are the names its file imports net/http under
// (importNames), "." among them where it imports it with a dot, as Request bare.
func readsRequestHost(decl ast.Decl, http map[string]bool) bool {
	requests := map[string]bool{}
	ast.Inspect(decl, func(node ast.Node) bool {
		if function, ok := node.(*ast.FuncType); ok && function.Params != nil {
			for _, field := range function.Params.List {
				if isRequestPointer(field.Type, http) {
					for _, name := range field.Names {
						requests[name.Name] = true
					}
				}
			}
		}
		return true
	})
	found := false
	ast.Inspect(decl, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "Host" {
			if request, ok := selector.X.(*ast.Ident); ok && requests[request.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// isRequestPointer reports whether expression is the type *http.Request, under one of http, the
// names its file imports net/http under, or bare where those hold ".", a dot import.
func isRequestPointer(expression ast.Expr, http map[string]bool) bool {
	pointer, ok := expression.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch target := pointer.X.(type) {
	case *ast.SelectorExpr:
		pkg, ok := target.X.(*ast.Ident)
		return ok && http[pkg.Name] && target.Sel.Name == "Request"
	case *ast.Ident:
		return http["."] && target.Name == "Request"
	}
	return false
}
