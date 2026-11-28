package api

import "testing"

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
