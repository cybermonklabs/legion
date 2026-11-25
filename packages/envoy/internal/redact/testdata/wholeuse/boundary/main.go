// Command boundary is the census of redact.OAuth2, the one way this repository calls
// golang.org/x/oauth2 and github.com/coreos/go-oidc: it reads, from the libraries' own export data
// at the version go.mod pins, every exported function, method and interface method of
// golang.org/x/oauth2, of each of its subpackages this repository imports, and of go-oidc, that has
// an error result, and every exported package-level variable of a function type with one. Then it
// reads every non-test file of the packages it is given, and fails on each reference to one of them
// that is not the function of a call that is itself the only argument of redact.OAuth2 or of
// oauth2call.Call, or the second argument of redact.OAuth2 beside a value for a call that answers
// an error alone, naming the line. A call through an interface or a type parameter resolves to a
// method this repository or the standard library declares, so it counts as well when that method
// has an error result and an exported type of the libraries implements the receiver's interface or
// the type parameter's constraint (types.Implements): the value it is called on can be the
// library's. So does each reference to a function or method of the libraries that answers an HTTP
// client or transport (*net/http.Client, a net/http.RoundTripper), whose every request calls the
// library and answers its error through net/http: it passes only where a token source the
// boundary made is among its arguments (oauth2.NewClient(ctx, redact.TokenSource(source))), and
// fails where it builds a token source of its own (Config.Client). It fails too on each value
// whose type implements oauth2.TokenSource handed to a function of another module, or set in a
// struct field another module declares, that is not a token source the boundary made
// (redact.TokenSource, or a type of package oauth2call): a library that asks it for a token prints
// the refusal it answers. No list of names is written here: the closed set is the libraries'.
//
// It loads the module once for each GOOS/GOARCH of -targets and each tag list of -tags, so a file
// one build constraint keeps out is read under another; the gate passes the targets CI builds and
// the tag sets .github/scripts/go_build_tags.py reads off the module.
//
// Usage, from this directory: GOWORK=off go run ./boundary -dir ../../../.. ./...
package main

import (
	"cmp"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"wholeuse/load"
)

const (
	oauth2Path       = "golang.org/x/oauth2"
	goOIDCPath       = "github.com/coreos/go-oidc"
	impersonatePath  = "google.golang.org/api/impersonate"
	cloudSQLConnPath = "cloud.google.com/go/cloudsqlconn"
	modulePath       = "github.com/sjawhar/envoy"
	redactPath       = modulePath + "/internal/redact"
	oauth2callPath   = modulePath + "/oauth2call"
	boundaryWrapper  = "BoundTokenSource"
)

// under reports whether path is the package root or a package below it.
func under(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }

// library reports whether path is a package of a library whose calls cross the boundary.
func library(path string) bool {
	return under(path, oauth2Path) || under(path, goOIDCPath) || path == impersonatePath || path == cloudSQLConnPath
}

// textOnly reports whether path is a library that hands back only the text of a refusal, with no
// response or request to read its record from: each call of it must be read under CallScope.
func textOnly(path string) bool {
	return under(path, goOIDCPath) || under(path, oauth2Path+"/google") || path == impersonatePath || path == cloudSQLConnPath
}

// dispatchLibrary reports whether library's exported types are used to resolve calls through an
// interface. cloudsqlconn's Dialer implements io.Closer, so including it would turn every Close of
// an unrelated io.Closer into a library call; its direct methods are already in the closed API.
func dispatchLibrary(path string) bool {
	return under(path, oauth2Path) || under(path, goOIDCPath)
}

func main() {
	dir := flag.String("dir", ".", "the module directory to load from")
	listAPI := flag.Bool("api", false, "print each library function the census holds calls of to the boundary")
	targets := flag.String("targets", load.Host, "comma-separated GOOS/GOARCH pairs the module is loaded for")
	tagSets := flag.String("tags", "", "semicolon-separated build tag lists the module is loaded with, each with every target; an empty list is no tags")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	// The module is loaded once for each target and tag set, so a file a build constraint keeps
	// from one of them (a darwin-only file on a Linux gate, an e2e file) is read under another.
	// Each load's target is kept, so the summary names, for each target, what its builds read.
	loaded := load.Modules("boundary", *dir, *targets, *tagSets, packages.NeedName|packages.NeedFiles|packages.NeedSyntax|packages.NeedTypes|packages.NeedTypesInfo|packages.NeedImports, patterns...)
	pkgs, targetOf := loaded.Packages, loaded.Target

	// The closed set: each library package any loaded package imports, read from its types, with
	// the targets whose builds import it.
	api := map[string]string{}
	perPackage := map[string]int{}
	importedFor := map[string]map[string]bool{}
	seen := map[string]bool{}
	var tokenSource *types.Interface
	var libTypes []types.Type
	for _, pkg := range pkgs {
		for _, imported := range pkg.Types.Imports() {
			path := imported.Path()
			if path == oauth2Path && tokenSource == nil {
				if obj, ok := imported.Scope().Lookup("TokenSource").(*types.TypeName); ok {
					tokenSource, _ = obj.Type().Underlying().(*types.Interface)
				}
			}
			if !library(path) {
				continue
			}
			if importedFor[path] == nil {
				importedFor[path] = map[string]bool{}
			}
			importedFor[path][targetOf[pkg]] = true
			if seen[path] {
				continue
			}
			seen[path] = true
			for name, kind := range libraryAPI(imported) {
				api[name] = kind
				perPackage[path]++
			}
			if dispatchLibrary(path) {
				libTypes = append(libTypes, libraryTypes(imported)...)
			}
		}
	}
	if len(api) == 0 || tokenSource == nil {
		fmt.Println("UNKNOWN: no golang.org/x/oauth2 package is imported, so there is nothing to check")
		os.Exit(2)
	}
	implementers := map[*types.Interface]string{}

	root, _ := filepath.Abs(*dir)
	var lines []string
	// builtFor is each non-test file's targets, the loads that compile it; fileRefs its references.
	builtFor := map[string]map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			if name := pkg.Fset.Position(file.Pos()).Filename; !strings.HasSuffix(name, "_test.go") {
				if builtFor[name] == nil {
					builtFor[name] = map[string]bool{}
				}
				builtFor[name][targetOf[pkg]] = true
			}
		}
	}
	fileRefs := map[string]int{}
	walked := map[string]bool{}
	refs, handoffs, failed, files := 0, 0, 0, 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			name := pkg.Fset.Position(file.Pos()).Filename
			// A file that more than one load builds is read once, by the first.
			if strings.HasSuffix(name, "_test.go") || walked[name] {
				continue
			}
			walked[name] = true
			files++
			var stack []ast.Node
			ast.Inspect(file, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				if failure, checked := handoff(pkg.TypesInfo, n, tokenSource); checked {
					handoffs++
					pos := pkg.Fset.Position(n.Pos())
					rel, err := filepath.Rel(root, pos.Filename)
					if err != nil {
						rel = pos.Filename
					}
					verdict := "ok"
					if failure != "" {
						verdict, failed = "FAIL", failed+1
					}
					lines = append(lines, fmt.Sprintf("%s\t%s:%d:%d\ttoken source or HTTP client handed to another module\t%s", verdict, rel, pos.Line, pos.Column, cmp.Or(failure, "a token source the boundary made, or a client RefusalClient made")))
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				key := objectKey(pkg.TypesInfo.Uses[id])
				kind, inAPI := api[key]
				via := ""
				if !inAPI {
					if key, via = libraryDispatch(pkg.TypesInfo, stack, libTypes, implementers); key == "" {
						return true
					}
				}
				refs++
				fileRefs[name]++
				pos := pkg.Fset.Position(id.Pos())
				rel, err := filepath.Rel(root, pos.Filename)
				if err != nil {
					rel = pos.Filename
				}
				verdict, why := "ok", "called across redact.OAuth2"
				scoped := throughScope(pkg.TypesInfo, stack)
				switch {
				case textOnlyObject(pkg.TypesInfo.Uses[id], key) && !scoped:
					verdict, why, failed = "FAIL", "not read under a CallScope: write s := redact.Scope[T](ctx), then redact.OAuth2(s.Read(call(s.Context(), ...)))", failed+1
				case strings.HasPrefix(kind, httpClientKind):
					if failure := clientBuilt(pkg.TypesInfo, stack, tokenSource); failure != "" {
						verdict, why, failed = "FAIL", failure, failed+1
					} else if kind != httpClientKind && !throughBoundary(pkg.TypesInfo, stack) {
						verdict, why, failed = "FAIL", "a library HTTP client not called as the only argument of redact.OAuth2 or oauth2call.Call", failed+1
					} else {
						why = "a library HTTP client over a RefusalClient base and a token source the boundary made"
					}
				case !throughBoundary(pkg.TypesInfo, stack):
					verdict, why, failed = "FAIL", "not called as the only argument of redact.OAuth2 or oauth2call.Call", failed+1
				case scoped:
					why = "read under CallScope and across redact.OAuth2"
				}
				if via != "" {
					why += "; called through " + via
				}
				lines = append(lines, fmt.Sprintf("%s\t%s:%d:%d\t%s\t%s", verdict, rel, pos.Line, pos.Column, key, why))
				return true
			})
		}
	}
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Println(line)
	}
	for _, path := range slices.Sorted(maps.Keys(perPackage)) {
		fmt.Printf("API\t%s\t%d functions, methods and function variables with an error result\tfor %s\n", path, perPackage[path], strings.Join(slices.Sorted(maps.Keys(importedFor[path])), ", "))
	}
	for _, target := range loaded.Order {
		targetFiles, targetRefs := 0, 0
		for name, built := range builtFor {
			if built[target] {
				targetFiles++
				targetRefs += fileRefs[name]
			}
		}
		fmt.Printf("TARGET\t%s\tnon-test files=%d\treferences=%d\n", target, targetFiles, targetRefs)
	}
	if *listAPI {
		var names []string
		for name, kind := range api {
			names = append(names, "LIBRARY\t"+kind+"\t"+name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Println(name)
		}
	}
	fmt.Printf("SUMMARY\tlibrary API=%d\tpackages=%d\tnon-test files=%d\treferences=%d\ttoken-source hand-offs=%d\tfailures=%d\n", len(api), len(pkgs), files, refs, handoffs, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// httpClientKind is the kind of a library function or method that answers an HTTP client or
// transport; one that answers an error too is that kind with errorResultSuffix.
const (
	httpClientKind    = "http client"
	errorResultSuffix = " with an error result"
)

// libraryAPI is each exported function, method, interface method and function variable of pkg
// with an error result, and each exported function and method that answers an HTTP client or
// transport (httpClientKind), by its key (objectKey), with its kind.
func libraryAPI(pkg *types.Package) map[string]string {
	out := map[string]string{}
	answersHTTP := httpAnswer(pkg)
	kind := func(t types.Type, plain string) string {
		switch {
		case answersHTTP(t) && errorResult(t):
			return httpClientKind + errorResultSuffix
		case answersHTTP(t):
			return httpClientKind
		case errorResult(t):
			return plain
		}
		return ""
	}
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !obj.Exported() {
			continue
		}
		switch obj := obj.(type) {
		case *types.Func:
			if k := kind(obj.Type(), "function"); k != "" {
				out[objectKey(obj)] = k
			}
		case *types.Var:
			if errorResult(obj.Type()) {
				out[objectKey(obj)] = "function variable"
			}
		case *types.TypeName:
			named, ok := obj.Type().(*types.Named)
			if !ok {
				continue
			}
			for i := range named.NumMethods() {
				if m := named.Method(i); m.Exported() {
					if k := kind(m.Type(), "method"); k != "" {
						out[objectKey(m)] = k
					}
				}
			}
			if iface, ok := named.Underlying().(*types.Interface); ok {
				for i := range iface.NumMethods() {
					if m := iface.Method(i); m.Exported() && errorResult(m.Type()) {
						out[objectKey(m)] = "interface method"
					}
				}
			}
		}
	}
	return out
}

// httpAnswer is whether a function type answers an HTTP client or transport - a *net/http.Client,
// or a value that implements net/http.RoundTripper - as pkg imports net/http. A library that
// does not import it answers neither.
func httpAnswer(pkg *types.Package) func(types.Type) bool {
	var client types.Type
	var roundTripper *types.Interface
	for _, imported := range pkg.Imports() {
		if imported.Path() != "net/http" {
			continue
		}
		if obj, ok := imported.Scope().Lookup("Client").(*types.TypeName); ok {
			client = types.NewPointer(obj.Type())
		}
		if obj, ok := imported.Scope().Lookup("RoundTripper").(*types.TypeName); ok {
			roundTripper, _ = obj.Type().Underlying().(*types.Interface)
		}
	}
	return func(t types.Type) bool {
		sig, ok := t.Underlying().(*types.Signature)
		if !ok || client == nil || roundTripper == nil {
			return false
		}
		for i := range sig.Results().Len() {
			r := sig.Results().At(i).Type()
			if types.Identical(r, client) || !types.IsInterface(r) && types.Implements(r, roundTripper) || types.Identical(r.Underlying(), roundTripper) {
				return true
			}
		}
		return false
	}
}

// libraryTypes is each exported type pkg declares that is not generic: a value of one can stand
// behind an interface or a type parameter this repository calls a method of.
func libraryTypes(pkg *types.Package) []types.Type {
	var out []types.Type
	scope := pkg.Scope()
	for _, name := range scope.Names() {
		if obj, ok := scope.Lookup(name).(*types.TypeName); ok && obj.Exported() && !obj.IsAlias() {
			if named, ok := obj.Type().(*types.Named); ok && named.TypeParams().Len() == 0 {
				out = append(out, named)
			}
		}
	}
	return out
}

// libraryDispatch answers, for the identifier at the top of stack, the method it names and how a
// library's code can answer it, where it names the method of a selector whose receiver is an
// interface or a type parameter, the method has an error result, and an exported type of the
// libraries - the type itself, or a pointer to it - implements that interface or the type
// parameter's constraint (types.Implements); otherwise "". implementers caches each interface's
// first implementing library type ("" for none).
func libraryDispatch(info *types.Info, stack []ast.Node, libTypes []types.Type, implementers map[*types.Interface]string) (key, via string) {
	if len(stack) < 2 {
		return "", ""
	}
	id := stack[len(stack)-1].(*ast.Ident)
	sel, ok := stack[len(stack)-2].(*ast.SelectorExpr)
	if !ok || sel.Sel != id {
		return "", ""
	}
	s := info.Selections[sel]
	if s == nil || s.Kind() == types.FieldVal {
		return "", ""
	}
	fn, ok := s.Obj().(*types.Func)
	if !ok || !errorResult(fn.Type()) {
		return "", ""
	}
	recv := s.Recv()
	if p, ok := recv.(*types.Pointer); ok {
		recv = p.Elem()
	}
	var iface *types.Interface
	what := ""
	if tp, ok := recv.(*types.TypeParam); ok {
		iface, _ = tp.Constraint().Underlying().(*types.Interface)
		what = "the type parameter " + tp.Obj().Name() + " " + types.TypeString(tp.Constraint(), nil)
	} else {
		iface, _ = recv.Underlying().(*types.Interface)
		what = "the interface " + types.TypeString(recv, nil)
	}
	if iface == nil || iface.NumMethods() == 0 {
		return "", ""
	}
	impl, cached := implementers[iface]
	if !cached {
		for _, t := range libTypes {
			if types.Implements(t, iface) || !types.IsInterface(t) && types.Implements(types.NewPointer(t), iface) {
				impl = types.TypeString(t, nil)
				break
			}
		}
		implementers[iface] = impl
	}
	if impl == "" {
		return "", ""
	}
	return fn.FullName(), what + ", which " + impl + " implements"
}

// clientBuilt says why the library HTTP client the identifier at the top of stack names fails, or
// answers "": it must be the function of a call one of whose arguments is a token source and
// every such argument a token source the boundary made, so the client's every request asks the
// boundary for its token. One built from a token source of the library's own (Config.Client), or
// named without a call, answers its refusal raw through net/http.
func clientBuilt(info *types.Info, stack []ast.Node, tokenSource *types.Interface) string {
	call, _, ok := calledAt(stack)
	if !ok {
		return "a library HTTP client named without a call"
	}
	sources := 0
	for _, arg := range call.Args {
		tv, ok := info.Types[arg]
		if !ok || tv.Type == nil {
			continue
		}
		if contextValue(tv.Type) && !refusalContext(info, arg) {
			return "a library HTTP client over a context whose oauth2.HTTPClient is not a call of redact.RefusalClient, oauth2call.RefusalClient or CallScope.Context"
		}
		if !types.Implements(tv.Type, tokenSource) {
			continue
		}
		sources++
		if !boundaryMade(tv.Type) {
			return fmt.Sprintf("a library HTTP client over %s, a token source the boundary did not make", types.ExprString(arg))
		}
	}

	if sources == 0 {
		return "a library HTTP client built over a token source of the library's own, whose refusal its requests answer raw through net/http"
	}
	return ""
}

// contextValue reports whether t is context.Context.
func contextValue(t types.Type) bool {
	named, ok := t.(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
}

// refusalContext reports whether value is CallScope.Context, or context.WithValue whose
// oauth2.HTTPClient value is a RefusalClient.
func refusalContext(info *types.Info, value ast.Expr) bool {
	call, ok := ast.Unparen(value).(*ast.CallExpr)
	if !ok {
		return false
	}
	fn := calleeFunc(info, call)
	if fn == nil || fn.Pkg() == nil {
		return false
	}
	if fn.Pkg().Path() == redactPath && fn.Name() == "Context" {
		return true
	}
	return fn.Pkg().Path() == "context" && fn.Name() == "WithValue" && len(call.Args) == 3 &&
		oauth2ClientKey(info, call.Args[1]) && refusalClient(info, call.Args[2])
}

// errorResult reports whether t is a function type one of whose results is error.
func errorResult(t types.Type) bool {
	sig, ok := t.Underlying().(*types.Signature)
	if !ok {
		return false
	}
	errorType := types.Universe.Lookup("error").Type()
	for i := range sig.Results().Len() {
		if types.Identical(sig.Results().At(i).Type(), errorType) {
			return true
		}
	}
	return false
}

// objectKey names obj the same way whichever package's type check produced it.
func objectKey(obj types.Object) string {
	switch obj := obj.(type) {
	case *types.Func:
		return obj.Origin().FullName()
	case *types.Var:
		if obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope() {
			return obj.Pkg().Path() + "." + obj.Name()
		}
	}
	return ""
}

// textOnlyObject reports whether obj, or its key after an interface dispatch, is a call of a
// text-only library.
func textOnlyObject(obj types.Object, key string) bool {
	if fn, ok := obj.(*types.Func); ok && fn.Pkg() != nil && textOnly(fn.Pkg().Path()) {
		return true
	}
	return strings.Contains(key, goOIDCPath) || strings.Contains(key, oauth2Path+"/google") ||
		strings.Contains(key, impersonatePath) || strings.Contains(key, cloudSQLConnPath)
}

// throughBoundary reports whether the identifier at the top of stack is the function of a call
// that is the only argument of redact.OAuth2 or oauth2call.Call, directly or as the value and
// error CallScope.Read or Err answers.
func throughBoundary(info *types.Info, stack []ast.Node) bool {
	return directBoundary(info, stack) || throughScope(info, stack)
}

// directBoundary is throughBoundary's direct form.
func directBoundary(info *types.Info, stack []ast.Node) bool {
	call, before, ok := calledAt(stack)
	if !ok || before < 1 {
		return false
	}
	outer, ok := stack[before-1].(*ast.CallExpr)
	return ok && boundaryCall(info, outer, call)
}

// throughScope reports whether the library call is the one CallScope.Read or Err reads, and that
// scope call is the one redact.OAuth2 or oauth2call.Call reads.
func throughScope(info *types.Info, stack []ast.Node) bool {
	call, before, ok := calledAt(stack)
	if !ok || before < 1 {
		return false
	}
	scopeCall, ok := stack[before-1].(*ast.CallExpr)
	if !ok || !scopeRead(info, scopeCall, call) || before < 2 {
		return false
	}
	outer, ok := stack[before-2].(*ast.CallExpr)
	return ok && boundaryCall(info, outer, scopeCall)
}

// calledAt is the call whose function is stack's top identifier, and the index before that call.
func calledAt(stack []ast.Node) (*ast.CallExpr, int, bool) {
	i := len(stack) - 1
	id, ok := stack[i].(*ast.Ident)
	if !ok {
		return nil, 0, false
	}
	fun := ast.Expr(id)
	if i > 0 {
		if sel, ok := stack[i-1].(*ast.SelectorExpr); ok && sel.Sel == id {
			fun, i = sel, i-1
		}
	}
	if i < 1 {
		return nil, 0, false
	}
	call, ok := stack[i-1].(*ast.CallExpr)
	return call, i - 1, ok && call.Fun == fun
}

// scopeRead reports whether call is CallScope.Read or Err with library as its error-bearing
// argument.
func scopeRead(info *types.Info, call *ast.CallExpr, library *ast.CallExpr) bool {
	fn := calleeFunc(info, call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != redactPath || (fn.Name() != "Read" && fn.Name() != "Err") {
		return false
	}
	for _, arg := range call.Args {
		if arg == library {
			return true
		}
	}
	return false
}

// boundaryCall reports whether outer is redact.OAuth2 or oauth2call.Call reading inner.
func boundaryCall(info *types.Info, outer *ast.CallExpr, inner ast.Expr) bool {
	if !(len(outer.Args) == 1 && outer.Args[0] == inner || len(outer.Args) == 2 && outer.Args[1] == inner) {
		return false
	}
	fn := calleeFunc(info, outer)
	return fn != nil && fn.Pkg() != nil &&
		(fn.Pkg().Path() == redactPath && fn.Name() == "OAuth2" || fn.Pkg().Path() == oauth2callPath && fn.Name() == "Call")
}

// calleeFunc is the function call calls, past an explicit instantiation, or nil.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	callee := ast.Unparen(call.Fun)
	if index, ok := callee.(*ast.IndexExpr); ok {
		callee = index.X
	}
	var name *ast.Ident
	switch c := callee.(type) {
	case *ast.Ident:
		name = c
	case *ast.SelectorExpr:
		name = c.Sel
	}
	if name == nil {
		return nil
	}
	fn, _ := info.Uses[name].(*types.Func)
	return fn
}

// handoff checks n where it hands a value to another module: a call of a function another module
// declares, a composite literal's field another module declares, or an assignment to one. It
// answers whether n is such a hand-off of a value whose type implements tokenSource, or of a
// *net/http.Client to a library whose calls cross the boundary (clientLibrary) or under x/oauth2's
// context key (context.WithValue(ctx, oauth2.HTTPClient, client)), and, where that value is not a
// token source the boundary made or a client RefusalClient made, why it fails: such a library
// builds a request's credentials itself and prints the refusal it answers, and only a client
// RefusalClient made records them for the boundary.
func handoff(info *types.Info, n ast.Node, tokenSource *types.Interface) (failure string, checked bool) {
	check := func(value ast.Expr, to string, toLibrary bool) {
		tv, ok := info.Types[value]
		if !ok || tv.IsType() || tv.Type == nil {
			return
		}
		if httpClient(tv.Type) {
			if !toLibrary {
				return
			}
			checked = true
			if !refusalClient(info, value) {
				failure = fmt.Sprintf("%s (%s) handed to %s, not a call of redact.RefusalClient or oauth2call.RefusalClient", types.ExprString(value), tv.Type, to)
			}
			return
		}
		if !types.Implements(tv.Type, tokenSource) {
			return
		}
		checked = true
		if !boundaryMade(tv.Type) {
			failure = fmt.Sprintf("%s (%s) handed to %s, not through redact.TokenSource", types.ExprString(value), tv.Type, to)
		}
	}
	switch n := n.(type) {
	case *ast.CallExpr:
		fn := calleeFunc(info, n)
		if fn == nil || fn.Pkg() == nil || strings.HasPrefix(fn.Pkg().Path(), modulePath) {
			return "", false
		}
		toLibrary := clientLibrary(fn.Pkg().Path()) ||
			fn.Pkg().Path() == "context" && fn.Name() == "WithValue" && len(n.Args) == 3 && oauth2ClientKey(info, n.Args[1])
		for _, arg := range n.Args {
			check(arg, fn.FullName(), toLibrary)
		}
	case *ast.KeyValueExpr:
		if key, ok := n.Key.(*ast.Ident); ok {
			if field, ok := info.Uses[key].(*types.Var); ok && foreignField(field) {
				check(n.Value, "the field "+field.Pkg().Path()+"."+field.Name(), clientLibrary(field.Pkg().Path()))
			}
		}
	case *ast.AssignStmt:
		for i, lhs := range n.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || i >= len(n.Rhs) {
				continue
			}
			if field, ok := info.Uses[sel.Sel].(*types.Var); ok && foreignField(field) {
				check(n.Rhs[i], "the field "+field.Pkg().Path()+"."+field.Name(), clientLibrary(field.Pkg().Path()))
			}
		}
	}
	return failure, checked
}

// clientLibrary reports whether path is a package of a library whose calls cross the boundary
// (library), or google.golang.org/api/option, whose WithHTTPClient hands a client to
// google.golang.org/api/impersonate.
func clientLibrary(path string) bool {
	return library(path) || path == "google.golang.org/api/option"
}

// oauth2ClientKey reports whether key is x/oauth2's context key for the HTTP client it sends with,
// oauth2.HTTPClient.
func oauth2ClientKey(info *types.Info, key ast.Expr) bool {
	sel, ok := ast.Unparen(key).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	v, ok := info.Uses[sel.Sel].(*types.Var)
	return ok && v.Pkg() != nil && v.Pkg().Path() == oauth2Path && v.Name() == "HTTPClient"
}

// foreignField reports whether v is a struct field another module declares.
func foreignField(v *types.Var) bool {
	return v.IsField() && v.Pkg() != nil && !strings.HasPrefix(v.Pkg().Path(), modulePath)
}

// httpClient reports whether t is *net/http.Client.
func httpClient(t types.Type) bool {
	p, ok := t.(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := p.Elem().(*types.Named)
	return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "net/http" && named.Obj().Name() == "Client"
}

// refusalClient reports whether value is a call of redact.RefusalClient, oauth2call.RefusalClient
// or CallScope.Client.
func refusalClient(info *types.Info, value ast.Expr) bool {
	call, ok := ast.Unparen(value).(*ast.CallExpr)
	if !ok {
		return false
	}
	fn := calleeFunc(info, call)
	if fn == nil || fn.Pkg() == nil {
		return false
	}
	return fn.Pkg().Path() == redactPath && (fn.Name() == "RefusalClient" || fn.Name() == "Client") ||
		fn.Pkg().Path() == oauth2callPath && fn.Name() == "RefusalClient"
}

// boundaryMade reports whether t is a token source the boundary made: *redact.BoundTokenSource, or
// a type of package oauth2call.
func boundaryMade(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	return path == redactPath && named.Obj().Name() == boundaryWrapper || path == oauth2callPath
}
