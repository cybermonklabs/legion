package api

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// A container's prefix is what the containers of a document's block write before each of the
// block's lines after the first, read from the block's lead, the bytes before its first byte on
// the line it starts on (containerPrefix). Each space of the lead is kept, a quote's `>` is kept
// with the one space after it where it has one, a list item's marker and the space after it are
// written as as many spaces, a task item's checkbox after the marker as nothing, and a footnote
// definition's label, whose `]` the renderer escapes, as the definition's four columns. A lead
// holding any other byte is no container's, and is refused. It reads no database.
func TestAContainersPrefixIsWhatItsLeadWritesBeforeEachLaterLine(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, lead, prefix string
		ok                 bool
	}{
		{"no lead, a block at the start of its line", "", "", true},
		{"a list item's content indentation", "  ", "  ", true},
		{"a quote's `>` and its space", "> ", "> ", true},
		{"a quote's `>` with no space after it", ">", ">", true},
		{"a bullet item's marker", "- ", "  ", true},
		{"an ordered item's marker of two digits", "10. ", "    ", true},
		{"an ordered item's marker closed by `)`", "3) ", "   ", true},
		{"a task item's marker and checkbox", "- [ ] ", "  ", true},
		{"a list item in a quote", "> - ", ">   ", true},
		{"an ordered item in a quote two columns into a list item", "  > 1. ", "  >    ", true},
		{"a footnote definition's label holding an escaped `]`", "[^a\\]b]: ", "    ", true},
		{"a footnote definition's label two columns into a list item", "  [^1]: ", "      ", true},
		{"a lead holding a byte no container writes", "x ", "", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			prefix, ok := containerPrefix(row.lead)
			if prefix != row.prefix || ok != row.ok {
				t.Fatalf("containerPrefix(%q) = %q, %t, want %q, %t", row.lead, prefix, ok, row.prefix, row.ok)
			}
		})
	}
}

// placedPrefix is what the containers of a document's block write before each of its lines after
// the first, and whether the block is ambiguous, where outer is the stored text of the block's
// outermost container, the block at the document's top level that holds it, and text is the
// block's own stored text: text placed in the placement index of outer made for text alone
// (newContainerIndex), with a budget no placement passes, so that containerIndex.place never stops
// for its budget and never answers unplaced. It is "" and not ambiguous where text holds one line,
// which has no later line, and where no place's bytes are text's.
//
// The block is ambiguous where its bytes stand at two or more starts that container syntax allows
// in outer, each up to where a line of outer ends, and two of those starts write different
// prefixes, before its own or after it: outer alone cannot tell which start is the block's, since
// the container can hold the block's bytes after another lead too, as in the code of a code block,
// and prefix is then the first start's, which the block's read replaces with the one its
// document's parse gives it (calledFootnotes, rangedPrefix). The block's own start is one of them,
// so where every start writes one prefix, that prefix is the block's, the only one the parse could
// answer, and the block is not ambiguous.
func placedPrefix(outer, text string) (prefix string, ambiguous bool) {
	unlimited := math.MaxInt
	prefix, ambiguous, _ = newContainerIndex(outer).place(text, &unlimited)
	return prefix, ambiguous
}

// listWhoseLinesRepeatItsBlock is a top-level list whose lines repeat the text of the block it
// places, and that block, the list's second item. The first item is `xA\` and then 2r - 2 lines,
// and the second is `zA\` and then r - 1 lines, each of those lines two spaces, run `z` and `A`.
// Every line but the second item's last ends with a backslash: a hard break inside its item's one
// paragraph, and on the first item's last line, where that paragraph ends, the backslash itself.
// The list is the block's one container, and the block's bytes stand in it r - 1 times before its
// own occurrence, each from the last `z` of one of the first item's lines, after a lead of two
// spaces and run - 1 `z`, which no container writes. Its own occurrence follows the second item's
// marker `- `, whose prefix is the item's two columns.
//
// As an upload measures the list (pmdoc.MeasureDocument), with r = 1,000 and a run of 30 it is
// 104,905 bytes and 12,005 elements, and with r = 5,000 and a run of 63 it is 1,019,806 bytes and
// 60,005 elements.
func listWhoseLinesRepeatItsBlock(r, run int) (list, block string) {
	line := "\n  " + strings.Repeat("z", run) + "A"
	block = "zA\\" + strings.Repeat(line+"\\", r-2) + line
	return "- xA\\" + strings.Repeat(line+"\\", 2*r-2) + "\n- " + block, block
}

// A block placed in a container whose lines repeat its text takes its own prefix: the second item
// of listWhoseLinesRepeatItsBlock with r = 1,000 and a run of 30 `z`. Its bytes stand in the list
// 999 times before its own occurrence, each after a lead of two spaces and `z`s, which no
// container writes, so the block is placed at its own occurrence, after the marker `- `, and its
// prefix is the item's two columns (placedPrefix). It reads no database.
func TestABlockInAContainerWhoseLinesRepeatItsTextTakesItsOwnPrefix(t *testing.T) {
	t.Parallel()
	list, block := listWhoseLinesRepeatItsBlock(1_000, 30)
	if prefix, _ := placedPrefix(list, block); prefix != "  " {
		t.Fatalf("the prefix of the second item's block of %d bytes in the list of %d bytes is %q, want %q, the item's two columns",
			len(block), len(list), prefix, "  ")
	}
}

// A block is ambiguous only where two of the starts that its bytes stand at in its outermost
// container's text, each after a lead that container syntax allows, write different prefixes
// (placedPrefix): its prefix is then the first start's, which its read replaces with the one its
// document's parse gives it. Where every start writes one prefix, the block's own start among
// them, that prefix is the block's and the block is not ambiguous, so its read spends none of the
// request's documents on a parse that could answer only that prefix. The rows are a list whose two
// items each hold the same code block, of two lines of code, two columns in, which answers the
// items' two columns and is not ambiguous; the shape of row 12 of check c-open-fence, a quote
// holding a code block fenced with four backticks whose code is `> ` and then the block, before
// the block itself, which answers the prefix `> > ` of the first start and is ambiguous; a quote
// holding the block twice and then that code block, whose third start alone writes another
// prefix, which answers the quote's `> ` and is ambiguous, since the walk goes on past a start
// that writes the first start's prefix; and a quote holding the block at one start alone, which
// answers the quote's `> ` and is not ambiguous. It reads no database.
func TestABlockIsAmbiguousOnlyWhereItsStartsWriteDifferentPrefixes(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name, outer, text, prefix string
		ambiguous                 bool
	}{
		{name: "a list whose two items each hold the same code block two columns in",
			outer:  "- The first item.\n\n  ```\n  first code line\n  second code line\n  ```\n\n- The second item.\n\n  ```\n  first code line\n  second code line\n  ```",
			text:   "```\n  first code line\n  second code line\n  ```",
			prefix: "  "},
		{name: "row 12's quote, whose code block fenced with four backticks holds the block after `> > `",
			outer:  "> ````\n> > ```\n> > The code line.\n> ```\n> ````\n>\n> ```\n> > The code line.\n> ```",
			text:   "```\n> > The code line.\n> ```",
			prefix: "> > ", ambiguous: true},
		{name: "a quote holding the block twice after `> ` and then in a code block's code after `> > `",
			outer:  "> ```\n> > The code line.\n> ```\n>\n> ```\n> > The code line.\n> ```\n>\n> ````\n> > ```\n> > The code line.\n> ```\n> ````",
			text:   "```\n> > The code line.\n> ```",
			prefix: "> ", ambiguous: true},
		{name: "a quote holding the block at one start alone",
			outer:  "> The quote's paragraph.\n>\n> ```\n> The code line.\n> ```",
			text:   "```\n> The code line.\n> ```",
			prefix: "> "},
	} {
		t.Run(row.name, func(t *testing.T) {
			prefix, ambiguous := placedPrefix(row.outer, row.text)
			if prefix != row.prefix || ambiguous != row.ambiguous {
				t.Fatalf("placedPrefix(%q, %q) = %q, %t, want %q, %t", row.outer, row.text, prefix, ambiguous, row.prefix, row.ambiguous)
			}
		})
	}
}

// mebibyteList is the list BenchmarkABlockPlacedInAMebibyteContainerWhoseLinesRepeatItsText
// places its block in, and that block, listWhoseLinesRepeatItsBlock with r = 5,000 and a run of
// 63 `z`, with the list's size as an upload measures it (pmdoc.MeasureDocument): made and
// measured once for the whole run, by the benchmark's first call, which logs that size.
var mebibyteList struct {
	sync.Once
	list, block string
	size        pmdoc.DocumentSize
}

// BenchmarkABlockPlacedInAMebibyteContainerWhoseLinesRepeatItsText times one placement of the
// second item of listWhoseLinesRepeatItsBlock with r = 5,000 and a run of 63 `z`, a list of about
// a mebibyte whose first item's lines hold the block's bytes 4,999 times before its own
// occurrence. Each operation is one call of placedPrefix, which must answer the item's two
// columns. The list is made and measured outside the timed loop, and the benchmark fails where an
// upload refuses it for its bytes or its elements, so what it times is a placement one upload can
// ask of a read.
func BenchmarkABlockPlacedInAMebibyteContainerWhoseLinesRepeatItsText(b *testing.B) {
	shape := &mebibyteList
	shape.Do(func() {
		shape.list, shape.block = listWhoseLinesRepeatItsBlock(5_000, 63)
		shape.size = pmdoc.MeasureDocument(shape.list)
		b.Logf("the list is %d bytes and %d elements (counted %t)", shape.size.Bytes, shape.size.Elements, shape.size.Counted)
	})
	if shape.size.TooLong() || shape.size.TooHeavy() {
		b.Fatalf("the list is %d bytes and %d elements (counted %t), past the %d bytes and %d elements one upload may send",
			shape.size.Bytes, shape.size.Elements, shape.size.Counted, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if prefix, _ := placedPrefix(shape.list, shape.block); prefix != "  " {
			b.Fatalf("the prefix of the second item's block is %q, want %q, the item's two columns", prefix, "  ")
		}
	}
}

// mebibyteListOfParagraphs is the shape of review 6098707764's thread on placedPrefix: one
// top-level list of 256 items, each item one paragraph of two lines, and the stored texts of the
// 256 paragraphs, each as the block index stores it, from the first byte after its item's marker
// `- ` to its last, so that its second line carries the two spaces the item writes before it. Each
// line of the paragraph of item n, counted from 0, is `No.`, a space, n in three digits and then
// 406 words of four letters, each after a space, 2,037 bytes in all. The first line ends with a
// backslash, a hard break, so the renderer writes the paragraph as those two lines. Each paragraph
// is 4,078 bytes and each item 4,080, and no paragraph's bytes stand in the list but after its own
// item's marker: every other item's lines carry another number, and from the start of its own
// second line, which holds its first line's bytes, the list holds a newline, or ends, where the
// paragraph holds its backslash.
//
// As an upload measures the list (pmdoc.MeasureDocument), it is 1,044,735 bytes and 3,331
// elements.
func mebibyteListOfParagraphs() (list string, texts []string) {
	words := strings.Fields("this item says once more what each line says")
	items := make([]string, 256)
	texts = make([]string, len(items))
	for n := range texts {
		var line strings.Builder
		fmt.Fprintf(&line, "No. %03d", n)
		for index := range 406 {
			line.WriteByte(' ')
			line.WriteString(words[index%len(words)])
		}
		texts[n] = line.String() + "\\\n  " + line.String()
		items[n] = "- " + texts[n]
	}
	return strings.Join(items, "\n"), texts
}

// requestPlacements is the placement of each of texts, the stored texts of document blocks one
// request of the batched reference read names, where outer is the stored text of their one
// outermost container, as blockTrees places them before it reads any document: the blocks named
// in the answer refs.OutermostContainers gives, each held by that one container, are placed in the
// order of texts, with one budget for the request, by the placement blockTrees makes
// (containerPlacements). It holds that answer and that call and nothing more, so that it stands for
// how a request places its blocks, and a change to that placement changes no line of it.
func requestPlacements(outer string, texts []string) []blockPlacement {
	holder := refs.DocumentBlock{ArtifactID: "document", BlockID: "list"}
	outermost := refs.Outermost{
		Holders: make(map[refs.DocumentBlock]refs.DocumentBlock, len(texts)),
		Texts:   map[refs.DocumentBlock]string{holder: outer},
	}
	blocks := make([]refs.DocumentBlock, len(texts))
	for index := range texts {
		blocks[index] = refs.DocumentBlock{ArtifactID: holder.ArtifactID, BlockID: "paragraph-" + strconv.Itoa(index)}
		outermost.Holders[blocks[index]] = holder
	}
	left := placementWorkMax
	return containerPlacements(outermost, blocks, texts, &left)
}

// placeRequest is the prefix of each of texts and whether each is ambiguous, where one request
// places them in outer (requestPlacements).
func placeRequest(outer string, texts []string) (prefixes []string, ambiguous []bool) {
	placed := requestPlacements(outer, texts)
	prefixes, ambiguous = make([]string, len(texts)), make([]bool, len(texts))
	for index, placement := range placed {
		prefixes[index], ambiguous[index] = placement.prefix, placement.ambiguous
	}
	return prefixes, ambiguous
}

// A request places each block of one container as it places the block alone, and each block
// takes the prefix and the ambiguity written for it here, whatever order the request names the
// blocks in. The container is one top-level list, written as the renderer writes it, whose blocks
// differ in their text's length, their first line's length, their prefix and their ambiguity: an
// item's paragraph of two lines, which takes the item's two columns; the paragraph of an item of a
// list inside an item, which takes four; the paragraph of an item of an ordered list inside an
// item, which takes five; a quote's paragraph inside an item, which takes `  > `; and a code block
// in a quote inside the last item, whose bytes the list holds before it too, in the code of a code
// block in a quote inside the second item, after the code's `> `. That copy's start writes
// `  > > ` and the block's own start `  > `, so the block takes the first start's prefix,
// `  > > `, and is ambiguous: its read takes `  > ` from its document's parse instead. Each copy
// of a block's bytes, and each block's own place, ends where a line ends. The blocks are placed
// together as one request of the batched reference read places them (placeRequest), in the list's
// order and again in the reverse order, and each alone (placedPrefix), and each answer is checked
// against the prefix and the ambiguity written for its block, never against another answer of the
// same code. It reads no database.
func TestARequestPlacesEachBlockOfOneContainerAsItPlacesItAlone(t *testing.T) {
	t.Parallel()
	list := "- The item's first line, a hard break after it.\\\n" +
		"  The item's second line.\n" +
		"\n" +
		"- A quote holding code:\n" +
		"\n" +
		"  > ````\n" +
		"  > > ```\n" +
		"  > The code line.\n" +
		"  > ```\n" +
		"  > ````\n" +
		"\n" +
		"- An item holding a list.\n" +
		"\n" +
		"  - The nested item's first line.\\\n" +
		"    And its second line, which is longer than the first.\n" +
		"\n" +
		"- An item holding an ordered list.\n" +
		"\n" +
		"  1. Ordered\\\n" +
		"     line.\n" +
		"\n" +
		"- > The quote's first line\\\n" +
		"  > and the quote's last line, the longest line of the list.\n" +
		"\n" +
		"- The quoted code itself:\n" +
		"\n" +
		"  > ```\n" +
		"  > The code line.\n" +
		"  > ```"
	blocks := []struct {
		name, text, prefix string
		ambiguous          bool
	}{
		{name: "the first item's paragraph",
			text:   "The item's first line, a hard break after it.\\\n  The item's second line.",
			prefix: "  "},
		{name: "the paragraph of the nested list's item",
			text:   "The nested item's first line.\\\n    And its second line, which is longer than the first.",
			prefix: "    "},
		{name: "the paragraph of the nested ordered list's item",
			text:   "Ordered\\\n     line.",
			prefix: "     "},
		{name: "the paragraph of the quote in the fifth item",
			text:   "The quote's first line\\\n  > and the quote's last line, the longest line of the list.",
			prefix: "  > "},
		{name: "the code block in the last item's quote, which the second item's quoted code holds after `> `",
			text:   "```\n  > The code line.\n  > ```",
			prefix: "  > > ", ambiguous: true},
	}
	texts := make([]string, len(blocks))
	for index, block := range blocks {
		texts[index] = block.text
	}
	check := func(way string, index int, prefix string, ambiguous bool) {
		t.Helper()
		if block := blocks[index]; prefix != block.prefix || ambiguous != block.ambiguous {
			t.Errorf("%s takes %q (ambiguous %t) %s, want %q (ambiguous %t)",
				block.name, prefix, ambiguous, way, block.prefix, block.ambiguous)
		}
	}
	prefixes, ambiguous := placeRequest(list, texts)
	if len(prefixes) != len(texts) || len(ambiguous) != len(texts) {
		t.Fatalf("the request answers %d prefixes and %d flags for %d blocks", len(prefixes), len(ambiguous), len(texts))
	}
	for index := range blocks {
		check("placed with the request in the list's order", index, prefixes[index], ambiguous[index])
	}
	reversed := slices.Clone(texts)
	slices.Reverse(reversed)
	prefixes, ambiguous = placeRequest(list, reversed)
	if len(prefixes) != len(texts) || len(ambiguous) != len(texts) {
		t.Fatalf("the request in the reverse order answers %d prefixes and %d flags for %d blocks", len(prefixes), len(ambiguous), len(texts))
	}
	for at := range reversed {
		check("placed with the request in the reverse order", len(blocks)-1-at, prefixes[at], ambiguous[at])
	}
	for index, block := range blocks {
		prefix, alone := placedPrefix(list, block.text)
		check("placed alone", index, prefix, alone)
	}
}

// paragraphList is the list BenchmarkARequestPlacing256BlocksOfAMebibyteList places the
// paragraphs of, and their texts, mebibyteListOfParagraphs, with the list's size as an upload
// measures it (pmdoc.MeasureDocument): made and measured once for the whole run, by the
// benchmark's first call, which logs that size.
var paragraphList struct {
	sync.Once
	list  string
	texts []string
	size  pmdoc.DocumentSize
}

// BenchmarkARequestPlacing256BlocksOfAMebibyteList times the placement of the 256 paragraphs of
// mebibyteListOfParagraphs, a list of about a mebibyte, as one request of the batched reference
// read places them: the shape of review 6098707764's thread on placedPrefix. Each operation is
// one call of placeRequest, which must answer each paragraph its item's two columns and none of
// them ambiguous. The list is made and measured outside the timed loop, and the benchmark fails
// where an upload refuses it for its bytes or its elements, so what it times is a placement one
// request can ask of a read of a document one upload can store.
func BenchmarkARequestPlacing256BlocksOfAMebibyteList(b *testing.B) {
	shape := &paragraphList
	shape.Do(func() {
		shape.list, shape.texts = mebibyteListOfParagraphs()
		shape.size = pmdoc.MeasureDocument(shape.list)
		b.Logf("the list is %d bytes and %d elements (counted %t)", shape.size.Bytes, shape.size.Elements, shape.size.Counted)
	})
	if shape.size.TooLong() || shape.size.TooHeavy() {
		b.Fatalf("the list is %d bytes and %d elements (counted %t), past the %d bytes and %d elements one upload may send",
			shape.size.Bytes, shape.size.Elements, shape.size.Counted, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		prefixes, ambiguous := placeRequest(shape.list, shape.texts)
		if len(prefixes) != len(shape.texts) || len(ambiguous) != len(shape.texts) {
			b.Fatalf("the request answers %d prefixes and %d flags for %d paragraphs", len(prefixes), len(ambiguous), len(shape.texts))
		}
		for index, prefix := range prefixes {
			if prefix != "  " || ambiguous[index] {
				b.Fatalf("the prefix of paragraph %d is %q (ambiguous %t), want %q, its item's two columns, and not ambiguous", index, prefix, ambiguous[index], "  ")
			}
		}
	}
}

// listWithManyCopies is the shape `copies` of review 6099364022's thread on containerIndex.place:
// a top-level list whose first item is a fenced code block of 110,000 copies of a paragraph's
// text, each the code lines `a\` and `b`, which the item writes two columns in, followed by 256
// items, each the paragraph `a\` and `  b`, and the stored texts of those 256 paragraphs, each as
// the block index stores it, from the first byte after its item's marker `- ` to its last. Each
// copy stands after the code block's two columns, which write the prefix the paragraphs' own items
// write, and ends where its second line ends, so each copy is a place of every paragraph's bytes
// whose lead writes the paragraph's own prefix: a placement that tests each of them finds no place
// that writes another prefix before the list's last line, for every paragraph.
//
// As an upload measures the list's document (pmdoc.MeasureDocument), the list and the newline the
// renderer writes after a document's last line, it is 992,316 bytes and 2,828 elements.
func listWithManyCopies() (list string, texts []string) {
	texts = make([]string, 256)
	for index := range texts {
		texts[index] = "a\\\n  b"
	}
	return "- ```\n" + strings.Repeat("  a\\\n  b\n", 110_000) + "  ```" + strings.Repeat("\n- a\\\n  b", len(texts)), texts
}

// listWithSharedEnds is the shape `shared-ends` of review 6099364022's thread on
// containerIndex.place: a top-level list whose first item is a fenced code block of 40,254 lines
// of code, each `the back front of xyz\`, which the item writes two columns in, followed by 256
// items, the item n, counted from 0, the paragraph `front of `, n in three digits and `\`, and
// then `  the back`, and the stored texts of those 256 paragraphs, each as the block index stores
// it, from the first byte after its item's marker `- ` to its last. The paragraphs are distinct
// and share the length of their first line, their length, their first 8 bytes and their last 8.
// Each line of code but the last ends in `front of xyz\` and the next starts with `the back` after
// the item's two columns, so on each line of code but the last the window of a paragraph's length
// from where its first line would start, which is past the line's two columns, starts with the
// paragraphs' first 8 bytes and ends with their last 8, though `xyz` is no paragraph's number: a
// placement that compares the bytes at a window's ends before the rest finds them the same at
// each of those 40,253 lines for every paragraph, and no copy of any paragraph there.
//
// As an upload measures the list's document (pmdoc.MeasureDocument), the list and the newline the
// renderer writes after a document's last line, it is 1,013,274 bytes and 3,340 elements.
func listWithSharedEnds() (list string, texts []string) {
	texts = make([]string, 256)
	var items strings.Builder
	for index := range texts {
		texts[index] = fmt.Sprintf("front of %03d\\\n  the back", index)
		items.WriteString("\n- " + texts[index])
	}
	return "- ```\n" + strings.Repeat("  the back front of xyz\\\n", 40_254) + "  ```" + items.String(), texts
}

// listWithDistinctCopies is the shape `distinct-copies` of check c-placed-crafted-cost: a
// top-level list whose first item is a fenced code block of 82,944 copies of the texts of 256
// distinct paragraphs, 324 of each, in turn, each the code lines `aNNN\` and `b`, which the item
// writes two columns in, where NNN runs from 001 to 256, followed by 256 items, the item of NNN
// the paragraph `aNNN\` and `  b`, and the stored texts of those 256 paragraphs, each as the block
// index stores it, from the first byte after its item's marker `- ` to its last. Each copy stands
// after the code block's two columns, which write the prefix the paragraphs' own items write, and
// ends where its second line ends. The later line of each copy and of each paragraph is `  b`, so
// a placement that tests a window's later lines before its first finds them agree at every copy
// for every paragraph, and only the first line, `aNNN\`, tells the copies of one paragraph from
// those of the others.
//
// As an upload measures the list's document (pmdoc.MeasureDocument), the list and the newline the
// renderer writes after a document's last line, it is 998,412 bytes and 2,828 elements.
func listWithDistinctCopies() (list string, texts []string) {
	texts = make([]string, 256)
	var copies, items strings.Builder
	for index := range texts {
		texts[index] = fmt.Sprintf("a%03d\\\n  b", index+1)
		items.WriteString("\n- " + texts[index])
	}
	for index := range 324 * len(texts) {
		copies.WriteString("  " + texts[index%len(texts)] + "\n")
	}
	return "- ```\n" + copies.String() + "  ```" + items.String(), texts
}

// placeRequestAsDocument is the prefix each of texts takes where one request of the batched
// reference read names their blocks, where markdown is their document's markdown and outer the
// stored text of their one outermost container: the prefix one request places each at
// (requestPlacements), and, for each that the placement leaves to its document's parse, an
// ambiguous block or one whose placement passed the request's budget, the prefix of the first
// nested block of that parse whose bytes are its text where one is (rangedPrefix), as blockTrees
// resolves it through calledFootnotes, which parses the document once, with its nested blocks'
// ranges, for all of them (footnoteParseOf). A text the parse gives no range keeps the
// placement's prefix. It holds that resolution and nothing more, so that it stands for how the
// batched read resolves a request's blocks: a change to which blocks the placement leaves to the
// parse changes its body alone, and no case that calls it.
func placeRequestAsDocument(markdown, outer string, texts []string) []string {
	placed := requestPlacements(outer, texts)
	prefixes := make([]string, len(texts))
	var nested [][2]int
	parsed := false
	for index, placement := range placed {
		prefixes[index] = placement.prefix
		if !placement.ambiguous && !placement.unplaced {
			continue
		}
		if !parsed {
			nested, parsed = footnoteParseOf(markdown, "document", true).Nested, true
		}
		if prefix, found := rangedPrefix(markdown, nested, texts[index]); found {
			prefixes[index] = prefix
		}
	}
	return prefixes
}

// manyCopiesShape is one shape TestARequestPlacesBlocksWithManyCopiesAsTheirDocumentDoes and
// BenchmarkARequestPlacingBlocksWithManyCopiesInOneContainer place: its name, which names the
// subtest and the sub-benchmark, and build, which makes the list and the stored texts of its 256
// paragraphs. The benchmark makes the shape once for the whole run, by its first call of it, which
// logs the document's size: list and texts; document, the list's document, the list and the
// newline the renderer writes after a document's last line; size, the document's size as an
// upload measures it (pmdoc.MeasureDocument); and parsed, the prefix of each paragraph that the
// document's parse gives it (footnoteParseOf, rangedPrefix), "" for a paragraph it gives no
// range, each of which unranged counts.
type manyCopiesShape struct {
	name  string
	build func() (list string, texts []string)

	once     sync.Once
	list     string
	texts    []string
	document string
	size     pmdoc.DocumentSize
	parsed   []string
	unranged int
}

// manyCopiesShapes are the shapes of check c-placed-crafted-cost, in order: copies
// (listWithManyCopies), shared-ends (listWithSharedEnds) and distinct-copies
// (listWithDistinctCopies).
var manyCopiesShapes = []*manyCopiesShape{
	{name: "copies", build: listWithManyCopies},
	{name: "shared-ends", build: listWithSharedEnds},
	{name: "distinct-copies", build: listWithDistinctCopies},
}

// A request places blocks whose bytes, or their ends alone, their container's code repeats many
// times as their document's parse places them: the shapes of check c-placed-crafted-cost
// (manyCopiesShapes), the subtests copies (listWithManyCopies) and shared-ends
// (listWithSharedEnds), of review 6099364022's thread on containerIndex.place, and
// distinct-copies (listWithDistinctCopies). Each shape's document is its list and the newline the
// renderer writes after a document's last line, so the list is the document's one top-level block
// and the stored text of its paragraphs' outermost container, which is the whole markdown but that
// newline. Each document is within the bytes and the elements one upload may send, as an upload
// measures it (pmdoc.MeasureDocument). The parse of the document (footnoteParseOf) gives each of
// its 256 paragraphs a range whose lead writes the item's two columns (rangedPrefix), and each
// paragraph, placed together with the others as one request of the batched reference read places
// and resolves them (placeRequestAsDocument), takes that prefix, whether the placement answers it
// or leaves it to the parse. It reads no database.
func TestARequestPlacesBlocksWithManyCopiesAsTheirDocumentDoes(t *testing.T) {
	t.Parallel()
	for _, shape := range manyCopiesShapes {
		t.Run(shape.name, func(t *testing.T) {
			list, texts := shape.build()
			markdown := list + "\n"
			size := pmdoc.MeasureDocument(markdown)
			t.Logf("the document is %d bytes and %d elements (counted %t)", size.Bytes, size.Elements, size.Counted)
			if size.TooLong() || size.TooHeavy() {
				t.Fatalf("the document is %d bytes and %d elements (counted %t), past the %d bytes and %d elements one upload may send",
					size.Bytes, size.Elements, size.Counted, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements)
			}
			nested := footnoteParseOf(markdown, "document", true).Nested
			parsed := make([]string, len(texts))
			for index, text := range texts {
				prefix, found := rangedPrefix(markdown, nested, text)
				if !found || prefix != "  " {
					t.Fatalf("the parse gives paragraph %d, %q, the prefix %q (found %t), want %q, its item's two columns",
						index, text, prefix, found, "  ")
				}
				parsed[index] = prefix
			}
			prefixes := placeRequestAsDocument(markdown, list, texts)
			if len(prefixes) != len(texts) {
				t.Fatalf("the request answers %d prefixes for %d paragraphs", len(prefixes), len(texts))
			}
			for index, prefix := range prefixes {
				if prefix != parsed[index] {
					t.Errorf("paragraph %d, %q, takes %q placed with the request, want %q, the prefix its document's parse gives it",
						index, texts[index], prefix, parsed[index])
				}
			}
		})
	}
}

// BenchmarkARequestPlacingBlocksWithManyCopiesInOneContainer times the placement of the 256
// paragraphs of each shape of check c-placed-crafted-cost (manyCopiesShapes) as one request of the
// batched reference read places and resolves them, in its sub-benchmarks copies, shared-ends and
// distinct-copies. Each operation is one call of placeRequestAsDocument, which must answer each
// paragraph the prefix its document's parse gives it. Each shape's document is made, measured and
// parsed outside the timed loop, the benchmark fails where an upload refuses the document for its
// bytes or its elements or where the parse gives a paragraph no range, and the parse is kept
// (footnoteParseOf) before the timer starts, so what it times is the placement one request can ask
// of a read of a document one upload can store, with that document's parse kept.
func BenchmarkARequestPlacingBlocksWithManyCopiesInOneContainer(b *testing.B) {
	for _, shape := range manyCopiesShapes {
		b.Run(shape.name, func(b *testing.B) {
			shape.once.Do(func() {
				shape.list, shape.texts = shape.build()
				shape.document = shape.list + "\n"
				shape.size = pmdoc.MeasureDocument(shape.document)
				b.Logf("the document is %d bytes and %d elements (counted %t)", shape.size.Bytes, shape.size.Elements, shape.size.Counted)
				nested := footnoteParseOf(shape.document, "document", true).Nested
				shape.parsed = make([]string, len(shape.texts))
				for index, text := range shape.texts {
					prefix, found := rangedPrefix(shape.document, nested, text)
					if !found {
						shape.unranged++
					}
					shape.parsed[index] = prefix
				}
			})
			if shape.size.TooLong() || shape.size.TooHeavy() {
				b.Fatalf("the document is %d bytes and %d elements (counted %t), past the %d bytes and %d elements one upload may send",
					shape.size.Bytes, shape.size.Elements, shape.size.Counted, pmdoc.MaxDocumentBytes, pmdoc.MaxDocumentElements)
			}
			if shape.unranged > 0 {
				b.Fatalf("the parse of the document gives %d of its %d paragraphs no range", shape.unranged, len(shape.texts))
			}
			footnoteParseOf(shape.document, "document", true)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				prefixes := placeRequestAsDocument(shape.document, shape.list, shape.texts)
				if len(prefixes) != len(shape.texts) {
					b.Fatalf("the request answers %d prefixes for %d paragraphs", len(prefixes), len(shape.texts))
				}
				for index, prefix := range prefixes {
					if prefix != shape.parsed[index] {
						b.Fatalf("paragraph %d takes %q placed with the request, want %q, the prefix its document's parse gives it", index, prefix, shape.parsed[index])
					}
				}
			}
		})
	}
}
