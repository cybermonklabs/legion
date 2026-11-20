package textcheck

import (
	"errors"
	"go/ast"
	"go/build"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// natsModule is nats.go's module, whose methods create a stream or a bucket.
const natsModule = "github.com/nats-io/nats.go"

// streamCreateMethods are the methods that create a stream or a bucket, the legacy JetStream
// context's and the jetstream package's: a call the check weighs is a call of one of these by its
// declaration in nats.go (go/types), whatever the receiver's static type spells. The method
// expressions below name each one, so a method nats.go renames fails this package's build rather
// than leave the check blind to its new name.
var streamCreateMethods = map[string]bool{
	"AddStream": true, "CreateKeyValue": true, "CreateObjectStore": true, "CreateStream": true,
	"CreateOrUpdateKeyValue": true, "CreateOrUpdateObjectStore": true,
}

var _ = []any{
	nats.JetStreamManager.AddStream, nats.KeyValueManager.CreateKeyValue, nats.ObjectStoreManager.CreateObjectStore,
	jetstream.StreamManager.CreateStream, jetstream.KeyValueManager.CreateKeyValue,
	jetstream.KeyValueManager.CreateOrUpdateKeyValue, jetstream.ObjectStoreManager.CreateObjectStore,
	jetstream.ObjectStoreManager.CreateOrUpdateObjectStore,
}

// clusterFixtures are the functions that create a stream or a bucket on a cluster for a test, by
// their full names: testnats's Cluster.CreateStream and Cluster.CreateKeyValue, which send the
// create once through a context of their own and fail the test with the server's refusal. A
// create inside them is the fixture's own.
var clusterFixtures = map[string]bool{
	"(*" + testnatsImport + ".Cluster).CreateStream":   true,
	"(*" + testnatsImport + ".Cluster).CreateKeyValue": true,
}

// isCluster reports whether t is testnats's Cluster or a pointer to it, by the type's identity.
func isCluster(t types.Type) bool {
	if t == nil {
		return false
	}
	if pointer, ok := types.Unalias(t).(*types.Pointer); ok {
		t = pointer.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == testnatsImport && named.Obj().Name() == "Cluster"
}

// typedSource is one Go file of a checked package, parsed; checked is whether a create in it is a
// test's own: a test file, or a file of a test helper package (testSources).
type typedSource struct {
	rel     string
	file    *ast.File
	checked bool
}

// typedUnit is one package of the tree, type-checked: a directory's package with its in-package
// test files, or that directory's external test package.
type typedUnit struct {
	files   []typedSource
	fileSet *token.FileSet
	info    *types.Info
}

// sourceImporter type-checks what a checked package imports from source, leniently: a package of
// the envoy module (from the tree checked, or else from the module's own directory) and nats.go's
// packages (from the module cache, at the version the module's go.mod names), each without its
// function bodies; every other import, the standard library among them, is an empty package.
// Every name the check weighs is declared in those two modules, testnats's Cluster and nats.go's
// create methods, so the rest of a file may fail to type-check and the check still reads the
// values it follows; errors are not reported. It runs no go command: only moduleDir, once, asks
// the go command where its module cache is.
type sourceImporter struct {
	fileSet   *token.FileSet
	roots     []string // where an envoy package's directory is looked for, in order
	natsDir   string
	packages  map[string]*types.Package
	importing map[string]bool
}

// packageDir is the directory importPath's source is read from, or "" for an empty package.
func (s *sourceImporter) packageDir(importPath string) string {
	switch {
	case importPath == natsModule || strings.HasPrefix(importPath, natsModule+"/"):
		return filepath.Join(s.natsDir, filepath.FromSlash(strings.TrimPrefix(importPath, natsModule)))
	case importPath == envoyModule || strings.HasPrefix(importPath, envoyModule+"/"):
		for _, root := range s.roots {
			dir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(importPath, envoyModule)))
			if entries, err := os.ReadDir(dir); err == nil && slices.ContainsFunc(entries, func(entry fs.DirEntry) bool {
				return strings.HasSuffix(entry.Name(), ".go")
			}) {
				return dir
			}
		}
	}
	return ""
}

func (s *sourceImporter) Import(importPath string) (*types.Package, error) {
	if importPath == "unsafe" {
		return types.Unsafe, nil
	}
	if pkg, ok := s.packages[importPath]; ok {
		return pkg, nil
	}
	dir := s.packageDir(importPath)
	if dir == "" || s.importing[importPath] {
		return s.empty(importPath), nil
	}
	s.importing[importPath] = true
	defer delete(s.importing, importPath)
	listed, err := build.Default.ImportDir(dir, 0)
	if err != nil && len(listed.GoFiles) == 0 {
		return s.empty(importPath), nil
	}
	var files []*ast.File
	for _, name := range listed.GoFiles {
		if file, err := parser.ParseFile(s.fileSet, filepath.Join(dir, name), nil, parser.SkipObjectResolution); err == nil {
			files = append(files, file)
		}
	}
	config := types.Config{Importer: s, IgnoreFuncBodies: true, FakeImportC: true, Error: func(error) {}}
	pkg, _ := config.Check(importPath, s.fileSet, files, nil)
	s.packages[importPath] = pkg
	return pkg, nil
}

func (s *sourceImporter) empty(importPath string) *types.Package {
	name := path.Base(importPath)
	if strings.HasPrefix(name, "v") && strings.Trim(name[1:], "0123456789") == "" && path.Dir(importPath) != "." {
		name = path.Base(path.Dir(importPath))
	}
	name = strings.Map(func(r rune) rune {
		if r == '-' || r == '.' {
			return -1
		}
		return r
	}, name)
	pkg := types.NewPackage(importPath, name)
	pkg.MarkComplete()
	s.packages[importPath] = pkg
	return pkg
}

// typedTestUnits parses every Go file under root (packages/envoy, infra included; goFiles) and
// type-checks each package that test sources belong to and that can hold a cluster: one whose
// files import testnats, or import a package of the module whose own files reach it. Imports of
// the module are read under root first and then under moduleRoot, the module's own directory, so
// a planted tree under a temporary root type-checks against the module's real testnats.
func typedTestUnits(root, moduleRoot string) ([]*typedUnit, error) {
	natsDir, err := moduleDir(moduleRoot, natsModule)
	if err != nil {
		return nil, err
	}
	fileSet := token.NewFileSet()
	type group struct {
		dir   string
		files []typedSource
	}
	groups := map[string]*group{}
	var keys []string
	err = goFiles(root, func(file, rel string, test bool) error {
		parsed, err := parser.ParseFile(fileSet, file, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		key := path.Dir(rel) + " " + parsed.Name.Name
		if groups[key] == nil {
			groups[key] = &group{dir: path.Dir(rel)}
			keys = append(keys, key)
		}
		groups[key].files = append(groups[key].files, typedSource{rel: rel, file: parsed, checked: test})
		return nil
	})
	if err != nil {
		return nil, err
	}
	imports := &sourceImporter{fileSet: fileSet, roots: []string{root, moduleRoot}, natsDir: natsDir, packages: map[string]*types.Package{}, importing: map[string]bool{}}
	reaches := map[string]bool{}
	var reachesTestnats func(importPath string, seen map[string]bool) bool
	reachesTestnats = func(importPath string, seen map[string]bool) bool {
		if importPath == testnatsImport {
			return true
		}
		if known, ok := reaches[importPath]; ok {
			return known
		}
		dir := imports.packageDir(importPath)
		if dir == "" || strings.HasPrefix(importPath, natsModule) || seen[importPath] {
			return false
		}
		seen[importPath] = true
		listed, _ := build.Default.ImportDir(dir, build.ImportComment)
		found := listed != nil && slices.ContainsFunc(listed.Imports, func(next string) bool { return reachesTestnats(next, seen) })
		reaches[importPath] = found
		return found
	}
	slices.Sort(keys)
	var units []*typedUnit
	for _, key := range keys {
		g := groups[key]
		if !slices.ContainsFunc(g.files, func(source typedSource) bool { return source.checked }) {
			continue
		}
		holdsCluster := slices.ContainsFunc(g.files, func(source typedSource) bool {
			return slices.ContainsFunc(source.file.Imports, func(spec *ast.ImportSpec) bool {
				imported, _ := strconv.Unquote(spec.Path.Value)
				return reachesTestnats(imported, map[string]bool{})
			})
		})
		if !holdsCluster {
			continue
		}
		unit := &typedUnit{files: g.files, fileSet: fileSet, info: &types.Info{
			Types: map[ast.Expr]types.TypeAndValue{}, Defs: map[*ast.Ident]types.Object{},
			Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{},
		}}
		files := make([]*ast.File, 0, len(g.files))
		for _, source := range g.files {
			files = append(files, source.file)
		}
		config := types.Config{Importer: imports, FakeImportC: true, Error: func(error) {}}
		_, _ = config.Check(envoyModule+"/"+g.dir, fileSet, files, unit.info)
		units = append(units, unit)
	}
	return units, nil
}

// clusterFacts are what the walk learnt across packages, by a function's full name
// (types.Func.FullName): the results of a function that hold a value it makes from a cluster of
// its own, whatever its arguments are, and the parameters some call hands a value made from a
// cluster.
type clusterFacts struct {
	results, params map[string]map[int]bool
	changed         bool
}

func (f *clusterFacts) mark(facts map[string]map[int]bool, key string, index int) {
	if facts[key] == nil {
		facts[key] = map[int]bool{}
	}
	if !facts[key][index] {
		facts[key][index] = true
		f.changed = true
	}
}

// clusterFlow follows, through one typed package, the values made from a testnats cluster: a value
// of type Cluster, and everything computed from one. A variable assigned such a value holds one,
// and so does a variable whose field, entry or element is assigned one (`s.js =
// cluster.JetStream(t)`, `contexts["a"] = cluster.JetStream(t)`), as a struct literal one of whose
// fields is set to one does, each value by value; a call whose receiver or any argument is one
// makes one (a member's URL from
// cluster.Servers[0], a connection from that URL, a relay in front of it, the JetStream context of
// that connection); and so does a call of a function whose result at that index is one it makes
// from a cluster of its own (clusterFacts.results), which keeps a helper's result index when its
// results are forwarded whole. A flow that reads arguments (fromCallers) also takes a parameter to
// hold one when some call of the function, in any package the walk reads, hands it one
// (clusterFacts.params), a function literal's parameter included: it is the flow that judges
// creates. The flow that learns a function's results does not, so a helper that a call on a
// cluster and a call on a lone server both reach makes a cluster's value only for the first (a
// relay in front of a member, against one in front of a lone server). A function value is
// followed when it is a function literal held in a variable or a struct field (a table row's
// function), or a create method bound to its receiver (`create := js.AddStream`), each by the
// type checker's objects.
type clusterFlow struct {
	unit        *typedUnit
	facts       *clusterFacts
	fromCallers bool
	holds       map[types.Object]bool
	literals    map[types.Object][]*ast.FuncLit // the function literals a variable or a struct field is set to
	bound       map[types.Object]bool           // a variable set to a create method bound to a receiver made from a cluster
	results     map[*ast.FuncLit]map[int]bool
	assigned    map[types.Object]map[ast.Expr]bool
	opaque      map[types.Object]bool // a variable whose Replicas is set, or whose address goes anywhere but a create
	toCreates   map[*ast.UnaryExpr]bool
	changed     bool
}

func (c *clusterFlow) object(ident *ast.Ident) types.Object {
	if obj := c.unit.info.Uses[ident]; obj != nil {
		return obj
	}
	return c.unit.info.Defs[ident]
}

func (c *clusterFlow) hold(obj types.Object) {
	if obj != nil && !c.holds[obj] {
		c.holds[obj] = true
		c.changed = true
	}
}

// holder is the variable a value assigned to left is kept in: left itself, or the variable whose
// field, entry or element left is (a struct, a map, a slice or an array), so a value one of its
// fields or entries is set to is that variable's, value by value as a struct literal's is.
func (c *clusterFlow) holder(left ast.Expr) types.Object {
	switch left := ast.Unparen(left).(type) {
	case *ast.Ident:
		if left.Name == "_" {
			return nil
		}
		return c.object(left)
	case *ast.SelectorExpr:
		if _, ok := c.unit.info.Selections[left]; ok {
			return c.holder(left.X)
		}
		return c.object(left.Sel)
	case *ast.IndexExpr:
		return c.holder(left.X)
	case *ast.StarExpr:
		return c.holder(left.X)
	}
	return nil
}

// literalKey is what a function literal that expr is set to, or that a call through expr calls,
// is recorded under: a variable, or a struct field by its declaration (a table row's function).
func (c *clusterFlow) literalKey(expr ast.Expr) types.Object {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.Ident:
		if expr.Name == "_" {
			return nil
		}
		return c.object(expr)
	case *ast.SelectorExpr:
		if selection, ok := c.unit.info.Selections[expr]; ok {
			if selection.Kind() == types.FieldVal {
				return selection.Obj()
			}
			return nil
		}
		return c.object(expr.Sel)
	}
	return nil
}

// fromCluster reports whether expr's value is made from a cluster.
func (c *clusterFlow) fromCluster(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	if value, ok := c.unit.info.Types[expr]; ok && isCluster(value.Type) {
		return true
	}
	switch expr := expr.(type) {
	case *ast.Ident:
		return c.holds[c.object(expr)]
	case *ast.ParenExpr:
		return c.fromCluster(expr.X)
	case *ast.SelectorExpr:
		if _, ok := c.unit.info.Selections[expr]; ok {
			return c.fromCluster(expr.X)
		}
		return c.holds[c.object(expr.Sel)]
	case *ast.CallExpr:
		return c.callFromCluster(expr, 0)
	case *ast.IndexExpr:
		return c.fromCluster(expr.X)
	case *ast.IndexListExpr:
		return c.fromCluster(expr.X)
	case *ast.SliceExpr:
		return c.fromCluster(expr.X)
	case *ast.StarExpr:
		return c.fromCluster(expr.X)
	case *ast.UnaryExpr:
		return c.fromCluster(expr.X)
	case *ast.BinaryExpr:
		return c.fromCluster(expr.X) || c.fromCluster(expr.Y)
	case *ast.TypeAssertExpr:
		return c.fromCluster(expr.X)
	case *ast.KeyValueExpr:
		return c.fromCluster(expr.Value)
	case *ast.CompositeLit:
		return slices.ContainsFunc(expr.Elts, c.fromCluster)
	}
	return false
}

// callee is the function call calls, when it is a declared one.
func (c *clusterFlow) callee(call *ast.CallExpr) *types.Func {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ := c.object(fun).(*types.Func)
		return fn
	case *ast.SelectorExpr:
		if selection, ok := c.unit.info.Selections[fun]; ok {
			fn, _ := selection.Obj().(*types.Func)
			return fn
		}
		fn, _ := c.object(fun.Sel).(*types.Func)
		return fn
	}
	return nil
}

// isCreate reports whether fn is one of nats.go's create methods (streamCreateMethods).
func isCreate(fn *types.Func) bool {
	return fn != nil && fn.Pkg() != nil && strings.HasPrefix(fn.Pkg().Path(), natsModule) && streamCreateMethods[fn.Name()]
}

// boundCreate reports whether expr is a create method bound to a receiver made from a cluster
// (`cluster.JetStream(t).AddStream`), or a variable set to one.
func (c *clusterFlow) boundCreate(expr ast.Expr) bool {
	switch expr := ast.Unparen(expr).(type) {
	case *ast.SelectorExpr:
		selection, ok := c.unit.info.Selections[expr]
		if !ok || selection.Kind() != types.MethodVal {
			return false
		}
		fn, _ := selection.Obj().(*types.Func)
		return isCreate(fn) && c.fromCluster(expr.X)
	case *ast.Ident:
		return c.bound[c.object(expr)]
	}
	return false
}

// calledLiterals are the function literals call calls: itself, or the ones a variable or a struct
// field it calls through is set to.
func (c *clusterFlow) calledLiterals(call *ast.CallExpr) []*ast.FuncLit {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.FuncLit:
		return []*ast.FuncLit{fun}
	case *ast.Ident, *ast.SelectorExpr:
		if obj := c.literalKey(fun); obj != nil {
			return c.literals[obj]
		}
	}
	return nil
}

// setLiteral records that obj, a variable or a struct field, is set to value when it is a function
// literal.
func (c *clusterFlow) setLiteral(obj types.Object, value ast.Expr) {
	literal, ok := ast.Unparen(value).(*ast.FuncLit)
	if obj == nil || !ok || slices.Contains(c.literals[obj], literal) {
		return
	}
	c.literals[obj] = append(c.literals[obj], literal)
	c.changed = true
}

// callFromCluster reports whether call's result at index is made from a cluster.
func (c *clusterFlow) callFromCluster(call *ast.CallExpr, index int) bool {
	if fun, ok := c.unit.info.Types[call.Fun]; ok && fun.IsType() {
		return len(call.Args) == 1 && c.fromCluster(call.Args[0])
	}
	if fn := c.callee(call); fn != nil && c.facts.results[fn.FullName()][index] {
		return true
	}
	if slices.ContainsFunc(c.calledLiterals(call), func(literal *ast.FuncLit) bool { return c.results[literal][index] }) {
		return true
	}
	if selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		if _, method := c.unit.info.Selections[selector]; method && c.fromCluster(selector.X) {
			return true
		}
	}
	return slices.ContainsFunc(call.Args, c.fromCluster)
}

// assign records that left takes right's value, at index when right is a call of many results.
func (c *clusterFlow) assign(left, right ast.Expr, index int, many bool) {
	if ident, ok := left.(*ast.Ident); ok && ident.Name != "_" {
		obj := c.object(ident)
		if !many {
			if c.assigned[obj] == nil {
				c.assigned[obj] = map[ast.Expr]bool{}
			}
			c.assigned[obj][right] = true
			if c.boundCreate(right) && !c.bound[obj] {
				c.bound[obj] = true
				c.changed = true
			}
		} else {
			c.opaque[obj] = true
		}
	}
	if !many {
		c.setLiteral(c.literalKey(left), right)
	}
	if selector, ok := left.(*ast.SelectorExpr); ok && selector.Sel.Name == "Replicas" {
		if ident, ok := ast.Unparen(selector.X).(*ast.Ident); ok {
			c.opaque[c.object(ident)] = true
		}
	}
	from := c.fromCluster(right)
	if call, ok := ast.Unparen(right).(*ast.CallExpr); ok && many {
		from = c.callFromCluster(call, index)
	}
	if from {
		c.hold(c.holder(left))
	}
}

// fieldsOf calls set with each field a struct literal sets and the value it sets it to, keyed or
// in order.
func (c *clusterFlow) fieldsOf(literal *ast.CompositeLit, set func(field types.Object, value ast.Expr)) {
	value, ok := c.unit.info.Types[literal]
	if !ok || value.Type == nil {
		return
	}
	t := value.Type
	if pointer, ok := types.Unalias(t).(*types.Pointer); ok {
		t = pointer.Elem()
	}
	fields, ok := t.Underlying().(*types.Struct)
	if !ok {
		return
	}
	for i, elt := range literal.Elts {
		if field, ok := elt.(*ast.KeyValueExpr); ok {
			if key, ok := field.Key.(*ast.Ident); ok {
				set(c.object(key), field.Value)
			}
		} else if i < fields.NumFields() {
			set(fields.Field(i), elt)
		}
	}
}

// walk follows the values of body, a function's, into its variables, and reports each of its
// returns through result; key is the function's full name, "" for a function literal.
func (c *clusterFlow) walk(body ast.Node, result func(index int)) {
	ast.Inspect(body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			if c.results[node] == nil {
				c.results[node] = map[int]bool{}
			}
			c.walk(node.Body, func(index int) {
				if !c.results[node][index] {
					c.results[node][index] = true
					c.changed = true
				}
			})
			return false
		case *ast.AssignStmt:
			many := len(node.Lhs) > 1 && len(node.Rhs) == 1
			for i, left := range node.Lhs {
				if many {
					c.assign(left, node.Rhs[0], i, true)
				} else if i < len(node.Rhs) {
					c.assign(left, node.Rhs[i], 0, false)
				}
			}
		case *ast.ValueSpec:
			many := len(node.Names) > 1 && len(node.Values) == 1
			for i, name := range node.Names {
				if many {
					c.assign(name, node.Values[0], i, true)
				} else if i < len(node.Values) {
					c.assign(name, node.Values[i], 0, false)
				}
			}
		case *ast.RangeStmt:
			if c.fromCluster(node.X) {
				for _, left := range []ast.Expr{node.Key, node.Value} {
					if ident, ok := left.(*ast.Ident); ok {
						c.hold(c.object(ident))
					}
				}
			}
		case *ast.ReturnStmt:
			if call, ok := singleCall(node.Results); ok {
				if tuple, ok := c.unit.info.Types[call].Type.(*types.Tuple); ok {
					for i := range tuple.Len() {
						if c.callFromCluster(call, i) {
							result(i)
						}
					}
					return true
				}
			}
			for i, value := range node.Results {
				if c.fromCluster(value) {
					result(i)
				}
			}
		case *ast.CompositeLit:
			// A struct literal's own value holds what its fields hold (fromCluster), value by value,
			// so its fields are not held by their declaration; a function literal it sets a field
			// to is recorded, for a call through that field (a table row's function).
			c.fieldsOf(node, c.setLiteral)
		case *ast.UnaryExpr:
			// An address of a variable that goes anywhere but a create's argument, to a call, a
			// variable or a field, lets the configuration change after the create's argument was
			// read: a create of it no longer provably sets one copy (atMostOneCopy).
			if ident, ok := ast.Unparen(node.X).(*ast.Ident); ok && node.Op == token.AND && !c.toCreates[node] {
				c.opaque[c.object(ident)] = true
			}
		case *ast.CallExpr:
			if isCreate(c.callee(node)) || c.boundCreate(node.Fun) {
				for _, arg := range node.Args {
					if unary, ok := ast.Unparen(arg).(*ast.UnaryExpr); ok && unary.Op == token.AND {
						c.toCreates[unary] = true
					}
				}
			}
			if !c.fromCallers {
				break
			}
			if fn := c.callee(node); fn != nil {
				if signature, ok := fn.Type().(*types.Signature); ok && signature.Params().Len() > 0 {
					for i, arg := range node.Args {
						if c.fromCluster(arg) {
							c.facts.mark(c.facts.params, fn.FullName(), min(i, signature.Params().Len()-1))
						}
					}
				}
			}
			for _, literal := range c.calledLiterals(node) {
				var params []*ast.Ident
				for _, field := range literal.Type.Params.List {
					params = append(params, field.Names...)
				}
				for i, arg := range node.Args {
					if len(params) > 0 && c.fromCluster(arg) {
						c.hold(c.object(params[min(i, len(params)-1)]))
					}
				}
			}
		}
		return true
	})
}

func singleCall(results []ast.Expr) (*ast.CallExpr, bool) {
	if len(results) != 1 {
		return nil, false
	}
	call, ok := ast.Unparen(results[0]).(*ast.CallExpr)
	return call, ok
}

// pass walks every function of the package once.
func (c *clusterFlow) pass() {
	for _, source := range c.unit.files {
		for _, decl := range source.file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				fn, _ := c.unit.info.Defs[decl.Name].(*types.Func)
				if fn == nil || decl.Body == nil {
					continue
				}
				key := fn.FullName()
				if signature, ok := fn.Type().(*types.Signature); ok && c.fromCallers {
					for i := range signature.Params().Len() {
						if c.facts.params[key][i] {
							c.hold(signature.Params().At(i))
						}
					}
				}
				c.walk(decl.Body, func(index int) {
					if !c.fromCallers {
						c.facts.mark(c.facts.results, key, index)
					}
				})
			case *ast.GenDecl:
				c.walk(decl, func(int) {})
			}
		}
	}
}

// atMostOneCopy reports whether config, a create's configuration, provably sets one copy: a
// composite literal, or the address of one, whose Replicas is absent or a constant of at most 1,
// or a variable of the function assigned only such literals, whose Replicas is set nowhere and
// whose address goes only to creates.
func (c *clusterFlow) atMostOneCopy(config ast.Expr) bool {
	config = ast.Unparen(config)
	if unary, ok := config.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		config = ast.Unparen(unary.X)
	}
	switch config := config.(type) {
	case *ast.CompositeLit:
		for _, elt := range config.Elts {
			field, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				return false
			}
			if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Replicas" {
				value := c.unit.info.Types[field.Value].Value
				if value == nil {
					return false
				}
				copies, exact := constant.Int64Val(constant.ToInt(value))
				return exact && copies <= 1
			}
		}
		return true
	case *ast.Ident:
		obj := c.object(config)
		values := c.assigned[obj]
		if len(values) == 0 || c.opaque[obj] {
			return false
		}
		for value := range values {
			if _, literal := ast.Unparen(value).(*ast.CompositeLit); !literal || !c.atMostOneCopy(value) {
				return false
			}
		}
		return true
	}
	return false
}

// configOf is the argument of call that is a create's configuration: a nats.go struct, or a
// pointer to one, with a Replicas field.
func (c *clusterFlow) configOf(call *ast.CallExpr) ast.Expr {
	for _, arg := range call.Args {
		t := c.unit.info.Types[arg].Type
		if t == nil {
			continue
		}
		if pointer, ok := types.Unalias(t).(*types.Pointer); ok {
			t = pointer.Elem()
		}
		named, ok := types.Unalias(t).(*types.Named)
		if !ok || named.Obj().Pkg() == nil || !strings.HasPrefix(named.Obj().Pkg().Path(), natsModule) {
			continue
		}
		if fields, ok := named.Underlying().(*types.Struct); ok {
			for field := range fields.Fields() {
				if field.Name() == "Replicas" {
					return arg
				}
			}
		}
	}
	return nil
}

// directClusterCreates type-checks the test sources under root (typedTestUnits) and returns every
// create of a stream or a bucket (streamCreateMethods) that a test sends itself through a JetStream
// context made from a cluster (clusterFlow), with a configuration that may set more than one copy
// (atMostOneCopy). nats-server answers a create of more than one copy only from the stream's first
// leader, once its members have elected one, and such a context bounds the create by a fixed wait,
// ten seconds on Cluster.JetStream, which on a loaded machine that election can outlast: the create
// then fails with `context deadline exceeded` although the server made the stream. A test creates
// a stream on a cluster with testnats's Cluster.CreateStream and a bucket with
// Cluster.CreateKeyValue (clusterFixtures). Three creates pass: one of at most one copy, which a
// one-member group answers at once; one a test abandons, on a goroutine of its own that a `go`
// statement starts, with every result discarded (`_, _ =`), whose wait can fail nothing; and one
// inside a fake JetStream context's own create method, which stands in for the server under the
// code's own create. A create whose results the test discards on its own goroutine is judged as
// any other: the test waits out its wait all the same, and goes on whether or not the stream was
// made. A context made inside a function of another package, from no cluster the walk sees, is not
// followed.
func directClusterCreates(root string) ([]directStreamUpdate, error) {
	units, err := typedTestUnits(root, filepath.Join("..", ".."))
	if err != nil {
		return nil, err
	}
	facts := &clusterFacts{results: map[string]map[int]bool{}, params: map[string]map[int]bool{}}
	newFlows := func(fromCallers bool) []*clusterFlow {
		flows := make([]*clusterFlow, len(units))
		for i, unit := range units {
			flows[i] = &clusterFlow{
				unit: unit, facts: facts, fromCallers: fromCallers, holds: map[types.Object]bool{}, literals: map[types.Object][]*ast.FuncLit{},
				bound: map[types.Object]bool{}, results: map[*ast.FuncLit]map[int]bool{}, assigned: map[types.Object]map[ast.Expr]bool{},
				opaque: map[types.Object]bool{}, toCreates: map[*ast.UnaryExpr]bool{},
			}
		}
		return flows
	}
	settle := func(flows []*clusterFlow) error {
		for round := 0; ; round++ {
			if round == 20 {
				return errors.New("the cluster values of the test sources did not settle in 20 passes")
			}
			facts.changed = false
			for _, flow := range flows {
				for flow.changed = true; flow.changed; {
					flow.changed = false
					flow.pass()
				}
			}
			if !facts.changed {
				return nil
			}
		}
	}
	if err := settle(newFlows(false)); err != nil {
		return nil, err
	}
	flows := newFlows(true)
	if err := settle(flows); err != nil {
		return nil, err
	}
	var found []directStreamUpdate
	for _, flow := range flows {
		for _, source := range flow.unit.files {
			if !source.checked {
				continue
			}
			for _, decl := range source.file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if declared, ok := flow.unit.info.Defs[fn.Name].(*types.Func); ok && clusterFixtures[declared.FullName()] {
					continue
				}
				if fn.Recv != nil && streamCreateMethods[fn.Name.Name] {
					// A method named as a create is a fake JetStream context's, which a test hands
					// the code under test: its create stands in for the server under that code's
					// own, as the update check lets a fake's UpdateStream pass.
					continue
				}
				abandoned := map[*ast.CallExpr]bool{}
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if start, ok := node.(*ast.GoStmt); ok {
						if literal, ok := ast.Unparen(start.Call.Fun).(*ast.FuncLit); ok {
							ast.Inspect(literal.Body, func(inner ast.Node) bool {
								if assign, ok := inner.(*ast.AssignStmt); ok && len(assign.Rhs) == 1 && !slices.ContainsFunc(assign.Lhs, func(left ast.Expr) bool {
									ident, ok := left.(*ast.Ident)
									return !ok || ident.Name != "_"
								}) {
									if call, ok := ast.Unparen(assign.Rhs[0]).(*ast.CallExpr); ok {
										abandoned[call] = true
									}
								}
								return true
							})
						}
					}
					call, ok := node.(*ast.CallExpr)
					if !ok || abandoned[call] {
						return true
					}
					if selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && isCreate(flow.callee(call)) {
						if !flow.fromCluster(selector.X) {
							return true
						}
					} else if !flow.boundCreate(call.Fun) {
						return true
					}
					if config := flow.configOf(call); config != nil && flow.atMostOneCopy(config) {
						return true
					}
					found = append(found, directStreamUpdate{path: source.rel, line: flow.unit.fileSet.Position(call.Pos()).Line, function: fn.Name.Name})
					return true
				})
			}
		}
	}
	slices.SortFunc(found, func(a, b directStreamUpdate) int { return strings.Compare(a.String(), b.String()) })
	return found, nil
}

// A test creates a stream or a bucket of more than one copy on a cluster only through testnats's
// Cluster.CreateStream or Cluster.CreateKeyValue (directClusterCreates).
func TestNoTestCreatesAStreamOnAClusterExceptThroughTheFixture(t *testing.T) {
	found, err := directClusterCreates(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		var lines []string
		for _, create := range found {
			lines = append(lines, create.String())
		}
		t.Errorf("%d creates of more than one copy a test sends itself through a JetStream context made from a cluster, whose fixed wait a first election can outlast; create each with testnats's Cluster.CreateStream or Cluster.CreateKeyValue:\n%s",
			len(found), strings.Join(lines, "\n"))
	}
}

// plantedClusterCreates is a tree under the envoy module's path for the control below: demo's
// tests create a stream or a bucket themselves through each kind of context made from a cluster,
// and through the fixture, a lone server's context, a fake context, at one copy and abandoned on a
// goroutine of their own, which the check lets pass; clustertest is a test helper package, whose
// files the check reads as tests' own (goFiles), and production is a package that is neither a
// test nor a helper.
var plantedClusterCreates = map[string]string{
	"internal/demo/helpers_test.go": `package demo

import (
	"net"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/testnats"
)

func startClusterJetStream(t *testing.T) (*testnats.Cluster, nats.JetStreamContext) {
	cluster := testnats.StartCluster(t, 3)
	return cluster, cluster.JetStream(t)
}

func clusterJetStream(t *testing.T) nats.JetStreamContext {
	_, js := startClusterJetStream(t)
	return js
}

func forwarded(t *testing.T) (*testnats.Cluster, nats.JetStreamContext) {
	return startClusterJetStream(t)
}

func memberJetStream(t *testing.T, cluster *testnats.Cluster, i int) (*nats.Conn, nats.JetStreamContext) {
	conn := testnats.Connect(t, cluster.Servers[i].URL())
	js, _ := conn.JetStream(nats.MaxWait(0))
	return conn, js
}

type relay struct{ url string }

func startRelayOn(t *testing.T, upstream string) *relay {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	_ = upstream
	return &relay{url: "nats://" + listener.Addr().String()}
}

func relayedJetStream(t *testing.T, r *relay) (*nats.Conn, nats.JetStreamContext) {
	conn := testnats.Connect(t, r.url)
	js, _ := conn.JetStream(nats.MaxWait(0))
	return conn, js
}

func createOn(js nats.JetStreamContext, cfg nats.StreamConfig) { js.AddStream(&cfg) }

type suite struct{ js nats.JetStreamContext }
`,
	"internal/demo/demo_test.go": `package demo

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/testnats"
)

func chained(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) { cluster.JetStream(t).AddStream(&cfg) }

func held(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	js := cluster.JetStream(t)
	js.AddStream(&cfg)
}

func declared(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	var js nats.JetStreamContext = cluster.JetStream(t)
	js.AddStream(&cfg)
}

func member(t *testing.T, cluster *testnats.Cluster, kv nats.KeyValueConfig) {
	js := cluster.MemberJetStream(t, 0)
	js.CreateKeyValue(&kv)
}

func throughHelper(t *testing.T, cfg nats.StreamConfig) {
	cluster, js := startClusterJetStream(t)
	js.AddStream(&cfg)
	cluster.CreateStream(t, cfg)
}

func throughHelperOfAHelper(t *testing.T, cfg nats.StreamConfig) {
	js := clusterJetStream(t)
	js.AddStream(&cfg)
}

func throughForwarded(t *testing.T, cfg nats.StreamConfig) {
	_, js := forwarded(t)
	js.AddStream(&cfg)
}

func throughMemberHelper(t *testing.T, cluster *testnats.Cluster, kv nats.KeyValueConfig) {
	conn, js := memberJetStream(t, cluster, 0)
	js.CreateKeyValue(&kv)
	cluster.CreateKeyValue(t, conn, &kv)
}

func onAMemberConnection(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	conn := testnats.Connect(t, cluster.Servers[1].URL())
	js, _ := conn.JetStream()
	js.AddStream(&cfg)
}

func throughARelay(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	r := startRelayOn(t, cluster.Servers[0].URL())
	_, js := relayedJetStream(t, r)
	if _, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "relayed", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
}

func handedOn(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	createOn(cluster.JetStream(t), cfg)
}

func jetstreamPackage(t *testing.T, cluster *testnats.Cluster) {
	conn := testnats.Connect(t, cluster.URL())
	js, _ := jetstream.New(conn)
	if _, err := js.CreateKeyValue(context.Background(), jetstream.KeyValueConfig{Bucket: "jet", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
}

func throughFixture(t *testing.T, cluster *testnats.Cluster, conn *nats.Conn, cfg nats.StreamConfig, kv nats.KeyValueConfig) {
	cluster.CreateStream(t, cfg)
	cluster.CreateKeyValue(t, conn, &kv)
}

func oneCopy(t *testing.T, cluster *testnats.Cluster) {
	js := cluster.JetStream(t)
	if _, err := js.AddStream(&nats.StreamConfig{Name: "ONE", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	kv := nats.KeyValueConfig{Bucket: "one"}
	if _, err := js.CreateKeyValue(&kv); err != nil {
		t.Fatal(err)
	}
}

func discarded(t *testing.T, cluster *testnats.Cluster) {
	js := cluster.JetStream(t)
	_, _ = js.AddStream(&nats.StreamConfig{Name: "DISCARDED", Replicas: 3})
}

func abandoned(t *testing.T, cluster *testnats.Cluster) {
	js := cluster.JetStream(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = js.AddStream(&nats.StreamConfig{Name: "ABANDONED", Replicas: 3})
	}()
	<-done
}

func boundMethod(t *testing.T, cluster *testnats.Cluster) {
	js := cluster.JetStream(t)
	create := js.AddStream
	if _, err := create(&nats.StreamConfig{Name: "BOUND", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
}

func boundOnTheCall(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	add := cluster.JetStream(t).AddStream
	again := add
	again(&cfg)
}

func pointerAlias(t *testing.T, cluster *testnats.Cluster) {
	js := cluster.JetStream(t)
	cfg := nats.StreamConfig{Name: "ALIASED", Replicas: 1}
	pointer := &cfg
	pointer.Replicas = 3
	if _, err := js.AddStream(&cfg); err != nil {
		t.Fatal(err)
	}
}

func literalParameter(t *testing.T, cluster *testnats.Cluster) {
	create := func(js nats.JetStreamContext) { js.AddStream(&nats.StreamConfig{Name: "PARAMETER", Replicas: 3}) }
	create(cluster.JetStream(t))
}

func tableRow(t *testing.T, cluster *testnats.Cluster) {
	for _, row := range []struct {
		name string
		run  func(nats.JetStreamContext)
	}{
		{"three copies", func(js nats.JetStreamContext) { js.AddStream(&nats.StreamConfig{Name: "ROW", Replicas: 3}) }},
	} {
		row.run(cluster.JetStream(t))
	}
}

func mapEntry(t *testing.T, cluster *testnats.Cluster) {
	contexts := map[string]nats.JetStreamContext{}
	contexts["cluster"] = cluster.JetStream(t)
	contexts["cluster"].AddStream(&nats.StreamConfig{Name: "MAPPED", Replicas: 3})
}

func structFieldLater(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	s := &suite{}
	s.js = cluster.JetStream(t)
	s.js.AddStream(&cfg)
}

type loneSuite struct{ js nats.JetStreamContext }

func loneBound(t *testing.T, conn *nats.Conn, cfg nats.StreamConfig) {
	js, _ := conn.JetStream()
	add := js.AddStream
	add(&cfg)
	s := &loneSuite{js: js}
	s.js.AddStream(&cfg)
}

func loneServer(t *testing.T, conn *nats.Conn, cfg nats.StreamConfig) {
	js, _ := conn.JetStream()
	js.AddStream(&cfg)
	waited, _ := conn.JetStream(nats.MaxWait(0))
	waited.AddStream(&cfg)
	lone := testnats.Connect(t, testnats.URL(t))
	loneJS, _ := lone.JetStream()
	loneJS.AddStream(&cfg)
	option := nats.MaxWait(0)
	named, _ := lone.JetStream(option)
	named.AddStream(&cfg)
	createOn(loneJS, cfg)
}

type fake struct{ nats.JetStreamContext }

func (f fake) AddStream(cfg *nats.StreamConfig, opts ...nats.JSOpt) (*nats.StreamInfo, error) {
	return f.JetStreamContext.AddStream(cfg, opts...)
}

func throughAFake(t *testing.T, cluster *testnats.Cluster, cfg nats.StreamConfig) {
	fake{}.AddStream(&cfg)
}

func readOnly(t *testing.T, cluster *testnats.Cluster) { cluster.JetStream(t).StreamInfo("S") }
`,
	"internal/clustertest/create.go": "package clustertest\n\nimport (\n\t\"testing\"\n\n\t\"github.com/nats-io/nats.go\"\n\t\"github.com/sjawhar/envoy/internal/testnats\"\n)\n\nfunc Create(t *testing.T, c *testnats.Cluster, cfg nats.StreamConfig) { c.JetStream(t).AddStream(&cfg) }\n",
	"internal/production/create.go":  "package production\n\nimport (\n\t\"testing\"\n\n\t\"github.com/nats-io/nats.go\"\n\t\"github.com/sjawhar/envoy/internal/testnats\"\n)\n\nfunc Create(t *testing.T, c *testnats.Cluster, cfg nats.StreamConfig) { c.JetStream(t).AddStream(&cfg) }\n",
}

// The control of the check above: in a planted tree it finds a create a test sends itself through
// every kind of context made from a cluster, a helper's forwarded result, a declared variable, a
// relay in front of a member, a context handed to a function of the package or to a function
// literal, a table row's function, a map entry and a struct field set after the struct was made
// among them, and a create method bound to such a context; a create whose configuration's address
// went to a pointer that set three copies, and one whose results the test discards on its own
// goroutine; and nothing through the fixture, a lone server's context however its options are
// spelt or held, a fake context, at one copy, abandoned on a goroutine of its own, or in a package
// that is neither a test nor a helper.
func TestTheClusterCreateCheckFindsEveryShapeItNamesAndNothingElse(t *testing.T) {
	root := t.TempDir()
	for name, contents := range plantedClusterCreates {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found, err := directClusterCreates(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, create := range found {
		got = append(got, create.key())
	}
	slices.Sort(got)
	want := []string{
		"internal/clustertest/create.go Create",
		"internal/demo/demo_test.go boundMethod",
		"internal/demo/demo_test.go boundOnTheCall",
		"internal/demo/demo_test.go chained",
		"internal/demo/demo_test.go declared",
		"internal/demo/demo_test.go discarded",
		"internal/demo/demo_test.go held",
		"internal/demo/demo_test.go jetstreamPackage",
		"internal/demo/demo_test.go literalParameter",
		"internal/demo/demo_test.go mapEntry",
		"internal/demo/demo_test.go member",
		"internal/demo/demo_test.go onAMemberConnection",
		"internal/demo/demo_test.go pointerAlias",
		"internal/demo/demo_test.go structFieldLater",
		"internal/demo/demo_test.go tableRow",
		"internal/demo/demo_test.go throughARelay",
		"internal/demo/demo_test.go throughForwarded",
		"internal/demo/demo_test.go throughHelper",
		"internal/demo/demo_test.go throughHelperOfAHelper",
		"internal/demo/demo_test.go throughMemberHelper",
		"internal/demo/helpers_test.go createOn",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the check found %q in the planted tree, want %q", got, want)
	}
}
