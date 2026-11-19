package pmdoc

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
)

// parseFullTarget and renderFullTarget are
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's full-size targets.
const (
	// parseFullTarget is a mebibyte: the write-path limit (MaxDocumentBytes) this loop's parse
	// reads back, since a caller's write of this much is refused for the elements it makes.
	parseFullTarget = 1 << 20
	// renderFullTarget is smaller: large enough for growthStep's quadratic margin, small enough
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

// Parse and Render finish in work linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each shape is counted at growthStep's fraction of its target size and then at
// the full size (parseFullTarget/renderFullTarget, above), through growsLinearly
// (below).
//
// What each shape's closure counts is the steps the call takes (workOf): the lines, delimiter
// runs and characters the parse reads (ParseWork), the characters the render's scans read
// (RenderWork) and the items a write into a document reaches (UpdateWork), each counted by the
// code that does the work. A count moves only with the work, never with the machine: wall time,
// and CPU time after it, both grew on a loaded runner without the call doing more, which is
// LEGION-569/571's whole flake history across three rounds of widening the growth step and the
// slack, and the collections a timed window ran (INFRA-3149). A call that hangs is the test
// binary's -timeout's to report, as it is every test's.
//
// A write of a mebibyte of these is refused for the elements it makes (MaxDocumentElements), so
// the parse here is a read-back's, which counts none and reads all of it. Render's target is
// smaller than parse's (above): this loop's own ratio check is `<a`'s live linearity
// measurement, at whatever parseFullTarget/renderFullTarget currently are, not a one-time aside
// recorded in a comment to go stale. The margin it checks is between the steps healthy linear
// work takes and the far greater number a reintroduced O(n²) bug would take in the loops that
// count them.
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		full := parseFullTarget / len(shape)
		small := full / growthStep
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("parsing %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) uint64 {
				text := strings.Repeat(shape, size/len(shape))
				return workOf(func() {
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
		full := parseFullTarget / len(repeat)
		small := full / growthStep
		name := "a typed block of spaced items in a list item"
		t.Run(name, func(t *testing.T) {
			growsLinearly(t, name, []int{small * len(repeat), full * len(repeat)}, linearSlack, func(size int) uint64 {
				text := open + strings.Repeat(repeat, size/len(repeat)) + closer
				return workOf(func() {
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
		full := parseFullTarget / unit
		small := full / growthStep
		name := "quoted typed blocks ending on a bare line"
		t.Run(name, func(t *testing.T) {
			growsLinearly(t, name, []int{small * unit, full * unit}, linearSlack, func(size int) uint64 {
				var text strings.Builder
				for index := 0; index < size/unit; index++ {
					fmt.Fprintf(&text, repeat, index)
				}
				body := text.String()
				return workOf(func() {
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
		full := parseFullTarget / pair
		small := full / growthStep
		name := fmt.Sprintf("%s%s…%s…%s", shape.before, shape.open, shape.close, shape.after)
		for _, reader := range []struct {
			name  string
			parse func(string) (*Node, error)
		}{{"ParseRendering", ParseRendering}, {"Parse", Parse}} {
			t.Run(reader.name+" "+name, func(t *testing.T) {
				growsLinearly(t, fmt.Sprintf("%s of %q", reader.name, name), []int{small * pair, full * pair}, linearSlack, func(size int) uint64 {
					repeats := size / pair
					text := shape.before + strings.Repeat(shape.open, repeats) + strings.Repeat(shape.close, repeats) + shape.after
					return workOf(func() {
						if _, err := reader.parse(text); err != nil && !refusedForItsMarks(err) {
							t.Fatalf("%s of %d bytes of %q: %v", reader.name, size, name, err)
						}
					})
				})
			})
		}
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		full := renderFullTarget / len(shape)
		small := full / growthStep
		doc := func(repeats int) *Node {
			return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, repeats)}}}}}
		}
		t.Run(shape, func(t *testing.T) {
			growsLinearly(t, fmt.Sprintf("rendering %q", shape), []int{small * len(shape), full * len(shape)}, linearSlack, func(size int) uint64 {
				tree := doc(size / len(shape))
				return workOf(func() {
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

// growthStep is the size ratio TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's two
// counts differ by (full is growthStep times small). growsLinearly's check is
// work > allowance*growth*before, so growth must exceed allowance for growth² (a quadratic
// regression's growth) to ever clear allowance*growth (linear's own expected growth) at all: at
// growthStep=8 and linearSlack=3 the bound is 3*8=24, true-linear clears it with 3x headroom, and
// a quadratic regression's 8²=64 clears it by 2.67x. A 2x step, which rounds 1-2 of LEGION-571
// used, could never discriminate at any slack - 2²=4 always clears 2*slack once slack>2 - which
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack's own synthetic check, below,
// catches as a negative control.
const growthStep = 8

// growthVerdict's check is work > allowance*growth*before: true-linear work grows about as fast
// as growth itself, quadratic work about growth² - so growth must exceed allowance for growth² to
// ever clear allowance*growth. This proves both directions with a synthetic count at the exact
// growth (growthStep) and slack (linearSlack)
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses: growthStep exceeds linearSlack,
// so the arithmetic actually discriminates. The quadratic direction goes through growthVerdict
// directly, not growsLinearly, since a real *testing.T that fails would fail this test too -
// growsLinearly is a thin wrapper over growthVerdict's verdict, so this is the same check
// growsLinearly makes. A count of nothing is refused, so a closure whose work no counter sees
// cannot pass as linear.
func TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack(t *testing.T) {
	const small, full = 1, growthStep // the growth step itself; growthVerdict never reads these as bytes
	linear := func(size int) uint64 { return uint64(size) * 1000 }
	quadratic := func(size int) uint64 { return uint64(size*size) * 1000 }
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, quadratic); failedStep != 0 {
		t.Errorf("growthVerdict did not fail a synthetic quadratic count at growth=%d, slack=%d (failedStep=%d), want step 0 to fail", growthStep, linearSlack, failedStep)
	}
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, linear); failedStep != -1 {
		t.Errorf("growthVerdict failed a synthetic linear count at growth=%d, slack=%d (step %d), want it to pass", growthStep, linearSlack, failedStep)
	}
	if failedStep, _ := growthVerdict([]int{small, full}, linearSlack, func(int) uint64 { return 0 }); failedStep != 0 {
		t.Errorf("growthVerdict passed a count of nothing (failedStep=%d), want step 0 to fail", failedStep)
	}
	// Negative control: round 1-2's 2x step, the same slack, the same quadratic function. This
	// must still pass growthVerdict (wrongly) - it is the historical defect
	// growthStep's doc comment names, kept here so a future reader can run it and see the defect
	// reproduce rather than take the claim on faith.
	if failedStep, _ := growthVerdict([]int{1, 2}, linearSlack, quadratic); failedStep != -1 {
		t.Errorf("growthVerdict unexpectedly failed the 2x-step negative control (failedStep=%d) - if this starts failing, the control no longer demonstrates round 1-2's defect and this test's comment needs updating, not alarm", failedStep)
	}
	// growsLinearly itself, through a real *testing.T, confirms the wrapper agrees with
	// growthVerdict on the passing direction (the failing direction cannot run through a real
	// t.Fatalf without failing this test, which the growthVerdict assertion above already proves).
	growsLinearly(t, "synthetic linear", []int{small, full}, linearSlack, linear)
}

// linearSlack is how many times faster than growth itself a linear operation's work may grow
// before growsLinearly calls it quadratic, at the growthStep growth
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText uses: true-linear work grows about
// growthStep times, comfortably inside the linearSlack*growthStep threshold; the quadratic cost
// LEGION-465 fixed would grow about growthStep² times, clearing that threshold with room to
// spare (growthStep's own doc comment has the exact numbers).
const linearSlack = 3

// The shapes a sweep found after the ones above, each quadratic before LEGION-465 finished: a
// text of one delimiter character, whose every character was escaped by a scan of its whole run;
// many `[^` before one `]`, whose every label was case-folded to look it up among the footnote
// labels; a text of many line feeds, whose every line joined the rest of the text to ask whether
// only whitespace followed; and a run of `~` in a paragraph, which goldmark's strikethrough parser
// scanned again from every `~` in it. Before the fix 128 KiB of each text took 18-115 s to render
// and 32 KiB of `[^a` 7.7 s.
//
// Each shape runs at two sizes through growsLinearly, counting the steps the render takes (workOf)
// rather than timing it. The sizes are eightfold apart, so a quadratic renderer grows 64x where a
// linear renderer may grow 24x (linearSlack*growthStep).
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
			growsLinearly(t, "rendering "+test.name, []int{renderFullTarget / growthStep, renderFullTarget}, linearSlack, func(size int) uint64 {
				document := test.doc(size)
				return workOf(func() {
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
			growsLinearly(t, "parsing "+test.name, []int{parseFullTarget / growthStep, parseFullTarget}, linearSlack, func(size int) uint64 {
				markdown := test.markdown(size)
				return workOf(func() {
					if _, err := test.parse(markdown); err != nil {
						t.Fatalf("parse %s at %s: %v", test.name, sizeText(size), err)
					}
				})
			})
		})
	}
}

// A table whose every row holds an escaped pipe in a code span parses in work linear in its size,
// up to the 1 MiB a document may be. goldmark's table transformer checked every code span's text
// against every escaped pipe in the document: on one machine at a load of about 300, 16 KiB of such
// rows parsed in 35 ms and 256 KiB in 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes,
// which replaced it, parsed 16 KiB in 19 ms, 256 KiB in 0.6-0.8 s and 1 MiB in 2.0-2.4 s there. No
// wall-clock bound holds on every runner, so the parse's steps (workOf) are counted at growing
// sizes and their growth bounded (growsLinearly), as TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText
// bounds a parse's. A caller's write of a table that size passes the element limit and is refused,
// so it is read as a rendering is, whose parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	growsLinearly(t, "parsing a table of `\\|` cells", []int{16 << 10, 256 << 10, 1 << 20}, growthAllowance, func(size int) uint64 {
		markdown := header + strings.Repeat(row, (size-len(header))/len(row))
		return workOf(func() {
			if _, err := ParseRendering(markdown); err != nil {
				t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
			}
		})
	})
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
	sizes := []int{16 << 10, 256 << 10, 1 << 20}
	for _, shape := range []string{
		"_www.a", "-www.x_", `(https://-a.b\`, ".www.a&", ".www", "www.", `www\.`, `www\.a `, "HTTP://a ", ` "'https://-a.b`, `(""www.a_`, ` ".www.a`,
		";https://-a.b", "x;www.a_", `\*https://-a`, "&amp;https://-a.", "éhttps://-a.", `http\://-a.`, `www\.a_`, `x"www.a_`, ";;", `\*`, ";a_a@", "é+a@b-", `www\.a@`,
	} {
		readsAndWritesLinearly(t, "", shape, "", sizes)
	}
	for _, shape := range []struct{ opener, closer string }{
		{"&lt;!-- a@b.cc ", "-->"}, {"&lt;?x;https://a.bc/ ", "?>"}, {"&lt;!X a@b.cc ", ">"}, {"&lt;![CDATA[ a@b.cc ", "]]>"},
		{`&lt;a b="x;a@b.cc `, `">`}, {`&lt;a b='&lt;a c="x;a@b.cc `, `'>`},
	} {
		readsAndWritesLinearly(t, "", shape.opener, shape.closer, sizes)
	}
	readsAndWritesLinearly(t, unreadableRun, "lorem ipsum ", "", []int{renderFullTarget / 16, renderFullTarget})
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
// Each shape is one paragraph, read as a rendering is and written from that tree, at the sizes
// TestBareURLShapesParseAndRenderLinearly reads its own shapes at. All but the CDATA sections are
// one text node, which no element limit stops; each `[` of those is a link opener of its own, so
// a caller's write of a mebibyte of them is refused for the elements they make, which the
// read-back counts none of.
func TestUnclosedRawHTMLShapesParseAndRenderLinearly(t *testing.T) {
	sizes := []int{16 << 10, 256 << 10, 1 << 20}
	for _, form := range []struct{ head, shape string }{
		{"p ", "<!a"}, {"p ", "<!A"}, {"", "x <!a\n"},
		{"p ", "<!--"}, {"p ", "<?"}, {"p ", "<![CDATA["},
	} {
		readsAndWritesLinearly(t, form.head, form.shape, "", sizes)
	}
}

// readsAndWritesLinearly requires a paragraph of head, then shape repeated to each of sizes and then
// tail to be read as a rendering is and written from that tree in time linear in its size
// (growsLinearly).
func readsAndWritesLinearly(t *testing.T, head, shape, tail string, sizes []int) {
	t.Helper()
	what := head + shape + tail
	growsLinearly(t, fmt.Sprintf("reading and writing %q", what), sizes, growthAllowance, func(size int) uint64 {
		markdown := head + strings.Repeat(shape, (size-len(head))/len(shape)) + tail + "._\n"
		return workOf(func() {
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

// Writing a tree of many blocks into a document takes work linear in its blocks when the blocks it
// adds or replaces stand together (LEGION-465). Update inserted the children a fragment gains one
// index at a time, and ygo's fragment insert walks the fragment's children to the index it is given
// - an element no document holds yet copies the children it has buffered instead - so n blocks
// reached about n²/2 items. On one machine at a load of 140 to 200, seeding 16,384 one-word
// paragraphs took 2.1 s of CPU and 65,536 took 36 s, a quote of 21,844 rules 4.3 s, and replacing
// a quote of 21,844 empty headings with a quote of as many rules 11 s through the upload route.
// Nothing in Update bounds the blocks: a rebuild from the latest version writes a stored rendering
// whole, whatever it weighs, so the seed of paragraphs runs to the mebibyte a document may hold
// (updateSeedTarget). Each write's work is counted (UpdateWork, workOf), the items its fragment
// operations reach, as TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText counts a parse's.
//
// Replaced blocks that stand apart are not linear. ygo's Delete and Insert walk the fragment from
// its start to the index each is given (crdt/yxml.go leftChildAt, deleteChildRange, in the pinned
// sjawhar/ygo v1.55.1-sami.2), and a replaced block between two kept ones is a run of its own, so a
// document whose every other block changes type costs a walk per replaced block, as it did before
// runs were batched. That shape is held to ygo's own walks instead: Update may reach the items a
// Delete and an Insert at each replaced index walk over and remove or add, and no more than
// alternatingAllowance items a block beside them. The write that typed its batch from the live
// fragment's children, which listed the whole fragment once more per replaced block, reached half
// a block's list again for every block beside the walks.
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
		full := test.target / len(test.to)
		small := full / growthStep
		t.Run(test.name, func(t *testing.T) {
			growsLinearly(t, test.name, []int{small * len(test.to), full * len(test.to)}, linearSlack, func(size int) uint64 {
				blocks := size / len(test.to)
				want := repeatedBlocks(t, test.to, blocks)
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				if test.from != "" {
					writeUpdate(t, doc, fragment, repeatedBlocks(t, test.from, blocks))
				}
				work := workOf(func() { writeUpdate(t, doc, fragment, want) })
				if got, err := Read(fragment); err != nil || !got.Equal(want) {
					t.Fatalf("%s: %d blocks read back otherwise (%v)", test.name, blocks, err)
				}
				return work
			})
		})
	}
	t.Run("replacing every other block", func(t *testing.T) {
		// What the write allocates at each size, beside the items it reaches: a list of the
		// fragment's children made again for each replaced block - ygo's Children copies them all -
		// grows with the square of the blocks there, where UpdateWork, which this package counts as
		// it lists, sees no list ygo makes on its own.
		allocated := make([]uint64, len(alternatingBlocks))
		for step, blocks := range alternatingBlocks {
			// Headings and paragraphs alternate, and every block becomes a paragraph: every heading
			// is a run of its own, between two paragraphs Update keeps.
			doc := crdt.New()
			fragment := doc.GetXmlFragment("prosemirror")
			writeUpdate(t, doc, fragment, repeatedBlocks(t, "#\n\na\n\n", blocks/2))
			want := repeatedBlocks(t, "a\n\n", blocks)
			before := UpdateWork()
			allocated[step] = allocatedBy(func() { writeUpdate(t, doc, fragment, want) })
			reached := UpdateWork() - before
			if got, err := Read(fragment); err != nil || !got.Equal(want) {
				t.Fatalf("replacing every other block: %d blocks read back otherwise (%v)", blocks, err)
			}
			// ygo's own Delete and Insert of one block at each replaced index walk over the items
			// before it and remove or add the one there.
			var walks uint64
			for index := 0; index < blocks; index += 2 {
				walks += 2 * uint64(index+1)
			}
			allowed := walks + alternatingAllowance*uint64(blocks)
			if reached > allowed {
				t.Fatalf("replacing every other block of %d reached %d items, past the %d allowed: the %d ygo's own Delete and Insert at those indexes reach and %d a block beside them", blocks, reached, allowed, walks, alternatingAllowance)
			}
			t.Logf("replacing every other block of %d: %d items reached, %d of them ygo's walks (at most %d), %d bytes allocated", blocks, reached, walks, allowed, allocated[step])
		}
		for step := 1; step < len(alternatingBlocks); step++ {
			growth := alternatingBlocks[step] / alternatingBlocks[step-1]
			if most := uint64(linearSlack*growth) * allocated[step-1]; allocated[step] > most {
				t.Fatalf("replacing every other block of %d allocated %d bytes, %.1f times the %d of %d blocks: the blocks grew %d times, and a write linear in them may allocate %d at most",
					alternatingBlocks[step], allocated[step], float64(allocated[step])/float64(allocated[step-1]), allocated[step-1], alternatingBlocks[step-1], growth, most)
			}
		}
	})
}

// alternatingAllowance is how many items a block Update may reach beside ygo's own walks to every
// other block (TestUpdateIsLinearInTheBlockCount): the list of the fragment's children once, and
// for each paragraph it keeps the list of that paragraph's children. A write that lists the
// fragment's children again for each replaced block reaches half the blocks again for every block.
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

// growthAllowance is how many times faster than its input a parse's work may grow from one size
// to the next. Linear work grows at most as fast as the input, less while a fixed cost counts, and
// quadratic work as the input's square, so four times the input's growth refuses a quadratic parse
// at any step over four.
const growthAllowance = 4

// workOf is the steps op takes, read from the package's counts of its own work before and after
// it - the parse's (ParseWork), the render's (RenderWork) and a write's into a document
// (UpdateWork) - and the bytes op allocates (allocatedBy). The counts see the scans and walks the
// package counts where it makes them; the bytes see what no count of the package's sits in: a
// string or a list built again for each piece of the input (a text joined from the rest of its
// lines at each line, a fragment's children listed by ygo again for each block), whose cost grows
// in the copies it allocates, whether the code making them is this package's, ygo's or
// goldmark's. Both are the process's, and no test in this package calls t.Parallel, so what grows
// between the two readings is op's.
func workOf(op func()) uint64 {
	before, allocated := allWork(), allocatedBy(op)
	return allWork() - before + allocated
}

// allWork is every step the package has counted.
func allWork() uint64 { return ParseWork() + RenderWork() + UpdateWork() }

// allocatedBy is the bytes the process allocated on the heap while op ran (runtime.MemStats's
// TotalAlloc, which grows by each allocation's size and which no collection or load of the machine
// moves).
func allocatedBy(op func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	op()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// growthVerdict is growsLinearly's verdict without a *testing.T to report through: which step
// (0-based, among sizes[1:]) first grew past its bound, or -1 if none did, and one line of context
// for every step up to and including a failure - the fail-style line at that step, a log-style
// line at every other. The counts are the work itself, so each size is counted once.
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack calls this directly, so the
// bound it proves is provable without a real *testing.T failing.
func growthVerdict(sizes []int, allowance int, counted func(size int) uint64) (failedStep int, lines []string) {
	before := counted(sizes[0])
	if before == 0 {
		return 0, []string{fmt.Sprintf("counted no work at %s, so its growth says nothing: none of the work it does is counted", sizeText(sizes[0]))}
	}
	failedStep = -1
	for i := 1; i < len(sizes); i++ {
		smaller, size := sizes[i-1], sizes[i]
		growth := size / smaller
		work := counted(size)
		allowed := uint64(allowance*growth) * before
		ratio := float64(work) / float64(before)
		if work > allowed {
			lines = append(lines, fmt.Sprintf("took %d steps at %s, %.1f times the %d it took at %s: the input grew %d times, and work linear in it may take %d steps at most",
				work, sizeText(size), ratio, before, sizeText(smaller), growth, allowed))
			return i - 1, lines
		}
		lines = append(lines, fmt.Sprintf("%d steps at %s, %d at %s, %.1f times for an input %d times larger (at most %d)", before, sizeText(smaller), work, sizeText(size), ratio, growth, allowed))
		before = work
	}
	return failedStep, lines
}

// growsLinearly counts what's work at each of sizes in turn and fails t at the first size whose
// work grew more than allowance times faster than the size did (growthVerdict, which this wraps).
// A count of none at the first size fails too, since a growth from nothing proves nothing.
func growsLinearly(t *testing.T, what string, sizes []int, allowance int, counted func(size int) uint64) {
	t.Helper()
	failedStep, lines := growthVerdict(sizes, allowance, counted)
	for step, line := range lines {
		if step == failedStep {
			t.Fatalf("%s %s", what, line)
			return
		}
		t.Logf("%s: %s", what, line)
	}
}

func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}
