package api

import (
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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
