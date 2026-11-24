package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
)

// The old layout's files and the names its route facts stood under, in package api.
const (
	tableFile       = "routes_table.go"
	shapesFile      = "routes_shapes.go"
	readClassesFile = "read_classes.go"

	writeClassesName    = "writeClasses"
	idRoutesName        = "idRoutes"
	routeScopesName     = "routeScopes"
	readScopeRoutesName = "readScopeRoutes"
	routeShapesName     = "routeShapes"
	readClassesName     = "readClasses"

	// hooksCondition is the one condition of routes() whose rows are the test hooks', mounted
	// only where Deps.TestHooksEnabled is set. readRows refuses a row under any other condition,
	// and sharedCode cuts the rows of this one alone.
	hooksCondition = "s.deps.TestHooksEnabled"
)

// tree is the files of one revision of package api: a commit read through git, or a directory.
type tree interface {
	// read answers the bytes of one file of the package, by its base name.
	read(name string) ([]byte, error)
	// filesNaming answers the base names of the package's Go files whose text holds word.
	filesNaming(word string) ([]string, error)
}

// oldRoute is everything the old layout said of one route: its row of routes() and its entry in
// each side map. Each expression is the source text it was written as, so a description built
// with fmt.Sprintf or a shared constant keeps being built that way.
type oldRoute struct {
	method  string // GET, POST, ...
	pattern string // the pattern's value, which names the route's file
	key     string // "<method> <pattern>"

	methodExpr, patternExpr, authExpr, descriptionExpr string
	handler                                            string // the method of *server that serves it
	testHook                                           bool

	writeClass, id, scopes, shape, read fact
	readScope                           bool

	// packages are the import names the route's expressions qualify an identifier with.
	packages map[string]bool
}

// fact is one side map's entry for a route: its value's source text, empty where the map holds
// no entry, and the comments the map wrote above it.
type fact struct {
	value    string
	comments []string
}

// splitOldLayout reads the old layout of src - the rows of routes() and the side maps beside them
// in routes_table.go and routes_shapes.go - and answers one route file per route, by file name.
// It fails on a fact it cannot place: a side-map entry no row has, a row two rows share, a comment
// between rows, or a pattern it cannot read the value of.
func splitOldLayout(src tree) (map[string][]byte, error) {
	routes, imports, err := readOldLayout(src)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(routes))
	for _, route := range routes {
		name, err := routeFileName(route.method, route.pattern)
		if err != nil {
			return nil, err
		}
		if _, taken := files[name]; taken {
			return nil, fmt.Errorf("%s: two routes are named %s", route.key, name)
		}
		content, err := renderRoute(route, imports)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", route.key, err)
		}
		files[name] = content
	}
	return files, nil
}

// hasOldLayout reports whether src's routes_table.go still holds the table the side maps stand
// beside: a []routeRow literal in routes().
func hasOldLayout(src tree) (bool, error) {
	content, err := src.read(tableFile)
	if err != nil {
		return false, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, tableFile, content, parser.SkipObjectResolution)
	if err != nil {
		return false, err
	}
	body := routesBody(file)
	if body == nil {
		return false, nil
	}
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if literal, ok := node.(*ast.CompositeLit); ok && isRowSlice(literal.Type) {
			found = true
		}
		return !found
	})
	return found, nil
}

// readOldLayout parses the old layout's files and joins every route's row to its side-map
// entries. It answers the routes in the table's order and the import path of every package name
// those files import. The table and the shapes are required; read_classes.go is #4557's and a
// branch made before it has none.
func readOldLayout(src tree) ([]*oldRoute, map[string]string, error) {
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	sources := map[string][]byte{}
	for _, name := range []string{tableFile, shapesFile, readClassesFile} {
		content, err := src.read(name)
		if err != nil {
			if name == readClassesFile && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, nil, err
		}
		file, err := parser.ParseFile(fset, name, content, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, nil, err
		}
		parsed[name], sources[name] = file, content
	}
	imports := map[string]string{}
	for _, file := range parsed {
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return nil, nil, err
			}
			name := path.Base(importPath)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if known, taken := imports[name]; taken && known != importPath {
				return nil, nil, fmt.Errorf("%s and %s are both imported as %s", known, importPath, name)
			}
			imports[name] = importPath
		}
	}
	text := func(name string, node ast.Node) string {
		return string(sources[name][fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
	}

	routes, err := readRows(fset, parsed[tableFile], func(node ast.Node) string { return text(tableFile, node) }, imports, src)
	if err != nil {
		return nil, nil, err
	}
	byKey := make(map[string]*oldRoute, len(routes))
	for _, route := range routes {
		if _, twice := byKey[route.key]; twice {
			return nil, nil, fmt.Errorf("%s: routes() holds the route twice", route.key)
		}
		byKey[route.key] = route
	}

	// A key written as a package constant, such as read_classes.go's whoamiRow.
	constants := stringConstants(parsed)
	constantOf := func(name string) (string, bool) {
		if text, known := constants[name]; known {
			return text, known
		}
		// A constant another file of the package declares, such as the going catalogue's path.
		text, err := packageStringConstant(src, name)
		if err != nil {
			return "", false
		}
		constants[name] = text
		return text, true
	}
	maps := []struct {
		file, name string
		into       func(*oldRoute) *fact
	}{
		{tableFile, writeClassesName, func(route *oldRoute) *fact { return &route.writeClass }},
		{tableFile, idRoutesName, func(route *oldRoute) *fact { return &route.id }},
		{tableFile, routeScopesName, func(route *oldRoute) *fact { return &route.scopes }},
		{shapesFile, routeShapesName, func(route *oldRoute) *fact { return &route.shape }},
		// #4557 moved the read from a list beside the table into a map of its own file. A branch
		// made before it has the list and no map, and one made after has the map and no list, so
		// each is read where it is and neither is required.
		{readClassesFile, readClassesName, func(route *oldRoute) *fact { return &route.read }},
	}
	for _, side := range maps {
		file, read := parsed[side.file]
		if !read && side.file == readClassesFile {
			continue
		}
		literal := packageLiteral(file, side.name)
		if literal == nil {
			if side.file == readClassesFile {
				return nil, nil, fmt.Errorf("%s holds no %s: this reader knows the read as a list in %s (before #4557) or as that map, and this branch has neither", readClassesFile, readClassesName, tableFile)
			}
			return nil, nil, fmt.Errorf("%s declares no %s", side.file, side.name)
		}
		entries, err := mapEntries(fset, file, literal, func(node ast.Node) string { return text(side.file, node) }, constantOf)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", side.name, err)
		}
		for _, entry := range entries {
			route, held := byKey[entry.key]
			if !held {
				return nil, nil, fmt.Errorf("%s names %q, which no row of routes() is", side.name, entry.key)
			}
			*side.into(route) = entry.fact
			qualifiers(entry.expr, imports, route.packages)
		}
	}

	// readScopeRoutes is the pre-#4557 spelling of the read: a list of the routes a read-only
	// token may call, which is now the map above. A branch that holds neither states no read.
	list := packageLiteral(parsed[tableFile], readScopeRoutesName)
	if list == nil {
		return routes, imports, nil
	}
	for _, element := range list.Elts {
		key, err := stringValue(element)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", readScopeRoutesName, err)
		}
		route, held := byKey[key]
		if !held {
			return nil, nil, fmt.Errorf("%s names %q, which no row of routes() is", readScopeRoutesName, key)
		}
		route.readScope = true
	}
	return routes, imports, nil
}

// readRows reads the rows of routes(): the []routeRow literal, and the routeRow literals the test
// hooks append under `if s.deps.TestHooksEnabled`.
func readRows(fset *token.FileSet, file *ast.File, text func(ast.Node) string, imports map[string]string, src tree) ([]*oldRoute, error) {
	body := routesBody(file)
	if body == nil {
		return nil, fmt.Errorf("%s holds no func (s *server) routes()", tableFile)
	}
	var rows []*ast.CompositeLit
	hooks := map[*ast.CompositeLit]bool{}
	var problem error
	var walk func(node ast.Node, hook bool)
	walk = func(node ast.Node, hook bool) {
		ast.Inspect(node, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.IfStmt:
				condition := text(n.Cond)
				if condition == hooksCondition {
					walk(n.Body, true)
					if n.Else != nil {
						walk(n.Else, hook)
					}
					return false
				}
				// Any other condition decides whether its rows mount, and this reader does not know
				// what it decides, so a row under it is refused rather than written as always
				// mounted. `if s.deps.TestHooksEnabled && s.deps.Store != nil` is such a condition:
				// read as a hooks block it would be right, read as none it writes 15 hooks ungated.
				ast.Inspect(n, func(inner ast.Node) bool {
					literal, isLiteral := inner.(*ast.CompositeLit)
					if !isLiteral {
						return true
					}
					if ident, named := literal.Type.(*ast.Ident); !named || ident.Name != "routeRow" {
						return !isRowSlice(literal.Type)
					}
					if problem == nil {
						problem = fmt.Errorf("routes() holds a row under `if %s`, a condition this reader does not know the meaning of, so whether it mounts cannot be read: %s",
							condition, text(literal))
					}
					return false
				})
				return false
			case *ast.CallExpr:
				if ident, ok := n.Fun.(*ast.Ident); ok && ident.Name == "append" && len(n.Args) > 1 {
					if err := noCommentBetween(fset, file, n.Lparen, n.Rparen, n.Args[1:]); err != nil && problem == nil {
						problem = err
					}
				}
			case *ast.CompositeLit:
				if isRowSlice(n.Type) {
					if err := noCommentBetween(fset, file, n.Lbrace, n.Rbrace, n.Elts); err != nil && problem == nil {
						problem = err
					}
					for _, element := range n.Elts {
						row, ok := element.(*ast.CompositeLit)
						if !ok {
							problem = fmt.Errorf("a row of routes() is no composite literal: %s", text(element))
							continue
						}
						rows = append(rows, row)
						hooks[row] = hook
					}
					return false
				}
				if ident, ok := n.Type.(*ast.Ident); ok && ident.Name == "routeRow" {
					rows = append(rows, n)
					hooks[n] = hook
					return false
				}
			}
			return true
		})
	}
	walk(body, false)
	if problem != nil {
		return nil, problem
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("routes() holds no row")
	}
	routes := make([]*oldRoute, 0, len(rows))
	for _, row := range rows {
		if len(row.Elts) != 5 {
			return nil, fmt.Errorf("a row of routes() has %d columns, want 5: %s", len(row.Elts), text(row))
		}
		for _, element := range row.Elts {
			if _, keyed := element.(*ast.KeyValueExpr); keyed {
				return nil, fmt.Errorf("a row of routes() names its columns, which this reader does not read: %s", text(row))
			}
		}
		route := &oldRoute{
			methodExpr: text(row.Elts[0]), patternExpr: text(row.Elts[1]),
			authExpr: text(row.Elts[2]), descriptionExpr: text(row.Elts[3]),
			testHook: hooks[row], packages: map[string]bool{},
		}
		method, ok := row.Elts[0].(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(method.Sel.Name, "Method") || text(method.X) != "http" {
			return nil, fmt.Errorf("a row's method is no http.Method constant: %s", route.methodExpr)
		}
		route.method = strings.ToUpper(strings.TrimPrefix(method.Sel.Name, "Method"))
		switch pattern := row.Elts[1].(type) {
		case *ast.BasicLit:
			value, err := stringValue(pattern)
			if err != nil {
				return nil, err
			}
			route.pattern = value
		case *ast.Ident:
			value, err := packageStringConstant(src, pattern.Name)
			if err != nil {
				return nil, fmt.Errorf("a row's pattern %s: %w", pattern.Name, err)
			}
			route.pattern = value
		default:
			return nil, fmt.Errorf("a row's pattern is neither a string nor a constant: %s", route.patternExpr)
		}
		handler, ok := row.Elts[4].(*ast.SelectorExpr)
		if !ok || text(handler.X) != "s" {
			return nil, fmt.Errorf("%s %s: its handler is no method of the server (s.<method>): %s", route.method, route.pattern, text(row.Elts[4]))
		}
		route.handler = handler.Sel.Name
		route.key = route.method + " " + route.pattern
		for _, element := range row.Elts[:4] {
			qualifiers(element, imports, route.packages)
		}
		routes = append(routes, route)
	}
	return routes, nil
}

// routesBody is the body of func (s *server) routes(), nil where the file declares none.
func routesBody(file *ast.File) *ast.BlockStmt {
	for _, declared := range file.Decls {
		function, ok := declared.(*ast.FuncDecl)
		if ok && function.Name.Name == "routes" && function.Recv != nil && function.Body != nil {
			return function.Body
		}
	}
	return nil
}

// isRowSlice reports whether a composite literal's type is []routeRow.
func isRowSlice(expression ast.Expr) bool {
	slice, ok := expression.(*ast.ArrayType)
	if !ok || slice.Len != nil {
		return false
	}
	element, ok := slice.Elt.(*ast.Ident)
	return ok && element.Name == "routeRow"
}

// packageLiteral is the composite literal a package-level var of that name is declared with.
func packageLiteral(file *ast.File, name string) *ast.CompositeLit {
	for _, declared := range file.Decls {
		general, ok := declared.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, ident := range value.Names {
				if ident.Name == name && index < len(value.Values) {
					literal, _ := value.Values[index].(*ast.CompositeLit)
					return literal
				}
			}
		}
	}
	return nil
}

// mapEntry is one entry of a side map: its key, its value's expression and the fact it states.
type mapEntry struct {
	key  string
	expr ast.Expr
	fact fact
}

// mapEntries reads a side map's entries in their order, each with the comments written for it: a
// comment above an entry, up to the entry above it, and a comment on the line an entry ends on.
//
// A comment that heads a group of entries is read as the first entry's alone. How far such a
// group reaches is the reader's to tell - the maps nest one group in another, and most of
// routeShapes' comments stand above one entry with the next entry right below - so reading it
// onto every entry up to the next blank line would set it on routes it says nothing of.
func mapEntries(fset *token.FileSet, file *ast.File, literal *ast.CompositeLit, text func(ast.Node) string, constant func(string) (string, bool)) ([]mapEntry, error) {
	line := func(pos token.Pos) int { return fset.Position(pos).Line }
	var comments []*ast.CommentGroup
	for _, group := range file.Comments {
		if group.Pos() > literal.Lbrace && group.End() < literal.Rbrace {
			comments = append(comments, group)
		}
	}
	entries := make([]mapEntry, 0, len(literal.Elts))
	previousEnd := literal.Lbrace
	for _, element := range literal.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("an entry is no key and value: %s", text(element))
		}
		key, err := stringValueWith(pair.Key, constant)
		if err != nil {
			return nil, err
		}
		var above []string
		for _, comment := range comments {
			if comment.Pos() <= previousEnd || comment.End() >= pair.Pos() {
				continue
			}
			if len(entries) > 0 && line(comment.Pos()) == line(previousEnd) {
				own := &entries[len(entries)-1].fact
				own.comments = append(own.comments, commentLines(comment)...)
				continue
			}
			above = append(above, commentLines(comment)...)
		}
		entries = append(entries, mapEntry{key: key, expr: pair.Value, fact: fact{value: text(pair.Value), comments: above}})
		previousEnd = pair.End()
	}
	for _, comment := range comments {
		if comment.Pos() > previousEnd && line(comment.Pos()) != line(previousEnd) {
			return nil, fmt.Errorf("a comment below the last entry stands for no entry: %s", comment.Text())
		}
		if comment.Pos() > previousEnd {
			own := &entries[len(entries)-1].fact
			own.comments = append(own.comments, commentLines(comment)...)
		}
	}
	return entries, nil
}

// commentLines is a comment group's lines as written, each a // or /* */ comment of its own.
func commentLines(group *ast.CommentGroup) []string {
	lines := make([]string, 0, len(group.List))
	for _, comment := range group.List {
		lines = append(lines, comment.Text)
	}
	return lines
}

// noCommentBetween fails on a comment inside a literal's braces that is in none of its elements:
// such a comment would stand for no route once the rows are split.
func noCommentBetween(fset *token.FileSet, file *ast.File, lbrace, rbrace token.Pos, elements []ast.Expr) error {
	for _, group := range file.Comments {
		if group.Pos() <= lbrace || group.End() >= rbrace {
			continue
		}
		inside := slices.ContainsFunc(elements, func(element ast.Expr) bool {
			return group.Pos() >= element.Pos() && group.End() <= element.End()
		})
		if !inside {
			return fmt.Errorf("%s: a comment between rows of routes() stands for no one row: %s", fset.Position(group.Pos()), strings.TrimSpace(group.Text()))
		}
	}
	return nil
}

// stringValue is the value of a string a key is written as: a literal, an `http.MethodX` constant,
// or the two joined with `+`, which is how a key naming the method by its constant is written
// ("GET /api/v1/whoami" as `http.MethodGet + " /api/v1/whoami"`). Any other expression is refused:
// its value is the Go compiler's to know, and a key read wrongly would move a route's fact onto
// another route.
func stringValue(expression ast.Expr) (string, error) {
	return stringValueWith(expression, func(name string) (string, bool) { return "", false })
}

// stringValueWith is stringValue with a package constant's value, for a key written as one
// (`whoamiRow`, which read_classes.go declares beside its map).
func stringValueWith(expression ast.Expr, constant func(string) (string, bool)) (string, error) {
	switch value := expression.(type) {
	case *ast.Ident:
		text, known := constant(value.Name)
		if !known {
			return "", fmt.Errorf("%s is no string constant this reader can resolve", value.Name)
		}
		return text, nil
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", fmt.Errorf("%s is no string literal", value.Kind)
		}
		return strconv.Unquote(value.Value)
	case *ast.SelectorExpr:
		pkg, isIdent := value.X.(*ast.Ident)
		method, known := httpMethodConstants[value.Sel.Name]
		if !isIdent || pkg.Name != "http" || !known {
			return "", fmt.Errorf("%s is no http.MethodX constant", text(expression))
		}
		return method, nil
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", fmt.Errorf("%s joins its parts with %s, and a key is written with +", text(expression), value.Op)
		}
		left, err := stringValueWith(value.X, constant)
		if err != nil {
			return "", err
		}
		right, err := stringValueWith(value.Y, constant)
		if err != nil {
			return "", err
		}
		return left + right, nil
	default:
		return "", fmt.Errorf("%T is no string literal, http.MethodX constant, or the two joined with +", expression)
	}
}

// stringConstants is every package-level string constant the files declare, by name: a key
// written as one (`whoamiRow`) reads as its value.
func stringConstants(files map[string]*ast.File) map[string]string {
	constants := map[string]string{}
	for _, file := range files {
		for _, declared := range file.Decls {
			general, isGeneral := declared.(*ast.GenDecl)
			if !isGeneral || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, isValue := spec.(*ast.ValueSpec)
				if !isValue {
					continue
				}
				for index, ident := range value.Names {
					if index >= len(value.Values) {
						continue
					}
					if text, err := stringValue(value.Values[index]); err == nil {
						constants[ident.Name] = text
					}
				}
			}
		}
	}
	return constants
}

// httpMethodConstants is net/http's method constants by their name, so a key that names its method
// by the constant reads as the method.
var httpMethodConstants = map[string]string{
	"MethodGet": http.MethodGet, "MethodHead": http.MethodHead, "MethodPost": http.MethodPost,
	"MethodPut": http.MethodPut, "MethodPatch": http.MethodPatch, "MethodDelete": http.MethodDelete,
	"MethodConnect": http.MethodConnect, "MethodOptions": http.MethodOptions, "MethodTrace": http.MethodTrace,
}

// text is an expression as Go prints it, for a refusal naming what it could not read.
func text(expression ast.Expr) string {
	var out strings.Builder
	if err := printer.Fprint(&out, token.NewFileSet(), expression); err != nil {
		return fmt.Sprintf("%T", expression)
	}
	return out.String()
}

// qualifiers adds to into every import name expression qualifies an identifier with.
func qualifiers(expression ast.Node, imports map[string]string, into map[string]bool) {
	ast.Inspect(expression, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok {
			if ident, ok := selector.X.(*ast.Ident); ok {
				if _, imported := imports[ident.Name]; imported {
					into[ident.Name] = true
				}
			}
		}
		return true
	})
}

// packageStringConstant is the value of one package-level string constant of the package's
// non-test files, written as one string literal: the value a row's pattern may name.
func packageStringConstant(src tree, name string) (string, error) {
	files, err := src.filesNaming(name)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		content, err := src.read(file)
		if err != nil {
			return "", err
		}
		parsed, err := parser.ParseFile(fset, file, content, parser.SkipObjectResolution)
		if err != nil {
			return "", err
		}
		for _, declared := range parsed.Decls {
			general, ok := declared.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value := spec.(*ast.ValueSpec)
				for index, ident := range value.Names {
					if ident.Name == name && index < len(value.Values) {
						return stringValue(value.Values[index])
					}
				}
			}
		}
	}
	return "", errors.New("no file of the package declares it a constant")
}

// renderRoute writes one route's file: the package clause, the imports its expressions name, and
// the one declaration that registers it, formatted as gofmt formats it.
func renderRoute(route *oldRoute, imports map[string]string) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("package api\n\n")
	packages := map[string]bool{"http": true}
	for name := range route.packages {
		packages[name] = true
	}
	var standard, module []string
	for name := range packages {
		importPath, known := imports[name]
		if !known {
			return nil, fmt.Errorf("the package %s is imported by neither %s nor %s", name, tableFile, shapesFile)
		}
		spec := strconv.Quote(importPath)
		if path.Base(importPath) != name {
			spec = name + " " + spec
		}
		if strings.Contains(strings.SplitN(importPath, "/", 2)[0], ".") {
			module = append(module, spec)
		} else {
			standard = append(standard, spec)
		}
	}
	slices.Sort(standard)
	slices.Sort(module)
	out.WriteString("import (\n")
	for _, spec := range standard {
		fmt.Fprintf(&out, "\t%s\n", spec)
	}
	if len(standard) > 0 && len(module) > 0 {
		out.WriteString("\n")
	}
	for _, spec := range module {
		fmt.Fprintf(&out, "\t%s\n", spec)
	}
	out.WriteString(")\n\n")

	out.WriteString("var _ = declareRoute(routeDecl{\n")
	field := func(comments []string, name, value string) {
		for _, comment := range comments {
			fmt.Fprintf(&out, "\t%s\n", comment)
		}
		fmt.Fprintf(&out, "\t%s: %s,\n", name, value)
	}
	field(nil, "Method", route.methodExpr)
	field(nil, "Pattern", route.patternExpr)
	field(nil, "Auth", route.authExpr)
	field(nil, "Description", route.descriptionExpr)
	field(nil, "Handler", "(*server)."+route.handler)
	if route.testHook {
		field(nil, "TestHook", "true")
	}
	if route.writeClass.value != "" {
		field(route.writeClass.comments, "WriteClass", route.writeClass.value)
	}
	if route.id.value != "" {
		value, err := typedLiteral("idRoute", route.id.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", idRoutesName, err)
		}
		field(route.id.comments, "ID", "&"+value)
	}
	if route.scopes.value != "" {
		value, err := typedLiteral("routeScopeSet", route.scopes.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", routeScopesName, err)
		}
		field(route.scopes.comments, "OAuthScopes", value)
	}
	switch {
	case route.read.value != "":
		value, err := typedLiteral("readEntry", route.read.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", readClassesName, err)
		}
		field(route.read.comments, "Read", value)
	case route.readScope:
		// A branch made before #4557, whose read is a name in readScopeRoutes and not an entry:
		// the route it named is a read that writes nothing, which is what the list meant.
		field(nil, "Read", "readEntry{Writes: readWritesNothing}")
	}
	if route.shape.value != "" {
		value, err := typedLiteral("routeShape", route.shape.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", routeShapesName, err)
		}
		field(route.shape.comments, "Shape", value)
	}
	out.WriteString("})\n")
	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format the route's file: %w\n%s", err, out.String())
	}
	return formatted, nil
}

// typedLiteral names the type of an entry's value, which the map's own type elided: {...} becomes
// typeName{...}. A value written with its type already is answered as written.
func typedLiteral(typeName, value string) (string, error) {
	switch {
	case strings.HasPrefix(value, "{"):
		return typeName + value, nil
	case strings.HasPrefix(value, typeName+"{"):
		return value, nil
	}
	return "", errors.New("its value is no composite literal: " + value)
}
