package pmdoc

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	extensionast "github.com/yuin/goldmark/extension/ast"
)

// A block calls each label of its footnote references once, in the order it first calls it, as
// the reference holds it. A label a definition inside the block defines, by the label or only up
// to case, is left out, since the block's own reading resolves the call to that definition, and
// so is every label of a node that is no footnote, such as a list item's marker.
func TestABlockCallsTheLabelsItsOwnDefinitionsDoNotDefine(t *testing.T) {
	t.Parallel()
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	reference := func(label string) *Node { return &Node{Type: "footnote_reference", Attrs: Attrs{"label": label}} }
	block := &Node{Type: "blockquote", Children: []*Node{
		{Type: "paragraph", Children: []*Node{
			text("Calls "), reference("b"), text(", "), reference("a"), text(", "), reference("b"),
			text(" again, "), reference("C"), text(" and "), reference("d"), text("."),
		}},
		bulletList(false, listItem(false, &Node{Type: "paragraph", Children: []*Node{text("In a list "), reference("c")}})),
		footnoteDefinition("c", paragraph("Defined in the block.")),
	}}
	if got, want := CalledFootnoteLabels(block), []string{"b", "a", "d"}; !slices.Equal(got, want) {
		t.Errorf("CalledFootnoteLabels = %q, want %q", got, want)
	}
	if got := CalledFootnoteLabels(paragraph("No call.")); len(got) != 0 {
		t.Errorf("CalledFootnoteLabels of a block with no reference = %q, want none", got)
	}
}

// Markdown with a stand-in definition of each label it calls reads each call as a reference to
// that label, a label the renderer escapes included, and its last blocks are the stand-ins, one
// footnote definition per label in the order given. With no label the markdown is as it stands.
func TestStandInDefinitionsReadBackAsTheLabelsTheyStandFor(t *testing.T) {
	t.Parallel()
	const markdown = "See [^a\\]b] and [^k1]."
	if got := WithStandInDefinitions(markdown, nil); got != markdown {
		t.Errorf("WithStandInDefinitions with no labels = %q, want %q", got, markdown)
	}
	labels := []string{"a]b", "k1"}
	withStandIns := WithStandInDefinitions(markdown, labels)
	if want := markdown + "\n\n[^a\\]b]: x\n\n[^k1]: x"; withStandIns != want {
		t.Fatalf("WithStandInDefinitions = %q, want %q", withStandIns, want)
	}
	doc := ReadForDisplay(withStandIns, DisplayLatest)
	var called, defined []string
	for _, node := range collect(doc, func(n *Node) bool { return n.Type == "footnote_reference" }) {
		label, _ := node.Attrs["label"].(string)
		called = append(called, label)
	}
	for _, node := range doc.Children[1:] {
		label, _ := node.Attrs["label"].(string)
		defined = append(defined, node.Type+" "+label)
	}
	if !slices.Equal(called, labels) {
		t.Errorf("%q reads the references %q, want %q", withStandIns, called, labels)
	}
	if want := []string{"footnote_definition a]b", "footnote_definition k1"}; !slices.Equal(defined, want) {
		t.Errorf("%q reads %q after its paragraph, want %q", withStandIns, defined, want)
	}
}

// A document defines each label of its footnote definitions once, in document order, a definition
// in a quote included. A text writes a defined label where it holds `[^label]` with the label
// escaped as the renderer writes it, compared up to case, and an escaped call as well, since the
// characters are what is read; a label that is only the start of a written one is not written.
func TestATextWritesTheDefinedLabelsItsCallsHoldUpToCase(t *testing.T) {
	t.Parallel()
	doc := &Node{Type: "doc", Children: []*Node{
		paragraph("Calls."),
		footnoteDefinition("b", paragraph("B.")),
		footnoteDefinition("a]b", paragraph("A.")),
		{Type: "blockquote", Children: []*Node{footnoteDefinition("esc", paragraph("In a quote."))}},
		footnoteDefinition("b", paragraph("B again.")),
		footnoteDefinition("k1", paragraph("K.")),
		footnoteDefinition("k10", paragraph("Longer.")),
	}}
	defined := FootnoteDefinitionLabels(doc)
	if want := []string{"b", "a]b", "esc", "k1", "k10"}; !slices.Equal(defined, want) {
		t.Fatalf("FootnoteDefinitionLabels = %q, want %q", defined, want)
	}
	const written = "See [^K1], [^a\\]b], \\[^esc] and [^k10x]."
	if got, want := WrittenFootnoteLabels(written, defined), []string{"a]b", "esc", "k1"}; !slices.Equal(got, want) {
		t.Errorf("WrittenFootnoteLabels(%q) = %q, want %q", written, got, want)
	}
	if got := WrittenFootnoteLabels("No call.", defined); len(got) != 0 {
		t.Errorf("WrittenFootnoteLabels of a text with no call = %q, want none", got)
	}
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
