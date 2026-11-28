//go:build ignore

// Gh-writes reads, out of gh's own source, every command gh has and whether it writes to GitHub,
// which rule 20 of `rig.sh health` (health.ts's `writes`) reads a recorded call's command by:
//
//	go run gh-writes.go <gh source dir>
//
// prints, as JSON, each command gh's root adds by the path it is typed by (`pr merge`), with the
// file that declares it, its aliases, and, for one that writes, the first write it can reach
// (`write`). gh-writes.ts runs it over the source of the tag the worker image pins and keeps its
// answer in gh-writes.json. It imports the standard library alone and belongs to no module, as
// gh-selectors.go does; the build constraint keeps it out of every package build.
//
// A command writes when any call of its own code, or of code that code reaches, sends GitHub a
// write: a GraphQL mutation (a call of a `Mutate` method, gh's and go-gh's, or a GraphQL text
// that opens with `mutation`), an HTTP request whose method is POST, PATCH, PUT or DELETE (the
// method written as a literal or as net/http's constant, wherever the function names it), a
// `git push` (the refs of a repository on GitHub), or the OAuth flow of github.com/cli/oauth,
// whose device and browser flows POST the device-code request and the token exchange to GitHub's
// login endpoints (`gh auth login`, `gh auth refresh`). A POST to a service of GitHub's the world
// does not model is a write the world cannot fake, so `gh send-telemetry`, whose telemetry client
// (internal/barista) POSTs to https://cafe.github.com (pkg/cmd/send-telemetry/send_telemetry.go:21
// at v2.98.0), writes too (the implementer's ruling of 2026-11-18, by the architect's 15:08Z one
// on `auth login`). What is reached is read by name: a function call by the package its
// qualifier imports, or its own package's; a method call by every method of that name declared
// in the caller's package or a package it imports, directly or not, which reads more than a call
// can reach and never less. A command's reading starts at the function that builds it and stops
// at the function of every other command, so a group that runs on its own (`codespace ports`) is
// read apart from the commands it adds.
//
// Commands the root adds at run time are none of gh's own and are not read: help topics, the
// extensions and aliases a configuration installs, and the stubs that suggest installing
// GitHub's official extensions. Any other command gh adds the derivation cannot read (a
// constructor it cannot resolve, a `cobra.Command` with no literal `Use`) is an error, never a
// command left out, since a command missing from the list is one rule 20 reads as no write.
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gh-writes <gh source dir>")
		os.Exit(2)
	}
	commands, err := derive(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-writes: %v\n", err)
		os.Exit(1)
	}
	out, err := json.MarshalIndent(commands, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh-writes: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

// Command is one command of gh's, as gh-writes.json keeps it: the file of its `cobra.Command`,
// its aliases, whether gh hides it, whether it runs on its own (one that does not prints its
// help), and whether it writes, with the first write its reading found (`write`: where, what,
// and the functions it is reached through). A command whose every request is the one its call
// names (`byRequest`) writes by that request, never by its name, and is no write here.
type Command struct {
	File      string   `json:"file"`
	Aliases   []string `json:"aliases,omitempty"`
	Hidden    bool     `json:"hidden,omitempty"`
	Runs      bool     `json:"runs"`
	ByRequest bool     `json:"byRequest,omitempty"`
	Writes    bool     `json:"writes"`
	Write     string   `json:"write,omitempty"`
}

// byRequest are the commands that send the request their call names, which a call is read by: `gh
// api`, whose method is GET unless the call gives a field or `--input` (then POST) or names one
// with `-X`/`--method` (pkg/cmd/api/api.go:289 and :325 at v2.98.0), and whose GraphQL call runs
// the operation its document names (the architect's ruling of 2026-11-18).
var byRequest = map[string]bool{"api": true}

// unit is one function, method or function literal of gh's source, with the imports of the file
// declaring it.
type unit struct {
	pkg     string
	file    string
	label   string
	body    *ast.BlockStmt
	imports map[string]string
}

func (u *unit) name() string { return u.label }

// declName is a function's name, and a method's with its receiver's type (`App.Delete`).
func declName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	for {
		switch t := recv.(type) {
		case *ast.StarExpr:
			recv = t.X
		case *ast.IndexExpr:
			recv = t.X
		case *ast.IndexListExpr:
			recv = t.X
		case *ast.Ident:
			return t.Name + "." + fn.Name.Name
		default:
			return fn.Name.Name
		}
	}
}

type source struct {
	root    string
	module  string
	fset    *token.FileSet
	funcs   map[string]*unit
	methods map[string][]*unit
	names   map[string]string
	deps    map[string]map[string]bool
	closure map[string]map[string]bool
	// inline are the commands built where they are added, a function literal called there
	// (`AddCommand(func() *cobra.Command {…}())`) or a `cobra.Command` literal: each is read as
	// that command's, and never as part of the function it is written in.
	inline map[ast.Node]*unit
}

// inspect walks u's body as ast.Inspect does, passing over every command built inline that is
// not u.
func (s *source) inspect(u *unit, visit func(ast.Node) bool) {
	ast.Inspect(u.body, func(n ast.Node) bool {
		if built := s.inline[n]; built != nil && built != u {
			return false
		}
		return visit(n)
	})
}

// skipped are the directories of gh's source that hold no code a command runs.
var skipped = map[string]bool{".git": true, "testdata": true, "acceptance": true, "third-party": true}

func load(root string) (*source, error) {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return nil, fmt.Errorf("read gh's go.mod: %w", err)
	}
	module := ""
	for _, line := range strings.Split(string(mod), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			module = strings.TrimSpace(rest)
		}
	}
	if module == "" {
		return nil, fmt.Errorf("gh's go.mod names no module")
	}
	src := &source{
		root: root, module: module, fset: token.NewFileSet(),
		funcs: map[string]*unit{}, methods: map[string][]*unit{}, names: map[string]string{},
		deps: map[string]map[string]bool{}, closure: map[string]map[string]bool{},
		inline: map[ast.Node]*unit{},
	}
	type parsed struct {
		pkg, rel string
		file     *ast.File
	}
	var files []parsed
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipped[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(src.fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", rel, err)
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		pkg := module
		if dir != "." {
			pkg = module + "/" + dir
		}
		src.names[pkg] = file.Name.Name
		files = append(files, parsed{pkg, filepath.ToSlash(rel), file})
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, one := range files {
		imports := map[string]string{}
		for _, spec := range one.file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			alias := src.defaultName(path)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "_" || alias == "." {
				continue
			}
			imports[alias] = path
			if _, inTree := src.names[path]; inTree {
				if src.deps[one.pkg] == nil {
					src.deps[one.pkg] = map[string]bool{}
				}
				src.deps[one.pkg][path] = true
			}
		}
		for _, decl := range one.file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			u := &unit{pkg: one.pkg, file: one.rel, label: declName(fn), body: fn.Body, imports: imports}
			if fn.Recv == nil {
				src.funcs[one.pkg+"."+fn.Name.Name] = u
			} else {
				src.methods[fn.Name.Name] = append(src.methods[fn.Name.Name], u)
			}
		}
	}
	return src, nil
}

// defaultName is the name a file refers to an import by when it names none: an in-tree package's
// own name, and for any other the last element of its path that is no major version, without a
// `.vN` suffix or a `go-` prefix.
func (s *source) defaultName(path string) string {
	if name, inTree := s.names[path]; inTree {
		return name
	}
	parts := strings.Split(path, "/")
	last := parts[len(parts)-1]
	if regexp.MustCompile(`^v[0-9]+$`).MatchString(last) && len(parts) > 1 {
		last = parts[len(parts)-2]
	}
	last = regexp.MustCompile(`\.v[0-9]+$`).ReplaceAllString(last, "")
	return strings.TrimPrefix(last, "go-")
}

// reaches is every in-tree package pkg imports, directly or not, and pkg itself.
func (s *source) reaches(pkg string) map[string]bool {
	if seen, done := s.closure[pkg]; done {
		return seen
	}
	seen := map[string]bool{pkg: true}
	stack := []string{pkg}
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for dep := range s.deps[next] {
			if !seen[dep] {
				seen[dep] = true
				stack = append(stack, dep)
			}
		}
	}
	s.closure[pkg] = seen
	return seen
}

func (s *source) where(pos token.Pos) string {
	at := s.fset.Position(pos)
	rel, err := filepath.Rel(s.root, at.Filename)
	if err != nil {
		rel = at.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), at.Line)
}

var (
	mutationText  = regexp.MustCompile(`^\s*mutation\b`)
	writeMethods  = map[string]bool{"POST": true, "PATCH": true, "PUT": true, "DELETE": true}
	httpConstants = map[string]bool{"MethodPost": true, "MethodPatch": true, "MethodPut": true, "MethodDelete": true}
	mutateMethods = map[string]bool{"Mutate": true, "MutateWithContext": true}
	gitCommands   = map[string]bool{"Command": true, "AuthenticatedCommand": true}
)

// readPosts are what a function names when the POST it sends writes nothing: GitHub's GraphQL
// endpoint (internal/ghinstance's `GraphQLEndpoint`), which takes every query as a POST and writes
// only for a mutation, read by its text; and `releases/generate-notes`, whose notes GitHub's REST
// reference says "are not saved anywhere".
var readPosts = map[string]bool{"GraphQLEndpoint": true, "generate-notes": true}

// write is the first write u's own body sends, or "" for none: where it is and what it is.
func (s *source) write(u *unit) string {
	readPost := false
	s.inspect(u, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			if ident, isIdent := n.X.(*ast.Ident); isIdent && readPosts[n.Sel.Name] &&
				u.imports[ident.Name] == s.module+"/internal/ghinstance" {
				readPost = true
			}
		case *ast.BasicLit:
			if text, err := strconv.Unquote(n.Value); err == nil && n.Kind == token.STRING && readPosts[text] {
				readPost = true
			}
		}
		return true
	})
	found := ""
	note := func(pos token.Pos, what string) {
		if found == "" {
			found = s.where(pos) + " " + what
		}
	}
	method := func(pos token.Pos, name string) {
		if name != "POST" || !readPost {
			note(pos, "an HTTP "+name)
		}
	}
	s.inspect(u, func(node ast.Node) bool {
		if found != "" {
			return false
		}
		switch n := node.(type) {
		case *ast.CallExpr:
			if sel, isSel := n.Fun.(*ast.SelectorExpr); isSel {
				if mutateMethods[sel.Sel.Name] {
					note(n.Pos(), "a GraphQL mutation (`"+sel.Sel.Name+"`)")
				}
				if gitCommands[sel.Sel.Name] {
					for _, arg := range n.Args {
						if lit, isLit := arg.(*ast.BasicLit); isLit && lit.Value == `"push"` {
							note(n.Pos(), "a `git push`")
						}
					}
				}
			}
		case *ast.CompositeLit:
			if len(n.Elts) > 0 {
				if lit, isLit := n.Elts[0].(*ast.BasicLit); isLit && lit.Value == `"push"` {
					note(n.Pos(), "a `git push`")
				}
			}
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(n.Value)
			if err != nil {
				return true
			}
			if writeMethods[text] {
				method(n.Pos(), text)
			} else if mutationText.MatchString(text) {
				note(n.Pos(), "a GraphQL mutation (its text)")
			}
		case *ast.SelectorExpr:
			ident, isIdent := n.X.(*ast.Ident)
			if !isIdent {
				return true
			}
			path := u.imports[ident.Name]
			if path == "net/http" && httpConstants[n.Sel.Name] {
				method(n.Pos(), strings.ToUpper(strings.TrimPrefix(n.Sel.Name, "Method")))
			}
			if path == "github.com/cli/oauth" || strings.HasPrefix(path, "github.com/cli/oauth/") {
				note(n.Pos(), "the OAuth flow of github.com/cli/oauth (`oauth."+n.Sel.Name+"`)")
			}
		}
		return true
	})
	return found
}

// callees are the functions and methods u's body names: each function it calls or refers to, by
// its qualifier's package or u's own, and each method of the name it calls on a value, of u's
// package and every package that package reaches.
func (s *source) callees(u *unit) []*unit {
	var out []*unit
	seen := map[*unit]bool{}
	add := func(v *unit) {
		if v != nil && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	reach := s.reaches(u.pkg)
	s.inspect(u, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			add(s.funcs[u.pkg+"."+n.Name])
		case *ast.SelectorExpr:
			if ident, isIdent := n.X.(*ast.Ident); isIdent {
				if path, imported := u.imports[ident.Name]; imported {
					add(s.funcs[path+"."+n.Sel.Name])
					return false
				}
			}
		case *ast.CallExpr:
			sel, isSel := n.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			if ident, isIdent := sel.X.(*ast.Ident); isIdent {
				if _, imported := u.imports[ident.Name]; imported {
					return true
				}
			}
			for _, method := range s.methods[sel.Sel.Name] {
				if reach[method.pkg] {
					add(method)
				}
			}
		}
		return true
	})
	return out
}

// node is one command of gh's tree as its constructor builds it: `unit` is the function whose
// `cobra.Command` literal it is, `from` the one its parent calls, which is unit or returns what
// unit builds, and `hooks` its PersistentPreRun and PersistentPostRun functions, which cobra
// runs around every command under it.
type node struct {
	path     string
	unit     *unit
	from     *unit
	aliases  []string
	hidden   bool
	runs     bool
	hooks    []*unit
	children []*node
}

const cobraPath = "github.com/spf13/cobra"

// command reads the `cobra.Command` literal u builds: its name (the first word of its `Use`),
// aliases, whether it is hidden, and whether it runs (a `Run` or `RunE`). A constructor that
// builds none and returns what one other constructor builds (`return newCmdX(f, nil)`) is read
// as that one, whose function the command's reading then starts at.
func (s *source) command(u *unit) (*node, error) {
	var lit *ast.CompositeLit
	var delegates []*unit
	s.inspect(u, func(n ast.Node) bool {
		if lit != nil {
			return false
		}
		if ret, isReturn := n.(*ast.ReturnStmt); isReturn && len(ret.Results) == 1 {
			if call, isCall := ret.Results[0].(*ast.CallExpr); isCall && !isFuncLit(call.Fun) {
				if delegate := s.constructor(u, call); delegate != nil {
					delegates = append(delegates, delegate)
				}
			}
		}
		c, isLit := n.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		sel, isSel := c.Type.(*ast.SelectorExpr)
		if !isSel || sel.Sel.Name != "Command" {
			return true
		}
		if ident, isIdent := sel.X.(*ast.Ident); !isIdent || u.imports[ident.Name] != cobraPath {
			return true
		}
		lit = c
		return false
	})
	if lit == nil && len(delegates) == 1 {
		return s.command(delegates[0])
	}
	if lit == nil {
		return nil, fmt.Errorf("%s (%s) builds no cobra.Command literal", u.name(), u.file)
	}
	out := &node{unit: u}
	name := ""
	for _, elt := range lit.Elts {
		kv, isKV := elt.(*ast.KeyValueExpr)
		if !isKV {
			continue
		}
		key, _ := kv.Key.(*ast.Ident)
		if key == nil {
			continue
		}
		switch key.Name {
		case "Use":
			use, isLit := kv.Value.(*ast.BasicLit)
			if !isLit {
				return nil, fmt.Errorf("%s (%s): its Use is no literal", u.name(), s.where(kv.Pos()))
			}
			text, err := strconv.Unquote(use.Value)
			if err != nil {
				return nil, err
			}
			name = strings.Fields(text)[0]
		case "Aliases":
			list, isList := kv.Value.(*ast.CompositeLit)
			if !isList {
				return nil, fmt.Errorf("%s (%s): its Aliases is no literal", u.name(), s.where(kv.Pos()))
			}
			for _, a := range list.Elts {
				alias, isLit := a.(*ast.BasicLit)
				if !isLit {
					return nil, fmt.Errorf("%s (%s): an alias is no literal", u.name(), s.where(a.Pos()))
				}
				text, err := strconv.Unquote(alias.Value)
				if err != nil {
					return nil, err
				}
				out.aliases = append(out.aliases, text)
			}
		case "Hidden":
			if ident, isIdent := kv.Value.(*ast.Ident); isIdent && ident.Name == "true" {
				out.hidden = true
			}
		case "Run", "RunE":
			out.runs = true
		case "PersistentPreRun", "PersistentPreRunE", "PersistentPostRun", "PersistentPostRunE":
			lit, isLit := kv.Value.(*ast.FuncLit)
			if !isLit {
				return nil, fmt.Errorf("%s (%s): its %s is no function literal", u.name(), s.where(kv.Pos()), key.Name)
			}
			out.hooks = append(out.hooks, &unit{
				pkg: u.pkg, file: u.file, label: u.label + "'s " + key.Name, body: lit.Body, imports: u.imports,
			})
		}
	}
	if name == "" {
		return nil, fmt.Errorf("%s (%s): its cobra.Command has no Use", u.name(), u.file)
	}
	out.path = name
	return out, nil
}

// constructor is the function a call names, by its qualifier's package or u's own, or the
// function literal it calls.
func (s *source) constructor(u *unit, call *ast.CallExpr) *unit {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return s.funcs[u.pkg+"."+fun.Name]
	case *ast.SelectorExpr:
		if ident, isIdent := fun.X.(*ast.Ident); isIdent {
			if path, imported := u.imports[ident.Name]; imported {
				return s.funcs[path+"."+fun.Sel.Name]
			}
		}
	case *ast.FuncLit:
		return s.built(u, fun, fun.Body)
	}
	return nil
}

// built is the command built inline at expr in u, its body being body.
func (s *source) built(u *unit, expr ast.Node, body *ast.BlockStmt) *unit {
	lit := &unit{
		pkg: u.pkg, file: u.file, label: u.label + "'s command at " + s.where(expr.Pos()),
		body: body, imports: u.imports,
	}
	s.inline[expr] = lit
	return lit
}

// children are the constructors of the commands u adds (`AddCommand`, cmdutil's `AddGroup`),
// each a call of one, a variable assigned one in u, or a command built there. Every command the
// root adds at run time is built by a constructor of the root's own package (its help topics,
// extensions, aliases and official extension stubs), and every command of gh's own by one of the
// command's package, so the root's own constructors are passed over.
func (s *source) children(u *unit, root bool) ([]*unit, error) {
	assigned := map[string]*ast.CallExpr{}
	s.inspect(u, func(n ast.Node) bool {
		if assign, isAssign := n.(*ast.AssignStmt); isAssign && len(assign.Lhs) == len(assign.Rhs) {
			for i, lhs := range assign.Lhs {
				ident, isIdent := lhs.(*ast.Ident)
				call, isCall := assign.Rhs[i].(*ast.CallExpr)
				if isIdent && isCall {
					assigned[ident.Name] = call
				}
			}
		}
		return true
	})
	var out []*unit
	var problem error
	s.inspect(u, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || problem != nil {
			return problem == nil
		}
		sel, isSel := call.Fun.(*ast.SelectorExpr)
		if !isSel {
			return true
		}
		var args []ast.Expr
		switch {
		case sel.Sel.Name == "AddCommand":
			args = call.Args
		case sel.Sel.Name == "AddGroup" && len(call.Args) > 2:
			if ident, isIdent := sel.X.(*ast.Ident); isIdent && u.imports[ident.Name] == s.module+"/pkg/cmdutil" {
				args = call.Args[2:]
			}
		}
		for _, arg := range args {
			var made *ast.CallExpr
			var child *unit
			switch a := arg.(type) {
			case *ast.CallExpr:
				made = a
			case *ast.UnaryExpr:
				if _, isLit := a.X.(*ast.CompositeLit); isLit && a.Op == token.AND {
					child = s.built(u, a, &ast.BlockStmt{List: []ast.Stmt{&ast.ExprStmt{X: a}}})
				}
			case *ast.Ident:
				made = assigned[a.Name]
			}
			if made != nil {
				child = s.constructor(u, made)
			}
			if child == nil {
				problem = fmt.Errorf("%s adds a command whose constructor cannot be read (%s)", u.name(), s.where(arg.Pos()))
				return false
			}
			if root && child.pkg == u.pkg {
				continue
			}
			out = append(out, child)
		}
		return true
	})
	return out, problem
}

func isFuncLit(expr ast.Expr) bool {
	_, is := expr.(*ast.FuncLit)
	return is
}

func (s *source) tree(u *unit, path string, root bool) (*node, error) {
	n, err := s.command(u)
	if err != nil {
		return nil, err
	}
	n.from = u
	if !root {
		n.path = strings.TrimSpace(path + " " + n.path)
	} else {
		n.path = ""
	}
	kids, err := s.children(n.unit, root)
	if err != nil {
		return nil, err
	}
	for _, kid := range kids {
		child, err := s.tree(kid, n.path, false)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, child)
	}
	return n, nil
}

// firstWrite is the first write reachable from start, not passing through the constructor of any
// other command (`stops`), with the functions it is reached through.
func (s *source) firstWrite(start *unit, stops map[*unit]bool) string {
	type step struct {
		at  *unit
		via []string
	}
	seen := map[*unit]bool{start: true}
	queue := []step{{start, []string{start.name()}}}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if found := s.write(next.at); found != "" {
			return found + ", reached by " + strings.Join(next.via, " → ")
		}
		for _, callee := range s.callees(next.at) {
			if seen[callee] || stops[callee] {
				continue
			}
			seen[callee] = true
			queue = append(queue, step{callee, append(slices.Clone(next.via), callee.name())})
		}
	}
	return ""
}

func derive(root string) (map[string]Command, error) {
	src, err := load(root)
	if err != nil {
		return nil, err
	}
	rootUnit := src.funcs[src.module+"/pkg/cmd/root.NewCmdRoot"]
	if rootUnit == nil {
		return nil, fmt.Errorf("gh's source has no pkg/cmd/root.NewCmdRoot")
	}
	top, err := src.tree(rootUnit, "", true)
	if err != nil {
		return nil, err
	}
	constructors := map[*unit]bool{}
	type reading struct {
		n     *node
		hooks []*unit
	}
	var all []reading
	var walk func(n *node, hooks []*unit)
	walk = func(n *node, hooks []*unit) {
		constructors[n.unit] = true
		constructors[n.from] = true
		all = append(all, reading{n, hooks})
		for _, child := range n.children {
			walk(child, append(slices.Clone(hooks), n.hooks...))
		}
	}
	walk(top, nil)
	out := map[string]Command{}
	for _, one := range all {
		n := one.n
		if n == top {
			continue
		}
		if _, twice := out[n.path]; twice {
			return nil, fmt.Errorf("gh adds the command %q twice", n.path)
		}
		stops := map[*unit]bool{}
		for u := range constructors {
			if u != n.unit {
				stops[u] = true
			}
		}
		found := ""
		if n.runs && !byRequest[n.path] {
			// Its own function, then each hook cobra runs around it, nearest the root first.
			for _, start := range append([]*unit{n.unit}, one.hooks...) {
				if found = src.firstWrite(start, stops); found != "" {
					break
				}
			}
		}
		aliases := slices.Clone(n.aliases)
		sort.Strings(aliases)
		out[n.path] = Command{
			File: n.unit.file, Aliases: aliases, Hidden: n.hidden, Runs: n.runs,
			ByRequest: byRequest[n.path], Writes: found != "", Write: found,
		}
	}
	for path := range byRequest {
		if _, found := out[path]; !found {
			return nil, fmt.Errorf("gh has no command %q, which a call is read by the request of", path)
		}
	}
	return out, nil
}
