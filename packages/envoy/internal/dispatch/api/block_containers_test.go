package api

import (
	"fmt"
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

// placeRequest is the prefix of each of texts, the stored texts of document blocks one request
// of the batched reference read names, where outer is the stored text of their one outermost
// container, as blockTrees answers them: the blocks named in the answer refs.OutermostContainers
// gives, each held by that one container, are placed by the placement blockTrees makes
// (containerPlacements). It holds that answer and that loop and nothing more, so that it stands for
// how a request places its blocks, and a change to that placement changes no line of it.
func placeRequest(outer string, texts []string) []string {
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
	placements := newContainerPlacements(outermost)
	prefixes := make([]string, len(texts))
	for index, text := range texts {
		prefixes[index], _ = placements.prefix(blocks[index], text)
	}
	return prefixes
}

// A request places each block of one container as it places the block alone: the 256 paragraphs
// of mebibyteListOfParagraphs, the shape of review 6098707764's thread on placedPrefix, placed
// together as one request of the batched reference read places them (placeRequest), each take the
// prefix placedPrefix gives the paragraph placed alone, which is its item's two columns, and no
// paragraph placed alone is ambiguous. It reads no database.
func TestARequestPlacesEachBlockOfOneContainerAsItPlacesItAlone(t *testing.T) {
	t.Parallel()
	list, texts := mebibyteListOfParagraphs()
	prefixes := placeRequest(list, texts)
	if len(prefixes) != len(texts) {
		t.Fatalf("the request answers %d prefixes for %d paragraphs", len(prefixes), len(texts))
	}
	for index, text := range texts {
		alone, ambiguous := placedPrefix(list, text)
		if prefixes[index] != alone || alone != "  " || ambiguous {
			t.Fatalf("paragraph %d takes %q placed with the request and %q (ambiguous %t) placed alone, want %q, its item's two columns, both ways and not ambiguous",
				index, prefixes[index], alone, ambiguous, "  ")
		}
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
// one call of placeRequest, which must answer each paragraph its item's two columns. The list is
// made and measured outside the timed loop, and the benchmark fails where an upload refuses it for
// its bytes or its elements, so what it times is a placement one request can ask of a read of a
// document one upload can store.
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
		prefixes := placeRequest(shape.list, shape.texts)
		if len(prefixes) != len(shape.texts) {
			b.Fatalf("the request answers %d prefixes for %d paragraphs", len(prefixes), len(shape.texts))
		}
		for index, prefix := range prefixes {
			if prefix != "  " {
				b.Fatalf("the prefix of paragraph %d is %q, want %q, its item's two columns", index, prefix, "  ")
			}
		}
	}
}
