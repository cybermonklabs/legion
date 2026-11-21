package pmdoc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/cputest"
)

// parseFullTarget and renderFullTarget are
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's full-size targets.
const (
	// parseFullTarget is a mebibyte: the write-path limit (MaxDocumentBytes) this loop's parse
	// reads back, since a caller's write of this much is refused for the elements it makes.
	parseFullTarget = 1 << 20
	// renderFullTarget is smaller: large enough for growthScale's quadratic margin, small enough
	// that even the slowest shape (`<a`) stays fast - rendering is not bound by MaxDocumentBytes.
	renderFullTarget = 512 << 10
	// updateSeedTarget is TestUpdateIsLinearInTheBlockCount's seed of one-word paragraphs: the
	// mebibyte a document may hold, which a rebuild from a stored rendering writes whole.
	updateSeedTarget = 1 << 20
	// updateShapeTarget is its other shapes', each 21,845 blocks or fewer, about what the 65,536
	// elements one upload may make allow of them.
	updateShapeTarget = 128 << 10
)

// alternatingBlocks are the documents TestUpdateIsLinearInTheBlockCount replaces every other block
// of: 16,384 blocks is about the most paragraphs the 65,536 elements one upload may make allow.
var alternatingBlocks = []int{4_096, 16_384}

// Parse and Render finish in time linear in the text on the shapes that once cost them quadratic
// time: the work of each at its full size (parseFullTarget/renderFullTarget, above)
// grows less than growthBound times over its work at a growthScale-th of it (growsLinearly,
// below). The work is the user CPU time of the thread that parses or renders (cputest.Growth), not
// wall time and not the process's CPU time: a wait for a core is not in it, nor the kernel's work
// for the thread, such as the reclaim of memory when the machine is short of it, nor the
// collector's work on another thread. The smaller size runs growthScale times in one window and
// the full size once in another, so that the two windows last about as long and a load of the
// machine that comes and goes reaches both alike.
//
// A write of a mebibyte of these is refused for the elements it makes (MaxDocumentElements), so
// the parse here is a read-back's, which counts none and reads all of it. Render's target is
// smaller than parse's (above). The margin the bound keeps is between the work healthy linear code
// does and the far greater work of a reintroduced O(n²) bug, not a hang, which the test binary's
// own timeout reports with the name of the test that runs.
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("parsing %q", shape), parseFullTarget/len(shape)*len(shape), func(size int) func() func() {
				text := strings.Repeat(shape, size/len(shape))
				return cputest.Again(func() {
					if _, err := ParseRendering(text); err != nil {
						t.Fatalf("parse %d bytes of %q: %v", size, shape, err)
					}
				})
			})
		})
	}
	// A block shape: a list item whose typed block holds one list item per blank line. Reading a
	// blank line inside such a block as the item's walked the whole block once for each of them,
	// and the list again for each node of it, which is quadratic in a block of blank lines; the
	// reading is taken once over the document now (readSpreads). A write is
	// what reads it, so this one is timed through ParseForWrite. The unit is one item and 200
	// blank lines, so the items and the lines grow together and a mebibyte of them stays under
	// MaxDocumentElements: the write is accepted at every size here, and any refusal - of the
	// elements above all, which would stop the read before the cost - fails the case rather than
	// passing it.
	{
		open, closer := "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n", "  :::\n"
		repeat := "  - p\n" + strings.Repeat("\n", 200)
		name := "a typed block of spaced items in a list item"
		t.Run(name, func(t *testing.T) {
			growsLinearly(t, name, parseFullTarget/len(repeat)*len(repeat), func(size int) func() func() {
				text := open + strings.Repeat(repeat, size/len(repeat)) + closer
				return cputest.Again(func() {
					if _, err := ParseForWrite(text, nil); err != nil {
						t.Fatalf("%s at %d bytes: %v", name, size, err)
					}
				})
			})
		})
	}
	// A block shape of bare exits: a quoted typed block whose last line holds the quote's marker
	// alone, then the blank lines that make the cost show. Each such block ends where its content
	// did, which puts its exits into the stream before the events of the lines already read;
	// writing them in one by one moved the stream and walked every line's marks per block, so the
	// cost was the blocks times the lines, and the inserts are collected and written once now
	// (insertEvents, writeInserts). The unit is one block and 200 blank lines, so the blocks and
	// the lines grow together and a mebibyte of them stays under MaxDocumentElements: the write is
	// accepted at every size here, and any refusal - of the elements above all, which would stop
	// the read before the cost - fails the case rather than passing it.
	{
		repeat := "> :::callout{#c%d kind=\"note\" title=\"T\"}\n>\nb\n" + strings.Repeat("\n", 200)
		unit := len(fmt.Sprintf(repeat, 1000))
		name := "quoted typed blocks ending on a bare line"
		t.Run(name, func(t *testing.T) {
			growsLinearly(t, name, parseFullTarget/unit*unit, func(size int) func() func() {
				var text strings.Builder
				for index := range size / unit {
					fmt.Fprintf(&text, repeat, index)
				}
				body := text.String()
				return cputest.Again(func() {
					if _, err := ParseForWrite(body, nil); err != nil {
						t.Fatalf("%s at %d bytes: %v", name, size, err)
					}
				})
			})
		})
	}
	// Two-part shapes: an opener half repeated, then a closer half repeated, so each closer looks
	// back past every run before it. goldmark's ProcessDelimiters walks a closer back over every
	// earlier run of either character, so `~~a ` then `a* ` and `*a ` then `a~ ` took 3.8-4.2 s at
	// 112 KB, and pairing `~` runs by length makes `~~a ` then `a~ ` and `~a ` then `a~~ ` the same
	// walk (dispatch://INFRA-534). The rule of three keeps every `*` of `a*a`, which can both open
	// and close, from every `**` before it, so a walk passed each of them: `**a ` then `a*a` took
	// 100 s at 437 KiB. Each is read as a rendering is and as a caller's write is; a write of a
	// mebibyte may be refused for the elements it makes, and a shape nesting past maxInlineNesting
	// for that, each timed all the same.
	for _, shape := range []struct{ before, open, close, after string }{
		{"", "~~a ", "a~ ", ""},
		{"", "~a ", "a~~ ", ""},
		{"", "~~a ", "a* ", ""},
		{"", "*a ", "a~ ", ""},
		{"~a [", "~~a ", "a~ ", "](u)"},
		{"", "*~a", "~*", ""},
		{"", "**a ", "a*a", ""},
		{"", "a**a*a ", "a*a**a", ""},
	} {
		pair := len(shape.open) + len(shape.close)
		name := fmt.Sprintf("%s%s…%s…%s", shape.before, shape.open, shape.close, shape.after)
		for _, reader := range []struct {
			name  string
			parse func(string) (*Node, error)
		}{{"ParseRendering", ParseRendering}, {"Parse", Parse}} {
			t.Run(reader.name+" "+name, func(t *testing.T) {
				growsLinearly(t, fmt.Sprintf("%s of %q", reader.name, name), parseFullTarget/pair*pair, func(size int) func() func() {
					repeats := size / pair
					text := shape.before + strings.Repeat(shape.open, repeats) + strings.Repeat(shape.close, repeats) + shape.after
					return cputest.Again(func() {
						if _, err := reader.parse(text); err != nil && !refusedForItsMarks(err) {
							t.Fatalf("%s of %d bytes of %q: %v", reader.name, size, name, err)
						}
					})
				})
			})
		}
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		doc := func(repeats int) *Node {
			return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, repeats)}}}}}
		}
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("rendering %q", shape), renderFullTarget/len(shape)*len(shape), func(size int) func() func() {
				tree := doc(size / len(shape))
				return cputest.Again(func() {
					if _, err := Render(tree); err != nil {
						t.Fatalf("render %d bytes of %q: %v", size, shape, err)
					}
				})
			})
		})
	}
}

// refusedForItsMarks reports whether err refuses markdown for the inline marks it makes: more
// elements than a write may make, or marks nested past maxInlineNesting.
func refusedForItsMarks(err error) bool {
	nesting := nestingError{}
	return errors.Is(err, ErrTooManyElements) || errors.As(err, &nesting) && nesting.inline
}

// The shapes a sweep found after the ones above, each quadratic before LEGION-465 finished: a
// text of one delimiter character, whose every character was escaped by a scan of its whole run;
// many `[^` before one `]`, whose every label was case-folded to look it up among the footnote
// labels; a text of many line feeds, whose every line joined the rest of the text to ask whether
// only whitespace followed; and a run of `~` in a paragraph, which goldmark's strikethrough parser
// scanned again from every `~` in it. Before the fix 128 KiB of each text took 18-115 s to render
// and 32 KiB of `[^a` 7.7 s. Each shape's work grows less than growthBound times from a
// growthScale-th of its full size to the full size (growsLinearly).
func TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds(t *testing.T) {
	paragraph := func(text string) *Node {
		return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}
	}
	doc := func(blocks ...*Node) *Node {
		return &Node{Type: "doc", Children: blocks}
	}
	definition := func(label string) *Node {
		return &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: []*Node{paragraph("x")}}
	}
	labels := func(size int) string {
		label := strings.Repeat("[^a", (size-1)/3) + "]"
		return label + strings.Repeat("x", size-len(label))
	}
	for _, test := range []struct {
		name string
		doc  func(int) *Node
	}{
		{"a run of *", func(size int) *Node { return doc(paragraph(strings.Repeat("*", size))) }},
		{"a run of _", func(size int) *Node { return doc(paragraph(strings.Repeat("_", size))) }},
		{"a run of ~", func(size int) *Node { return doc(paragraph(strings.Repeat("~", size))) }},
		{"[^a before one ] with no footnote", func(size int) *Node { return doc(paragraph(labels(size))) }},
		{"[^a before one ] with a short label defined", func(size int) *Node {
			return doc(paragraph(labels(size)), definition("a"))
		}},
		{"[^a before one ] with a long label defined", func(size int) *Node {
			return doc(paragraph(labels(size)), definition(strings.Repeat("b", 1000)))
		}},
		{"line feeds", func(size int) *Node { return doc(paragraph(strings.Repeat("a\n", size/2))) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			growsLinearly(t, "rendering "+test.name, renderFullTarget, func(size int) func() func() {
				document := test.doc(size)
				return cputest.Again(func() {
					if _, err := Render(document); err != nil {
						t.Fatalf("render %s at %s: %v", test.name, sizeText(size), err)
					}
				})
			})
		})
	}
	// A run after an escaped `~` may open or close, so its run is scanned rather than refused. A
	// write of a mebibyte of escapes is refused for the elements they make, so `a\~~` repeated is
	// read as a rendering is, which counts none and reads all of it.
	for _, test := range []struct {
		name     string
		markdown func(int) string
		parse    func(string) (*Node, error)
	}{
		{"a run of ~", func(size int) string { return "a" + strings.Repeat("~", size-1) }, Parse},
		{"\\~ and a run of ~", func(size int) string { return "\\~" + strings.Repeat("~", size-2) }, Parse},
		{"a\\~~ repeated", func(size int) string { return strings.Repeat("a\\~~", size/4) }, ParseRendering},
	} {
		t.Run(test.name, func(t *testing.T) {
			growsLinearly(t, "parsing "+test.name, parseFullTarget, func(size int) func() func() {
				markdown := test.markdown(size)
				return cputest.Again(func() {
					if _, err := test.parse(markdown); err != nil {
						t.Fatalf("parse %s at %s: %v", test.name, sizeText(size), err)
					}
				})
			})
		})
	}
}

// A table whose every row holds an escaped pipe in a code span parses in time linear in its size,
// up to the 1 MiB a document may be. goldmark's table transformer checked every code span's text
// against every escaped pipe in the document: on one machine at a load of about 300, 16 KiB of such
// rows parsed in 35 ms and 256 KiB in 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes,
// which replaced it, parsed 16 KiB in 19 ms, 256 KiB in 0.6-0.8 s and 1 MiB in 2.0-2.4 s there. No
// wall-clock bound holds on every runner, so the work of the parse is bounded in its growth
// (growsLinearly), as TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText bounds it. A
// caller's write of a table that size passes the element limit and is refused, so it is read as a
// rendering is, whose parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	growsLinearly(t, "parsing a table of `\\|` cells", 1<<20, func(size int) func() func() {
		markdown := header + strings.Repeat(row, (size-len(header))/len(row))
		return cputest.Again(func() {
			if _, err := ParseRendering(markdown); err != nil {
				t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
			}
		})
	})
}

// A document of many footnote references and many definitions parses in time linear in its size,
// up to the 1 MiB a document may be: one paragraph that refers to each definition, many paragraphs
// that each refer to a definition of their own, and references that match their definitions only
// up to case. goldmark's footnote reference parser walked its whole list of definitions for each
// reference: on one machine at a load of about 800, one paragraph of 16,000 references parsed in
// 1.1 s, 16 times the 65 ms of 4,000. footnoteReferenceParser finds each definition in a map built
// once for each parse.
func TestParseIsLinearInFootnoteReferences(t *testing.T) {
	references := func(size int, each func(index int) (reference, definition string)) string {
		var markdown, definitions strings.Builder
		for index := 1; markdown.Len()+definitions.Len() < size; index++ {
			reference, definition := each(index)
			markdown.WriteString(reference)
			definitions.WriteString(definition)
		}
		return markdown.String() + "\n\n" + definitions.String()
	}
	for _, shape := range []struct {
		name string
		each func(index int) (reference, definition string)
	}{
		{"one paragraph that refers to each definition", func(index int) (string, string) {
			return fmt.Sprintf("x[^%d]", index), fmt.Sprintf("[^%d]: Note %[1]d.\n\n", index)
		}},
		{"many paragraphs that each refer to their own definition", func(index int) (string, string) {
			return fmt.Sprintf("Text %d[^%[1]d].\n\n", index), fmt.Sprintf("[^%d]: Note %[1]d.\n\n", index)
		}},
		{"references that match their definitions up to case", func(index int) (string, string) {
			return fmt.Sprintf("x[^N%d]", index), fmt.Sprintf("[^n%d]: Note %[1]d.\n\n", index)
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			growsLinearly(t, "parsing "+shape.name, 1<<20, func(size int) func() func() {
				markdown := references(size, shape.each)
				return cputest.Again(func() {
					if _, err := ParseRendering(markdown); err != nil {
						t.Fatalf("parse %d bytes of footnotes: %v", size, err)
					}
				})
			})
		})
	}
}

// A bare URL or email is read and written in time linear in its line. A line can hold an address
// opening at every place the browser editor's engine starts one - after whitespace, any
// punctuation, an escape, a character reference or a letter outside ASCII, in the markdown its
// tokenizer reads and in the decoded text its transform reads - that the engine refuses only at
// its domain, its `@` or its previous character, so nothing a refused read scans may be scanned
// again from the next place. Each shape is one paragraph of one text node, which no element limit
// stops, read as a rendering is and written from that tree.
//
// A text `<` before each address that opens a comment, a processing instruction, a declaration, a
// CDATA section or a tag the run's markdown closes only at its end is asked whether that markdown
// reads as raw HTML there (rawHTMLScan), which reads to the closer at the run's end, and past the
// next `<` in a quoted attribute value. Each such `<` is then escaped, so the readers, which search
// from each `<` they read as one for its closer, read none of them.
//
// A paragraph that reads back in no spelling is written in every spelling runSpellings lists for
// it, and unreadableRun calls for every family of them. The plain text after it is in each writing,
// and each distinct writing is read back once (TestRenderReadsEachWritingBackUnderAReaderOnce). It
// is timed up to renderFullTarget, as TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText
// times a render.
func TestBareURLShapesParseAndRenderLinearly(t *testing.T) {
	for _, shape := range []string{
		"_www.a", "-www.x_", `(https://-a.b\`, ".www.a&", ".www", "www.", `www\.`, `www\.a `, "HTTP://a ", ` "'https://-a.b`, `(""www.a_`, ` ".www.a`,
		";https://-a.b", "x;www.a_", `\*https://-a`, "&amp;https://-a.", "éhttps://-a.", `http\://-a.`, `www\.a_`, `x"www.a_`, ";;", `\*`, ";a_a@", "é+a@b-", `www\.a@`,
	} {
		readsAndWritesLinearly(t, "", shape, "", 1<<20)
	}
	for _, shape := range []struct{ opener, closer string }{
		{"&lt;!-- a@b.cc ", "-->"}, {"&lt;?x;https://a.bc/ ", "?>"}, {"&lt;!X a@b.cc ", ">"}, {"&lt;![CDATA[ a@b.cc ", "]]>"},
		{`&lt;a b="x;a@b.cc `, `">`}, {`&lt;a b='&lt;a c="x;a@b.cc `, `'>`},
	} {
		readsAndWritesLinearly(t, "", shape.opener, shape.closer, 1<<20)
	}
	readsAndWritesLinearly(t, unreadableRun, "lorem ipsum ", "", renderFullTarget)
}

// A paragraph of declarations no `>` closes is read and written in time linear in its size. A
// declaration opens at `<!` and any ASCII letter (opensDeclaration) and ends at the first `>`
// after it, over as many lines of its block as that takes, so an opener with no `>` after it
// walked to the block's end and kept nothing: every later opener in the paragraph walked the same
// bytes again. A mebibyte of `p ` and `<!a` repeated took 9.97 s to Parse and 19.38 s to
// ParseForWrite, and `<!A` 19.24 s and 37.02 s, since goldmark's own raw HTML parser, which opens
// a declaration after an uppercase letter, walked each of them again after this package's parser
// gave up; `x <!a` on each line, where a walk crosses every later line of the paragraph, grew
// about fourfold per doubling, 6.88 s at 64 KiB and 27.60 s at 128 KiB. The block's record of the
// offset from which it holds no `>` (unclosedRawHTML) now answers every later opener without a
// walk, and the one parser that reads every kind of inline raw HTML (inlineRawHTMLParser, which
// wraps goldmark's own) walks a declaration once rather than twice: the same reads take 0.21 s,
// 0.67 s, 0.20 s, 0.51 s, 0.05 s and 0.13 s.
//
// So is a paragraph of the three other kinds of inline raw HTML that end at a closer of their own
// - a comment at `-->`, a processing instruction at `?>`, a CDATA section at `]]>` - whose walk
// is goldmark's, inside the wrapped parser, and which goldmark has walked from every opener to
// the block's end since it had a raw HTML parser. Before the record a mebibyte of `p ` and
// `<!--` repeated took 24.97 s to Parse and 50.17 s to ParseForWrite, `<?` 46.31 s and 93.65 s,
// and `<![CDATA[`, whose parse the element limit refuses past, 1.61 s and 1.66 s. The record
// holds one offset per kind, and the wrapper records for those three what the wrapped parser's
// own walk answered, so the same reads take 0.23 s and 0.55 s, 0.22 s and 0.62 s, and 0.15 s
// and 0.11 s.
//
// Each shape is one paragraph, read as a rendering is and written from that tree, at the size
// TestBareURLShapesParseAndRenderLinearly reads its own shapes at. All but the CDATA sections are
// one text node, which no element limit stops; each `[` of those is a link opener of its own, so
// a caller's write of a mebibyte of them is refused for the elements they make, which the
// read-back counts none of.
func TestUnclosedRawHTMLShapesParseAndRenderLinearly(t *testing.T) {
	for _, form := range []struct{ head, shape string }{
		{"p ", "<!a"}, {"p ", "<!A"}, {"", "x <!a\n"},
		{"p ", "<!--"}, {"p ", "<?"}, {"p ", "<![CDATA["},
	} {
		readsAndWritesLinearly(t, form.head, form.shape, "", 1<<20)
	}
}

// readsAndWritesLinearly requires a paragraph of head, then shape repeated to a size of full and
// then tail to be read as a rendering is and written from that tree in time linear in its size
// (growsLinearly).
func readsAndWritesLinearly(t *testing.T, head, shape, tail string, full int) {
	t.Helper()
	what := head + shape + tail
	growsLinearly(t, fmt.Sprintf("reading and writing %q", what), full, func(size int) func() func() {
		markdown := head + strings.Repeat(shape, (size-len(head))/len(shape)) + tail + "._\n"
		return cputest.Again(func() {
			doc, err := ParseRendering(markdown)
			if err != nil {
				t.Fatalf("parse %d bytes of %q: %v", size, what, err)
			}
			if _, err := Render(doc); err != nil {
				t.Fatalf("render %d bytes of %q: %v", size, what, err)
			}
		})
	})
}

// Writing a tree of many blocks into a document takes time linear in its blocks when the blocks it
// adds or replaces stand together (LEGION-465). Update inserted the children a fragment gains one
// index at a time, and ygo's fragment insert walks the fragment's children to the index it is given
// - an element no document holds yet copies the children it has buffered instead - so n blocks
// took about n²/2 steps. On one machine at a load of 140 to 200, seeding 16,384 one-word
// paragraphs took 2.1 s of CPU and 65,536 took 36 s, a quote of 21,844 rules 4.3 s, and replacing
// a quote of 21,844 empty headings with a quote of as many rules 11 s through the upload route.
// Nothing in Update bounds the blocks: a rebuild from the latest version writes a stored rendering
// whole, whatever it weighs, so the seed of paragraphs runs to the mebibyte a document may hold
// (updateSeedTarget). The work of each write grows less than growthBound times from a
// growthScale-th of its blocks to all of them (growsLinearly), and each write goes into a document
// of its own, which Growth builds and seeds before the window opens.
//
// Replaced blocks that stand apart are not linear. ygo's Delete and Insert walk the fragment from
// its start to the index each is given (crdt/yxml.go leftChildAt, deleteChildRange, in the pinned
// sjawhar/ygo v1.55.1-sami.2), and a replaced block between two kept ones is a run of its own, so a
// document whose every other block changes type costs a walk per replaced block, as it did before
// runs were batched. That shape is held to ygo's own walks instead: the work of Update is no more
// than alternatingAllowance times the work of a Delete and an Insert at each replaced index on the
// same document, the two timed in turn in windows of cputest.Growth at a scale of 1 (grown). On one
// machine at a load of 105 to 121, back to back, it took 134 ms and 5.83 s at 4,096 and 16,384
// blocks (2.1 and 1.6 times ygo's walks), the Update before sjawhar/legion#1892 112 ms and 5.65 s
// (1.6 and 1.6 times); the write that typed its batch from the live fragment's children, walking
// the whole fragment once more per replaced block, took 936 ms at 4,096 (12.7 times).
func TestUpdateIsLinearInTheBlockCount(t *testing.T) {
	for _, test := range []struct {
		name, from, to string
		target         int
	}{
		{"seeding paragraphs", "", "a\n\n", updateSeedTarget},
		{"seeding a quote of rules", "", "> ***\n", updateShapeTarget},
		{"replacing headings with paragraphs", "#\n", "a\n\n", updateShapeTarget},
		{"replacing a quote of headings with a quote of rules", "> #\n", "> ***\n", updateShapeTarget},
	} {
		t.Run(test.name, func(t *testing.T) {
			growsLinearly(t, test.name, test.target/len(test.to)*len(test.to), func(size int) func() func() {
				blocks := size / len(test.to)
				var seed *Node
				if test.from != "" {
					seed = repeatedBlocks(t, test.from, blocks)
				}
				want := repeatedBlocks(t, test.to, blocks)
				seeded := func() (*crdt.Doc, *crdt.YXmlFragment) {
					doc := crdt.New()
					fragment := doc.GetXmlFragment("prosemirror")
					if seed != nil {
						writeUpdate(t, doc, fragment, seed)
					}
					return doc, fragment
				}
				readsBack(t, test.name, seeded, want)
				return func() func() {
					doc, fragment := seeded()
					return func() { writeUpdate(t, doc, fragment, want) }
				}
			})
		})
	}
	t.Run("replacing every other block", func(t *testing.T) {
		for _, blocks := range alternatingBlocks {
			// Headings and paragraphs alternate, and every block becomes a paragraph: every heading
			// is a run of its own, between two paragraphs Update keeps.
			seed := repeatedBlocks(t, "#\n\na\n\n", blocks/2)
			from := func() (*crdt.Doc, *crdt.YXmlFragment) {
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				writeUpdate(t, doc, fragment, seed)
				return doc, fragment
			}
			want := repeatedBlocks(t, "a\n\n", blocks)
			readsBack(t, "replacing every other block", from, want)
			update := func() func() {
				doc, fragment := from()
				return func() { writeUpdate(t, doc, fragment, want) }
			}
			walks := func() func() {
				doc, fragment := from()
				return func() {
					doc.Transact(func(txn *crdt.Transaction) {
						for index := 0; index < blocks; index += 2 {
							fragment.Delete(txn, index, 1)
							fragment.InsertElement(txn, index, crdt.NewYXmlElement("paragraph"))
						}
					})
				}
			}
			ratio, walked, took, repeat := grown(t, fmt.Sprintf("replacing every other block of %d", blocks), 1, walks, update)
			if ratio > alternatingAllowance {
				t.Errorf("replacing every other block of %d took %v, %.1f times the %v that ygo's own Delete and Insert at those indexes take (each run %d times, at most %d)",
					blocks, took, ratio, walked, repeat, alternatingAllowance)
			}
			t.Logf("replacing every other block of %d: %v, %.1f times ygo's walks (%v, each run %d times, at most %d)", blocks, took, ratio, walked, repeat, alternatingAllowance)
		}
	})
}

// readsBack writes want, untimed, into the document that seeded makes, and requires the document
// to read back as want: so each timed write of want into a document that seeded makes writes it.
func readsBack(t *testing.T, what string, seeded func() (*crdt.Doc, *crdt.YXmlFragment), want *Node) {
	t.Helper()
	doc, fragment := seeded()
	writeUpdate(t, doc, fragment, want)
	if got, err := Read(fragment); err != nil || !got.Equal(want) {
		t.Fatalf("%s: %d blocks read back otherwise (%v)", what, len(want.Children), err)
	}
}

// alternatingAllowance is how many times ygo's own index walks replacing every other block may
// take (TestUpdateIsLinearInTheBlockCount). Update's work beside the walks - building each block
// and comparing the trees - grows linearly while the walks grow quadratically, so the ratio falls
// as the document grows: 1.6 to 2.6 at the sizes the test runs, the Update before
// sjawhar/legion#1892 included, against 12.7 for a write that walks the fragment once more per
// replaced block.
const alternatingAllowance = 4

// repeatedBlocks is the tree of unit written blocks times, read as a stored rendering is, which no
// element limit stops.
func repeatedBlocks(t *testing.T, unit string, blocks int) *Node {
	t.Helper()
	tree, err := ParseRendering(strings.Repeat(unit, blocks))
	if err != nil {
		t.Fatalf("parse %d of %q: %v", blocks, unit, err)
	}
	return tree
}

// writeUpdate writes tree into fragment in one transaction of doc.
func writeUpdate(t *testing.T, doc *crdt.Doc, fragment *crdt.YXmlFragment, tree *Node) {
	t.Helper()
	if err := doc.TransactE(func(txn *crdt.Transaction) error { return Update(txn, fragment, tree) }); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// growthScale is how many times the full size of each growth row of this package is its smaller
// size, and growthBound how many times its work may grow from the smaller size to the full one
// (growsLinearly): work in proportion to the input grows about growthScale times, and work for
// each pair of units about growthScale² (256) times, so a walk for each pair breaks the bound once
// it is about a sixth of the work at the smaller size.
const (
	growthScale = 16
	growthBound = 48
)

// growthSamples is how many times cputest.Growth times each window of a row.
const growthSamples = 3

// growthWindow is about the least that grown lets each window of cputest.Growth last: four times
// cputest.MinWindow, so that a machine several times faster than the one it was measured on still
// reads windows past cputest.MinWindow. maxRepeat is the most times grown repeats each run.
const (
	growthWindow = 4 * cputest.MinWindow
	maxRepeat    = 1 << 16
)

// growsLinearly requires the work at a size of full to grow less than growthBound times over the
// work at full/growthScale (grown). input builds the input of one size, untimed, and returns the
// small or large of cputest.Growth for it: cputest.Again of a run that changes nothing, or a
// function that builds a fresh input for each run.
func growsLinearly(t *testing.T, what string, full int, input func(size int) func() func()) {
	t.Helper()
	small := full / growthScale
	growth, smallWindow, largeWindow, repeat := grown(t, what, growthScale, input(small), input(full))
	t.Logf("%s: %s %d times %v, %s once %v, each run %d times: %.1f times", what, sizeText(small), growthScale, smallWindow, sizeText(full), largeWindow, repeat, growth)
	if growth >= growthBound {
		t.Errorf("%s: the work grew %.1f times, from %v for %d runs of %s to %v for one run of %s, each run %d times, want less than %d times",
			what, growth, smallWindow, growthScale, sizeText(small), largeWindow, sizeText(full), repeat, growthBound)
	}
}

// grown is cputest.Growth of small and large at scale, with each run of a window repeated as many
// times as makes the window last growthWindow or more: a reading that Growth refuses for a window
// shorter than cputest.MinWindow (cputest.ErrShortWindow) is taken again with each run repeated
// as many more times as brings the shorter window past growthWindow. Any other refusal fails t.
func grown(t *testing.T, what string, scale int, small, large func() func()) (float64, time.Duration, time.Duration, int) {
	t.Helper()
	for repeat := 1; ; {
		growth, smallWindow, largeWindow, err := cputest.Growth(growthSamples, scale, repeated(repeat, small), repeated(repeat, large))
		if err == nil {
			return growth, smallWindow, largeWindow, repeat
		}
		shortest := min(smallWindow, largeWindow)
		if !errors.Is(err, cputest.ErrShortWindow) || shortest <= 0 || repeat >= maxRepeat {
			t.Fatalf("%s, each run %d times: %v", what, repeat, err)
		}
		repeat *= int(growthWindow/shortest) + 1
	}
}

// repeated is input with each run repeated times: each call builds the inputs of times runs and
// returns one run that runs them in turn.
func repeated(times int, input func() func()) func() func() {
	if times == 1 {
		return input
	}
	return func() func() {
		runs := make([]func(), times)
		for index := range runs {
			runs[index] = input()
		}
		return func() {
			for _, run := range runs {
				run()
			}
		}
	}
}

// processCPUTime is this process's total CPU time, user and system, since it started
// (cputest.Process), not wall time: it leaves out the time the process waits for a core, though
// another process's load still slows its own CPU time through shared caches and clock frequency.
// No test in this package calls t.Parallel (there is no t.Parallel() anywhere under
// internal/dispatch/pmdoc), so ordinarily at most one measured call runs at a time.
// leakedCallGuard (below) is what keeps that true even once a call has missed its own deadline
// and gone on running in the background. Every measured call reads it through timedWindow.
func processCPUTime() time.Duration { return cputest.Process() }

// cpuTimeOf is the CPU time one run of an operation takes, for a test that holds one run to a
// budget of CPU time rather than to a growth (heavyBodyCPUBudget). prepare builds one run's input,
// untimed, and returns the run, as the small and large of cputest.Growth do. cputest.Measure picks
// how many runs one timed window holds, so the clock resolves the window, and divides by them;
// every run's input is built before its window opens (timedWindow), since the input is not the
// code under test.
func cpuTimeOf(prepare func() (run func())) (time.Duration, error) {
	return cputest.Measure(func(count int) (time.Duration, error) {
		runs := make([]func(), count)
		for i := range runs {
			runs[i] = prepare()
		}
		return timedWindow(func() {
			for _, run := range runs {
				run()
			}
		})
	})
}

// timedWindow is the CPU time this process spends running op, with the garbage collector settled
// before op starts and stopped while it runs, so the time is op's own work alone.
//
// A collection inside the window adds CPU time that depends on when it lands rather than on op.
// Whether one lands depends on the heap goal the untimed setup and the tests before it leave
// behind, so a small size's write can run with no collection at all while a full-size write runs
// one or two. A collection's own CPU time varies several times over between cycles, since its
// idle mark workers run on every processor the single-threaded write leaves idle and
// processCPUTime counts them. Either way the full size's time grows against the small size's
// without the work growing.
//
// Stopping the collector hides nothing a regression would show: work that grows quadratically
// does so on op's own goroutine, and allocation that grows quadratically still pays inside the
// window for the fresh memory it touches. The heap may grow to windowMemoryLimit while the
// collector is stopped, so a regression that allocates without bound is collected rather than
// killing the process; a window that reaches the limit comes back with an error naming its
// collections, since that collection would put its noise back into the time.
//
// timedWindow reports through no *testing.T. It runs inside the goroutine runWithDeadline starts,
// which a call that misses its deadline leaves running past its test's end, and testing panics the
// whole binary on a Fail from such a goroutine; the caller waiting on runWithDeadline reports the
// error on its test's own goroutine (timedWithDeadline). A call that misses its deadline may also
// never return to restore the collector, so runWithDeadline restores it as well.
func timedWindow(op func()) (time.Duration, error) {
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	limit := min(debug.SetMemoryLimit(-1), windowMemoryLimit)
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(limit))
	cycles := gcCycles()
	before := processCPUTime()
	op()
	took := processCPUTime() - before
	if collected := gcCycles() - cycles; collected != 0 {
		return took, fmt.Errorf("%d collections ran inside a timed window, which grew the heap to the %d MiB memory limit; their CPU time is in its %s", collected, limit>>20, took.Round(time.Millisecond))
	}
	return took, nil
}

// windowMemoryLimit is the most memory timedWindow lets a window grow the heap to with the
// collector stopped, unless the process already runs under a lower limit. A window times the parse
// of one body that a test holds to a budget (heavyBodyCPUBudget), or of many runs of a smaller one
// that hold about 50 of the clock's steps of work (cputest.Measure), around a second's even at
// gVisor's 10 ms step.
const windowMemoryLimit = 3 << 30

// gcCycles is how many collections this process has completed.
func gcCycles() uint64 {
	sample := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// collectorSettings is GOGC and the memory limit as they stand, read without changing either.
func collectorSettings() (percent int, limit int64) {
	sample := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(sample)
	return int(int64(sample[0].Value.Uint64())), debug.SetMemoryLimit(-1)
}

// leaking counts how many of each what (timedWithDeadline's label) are currently
// running past their own deadline - a count, not a single flag, so two concurrently leaking calls
// sharing one label cannot be mistaken for one clearing when either returns. Go cannot cancel a
// goroutine mid-computation, so this is the only way a later call can tell that measuring right
// now would share processCPUTime's process-wide counter with CPU a still-running earlier call
// keeps spending. runWithDeadline adds to it the instant a deadline misses and removes from it
// only once that call's own goroutine finally returns; leakedCallGuard reads it.
var leaking struct {
	mu   sync.Mutex
	what map[string]int
}

// leakedCallGuard reports what, if anything, is still running past its own deadline - the message
// timedWithDeadline fails a new call with before ever starting it, naming the call still leaking
// CPU into this process's RUSAGE_SELF rather than inventing a ratio out of a measurement that
// shared it. A pure function (no *testing.T) so a unit test can assert it directly: see
// TestLeakedCallBlocksTheNextMeasurement.
func leakedCallGuard(what string) string {
	leaking.mu.Lock()
	defer leaking.mu.Unlock()
	if len(leaking.what) == 0 {
		return ""
	}
	labels := make([]string, 0, len(leaking.what))
	for label, count := range leaking.what {
		if count > 1 {
			label = fmt.Sprintf("%s (x%d)", label, count)
		}
		labels = append(labels, label)
	}
	sort.Strings(labels) // a fixed order, not map iteration's, so the message is reproducible
	return fmt.Sprintf("%s: refusing to measure while %s is still running past its own deadline (processCPUTime is this whole process's, so measuring now would attribute its ongoing CPU to this call instead)", what, strings.Join(labels, ", "))
}

// runWithDeadline runs op, labelled what, in its own goroutine and reports whether it returned
// within deadline. Go cannot cancel a goroutine mid-computation, so an op that does not return in
// time is left running - registered under what in leaking until it eventually returns and
// deregisters itself, so leakedCallGuard can refuse a later call that would otherwise share its
// ongoing CPU - rather than stopped; the point of the deadline is only to stop waiting for it and
// report that promptly, not to free its resources. A deregistration goroutine is spawned only on
// a deadline miss, and only then: on a call that truly never returns, it stays parked on result's
// receive, at no CPU cost, for as long as the test binary runs. cleared, non-nil only on a
// deadline miss, closes the instant that goroutine deregisters op's own entry - the signal
// waitForLeakToClear waits on, rather than polling. err is op's own, a timed window's verdict
// (timedWindow), handed back only with an op that returned in time: a late op's is dropped with it,
// since no test waits for it any more. It hands back the collector's settings as it found them on
// either return, since an op stopped inside timedWindow that misses its deadline would
// otherwise leave the collector stopped for every later test in the binary.
func runWithDeadline(what string, deadline time.Duration, op func() (time.Duration, error)) (took time.Duration, onTime bool, cleared <-chan struct{}, err error) {
	percent, limit := collectorSettings()
	defer debug.SetMemoryLimit(limit)
	defer debug.SetGCPercent(percent)
	type outcome struct {
		took time.Duration
		err  error
	}
	result := make(chan outcome, 1)
	go func() {
		took, err := op()
		result <- outcome{took, err}
	}()
	select {
	case got := <-result:
		return got.took, true, nil, got.err
	case <-time.After(deadline):
		leaking.mu.Lock()
		if leaking.what == nil {
			leaking.what = make(map[string]int)
		}
		leaking.what[what]++
		leaking.mu.Unlock()
		done := make(chan struct{})
		go func() {
			<-result
			leaking.mu.Lock()
			leaking.what[what]--
			if leaking.what[what] == 0 {
				delete(leaking.what, what)
			}
			leaking.mu.Unlock()
			close(done)
		}()
		return 0, false, done, nil
	}
}

// timedWithDeadline refuses to run op, naming whatever is still leaking (leakedCallGuard), then
// runs it under runWithDeadline and fails t, naming what and deadline, if it did not return in
// time, and reports op's error, naming what, if it did. Both are reported here, on t's own
// goroutine, never from inside op, which can outlive t.
func timedWithDeadline(t *testing.T, what string, deadline time.Duration, op func() (time.Duration, error)) time.Duration {
	t.Helper()
	if msg := leakedCallGuard(what); msg != "" {
		t.Fatal(msg)
	}
	took, onTime, _, err := runWithDeadline(what, deadline, op)
	if !onTime {
		t.Fatalf("%s did not return within %s", what, deadline)
	}
	if err != nil {
		t.Errorf("%s: %v", what, err)
	}
	return took
}

// TestRunWithDeadlineReportsAHangPromptly proves runWithDeadline's own guarantee directly: a
// synthetic op that blocks far longer than its deadline is reported late (onTime=false) promptly -
// close to the deadline itself, not close to how long op actually blocks - the property
// timedWithDeadline relies on to fail fast and name its call instead of waiting out go test's own
// default timeout with none attached.
func TestRunWithDeadlineReportsAHangPromptly(t *testing.T) {
	const deadline = 20 * time.Millisecond
	release := make(chan struct{})
	started := time.Now()
	_, onTime, cleared, _ := runWithDeadline("synthetic prompt hang", deadline, func() (time.Duration, error) {
		<-release // blocked until this test has its verdict, however the scheduler runs it
		return 0, nil
	})
	if elapsed := time.Since(started); elapsed > 10*deadline {
		t.Errorf("runWithDeadline took %s to report a hang against a %s deadline, want close to immediate", elapsed, deadline)
	}
	if onTime {
		t.Error("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestLeakedCallBlocksTheNextMeasurement proves leakedCallGuard directly: once a synthetic op has
// missed its own deadline and is still running, a second, otherwise-healthy call through
// timedWithDeadline refuses to start - naming the first call - rather than silently sharing its
// still-accruing CPU (processCPUTime is this whole process's, not per-goroutine). The op is held
// until the guard has answered, so it is still running when asked, however late the test runs.
func TestLeakedCallBlocksTheNextMeasurement(t *testing.T) {
	const deadline = 20 * time.Millisecond
	release := make(chan struct{})
	_, onTime, cleared, _ := runWithDeadline("synthetic leaked hang", deadline, func() (time.Duration, error) {
		<-release
		return 0, nil
	})
	if onTime {
		close(release)
		t.Fatal("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	if msg := leakedCallGuard("a second call"); msg == "" {
		t.Error("leakedCallGuard let a second call measure while the first was still registered as leaked, want it refused")
	} else {
		t.Logf("leakedCallGuard correctly refused: %s", msg)
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestAMissedDeadlineLeavesTheCollectorAsItWas proves runWithDeadline hands back the collector as
// it found it when a call misses its deadline inside timedWindow, where the collector is
// stopped: the call may never return to restore it, and a collector left stopped would hold every
// later test in this binary to the window's memory limit with no collection below it.
func TestAMissedDeadlineLeavesTheCollectorAsItWas(t *testing.T) {
	percent, limit := collectorSettings()
	stopped, release := make(chan struct{}), make(chan struct{})
	runtime.GC() // so the window's own collection takes no part of the deadline
	_, onTime, cleared, _ := runWithDeadline("synthetic hang inside a timed window", time.Second, func() (time.Duration, error) {
		return timedWindow(func() {
			close(stopped)
			<-release
		})
	})
	if onTime {
		close(release)
		t.Fatal("runWithDeadline reported onTime=true for an op still blocked past its deadline")
	}
	select {
	case <-stopped:
	default:
		close(release)
		waitForLeakToClear(t, cleared)
		t.Fatal("the op had not stopped the collector by its deadline, so this test proves nothing")
	}
	gotPercent, gotLimit := collectorSettings()
	if gotPercent != percent || gotLimit != limit {
		t.Errorf("after a missed deadline the collector reads GOGC %d and a %d-byte memory limit, want %d and %d as before the call", gotPercent, gotLimit, percent, limit)
	}
	close(release)
	waitForLeakToClear(t, cleared)
}

// TestALateCallFinishingAfterItsTestDoesNotPanic proves a timed call that misses its deadline and
// returns only after its test has ended, with a collection in its window, reports nothing through
// that test: testing panics the whole binary on a Fail from a goroutine whose test has completed,
// which would turn one missed deadline into a crash of every test in pmdoc. The call runs in a
// child process so that crash, if it happens, is this test's failure rather than the binary's; the
// child's own verdict is the missed deadline, reported on its test's goroutine.
func TestALateCallFinishingAfterItsTestDoesNotPanic(t *testing.T) {
	child := exec.Command(os.Args[0], "-test.run", "^TestALateCallFinishingAfterItsTestDoesNotPanicHelper$", "-test.count=1", "-test.v")
	child.Env = append(os.Environ(), lateCallHelperEnv+"=1")
	output, _ := child.CombinedOutput() // the helper fails by design: its call misses its deadline
	if strings.Contains(string(output), "panic:") {
		t.Fatalf("a call that finished after its test had ended crashed the test binary:\n%s", output)
	}
	if !strings.Contains(string(output), lateCallWhat+" did not return within") || !strings.Contains(string(output), lateCallFinished) {
		t.Fatalf("the child process did not report the missed deadline and then outlive the late call:\n%s", output)
	}
}

const (
	// lateCallHelperEnv names the child process TestALateCallFinishingAfterItsTestDoesNotPanic
	// starts.
	lateCallHelperEnv = "PMDOC_LATE_CALL_HELPER"
	// lateCallWhat labels the child's call.
	lateCallWhat = "synthetic call finishing after its test"
	// lateCallFinished is what the child logs once the late call has returned.
	lateCallFinished = "the late call returned after its test had ended"
)

// TestALateCallFinishingAfterItsTestDoesNotPanicHelper is that child process: a subtest whose call
// misses its deadline through timedWithDeadline, as heavyBodyCPUBudget's measurement does, then the
// call's release once the subtest has ended, a collection inside its window, and a wait until it
// has returned.
func TestALateCallFinishingAfterItsTestDoesNotPanicHelper(t *testing.T) {
	if os.Getenv(lateCallHelperEnv) == "" {
		t.Skip("the child process of TestALateCallFinishingAfterItsTestDoesNotPanic")
	}
	stopped, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	runtime.GC() // so the window's own collection takes no part of the deadline
	t.Run("a call that misses its deadline", func(t *testing.T) {
		timedWithDeadline(t, lateCallWhat, time.Second, func() (time.Duration, error) {
			defer close(finished)
			return timedWindow(func() {
				close(stopped)
				<-release
				runtime.GC() // a collection inside the window, once its test has ended
			})
		})
	})
	select {
	case <-stopped:
	default:
		t.Fatal("the op had not opened its window by its deadline, so this test proves nothing")
	}
	close(release)
	select {
	case <-finished:
		t.Log(lateCallFinished)
	case <-time.After(10 * time.Second):
		t.Fatal("the late call never returned")
	}
}

// waitForLeakToClear blocks until cleared closes or fails t: the deregistration goroutine
// runWithDeadline spawned on a deadline miss closes cleared the instant it has removed its own
// entry from leaking, so a later test in this same binary never starts measuring while this
// test's own synthetic leak is still registered. 10s (packages/envoy/internal/cistore/burst_test.go's
// waitAll uses the same bound for the same kind of wait) is far more than the handoff needs; past
// it, something is wrong.
func waitForLeakToClear(t *testing.T, cleared <-chan struct{}) {
	t.Helper()
	select {
	case <-cleared:
	case <-time.After(10 * time.Second):
		t.Fatal("a synthetic leaked call never deregistered")
	}
}

// perCallReportMargin is what perCallDeadline leaves of the test binary's own deadline, so a
// measured call that hangs fails by name (timedWithDeadline) while go test's own timeout is still
// some way off.
const perCallReportMargin = 60 * time.Second

// perCallHangCap bounds a measured call when the run has no deadline to read (-timeout 0): a
// hang still has to be reported, and half an hour is far past any healthy call at these sizes.
const perCallHangCap = 30 * time.Minute

// perCallDeadline bounds a call that timedWithDeadline measures: what the test binary's own
// deadline leaves after perCallReportMargin, capped at perCallHangCap. It is read from the
// deadline rather than fixed because each measured call is a CPU-time measurement (cpuTimeOf,
// cputest.Measure) that repeats its operation until a window reaches the measurement's
// resolution, so the wall time it takes is the CPU it needs multiplied by whatever contention the
// runner is under; a fixed wall-clock bound over a CPU-time budget fails healthy calls on a
// loaded shared runner, which is what this file's own history records. For scale: a healthy
// '<a b="x;a@b.cc ">' call at the 1 MiB tag shape, measured under -race in a two-CPU gVisor pod on
// 2026-11-03, took 50.26s - 84% of the 60s bound this replaced, which is why the bound had to go
// rather than be raised. A run whose deadline leaves no more than the
// margin fails once, by name: that reads like a misconfigured run, not a hang.
func perCallDeadline(t *testing.T) time.Duration {
	t.Helper()
	deadline, bounded := t.Deadline()
	if !bounded {
		return perCallHangCap
	}
	left := time.Until(deadline)
	if left <= perCallReportMargin {
		t.Fatalf("the test binary's deadline leaves %s, not more than perCallReportMargin (%s): this looks like a misconfigured run; raise -timeout", left.Round(time.Millisecond), perCallReportMargin)
	}
	return min(perCallHangCap, left-perCallReportMargin)
}

// sizeText is size, a count of bytes, in KiB or MiB.
func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}
