package refs

import "testing"

// TestABlockIsTypedOnlyByTheTypedBlockItsTextOpensAsItsOwn holds TreeNodeType to the rule that a
// block is the typed block its text opens only where that block is its own, the id the opening
// writes being the block's own id, the part of its address after the first '#'. A quote, a list and
// a list item whose first block is a typed block open with that block's line, written after their
// markers, and are taken by their supertag, a quote whose id ends in '#' and that block's id too;
// the typed block itself is taken by its type, whether its own text opens with its line alone or
// with the markers it is written after, and whatever its id holds. An opening naming no id, or a
// type the schema does not declare, types nothing either.
func TestABlockIsTypedOnlyByTheTypedBlockItsTextOpensAsItsOwn(t *testing.T) {
	for _, test := range []struct {
		name, address, opening, want string
	}{
		{"a quote whose first block is a requirement", "CORE-1/tree#q1", "> :::requirement{#r3}", BlockSupertag},
		{"a list whose first item opens with a requirement", "CORE-1/tree#l1", "- :::requirement{#r4}", BlockSupertag},
		{"a list item that opens with a requirement", "CORE-1/tree#i1", "- :::requirement{#r4}", BlockSupertag},
		{"a numbered list item that opens with a check", "CORE-1/tree#i2", "1. :::check{#c1}", BlockSupertag},
		{"a footnote definition that opens with a requirement", "CORE-1/tree#f1", "[^1]: :::requirement{#r5}", BlockSupertag},
		{"a quote nested in a list item, over a requirement", "CORE-1/tree#q2", "- > :::requirement{#r6}", BlockSupertag},
		{"a requirement in a quote, by its own line", "CORE-1/tree#r3", ":::requirement{#r3}", "requirement"},
		{"a requirement in a quote, by its line after the marker", "CORE-1/tree#r3", "> :::requirement{#r3}", "requirement"},
		{"a requirement first in a list item", "CORE-1/tree#r4", "- :::requirement{#r4}", "requirement"},
		{"an indented requirement deeper in a list", "CORE-1/tree#r4", "    :::requirement{#r4}", "requirement"},
		{"a check first in a numbered list item", "CORE-1/tree#c1", "1. :::check{#c1}", "check"},
		{"a requirement first in a footnote definition", "CORE-1/tree#r5", "[^1]: :::requirement{#r5}", "requirement"},
		{"a requirement whose id is written id=\"…\"", "CORE-1/tree#r7", `:::requirement{id="r7"}`, "requirement"},
		{"a block whose id only ends the same", "CORE-1/tree#ar1", ":::requirement{#r1}", BlockSupertag},
		{"a quote whose id ends in '#' and its first block's id", "CORE-1/tree#q3#r3", "> :::requirement{#r3}", BlockSupertag},
		{"a requirement whose id holds a '#'", "CORE-1/tree#q3#r3", `:::requirement{id="q3#r3"}`, "requirement"},
		{"an opening that names no id", "CORE-1/tree#r8", ":::requirement{}", BlockSupertag},
		{"a line that opens no directive", "CORE-1/tree#r8", ":::requirement", BlockSupertag},
		{"a type the schema does not declare", "CORE-1/tree#x1", ":::nonesuch{#x1}", BlockSupertag},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := TreeNodeType(BlockNodeKind, test.address, BlockSupertag, test.opening); got != test.want {
				t.Errorf("TreeNodeType of %s opening %q is %q, want %q", test.address, test.opening, got, test.want)
			}
		})
	}
	// A node that is no block is taken by its supertag whatever its text opens with, and by its kind
	// where the store holds no row of it.
	if got := TreeNodeType("comment", "c-1", "comment", ":::requirement{#c-1}"); got != "comment" {
		t.Errorf("a comment opening with a requirement's line is taken by %q, want comment", got)
	}
	if got := TreeNodeType("ask", "a-1", "", ""); got != "ask" {
		t.Errorf("a node with no row is taken by %q, want its kind ask", got)
	}
}
