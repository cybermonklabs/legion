package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// headFile is the text of file at repo's HEAD, the head a leak's files and declarations are
// copied from.
func headFile(repo, file string) ([]byte, error) {
	data, err := exec.Command("git", "-C", repo, "show", "HEAD:"+file).Output()
	if err != nil {
		return nil, fmt.Errorf("the head's %s: %v", file, err)
	}
	return data, nil
}

// headDeclarations puts, in the Go file d.File of the tree at wt, the head's text of each
// declaration d names in place of the base's (spliceDeclarations). The head is repo's HEAD, as
// for a leak's whole files.
func headDeclarations(repo, wt string, d declaration) error {
	head, err := headFile(repo, d.File)
	if err != nil {
		return err
	}
	path := filepath.Join(wt, d.File)
	base, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the base's %s, whose declarations the head's take the place of: %v", d.File, err)
	}
	pkg, ok := goImportPath(wt, d.File)
	if !ok {
		return fmt.Errorf("the base's %s is in no Go module", d.File)
	}
	out, err := spliceDeclarations(head, base, d.File, pkg, d.Names)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// spliceDeclarations is base, the base's text of the Go file file of the package at import path
// pkg, with the head's text (head) of each declaration names names in place of the base's, and
// each one the base does not declare added after its last declaration: a function or method
// whole, with its doc comment; a type, constant or variable as its spec, inside the base's group
// where the base declares it in one. It refuses a name the head's file does not declare alone in
// its spec, a constant whose value its group's spec before it gives (the head's text would lose
// it), and a declaration whose head text is the base's, which puts nothing at the base. The names
// are read from syntax alone; a declaration that needs an import the base's file lacks does not
// compile there, and the row's run names the compiler's error.
func spliceDeclarations(head, base []byte, file, pkg string, names []string) ([]byte, error) {
	headDecls, err := topDeclarations(head, file, pkg)
	if err != nil {
		return nil, fmt.Errorf("the head's %s: %v", file, err)
	}
	baseDecls, err := topDeclarations(base, file, pkg)
	if err != nil {
		return nil, fmt.Errorf("the base's %s: %v", file, err)
	}
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	var added []string
	for _, name := range names {
		h, ok := headDecls[name]
		switch {
		case !ok:
			return nil, fmt.Errorf("the head's %s declares no %s", file, name)
		case h.why != "":
			return nil, fmt.Errorf("the head's %s declares %s %s", file, name, h.why)
		}
		b, ok := baseDecls[name]
		if !ok {
			added = append(added, h.alone())
			continue
		}
		if b.why != "" {
			return nil, fmt.Errorf("the base's %s declares %s %s", file, name, b.why)
		}
		text := h.alone()
		if b.grouped {
			text = h.inGroup()
		}
		if string(base[b.start:b.end]) == text {
			return nil, fmt.Errorf("the head's %s in %s is the base's, so naming it puts nothing at the base", name, file)
		}
		edits = append(edits, edit{b.start, b.end, text})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	for i := 1; i < len(edits); i++ {
		if edits[i].end > edits[i-1].start {
			return nil, fmt.Errorf("two of the names in %s are one declaration of the base's", file)
		}
	}
	out := slices.Clone(base)
	for _, e := range edits {
		out = slices.Replace(out, e.start, e.end, []byte(e.text)...)
	}
	for _, text := range added {
		out = append(append(append(out, '\n'), text...), '\n')
	}
	return out, nil
}

// topDeclaration is one name a Go file declares at its top level: the byte range of its text,
// doc comment included (a spec's alone where it is in a parenthesized group, grouped), and its
// doc comment and body, from which inGroup and alone write it. tok is const, var or type for a
// spec and ILLEGAL for a function. why says why a declaration entry cannot take it, or is "".
type topDeclaration struct {
	start, end int
	grouped    bool
	tok        token.Token
	doc, body  string
	why        string
}

// inGroup is the declaration's text as a spec of a parenthesized group.
func (d topDeclaration) inGroup() string {
	if d.doc == "" {
		return d.body
	}
	return d.doc + "\n\t" + d.body
}

// alone is the declaration's text as a declaration of its own.
func (d topDeclaration) alone() string {
	body := d.body
	if d.tok != token.ILLEGAL {
		body = d.tok.String() + " " + body
	}
	if d.doc == "" {
		return body
	}
	return d.doc + "\n" + body
}

// topDeclarations is every top-level name src, the Go file file of the package at import path
// pkg, declares, by the form a declaration entry names it in.
func topDeclarations(src []byte, file, pkg string) (map[string]topDeclaration, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	offset := func(p token.Pos) int { return fset.Position(p).Offset }
	text := func(from, to token.Pos) string { return string(src[offset(from):offset(to)]) }
	docOf := func(doc *ast.CommentGroup) string {
		if doc == nil {
			return ""
		}
		return text(doc.Pos(), doc.End())
	}
	startOf := func(doc *ast.CommentGroup, n ast.Node) int {
		if doc != nil {
			return offset(doc.Pos())
		}
		return offset(n.Pos())
	}
	decls := map[string]topDeclaration{}
	for _, decl := range f.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			decls[goFuncName(pkg, decl)] = topDeclaration{
				start: startOf(decl.Doc, decl), end: offset(decl.End()), tok: token.ILLEGAL,
				doc: docOf(decl.Doc), body: text(decl.Pos(), decl.End()),
			}
		case *ast.GenDecl:
			if decl.Tok == token.IMPORT {
				continue
			}
			grouped := decl.Lparen.IsValid()
			for _, spec := range decl.Specs {
				var names []string
				why := ""
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names = []string{spec.Name.Name}
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						names = append(names, n.Name)
					}
					if len(names) > 1 {
						why = "in one spec with " + strings.Join(names, ", ") + ", which a declaration entry cannot take apart"
					} else if decl.Tok == token.CONST && spec.Type == nil && len(spec.Values) == 0 {
						why = "as a constant whose value the spec before it in its group gives"
					}
				}
				doc := decl.Doc
				start, end := startOf(decl.Doc, decl), offset(decl.End())
				if grouped {
					doc = specDoc(spec)
					start, end = startOf(doc, spec), offset(spec.End())
				}
				for _, name := range names {
					decls[pkg+"."+name] = topDeclaration{
						start: start, end: end, grouped: grouped, tok: decl.Tok,
						doc: docOf(doc), body: text(spec.Pos(), spec.End()), why: why,
					}
				}
			}
		}
	}
	return decls, nil
}

// specDoc is the doc comment of a spec inside a parenthesized group, or nil.
func specDoc(spec ast.Spec) *ast.CommentGroup {
	switch spec := spec.(type) {
	case *ast.TypeSpec:
		return spec.Doc
	case *ast.ValueSpec:
		return spec.Doc
	}
	return nil
}
