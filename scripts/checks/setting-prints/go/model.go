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
	kind    string // "source", "written", "function" or "environment function"
	keyed   bool   // a source: the environment as a whole, which a constant key names more closely
	what    string // a source: what it reads, and where
	how     string // a written line: what writes the value into text
	file    string
	line    int
	expr    string
	sources map[int]bool
	leading map[int]bool // a written line: the settings written at the head of its text
	ends    map[string]bool
	fn      any // a function: its *types.Func or *ast.FuncLit
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

type analysis struct {
	root    string
	fset    *token.FileSet
	info    *types.Info
	text    map[string][]byte
	bodies  map[*types.Func]*ast.FuncDecl
	labels  []*label
	ids     map[string]int
	carried map[any]set
	changed bool
	memo    map[ast.Expr]set
	collect bool
	report  map[string]bool
	read    map[string]bool // the import paths of the packages read from source
	module  string          // the module's path
	urls    map[int]bool    // the sources whose value reaches a URL taker
	sites   map[*types.Var]map[token.Pos]*site
	lits    []span // every function literal
	blocks  []span // every block and case body
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

func (a *analysis) carries(s set) bool {
	for id := range s {
		if a.labels[id].kind == "source" {
			return true
		}
	}
	return false
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

// write is the value at n written into text by how. When the value holds a setting as read, the
// line is labelled, and the text it makes carries that label in place of the setting: a line that
// writes that text again is not a second line that writes the setting.
func (a *analysis) write(n ast.Node, how string, value set) set {
	if !a.carries(value) {
		return value
	}
	file, line := a.position(n)
	id := a.intern(fmt.Sprintf("w %s %d %s", file, a.fset.Position(n.Pos()).Offset, how), func() *label {
		return &label{kind: "written", how: how, file: file, line: line, expr: a.exprText(n), sources: map[int]bool{}, leading: map[int]bool{}, ends: map[string]bool{}}
	})
	out := set{id: true}
	for s := range value {
		if a.labels[s].kind == "source" {
			a.labels[id].sources[s] = true
		} else {
			out[s] = true
		}
	}
	return out
}

// writeHead is write for a value written at the head of the text, where a URL's scheme and host
// stand: the left operand of a string +.
func (a *analysis) writeHead(n ast.Node, how string, value set) set {
	out := a.write(n, how, value)
	for id := range out {
		if l := a.labels[id]; l.kind == "written" && !value[id] {
			for s := range value {
				if a.labels[s].kind == "source" {
					l.leading[s] = true
				}
			}
		}
	}
	return out
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

func (a *analysis) grow(key any, value set) {
	if len(value) == 0 || key == nil {
		return
	}
	held := a.carried[key]
	if held == nil {
		held = set{}
		a.carried[key] = held
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
