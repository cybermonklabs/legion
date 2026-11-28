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
	shapes, ok := countedRows[row]
	if !ok {
		t.Fatalf("no counted shapes for %q", row)
	}
	for _, leaf := range shapes() {
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
		text := body(size)
		doc := &Node{Type: "doc", Children: []*Node{paragraph(text)}}
		characters := len(text)
		if label != "" {
			doc.Children = append(doc.Children, footnoteDefinition(label, paragraph("x")))
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

// delimiterAndOpenerReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var delimiterAndOpenerReadings = map[string]linearReading{
	"**a …a*a…/Parse":          {292, countedReading{past: 1709.5308219178082, most: 1722.541095890411, scanMost: 7}},
	"**a …a*a…/ParseRendering": {292, countedReading{past: 1517.0308219178082, most: 1529.7705479452054, scanMost: 7}},
	"*a …a~ …/Parse":           {682, countedReading{past: 1522.0312107247591, most: 1526.983870967742, scanMost: 7}},
	"*a …a~ …/ParseRendering":  {341, countedReading{past: 1344.0314201927106, most: 1353.8709677419354, scanMost: 7}},
	"*~a…~*…/Parse":            {819, countedReading{past: 1770.060003488575, most: 1779.3003663003662, scanMost: 7}},
	"*~a…~*…/ParseRendering":   {819, countedReading{past: 1654.060003488575, most: 1663.2905982905984, scanMost: 7}},
	"<a/Render":                {2, countedReading{past: 988.7142857142857, most: 3267.5, searchedMost: 1.9375, scanMost: 4.5}},
	"[^a/Render":               {2, countedReading{past: 86, most: 328.5, searchedMost: 2.9375}},
	"[a/Render":                {4, countedReading{past: 61, most: 182.25, searchedMost: 1.96875}},
	"a typed block of spaced items in a list item/ParseForWrite": {2, countedReading{past: 124479, most: 131490.5, scanMost: 263}},
	"a**a*a …a*a**a…/Parse":                                      {19, countedReading{past: 3473.7894736842104, most: 3679.1052631578946, scanMost: 13}},
	"a**a*a …a*a**a…/ParseRendering":                             {19, countedReading{past: 3098.1954887218044, most: 3297.2105263157896, scanMost: 13}},
	"a_/ParseRendering":                                          {128, countedReading{past: 515, most: 541.546875, scanMost: 2}},
	"a_b&/Render":                                                {32, countedReading{past: 129, most: 143.78125}},
	"a_b*/ParseRendering":                                        {1024, countedReading{past: 1267.015625, most: 1270.1640625, scanMost: 4}},
	"a~b_/ParseRendering":                                        {1024, countedReading{past: 1247.515625, most: 1250.8251953125, scanMost: 5}},
	"quoted typed blocks ending on a bare line/ParseForWrite":    {32, countedReading{past: 26856.959821428572, most: 26934.15625, scanMost: 300.13671875}},
	"~a [~~a …a~ …](u)/Parse":                                    {292, countedReading{past: 1422.0313111545988, most: 1440.2671232876712, scanMost: 10.030821917808218}},
	"~a [~~a …a~ …](u)/ParseRendering":                           {292, countedReading{past: 1243.0313111545988, most: 1260.3184931506848, scanMost: 10.030821917808218}},
	"~a …a~~ …/Parse":                                            {292, countedReading{past: 1520.0308219178082, most: 1531.0650684931506, scanMost: 10}},
	"~a …a~~ …/ParseRendering":                                   {292, countedReading{past: 1341.0308219178082, most: 1351.9897260273972, scanMost: 10}},
	"~~a …a* …/Parse":                                            {585, countedReading{past: 1526.0312576312576, most: 1531.5299145299145, scanMost: 9}},
	"~~a …a* …/ParseRendering":                                   {585, countedReading{past: 1347.0312576312576, most: 1352.4923076923078, scanMost: 9}},
	"~~a …a~ …/Parse":                                            {292, countedReading{past: 1520.0308219178082, most: 1531.0513698630136, scanMost: 10}},
	"~~a …a~ …/ParseRendering":                                   {292, countedReading{past: 1341.0308219178082, most: 1351.9760273972602, scanMost: 10}},
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

// runsAndLabelsReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var runsAndLabelsReadings = map[string]linearReading{
	"[^a before one ] with a long label defined/Render":  {512, countedReading{past: 32.335379464285715, most: 171.4296875, searchedMost: 0.999755859375}},
	"[^a before one ] with a short label defined/Render": {32, countedReading{past: 32.36607142857143, most: 91.8125, searchedMost: 0.99609375}},
	"[^a before one ] with no footnote/Render":           {8, countedReading{past: 29.071428571428573, most: 88.625, searchedMost: 0.984375}},
	"\\~ and a run of ~/Parse":                           {512, countedReading{past: 225, most: 231.732421875, scanMost: 1.99951171875}},
	"a run of */Render":                                  {32, countedReading{past: 58, most: 198.28125, scanMost: 2.03125}},
	"a run of _/Render":                                  {32, countedReading{past: 46, most: 187.8125, scanMost: 2.03125}},
	"a run of ~/Parse":                                   {512, countedReading{past: 222, most: 229.21484375, scanMost: 1.999755859375}},
	"a run of ~/Render":                                  {512, countedReading{past: 631, most: 649.509765625, scanMost: 6.001953125}},
	"a\\~~ repeated/ParseRendering":                      {128, countedReading{past: 756.015625, most: 783.34375, scanMost: 5}},
	"line feeds/Render":                                  {256, countedReading{past: 286, most: 287.15234375}},
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

// escapedPipeTableReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var escapedPipeTableReadings = map[string]linearReading{
	"a table of \\| cells/ParseRendering": {409, countedReading{past: 2010.03108627314, most: 2022.9364303178484, scanMost: 5.034229828850855}},
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

// footnoteReferenceReadings were read at these sizes in build-cluster Job
// infra6314-pf-footnote-readings-3524-96v7h at 35246ebc (2026-11-28, Go 1.27.3, 4 CPUs): the largest
// body exceeds a mebibyte, so the pod does not run this row.
var footnoteReferenceReadings = map[string]linearReading{
	"many paragraphs that each refer to their own definition/ParseRendering": {3616, countedReading{past: 7887.762760745892, most: 7856.891765763275, scanMost: 42.46439435840708}},
	"one paragraph that refers to each definition/ParseRendering":            {2512, countedReading{past: 5939.858905823476, most: 5914.661823248408, scanMost: 30.342157643312103}},
	"references that match their definitions up to case/ParseRendering":      {1168, countedReading{past: 6669.015655577299, most: 6640.130886130137, scanMost: 30.644798801369863}},
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

// bareURLReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var bareURLReadings = map[string]linearReading{
	" \"'https://-a.b/ParseRendering+Render":                          {8, countedReading{past: 5194, most: 5942.5, scanMost: 30.5}},
	" \".www.a/ParseRendering+Render":                                 {16, countedReading{past: 3789, most: 4150.5625, scanMost: 16.25}},
	"&amp;https://-a./ParseRendering+Render":                          {4, countedReading{past: 2896, most: 5214, scanMost: 29.25}},
	"&lt;!-- a@b.cc …-->/ParseRendering+Render":                       {2, countedReading{past: 6351, most: 10838, searchedMost: 63, scanMost: 50.5}},
	"&lt;!X a@b.cc …>/ParseRendering+Render":                          {2, countedReading{past: 5752, most: 9885.5, searchedMost: 32.4375, scanMost: 46.5}},
	"&lt;![CDATA[ a@b.cc …]]>/ParseRendering+Render":                  {3, countedReading{past: 7972, most: 11105.666666666666, searchedMost: 97.58333333333333, scanMost: 58.666666666666664}},
	"&lt;?x;https://a.bc/ …?>/ParseRendering+Render":                  {3, countedReading{past: 4980, most: 7875.333333333333, searchedMost: 40.833333333333336, scanMost: 43}},
	"&lt;a b=\"x;a@b.cc …\">/ParseRendering+Render":                   {908, countedReading{past: 20821, most: 20830.350220264318, searchedMost: 14, scanMost: 178.03303964757708}},
	"&lt;a b='&lt;a c=\"x;a@b.cc …'>/ParseRendering+Render":           {604, countedReading{past: 31924, most: 31930.67880794702, searchedMost: 26, scanMost: 217.0496688741722}},
	"(\"\"www.a_/ParseRendering+Render":                               {14, countedReading{past: 2771.0102040816328, most: 3320.214285714286, scanMost: 19.357142857142858}},
	"(https://-a.b\\/ParseRendering+Render":                           {4, countedReading{past: 2658, most: 4971.75, scanMost: 28.25}},
	"-www.x_/ParseRendering+Render":                                   {9, countedReading{past: 3556.0158730158732, most: 4845.777777777777, scanMost: 32.111111111111114}},
	".www.a&/ParseRendering+Render":                                   {4, countedReading{past: 1318, most: 2775.75, scanMost: 7.75}},
	".www/ParseRendering+Render":                                      {16, countedReading{past: 811, most: 1292.4375, scanMost: 8.3125}},
	";;/ParseRendering+Render":                                        {64, countedReading{past: 334, most: 403.6875, scanMost: 2.046875}},
	";a_a@/ParseRendering+Render":                                     {25, countedReading{past: 2018, most: 2325.28, scanMost: 10.2}},
	";https://-a.b/ParseRendering+Render":                             {4, countedReading{past: 2572, most: 4872.25, scanMost: 27.25}},
	"HTTP://a /ParseRendering+Render":                                 {56, countedReading{past: 2204, most: 2332.410714285714, scanMost: 18.089285714285715}},
	"\\*/ParseRendering+Render":                                       {32, countedReading{past: 216, most: 487.9375, scanMost: 4.25}},
	"\\*https://-a/ParseRendering+Render":                             {5, countedReading{past: 2706, most: 4248.8, scanMost: 25}},
	"_www.a/ParseRendering+Render":                                    {21, countedReading{past: 5082, most: 6162.190476190476, scanMost: 41.523809523809526}},
	"http\\://-a./ParseRendering+Render":                              {5, countedReading{past: 2074, most: 3844.6, scanMost: 22}},
	"unreadableRun before repeated lorem ipsum/ParseRendering+Render": {10, countedReading{past: 165728, most: 670920.8, scanMost: 1945.3}},
	"www./ParseRendering+Render":                                      {8, countedReading{past: 420, most: 1555.875, scanMost: 8.625}},
	"www\\./ParseRendering+Render":                                    {25, countedReading{past: 1008, most: 1422.36, scanMost: 14.28}},
	"www\\.a /ParseRendering+Render":                                  {73, countedReading{past: 2885, most: 2984.849315068493, scanMost: 13.068493150684931}},
	"www\\.a@/ParseRendering+Render":                                  {9, countedReading{past: 639, most: 1244.3333333333333, scanMost: 7.333333333333333}},
	"www\\.a_/ParseRendering+Render":                                  {18, countedReading{past: 3313, most: 3847, scanMost: 20.5}},
	"x\"www.a_/ParseRendering+Render":                                 {16, countedReading{past: 2529, most: 3008.3125, scanMost: 16.375}},
	"x;www.a_/ParseRendering+Render":                                  {16, countedReading{past: 2529, most: 3008.3125, scanMost: 16.375}},
	"é+a@b-/ParseRendering+Render":                                    {9, countedReading{past: 1515, most: 2396.3333333333335, scanMost: 14.555555555555555}},
	"éhttps://-a./ParseRendering+Render":                              {4, countedReading{past: 2548, most: 4527.25, scanMost: 27.25}},
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

// unclosedRawHTMLReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var unclosedRawHTMLReadings = map[string]linearReading{
	"p <!--/ParseRendering+Render":                {2, countedReading{past: 920, most: 3444, searchedMost: 24.5, scanMost: 6.5}},
	"p <!A/ParseRendering+Render":                 {5, countedReading{past: 625, most: 1627.4, searchedMost: 10, scanMost: 4}},
	"p <![CDATA[/ParseRendering+Render":           {3, countedReading{past: 1716, most: 3388.3333333333335, searchedMost: 56.666666666666664, scanMost: 10.666666666666666}},
	"p <!a/ParseRendering+Render":                 {5, countedReading{past: 625, most: 1627.4, searchedMost: 10, scanMost: 4}},
	"p <?/ParseRendering+Render":                  {4, countedReading{past: 480, most: 1742.5, searchedMost: 12.25, scanMost: 3.25}},
	"x <!a and a line feed/ParseRendering+Render": {2, countedReading{past: 1792, most: 4103, searchedMost: 17.9375, scanMost: 7.5}},
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

// updateReadings records the counts at the chosen sizes (2026-11-28, pod legion-infra-infra-6314, Go 1.27.3).
var updateReadings = map[string]linearReading{
	"replacing a quote of headings with a quote of rules/Update": {128, countedReading{past: 846.875, most: 845.55859375}},
	"replacing every other block/Update":                         {512, countedReading{past: 15670.966145833334, most: 12725.46142578125}},
	"replacing headings with paragraphs/Update":                  {128, countedReading{past: 1313.205357142857, most: 1311.3720703125}},
	"seeding a quote of rules/Update":                            {128, countedReading{past: 411, most: 416.3984375}},
	"seeding paragraphs/Update":                                  {128, countedReading{past: 906, most: 908.0546875}},
}

// alternatingBlocks uses a fourfold step for the index walks this shape cannot avoid.
var alternatingBlocks = [2]int{512, 2048}

// alternatingAllowance permits that many items reached per block beside ygo's own walks, and
// that many times ygo's counted statements for Update with its read-back.
const alternatingAllowance = 4

// alternatingFloor and alternatingCeiling bound the counted statements per reached item allowed.
// At the same reading as updateReadings, 512 blocks took 14.957489 statements per allowed item
// and 2,048 took 12.378854. The bounds are a quarter of the lower and four times the higher.
const (
	alternatingFloor   = 12.378854 / countedMargin
	alternatingCeiling = 14.957489 * countedMargin
)

// alternatingShape replaces each heading between two kept paragraphs as a run of its own.
func alternatingShape() countedShape {
	leaf := linearShape("replacing every other block", "Update", "block")
	leaf.sizes = alternatingBlocks
	leaf.limitChildren = true
	seed := func(t *testing.T, blocks int) (*crdt.Doc, *crdt.YXmlFragment, *Node) {
		doc, fragment := writeTree(t, repeatedBlocks(t, "#\n\na\n\n", blocks/2), crdt.WithClientID(1))
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
