package pmdoc

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
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
var alternatingBlocks = [2]int{4_096, 16_384}

// Parse and Render finish in work linear in the text on the shapes that cost quadratic time
// before LEGION-465. Each shape is counted at growthStep's fraction of its target size and then at
// the full size (parseFullTarget/renderFullTarget, above), through countedWorkIsLinear
// (counted_work_test.go).
//
// What each shape counts is the work its call does, counted by Go's coverage counters in this
// package and in every package it imports outside the standard library - goldmark's parser, whose
// delimiter and link walks were the quadratic ones, among them - in a child process that runs the
// call alone. A count moves only with the work, never with the machine: wall time, and CPU time
// after it, both grew on a loaded runner without the call doing more, which is LEGION-569/571's
// whole flake history across three rounds of widening the growth step and the slack, and the
// collections a timed window ran (INFRA-3149). The steps this package once counted of itself
// (ParseWork, RenderWork) saw only the loops that called them. A call that hangs is the test
// binary's deadline's to report, through the shared wait the child runs under.
//
// A write of a mebibyte of these is refused for the elements it makes (MaxDocumentElements), so
// the parse here is a read-back's, which counts none and reads all of it. Render's target is
// smaller than parse's (above). The margin each shape's ratio checks is between the work healthy
// linear code does and the far greater work a reintroduced O(n²) bug would do, in any loop of a
// counted package; the floor and the ceiling each shape's reading sets hold its work a character
// near what it was read at (delimiterAndOpenerHeavyReadings).
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	countedRowIsLinear(t, delimiterAndOpenerHeavyShapes())
}

// countedRowIsLinear runs each of shapes as a subtest of t, the row the counted child finds it by,
// through countedWorkIsLinear.
func countedRowIsLinear(t *testing.T, shapes []countedShape) {
	t.Helper()
	row := t.Name()
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			countedWorkIsLinear(t, row, shape)
		})
	}
}

// textShape is a shape of characters whose op reads or writes text, built by text at each size;
// its sizes are full/growthStep and full, each rounded down to whole units of unit characters.
func textShape(name string, full, unit int, readings map[string]countedReading, text func(size int) string,
	op func(t *testing.T, size int, text string)) countedShape {
	units := full / unit
	return countedShape{
		name:    name,
		sizes:   [2]int{units / growthStep * unit, units * unit},
		unit:    "character",
		reading: readings[name],
		stage:   searchedBytes,
		prepare: func(t *testing.T, size int) (int, func()) {
			input := text(size)
			return len(input), func() { op(t, size, input) }
		},
	}
}

// renderShape is a shape whose op renders the tree tree builds at each size, its units the
// characters of the tree's text; its sizes are full/growthStep and full, each rounded down to whole
// units of unit characters.
func renderShape(name string, full, unit int, readings map[string]countedReading, tree func(size int) *Node) countedShape {
	units := full / unit
	return countedShape{
		name:    name,
		sizes:   [2]int{units / growthStep * unit, units * unit},
		unit:    "character",
		reading: readings[name],
		stage:   searchedBytes,
		prepare: func(t *testing.T, size int) (int, func()) {
			doc := tree(size)
			return textLength(doc), func() {
				if _, err := Render(doc); err != nil {
					t.Fatalf("render %s at %s: %v", name, sizeText(size), err)
				}
			}
		},
	}
}

// textLength is how many characters the text nodes under node hold.
func textLength(node *Node) int {
	length := len(node.Text)
	for _, child := range node.Children {
		length += textLength(child)
	}
	return length
}

// delimiterAndOpenerHeavyShapes are TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's
// shapes.
func delimiterAndOpenerHeavyShapes() []countedShape {
	readings := delimiterAndOpenerHeavyReadings
	var shapes []countedShape
	for _, shape := range []string{"a_", "a_b*", "a~b_"} {
		shapes = append(shapes, textShape(shape, parseFullTarget, len(shape), readings,
			func(size int) string { return strings.Repeat(shape, size/len(shape)) },
			func(t *testing.T, size int, text string) {
				if _, err := ParseRendering(text); err != nil {
					t.Fatalf("parse %d bytes of %q: %v", size, shape, err)
				}
			}))
	}
	// A block shape: a list item whose typed block holds one list item per blank line. Reading a
	// blank line inside such a block as the item's walked the whole block once for each of them,
	// and the list again for each node of it, which is quadratic in a block of blank lines; the
	// reading is taken once over the document now (readSpreads). A write is
	// what reads it, so this one is counted through ParseForWrite. The unit is one item and 200
	// blank lines, so the items and the lines grow together and a mebibyte of them stays under
	// MaxDocumentElements: the write is accepted at every size here, and any refusal - of the
	// elements above all, which would stop the read before the cost - fails the case rather than
	// passing it.
	{
		open, closer := "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n", "  :::\n"
		repeat := "  - p\n" + strings.Repeat("\n", 200)
		name := "a typed block of spaced items in a list item"
		shapes = append(shapes, textShape(name, parseFullTarget, len(repeat), readings,
			func(size int) string { return open + strings.Repeat(repeat, size/len(repeat)) + closer },
			func(t *testing.T, size int, text string) {
				if _, err := ParseForWrite(text, nil); err != nil {
					t.Fatalf("%s at %d bytes: %v", name, size, err)
				}
			}))
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
		shapes = append(shapes, textShape(name, parseFullTarget, unit, readings,
			func(size int) string {
				var text strings.Builder
				for index := range size / unit {
					fmt.Fprintf(&text, repeat, index)
				}
				return text.String()
			},
			func(t *testing.T, size int, text string) {
				if _, err := ParseForWrite(text, nil); err != nil {
					t.Fatalf("%s at %d bytes: %v", name, size, err)
				}
			}))
	}
	// Two-part shapes: an opener half repeated, then a closer half repeated, so each closer looks
	// back past every run before it. goldmark's ProcessDelimiters walks a closer back over every
	// earlier run of either character, so `~~a ` then `a* ` and `*a ` then `a~ ` took 3.8-4.2 s at
	// 112 KB, and pairing `~` runs by length makes `~~a ` then `a~ ` and `~a ` then `a~~ ` the same
	// walk (dispatch://INFRA-534). The rule of three keeps every `*` of `a*a`, which can both open
	// and close, from every `**` before it, so a walk passed each of them: `**a ` then `a*a` took
	// 100 s at 437 KiB. Each is read as a rendering is and as a caller's write is; a write of a
	// mebibyte may be refused for the elements it makes, and a shape nesting past maxInlineNesting
	// for that, each counted all the same.
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
			shapes = append(shapes, textShape(reader.name+" "+name, parseFullTarget, pair, readings,
				func(size int) string {
					repeats := size / pair
					return shape.before + strings.Repeat(shape.open, repeats) + strings.Repeat(shape.close, repeats) + shape.after
				},
				func(t *testing.T, size int, text string) {
					if _, err := reader.parse(text); err != nil && !refusedForItsMarks(err) {
						t.Fatalf("%s of %d bytes of %q: %v", reader.name, size, name, err)
					}
				}))
		}
	}
	for _, shape := range []string{"[a", "a_b&", "<a", "[^a"} {
		shapes = append(shapes, renderShape(shape, renderFullTarget, len(shape), readings, func(size int) *Node {
			return &Node{Type: "doc", Children: []*Node{{Type: "paragraph", Children: []*Node{{Type: "text", Text: strings.Repeat(shape, size/len(shape))}}}}}
		}))
	}
	return shapes
}

// delimiterAndOpenerHeavyReadings are the readings (countedReading) of
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText's shapes, by name, taken on 2026-11-20
// in INFRA-3149's pod (Go 1.27.3) at the row's two sizes, counting every package pmdoc imports
// outside the standard library: past is the statements each character past the smaller size cost,
// most the statements a character cost at whichever size cost more; each shape's floor is a
// quarter of past and its ceiling four times most (countedMargin). A caller's write (Parse) of the
// two-part shapes is refused for the elements or the nesting its marks make, past which it reads
// little more: its work grows 1.06 to 1.76 times over eight times the characters, and its past is
// a tenth of its most or less.
var delimiterAndOpenerHeavyReadings = map[string]countedReading{
	"a_":   {past: 250.5, most: 250.5},
	"a_b*": {past: 306.0, most: 306.0},
	"a~b_": {past: 300.9, most: 300.9},
	"a typed block of spaced items in a list item": {past: 606.7, most: 606.7},
	"quoted typed blocks ending on a bare line":    {past: 108.1, most: 108.5},
	"ParseRendering ~~a …a~ …":                     {past: 187.4, most: 187.5},
	"Parse ~~a …a~ …":                              {past: 22.9, most: 213.0},
	"ParseRendering ~a …a~~ …":                     {past: 187.4, most: 187.5},
	"Parse ~a …a~~ …":                              {past: 23.1, most: 213.0},
	"ParseRendering ~~a …a* …":                     {past: 188.4, most: 188.5},
	"Parse ~~a …a* …":                              {past: 22.7, most: 214.0},
	"ParseRendering *a …a~ …":                      {past: 219.3, most: 219.4},
	"Parse *a …a~ …":                               {past: 18.6, most: 249.0},
	"ParseRendering ~a [~~a …a~ …](u)":             {past: 176.7, most: 176.7},
	"Parse ~a [~~a …a~ …](u)":                      {past: 16.5, most: 202.3},
	"ParseRendering *~a…~*…":                       {past: 319.6, most: 319.7},
	"Parse *~a…~*…":                                {past: 2.8, most: 320.8},
	"ParseRendering **a …a*a…":                     {past: 210.7, most: 210.7},
	"Parse **a …a*a…":                              {past: 20.0, most: 238.2},
	"ParseRendering a**a*a …a*a**a…":               {past: 231.1, most: 231.1},
	"Parse a**a*a …a*a**a…":                        {past: 16.6, most: 260.0},
	"[a":                                           {past: 30.5, most: 30.5},
	"a_b&":                                         {past: 32.2, most: 32.3},
	"<a":                                           {past: 495.5, most: 495.6},
	"[^a":                                          {past: 28.7, most: 28.7},
}

// refusedForItsMarks reports whether err refuses markdown for the inline marks it makes: more
// elements than a write may make, or marks nested past maxInlineNesting.
func refusedForItsMarks(err error) bool {
	nesting := nestingError{}
	return errors.Is(err, ErrTooManyElements) || errors.As(err, &nesting) && nesting.inline
}

// growthStep is the size ratio a counted shape's two sizes differ by (the larger is growthStep
// times the smaller). countedVerdict's ratio check is work > linearSlack*growth*smaller, so growth
// must exceed linearSlack for growth² (a quadratic regression's growth) to ever clear
// linearSlack*growth (linear's own expected growth) at all: at growthStep=8 and linearSlack=3 the
// bound is 3*8=24, true-linear clears it with 3x headroom, and a quadratic regression's 8²=64
// clears it by 2.67x. A 2x step, which rounds 1-2 of LEGION-571 used, could never discriminate at
// any slack - 2²=4 always clears 2*slack once slack>2 - which
// TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack's own synthetic check, below,
// catches as a negative control.
const growthStep = 8

// countedVerdict's check is work > linearSlack*growth*smaller: true-linear work grows about as
// fast as growth itself, quadratic work about growth² - so growth must exceed linearSlack for
// growth² to ever clear linearSlack*growth. This proves both directions with synthetic counts at
// the exact growth (growthStep) and slack (linearSlack) every counted row uses, and the floor, the
// ceiling, the bytes and the two rules on a counter's wrap beside them. The verdict is
// countedVerdict's, without a *testing.T, so a failing direction is provable without failing this
// test.
func TestGrowsLinearlyCatchesQuadraticAtThisFilesGrowthStepAndSlack(t *testing.T) {
	t.Parallel()
	const small, full = 1000, growthStep * 1000
	// A linear op counts 1,000 statements a character and allocates as many bytes; its reading is
	// that.
	shape := countedShape{name: "synthetic", sizes: [2]int{small, full}, unit: "character",
		reading: countedReading{past: 1000, most: 1000}}
	run := func(units int, work int64, bytes uint64) counted {
		return counted{units: units, work: work, byModule: map[string]int64{"synthetic": work}, bytes: bytes}
	}
	linear := func(units int) counted { return run(units, int64(units)*1000, uint64(units)*1000) }
	quadratic := func(units int) counted { return run(units, int64(units)*int64(units), uint64(units)*1000) }
	failsWith := func(what string, shape countedShape, runs [2]counted, want string) {
		t.Helper()
		failures, readings := countedVerdict(shape, runs)
		for _, failure := range failures {
			if strings.Contains(failure, want) {
				return
			}
		}
		t.Errorf("countedVerdict passed %s, want a failure saying %q (failures %q; %s)", what, want, failures, readings)
	}
	if failures, readings := countedVerdict(shape, [2]counted{linear(small), linear(full)}); len(failures) != 0 {
		t.Errorf("countedVerdict failed a synthetic linear count at growth=%d, slack=%d: %q (%s)", growthStep, linearSlack, failures, readings)
	}
	// A quadratic count whose smaller run reads as the linear one does: the ratio fails it, and its
	// larger run's work a character fails the ceiling.
	failsWith("a synthetic quadratic count", shape, [2]counted{quadratic(small), quadratic(full)}, "it is not linear")
	failsWith("a synthetic quadratic count", shape, [2]counted{quadratic(small), quadratic(full)}, "statements a character, more than")
	// A count of nothing, and a count that sees 17 statements of a run of brackets whatever its
	// length (ParseWork over the run of 15,000 brackets, dispatch://INFRA-3149), fail the floor.
	failsWith("a count of nothing", shape, [2]counted{run(small, 0, 1000), run(full, 0, 8000)}, "it is not linear")
	failsWith("a count that does not grow", shape, [2]counted{run(small, 17, 1000), run(full, 17, 8000)}, "the counters did not see")
	// Quadratic bytes beside linear work fail on the bytes, and an op that allocates nothing fails.
	failsWith("quadratic bytes", shape, [2]counted{run(small, small*1000, small*small), run(full, full*1000, full*full)}, "bytes allocated grow")
	failsWith("no bytes", shape, [2]counted{run(small, small*1000, 0), run(full, full*1000, 0)}, "bytes allocated grow")
	// A shape with no reading recorded fails, naming the reading to record.
	unread := shape
	unread.reading = countedReading{}
	failsWith("a shape with no reading", unread, [2]counted{linear(small), linear(full)}, "no reading is recorded")
	// A block counted 2^31 times fails its run, naming the block, though the work summed reads
	// linear: the next size can wrap its counter. A larger size whose work is below the smaller's
	// fails as the wrap it is.
	busy := linear(small)
	busy.busiest, busy.busiestBlock = countedBlockLimit, "example.com/synthetic/scan.go:10.2,12.3"
	failsWith("a block counted 2^31 times", shape, [2]counted{busy, linear(full)}, "the block example.com/synthetic/scan.go:10.2,12.3 ran 2147483648 times")
	failsWith("work that shrinks", shape, [2]counted{run(small, small*1000, 1000), run(full, small*1000-1, 8000)}, "a coverage counter that wrapped")
	// Negative control: round 1-2's 2x step, the same slack, a quadratic count whose work a
	// character the ceiling allows at both sizes. This must still pass the ratio (wrongly) - it is
	// the historical defect growthStep's doc comment names, kept here so a future reader can run it
	// and see the defect reproduce rather than take the claim on faith.
	half := shape
	half.sizes = [2]int{small, 2 * small}
	if failures, readings := countedVerdict(half, [2]counted{quadratic(small), quadratic(2 * small)}); len(failures) != 0 {
		t.Errorf("countedVerdict unexpectedly failed the 2x-step negative control (%q; %s) - if this starts failing, the control no "+
			"longer demonstrates round 1-2's defect and this test's comment needs updating, not alarm", failures, readings)
	}
}

// linearSlack is how many times faster than its units a linear operation's work may grow before
// countedVerdict calls it quadratic, at the growthStep growth every counted row uses: true-linear
// work grows about growthStep times, comfortably inside the linearSlack*growthStep threshold; the
// quadratic cost LEGION-465 fixed would grow about growthStep² times, clearing that threshold with
// room to spare (growthStep's own doc comment has the exact numbers). A linear op whose work has a
// fixed term below zero grows faster than its units, and less than linearSlack times: goldmark's
// link parser copies the label of only the last 998 closers of a run of brackets, so the run's
// work is linear past 2*998 characters less a fixed 998², and grows 9.71 times over 8 times the
// characters at the sizes heavyBodyParseReadings records.
const linearSlack = 3

// The shapes a sweep found after the ones above, each quadratic before LEGION-465 finished: a
// text of one delimiter character, whose every character was escaped by a scan of its whole run;
// many `[^` before one `]`, whose every label was case-folded to look it up among the footnote
// labels; a text of many line feeds, whose every line joined the rest of the text to ask whether
// only whitespace followed; and a run of `~` in a paragraph, which goldmark's strikethrough parser
// scanned again from every `~` in it. Before the fix 128 KiB of each text took 18-115 s to render
// and 32 KiB of `[^a` 7.7 s.
//
// Each shape is counted at two sizes eightfold apart (countedWorkIsLinear), so a quadratic renderer
// grows 64x where a linear renderer may grow 24x (linearSlack*growthStep).
func TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds(t *testing.T) {
	countedRowIsLinear(t, runFootnoteAndLineFeedShapes())
}

// runFootnoteAndLineFeedShapes are TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds's shapes.
func runFootnoteAndLineFeedShapes() []countedShape {
	readings := runFootnoteAndLineFeedReadings
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
	var shapes []countedShape
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
		shapes = append(shapes, renderShape("rendering "+test.name, renderFullTarget, 1, readings, test.doc))
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
		shapes = append(shapes, textShape("parsing "+test.name, parseFullTarget, 4, readings, test.markdown,
			func(t *testing.T, size int, markdown string) {
				if _, err := test.parse(markdown); err != nil {
					t.Fatalf("parse %s at %s: %v", test.name, sizeText(size), err)
				}
			}))
	}
	return shapes
}

// runFootnoteAndLineFeedReadings are the readings (countedReading) of
// TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds's shapes, by name, taken as
// delimiterAndOpenerHeavyReadings were.
var runFootnoteAndLineFeedReadings = map[string]countedReading{
	"rendering a run of *":                                  {past: 58.0, most: 58.1},
	"rendering a run of _":                                  {past: 46.0, most: 46.1},
	"rendering a run of ~":                                  {past: 634.0, most: 634.1},
	"rendering [^a before one ] with no footnote":           {past: 29.0, most: 29.0},
	"rendering [^a before one ] with a short label defined": {past: 32.3, most: 32.4},
	"rendering [^a before one ] with a long label defined":  {past: 32.3, most: 33.4},
	"rendering line feeds":                                  {past: 143.0, most: 143.0},
	"parsing a run of ~":                                    {past: 223.0, most: 223.0},
	"parsing \\~ and a run of ~":                            {past: 226.0, most: 226.0},
	"parsing a\\~~ repeated":                                {past: 185.5, most: 185.5},
}

// A table whose every row holds an escaped pipe in a code span parses in work linear in its size,
// up to the 1 MiB a document may be. goldmark's table transformer checked every code span's text
// against every escaped pipe in the document: on one machine at a load of about 300, 16 KiB of such
// rows parsed in 35 ms and 256 KiB in 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes,
// which replaced it, parsed 16 KiB in 19 ms, 256 KiB in 0.6-0.8 s and 1 MiB in 2.0-2.4 s there. No
// wall-clock bound holds on every runner, so the parse's work is counted at an eighth of a
// mebibyte and at the whole of it (countedWorkIsLinear), as
// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText counts a parse's. A caller's write of a
// table that size passes the element limit and is refused, so it is read as a rendering is, whose
// parse no element limit stops.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	countedRowIsLinear(t, escapedPipeTableShapes())
}

// escapedPipeTableShapes is TestParseIsLinearInATableOfEscapedPipes's one shape.
func escapedPipeTableShapes() []countedShape {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	return []countedShape{textShape("a table of `\\|` cells", parseFullTarget, 1, escapedPipeTableReadings,
		func(size int) string { return header + strings.Repeat(row, (size-len(header))/len(row)) },
		func(t *testing.T, size int, markdown string) {
			if _, err := ParseRendering(markdown); err != nil {
				t.Fatalf("parse %d bytes of `\\|` rows: %v", size, err)
			}
		})}
}

// escapedPipeTableReadings are the readings (countedReading) of
// TestParseIsLinearInATableOfEscapedPipes's shape, by name.
var escapedPipeTableReadings = map[string]countedReading{
	"a table of `\\|` cells": {past: 395.4, most: 395.4},
}

// The row above counts unescapeTablePipes, pmdoc's own walk of a table's cells, and goldmark's
// parse around it. goldmark's table AST transformer, which did that work in time quadratic in a
// table of escaped pipes, is goldmark's too, so registered again it would grow that row's count
// with the square of the table; what keeps it out is that no reader registers it, which this row
// holds of every reader directly. Every goldmark parser pmdoc reads markdown with - the document
// reader, the display reader and the inline readers a parse and a render's read-backs use - holds
// no AST transformer of goldmark's table extension, read off the parser after a parse, a display
// read and a render have each built theirs, and off its configuration where a parser has not been
// built yet.
func TestNoReaderRegistersGoldmarksTableASTTransformer(t *testing.T) {
	const table = "| a | b |\n| --- | --- |\n| `\\|` | x |\n"
	tree, err := ParseRendering(table)
	if err != nil {
		t.Fatalf("parse the table: %v", err)
	}
	if _, err := Render(tree); err != nil {
		t.Fatalf("render the table: %v", err)
	}
	_ = ReadForDisplay(table, DisplayLatest)
	if _, err := ParseInline("`\\|` a", NewWriteBudget()); err != nil {
		t.Fatalf("parse the inline markdown: %v", err)
	}
	goldmarks := reflect.TypeOf(extension.NewTableASTTransformer())
	for _, reader := range []struct {
		name   string
		parser parser.Parser
	}{
		{"the document reader (blockReader)", blockReader.md.Parser()},
		{"the display reader (displayReader)", displayReader.md.Parser()},
		{"inlineMarkdown", inlineMarkdown.run},
		{"inlineMarkdown with definitions", inlineMarkdown.withDefinitions},
		{"storedMarkdown", storedMarkdown.run},
		{"storedMarkdown with definitions", storedMarkdown.withDefinitions},
		{"flankingMarkdown", flankingMarkdown.run},
		{"flankingMarkdown with definitions", flankingMarkdown.withDefinitions},
	} {
		for _, registered := range astTransformerTypes(t, reader.parser) {
			if registered == goldmarks {
				t.Errorf("%s registers goldmark's table AST transformer (%s), whose work in a table of escaped pipes "+
					"is quadratic: unescapeTablePipes does that work", reader.name, registered)
			}
		}
	}
}

// astTransformerTypes is the type of every AST transformer goldmark's parser p runs after a parse:
// those it built at its first parse (its astTransformers) and, for a parser not built yet, those its
// configuration still holds (config.ASTTransformers). goldmark answers neither, so they are read
// off its unexported fields; a goldmark whose parser holds neither fails here, naming the fields,
// rather than passing on a list it could not read.
func astTransformerTypes(t *testing.T, p parser.Parser) []reflect.Type {
	t.Helper()
	value := reflect.ValueOf(p)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Struct {
		t.Fatalf("goldmark's parser is a %s, not a pointer to its struct: read where it keeps its AST transformers again", value.Type())
	}
	built, config := value.Elem().FieldByName("astTransformers"), value.Elem().FieldByName("config")
	if !built.IsValid() || !config.IsValid() {
		t.Fatalf("goldmark's parser %s holds no astTransformers or config field: read where it keeps its AST transformers again", value.Type())
	}
	var types []reflect.Type
	for index := range built.Len() {
		types = append(types, built.Index(index).Elem().Type())
	}
	if !config.IsNil() {
		configured := config.Elem().FieldByName("ASTTransformers")
		for index := range configured.Len() {
			types = append(types, configured.Index(index).FieldByName("Value").Elem().Type())
		}
	}
	return types
}

// A bare URL or email is read and written in work linear in its line. A line can hold an address
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
// is counted up to renderFullTarget, as TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText
// counts a render.
func TestBareURLShapesParseAndRenderLinearly(t *testing.T) {
	countedRowIsLinear(t, bareURLShapes())
}

// bareURLShapes are TestBareURLShapesParseAndRenderLinearly's shapes.
func bareURLShapes() []countedShape {
	var shapes []countedShape
	for _, shape := range []string{
		"_www.a", "-www.x_", `(https://-a.b\`, ".www.a&", ".www", "www.", `www\.`, `www\.a `, "HTTP://a ", ` "'https://-a.b`, `(""www.a_`, ` ".www.a`,
		";https://-a.b", "x;www.a_", `\*https://-a`, "&amp;https://-a.", "éhttps://-a.", `http\://-a.`, `www\.a_`, `x"www.a_`, ";;", `\*`, ";a_a@", "é+a@b-", `www\.a@`,
	} {
		shapes = append(shapes, readAndWriteShape("", shape, "", parseFullTarget, bareURLReadings))
	}
	for _, shape := range []struct{ opener, closer string }{
		{"&lt;!-- a@b.cc ", "-->"}, {"&lt;?x;https://a.bc/ ", "?>"}, {"&lt;!X a@b.cc ", ">"}, {"&lt;![CDATA[ a@b.cc ", "]]>"},
		{`&lt;a b="x;a@b.cc `, `">`}, {`&lt;a b='&lt;a c="x;a@b.cc `, `'>`},
	} {
		shapes = append(shapes, readAndWriteShape("", shape.opener, shape.closer, parseFullTarget, bareURLReadings))
	}
	return append(shapes, readAndWriteShape(unreadableRun, "lorem ipsum ", "", renderFullTarget, bareURLReadings))
}

// bareURLReadings are the readings (countedReading) of TestBareURLShapesParseAndRenderLinearly's
// shapes, by name, taken as delimiterAndOpenerHeavyReadings were.
var bareURLReadings = map[string]countedReading{
	"_www.a":                         {past: 818.5, most: 818.6},
	"-www.x_":                        {past: 507.3, most: 507.4},
	"(https://-a.b\\":                {past: 190.7, most: 190.8},
	".www.a&":                        {past: 188.7, most: 188.8},
	".www":                           {past: 203.2, most: 203.3},
	"www.":                           {past: 105.0, most: 105.1},
	"www\\.":                         {past: 202.0, most: 202.1},
	"www\\.a ":                       {past: 393.1, most: 393.2},
	"HTTP://a ":                      {past: 230.6, most: 230.6},
	" \"'https://-a.b":               {past: 337.6, most: 337.6},
	"(\"\"www.a_":                    {past: 307.1, most: 307.2},
	" \".www.a":                      {past: 457.0, most: 457.0},
	";https://-a.b":                  {past: 198.8, most: 198.8},
	"x;www.a_":                       {past: 312.9, most: 312.9},
	"\\*https://-a":                  {past: 226.3, most: 226.4},
	"&amp;https://-a.":               {past: 181.8, most: 181.9},
	"éhttps://-a.":                   {past: 196.8, most: 196.8},
	"http\\://-a.":                   {past: 189.5, most: 189.5},
	"www\\.a_":                       {past: 464.1, most: 464.2},
	"x\"www.a_":                      {past: 312.9, most: 312.9},
	";;":                             {past: 168.0, most: 168.0},
	"\\*":                            {past: 108.5, most: 108.6},
	";a_a@":                          {past: 398.4, most: 398.4},
	"é+a@b-":                         {past: 217.3, most: 217.3},
	"www\\.a@":                       {past: 91.6, most: 91.6},
	"&lt;!-- a@b.cc -->":             {past: 415.1, most: 415.1},
	"&lt;?x;https://a.bc/ ?>":        {past: 227.8, most: 227.9},
	"&lt;!X a@b.cc >":                {past: 401.9, most: 401.9},
	"&lt;![CDATA[ a@b.cc ]]>":        {past: 642.9, most: 642.9},
	"&lt;a b=\"x;a@b.cc \">":         {past: 1147.9, most: 1148.0},
	"&lt;a b='&lt;a c=\"x;a@b.cc '>": {past: 1175.9, most: 1175.8},
	unreadableRun + "lorem ipsum ":   {past: 13822.0, most: 13871.0},
}

// A paragraph of declarations no `>` closes is read and written in work linear in its size. A
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
// and 0.11 s. goldmark's walk is counted as this package's is, since the counters count goldmark.
//
// Each shape is one paragraph, read as a rendering is and written from that tree, at the sizes
// TestBareURLShapesParseAndRenderLinearly reads its own shapes at. All but the CDATA sections are
// one text node, which no element limit stops; each `[` of those is a link opener of its own, so
// a caller's write of a mebibyte of them is refused for the elements they make, which the
// read-back counts none of.
func TestUnclosedRawHTMLShapesParseAndRenderLinearly(t *testing.T) {
	countedRowIsLinear(t, unclosedRawHTMLShapes())
}

// unclosedRawHTMLShapes are TestUnclosedRawHTMLShapesParseAndRenderLinearly's shapes.
func unclosedRawHTMLShapes() []countedShape {
	var shapes []countedShape
	for _, form := range []struct{ head, shape string }{
		{"p ", "<!a"}, {"p ", "<!A"}, {"", "x <!a\n"},
		{"p ", "<!--"}, {"p ", "<?"}, {"p ", "<![CDATA["},
	} {
		shapes = append(shapes, readAndWriteShape(form.head, form.shape, "", parseFullTarget, unclosedRawHTMLReadings))
	}
	return shapes
}

// unclosedRawHTMLReadings are the readings (countedReading) of
// TestUnclosedRawHTMLShapesParseAndRenderLinearly's shapes, by name, taken as
// delimiterAndOpenerHeavyReadings were.
var unclosedRawHTMLReadings = map[string]countedReading{
	"p <!a":       {past: 217.7, most: 217.7},
	"p <!A":       {past: 217.7, most: 217.7},
	"x <!a\n":     {past: 298.7, most: 298.7},
	"p <!--":      {past: 237.5, most: 237.5},
	"p <?":        {past: 242.0, most: 242.0},
	"p <![CDATA[": {past: 189.4, most: 189.5},
}

// readAndWriteShape is a paragraph of head, then shape repeated to each of full/growthStep and
// full and then tail, read as a rendering is and written from that tree.
func readAndWriteShape(head, shape, tail string, full int, readings map[string]countedReading) countedShape {
	what := head + shape + tail
	return countedShape{
		name:    what,
		sizes:   [2]int{full / growthStep, full},
		unit:    "character",
		reading: readings[what],
		stage:   searchedBytes,
		prepare: func(t *testing.T, size int) (int, func()) {
			markdown := head + strings.Repeat(shape, (size-len(head))/len(shape)) + tail + "._\n"
			return len(markdown), func() {
				doc, err := ParseRendering(markdown)
				if err != nil {
					t.Fatalf("parse %d bytes of %q: %v", size, what, err)
				}
				if _, err := Render(doc); err != nil {
					t.Fatalf("render %d bytes of %q: %v", size, what, err)
				}
			}
		},
	}
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
// (updateSeedTarget). Each write's work is counted by Go's coverage counters in pmdoc and in ygo,
// whose walks were the quadratic ones (countedWorkIsLinear), with its read-back beside it: the
// document a write replaces is written before the count starts.
//
// Replaced blocks that stand apart are not linear. ygo's Delete and Insert walk the fragment from
// its start to the index each is given (crdt/yxml.go leftChildAt, deleteChildRange, in the pinned
// sjawhar/ygo v1.55.1-sami.2), and a replaced block between two kept ones is a run of its own, so a
// document whose every other block changes type costs a walk per replaced block, as it did before
// runs were batched. That shape is held to ygo's own walks instead (alternatingWorkIsBounded).
func TestUpdateIsLinearInTheBlockCount(t *testing.T) {
	countedRowIsLinear(t, updateShapes())
	row := t.Name()
	t.Run(alternatingShape().name, func(t *testing.T) { alternatingWorkIsBounded(t, row) })
}

// updateShapes are TestUpdateIsLinearInTheBlockCount's shapes linear in their blocks; its row in
// countedRows holds alternatingShape too.
func updateShapes() []countedShape {
	var shapes []countedShape
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
		shapes = append(shapes, countedShape{
			name:    test.name,
			sizes:   [2]int{full / growthStep, full},
			unit:    "block",
			reading: updateReadings[test.name],
			prepare: func(t *testing.T, blocks int) (int, func()) {
				want := repeatedBlocks(t, test.to, blocks)
				doc := crdt.New()
				fragment := doc.GetXmlFragment("prosemirror")
				if test.from != "" {
					writeUpdate(t, doc, fragment, repeatedBlocks(t, test.from, blocks))
				}
				return blocks, func() {
					writeUpdate(t, doc, fragment, want)
					if got, err := Read(fragment); err != nil || !got.Equal(want) {
						t.Fatalf("%s: %d blocks read back otherwise (%v)", test.name, blocks, err)
					}
				}
			},
		})
	}
	return shapes
}

// updateReadings are the readings (countedReading) of TestUpdateIsLinearInTheBlockCount's shapes
// linear in their blocks, by name, taken as delimiterAndOpenerHeavyReadings were: past and most
// are statements a block, the write and its read-back, ygo's work among them.
var updateReadings = map[string]countedReading{
	"seeding paragraphs":                                  {past: 885.0, most: 885.0},
	"seeding a quote of rules":                            {past: 401.0, most: 401.2},
	"replacing headings with paragraphs":                  {past: 1319.7, most: 1317.6},
	"replacing a quote of headings with a quote of rules": {past: 859.0, most: 856.9},
}

// alternatingShape is a document whose headings and paragraphs alternate, every block of which
// becomes a paragraph: every heading is a run of its own, between two paragraphs Update keeps. Its
// sizes are alternatingBlocks, and its stage is what Update counts of the items its writes reach
// (UpdateWork).
func alternatingShape() countedShape {
	return countedShape{
		name:  "replacing every other block",
		sizes: alternatingBlocks,
		unit:  "block",
		stage: UpdateWork,
		prepare: func(t *testing.T, blocks int) (int, func()) {
			doc := crdt.New()
			fragment := doc.GetXmlFragment("prosemirror")
			writeUpdate(t, doc, fragment, repeatedBlocks(t, "#\n\na\n\n", blocks/2))
			want := repeatedBlocks(t, "a\n\n", blocks)
			return blocks, func() {
				writeUpdate(t, doc, fragment, want)
				if got, err := Read(fragment); err != nil || !got.Equal(want) {
					t.Fatalf("replacing every other block: %d blocks read back otherwise (%v)", blocks, err)
				}
			}
		},
	}
}

// ygoWalks is the items ygo's own Delete and Insert of one block at each replaced index of an
// alternating document of blocks walk over, before it, and remove or add, the one there.
func ygoWalks(blocks int) uint64 {
	var walks uint64
	for index := 0; index < blocks; index += 2 {
		walks += 2 * uint64(index+1)
	}
	return walks
}

// alternatingWorkIsBounded counts alternatingShape at each of its sizes and holds it to ygo's own
// walks. Update may reach (UpdateWork) the items a Delete and an Insert at each replaced index walk
// over and remove or add, and no more than alternatingAllowance items a block beside them: the
// write that typed its batch from the live fragment's children, which listed the whole fragment
// once more per replaced block, reached half a block's list again for every block beside the
// walks. That count is the named stage the counters cannot tell apart from ygo's walks, so it is
// judged beside the counted work, which must stay within alternatingCeiling statements of every
// item those walks and alternatingAllowance a block allow, and must see at least alternatingFloor
// of them. The bytes the write allocates grow no faster than linearSlack times the blocks: a list
// of the fragment's children made again for each replaced block - ygo's Children copies them all -
// grows with the square of the blocks there. The two rules that keep a coverage counter from
// wrapping hold here as in countedVerdict: no block counted countedBlockLimit times or more at
// either size, and no less work at the larger size than at the smaller.
func alternatingWorkIsBounded(t *testing.T, row string) {
	shape := alternatingShape()
	var runs [2]counted
	for step, blocks := range shape.sizes {
		runs[step] = countedRun(t, row, shape, blocks)
		walks := ygoWalks(blocks)
		allowed := walks + alternatingAllowance*uint64(blocks)
		perAllowed := float64(runs[step].work) / float64(allowed)
		reading := fmt.Sprintf("replacing every other block of %d: %d items reached, %d of them ygo's walks (at most %d); work %d (%s), "+
			"%.2f a reached item allowed (at least %.2f, at most %.2f); %d bytes allocated; busiest block %d", blocks, runs[step].stage, walks, allowed,
			runs[step].work, byModuleText(runs[step].byModule), perAllowed, alternatingFloor, alternatingCeiling, runs[step].bytes, runs[step].busiest)
		if runs[step].stage > allowed {
			t.Errorf("replacing every other block of %d reached %d items, past the %d allowed: the %d ygo's own Delete and Insert at "+
				"those indexes reach and %d a block beside them (%s)", blocks, runs[step].stage, allowed, walks, alternatingAllowance, reading)
		}
		if perAllowed < alternatingFloor || perAllowed > alternatingCeiling {
			t.Errorf("replacing every other block of %d counted %.2f statements a reached item allowed, outside %.2f to %.2f (%s)",
				blocks, perAllowed, alternatingFloor, alternatingCeiling, reading)
		}
		if runs[step].busiest >= countedBlockLimit {
			t.Errorf("replacing every other block of %d: the block %s ran %d times, at or past %d: its counter is 32 bits wide, so "+
				"the next size can wrap it, and the work summed from it is no longer a count (%s)", blocks, runs[step].busiestBlock,
				runs[step].busiest, int64(countedBlockLimit), reading)
		}
		t.Log(reading)
	}
	if runs[1].work < runs[0].work {
		t.Errorf("replacing every other block: the work falls from %d at %d blocks to %d at %d: work that shrinks as the input grows "+
			"is a coverage counter that wrapped, not a reading", runs[0].work, shape.sizes[0], runs[1].work, shape.sizes[1])
	}
	growth := shape.sizes[1] / shape.sizes[0]
	if most := uint64(linearSlack*growth) * runs[0].bytes; runs[0].bytes == 0 || runs[1].bytes > most {
		t.Errorf("replacing every other block of %d allocated %d bytes, %.1f times the %d of %d blocks: the blocks grew %d times, and a "+
			"write linear in them may allocate %d at most", shape.sizes[1], runs[1].bytes, float64(runs[1].bytes)/float64(runs[0].bytes),
			runs[0].bytes, shape.sizes[0], growth, most)
	}
}

// alternatingAllowance is how many items a block Update may reach beside ygo's own walks to every
// other block (TestUpdateIsLinearInTheBlockCount): the list of the fragment's children once, and
// for each paragraph it keeps the list of that paragraph's children. A write that lists the
// fragment's children again for each replaced block reaches half the blocks again for every block.
const alternatingAllowance = 4

// alternatingFloor and alternatingCeiling bound the statements the counters count for each item
// Update may reach replacing every other block (ygoWalks, and alternatingAllowance a block), from
// the readings of alternatingShape taken as delimiterAndOpenerHeavyReadings were: 100,277,753
// statements over 4,096 blocks (11.93 an item allowed, 99,233,140 of them ygo's) and 1,558,820,017
// over 16,384 (11.61, 1,554,641,964 of them ygo's). The floor is a quarter of the lower, the
// ceiling four times the higher.
const (
	alternatingFloor   = 2.90
	alternatingCeiling = 47.7
)

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

// allocatedBy is the bytes the process allocated on the heap while op ran (runtime.MemStats's
// TotalAlloc, which grows by each allocation's size and which no collection or load of the machine
// moves). The counted child reads it of each shape's op: the bytes see what no counter sits in, a
// string or a list built again for each piece of the input by the standard library (a text joined
// from the rest of its lines at each line), whose cost grows in the copies it allocates.
func allocatedBy(op func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	op()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func sizeText(size int) string {
	if size >= 1<<20 && size%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", size>>20)
	}
	return fmt.Sprintf("%d KiB", size>>10)
}
