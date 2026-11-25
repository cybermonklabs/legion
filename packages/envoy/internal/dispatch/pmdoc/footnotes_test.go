package pmdoc

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	gmtext "github.com/yuin/goldmark/text"
)

// A footnote reference whose label equals a definition's byte for byte, which this package resolves
// from a map of the parse's definitions by label (footnoteReferenceParser, footnoteDefinitionsOf),
// resolves as goldmark's own footnote reference parser resolves it, which compares the reference
// with every definition in the document's list: the same definition, the first whose label equals
// the reference's byte for byte, and the same Index, each definition numbered in the order of its
// first reference. Each document is read by the document reader and by goldmark with its footnote
// extension alone, and the two give each reference, in document order, the same Index, and each
// definition a reference numbered the same Index: references repeated, references to no definition
// and an empty one, a label defined twice, a label of 1,500 characters (goldmark's footnote parser
// bounds no label's length; its 999 characters bound a link's label alone, parser/link.go:153, 188
// and 288 at v1.8.6) and one a character longer that no definition has, an image's `!` before a
// reference, references after their definitions, and a document with no definition at all.
func TestFootnoteReferencesResolveAsGoldmarksParserResolvesThem(t *testing.T) {
	long := strings.Repeat("l", 1500)
	for _, test := range []struct{ name, markdown string }{
		{"repeated references", "a[^b] b[^a] c[^b] d[^a] e[^c] f[^b]\n\n[^a]: x\n\n[^b]: y\n\n[^c]: z\n"},
		{"references to no definition", "a[^nope] b[^a] c[^missing] d[^a] e[^]\n\n[^a]: x\n"},
		{"a label defined twice", "a[^a] b[^a]\n\n[^a]: first\n\n[^a]: second\n"},
		{"a label of 1,500 characters", "a[^" + long + "] b[^" + long + "x] c[^" + long + "]\n\n[^" + long + "]: x\n"},
		{"an image's ! before a reference", "a![^a] b\n\n[^a]: x\n"},
		{"references after their definitions", "[^a]: x\n\n[^b]: y\n\nc[^b] d[^a] e[^b]\n"},
		{"no definition at all", "a[^a] b[^b]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []byte(test.markdown)
			pc := parser.NewContext()
			pc.Set(writeBudgetKey, NewWriteBudget())
			ours := footnoteNumbering(blockReader.md.Parser().Parse(gmtext.NewReader(source), parser.WithContext(pc)))
			theirs := footnoteNumbering(goldmark.New(goldmark.WithExtensions(extension.Footnote)).Parser().Parse(gmtext.NewReader(source)))
			if ours != theirs {
				t.Errorf("%q: the document reader numbers %s, goldmark's footnote extension %s", test.markdown, ours, theirs)
			}
		})
	}
}

// footnoteNumbering is the Index of each footnote reference under root, in document order, and the
// label and Index of each definition a reference numbered, in label order.
func footnoteNumbering(root ast.Node) string {
	var references, definitions []string
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *extensionast.FootnoteLink:
			references = append(references, strconv.Itoa(node.Index))
		case *extensionast.Footnote:
			if node.Index > 0 {
				definitions = append(definitions, fmt.Sprintf("%q=%d", node.Ref, node.Index))
			}
		}
		return ast.WalkContinue, nil
	})
	slices.Sort(definitions)
	return fmt.Sprintf("references [%s] and definitions [%s]", strings.Join(references, " "), strings.Join(definitions, " "))
}

// A footnote reference resolves to the first definition whose label it equals byte for byte, and
// failing that to the first whose label matches it up to case (footnoteLabelKey), carrying its
// label as written then; a reference that matches no definition stays text. That was goldmark's
// walk of the definitions and this package's fallback together, before footnoteReferenceParser
// resolved every reference from maps of its own: each case here but the last reads the same at
// main. The last is a bang whose next byte opens no label: goldmark's parser read its third byte
// as the caret, so at main `t !x^a] u` beside `[^a]: ` became `t !`, a reference and ` u`, and
// lost the `x`.
func TestFootnoteReferencesResolveToAnEqualLabelFirstAndThenToOneEqualUpToCase(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, markdown string
		// resolved is each reference goldmark's tree holds, in document order: the label of the
		// definition it resolved to, and the label it carries as written where it matched only up
		// to case.
		resolved []string
		rendered string
	}{
		{
			name:     "two definitions apart only in case, each referred to by its own label, and one reference equal to neither",
			markdown: "Upper [^AB], lower [^ab], mixed [^Ab].\n\n[^ab]: lower\n\n[^AB]: upper\n",
			resolved: []string{`1: definition "AB", as written`, `2: definition "ab", as written`, `2: definition "ab", written "Ab"`},
			rendered: "Upper [^AB], lower [^ab], mixed [^Ab].\n\n[^ab]: lower\n\n[^AB]: upper\n",
		},
		{
			name:     "the reference equal to neither first",
			markdown: "Mixed [^Ab], upper [^AB], lower [^ab].\n\n[^ab]: lower\n\n[^AB]: upper\n",
			resolved: []string{`1: definition "ab", written "Ab"`, `2: definition "AB", as written`, `1: definition "ab", as written`},
			rendered: "Mixed [^Ab], upper [^AB], lower [^ab].\n\n[^ab]: lower\n\n[^AB]: upper\n",
		},
		{
			name:     "an image's bang before a reference",
			markdown: "Bang ![^AB] and ![^aB].\n\n[^ab]: lower\n\n[^AB]: upper\n",
			resolved: []string{`1: definition "AB", as written`, `2: definition "ab", written "aB"`},
			rendered: "Bang \\![^AB] and \\![^aB].\n\n[^ab]: lower\n\n[^AB]: upper\n",
		},
		{
			name:     "a reference no definition matches",
			markdown: "None [^zz], one [^ab].\n\n[^ab]: lower\n",
			resolved: []string{`1: definition "ab", as written`},
			rendered: "None [^zz], one [^ab].\n\n[^ab]: lower\n",
		},
		{
			name:     "a bang whose next byte opens no label, beside the definition its rest would name",
			markdown: "t !x^a] u\n\n[^a]: d\n",
			resolved: nil,
			rendered: "t !x^a] u\n\n[^a]: d\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolved := footnoteResolutions(t, test.markdown)
			if got, want := strings.Join(resolved, "; "), strings.Join(test.resolved, "; "); got != want {
				t.Errorf("references resolved as %q, want %q", got, want)
			}
			doc, err := Parse(test.markdown)
			if err != nil {
				t.Fatal(err)
			}
			rendered, err := Render(doc)
			if err != nil {
				t.Fatal(err)
			}
			if rendered != test.rendered {
				t.Errorf("Render(Parse()) = %q, want %q", rendered, test.rendered)
			}
		})
	}
}

// footnoteResolutions is each footnote reference of the tree a document's parse reads markdown
// into, in document order: the label of the definition it resolved to, and the label it carries as
// written (referenceLabelAttr), or "as written" where it carries none.
func footnoteResolutions(t *testing.T, markdown string) []string {
	t.Helper()
	pc, _ := readContext(false, NewWriteBudget(), 0, false)
	source := []byte(markdown)
	root := readSource(blockReader.md.Parser(), source, pc)
	definitions := map[int]string{}
	var links []*extensionast.FootnoteLink
	if err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node := node.(type) {
		case *extensionast.Footnote:
			if node.Index > 0 {
				definitions[node.Index] = string(node.Ref)
			}
		case *extensionast.FootnoteLink:
			links = append(links, node)
		}
		return ast.WalkContinue, nil
	}); err != nil {
		t.Fatal(err)
	}
	var resolved []string
	for _, link := range links {
		written := "as written"
		if label, set := link.AttributeString(string(referenceLabelAttr)); set {
			written = fmt.Sprintf("written %q", label)
		}
		resolved = append(resolved, fmt.Sprintf("%d: definition %q, %s", link.Index, definitions[link.Index], written))
	}
	return resolved
}
