package workflow

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// noticeKindsSource is the file whose validNoticeKinds doc says, kind by kind, what a notice
// means; each architect's text that names a notice is written from that paragraph.
const noticeKindsSource = "../record/outbox.go"

// noticeParagraphOpening is the opening of a kind's paragraph in that doc: the kind, quoted.
var noticeParagraphOpening = regexp.MustCompile("^`([a-z-]+)` ")

// A notice kind's paragraph in the doc of record's validNoticeKinds is the one place that says
// every meaning that kind of notice carries, and each function of the workflow that writes a
// notice of a kind with such a paragraph is a meaning it carries. So each such writer is named in
// its kind's paragraph: a writer added for a new meaning fails here until the paragraph says it.
// The writers are read from this package's own source, every record.Notice literal that names its
// kind, so the list is the code's and not a copy of it. The control is a writer taken out of its
// paragraph, which this test then names.
func TestEveryNoticeWriterIsNamedInItsKindsParagraph(t *testing.T) {
	t.Parallel()
	writers := noticeWriters(t)
	paragraphs := noticeParagraphs(t)
	if len(writers["plan-not-taken"]) == 0 || paragraphs["plan-not-taken"] == "" {
		t.Fatalf("read no writer (%v) or no paragraph (%q) of plan-not-taken: the reading itself is broken, and a test that read nothing passes nothing",
			writers["plan-not-taken"], paragraphs["plan-not-taken"])
	}
	compared := 0
	for kind, paragraph := range paragraphs {
		for _, writer := range writers[kind] {
			compared++
			if !regexp.MustCompile(`\b` + regexp.QuoteMeta(writer) + `\b`).MatchString(paragraph) {
				t.Errorf("%s writes a %s notice, and the paragraph of %s in %s names it nowhere:\n%s",
					writer, kind, kind, noticeKindsSource, paragraph)
			}
		}
	}
	t.Logf("writers compared with their kind's paragraph: %d", compared)
}

// noticeWriters is, for each notice kind, the functions of this package's own source (its test
// files left out) holding a record.Notice literal that names that kind.
func noticeWriters(t *testing.T) map[string][]string {
	t.Helper()
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
				if kind := noticeLiteralKind(node); kind != "" && !slices.Contains(writers[kind], fn.Name.Name) {
					writers[kind] = append(writers[kind], fn.Name.Name)
				}
				return true
			})
		}
	}
	return writers
}

// noticeLiteralKind is the kind node names where it is a record.Notice literal whose Kind is a
// string, and "" otherwise.
func noticeLiteralKind(node ast.Node) string {
	literal, ok := node.(*ast.CompositeLit)
	if !ok {
		return ""
	}
	selector, ok := literal.Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Notice" {
		return ""
	}
	if pkg, ok := selector.X.(*ast.Ident); !ok || pkg.Name != "record" {
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
		if value, ok := field.Value.(*ast.BasicLit); ok && value.Kind == gotoken.STRING {
			kind, err := strconv.Unquote(value.Value)
			if err == nil {
				return kind
			}
		}
	}
	return ""
}

// noticeParagraphs is each paragraph of validNoticeKinds' doc that opens with a kind, keyed by
// that kind.
func noticeParagraphs(t *testing.T) map[string]string {
	t.Helper()
	source, err := os.ReadFile(noticeKindsSource)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parser.ParseFile(gotoken.NewFileSet(), noticeKindsSource, source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", noticeKindsSource, err)
	}
	var text string
	for _, decl := range parsed.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Doc == nil {
			continue
		}
		for _, spec := range gen.Specs {
			if value, ok := spec.(*ast.ValueSpec); ok && slices.ContainsFunc(value.Names, func(name *ast.Ident) bool { return name.Name == "validNoticeKinds" }) {
				text = gen.Doc.Text()
			}
		}
	}
	if text == "" {
		t.Fatalf("%s holds no doc comment on validNoticeKinds", noticeKindsSource)
	}
	paragraphs := map[string]string{}
	for _, paragraph := range strings.Split(text, "\n\n") {
		paragraph = strings.Join(strings.Fields(paragraph), " ")
		if match := noticeParagraphOpening.FindStringSubmatch(paragraph); match != nil {
			paragraphs[match[1]] = paragraph
		}
	}
	return paragraphs
}
