package ghroutetest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Source is the Go files a source walk reads (SourceCalls), a glob whose test files are passed
// over, and the prefix the functions of those files are named under: none for a test's own
// package, `ghroute.` for internal/ghroute's read from cmd/legion.
type Source struct{ Glob, Prefix string }

// SourceCall is one call in the files a source walk reads, as SourceCalls hands it on: the
// function that holds it (Owner, a method by its receiver's type, under its Source's prefix), the
// file and line it stands at (At), the call, and Text, which is a node's source.
type SourceCall struct {
	Owner, At string
	Call      *ast.CallExpr
	Text      func(ast.Node) string
}

// ShownCallees are how a call of ghroute.Shown reads in the source: by its own name inside
// internal/ghroute, and by its package's name in cmd/legion.
var ShownCallees = []string{"Shown", "ghroute.Shown"}

// SourceCalls is every call in the non-test files sources name, in each function's body. It is
// the one walk cmd/legion's refusal paths (its ghShownPaths) and every package's formatting
// places (FormatSites) read, so a file or a receiver form one of them reaches the other reaches
// too. A source glob matching no non-test file is refused: the walk would have read nothing of it.
func SourceCalls(t *testing.T, sources ...Source) []SourceCall {
	t.Helper()
	var calls []SourceCall
	fset := token.NewFileSet()
	for _, src := range sources {
		files, err := filepath.Glob(src.Glob)
		if err != nil {
			t.Fatal(err)
		}
		read := 0
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			read++
			source, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(fset, name, source, 0)
			if err != nil {
				t.Fatal(err)
			}
			text := func(node ast.Node) string {
				return string(source[fset.Position(node.Pos()).Offset:fset.Position(node.End()).Offset])
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				owner := fn.Name.Name
				if fn.Recv != nil && len(fn.Recv.List) == 1 {
					recv := fn.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					switch generic := recv.(type) {
					case *ast.IndexExpr:
						recv = generic.X
					case *ast.IndexListExpr:
						recv = generic.X
					}
					if ident, ok := recv.(*ast.Ident); ok {
						owner = ident.Name + "." + owner
					} else {
						t.Fatalf("%s: a receiver of a form the walk does not read: %s", fset.Position(fn.Pos()), text(recv))
					}
				}
				owner = src.Prefix + owner
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok {
						calls = append(calls, SourceCall{Owner: owner, At: fset.Position(call.Pos()).String(), Call: call, Text: text})
					}
					return true
				})
			}
		}
		if read == 0 {
			t.Fatalf("UNKNOWN: no non-test file of %s was read", src.Glob)
		}
	}
	return calls
}

// FormatSites is every place among calls that quotes or formats a value without ghroute.Shown,
// keyed `fn: <place>`, fn the function that holds it, a method by its receiver's type:
//   - each strconv.Quote call (`fn: strconv.Quote(<arg>)`);
//   - each errors.New call whose text is no literal (`fn: errors.New(<arg>)`);
//   - each fmt call whose format is no literal (`fn: <the call's function> format <format>`), whose
//     arguments no reading can hold to a verb;
//   - each argument a %s, %v, %q, %x, %X or %w verb of an fmt call's literal format formats (`fn:
//     %q <arg>`) but a literal, a ghroute.Shown call or a position (ordinalArgument);
//   - each cannotPlace call whose name is neither a ghroute.Shown call nor a literal (`fn:
//     cannotPlace(<name>)`);
//   - and each piece of a cannotPlace call's why, read as the "+" of its pieces, that is neither a
//     literal, a ghroute.Shown call nor an fmt.Sprintf call, whose format and arguments the fmt
//     case reads on its own (`fn: cannotPlace why <piece>`): an fmt.Sprint piece, whose arguments
//     no format holds to a verb, is reported whole.
//
// A format or a text built only of literals joined with "+" is a literal. A place that stands
// more than once in one function is keyed with its count (`(5 places)`). The value of each key is
// where the place stands.
func FormatSites(calls []SourceCall) map[string]string {
	sites, found := map[string]string{}, map[string][]string{}
	for _, c := range calls {
		call, owner, text := c.Call, c.Owner, c.Text
		add := func(key string) { found[owner+": "+key] = append(found[owner+": "+key], c.At) }
		calls := func(expr ast.Expr, names ...string) bool {
			inner, ok := expr.(*ast.CallExpr)
			return ok && slices.Contains(names, text(inner.Fun))
		}
		switch callee := text(call.Fun); {
		case callee == "strconv.Quote" && len(call.Args) == 1:
			add("strconv.Quote(" + text(call.Args[0]) + ")")
		case callee == "errors.New" && len(call.Args) == 1:
			if _, literal := literalString(call.Args[0]); !literal {
				add("errors.New(" + text(call.Args[0]) + ")")
			}
		case callee == "cannotPlace" && len(call.Args) > 1:
			if _, literal := literalString(call.Args[0]); !literal && !calls(call.Args[0], ShownCallees...) {
				add("cannotPlace(" + text(call.Args[0]) + ")")
			}
			for _, piece := range sumPieces(call.Args[1]) {
				if _, literal := literalString(piece); literal || calls(piece, append(slices.Clone(ShownCallees), "fmt.Sprintf")...) {
					continue
				}
				add("cannotPlace why " + text(piece))
			}
		case strings.HasPrefix(callee, "fmt.") && len(call.Args) > 0:
			formatAt := 0
			if callee == "fmt.Fprintf" {
				formatAt = 1
			}
			if !strings.HasSuffix(callee, "f") || formatAt >= len(call.Args) {
				break
			}
			format, literal := literalString(call.Args[formatAt])
			if !literal {
				add(callee + " format " + text(call.Args[formatAt]))
				break
			}
			for i, verb := range formatVerbs(format) {
				arg := formatAt + 1 + i
				if arg >= len(call.Args) || !strings.ContainsRune("sqvxXw", verb) {
					continue
				}
				if _, literal := literalString(call.Args[arg]); literal || calls(call.Args[arg], append(slices.Clone(ShownCallees), "ordinalArgument")...) {
					continue
				}
				add("%" + string(verb) + " " + text(call.Args[arg]))
			}
		}
	}
	for key, at := range found {
		if len(at) > 1 {
			key += " (" + strconv.Itoa(len(at)) + " places)"
		}
		sites[key] = strings.Join(at, ", ")
	}
	return sites
}

// RequireReviewed fails t unless every place sites holds (FormatSites) is one reviewed, the table
// named table, holds with the reason its value is none a person typed, and every place reviewed
// holds is still in sites. A sites holding no place at all is refused: the walk read nothing.
func RequireReviewed(t *testing.T, sites, reviewed map[string]string, table string) {
	t.Helper()
	if len(sites) == 0 {
		t.Fatal("UNKNOWN: the source holds no strconv.Quote call, no formatted value and no cannotPlace name, so this read nothing")
	}
	for _, key := range slices.Sorted(maps.Keys(sites)) {
		if _, ok := reviewed[key]; !ok {
			t.Errorf("%s formats a value itself, at %s, which %s does not hold: name a person's value by its place through ghroute.Shown, or name why this one is none", key, sites[key], table)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(reviewed)) {
		if _, ok := sites[key]; !ok {
			t.Errorf("%s holds %s, which the source no longer has", table, key)
		}
	}
	t.Logf("%d places format a value without ghroute.Shown, each reviewed", len(sites))
}

// literalString is expr's value when expr is a string literal or literals joined with "+", and
// whether it is.
func literalString(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(e.Value)
		return value, err == nil
	case *ast.ParenExpr:
		return literalString(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := literalString(e.X)
		if !ok {
			return "", false
		}
		right, ok := literalString(e.Y)
		return left + right, ok
	}
	return "", false
}

// sumPieces is expr read as the "+" of its pieces, each piece that is no "+" itself.
func sumPieces(expr ast.Expr) []ast.Expr {
	switch e := expr.(type) {
	case *ast.ParenExpr:
		return sumPieces(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return append(sumPieces(e.X), sumPieces(e.Y)...)
		}
	}
	return []ast.Expr{expr}
}

// formatVerbs is the verb of each argument format formats, in order; %% takes none.
func formatVerbs(format string) []rune {
	var verbs []rune
	runes := []rune(format)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '%' {
			continue
		}
		i++
		for i < len(runes) && strings.ContainsRune("+-# 0123456789.[]*", runes[i]) {
			i++
		}
		if i < len(runes) && runes[i] != '%' {
			verbs = append(verbs, runes[i])
		}
	}
	return verbs
}
