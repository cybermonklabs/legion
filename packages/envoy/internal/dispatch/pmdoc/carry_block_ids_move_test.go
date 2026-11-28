package pmdoc

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// carryMoveParagraphs is how many paragraphs the moving document holds.
const carryMoveParagraphs = 12

// carryMoveParagraph is the text of paragraph n of the moving document, which no other paragraph
// holds.
func carryMoveParagraph(n int) string {
	return fmt.Sprintf("Paragraph %02d says a thing of its own.", n)
}

// carryMoveOrder is the moving document's paragraphs in their first order, or with paragraph from
// taken out and put back right after paragraph after where from is set.
func carryMoveOrder(from, after int) []int {
	order := make([]int, 0, carryMoveParagraphs)
	for n := 1; n <= carryMoveParagraphs; n++ {
		if n != from {
			order = append(order, n)
		}
		if n == after {
			order = append(order, from)
		}
	}
	return order
}

// carryMoveMarkdown is the moving document's markdown: paragraph n for each n of order, with
// written standing in for paragraph n where it names one.
func carryMoveMarkdown(order []int, written map[int]string) string {
	blocks := make([]string, len(order))
	for index, n := range order {
		blocks[index] = carryMoveParagraph(n)
		if text, ok := written[n]; ok {
			blocks[index] = text
		}
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// carryMoveIDsByText is the ids each child of tree carries, its own and its descendants'
// (blockIDsOf), by the child's text (TextContent).
func carryMoveIDsByText(tree *Node) map[string][]string {
	ids := make(map[string][]string, len(tree.Children))
	for _, child := range tree.Children {
		ids[TextContent(child)] = blockIDsOf(child)
	}
	return ids
}

// carryMoveRequireKept fails t unless each of texts names a child of after carrying the ids the
// child of before with that text carried.
func carryMoveRequireKept(t *testing.T, before, after *Node, texts []string) {
	t.Helper()
	held, now := carryMoveIDsByText(before), carryMoveIDsByText(after)
	for _, text := range texts {
		if !slices.Equal(now[text], held[text]) || len(held[text]) == 0 {
			t.Errorf("the block holding %q carries the ids %q after the upload, want live's %q", text, now[text], held[text])
		}
	}
}

// carryMoveRequireFresh fails t unless every id block carries is one live carries nowhere.
func carryMoveRequireFresh(t *testing.T, live, block *Node, why string) {
	t.Helper()
	liveIDs := blockIDsOf(live)
	for _, id := range blockIDsOf(block) {
		if slices.Contains(liveIDs, id) {
			t.Errorf("the %s holding %q carries live's id %q, want a fresh one: %s", block.Type, TextContent(block), id, why)
		}
	}
}

// A version that moves a block among its parent's children without changing it keeps the id of
// that block and of every block the move passes (R13 of the issue-tree spec), so a pointer or an
// anchor keyed on any of them still resolves: the carry aligns each parent's children in live with
// its children in next (carryChildren), rather than pairing them by place alone, which gave the
// moved block and every block it passed a fresh id. An unchanged ask the version moves keeps its
// id, and so keeps the row holding its answer. A block the move changes, a block put under another
// parent and a changed container moved out of its place still take a fresh id (carryKept). api's
// TestAMoveOfOneParagraphKeepsEveryBlockID holds the same rule through the upload route, the block
// index and the store, over 2,000 paragraphs.
func TestAnUploadThatMovesABlockKeepsTheIDsOfTheBlocksItMovesAndPasses(t *testing.T) {
	t.Parallel()
	first := carryMoveOrder(0, 0)
	every := make([]string, len(first))
	for index, n := range first {
		every[index] = carryMoveParagraph(n)
	}

	for _, test := range []struct {
		name  string
		order []int
	}{
		{name: "a paragraph moved forward past seven others", order: carryMoveOrder(3, 10)},
		{name: "a paragraph moved back past seven others", order: carryMoveOrder(10, 2)},
		{name: "two swapped paragraphs", order: carryMoveOrder(4, 5)},
	} {
		t.Run(test.name, func(t *testing.T) {
			doc, frag, live, _ := liveMarkdown(t, carryMoveMarkdown(first, nil))
			after, diff := uploadOver(t, doc, frag, live, carryMoveMarkdown(test.order, nil))
			t.Logf("the upload of the paragraphs in the order %v: diff update %d bytes", test.order, diff)
			carryMoveRequireKept(t, live, after, every)
		})
	}

	t.Run("a moved paragraph whose text changed takes a fresh id", func(t *testing.T) {
		const changed = "Paragraph 03 says a thing of its own, revised."
		doc, frag, live, _ := liveMarkdown(t, carryMoveMarkdown(first, nil))
		after, diff := uploadOver(t, doc, frag, live, carryMoveMarkdown(carryMoveOrder(3, 10), map[int]string{3: changed}))
		t.Logf("diff update %d bytes", diff)
		block := after.Children[9]
		if TextContent(block) != changed {
			t.Fatalf("block 9 after the upload holds %q, want the moved paragraph holding %q", TextContent(block), changed)
		}
		carryMoveRequireFresh(t, live, block, "a changed leaf takes a fresh id")
		carryMoveRequireKept(t, live, after, slices.DeleteFunc(slices.Clone(every), func(text string) bool { return text == carryMoveParagraph(3) }))
	})

	t.Run("a paragraph moved under a blockquote takes a fresh id", func(t *testing.T) {
		doc, frag, live, _ := liveMarkdown(t, carryMoveMarkdown(first, nil))
		after, diff := uploadOver(t, doc, frag, live, carryMoveMarkdown(carryMoveOrder(3, 10), map[int]string{3: "> " + carryMoveParagraph(3)}))
		t.Logf("diff update %d bytes", diff)
		quote := after.Children[9]
		if quote.Type != "blockquote" || TextContent(quote) != carryMoveParagraph(3) {
			t.Fatalf("block 9 after the upload is a %s holding %q, want the blockquote holding paragraph 3", quote.Type, TextContent(quote))
		}
		carryMoveRequireFresh(t, live, quote, "a block put under another parent takes a fresh id")
		carryMoveRequireKept(t, live, after, slices.DeleteFunc(slices.Clone(every), func(text string) bool { return text == carryMoveParagraph(3) }))
	})

	t.Run("an item moved within its list", func(t *testing.T) {
		doc, frag, live, _ := liveMarkdown(t, "Opening paragraph.\n\n- Alpha item\n- Beta item\n- Gamma item\n- Delta item\n\nClosing paragraph.\n")
		after, diff := uploadOver(t, doc, frag, live, "Opening paragraph.\n\n- Beta item\n- Gamma item\n- Alpha item\n- Delta item\n\nClosing paragraph.\n")
		t.Logf("diff update %d bytes", diff)
		liveList, list := live.Children[1], after.Children[1]
		if list.Type != "bullet_list" || len(list.Children) != 4 || TextContent(list.Children[2]) != "Alpha item" {
			t.Fatalf("block 1 after the upload is a %s holding %q, want the bullet list with the alpha item third", list.Type, TextContent(list))
		}
		if got, want := blockIDOfNode(t, list), blockIDOfNode(t, liveList); got != want {
			t.Errorf("the list whose item moved has id %q, want live's %q", got, want)
		}
		carryMoveRequireKept(t, liveList, list, []string{"Alpha item", "Beta item", "Gamma item", "Delta item"})
		carryMoveRequireKept(t, live, after, []string{"Opening paragraph.", "Closing paragraph."})
	})

	t.Run("an unchanged ask moved past a paragraph", func(t *testing.T) {
		const ask = ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
		doc, frag, live, markdown := liveMarkdown(t, "Opening paragraph.\n\n"+ask+"\nClosing paragraph.\n")
		start := strings.Index(markdown, ":::ask{")
		if start < 0 {
			t.Fatalf("the rendered document holds no ask block:\n%s", markdown)
		}
		end := strings.Index(markdown[start:], "\n:::\n")
		if end < 0 {
			t.Fatalf("the ask block of the rendered document does not close:\n%s", markdown)
		}
		block := markdown[start : start+end+len("\n:::\n")]
		moved := strings.Replace(markdown, block, "", 1) + "\n" + strings.Replace(block, "{#ask-1 ", "{", 1)
		after, diff := uploadOver(t, doc, frag, live, moved)
		t.Logf("diff update %d bytes", diff)
		last := after.Children[len(after.Children)-1]
		if last.Type != "ask" {
			t.Fatalf("the last block after the upload is a %s, want the ask the upload moved there", last.Type)
		}
		if got, want := blockIDsOf(last), blockIDsOf(live.Children[1]); !slices.Equal(got, want) || got[0] != "ask-1" {
			t.Errorf("the moved ask and its blocks carry the ids %q, want live's %q, the ask's ask-1 first", got, want)
		}
		carryMoveRequireKept(t, live, after, []string{"Opening paragraph.", "Closing paragraph."})
	})

	t.Run("a changed list moved past a paragraph left in place takes a fresh id", func(t *testing.T) {
		doc, frag, live, _ := liveMarkdown(t, "Opening paragraph.\n\n- Alpha item\n- Beta item\n\nMiddle paragraph.\n\nClosing paragraph.\n")
		after, diff := uploadOver(t, doc, frag, live, "Opening paragraph.\n\nMiddle paragraph.\n\n- Alpha item\n- Beta item\n- Gamma item\n\nClosing paragraph.\n")
		t.Logf("diff update %d bytes", diff)
		list := after.Children[2]
		if list.Type != "bullet_list" || len(list.Children) != 3 {
			t.Fatalf("block 2 after the upload is a %s with %d children, want the three-item bullet list", list.Type, len(list.Children))
		}
		carryMoveRequireFresh(t, live, list, "a container keeps its id only in its own place, and its items stand under a list live never held")
		carryMoveRequireKept(t, live, after, []string{"Opening paragraph.", "Middle paragraph.", "Closing paragraph."})
	})
}
