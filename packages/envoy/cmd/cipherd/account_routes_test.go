// account_routes_test.go holds account_powers_test.go's rows to the broker's route table: every
// route the table authenticates with accountAuth has a row there, and every row names a route the
// table has. The rows are written by hand, so without this a new named-caller route would pass
// the powers walk unchecked. It reads the table as source, as cmd/cipher-refgen does, since
// routes() is internal/cipher/api's own and unexported.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// routesTable is the broker's route table, relative to this package's directory, where go test
// runs it.
var routesTable = filepath.Join("..", "..", "internal", "cipher", "api", "routes_table.go")

// TestEveryAccountRouteHasARow: the set of "<method> <pattern>" the route table authenticates with
// accountAuth equals the set accountRoutes names, so a route added to the table without a row, and
// a row left behind by a route the table no longer has, each fail here naming it.
func TestEveryAccountRouteHasARow(t *testing.T) {
	table := accountAuthRoutes(t, routesTable)
	if len(table) == 0 {
		t.Fatalf("%s yields no accountAuth route at all: this test read nothing, so it can say nothing", routesTable)
	}
	rows := map[string]bool{}
	for _, route := range accountRoutes {
		rows[route.method+" "+route.pattern] = true
	}
	for _, route := range slices.Sorted(maps.Keys(table)) {
		if !rows[route] {
			t.Errorf("%s authenticates %s with accountAuth, and accountRoutes (account_powers_test.go) has no row for it: add one naming its caller and its own answer", routesTable, route)
		}
	}
	for _, route := range slices.Sorted(maps.Keys(rows)) {
		if !table[route] {
			t.Errorf("accountRoutes has a row for %s, which %s does not authenticate with accountAuth", route, routesTable)
		}
	}
}

// accountAuthRoutes is every "<method> <pattern>" of a row of path's routes() whose handler is an
// accountAuth call. A row is read in either form a Go composite literal takes: positional,
// {http.Method<X>, "<pattern>", accountAuth(…)}, or with field names, {Method: http.Method<X>,
// Pattern: "<pattern>", Handler: accountAuth(…)}. Every accountAuth call in routes() must then be
// the handler of a row it read: a call in any other shape fails t, naming its line, so the set is
// never short of a route it skipped.
func accountAuthRoutes(t *testing.T, path string) map[string]bool {
	t.Helper()
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "routes" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatalf("%s declares no func routes()", path)
	}
	routes := map[string]bool{}
	read := map[*ast.CallExpr]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		row, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		method, pattern, handler := rowFields(row)
		call, ok := handler.(*ast.CallExpr)
		if !ok || !isAccountAuth(call) {
			return true
		}
		selector, methodOK := method.(*ast.SelectorExpr)
		literal, patternOK := pattern.(*ast.BasicLit)
		if !methodOK || !patternOK || literal.Kind != token.STRING || !strings.HasPrefix(selector.Sel.Name, "Method") {
			t.Fatalf("%s: an accountAuth row this test cannot read (want its method as http.Method<X> and its pattern as a string literal)", files.Position(row.Pos()))
		}
		text, err := strconv.Unquote(literal.Value)
		if err != nil {
			t.Fatalf("%s: the pattern %s: %v", files.Position(literal.Pos()), literal.Value, err)
		}
		routes[httpMethod(t, selector.Sel.Name)+" "+text] = true
		read[call] = true
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && isAccountAuth(call) && !read[call] {
			t.Fatalf("%s: an accountAuth call that is no row's handler, in a shape this test cannot read: write the row {http.Method<X>, \"<pattern>\", accountAuth(…)} or with the field names Method, Pattern and Handler", files.Position(call.Pos()))
		}
		return true
	})
	return routes
}

// rowFields is a route row's method, pattern and handler, from three positional elements or from
// the field names Method, Pattern and Handler; nil for any it does not find.
func rowFields(row *ast.CompositeLit) (method, pattern, handler ast.Expr) {
	if len(row.Elts) == 3 {
		if _, keyed := row.Elts[0].(*ast.KeyValueExpr); !keyed {
			return row.Elts[0], row.Elts[1], row.Elts[2]
		}
	}
	for _, element := range row.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := field.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Method":
			method = field.Value
		case "Pattern":
			pattern = field.Value
		case "Handler":
			handler = field.Value
		}
	}
	return method, pattern, handler
}

// isAccountAuth reports whether call is a call of accountAuth.
func isAccountAuth(call *ast.CallExpr) bool {
	name, ok := call.Fun.(*ast.Ident)
	return ok && name.Name == "accountAuth"
}

// httpMethod is the method net/http's Method<X> constant named by name spells.
func httpMethod(t *testing.T, name string) string {
	t.Helper()
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace} {
		if strings.EqualFold("Method"+method, name) {
			return method
		}
	}
	t.Fatalf("the route table names http.%s, which is no net/http method constant", name)
	return ""
}
