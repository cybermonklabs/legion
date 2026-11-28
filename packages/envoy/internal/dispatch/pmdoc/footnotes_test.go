package pmdoc

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/alonetest"
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

// A typed block calls each label once however often it calls it, a call in its second paragraph
// of a label its first paragraph called included, in the order it first calls it and as that
// first reference holds it. Repeats compare byte for byte, so `A` and `a` are two labels, while a
// label the block defines itself is left out up to case: its definition of `C` leaves out `c`.
func TestABlockCallingEachLabelTwiceNamesEachOnceInTheOrderOfItsFirstCall(t *testing.T) {
	t.Parallel()
	text := func(value string) *Node { return &Node{Type: "text", Text: value} }
	reference := func(label string) *Node { return &Node{Type: "footnote_reference", Attrs: Attrs{"label": label}} }
	block := &Node{Type: "requirement", Attrs: Attrs{BlockIDAttr: "r1"}, Children: []*Node{
		{Type: "paragraph", Children: []*Node{
			text("Calls "), reference("b"), text(", "), reference("a"), text(", "), reference("c"),
			text(" and "), reference("b"), text(" again."),
		}},
		{Type: "paragraph", Children: []*Node{
			text("Then "), reference("A"), text(", "), reference("d"), text(", "), reference("a"),
			text(" and "), reference("c"), text(" again."),
		}},
		footnoteDefinition("C", paragraph("Defined in the block.")),
	}}
	if got, want := CalledFootnoteLabels(block), []string{"b", "a", "A", "d"}; !slices.Equal(got, want) {
		t.Errorf("CalledFootnoteLabels = %q, want %q", got, want)
	}
}

// BenchmarkATypedBlockCallingEightThousandLabels times the labels one typed block calls
// (CalledFootnoteLabels): a requirement whose 80 paragraphs call 8,000 distinct labels, `l0` to
// `l7999`, 100 to a paragraph with text between the calls, and which defines `L0` itself, so that
// `l0` is left out up to case and the check of each call against the block's own definitions runs
// over every call. The block is built once, outside the timed loop.
func BenchmarkATypedBlockCallingEightThousandLabels(b *testing.B) {
	const labels, perParagraph = 8_000, 100
	block := &Node{Type: "requirement", Attrs: Attrs{BlockIDAttr: "r1"}}
	for first := 0; first < labels; first += perParagraph {
		calls := &Node{Type: "paragraph"}
		for index := first; index < first+perParagraph; index++ {
			calls.Children = append(calls.Children,
				&Node{Type: "text", Text: " calls "},
				&Node{Type: "footnote_reference", Attrs: Attrs{"label": "l" + strconv.Itoa(index)}})
		}
		block.Children = append(block.Children, calls)
	}
	block.Children = append(block.Children, footnoteDefinition("L0", paragraph("Defined in the block.")))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := CalledFootnoteLabels(block); len(got) != labels-1 {
			b.Fatalf("CalledFootnoteLabels answers %d labels, want %d", len(got), labels-1)
		}
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
	if got, want := KeyFootnoteLabels(defined).Written(written), []string{"a]b", "esc", "k1"}; !slices.Equal(got, want) {
		t.Errorf("KeyFootnoteLabels(defined).Written(%q) = %q, want %q", written, got, want)
	}
	if got := KeyFootnoteLabels(defined).Written("No call."); len(got) != 0 {
		t.Errorf("KeyFootnoteLabels(defined).Written of a text with no call = %q, want none", got)
	}
}

// longBlockCalls is the markdown of one block, a paragraph of about 2,700 bytes of prose whose
// calls name nine labels its document defines, some of them more than once, `k3` written `[^K3]`
// as well as `[^k3]`, `ref` written `[^Ref]` and `a]b` written as the renderer escapes it,
// `[^a\]b]`, beside a call of `zz`, which nothing defines; nine, those nine labels in definition
// order; and nineThousand, 9,000 distinct labels a document defines that hold the nine in the same
// order, one in each thousand, among labels the block never calls.
func longBlockCalls() (block string, nine, nineThousand []string) {
	nine = []string{"k1", "a]b", "k3", "src", "k5", "fig", "k7", "ref", "k9"}
	calls := []string{"k1", "src", "K3", "a\\]b", "zz", "k5", "fig", "k1", "k7", "Ref", "k9", "a\\]b", "k3"}
	var written strings.Builder
	for sentence := 1; written.Len() < 2_700; sentence++ {
		fmt.Fprintf(&written, "Step %d reads what the step before it wrote and writes what the next one reads[^%s]. ", sentence, calls[(sentence-1)%len(calls)])
	}
	nineThousand = make([]string, 0, 9_000)
	for index, filler := 0, 0; index < 9_000; index++ {
		if index%1_000 == 500 {
			nineThousand = append(nineThousand, nine[index/1_000])
			continue
		}
		nineThousand = append(nineThousand, "n"+strconv.Itoa(filler))
		filler++
	}
	return strings.TrimSpace(written.String()), nine, nineThousand
}

// blockCallsAllocsChildEnv marks the run of the test binary that measures the reading of one
// block's calls alone (TestABlocksCallsCostNothingForEachLabelItsDocumentDefines).
const blockCallsAllocsChildEnv = "ENVOY_TEST_BLOCK_CALLS_ALLOCS_CHILD"

// The reading of one block's calls (FootnoteLabelKeys.Written) against the keys of the 9,000
// labels a document defines makes no more allocations than against the keys of 9, and answers the
// same labels: the nine the block calls, in definition order, those it calls in another case than
// their definitions and the one the renderer escapes among them, and not the one nothing defines
// (longBlockCalls). The server makes the keys of the labels a document defines once and reads each
// block of the document against them (footnotesCalledIn), so a reading that cost something for
// each of those labels would cost a request the document's blocks times its labels.
//
// testing.AllocsPerRun counts every allocation the process makes while it measures, those of
// whatever the other tests of this package leave running included. So the test runs the test
// binary again, for this test alone (alonetest), and the measurement is that run's; it fails
// unless that run reports the measurement.
func TestABlocksCallsCostNothingForEachLabelItsDocumentDefines(t *testing.T) {
	// Not parallel: its run alone counts allocations with testing.AllocsPerRun, which panics
	// during a parallel test.
	if !alonetest.Child(blockCallsAllocsChildEnv) {
		alonetest.Run(t, blockCallsAllocsChildEnv, 1)
		return
	}
	block, nine, nineThousand := longBlockCalls()
	few, many := KeyFootnoteLabels(nine), KeyFootnoteLabels(nineThousand)
	if got := few.Written(block); !slices.Equal(got, nine) {
		t.Errorf("the block's reading against the keys of 9 labels = %q, want %q", got, nine)
	}
	if got := many.Written(block); !slices.Equal(got, nine) {
		t.Errorf("the block's reading against the keys of 9,000 labels = %q, want %q", got, nine)
	}
	fewAllocs := testing.AllocsPerRun(10, func() { few.Written(block) })
	manyAllocs := testing.AllocsPerRun(10, func() { many.Written(block) })
	alonetest.Measured(t, "%v allocations against the keys of 9 labels, %v against those of 9,000, for a block of %d bytes", fewAllocs, manyAllocs, len(block))
	if manyAllocs > fewAllocs {
		t.Errorf("the reading of a block of %d bytes makes %v allocations against the keys of 9,000 labels and %v against those of 9; want no more against more labels", len(block), manyAllocs, fewAllocs)
	}
}

// BenchmarkALongBlocksCallsAgainstNineThousandLabels times the reading of one block of about 2,700
// bytes whose calls name nine labels its document defines against the keys of 9,000 labels
// (longBlockCalls), the keys made once outside the timed loop, as the server makes them once for
// a document (footnotesCalledIn).
func BenchmarkALongBlocksCallsAgainstNineThousandLabels(b *testing.B) {
	block, nine, nineThousand := longBlockCalls()
	keys := KeyFootnoteLabels(nineThousand)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := keys.Written(block); !slices.Equal(got, nine) {
			b.Fatalf("the block's reading answers %q, want %q", got, nine)
		}
	}
}

// longLabelOpenCalls is the markdown of two blocks of about 50,000 bytes, each opening thousands of
// calls that its one `]` closes: open, `[^` written 24,999 times and then `[^k1]`, 25,000 calls;
// escaped, `\[^` written 16,665 times and then `\[^k1]`, 16,666 calls, each opened by a `[^` after
// a backslash; and labels, the labels a document defines, `k1` and one label of 50,000 bytes, `x`
// written 50,000 times, which no call of either block names.
func longLabelOpenCalls() (open, escaped string, labels []string) {
	open = strings.Repeat("[^", 24_999) + "[^k1]"
	escaped = strings.Repeat(`\[^`, 16_665) + `\[^k1]`
	return open, escaped, []string{"k1", strings.Repeat("x", 50_000)}
}

// A block of about 50,000 bytes whose one `]` closes the 25,000 calls it opens writes `k1` alone
// against the keys of `k1` and of a label of 50,000 bytes, and so does one whose 16,666 calls each
// open after a backslash (longLabelOpenCalls): of the labels those calls run to, from `k1` to one
// of about 50,000 bytes, only `k1` folds to the key of a label the document defines. A document's
// author sets the length of a label, so the reading of a block must not fold each call open within
// a bound that grows with it: against a label of 50,000 bytes such a bound would fold every call of
// both blocks, for an answer that stays `k1` (BenchmarkALongBlocksOpenCallsAgainstALongLabel times
// both readings).
func TestALongBlocksOpenCallsAgainstALongLabelWriteTheSameLabels(t *testing.T) {
	t.Parallel()
	open, escaped, labels := longLabelOpenCalls()
	want := []string{"k1"}
	for _, block := range []struct{ name, markdown string }{{"open", open}, {"escaped", escaped}} {
		if got := KeyFootnoteLabels(labels).Written(block.markdown); !slices.Equal(got, want) {
			t.Errorf("the reading of the %s block of %d bytes = %q, want %q", block.name, len(block.markdown), got, want)
		}
	}
}

// BenchmarkALongBlocksOpenCallsAgainstALongLabel times the reading of each block of
// longLabelOpenCalls against the keys of `k1` and of a label of 50,000 bytes: open, whose one `]`
// closes the 25,000 calls it opens, and escaped, whose 16,666 calls each open after a backslash.
// The keys are made once outside the timed loop, as the server makes them once for a document
// (footnotesCalledIn).
func BenchmarkALongBlocksOpenCallsAgainstALongLabel(b *testing.B) {
	open, escaped, labels := longLabelOpenCalls()
	keys := KeyFootnoteLabels(labels)
	want := []string{"k1"}
	read := func(markdown string) func(*testing.B) {
		return func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if got := keys.Written(markdown); !slices.Equal(got, want) {
					b.Fatalf("the block's reading answers %q, want %q", got, want)
				}
			}
		}
	}
	b.Run("open", read(open))
	b.Run("escaped", read(escaped))
}

// A footnote reference resolves to the first definition whose label it equals byte for byte, and
// failing that to the first whose label matches it up to case (FootnoteLabelKey), carrying its
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
