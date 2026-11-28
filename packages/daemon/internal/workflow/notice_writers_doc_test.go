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

	"github.com/sjawhar/legion/daemon/internal/record"
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
// kind, as a string or as one of record's string constants, so the list is the code's and not a
// copy of it. The control is a writer taken out of its paragraph, which this test then names.
// plan-not-taken's writers name their kind as a string and claim-write-refused's as record's
// constant (record.ClaimWriteRefusedNotice), and each must be read, or the reading itself is
// broken.
func TestEveryNoticeWriterIsNamedInItsKindsParagraph(t *testing.T) {
	t.Parallel()
	writers := noticeWriters(t)
	paragraphs := noticeParagraphs(t)
	for _, kind := range []string{"plan-not-taken", string(record.ClaimWriteRefusedNotice)} {
		if len(writers[kind]) == 0 || paragraphs[kind] == "" {
			t.Fatalf("read no writer (%v) or no paragraph (%q) of %s: the reading itself is broken, and a test that read nothing passes nothing",
				writers[kind], paragraphs[kind], kind)
		}
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
	constants := recordStringConstants(t)
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
				if kind := noticeLiteralKind(node, constants); kind != "" && !slices.Contains(writers[kind], fn.Name.Name) {
					writers[kind] = append(writers[kind], fn.Name.Name)
				}
				return true
			})
		}
	}
	return writers
}

// recordStringConstants is each constant of record's own source (its test files left out) whose
// value is a string literal, by name: what a record.Notice literal's Kind names when it names
// record.ClaimWriteRefusedNotice rather than "claim-write-refused".
func recordStringConstants(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(noticeKindsSource), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	constants := map[string]string{}
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
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != gotoken.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for at, name := range value.Names {
					if at >= len(value.Values) {
						continue
					}
					if literal, ok := value.Values[at].(*ast.BasicLit); ok && literal.Kind == gotoken.STRING {
						if text, err := strconv.Unquote(literal.Value); err == nil {
							constants[name.Name] = text
						}
					}
				}
			}
		}
	}
	return constants
}

// noticeLiteralKind is the kind node names where it is a record.Notice literal whose Kind is a
// string, or one of record's string constants (constants), and "" otherwise.
func noticeLiteralKind(node ast.Node, constants map[string]string) string {
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
		switch value := field.Value.(type) {
		case *ast.BasicLit:
			if value.Kind != gotoken.STRING {
				continue
			}
			if kind, err := strconv.Unquote(value.Value); err == nil {
				return kind
			}
		case *ast.SelectorExpr:
			if pkg, ok := value.X.(*ast.Ident); ok && pkg.Name == "record" {
				return constants[value.Sel.Name]
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
