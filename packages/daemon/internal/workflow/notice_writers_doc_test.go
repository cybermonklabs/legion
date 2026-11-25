package workflow

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/texts"
)

// noticeRecordSources are the files of the one list's records (internal/texts), where the doc of
// each notice kind's record says what a notice of that kind means; each architect's text that
// names a notice is written from that doc.
const noticeRecordSources = "../texts/records*.go"

// noticeKindsSource is the file of the one list's kinds, whose constants name the kind each
// texts.NoticeHead literal of this package sets.
const noticeKindsSource = "../texts/kinds.go"

// A notice kind's record doc (the doc of the record type of its entry on the one list, texts'
// Entries) is the one place that says every meaning that kind of notice carries, and each function
// of the workflow that writes a notice of a kind is a meaning it carries. So each such writer is
// named in its kind's record doc: a writer added for a new meaning fails here until the doc says
// it. The writers are read from this package's own source, every texts.NoticeHead literal that
// names its kind by the list's constant, so the list is the code's and not a copy of it. The
// control is a writer taken out of its record doc, which this test then names.
func TestEveryNoticeWriterIsNamedInItsKindsParagraph(t *testing.T) {
	writers := noticeWriters(t)
	paragraphs := noticeParagraphs(t)
	if len(writers["plan-not-taken"]) == 0 || paragraphs["plan-not-taken"] == "" {
		t.Fatalf("read no writer (%v) or no record doc (%q) of plan-not-taken: the reading itself is broken, and a test that read nothing passes nothing",
			writers["plan-not-taken"], paragraphs["plan-not-taken"])
	}
	compared := 0
	for kind, paragraph := range paragraphs {
		for _, writer := range writers[kind] {
			compared++
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(writer) + `\b`).MatchString(paragraph) {
				t.Errorf("%s writes a %s notice, and the doc of the record of %s in %s names it nowhere:\n%s",
					writer, kind, kind, noticeRecordSources, paragraph)
			}
		}
	}
	t.Logf("writers compared with their kind's record doc: %d", compared)
}

// noticeWriters is, for each notice kind, the functions of this package's own source (its test
// files left out) holding a texts.NoticeHead literal whose Kind is a constant of the one list.
func noticeWriters(t *testing.T) map[string][]string {
	t.Helper()
	kinds := noticeKindConstants(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string][]string{}
	fset := gotoken.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if kind := kinds[noticeHeadKind(node)]; kind != "" && !slices.Contains(writers[kind], fn.Name.Name) {
					writers[kind] = append(writers[kind], fn.Name.Name)
				}
				return true
			})
		}
	}
	return writers
}

// noticeHeadKind is the name of the texts constant node sets as its Kind where it is a
// texts.NoticeHead literal, and "" otherwise.
func noticeHeadKind(node ast.Node) string {
	literal, ok := node.(*ast.CompositeLit)
	if !ok || !isTextsSelector(literal.Type, "NoticeHead") {
		return ""
	}
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := field.Key.(*ast.Ident); !ok || key.Name != "Kind" {
			continue
		}
		if value, ok := field.Value.(*ast.SelectorExpr); ok && isTextsSelector(value, value.Sel.Name) {
			return value.Sel.Name
		}
	}
	return ""
}

// isTextsSelector says whether expr is texts.<name>.
func isTextsSelector(expr ast.Expr, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "texts"
}

// noticeKindConstants is each constant of the one list's kinds by its name, with the kind it
// names, read from noticeKindsSource.
func noticeKindConstants(t *testing.T) map[string]string {
	t.Helper()
	parsed, err := parser.ParseFile(gotoken.NewFileSet(), noticeKindsSource, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", noticeKindsSource, err)
	}
	kinds := map[string]string{}
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != gotoken.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			if literal, ok := value.Values[0].(*ast.BasicLit); ok && literal.Kind == gotoken.STRING {
				if kind, err := strconv.Unquote(literal.Value); err == nil {
					kinds[value.Names[0].Name] = kind
				}
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatalf("%s holds no kind constant", noticeKindsSource)
	}
	return kinds
}

// noticeParagraphs is the doc of the record type of each notice kind to an architect on the one
// list (texts.Notice, every kind of the notice that record's validNoticeKinds named before the
// list took its place), keyed by that kind, read from noticeRecordSources.
func noticeParagraphs(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(noticeRecordSources)
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{}
	fset := gotoken.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != gotoken.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				typ := spec.(*ast.TypeSpec)
				doc := typ.Doc
				if doc == nil {
					doc = gen.Doc
				}
				if doc != nil {
					docs[typ.Name.Name] = strings.Join(strings.Fields(doc.Text()), " ")
				}
			}
		}
	}
	paragraphs := map[string]string{}
	for _, entry := range texts.Entries() {
		if entry.Form != texts.Notice {
			continue
		}
		name := reflect.TypeOf(entry.Record).Name()
		if docs[name] == "" {
			t.Fatalf("%s holds no doc of %s, the record of %s", noticeRecordSources, name, entry.Kind)
		}
		paragraphs[string(entry.Kind)] = docs[name]
	}
	return paragraphs
}
