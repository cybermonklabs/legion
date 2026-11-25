#!/usr/bin/env bash
# The declaration comparison of check c-windows-kept: every xid window a Go file under
# packages/envoy declares, at a baseline and at a head, compared declaration by declaration, so
# that no window this branch inherits is loosened, dropped or hidden, apart from root R29's
# approved restore exception, and no new window holds more than the Verification names.
#
#   bash packages/dispatch/e2e/acceptance/xid-window-bounds.sh BASELINE HEAD
#
# BASELINE and HEAD are each a revision of this checkout (any name git resolves to a commit:
# "$BASELINE_SHA", the commit the pull request body names, and HEAD), or a directory that holds a
# tree of this repository, read from disk as it stands: the scratch copy a negative control
# mutates. Nothing is checked out and the working copy is never written.
#
# What it reads. At each side it finds every Go file under packages/envoy that names XidWindowOf
# or XidBatchedWindow (`git grep` at a revision, grep in a directory), takes the package
# directories that hold one (`git archive` of those directories into a directory of this run's own
# at a revision), and reads every Go file of those packages with go/parser and go/scanner, through
# a reader this script writes into its own directory and builds with the go on PATH. A
# declaration is one of:
#   - a use of XidWindowOf or XidBatchedWindow, or of a package-level window a package names
#     (specRestoreWindow, specVersionWindow, xidRigWindow), wherever it stands;
#   - a call of a window helper, a function of the package whose own body declares a window and
#     that no single test case names (measureBatched), read as the windows that helper declares;
#   - the definitions of the window helpers themselves, XidWindowOf and XidBatchedWindow.
# Each is keyed by where it stands, never by its line: its package, its test function or the
# package-level table that holds it, the t.Run subtest and the case of a table it stands in (a
# route row or a step, by its name), and its place among the declarations that share that key. A
# step function a single case names (runXidSteps' {"result recorded", xidWindowResultRecorded}) is
# keyed by that case. Its value is its whole expression with every comment and all formatting
# taken out (the go/scanner tokens of its text, each string literal requoted), each package-level
# constant or window it names written out in full, and adjacent string constants joined, so a
# window reads the same however its text is wrapped. Beside it stand its statement count, which
# the comparison prints, and the tests that run it: each test function of its package that reaches
# its declaration through the package's own references.
#
# What it prints: both commits in full (or the directories), the count of declarations each side
# holds, and `diff -u` of the two lists. Then one line for each declaration that differs:
#   RESTORE: a named restore assertion with its baseline and head windows - the two route-table rows
#            that restore the approved text, which use specRestoreWindow at 17, the definition of
#            specRestoreWindow, and the dedicated upload test
#            TestXidWindowUploadRestoringTheApprovedText, which uses XidWindowOf(18);
#   MISSING: a baseline declaration the head no longer holds;
#   CHANGED: a baseline declaration the head holds with another window, with both statement counts;
#   HIDDEN:  a baseline declaration some test ran at the baseline that no test reaches at the head;
#   NEW:     a declaration the head adds, and whether it is a case the Verification names at its
#            batched limit (`accepted`) or not (`refused`): the node routes' test
#            TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill, and the route-table rows of the
#            Verification uploads above the bound and of the writes on the deep spec that defer
#            their block index (c-render-at-version), each at storetest.XidBatchedWindow.
# A new case cannot hide or replace a baseline one: a baseline key the head lacks is MISSING
# whatever the head adds. The run fails when either side reads no declaration, and on any MISSING,
# CHANGED, HIDDEN or refused NEW line and any restore assertion the head does not hold, naming
# each. Its last line, printed only once every comparison passed, is
#   PASS: xid-window-bounds.sh complete <N> comparisons
# N being the declarations compared: the baseline's and the head's new ones.
#
# Inputs: the two arguments; git, go, grep, tar, diff and sort on PATH. Each unmet input is exit 2;
# a failed check prints `FAIL: <section> <what>: <observed>` and exits 1.

set -euo pipefail
set +x

usage() {
  cat >&2 <<'USAGE'
Usage: bash packages/dispatch/e2e/acceptance/xid-window-bounds.sh BASELINE HEAD
       (each a revision of this checkout, such as "$BASELINE_SHA" and HEAD, or a directory holding
       a tree of this repository, read from disk)
USAGE
}

# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[[ $# -eq 2 && -n "$1" && -n "$2" ]] || {
  usage
  exit 2
}
require_commands cut diff git go grep sort tar wc

readonly driver=xid-window-bounds.sh
# The checkout a revision is read out of: the one this file stands in. A run whose two sides are
# both directories needs none.
repo="$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel 2>/dev/null)" || repo=""
readonly repo

work_dir
cleanup() {
  local code=$?
  rm -rf "$work"
  exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# What a file names when it declares an xid window.
readonly window_pattern='XidWindowOf|XidBatchedWindow'
# The route-table rows the restore exception names, the window both use, and the dedicated upload
# test it names: the keys the reader below gives them.
readonly api_package='internal/dispatch/api:api'
readonly restore_rows=(
  "$api_package:var documentRouteXidWindows/case \"the spec edit restoring the approved text under an open approval request\"#1"
  "$api_package:var documentRouteXidWindows/case \"the spec upload restoring the approved text under an open approval request\"#1"
)
readonly restore_window="$api_package:var specRestoreWindow#1"
readonly restore_test="$api_package:TestXidWindowUploadRestoringTheApprovedText#1"
readonly restore_row_count=17 restore_test_count=18
# The new cases this Verification names, each at storetest.XidBatchedWindow: the rows of the
# documents table that c-render-at-version names, and every declaration the node routes' test alone
# runs (c-windows-kept).
readonly accepted_rows=(
  "the Verification upload above the bound on an issue that has not converted"
  "the Verification upload above the bound on a converted issue"
  "the edit of a spec of more than 2,000 blocks nested six deep under an open approval request"
  "the anchored comment on a spec of more than 2,000 blocks nested six deep"
  "the anchored ask on a spec of more than 2,000 blocks nested six deep"
  "the accept of a suggestion on a spec of more than 2,000 blocks nested six deep"
  "the named version of a spec of more than 2,000 blocks nested six deep"
  "the rebuild of a spec of more than 2,000 blocks nested six deep"
)
readonly accepted_test=TestXidWindowsOfTheNodeRoutesTheDrainAndTheFill
readonly batched=XidBatchedWindow

# --- the reader ----------------------------------------------------------------------------------

# The reader of one side: given the directory that holds packages/envoy's tree and the package
# directories under it to read, it prints one line for each declaration, its fields separated by
# tabs: key, statement count, the tests that run it (comma-separated, `-` for none), value, and
# where it stands (file:line, for a reader of the output). No field is empty or holds a tab or a
# newline: each string literal of a value is requoted, and each label of a key quoted.
mkdir "$work/reader"
printf 'module xidwindowbounds\n\ngo 1.21\n' >"$work/reader/go.mod"
cat >"$work/reader/main.go" <<'GO'
// Command xidwindowbounds prints the xid window declarations of the Go packages it is given; the
// head comment of xid-window-bounds.sh says what a declaration, its key and its value are.
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	windowOf      = "XidWindowOf"
	batchedWindow = "XidBatchedWindow"
	// The marks a written-out constant stands between until its strings are joined.
	openMark  = "\u27e8"
	closeMark = "\u27e9"
	// How deep a constant that names a constant is written out.
	resolveDepth = 8
)

type tok struct {
	kind token.Token
	text string
}

type fileInfo struct {
	rel string
	ast *ast.File
}

type pkgInfo struct {
	dir, name string
	files     []*fileInfo
	// values is each package-level var or const that has a value of its own, by name.
	values map[string]ast.Expr
	// names is every package-level function, method, var and const name.
	names map[string]bool
	// named is each package-level value that is a window.
	named map[string]bool
	// direct is each function's own window declarations, by its name.
	direct map[string][]use
	// helpers is each function whose own body declares a window and that no single case names.
	helpers map[string]bool
	// mapped is the case paths that name each package-level name.
	mapped map[string][]string
	// refs is the package-level names each top-level declaration names.
	refs map[string]map[string]bool
	// owners is the tests that reach each top-level declaration.
	owners map[string][]string
}

type use struct{ count, value string }

type record struct {
	path, count, owners, value, where string
}

var (
	fset    = token.NewFileSet()
	sources = map[*token.File][]byte{}
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: xidwindowbounds ROOT DIR...")
		os.Exit(2)
	}
	root := os.Args[1]
	var records []record
	for _, dir := range os.Args[2:] {
		pkgs, err := readDir(root, dir)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		for _, p := range pkgs {
			records = append(records, p.declarations()...)
		}
	}
	seen := map[string]int{}
	lines := make([]string, 0, len(records))
	for _, r := range records {
		seen[r.path]++
		key := r.path + "#" + strconv.Itoa(seen[r.path])
		lines = append(lines, strings.Join([]string{key, r.count, r.owners, r.value, r.where}, "\t"))
	}
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Println(line)
	}
}

// readDir parses every Go file directly in root/dir: one package for each package clause.
func readDir(root, dir string) ([]*pkgInfo, error) {
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return nil, err
	}
	byName := map[string]*pkgInfo{}
	var order []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, entry.Name()))
		src, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		parsed, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("%s does not parse: %v", rel, err)
		}
		sources[fset.File(parsed.Pos())] = src
		name := parsed.Name.Name
		p, ok := byName[name]
		if !ok {
			p = &pkgInfo{dir: dir, name: name}
			byName[name] = p
			order = append(order, name)
		}
		p.files = append(p.files, &fileInfo{rel: rel, ast: parsed})
	}
	sort.Strings(order)
	pkgs := make([]*pkgInfo, 0, len(order))
	for _, name := range order {
		pkgs = append(pkgs, byName[name])
	}
	return pkgs, nil
}

// text is the source of n.
func text(n ast.Node) []byte {
	file := fset.File(n.Pos())
	return sources[file][file.Offset(n.Pos()):file.Offset(n.End())]
}

func where(n ast.Node) string {
	position := fset.Position(n.Pos())
	return position.Filename + ":" + strconv.Itoa(position.Line)
}

// scan is the tokens of src with every comment, and every newline the scanner reads as a
// semicolon, taken out, and each string literal requoted, so two spellings of one string read
// alike.
func scan(src []byte) []tok {
	files := token.NewFileSet()
	file := files.AddFile("", files.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	var out []tok
	for {
		_, kind, lit := s.Scan()
		if kind == token.EOF {
			return out
		}
		switch {
		case kind == token.SEMICOLON && lit != ";":
			continue
		case kind == token.STRING:
			if value, err := strconv.Unquote(lit); err == nil {
				lit = strconv.Quote(value)
			}
		case kind.IsLiteral():
		default:
			lit = kind.String()
		}
		out = append(out, tok{kind, lit})
	}
}

// resolve writes out each package-level value toks names, but XidBatchedWindow, a selector's
// field and a composite literal's key.
func (p *pkgInfo) resolve(toks []tok, depth int) []tok {
	out := make([]tok, 0, len(toks))
	for i, t := range toks {
		value, held := p.values[t.text]
		if t.kind != token.IDENT || !held || depth >= resolveDepth || t.text == batchedWindow ||
			(i > 0 && toks[i-1].kind == token.PERIOD) || (i+1 < len(toks) && toks[i+1].kind == token.COLON) {
			out = append(out, t)
			continue
		}
		inner := p.resolve(scan(text(value)), depth+1)
		if len(inner) == 1 {
			out = append(out, inner[0])
			continue
		}
		out = append(out, tok{token.LPAREN, openMark})
		out = append(out, inner...)
		out = append(out, tok{token.RPAREN, closeMark})
	}
	return out
}

// words is n's text as the comparison reads it: scanned, its constants written out, and each
// sum of two string literals joined into one.
func (p *pkgInfo) words(n ast.Node) string {
	toks := p.resolve(scan(text(n)), 0)
	for changed := true; changed; {
		changed = false
		out := make([]tok, 0, len(toks))
		for i := 0; i < len(toks); i++ {
			if i+2 < len(toks) && toks[i].text == openMark && toks[i+1].kind == token.STRING && toks[i+2].text == closeMark {
				out = append(out, toks[i+1])
				i += 2
				changed = true
				continue
			}
			if i+2 < len(toks) && toks[i].kind == token.STRING && toks[i+1].kind == token.ADD && toks[i+2].kind == token.STRING {
				left, _ := strconv.Unquote(toks[i].text)
				right, _ := strconv.Unquote(toks[i+2].text)
				out = append(out, tok{token.STRING, strconv.Quote(left + right)})
				i += 2
				changed = true
				continue
			}
			out = append(out, toks[i])
		}
		toks = out
	}
	words := make([]string, len(toks))
	for i, t := range toks {
		switch t.text {
		case openMark:
			words[i] = "("
		case closeMark:
			words[i] = ")"
		default:
			words[i] = t.text
		}
	}
	return strings.Join(words, " ")
}

// isWindowOf says whether call calls XidWindowOf, qualified or not.
func isWindowOf(call *ast.CallExpr) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == windowOf
	case *ast.SelectorExpr:
		return fun.Sel.Name == windowOf
	}
	return false
}

// isBatched says whether n names XidBatchedWindow, qualified or not.
func isBatched(n ast.Node) bool {
	switch x := n.(type) {
	case *ast.Ident:
		return x.Name == batchedWindow
	case *ast.SelectorExpr:
		return x.Sel.Name == batchedWindow
	}
	return false
}

// isWindow says whether expr is a window: a call of XidWindowOf, XidBatchedWindow, or a
// package-level window.
func (p *pkgInfo) isWindow(expr ast.Expr) bool {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			break
		}
		expr = paren.X
	}
	switch x := expr.(type) {
	case *ast.CallExpr:
		return isWindowOf(x)
	case *ast.SelectorExpr:
		return isBatched(x)
	case *ast.Ident:
		return x.Name == batchedWindow || p.named[x.Name]
	}
	return false
}

// countOf is the statement count of the first window expr holds, as the comparison prints it:
// XidWindowOf's first argument, or XidBatchedWindow.
func (p *pkgInfo) countOf(expr ast.Node, depth int) string {
	count := "?"
	ast.Inspect(expr, func(n ast.Node) bool {
		if count != "?" {
			return false
		}
		switch x := n.(type) {
		case *ast.CallExpr:
			if isWindowOf(x) {
				if len(x.Args) > 0 {
					count = p.words(x.Args[0])
				}
				return false
			}
		case *ast.SelectorExpr:
			if isBatched(x) {
				count = batchedWindow
				return false
			}
		case *ast.Ident:
			if x.Name == batchedWindow {
				count = batchedWindow
			} else if p.named[x.Name] && depth < resolveDepth {
				count = p.countOf(p.values[x.Name], depth+1)
			}
		}
		return true
	})
	return count
}

// windowUse is the count and the value of the window declaration n: a package-level window by
// its name and its definition.
func (p *pkgInfo) windowUse(n ast.Expr) use {
	if ident, ok := n.(*ast.Ident); ok && p.named[ident.Name] {
		return use{count: p.countOf(p.values[ident.Name], 1), value: ident.Name + " = " + p.words(p.values[ident.Name])}
	}
	return use{count: p.countOf(n, 0), value: p.words(n)}
}

func isTest(decl *ast.FuncDecl, file *fileInfo) bool {
	if decl.Recv != nil || !strings.HasSuffix(file.rel, "_test.go") {
		return false
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(decl.Name.Name, prefix) {
			return true
		}
	}
	return false
}

// displayName is a function's name as a key names it: a method's with its receiver's type.
func displayName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	return "(" + strings.Join(strings.Fields(string(text(decl.Recv.List[0].Type))), "") + ")." + decl.Name.Name
}

// caseLabel is the name of the case elt is, where it is one: a composite literal whose `name`
// field, or whose first element, is a string literal.
func caseLabel(elt ast.Expr) (string, bool) {
	if unary, ok := elt.(*ast.UnaryExpr); ok && unary.Op == token.AND {
		elt = unary.X
	}
	lit, ok := elt.(*ast.CompositeLit)
	if !ok {
		return "", false
	}
	for index, element := range lit.Elts {
		if pair, ok := element.(*ast.KeyValueExpr); ok {
			if key, ok := pair.Key.(*ast.Ident); ok && key.Name == "name" {
				return stringLit(pair.Value)
			}
			continue
		}
		if index == 0 {
			return stringLit(element)
		}
	}
	return "", false
}

func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func with(path []string, segment string) []string {
	return append(append(make([]string, 0, len(path)+1), path...), segment)
}

// walker visits one top-level declaration. In the mapping pass it records the package-level names
// the declaration names and the case paths that name them; in the recording pass, each window
// declaration.
type walker struct {
	p       *pkgInfo
	top     string
	mapping bool
	owners  string
	records *[]record
}

func (w *walker) walk(n ast.Node, path []string) {
	ast.Inspect(n, func(c ast.Node) bool { return w.visit(c, path) })
}

func (w *walker) emit(path []string, u use, at ast.Node) {
	*w.records = append(*w.records, record{
		path:   w.p.dir + ":" + w.p.name + ":" + strings.Join(path, "/"),
		count:  u.count,
		owners: w.owners,
		value:  u.value,
		where:  where(at),
	})
}

// name records, in the mapping pass, that the declaration names ident, and the case it does so in.
func (w *walker) name(ident *ast.Ident, path []string) {
	if !w.mapping || !w.p.names[ident.Name] {
		return
	}
	if w.p.refs[w.top] == nil {
		w.p.refs[w.top] = map[string]bool{}
	}
	w.p.refs[w.top][ident.Name] = true
	if len(path) > 1 && strings.HasPrefix(path[len(path)-1], "case ") {
		w.p.mapped[ident.Name] = append(w.p.mapped[ident.Name], strings.Join(path, "\x00"))
	}
}

func (w *walker) visit(c ast.Node, path []string) bool {
	switch x := c.(type) {
	case nil:
		return false
	case *ast.CompositeLit:
		for _, element := range x.Elts {
			if label, ok := caseLabel(element); ok {
				w.walk(element, with(path, "case "+strconv.Quote(label)))
			} else {
				w.walk(element, path)
			}
		}
		return false
	case *ast.CallExpr:
		if isWindowOf(x) {
			if !w.mapping {
				w.emit(path, w.p.windowUse(x), x)
			}
			w.walk(x.Fun, path)
			return false
		}
		if selector, ok := x.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Run" && len(x.Args) == 2 {
			label, ok := stringLit(x.Args[0])
			if !ok {
				label = w.p.words(x.Args[0])
			}
			w.walk(x.Fun, path)
			w.walk(x.Args[0], path)
			w.walk(x.Args[1], with(path, "run "+strconv.Quote(label)))
			return false
		}
		if !w.mapping {
			callee := ""
			switch fun := x.Fun.(type) {
			case *ast.Ident:
				callee = fun.Name
			case *ast.SelectorExpr:
				callee = fun.Sel.Name
			}
			if w.p.helpers[callee] {
				var counts, values []string
				for _, u := range w.p.direct[callee] {
					counts = append(counts, u.count)
					values = append(values, u.value)
				}
				w.emit(path, use{count: strings.Join(counts, "+"), value: "call " + callee + " => " + strings.Join(values, " ; ")}, x)
			}
		}
		return true
	case *ast.SelectorExpr:
		if isBatched(x) {
			if !w.mapping {
				w.emit(path, w.p.windowUse(x), x)
			}
			return false
		}
		w.walk(x.X, path)
		w.name(x.Sel, path)
		return false
	case *ast.Ident:
		if !w.mapping && (x.Name == batchedWindow || w.p.named[x.Name]) {
			w.emit(path, w.p.windowUse(x), x)
		}
		w.name(x, path)
		return true
	}
	return true
}

type funcAt struct {
	decl *ast.FuncDecl
	file *fileInfo
}

type specAt struct {
	name  *ast.Ident
	value ast.Expr
	spec  *ast.ValueSpec
}

// declarations is every window declaration of the package.
func (p *pkgInfo) declarations() []record {
	p.values, p.names, p.named = map[string]ast.Expr{}, map[string]bool{}, map[string]bool{}
	p.direct, p.helpers, p.mapped = map[string][]use{}, map[string]bool{}, map[string][]string{}
	p.refs, p.owners = map[string]map[string]bool{}, map[string][]string{}
	var funcs []funcAt
	var specs []specAt
	for _, file := range p.files {
		for _, decl := range file.ast.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				p.names[d.Name.Name] = true
				funcs = append(funcs, funcAt{d, file})
			case *ast.GenDecl:
				if d.Tok != token.VAR && d.Tok != token.CONST {
					continue
				}
				for _, s := range d.Specs {
					spec := s.(*ast.ValueSpec)
					for index, name := range spec.Names {
						p.names[name.Name] = true
						if index < len(spec.Values) {
							p.values[name.Name] = spec.Values[index]
							specs = append(specs, specAt{name, spec.Values[index], spec})
						}
					}
				}
			}
		}
	}
	// The package-level windows, read until no value names one more.
	for changed := true; changed; {
		changed = false
		for _, s := range specs {
			if !p.named[s.name.Name] && s.name.Name != batchedWindow && p.isWindow(s.value) {
				p.named[s.name.Name] = true
				changed = true
			}
		}
	}
	var records []record
	// The mapping pass.
	for _, f := range funcs {
		if f.decl.Body != nil {
			w := &walker{p: p, top: f.decl.Name.Name, mapping: true, records: &records}
			w.walk(f.decl.Body, []string{displayName(f.decl)})
		}
	}
	for _, s := range specs {
		w := &walker{p: p, top: s.name.Name, mapping: true, records: &records}
		w.walk(s.value, []string{"var " + s.name.Name})
	}
	// The tests that reach each top-level declaration through the package's references.
	for _, f := range funcs {
		if !isTest(f.decl, f.file) {
			continue
		}
		test := f.decl.Name.Name
		reached := map[string]bool{test: true}
		frontier := []string{test}
		for len(frontier) > 0 {
			next := frontier[0]
			frontier = frontier[1:]
			for name := range p.refs[next] {
				if !reached[name] {
					reached[name] = true
					frontier = append(frontier, name)
				}
			}
		}
		for name := range reached {
			p.owners[name] = append(p.owners[name], test)
		}
	}
	ownersOf := func(name string) string {
		owners := append([]string(nil), p.owners[name]...)
		if len(owners) == 0 {
			return "-"
		}
		sort.Strings(owners)
		once := owners[:0:0]
		for index, test := range owners {
			if index == 0 || owners[index-1] != test {
				once = append(once, test)
			}
		}
		return strings.Join(once, ",")
	}
	mappedOnce := func(f funcAt) bool {
		return !isTest(f.decl, f.file) && len(p.mapped[f.decl.Name.Name]) == 1
	}
	// Each function's own window declarations, and the helpers among them: a function no single
	// case names, whose callers' declarations are its own.
	for _, f := range funcs {
		if f.decl.Body == nil || isTest(f.decl, f.file) {
			continue
		}
		var own []record
		w := &walker{p: p, top: f.decl.Name.Name, records: &own}
		w.walk(f.decl.Body, []string{"-"})
		for _, r := range own {
			p.direct[f.decl.Name.Name] = append(p.direct[f.decl.Name.Name], use{count: r.count, value: r.value})
		}
		if len(own) > 0 && !mappedOnce(f) {
			p.helpers[f.decl.Name.Name] = true
		}
	}
	// The recording pass: the definitions of the window helpers, and every declaration.
	for _, f := range funcs {
		if f.decl.Body == nil {
			continue
		}
		if f.decl.Recv == nil && f.decl.Name.Name == windowOf {
			w := &walker{p: p, top: windowOf, owners: "-", records: &records}
			w.emit([]string{"definition func " + windowOf}, use{count: "-", value: p.words(f.decl)}, f.decl)
		}
		path := []string{displayName(f.decl)}
		if mappedOnce(f) {
			path = strings.Split(p.mapped[f.decl.Name.Name][0], "\x00")
		}
		w := &walker{p: p, top: f.decl.Name.Name, owners: ownersOf(f.decl.Name.Name), records: &records}
		w.walk(f.decl.Body, path)
	}
	for _, s := range specs {
		w := &walker{p: p, top: s.name.Name, owners: ownersOf(s.name.Name), records: &records}
		if s.name.Name == batchedWindow {
			w.owners = "-"
			w.emit([]string{"definition var " + batchedWindow}, use{count: "-", value: p.words(s.spec)}, s.spec)
			continue
		}
		w.walk(s.value, []string{"var " + s.name.Name})
	}
	return records
}
GO

(cd -- "$work/reader" && env -u GOFLAGS GOWORK=off GOTOOLCHAIN=local GO111MODULE=on \
  go build -o xidwindowbounds .) >"$work/reader.log" 2>&1 ||
  fail setup "build the declaration reader" "go build: $(<"$work/reader.log")"

# --- the two sides -------------------------------------------------------------------------------

# xid_window_bounds_side NAME ARGUMENT: reads one side, a revision or a directory, into
# $work/NAME.tsv (the reader's lines) and $work/NAME.list (key, count and value: the lines the diff
# compares), and prints what it read. It sets side_label to the commit in full, or the directory.
side_label=""
xid_window_bounds_side() {
  local name="$1" argument="$2" root commit="" line path
  local -a files=() dirs=()
  if [[ -d "$argument" ]]; then
    root="$(cd -- "$argument" && pwd)"
    [[ -d "$root/packages/envoy" ]] ||
      fail "$name" "read the tree of $argument" "it holds no packages/envoy"
    side_label="directory $root"
    while IFS= read -r line; do
      files+=("${line#"$root/packages/envoy/"}")
    done < <(grep -rlE --include='*.go' "$window_pattern" "$root/packages/envoy" | LC_ALL=C sort)
  else
    [[ -n "$repo" ]] ||
      fail "$name" "resolve $argument" "$argument is no directory, and $(dirname "${BASH_SOURCE[0]}") stands in no git checkout to read a revision out of"
    commit="$(git -C "$repo" rev-parse --verify --quiet "${argument}^{commit}")" ||
      fail "$name" "resolve $argument" "$argument names no commit of $repo"
    side_label="commit $commit"
    root="$work/$name"
    mkdir "$root"
    while IFS= read -r line; do
      path="${line#"$commit":}"
      files+=("${path#packages/envoy/}")
    done < <(git -C "$repo" grep -l -E "$window_pattern" "$commit" -- 'packages/envoy/*.go' | LC_ALL=C sort)
  fi
  ((${#files[@]} > 0)) ||
    fail "$name" "find a Go file that declares an xid window" \
      "no Go file under packages/envoy at $side_label names XidWindowOf or XidBatchedWindow"
  for path in "${files[@]}"; do
    path="${path%/*}"
    [[ " ${dirs[*]} " == *" $path "* ]] || dirs+=("$path")
  done
  if [[ -n "$commit" ]]; then
    git -C "$repo" archive "$commit" -- "${dirs[@]/#/packages/envoy/}" | tar -x -C "$root" ||
      fail "$name" "take the packages of $commit out of the repository" "git archive or tar exited non-zero"
  fi
  "$work/reader/xidwindowbounds" "$root/packages/envoy" "${dirs[@]}" >"$work/$name.tsv" 2>"$work/$name.err" ||
    fail "$name" "read the xid window declarations at $side_label" "$(<"$work/$name.err")"
  [[ -s "$work/$name.tsv" ]] ||
    fail "$name" "read the xid window declarations at $side_label" "the reader found none in ${files[*]}"
  cut -f1,2,4 "$work/$name.tsv" >"$work/$name.list"
  printf '%s %s: %d declarations in %d files of %d package directories\n' \
    "$name" "$side_label" "$(wc -l <"$work/$name.tsv")" "${#files[@]}" "${#dirs[@]}"
}

xid_window_bounds_side baseline "$1"
readonly baseline_label="$side_label"
xid_window_bounds_side head "$2"
readonly head_label="$side_label"
printf 'baseline: %s\nhead: %s\n' "$baseline_label" "$head_label"
pass read "both sides declare xid windows: $(wc -l <"$work/baseline.tsv") at the baseline, $(wc -l <"$work/head.tsv") at the head"

printf -- '--- the declarations, baseline against head (key, statements, window) ---\n'
diff_status=0
diff -u --label "baseline $baseline_label" --label "head $head_label" "$work/baseline.list" "$work/head.list" ||
  diff_status=$?
((diff_status < 2)) || fail diff "compare the two lists of declarations" "diff exited $diff_status"
printf -- '--- end of the diff ---\n'

declare -A base_count=() base_owners=() base_value=() base_where=()
declare -A head_count=() head_owners=() head_value=() head_where=()
while IFS=$'\t' read -r key count owners value at; do
  base_count["$key"]="$count" base_owners["$key"]="$owners" base_value["$key"]="$value" base_where["$key"]="$at"
done <"$work/baseline.tsv"
while IFS=$'\t' read -r key count owners value at; do
  head_count["$key"]="$count" head_owners["$key"]="$owners" head_value["$key"]="$value" head_where["$key"]="$at"
done <"$work/head.tsv"

problems=()
compared=0
unchanged=0
# xid_window_bounds_problem LINE: prints LINE and keeps it for the verdict.
xid_window_bounds_problem() {
  printf '%s\n' "$1"
  problems+=("$1")
}

# xid_window_bounds_is_restore KEY: whether KEY is a named restore assertion.
xid_window_bounds_is_restore() {
  local restore
  for restore in "${restore_rows[@]}" "$restore_window" "$restore_test"; do
    [[ "$1" != "$restore" ]] || return 0
  done
  return 1
}

# xid_window_bounds_restore KEY: prints the restore diff of KEY, its baseline and its head window.
xid_window_bounds_restore() {
  printf 'RESTORE: %s\n  baseline: statements %s, %s\n  head:     statements %s, %s\n' "$1" \
    "${base_count[$1]:-(none)}" "${base_value[$1]:-(no declaration)}" \
    "${head_count[$1]:-(none)}" "${head_value[$1]:-(no declaration)}"
}

# --- the restore exception -----------------------------------------------------------------------

printf -- '--- the restore exception (root R29) ---\n'
for key in "${restore_rows[@]}"; do
  xid_window_bounds_restore "$key"
  [[ "${head_value[$key]:-}" == "specRestoreWindow = "* && "${head_count[$key]:-}" == "$restore_row_count" ]] ||
    xid_window_bounds_problem "RESTORE REFUSED: $key: the head declares statements ${head_count[$key]:-(none)}, ${head_value[$key]:-(no declaration)}; the row that restores the approved text uses specRestoreWindow at $restore_row_count"
done
xid_window_bounds_restore "$restore_window"
[[ "${head_count[$restore_window]:-}" == "$restore_row_count" && "${head_value[$restore_window]:-}" == *XidWindowOf* ]] ||
  xid_window_bounds_problem "RESTORE REFUSED: $restore_window: the head declares statements ${head_count[$restore_window]:-(none)}, ${head_value[$restore_window]:-(no declaration)}; specRestoreWindow is XidWindowOf($restore_row_count, ...)"
xid_window_bounds_restore "$restore_test"
[[ "${head_count[$restore_test]:-}" == "$restore_test_count" && "${head_value[$restore_test]:-}" == *XidWindowOf* ]] ||
  xid_window_bounds_problem "RESTORE REFUSED: $restore_test: the head declares statements ${head_count[$restore_test]:-(none)}, ${head_value[$restore_test]:-(no declaration)}; the dedicated upload restore test uses XidWindowOf($restore_test_count, ...)"
[[ -z "${head_count[${restore_test%#1}#2]:-}" ]] ||
  xid_window_bounds_problem "RESTORE REFUSED: ${restore_test%#1}#2: the dedicated upload restore test declares a second window, statements ${head_count[${restore_test%#1}#2]}, beside its XidWindowOf($restore_test_count, ...)"
for key in "${!head_count[@]}"; do
  [[ "$key" == *"restoring the approved text"* ]] || continue
  xid_window_bounds_is_restore "$key" ||
    xid_window_bounds_problem "RESTORE REFUSED: $key: a declaration restoring the approved text that the exception does not name, statements ${head_count[$key]}, ${head_value[$key]}"
done

# --- every baseline declaration ------------------------------------------------------------------

printf -- '--- the baseline declarations at the head ---\n'
while IFS= read -r key; do
  compared=$((compared + 1))
  if [[ -z "${head_count[$key]+held}" ]]; then
    xid_window_bounds_problem "MISSING: $key: the baseline declares statements ${base_count[$key]}, ${base_value[$key]} (at ${base_where[$key]}), and the head holds no declaration there"
    continue
  fi
  if [[ "${head_value[$key]}" != "${base_value[$key]}" ]] && ! xid_window_bounds_is_restore "$key"; then
    xid_window_bounds_problem "CHANGED: $key: baseline statements ${base_count[$key]}, ${base_value[$key]} (at ${base_where[$key]}); head statements ${head_count[$key]}, ${head_value[$key]} (at ${head_where[$key]})"
    continue
  fi
  if [[ "${base_owners[$key]}" != - && "${head_owners[$key]}" == - ]]; then
    xid_window_bounds_problem "HIDDEN: $key: the baseline runs it in ${base_owners[$key]}, and no test reaches it at the head (at ${head_where[$key]})"
    continue
  fi
  [[ "${head_value[$key]}" != "${base_value[$key]}" ]] || unchanged=$((unchanged + 1))
done < <(printf '%s\n' "${!base_count[@]}" | LC_ALL=C sort)
printf '%d of %d baseline declarations stand unchanged at the head\n' "$unchanged" "${#base_count[@]}"

# --- the new cases -------------------------------------------------------------------------------

printf -- '--- the declarations the head adds ---\n'
new=0
while IFS= read -r key; do
  [[ -z "${base_count[$key]+held}" ]] || continue
  new=$((new + 1))
  compared=$((compared + 1))
  ! xid_window_bounds_is_restore "$key" || continue
  named=""
  for row in "${accepted_rows[@]}"; do
    [[ "$key" != "$api_package:var documentRouteXidWindows/case \"$row\"#1" ]] ||
      named="the documents table's row \"$row\" (c-render-at-version)"
  done
  [[ "${head_owners[$key]}" != "$accepted_test" ]] || named="$accepted_test, the node routes' test"
  if [[ -z "$named" ]]; then
    xid_window_bounds_problem "NEW refused: $key: statements ${head_count[$key]}, ${head_value[$key]} (at ${head_where[$key]}, run by ${head_owners[$key]}): no case this Verification names"
  elif [[ "${head_count[$key]}" != "$batched" ]]; then
    xid_window_bounds_problem "NEW refused: $key: $named declares statements ${head_count[$key]}, ${head_value[$key]} (at ${head_where[$key]}), not storetest.XidBatchedWindow"
  else
    printf 'NEW accepted: %s: %s, at storetest.XidBatchedWindow (at %s)\n' "$key" "$named" "${head_where[$key]}"
  fi
done < <(printf '%s\n' "${!head_count[@]}" | LC_ALL=C sort)
printf '%d declarations are new at the head\n' "$new"

# --- the verdict ---------------------------------------------------------------------------------

if ((${#problems[@]} > 0)); then
  fail verdict "the head keeps every baseline xid window and adds only the cases the Verification names" \
    "${#problems[@]} problem(s) over $compared comparisons, baseline $baseline_label, head $head_label: $(printf '%s | ' "${problems[@]}")"
fi
pass verdict "the head keeps all ${#base_count[@]} baseline xid windows, the restore exception holds, and its $new new declarations are cases the Verification names"
printf 'PASS: %s complete %d comparisons\n' "$driver" "$compared"
