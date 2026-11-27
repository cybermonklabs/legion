package pmdoc

import (
	"reflect"
	"testing"
)

// TestAQuoteInABlockMatchesAsTheWholeTextDoes holds the matches of a quote inside one block
// (matchesInBlock), which searches the block's own text first, to the whole text's matches of the
// quote that lie inside the block (quoteMatches over every unit, filtered by the block's range),
// for every block and every quote of a document whose quotes overlap themselves, cross the
// boundary of two blocks, match word for word in one block and only once whitespace is normalized
// in another, and open with a heading's marker; and OccurrenceInBlock to the count it takes of
// them.
func TestAQuoteInABlockMatchesAsTheWholeTextDoes(t *testing.T) {
	t.Parallel()
	doc, err := Parse("# The alpha heading\n\nalpha beta alpha\n\nbeta  alpha\n\naaaa\n\nX alp\n\nha Y\n\nthe quick\nbrown fox\n\nquick brown\n")
	if err != nil {
		t.Fatal(err)
	}
	EnsureBlockIDs(doc)
	var blocks []string
	walk(doc, func(node *Node, _ []int, _, _ int) bool {
		if id, ok := node.Attrs[BlockIDAttr].(string); ok {
			blocks = append(blocks, id)
		}
		return true
	})
	if len(blocks) < 8 {
		t.Fatalf("the document holds %d blocks with an id, want each of its 8", len(blocks))
	}
	quotes := []string{"alpha", "beta alpha", "aa", "aaa", "alp ha", "quick brown", "brown fox", "# The alpha", "The alpha heading", "absent", ""}
	finder := NewQuoteFinder(doc)
	whole := NewQuoteFinder(doc)
	compared := 0
	for _, block := range blocks {
		for _, quote := range quotes {
			got, gotBlock, gotHeld, gotErr := finder.matchesInBlock(quote, block)
			matches, wantErr := whole.matches(quote)
			wantBlock, wantHeld := whole.blockRange(block)
			var want []quoteMatch
			for _, match := range matches {
				if match.From >= wantBlock.From && match.To <= wantBlock.To {
					want = append(want, match)
				}
			}
			if (gotErr != nil) != (wantErr != nil) || gotHeld != wantHeld || gotBlock != wantBlock || len(got) != len(want) || len(want) > 0 && !reflect.DeepEqual(got, want) {
				t.Errorf("the block %s answers the quote %q as %v in %v (held %t, %v), the whole text as %v in %v (held %t, %v)",
					block, quote, got, gotBlock, gotHeld, gotErr, want, wantBlock, wantHeld, wantErr)
			}
			for _, match := range want {
				occurrence, held, err := finder.OccurrenceInBlock(quote, block, match.From)
				count := 0
				for _, before := range want {
					if before.From < match.From {
						count++
					}
				}
				if err != nil || !held || occurrence != count {
					t.Errorf("the match of %q at %d in the block %s answers occurrence %d (held %t, %v), want %d", quote, match.From, block, occurrence, held, err, count)
				}
			}
			compared += len(want)
		}
	}
	if compared == 0 {
		t.Fatal("no quote matched inside any block, so nothing was compared")
	}
}
