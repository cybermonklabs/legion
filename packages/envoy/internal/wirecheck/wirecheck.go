// Package wirecheck is the test that holds a wire package's clients to its types (Check). It
// type-checks the client packages and names every value they encode into a request, or decode
// from an answer, whose type the wire package does not declare. The rule is type identity, not
// field likeness: a struct of the client's own with one field of the wire body is a finding as
// surely as a copy of the whole body, and so are a map, an anonymous struct and a value held as an
// interface. It reads only the standard library, and the compiler's export data for every
// package the clients import (go list -deps -export), so it adds no module to the one it runs in.
package wirecheck

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Finding is one value a call to the other party encodes or decodes with a type the wire package
// does not declare: Pos is the value's place in the source, relative to the directory Check ran
// in where it lies under it; Type is the value's type as its package spells it (envoy.Session),
// through one pointer, then one slice, then one pointer, or Untyped for a value whose static type
// is an interface; Call is the JSON call or transport helper it was passed to.
type Finding struct {
	Pos  token.Position
	Type string
	Call string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s: %s passed to %s", f.Pos, f.Type, f.Call)
}

// Untyped is Finding.Type for a value whose static type is an interface: it names no type the
// check could hold to the wire package, so it is a finding of its own.
const Untyped = "UNTYPED"

// jsonCalls are the JSON calls whose value argument Check judges, each with that argument's index.
var jsonCalls = map[string]int{
	"encoding/json.Marshal":           0,
	"encoding/json.MarshalIndent":     0,
	"encoding/json.Unmarshal":         1,
	"(*encoding/json.Decoder).Decode": 0,
	"(*encoding/json.Encoder).Encode": 0,
}

// requestBuilders are the calls that make a function a party call by building a request.
var requestBuilders = map[string]bool{
	"net/http.NewRequest":            true,
	"net/http.NewRequestWithContext": true,
}

// Check type-checks pkgs, the patterns go list resolves in dir, and returns every value their
// party calls encode or decode whose type wirePkg, an import path, does not declare.
//
// A party call is a function or method of pkgs (its function literals included) that sends a
// request to the other party or reads its answer: one that calls net/http.NewRequest or
// NewRequestWithContext, one of transport (each the go/types full name of a function of pkgs,
// such as "(*github.com/sjawhar/envoy/cmd/cipher.client).do"), or any function whose results
// include a *net/http.Response; one that takes a *net/http.Request or *net/http.Response; and a
// body reader that a party call calls: a function that hands one of its own parameters, or a
// value made from one, to json.Unmarshal's data or json.NewDecoder, such as a helper that decodes
// the answer bytes it was handed. In a party call, each value given to encoding/json's Marshal,
// MarshalIndent, Unmarshal, (*Decoder).Decode or (*Encoder).Encode is judged, but a Marshal or
// MarshalIndent whose result is used only converted to a string that goes into a field of a wire
// type's value is not: that string is taken as a payload carried inside a body, whatever is done
// with that field afterwards. A marshal used any other way, sent as the request body itself or put
// into a map or a struct of the client's own, is judged.
//
// A transport helper is a function of pkgs that hands one of its own parameters of type any to one
// of those JSON calls, directly or through another transport helper. The argument a caller gives
// for that parameter is judged at the call, wherever the helper or its caller is a party call, so
// a helper's out any is held to the caller's static type.
//
// A value passes when its type, through one pointer, then one slice, then one pointer, is a named
// type declared in wirePkg (an alias of one included). Check fails the test, rather than return,
// when go list or a type check fails, when a transport names no function of pkgs, and, as
// UNKNOWN, when any package of pkgs holds no party call: a check that saw nothing has not passed.
func Check(t testing.TB, dir string, pkgs []string, wirePkg string, transport []string) []Finding {
	t.Helper()
	listed, err := goList(dir, pkgs)
	if err != nil {
		t.Fatalf("wirecheck: %v", err)
		return nil
	}
	fset := token.NewFileSet()
	exports := map[string]string{}
	var targets []listedPackage
	for _, p := range listed {
		exports[p.ImportPath] = p.Export
		if !p.DepOnly {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		t.Fatalf("wirecheck: UNKNOWN: go list in %s matched no package of %v", dir, pkgs)
		return nil
	}
	compiled := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("go list gave no export data for %s", path)
		}
		return os.Open(file)
	})
	a := &analysis{wirePkg: wirePkg, transport: map[string]bool{}, helpers: map[string]map[int]bool{}}
	for _, name := range transport {
		a.transport[name] = true
	}
	for _, p := range targets {
		checked, err := typeCheck(fset, compiled, p)
		if err != nil {
			t.Fatalf("wirecheck: %v", err)
			return nil
		}
		a.packages = append(a.packages, checked)
	}
	a.collect()
	var missing []string
	for _, name := range transport {
		if a.functions[name] == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("wirecheck: transport %s names no function of %v in %s", strings.Join(missing, ", "), pkgs, dir)
		return nil
	}
	var blind []string
	for _, p := range a.packages {
		if !slices.ContainsFunc(p.funcs, func(f *function) bool { return f.party }) {
			blind = append(blind, p.path)
		}
	}
	if len(blind) > 0 {
		t.Fatalf("wirecheck: UNKNOWN: %s holds no call that sends a request or reads an answer, so the check saw nothing there",
			strings.Join(blind, ", "))
		return nil
	}
	findings := a.findings(fset, dir)
	slices.SortFunc(findings, func(x, y Finding) int {
		return cmp.Or(cmp.Compare(x.Pos.Filename, y.Pos.Filename), cmp.Compare(x.Pos.Line, y.Pos.Line), cmp.Compare(x.Pos.Column, y.Pos.Column))
	})
	return findings
}

// listedPackage is what Check reads of one package go list names.
type listedPackage struct {
	ImportPath string
	Dir        string
	Export     string
	GoFiles    []string
	CgoFiles   []string
	ImportMap  map[string]string
	DepOnly    bool
	Error      *struct{ Err string }
}

// goList is `go list -deps -export -json` of pkgs in dir: every package pkgs reach, each with the
// export data the compiler wrote for it. It fetches no module: go list reads the module cache alone
// (GOPROXY=off), since a fetch while a test runs would fail the test whenever the module proxy or
// the checksum database does, on a change that touched nothing it checks. The job that runs the
// check fetches the modules of pkgs in a step before the tests; a module the cache lacks fails the
// listing with the command that does it.
func goList(dir string, pkgs []string) ([]listedPackage, error) {
	args := append([]string{"list", "-deps", "-export", "-json=ImportPath,Dir,Export,GoFiles,CgoFiles,ImportMap,DepOnly,Error"}, pkgs...)
	command := exec.Command("go", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GOPROXY=off")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s in %s: %v\n%s%s", strings.Join(args, " "), dir, err, stderr.String(), fetchHint(dir, pkgs, stderr.String()))
	}
	var listed []listedPackage
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := decoder.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("read go list's answer in %s: %v", dir, err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list in %s: %s: %s%s", dir, p.ImportPath, p.Error.Err, fetchHint(dir, pkgs, p.Error.Err))
		}
		listed = append(listed, p)
	}
	return listed, nil
}

// fetchHint is what goList adds to an error whose text, refusal, says GOPROXY=off refused a module
// lookup: the command that fetches the modules of pkgs in dir before the tests, from the
// repository root, with dir made absolute so the command runs from there whatever directory the
// test ran Check in. It is empty for any other error.
func fetchHint(dir string, pkgs []string, refusal string) string {
	if !strings.Contains(refusal, "GOPROXY=off") {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return "\nThe module cache lacks a module this listing needs, and a test fetches no module while it runs. " +
		"Fetch them before the tests, from the repository root:\n  .github/scripts/go-fetch.sh deps " + dir + " " + strings.Join(pkgs, " ")
}

// checkedPackage is one package of pkgs, parsed and type-checked from its source.
type checkedPackage struct {
	path  string
	info  *types.Info
	funcs []*function
}

// function is one function or method declaration of a checked package.
type function struct {
	pkg  *checkedPackage
	decl *ast.FuncDecl
	name string
	// party is whether it sends a request to the other party or reads its answer.
	party bool
	// bodyReader is whether it decodes bytes or a reader one of its own parameters carries.
	bodyReader bool
	// parents maps each node of the body to the node holding it.
	parents map[ast.Node]ast.Node
	// params are its parameters of type any, by index.
	params map[*types.Var]int
	// codec are its JSON calls that are judged, each with its value argument.
	codec []jsonUse
	// calls are its calls of functions of pkgs, by callee full name.
	calls []call
}

type jsonUse struct {
	call  string
	value ast.Expr
}

type call struct {
	callee string
	args   []ast.Expr
}

// typeCheck parses p's Go files and type-checks them, importing every other package from the
// export data go list wrote.
func typeCheck(fset *token.FileSet, compiled types.Importer, p listedPackage) (*checkedPackage, error) {
	if len(p.CgoFiles) > 0 {
		return nil, fmt.Errorf("%s uses cgo, which this check does not type-check", p.ImportPath)
	}
	var files []*ast.File
	for _, name := range p.GoFiles {
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %v", p.ImportPath, err)
		}
		files = append(files, file)
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	config := types.Config{Importer: mappedImporter{compiled, p.ImportMap}}
	if _, err := config.Check(p.ImportPath, fset, files, info); err != nil {
		return nil, fmt.Errorf("type-check %s: %v", p.ImportPath, err)
	}
	checked := &checkedPackage{path: p.ImportPath, info: info}
	for _, file := range files {
		for _, decl := range file.Decls {
			if decl, ok := decl.(*ast.FuncDecl); ok && decl.Body != nil {
				if object, ok := info.Defs[decl.Name].(*types.Func); ok {
					checked.funcs = append(checked.funcs, &function{pkg: checked, decl: decl, name: object.FullName()})
				}
			}
		}
	}
	return checked, nil
}

// mappedImporter resolves an import path as the importing package's ImportMap names it.
type mappedImporter struct {
	compiled  types.Importer
	importMap map[string]string
}

func (m mappedImporter) Import(path string) (*types.Package, error) {
	if mapped, ok := m.importMap[path]; ok {
		path = mapped
	}
	return m.compiled.Import(path)
}

// analysis is one Check's reading of its packages.
type analysis struct {
	wirePkg   string
	transport map[string]bool
	packages  []*checkedPackage
	// functions are the packages' functions by full name.
	functions map[string]*function
	// helpers are the transport helpers: full name to the indices of their any parameters that
	// reach a JSON call.
	helpers map[string]map[int]bool
}

// collect reads every function: whether it is a party call, its judged JSON calls and its calls,
// then the body readers party calls call and the transport helpers, each to a fixed point.
func (a *analysis) collect() {
	a.functions = map[string]*function{}
	for _, p := range a.packages {
		for _, f := range p.funcs {
			a.functions[f.name] = f
		}
	}
	for _, p := range a.packages {
		for _, f := range p.funcs {
			a.read(f)
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range a.packages {
			for _, f := range p.funcs {
				if !f.party {
					continue
				}
				for _, c := range f.calls {
					if callee := a.functions[c.callee]; callee.bodyReader && !callee.party {
						callee.party = true
						changed = true
					}
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for _, p := range a.packages {
			for _, f := range p.funcs {
				for _, index := range f.helperParams(a) {
					if a.helpers[f.name] == nil {
						a.helpers[f.name] = map[int]bool{}
					}
					if !a.helpers[f.name][index] {
						a.helpers[f.name][index] = true
						changed = true
					}
				}
			}
		}
	}
}

// read records what f's body does: its party calls, JSON calls and calls of other functions.
func (a *analysis) read(f *function) {
	info := f.pkg.info
	f.params = map[*types.Var]int{}
	signature := info.Defs[f.decl.Name].(*types.Func).Type().(*types.Signature)
	all := map[*types.Var]bool{}
	for i := range signature.Params().Len() {
		param := signature.Params().At(i)
		all[param] = true
		if isHTTP(param.Type(), "Request") || isHTTP(param.Type(), "Response") {
			f.party = true
		}
		if isEmptyInterface(param.Type()) {
			f.params[param] = i
		}
	}
	f.parents = parentsOf(f.decl.Body)
	ast.Inspect(f.decl.Body, func(node ast.Node) bool {
		expr, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if signature, ok := info.TypeOf(expr.Fun).(*types.Signature); ok && returnsResponse(signature) {
			f.party = true
		}
		callee := calleeOf(info, expr)
		if callee == nil {
			return true
		}
		name := callee.Origin().FullName()
		if requestBuilders[name] || a.transport[name] {
			f.party = true
		}
		if index, ok := jsonCalls[name]; ok && index < len(expr.Args) {
			if (name != "encoding/json.Marshal" && name != "encoding/json.MarshalIndent") || !a.carriedAsString(f, expr) {
				f.codec = append(f.codec, jsonUse{call: name, value: expr.Args[index]})
			}
		}
		if (name == "encoding/json.Unmarshal" || name == "encoding/json.NewDecoder") && len(expr.Args) > 0 && mentions(info, expr.Args[0], all) {
			f.bodyReader = true
		}
		if a.functions[name] != nil {
			f.calls = append(f.calls, call{callee: name, args: expr.Args})
		}
		return true
	})
}

// helperParams are the indices of f's any parameters that reach a JSON call, directly or through
// a transport helper known so far.
func (f *function) helperParams(a *analysis) []int {
	var indices []int
	for _, use := range f.codec {
		if index, ok := f.ownParam(use.value); ok {
			indices = append(indices, index)
		}
	}
	for _, c := range f.calls {
		for index := range a.helpers[c.callee] {
			if index < len(c.args) {
				if own, ok := f.ownParam(c.args[index]); ok {
					indices = append(indices, own)
				}
			}
		}
	}
	return indices
}

// ownParam reports whether value is one of f's own any parameters, and its index.
func (f *function) ownParam(value ast.Expr) (int, bool) {
	ident, ok := ast.Unparen(value).(*ast.Ident)
	if !ok {
		return 0, false
	}
	param, ok := f.pkg.info.Uses[ident].(*types.Var)
	if !ok {
		return 0, false
	}
	index, ok := f.params[param]
	return index, ok
}

// findings judges every JSON value of a party call, and every argument a transport helper is
// handed where the helper or its caller is a party call.
func (a *analysis) findings(fset *token.FileSet, dir string) []Finding {
	var findings []Finding
	judge := func(f *function, value ast.Expr, callName string) {
		if _, own := f.ownParam(value); own {
			return
		}
		if name, ok := a.judge(f.pkg.info.TypeOf(value)); !ok {
			findings = append(findings, Finding{Pos: position(fset, dir, value.Pos()), Type: name, Call: callName})
		}
	}
	for _, p := range a.packages {
		for _, f := range p.funcs {
			if f.party {
				for _, use := range f.codec {
					judge(f, use.value, use.call)
				}
			}
			for _, c := range f.calls {
				if !f.party && !a.functions[c.callee].party {
					continue
				}
				for index := range a.helpers[c.callee] {
					if index < len(c.args) {
						judge(f, c.args[index], c.callee)
					}
				}
			}
		}
	}
	return findings
}

// judge reports whether a value of type t passes, and the name of its type when it does not.
func (a *analysis) judge(t types.Type) (string, bool) {
	base := types.Unalias(t)
	if pointer, ok := base.(*types.Pointer); ok {
		base = types.Unalias(pointer.Elem())
	}
	if slice, ok := base.(*types.Slice); ok {
		base = types.Unalias(slice.Elem())
	}
	if pointer, ok := base.(*types.Pointer); ok {
		base = types.Unalias(pointer.Elem())
	}
	if types.IsInterface(base) {
		return Untyped, false
	}
	if named, ok := base.(*types.Named); ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == a.wirePkg {
		return "", true
	}
	return types.TypeString(base, func(p *types.Package) string { return p.Name() }), false
}

// carriedAsString reports whether marshal, a json.Marshal or MarshalIndent call, is assigned to a
// variable whose every use is a conversion to a string type that goes into a wire type's field
// (intoWireField), directly or through a variable held for one (heldForWireField): a payload
// carried inside a body.
func (a *analysis) carriedAsString(f *function, marshal *ast.CallExpr) bool {
	variable, target := f.assignedTo(marshal)
	if variable == nil {
		return false
	}
	return f.everyUse(variable, target, func(ident *ast.Ident) bool {
		conversion, ok := f.parents[ident].(*ast.CallExpr)
		if !ok || len(conversion.Args) != 1 || conversion.Args[0] != ident {
			return false
		}
		if tv, ok := f.pkg.info.Types[conversion.Fun]; !ok || !tv.IsType() || !isString(tv.Type) {
			return false
		}
		return a.intoWireField(f, conversion) || a.heldForWireField(f, conversion)
	})
}

// assignedTo is the variable an assignment or declaration of expr gives it to, and the identifier
// that names it there: the first of a single value's left-hand sides (payload of `payload, err :=
// json.Marshal(v)`), or the one matching its place among several values. It is nil for any other
// use of expr and for the blank identifier.
func (f *function) assignedTo(expr ast.Expr) (types.Object, *ast.Ident) {
	var target *ast.Ident
	switch parent := f.parents[expr].(type) {
	case *ast.AssignStmt:
		for i, rhs := range parent.Rhs {
			if rhs != expr {
				continue
			}
			switch {
			case len(parent.Rhs) == 1 && len(parent.Lhs) > 0:
				target, _ = parent.Lhs[0].(*ast.Ident)
			case len(parent.Lhs) == len(parent.Rhs):
				target, _ = parent.Lhs[i].(*ast.Ident)
			}
		}
	case *ast.ValueSpec:
		for i, value := range parent.Values {
			if value != expr {
				continue
			}
			switch {
			case len(parent.Values) == 1 && len(parent.Names) > 0:
				target = parent.Names[0]
			case len(parent.Names) == len(parent.Values):
				target = parent.Names[i]
			}
		}
	}
	if target == nil || target.Name == "_" {
		return nil, nil
	}
	if object := f.pkg.info.Defs[target]; object != nil {
		return object, target
	}
	return f.pkg.info.Uses[target], target
}

// everyUse reports whether variable is used in f's body and every use passes ok. target, the
// identifier the assignment of interest names it by, is passed over: go/types records the left
// side of a plain `=` among the uses, and that assignment is the variable's own write.
func (f *function) everyUse(variable types.Object, target *ast.Ident, ok func(*ast.Ident) bool) bool {
	uses, passed := 0, true
	ast.Inspect(f.decl.Body, func(node ast.Node) bool {
		if ident, isIdent := node.(*ast.Ident); isIdent && ident != target && f.pkg.info.Uses[ident] == variable {
			uses++
			passed = passed && ok(ident)
		}
		return true
	})
	return uses > 0 && passed
}

// intoWireField reports whether expr goes into a field of a wire type's value: it is a composite
// literal's field value whose literal is a wire type (wire.Body{Payload: expr}), or the value
// assigned to a field of a wire-typed value (body.Payload = expr). A map, a struct of the client's
// own and an anonymous struct are none, since nothing holds what such a value carries to the wire
// package's types.
func (a *analysis) intoWireField(f *function, expr ast.Expr) bool {
	switch parent := f.parents[expr].(type) {
	case *ast.KeyValueExpr:
		literal, ok := f.parents[parent].(*ast.CompositeLit)
		return ok && parent.Value == expr && a.isWire(f.pkg.info.TypeOf(literal))
	case *ast.AssignStmt:
		if len(parent.Lhs) != len(parent.Rhs) {
			return false
		}
		for i, rhs := range parent.Rhs {
			if rhs == expr {
				field, ok := parent.Lhs[i].(*ast.SelectorExpr)
				return ok && a.isWire(f.pkg.info.TypeOf(field.X))
			}
		}
	}
	return false
}

// heldForWireField reports whether expr is assigned to a variable every use of which goes into a
// wire type's field, by value or as its address (wire.Body{Detail: &text}).
func (a *analysis) heldForWireField(f *function, expr ast.Expr) bool {
	variable, target := f.assignedTo(expr)
	if variable == nil {
		return false
	}
	return f.everyUse(variable, target, func(ident *ast.Ident) bool {
		if a.intoWireField(f, ident) {
			return true
		}
		address, ok := f.parents[ident].(*ast.UnaryExpr)
		return ok && address.Op == token.AND && a.intoWireField(f, address)
	})
}

// isWire reports whether a value of type t passes judge: a type the wire package declares, through
// one pointer, then one slice, then one pointer. A missing type, as a package name's is, is none.
func (a *analysis) isWire(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := a.judge(t)
	return ok
}

// calleeOf is the function call names, nil for a call of a function value or a conversion.
func calleeOf(info *types.Info, call *ast.CallExpr) *types.Func {
	fun := ast.Unparen(call.Fun)
	switch generic := fun.(type) {
	case *ast.IndexExpr:
		fun = generic.X
	case *ast.IndexListExpr:
		fun = generic.X
	}
	switch fun := fun.(type) {
	case *ast.Ident:
		callee, _ := info.Uses[fun].(*types.Func)
		return callee
	case *ast.SelectorExpr:
		if selection, ok := info.Selections[fun]; ok {
			callee, _ := selection.Obj().(*types.Func)
			return callee
		}
		callee, _ := info.Uses[fun.Sel].(*types.Func)
		return callee
	}
	return nil
}

// parentsOf maps each node under root to the node holding it.
func parentsOf(root ast.Node) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})
	return parents
}

// mentions reports whether expr names one of params anywhere in it, as bytes.NewReader(body) and
// body[1:] name body.
func mentions(info *types.Info, expr ast.Expr, params map[*types.Var]bool) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok {
			if variable, ok := info.Uses[ident].(*types.Var); ok && params[variable] {
				found = true
			}
		}
		return !found
	})
	return found
}

func returnsResponse(signature *types.Signature) bool {
	for i := range signature.Results().Len() {
		if isHTTP(signature.Results().At(i).Type(), "Response") {
			return true
		}
	}
	return false
}

// isHTTP reports whether t is a pointer to net/http's type of that name.
func isHTTP(t types.Type, name string) bool {
	pointer, ok := types.Unalias(t).(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := types.Unalias(pointer.Elem()).(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == name
}

func isEmptyInterface(t types.Type) bool {
	iface, ok := t.Underlying().(*types.Interface)
	return ok && iface.NumMethods() == 0
}

func isString(t types.Type) bool {
	basic, ok := t.Underlying().(*types.Basic)
	return ok && basic.Info()&types.IsString != 0
}

// position is pos, its file named relative to dir where it lies under it.
func position(fset *token.FileSet, dir string, pos token.Pos) token.Position {
	position := fset.Position(pos)
	if base, err := filepath.Abs(dir); err == nil {
		if relative, err := filepath.Rel(base, position.Filename); err == nil && filepath.IsLocal(relative) {
			position.Filename = relative
		}
	}
	return position
}
