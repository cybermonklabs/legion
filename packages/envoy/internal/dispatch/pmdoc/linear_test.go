package pmdoc

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

// runLinearRow uses the same shape registry as the counted child. A URL slash must not
// turn one shape name into another level of the subtest path.
func runLinearRow(t *testing.T) {
	t.Helper()
	t.Parallel()
	row := t.Name()
	for _, leaf := range countedRows[row]() {
		name := strings.ReplaceAll(leaf.shape, "/", "∕")
		// Go changes spaces to underscores, which would merge two URL spellings.
		if strings.TrimSpace(name) != name {
			name = strings.ReplaceAll(name, " ", "␠")
		}
		t.Run(name+"/"+leaf.operation, func(t *testing.T) {
			if leaf.sizes[0] <= 0 {
				t.Fatalf("%s: no builder size is recorded", leaf.name)
			}
			runCountedLeaf(t, row, leaf)
		})
	}
}

// linearReading records each leaf's own builder size and its measured bounds.
type linearReading struct {
	small int
	countedReading
}

// withLinearReadings binds each reading to the shape and operation that took it.
func withLinearReadings(shapes []countedShape, readings map[string]linearReading) []countedShape {
	for i := range shapes {
		shape := &shapes[i]
		if reading, ok := readings[shape.name]; ok {
			shape.reading = reading.countedReading
			if shape.sizes == [2]int{} {
				shape.sizes = [2]int{reading.small, reading.small * growthStep}
			}
		}
	}
	return shapes
}

// linearShape labels one operation on one fixture for both the parent and the child.
func linearShape(shape, operation, unit string) countedShape {
	return countedShape{
		countedLeaf: countedLeaf{name: shape + "/" + operation, unit: unit},
		shape:       shape, operation: operation,
	}
}

// parseGrowthShape counts a markdown operation but not the building of its input.
func parseGrowthShape(shape, operation, unit string, body func(int) string, allowMarksRefusal bool) countedShape {
	leaf := linearShape(shape, operation, unit)
	leaf.prepare = func(t *testing.T, size int) (int, int, func() string) {
		markdown := body(size)
		return size, len(markdown), func() string {
			var err error
			switch operation {
			case "Parse":
				_, err = Parse(markdown)
			case "ParseForWrite":
				_, err = ParseForWrite(markdown, nil)
			case "ParseRendering":
				_, err = ParseRendering(markdown)
			case "ParseRendering+Render":
				var doc *Node
				doc, err = ParseRendering(markdown)
				if err != nil {
					t.Fatalf("ParseRendering at %d bytes: %v", len(markdown), err)
				}
				_, err = Render(doc)
			default:
				t.Fatalf("unknown markdown operation %q", operation)
			}
			if err != nil && !(allowMarksRefusal && refusedForItsMarks(err)) {
				t.Fatalf("%s at %d bytes: %v", operation, len(markdown), err)
			}
			return ""
		}
	}
	return leaf
}

// renderGrowthShape builds the render tree outside the operation it counts.
func renderGrowthShape(shape, unit string, body func(int) string, label string) countedShape {
	leaf := linearShape(shape, "Render", unit)
	leaf.prepare = func(t *testing.T, size int) (int, int, func() string) {
		paragraph := func(text string) *Node {
			return &Node{Type: "paragraph", Children: []*Node{{Type: "text", Text: text}}}
		}
		text := body(size)
		doc := &Node{Type: "doc", Children: []*Node{paragraph(text)}}
		characters := len(text)
		if label != "" {
			doc.Children = append(doc.Children, &Node{Type: "footnote_definition", Attrs: Attrs{"label": label}, Children: []*Node{paragraph("x")}})
			characters++
		}
		return size, characters, func() string {
			if _, err := Render(doc); err != nil {
				t.Fatalf("Render at %d bytes: %v", len(text), err)
			}
			return ""
		}
	}
	return leaf
}

// TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText counts each shape at two sizes,
// including work in Goldmark and the bytes that standard-library searches read. The reader
// formerly walked back over delimiter runs for each closer, and the renderer searched the
// rest of the text from each opener. A caller's write of a mebibyte of some shapes would be
// refused for the elements it makes, so the uncapped parse reads them as a rendering does.
func TestParseAndRenderAreLinearOnDelimiterAndOpenerHeavyText(t *testing.T) {
	runLinearRow(t)
}

// delimiterAndOpenerShapes keeps the different reasons for a repeated scan in separate leaves.
func delimiterAndOpenerShapes() []countedShape {
	var shapes []countedShape
	for _, repeat := range []string{"a_", "a_b*", "a~b_"} {
		shapes = append(shapes, parseGrowthShape(repeat, "ParseRendering", "repeat",
			func(n int) string { return strings.Repeat(repeat, n) }, false))
	}

	// Reading a blank line in a list item's typed block once per item walked the whole
	// block and then the list again for each node. readSpreads now reads the document once.
	// Each item brings 200 blank lines so both grow, without hitting MaxDocumentElements.
	// Refusing the write would stop the operation before the work this shape must count.
	spacedItem := "  - p\n" + strings.Repeat("\n", 200)
	shapes = append(shapes, parseGrowthShape("a typed block of spaced items in a list item", "ParseForWrite", "item",
		func(n int) string {
			return "- a\n  :::callout{#c1 kind=\"note\" title=\"T\"}\n" + strings.Repeat(spacedItem, n) + "  :::\n"
		}, false))

	// A typed block in a quote that ends with a bare quote marker inserts its exits before
	// events already read. Inserting one by one moved the stream and walked every line's
	// marks per block, so insertEvents and writeInserts now collect and write them once.
	// Each block brings 200 blank lines. This shape also requires the write to succeed.
	bareExit := "> :::callout{#c%d kind=\"note\" title=\"T\"}\n>\nb\n" + strings.Repeat("\n", 200)
	shapes = append(shapes, parseGrowthShape("quoted typed blocks ending on a bare line", "ParseForWrite", "block",
		func(n int) string {
			var text strings.Builder
			for index := range n {
				fmt.Fprintf(&text, bareExit, index)
			}
			return text.String()
		}, false))

	// Goldmark's ProcessDelimiters walked past runs of either character: `~~a ` then `a* `
	// and `*a ` then `a~ ` took 3.8–4.2 s at 112 KB. Pairing tilde runs by length makes
	// `~~a ` then `a~ ` and `~a ` then `a~~ ` do the same walk (INFRA-534). The rule of
	// three prevents the `*` of `a*a` from closing `**`, so `**a ` then `a*a` took 100 s
	// at 437 KiB. Both readers may refuse these shapes for the elements or nesting their
	// marks make, but neither may take quadratic work to answer.
	for _, part := range []struct{ before, open, close, after string }{
		{"", "~~a ", "a~ ", ""},
		{"", "~a ", "a~~ ", ""},
		{"", "~~a ", "a* ", ""},
		{"", "*a ", "a~ ", ""},
		{"~a [", "~~a ", "a~ ", "](u)"},
		{"", "*~a", "~*", ""},
		{"", "**a ", "a*a", ""},
		{"", "a**a*a ", "a*a**a", ""},
	} {
		name := part.before + part.open + "…" + part.close + "…" + part.after
		for _, operation := range []string{"ParseRendering", "Parse"} {
			shapes = append(shapes, parseGrowthShape(name, operation, "pair",
				func(n int) string {
					return part.before + strings.Repeat(part.open, n) + strings.Repeat(part.close, n) + part.after
				}, true))
		}
	}
	for _, repeat := range []string{"[a", "a_b&", "<a", "[^a"} {
		shapes = append(shapes, renderGrowthShape(repeat, "repeat",
			func(n int) string { return strings.Repeat(repeat, n) }, ""))
	}
	return withLinearReadings(shapes, delimiterAndOpenerReadings)
}

// delimiterAndOpenerReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var delimiterAndOpenerReadings = map[string]linearReading{
	"**a …a*a…/Parse":          {585, countedReading{past: 1709.5667887667887, most: 1715.7760683760685, scanMost: 7}},
	"**a …a*a…/ParseRendering": {585, countedReading{past: 1517.063492063492, most: 1523.1641025641027, scanMost: 7}},
	"*a …a~ …/Parse":           {682, countedReading{past: 1520.0312107247591, most: 1524.983870967742, scanMost: 6}},
	"*a …a~ …/ParseRendering":  {682, countedReading{past: 1342.0312107247591, most: 1346.9516129032259, scanMost: 6}},
	"*~a…~*…/Parse":            {819, countedReading{past: 1766.060003488575, most: 1775.3003663003662, scanMost: 5}},
	"*~a…~*…/ParseRendering":   {819, countedReading{past: 1650.060003488575, most: 1659.2905982905984, scanMost: 5}},
	"<a/Render":                {2048, countedReading{past: 989, most: 991.22314453125, searchedMost: 1.99993896484375, scanMost: 4.00048828125}},
	"[^a/Render":               {1365, countedReading{past: 86, most: 86.35531135531136, searchedMost: 2.999908424908425}},
	"[a/Render":                {2048, countedReading{past: 61, most: 61.23681640625, searchedMost: 1.99993896484375}},
	"a typed block of spaced items in a list item/ParseForWrite": {19, countedReading{past: 124479.03007518797, most: 125217.05263157895, scanMost: 218.26315789473685}},
	"a**a*a …a*a**a…/Parse":                                      {315, countedReading{past: 3474.7292517006804, most: 3486.504761904762, scanMost: 13}},
	"a**a*a …a*a**a…/ParseRendering":                             {315, countedReading{past: 3099.0625850340134, most: 3110.5174603174605, scanMost: 13}},
	"a_/ParseRendering":                                          {2048, countedReading{past: 515, most: 516.6591796875, scanMost: 2}},
	"a_b&/Render":                                                {1024, countedReading{past: 129, most: 129.4619140625}},
	"a_b*/ParseRendering":                                        {1024, countedReading{past: 1267.015625, most: 1270.1640625, scanMost: 4}},
	"a~b_/ParseRendering":                                        {1024, countedReading{past: 1245.515625, most: 1248.8251953125, scanMost: 4}},
	"quoted typed blocks ending on a bare line/ParseForWrite":    {16, countedReading{past: 26852.348214285714, most: 27019.1875, scanMost: 299.2734375}},
	"~a [~~a …a~ …](u)/Parse":                                    {585, countedReading{past: 1418.0312576312576, most: 1427.1299145299145, scanMost: 7.013675213675214}},
	"~a [~~a …a~ …](u)/ParseRendering":                           {585, countedReading{past: 1239.0312576312576, most: 1247.6564102564103, scanMost: 7.013675213675214}},
	"~a …a~~ …/Parse":                                            {585, countedReading{past: 1516.0312576312576, most: 1521.5384615384614, scanMost: 7}},
	"~a …a~~ …/ParseRendering":                                   {585, countedReading{past: 1337.0312576312576, most: 1342.5008547008547, scanMost: 7}},
	"~~a …a* …/Parse":                                            {585, countedReading{past: 1524.0312576312576, most: 1529.5299145299145, scanMost: 7}},
	"~~a …a* …/ParseRendering":                                   {585, countedReading{past: 1345.0312576312576, most: 1350.4923076923078, scanMost: 7}},
	"~~a …a~ …/Parse":                                            {585, countedReading{past: 1516.0312576312576, most: 1521.531623931624, scanMost: 7}},
	"~~a …a~ …/ParseRendering":                                   {585, countedReading{past: 1337.0312576312576, most: 1342.4940170940172, scanMost: 7}},
}

// refusedForItsMarks reports whether err refuses markdown for the inline marks it makes: more
// elements than a write may make, or marks nested past maxInlineNesting.
func refusedForItsMarks(err error) bool {
	nesting := nestingError{}
	return errors.Is(err, ErrTooManyElements) || errors.As(err, &nesting) && nesting.inline
}

// TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds counts shapes that took quadratic work
// before LEGION-465. Each delimiter character measured its whole run; each `[^` before one `]`
// case-folded the label to look it up; each line feed joined the rest of the text to ask if only
// whitespace followed; and Goldmark's strikethrough parser rescanned a run from each `~`. Before
// the fix 128 KiB of each text took 18–115 s to render, and 32 KiB of `[^a` took 7.7 s.
func TestRenderIsLinearOnRunsFootnoteLabelsAndLineFeeds(t *testing.T) {
	runLinearRow(t)
}

// runsAndLabelsShapes separates the scans that the run, footnote and line-feed inputs exercise.
func runsAndLabelsShapes() []countedShape {
	var shapes []countedShape
	for _, delimiter := range []string{"*", "_", "~"} {
		shapes = append(shapes, renderGrowthShape("a run of "+delimiter, "byte",
			func(n int) string { return strings.Repeat(delimiter, n) }, ""))
	}
	for _, footnote := range []struct{ description, label string }{
		{"no footnote", ""}, {"a short label defined", "a"}, {"a long label defined", strings.Repeat("b", 1000)},
	} {
		shapes = append(shapes, renderGrowthShape("[^a before one ] with "+footnote.description, "byte",
			func(n int) string { return strings.Repeat("[^a", (n-1)/3) + "]" + strings.Repeat("x", (n-1)%3) }, footnote.label))
	}
	shapes = append(shapes, renderGrowthShape("line feeds", "repeat",
		func(n int) string { return strings.Repeat("a\n", n) }, ""))

	// A run after an escaped `~` may open or close, so its run is scanned rather than
	// refused. The repeated escapes are read as a rendering so no element limit stops them.
	shapes = append(shapes,
		parseGrowthShape("a run of ~", "Parse", "byte", func(n int) string { return "a" + strings.Repeat("~", n-1) }, false),
		parseGrowthShape(`\~ and a run of ~`, "Parse", "byte", func(n int) string { return `\~` + strings.Repeat("~", n-2) }, false),
		parseGrowthShape(`a\~~ repeated`, "ParseRendering", "repeat", func(n int) string { return strings.Repeat(`a\~~`, n) }, false),
	)
	return withLinearReadings(shapes, runsAndLabelsReadings)
}

// runsAndLabelsReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var runsAndLabelsReadings = map[string]linearReading{
	"[^a before one ] with a long label defined/Render":  {4096, countedReading{past: 32.333077566964285, most: 49.72216796875, searchedMost: 0.99993896484375}},
	"[^a before one ] with a short label defined/Render": {4096, countedReading{past: 32.333077566964285, most: 32.7998046875, searchedMost: 0.99993896484375}},
	"[^a before one ] with no footnote/Render":           {4096, countedReading{past: 28.999860491071427, most: 29.117431640625, searchedMost: 0.99993896484375}},
	"\\~ and a run of ~/Parse":                           {4096, countedReading{past: 225, most: 225.841064453125, scanMost: 1}},
	"a run of */Render":                                  {4096, countedReading{past: 58, most: 59.095947265625, scanMost: 2.000244140625}},
	"a run of _/Render":                                  {4096, countedReading{past: 46, most: 47.10791015625, scanMost: 2.000244140625}},
	"a run of ~/Parse":                                   {4096, countedReading{past: 222, most: 222.9013671875, scanMost: 1}},
	"a run of ~/Render":                                  {4096, countedReading{past: 631, most: 633.312744140625, scanMost: 4.000732421875}},
	"a\\~~ repeated/ParseRendering":                      {1024, countedReading{past: 754.015625, most: 757.431640625, scanMost: 4}},
	"line feeds/Render":                                  {2048, countedReading{past: 286, most: 286.14404296875}},
}

// TestParseIsLinearInATableOfEscapedPipes counts the read-back of a table whose every row
// holds an escaped pipe in a code span. Goldmark's table transformer compared each code span
// with every escaped pipe in the document. On a machine with a load of about 300, 16 KiB took
// 35 ms and 256 KiB took 6.8 s, and 1 MiB took 2 min 9 s. unescapeTablePipes took 19 ms,
// 0.6–0.8 s and 2.0–2.4 s there. A large table is read as a rendering so the element limit
// does not stop the parse before it reaches the escaped pipes.
func TestParseIsLinearInATableOfEscapedPipes(t *testing.T) {
	runLinearRow(t)
}

// escapedPipeTableShapes keeps one growing code-span cell per row.
func escapedPipeTableShapes() []countedShape {
	const header, row = "| a |\n| --- |\n", "`\\|`\n"
	return withLinearReadings([]countedShape{
		parseGrowthShape(`a table of \| cells`, "ParseRendering", "row",
			func(n int) string { return header + strings.Repeat(row, n) }, false),
	}, escapedPipeTableReadings)
}

// escapedPipeTableReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var escapedPipeTableReadings = map[string]linearReading{
	"a table of \\| cells/ParseRendering": {819, countedReading{past: 2010.027036455608, most: 2016.4725274725274, scanMost: 5.017094017094017}},
}

// TestNoReaderRegistersGoldmarksTableASTTransformer checks the display and inline readers as
// well as the document reader. Goldmark's transformer compares every escaped pipe with every
// code span; pmdoc's unescapeTablePipes does the same job in one walk of the cells.
func TestNoReaderRegistersGoldmarksTableASTTransformer(t *testing.T) {
	t.Parallel()
	const table = "| a | b |\n| --- | --- |\n| `\\|` | x |\n"
	tree, err := ParseRendering(table)
	if err != nil {
		t.Fatalf("parse the table: %v", err)
	}
	if _, err := Render(tree); err != nil {
		t.Fatalf("render the table: %v", err)
	}
	_ = ReadForDisplay(table, DisplayLatest)
	const inline = "`\\|` a"
	if _, err := ParseInline(inline, NewWriteBudget()); err != nil {
		t.Fatalf("parse the inline markdown: %v", err)
	}

	goldmarks := reflect.TypeOf(extension.NewTableASTTransformer())
	check := func(name string, p parser.Parser) {
		t.Run(name, func(t *testing.T) {
			for _, registered := range astTransformerTypes(t, p) {
				if registered == goldmarks {
					t.Errorf("registers goldmark's table AST transformer (%s), whose work in a table of escaped pipes is quadratic", registered)
				}
			}
		})
	}
	check("blockReader", blockReader.md.Parser())
	check("displayReader", displayReader.md.Parser())
	for _, entry := range []struct {
		name string
		read inlineReader
	}{
		{"inlineMarkdown", inlineMarkdown},
		{"storedMarkdown", storedMarkdown},
		{"flankingMarkdown", flankingMarkdown},
	} {
		// Build both parsers even if the table's render did not need this reader.
		for _, labels := range [][]string{nil, {"note"}} {
			if _, _, err := parseInlineWithDefinitions(inline, labels, entry.read); err != nil {
				t.Fatalf("build %s with definitions %v: %v", entry.name, labels, err)
			}
		}
		check(entry.name+".run", entry.read.run)
		check(entry.name+".withDefinitions", entry.read.withDefinitions)
	}
}

// astTransformerTypes reads the built list because Goldmark exposes no accessor for it. A
// parser that has not been built or whose fields have changed fails instead of appearing empty.
func astTransformerTypes(t *testing.T, p parser.Parser) []reflect.Type {
	t.Helper()
	value := reflect.ValueOf(p)
	if value.Kind() != reflect.Pointer || value.Elem().Kind() != reflect.Struct {
		t.Fatalf("goldmark parser %T is not a pointer to a struct", p)
	}
	built := value.Elem().FieldByName("astTransformers")
	config := value.Elem().FieldByName("config")
	if built.Kind() != reflect.Slice || config.Kind() != reflect.Pointer || !config.IsNil() {
		t.Fatalf("goldmark parser %T does not have a built astTransformers slice and a cleared config", p)
	}
	types := make([]reflect.Type, 0, built.Len())
	for i := range built.Len() {
		slot := built.Index(i)
		if slot.Kind() != reflect.Interface || slot.IsNil() {
			t.Fatalf("goldmark parser %T has an unreadable AST transformer at %d", p, i)
		}
		types = append(types, slot.Elem().Type())
	}
	return types
}

// TestParseIsLinearInFootnoteReferences counts one paragraph referring to each definition,
// many paragraphs with their own definitions, and references that match only up to case.
// Goldmark's footnote parser walked every definition for each reference: at a load of about
// 800, a paragraph of 16,000 references took 1.1 s, 16 times the 65 ms of 4,000. pmdoc's
// footnoteReferenceParser looks up each definition in a map built once per parse.
func TestParseIsLinearInFootnoteReferences(t *testing.T) {
	runLinearRow(t)
}

// footnoteReferenceShapes grows references and definitions together, one of each per unit.
func footnoteReferenceShapes() []countedShape {
	var shapes []countedShape
	for _, shape := range []struct {
		name string
		each func(int) (string, string)
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
		shapes = append(shapes, parseGrowthShape(shape.name, "ParseRendering", "reference",
			func(n int) string {
				var markdown, definitions strings.Builder
				for index := 1; index <= n; index++ {
					reference, definition := shape.each(index)
					markdown.WriteString(reference)
					definitions.WriteString(definition)
				}
				return markdown.String() + "\n\n" + definitions.String()
			}, false))
	}
	return withLinearReadings(shapes, footnoteReferenceReadings)
}

// footnoteReferenceReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var footnoteReferenceReadings = map[string]linearReading{
	"many paragraphs that each refer to their own definition/ParseRendering": {113, countedReading{past: 7477.022756005057, most: 7451.804203539823, scanMost: 35.52433628318584}},
	"one paragraph that refers to each definition/ParseRendering":            {157, countedReading{past: 5671.787079162876, most: 5651.548566878981, scanMost: 26.35748407643312}},
	"references that match their definitions up to case/ParseRendering":      {146, countedReading{past: 6462.86301369863, most: 6437.9375, scanMost: 28.158390410958905}},
}

// TestBareURLShapesParseAndRenderLinearly counts a read and its render as one operation.
// The tokenizer reads markdown and the transform reads decoded text. A line can try an
// address after whitespace, punctuation, an escape, a character reference or a letter outside
// ASCII, and refuse it only at the domain, its `@` or its previous character. Nothing scanned
// on a refused read may be scanned again from the next opening.
//
// A text `<` before each address can open a comment, a processing instruction, a declaration,
// a CDATA section or a tag. The renderer searches to the closer at the end of the run, past the
// next `<` in a quoted attribute value. It escapes each such `<`, so the readers do not repeat
// those searches. A paragraph that reads back in no spelling is written in every spelling
// runSpellings lists; unreadableRun needs all the families. Each writing includes the plain
// text after the prefix and is read back once (TestRenderReadsEachWritingBackUnderAReaderOnce).
func TestBareURLShapesParseAndRenderLinearly(t *testing.T) {
	runLinearRow(t)
}

// bareURLShapes keeps the rejected address spellings and the escaped opener/closer pairs apart.
func bareURLShapes() []countedShape {
	var shapes []countedShape
	for _, repeat := range []string{
		"_www.a", "-www.x_", `(https://-a.b\`, ".www.a&", ".www", "www.", `www\.`, `www\.a `, "HTTP://a ", ` "'https://-a.b`, `(""www.a_`, ` ".www.a`,
		";https://-a.b", "x;www.a_", `\*https://-a`, "&amp;https://-a.", "éhttps://-a.", `http\://-a.`, `www\.a_`, `x"www.a_`, ";;", `\*`, ";a_a@", "é+a@b-", `www\.a@`,
	} {
		shapes = append(shapes, readAndRenderShape(repeat, "", repeat, ""))
	}
	for _, html := range []struct{ opener, closer string }{
		{"&lt;!-- a@b.cc ", "-->"}, {"&lt;?x;https://a.bc/ ", "?>"}, {"&lt;!X a@b.cc ", ">"}, {"&lt;![CDATA[ a@b.cc ", "]]>"},
		{`&lt;a b="x;a@b.cc `, `">`}, {`&lt;a b='&lt;a c="x;a@b.cc `, `'>`},
	} {
		shapes = append(shapes, readAndRenderShape(html.opener+"…"+html.closer, "", html.opener, html.closer))
	}
	shapes = append(shapes, readAndRenderShape("unreadableRun before repeated lorem ipsum", unreadableRun, "lorem ipsum ", ""))
	return withLinearReadings(shapes, bareURLReadings)
}

// bareURLReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var bareURLReadings = map[string]linearReading{
	" \"'https://-a.b/ParseRendering+Render":                          {273, countedReading{past: 5194, most: 5215.934065934066, scanMost: 30.014652014652015}},
	" \".www.a/ParseRendering+Render":                                 {512, countedReading{past: 3789, most: 3800.298828125, scanMost: 16.0078125}},
	"&amp;https://-a./ParseRendering+Render":                          {256, countedReading{past: 2896, most: 2932.21875, scanMost: 28.01953125}},
	"&lt;!-- a@b.cc …-->/ParseRendering+Render":                       {273, countedReading{past: 6351, most: 6383.871794871795, searchedMost: 65.97802197802197, scanMost: 45.040293040293044}},
	"&lt;!X a@b.cc …>/ParseRendering+Render":                          {292, countedReading{past: 5752, most: 5780.311643835616, searchedMost: 33.98929794520548, scanMost: 43.023972602739725}},
	"&lt;![CDATA[ a@b.cc …]]>/ParseRendering+Render":                  {204, countedReading{past: 13617, most: 13598.96875, searchedMost: 165.94975490196077, scanMost: 73.07843137254902}},
	"&lt;?x;https://a.bc/ …?>/ParseRendering+Render":                  {195, countedReading{past: 4980, most: 5024.54358974359, searchedMost: 41.98205128205128, scanMost: 40.04615384615385}},
	"&lt;a b=\"x;a@b.cc …\">/ParseRendering+Render":                   {227, countedReading{past: 20821, most: 20858.40088105727, searchedMost: 14, scanMost: 178.13215859030836}},
	"&lt;a b='&lt;a c=\"x;a@b.cc …'>/ParseRendering+Render":           {151, countedReading{past: 31924, most: 31950.71523178808, searchedMost: 26, scanMost: 217.19867549668874}},
	"(\"\"www.a_/ParseRendering+Render":                               {455, countedReading{past: 2771.0153846153844, most: 2787.9142857142856, scanMost: 19.01098901098901}},
	"(https://-a.b\\/ParseRendering+Render":                           {292, countedReading{past: 2658, most: 2689.695205479452, scanMost: 27.017123287671232}},
	"-www.x_/ParseRendering+Render":                                   {585, countedReading{past: 3556.015628815629, most: 3575.8581196581194, scanMost: 31.017094017094017}},
	".www.a&/ParseRendering+Render":                                   {585, countedReading{past: 1318, most: 1327.9675213675214, scanMost: 7.005128205128205}},
	".www/ParseRendering+Render":                                      {1024, countedReading{past: 811, most: 818.5224609375, scanMost: 8.0048828125}},
	";;/ParseRendering+Render":                                        {2048, countedReading{past: 334, most: 336.177734375, scanMost: 2.00146484375}},
	";a_a@/ParseRendering+Render":                                     {819, countedReading{past: 2018, most: 2027.3797313797313, scanMost: 10.006105006105006}},
	";https://-a.b/ParseRendering+Render":                             {315, countedReading{past: 2572, most: 2601.209523809524, scanMost: 26.015873015873016}},
	"HTTP://a /ParseRendering+Render":                                 {455, countedReading{past: 2204, most: 2219.8043956043957, scanMost: 18.01098901098901}},
	"\\*/ParseRendering+Render":                                       {2048, countedReading{past: 216, most: 220.2490234375, scanMost: 4.00390625}},
	"\\*https://-a/ParseRendering+Render":                             {341, countedReading{past: 2706, most: 2728.6217008797653, scanMost: 24.01466275659824}},
	"_www.a/ParseRendering+Render":                                    {682, countedReading{past: 5082, most: 5115.260997067448, scanMost: 39.077712609970675}},
	"http\\://-a./ParseRendering+Render":                              {372, countedReading{past: 2074, most: 2097.798387096774, scanMost: 21.013440860215052}},
	"unreadableRun before repeated lorem ipsum/ParseRendering+Render": {341, countedReading{past: 165728, most: 180542.63929618767, scanMost: 848.8181818181819}},
	"www./ParseRendering+Render":                                      {1024, countedReading{past: 420, most: 428.8740234375, scanMost: 8.0048828125}},
	"www\\./ParseRendering+Render":                                    {819, countedReading{past: 1008, most: 1020.6483516483516, scanMost: 14.008547008547009}},
	"www\\.a /ParseRendering+Render":                                  {585, countedReading{past: 2885, most: 2897.459829059829, scanMost: 13.008547008547009}},
	"www\\.a@/ParseRendering+Render":                                  {585, countedReading{past: 639, most: 648.3128205128205, scanMost: 7.005128205128205}},
	"www\\.a_/ParseRendering+Render":                                  {585, countedReading{past: 3313, most: 3329.4307692307693, scanMost: 20.015384615384615}},
	"x\"www.a_/ParseRendering+Render":                                 {512, countedReading{past: 2529, most: 2543.978515625, scanMost: 16.01171875}},
	"x;www.a_/ParseRendering+Render":                                  {512, countedReading{past: 2529, most: 2543.978515625, scanMost: 16.01171875}},
	"é+a@b-/ParseRendering+Render":                                    {585, countedReading{past: 1515, most: 1528.5589743589744, scanMost: 14.008547008547009}},
	"éhttps://-a./ParseRendering+Render":                              {315, countedReading{past: 2548, most: 2573.133333333333, scanMost: 26.015873015873016}},
}

// TestUnclosedRawHTMLShapesParseAndRenderLinearly counts a read and its render as one operation.
// A declaration opens at `<!` and an ASCII letter and ends at `>` even on a later line of the
// block. Without a closer, each opener walked to the block's end. A mebibyte of `p ` and `<!a`
// repeated took 9.97 s to Parse and 19.38 s to ParseForWrite. With `<!A` it took 19.24 s and
// 37.02 s because Goldmark's own parser walked the block again. `x <!a` on each line took
// 6.88 s at 64 KiB and 27.60 s at 128 KiB. unclosedRawHTML records the missing closer, and
// inlineRawHTMLParser walks a declaration once rather than twice. The same reads took 0.21 s,
// 0.67 s, 0.20 s, 0.51 s, 0.05 s and 0.13 s after those changes.
//
// A comment ends at `-->`, a processing instruction at `?>`, and a CDATA section at `]]>`.
// Goldmark walked to the end from each opener. A mebibyte took 24.97 s to Parse and 50.17 s to
// ParseForWrite for comments, 46.31 s and 93.65 s for processing instructions, and 1.61 s and
// 1.66 s for CDATA, which the element limit refused. The wrapper now records each kind's
// missing closer, so the same reads took 0.23 s and 0.55 s, 0.22 s and 0.62 s, and 0.15 s and
// 0.11 s. CDATA has a link opener at each `[`, so reading as a rendering keeps the element
// limit from stopping its parse.
func TestUnclosedRawHTMLShapesParseAndRenderLinearly(t *testing.T) {
	runLinearRow(t)
}

// unclosedRawHTMLShapes uses a leading word so each opener belongs to a paragraph.
func unclosedRawHTMLShapes() []countedShape {
	var shapes []countedShape
	for _, repeat := range []string{"<!a", "<!A", "<!--", "<?", "<![CDATA["} {
		shapes = append(shapes, readAndRenderShape("p "+repeat, "p ", repeat, ""))
	}
	shapes = append(shapes, readAndRenderShape("x <!a and a line feed", "", "x <!a\n", ""))
	return withLinearReadings(shapes, unclosedRawHTMLReadings)
}

// unclosedRawHTMLReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var unclosedRawHTMLReadings = map[string]linearReading{
	"p <!--/ParseRendering+Render":                {1024, countedReading{past: 920, most: 924.9296875, searchedMost: 20.0087890625, scanMost: 4.0048828125}},
	"p <!A/ParseRendering+Render":                 {1365, countedReading{past: 625, most: 628.6717948717949, searchedMost: 9.003663003663004, scanMost: 3.0036630036630036}},
	"p <![CDATA[/ParseRendering+Render":           {455, countedReading{past: 1716, most: 1727.0263736263737, searchedMost: 54.01758241758242, scanMost: 9.010989010989011}},
	"p <!a/ParseRendering+Render":                 {1365, countedReading{past: 625, most: 628.6717948717949, searchedMost: 9.003663003663004, scanMost: 3.0036630036630036}},
	"p <?/ParseRendering+Render":                  {2048, countedReading{past: 480, most: 482.4658203125, searchedMost: 10.00439453125, scanMost: 2.00244140625}},
	"x <!a and a line feed/ParseRendering+Render": {682, countedReading{past: 1792.0043988269795, most: 1798.7771260997067, searchedMost: 17.999816715542522, scanMost: 6.004398826979473}},
}

// readAndRenderShape keeps the input's prefix and closer fixed while the middle grows.
func readAndRenderShape(name, head, repeat, tail string) countedShape {
	return parseGrowthShape(name, "ParseRendering+Render", "repeat",
		func(n int) string { return head + strings.Repeat(repeat, n) + tail + "._\n" }, false)
}

// TestUpdateIsLinearInTheBlockCount counts the work of seeding or replacing contiguous blocks.
// Update used to insert the children one index at a time, and ygo walks to the index for each
// insert unless the element is still buffered. On a machine with load 140–200, seeding 16,384
// one-word paragraphs took 2.1 s of CPU and 65,536 took 36 s; a quote of 21,844 rules took
// 4.3 s, and replacing a quote of 21,844 headings with a quote of rules took 11 s through the
// upload route. A rebuild writes the stored rendering whole, whatever its block count.
//
// Every other block is a separate run, so the index walks necessarily grow quadratically.
// At 4,096 and 16,384 blocks, Update took 134 ms and 5.83 s, or 2.1 and 1.6 times ygo's
// Delete and Insert alone. Before batching, it took 112 ms and 5.65 s (1.6 times ygo's walks
// at both sizes). Typing each batch from the live fragment's children caused another full
// walk per replaced block and took 936 ms at 4,096 blocks (12.7 times). This shape counts
// the items reached, and compares its counted statements with the same edits made in ygo.
func TestUpdateIsLinearInTheBlockCount(t *testing.T) {
	runLinearRow(t)
}

// updateShapes builds a fresh document outside each measured Update and checks its read-back.
func updateShapes() []countedShape {
	var shapes []countedShape
	for _, test := range []struct{ name, from, to string }{
		{"seeding paragraphs", "", "a\n\n"},
		{"seeding a quote of rules", "", "> ***\n"},
		{"replacing headings with paragraphs", "#\n", "a\n\n"},
		{"replacing a quote of headings with a quote of rules", "> #\n", "> ***\n"},
	} {
		leaf := linearShape(test.name, "Update", "block")
		leaf.prepare = func(t *testing.T, blocks int) (int, int, func() string) {
			doc := crdt.New(crdt.WithClientID(1))
			fragment := doc.GetXmlFragment("prosemirror")
			if test.from != "" {
				writeUpdate(t, doc, fragment, repeatedBlocks(t, test.from, blocks))
			}
			want := repeatedBlocks(t, test.to, blocks)
			return blocks, len(test.to) * blocks, func() string {
				writeUpdate(t, doc, fragment, want)
				requireUpdateReadBack(t, fragment, want)
				return ""
			}
		}
		shapes = append(shapes, leaf)
	}
	return withLinearReadings(append(shapes, alternatingShape()), updateReadings)
}

// updateReadings records this row's counts (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var updateReadings = map[string]linearReading{
	"replacing a quote of headings with a quote of rules/Update": {512, countedReading{past: 856.8727678571429, most: 854.93701171875}},
	"replacing every other block/Update":                         {4096, countedReading{past: 118718.47330729167, most: 95164.82373046875}},
	"replacing headings with paragraphs/Update":                  {512, countedReading{past: 1323.205357142857, most: 1321.139892578125}},
	"seeding a quote of rules/Update":                            {512, countedReading{past: 411, most: 412.349609375}},
	"seeding paragraphs/Update":                                  {512, countedReading{past: 906, most: 906.513671875}},
}

// alternatingBlocks ends near the most paragraphs a caller write's element budget allows.
var alternatingBlocks = [2]int{4096, 16384}

// alternatingAllowance permits that many items reached per block beside ygo's own walks, and
// that many times ygo's counted statements for Update with its read-back.
const alternatingAllowance = 4

// alternatingFloor and alternatingCeiling bound the counted statements per reached item allowed.
// At the same reading as updateReadings, 4,096 blocks took 11.941460 statements per allowed item
// and 16,384 took 11.611130. The bounds are a quarter of the lower and four times the higher.
const (
	alternatingFloor   = 11.611130 / countedMargin
	alternatingCeiling = 11.941460 * countedMargin
)

// alternatingShape replaces each heading between two kept paragraphs as a run of its own.
func alternatingShape() countedShape {
	leaf := linearShape("replacing every other block", "Update", "block")
	leaf.sizes = alternatingBlocks
	leaf.limitChildren = true
	seed := func(t *testing.T, blocks int) (*crdt.Doc, *crdt.YXmlFragment, *Node) {
		doc := crdt.New(crdt.WithClientID(1))
		fragment := doc.GetXmlFragment("prosemirror")
		writeUpdate(t, doc, fragment, repeatedBlocks(t, "#\n\na\n\n", blocks/2))
		return doc, fragment, repeatedBlocks(t, "a\n\n", blocks)
	}
	leaf.prepare = func(t *testing.T, blocks int) (int, int, func() string) {
		doc, fragment, want := seed(t, blocks)
		return 2 * (blocks / 2), len("a\n\n") * blocks, func() string {
			writeUpdate(t, doc, fragment, want)
			requireUpdateReadBack(t, fragment, want)
			return ""
		}
	}
	leaf.prepareWalks = func(t *testing.T, blocks int) (int, int, func() string) {
		doc, fragment, _ := seed(t, blocks)
		return 2 * (blocks / 2), len("a\n\n") * blocks, func() string {
			doc.Transact(func(txn *crdt.Transaction) {
				for index := 0; index < blocks; index += 2 {
					fragment.Delete(txn, index, 1)
					fragment.InsertElement(txn, index, crdt.NewYXmlElement("paragraph"))
				}
			})
			return ""
		}
	}
	return leaf
}

// ygoWalks counts the items ygo's Delete and Insert walk over, and the item each edits.
func ygoWalks(blocks int) uint64 {
	var walked uint64
	for index := 0; index < blocks; index += 2 {
		walked += 2 * (uint64(index) + 1)
	}
	return walked
}

// requireUpdateReadBack requires the measured Update to write exactly the requested tree.
func requireUpdateReadBack(t *testing.T, fragment *crdt.YXmlFragment, want *Node) {
	t.Helper()
	if got, err := Read(fragment); err != nil || !got.Equal(want) {
		t.Fatalf("%d blocks read back otherwise (%v)", len(want.Children), err)
	}
}

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
