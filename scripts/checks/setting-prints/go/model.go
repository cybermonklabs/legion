//go:build ignore

package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"
)

type label struct {
	kind    string // "source", "written", "function", "environment function", "param", "chain" or "type"
	keyed   bool   // a source: the environment as a whole, which a constant key names more closely
	what    string // a source: what it reads, and where
	how     string // a written line: what writes the value into text
	file    string
	line    int
	expr    string
	sources map[int]bool
	leading map[int]bool    // a written line: the settings written at the head of its text
	ends    map[string]bool // a written line or a chain: where its text is printed
	fn      any             // a function: its *types.Func or *ast.FuncLit; a parameter or a chain: the function it is of
	index   int             // a parameter or a chain: its place, -1 for a method's receiver
	param   *types.Var
	written bool         // a parameter: as the lines of its function write it into text
	edges   map[int]bool // a chain: the lines of its function that write its parameter, and the chains of the calls that hand it on
	typ     *types.Named // a type: the type of the module a value was made as, which a call of an interface method dispatches on
}

type set map[int]bool

func union(sets ...set) set {
	var first set
	var out set
	for _, s := range sets {
		if len(s) == 0 {
			continue
		}
		if first == nil {
			first = s
			continue
		}
		if out == nil {
			out = make(set, len(first)+len(s))
			for id := range first {
				out[id] = true
			}
		}
		for id := range s {
			out[id] = true
		}
	}
	if out != nil {
		return out
	}
	return first
}

// analysis is one run of the search. It reads the module twice over, grown together to a fixed
// point, as the TypeScript half does: the symbolic reading reads each function with its parameters
// as themselves, so a function's answer is a summary (returns) that each call fills in with its own
// arguments; the concrete reading hands each parameter what every call hands it, and is what the
// lines that print read. A field and a variable of a package hold what the concrete reading wrote.
type analysis struct {
	root       string
	fset       *token.FileSet
	info       *types.Info
	text       map[string][]byte
	bodies     map[*types.Func]*ast.FuncDecl
	labels     []*label
	ids        map[string]int
	carried    map[any]set // the concrete reading's
	symCarried map[any]set // the symbolic reading's, of the variables of a function
	returns    map[any]set // each function's answer, as the symbolic reading reads it
	symbolic   bool
	changed    bool
	memo       map[ast.Expr]set
	collect    bool
	report     map[string]bool
	read       map[string]bool // the import paths of the packages read from source
	urls       map[int]bool    // the sources whose value reaches a URL taker
	sites      map[*types.Var]map[token.Pos]*site
	symSites   map[*types.Var]map[token.Pos]*site
	lits       []span                 // every function literal of the unit
	blocks     []span                 // every block and case body of the unit
	impls      map[[2]any]*types.Func // the method a call of each interface method runs for each type its receiver holds
}

// site is one assignment of a variable of a function: the block it stands in, and what it put.
type site struct {
	block  span
	labels set
}

func (a *analysis) intern(key string, make func() *label) int {
	if id, ok := a.ids[key]; ok {
		return id
	}
	a.labels = append(a.labels, make())
	a.ids[key] = len(a.labels) - 1
	return len(a.labels) - 1
}

func (a *analysis) position(n ast.Node) (string, int) {
	p := a.fset.Position(n.Pos())
	rel, err := filepath.Rel(a.root, p.Filename)
	if err != nil {
		rel = p.Filename
	}
	return rel, p.Line
}

func (a *analysis) source(n ast.Node, what string) set {
	file, line := a.position(n)
	where := fmt.Sprintf("%s:%d", file, line)
	return set{a.intern("s "+where+" "+what, func() *label { return &label{kind: "source", what: what + " (" + where + ")"} }): true}
}

func (a *analysis) fnLabel(kind string, fn any) set {
	return set{a.intern(fmt.Sprintf("f %p %s", fn, kind), func() *label { return &label{kind: kind, fn: fn} }): true}
}

// typeLabel is the label a value made as t carries (a composite literal of t, &t{}, new(t)), when t
// is a type a package of the module declares: a call of an interface method on that value runs t's
// method (implementations).
func (a *analysis) typeLabel(t types.Type) set {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || !a.read[packagePath(named.Obj())] || types.IsInterface(named) {
		return nil
	}
	named = named.Origin()
	return set{a.intern(fmt.Sprintf("t %p", named), func() *label { return &label{kind: "type", typ: named} }): true}
}

// paramLabel is the parameter v of fn (index -1 for a method's receiver) as the symbolic reading
// reads it, or, written, as the lines of fn write it into the answer: a call fills either in with
// what it hands that parameter.
func (a *analysis) paramLabel(fn any, index int, v *types.Var, written bool) int {
	return a.intern(fmt.Sprintf("p %p %d %t", fn, index, written), func() *label {
		return &label{kind: "param", fn: fn, index: index, param: v, written: written}
	})
}

// chainLabel is the text the lines of fn write its parameter index into, as a call's answer
// holds it once that call handed the parameter a setting: each line of fn that writes the
// parameter, and each call of fn that hands it on to a function whose lines write it, is an edge,
// and a print of the chain is a print of each line it reaches (chainEnds).
func (a *analysis) chainLabel(fn any, index int) int {
	return a.intern(fmt.Sprintf("c %p %d", fn, index), func() *label {
		return &label{kind: "chain", fn: fn, index: index, edges: map[int]bool{}, ends: map[string]bool{}}
	})
}

// edge adds to the chain from the edge to the line or chain to.
func (a *analysis) edge(from, to int) {
	if l := a.labels[from]; !l.edges[to] {
		l.edges[to] = true
		a.changed = true
	}
}

// unwritten reports whether id is a setting a value holds as read, not yet written into text by a
// line, or in the symbolic reading such a parameter.
func (a *analysis) unwritten(id int) bool {
	l := a.labels[id]
	return l.kind == "source" || (a.symbolic && l.kind == "param" && !l.written)
}

func (a *analysis) exprText(n ast.Node) string {
	start, end := a.fset.Position(n.Pos()), a.fset.Position(n.End())
	src := a.text[start.Filename]
	if src == nil || end.Offset > len(src) {
		return ""
	}
	text := strings.Join(strings.Fields(string(src[start.Offset:end.Offset])), " ")
	if len(text) > 160 {
		text = text[:160]
	}
	return text
}

// settingsIn are the settings a value carries: each it holds as read, and each that a line wrote
// into the text it holds.
func (a *analysis) settingsIn(s set) []int {
	var out []int
	seen := map[int]bool{}
	for id := range s {
		l := a.labels[id]
		switch l.kind {
		case "source":
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		case "written":
			for each := range l.sources {
				if !seen[each] {
					seen[each] = true
					out = append(out, each)
				}
			}
		}
	}
	return out
}

// write is the value at n written into text by how (at the head of the text when atHead, where a
// URL's scheme and host stand: the left operand of a string +). When the value holds a setting as
// read, the line is labelled, and the text it makes carries that label in place of the setting: a
// line that writes that text again is not a second line that writes the setting. A parameter of
// the symbolic reading becomes that parameter written, and the line an edge of its chain: a call
// that hands the parameter a setting answers with the chain (writtenAt), so a print of that answer
// is a print of this line. The line names every setting any call hands the parameter, as the
// concrete reading reads it.
func (a *analysis) write(n ast.Node, how string, value set, atHead bool) set {
	var read []int
	for id := range value {
		if a.unwritten(id) {
			read = append(read, id)
		}
	}
	if len(read) == 0 {
		return value
	}
	file, line := a.position(n)
	id := a.intern(fmt.Sprintf("w %s %d %s", file, a.fset.Position(n.Pos()).Offset, how), func() *label {
		return &label{kind: "written", how: how, file: file, line: line, expr: a.exprText(n), sources: map[int]bool{}, leading: map[int]bool{}, ends: map[string]bool{}}
	})
	labelled := a.labels[id]
	out := make(set, len(value)+1)
	for each := range value {
		out[each] = true
	}
	for _, each := range read {
		delete(out, each)
		switch l := a.labels[each]; l.kind {
		case "source":
			labelled.sources[each] = true
			if atHead {
				labelled.leading[each] = true
			}
		case "param":
			out[a.paramLabel(l.fn, l.index, l.param, true)] = true
			a.edge(a.chainLabel(l.fn, l.index), id)
		}
	}
	out[id] = true
	return out
}

// writtenAt is what value, an argument a call hands the written parameter of fn at index, holds
// once the lines of fn wrote it: where it holds a setting as read, the chain of that parameter in
// place of the setting; where, in the symbolic reading, it holds a parameter of the caller, that
// parameter written, its chain handing on to this one; and the value itself otherwise.
func (a *analysis) writtenAt(fn any, index int, value set) set {
	var read []int
	for id := range value {
		if a.unwritten(id) {
			read = append(read, id)
		}
	}
	if len(read) == 0 {
		return value
	}
	chain := a.chainLabel(fn, index)
	out := make(set, len(value)+1)
	for each := range value {
		out[each] = true
	}
	for _, each := range read {
		delete(out, each)
		switch l := a.labels[each]; l.kind {
		case "source":
			out[chain] = true
		case "param":
			out[a.paramLabel(l.fn, l.index, l.param, true)] = true
			a.edge(a.chainLabel(l.fn, l.index), chain)
		}
	}
	return out
}

// chainEnds gives each line every place its chains are printed: a print of a chain is a print of
// each line it reaches through its edges.
func (a *analysis) chainEnds() {
	for _, l := range a.labels {
		if l.kind != "chain" || len(l.ends) == 0 {
			continue
		}
		seen := map[int]bool{}
		stack := make([]int, 0, len(l.edges))
		for to := range l.edges {
			stack = append(stack, to)
		}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			target := a.labels[id]
			if target.kind == "written" {
				for e := range l.ends {
					target.ends[e] = true
				}
				continue
			}
			for to := range target.edges {
				stack = append(stack, to)
			}
		}
	}
}

// leadingIn are the settings a value holds as read, and those a line wrote at the head of its text.
func (a *analysis) leadingIn(s set) []int {
	var out []int
	for id := range s {
		switch l := a.labels[id]; l.kind {
		case "source":
			out = append(out, id)
		case "written":
			for each := range l.leading {
				out = append(out, each)
			}
		}
	}
	return out
}

func (a *analysis) grow(key any, value set) { a.growIn(a.carried, key, value) }

func (a *analysis) growIn(m map[any]set, key any, value set) {
	if len(value) == 0 || key == nil {
		return
	}
	held := m[key]
	if held == nil {
		held = set{}
		m[key] = held
	}
	for id := range value {
		if !held[id] {
			held[id] = true
			a.changed = true
		}
	}
}

type span [2]token.Pos

func (s span) holds(at token.Pos) bool { return s[0] <= at && at < s[1] }

// innermost is the narrowest of spans that holds at, or the zero span.
func innermost(spans []span, at token.Pos) span {
	var best span
	for _, s := range spans {
		if s.holds(at) && (best == span{} || s[1]-s[0] < best[1]-best[0]) {
			best = s
		}
	}
	return best
}

func (a *analysis) relFile(n ast.Node) string {
	file, _ := a.position(n)
	return file
}

func appendNew(list []string, item string) []string {
	for _, each := range list {
		if each == item {
			return list
		}
	}
	return append(list, item)
}
