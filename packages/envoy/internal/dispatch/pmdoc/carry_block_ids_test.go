package pmdoc

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// An upload writes the whole document's markdown over the live one, and that markdown names no
// untyped block's id (GET /text renders none). The blocks it leaves unchanged keep the ids live
// gave them, so the write tombstones no blockId attribute it did not change, and an anchor or a
// reference keyed on such a block's id stays on it. Only the changed block itself takes a fresh
// id, and an untyped container holding it keeps its own, whose identity is its place in the tree
// (carryKept); an id never moves onto other content (a swapped, retyped or replaced block), a
// typed block's id is the one its markdown names, and a repeat live carries is kept for its
// first holder alone.
func TestAnUploadKeepsTheIDsOfTheBlocksItLeavesUnchanged(t *testing.T) {
	spec, _, _ := docstest.LoadSpec(rand.New(rand.NewPCG(715, 1)), "LOAD-1", 25_000)

	oneToken := func(t *testing.T, old, replacement string, wantTypes []string) {
		t.Helper()
		parsed, err := Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		if count := strings.Count(markdown, old); count != 1 {
			t.Fatalf("the rendered spec holds %q %d times, want once", old, count)
		}
		after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, old, replacement, 1))
		t.Logf("upload of %d markdown bytes changing %q to %q: diff update %d bytes", len(markdown), old, replacement, diff)
		requireOnlyTheChangedLeafChangesID(t, live, after, wantTypes)
	}
	t.Run("a token in a top-level paragraph", func(t *testing.T) {
		oneToken(t, "Revision token r0", "Revision token r1", []string{"paragraph"})
	})
	t.Run("a token in a table cell", func(t *testing.T) {
		oneToken(t, "Requirement LOAD-1-R5 holds", "Requirement LOAD-1-R5 fails", []string{"table", "table_row", "table_cell", "paragraph"})
	})

	t.Run("an ask replaced by one naming no id", func(t *testing.T) {
		const ask = ":::ask{#ask-1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich transport should we expose?\n\n- REST: Matches the existing platform\n- gRPC: Adds streaming\n:::\n"
		for _, test := range []struct {
			name string
			edit func(string) string
		}{
			{name: "with the same content", edit: func(markdown string) string { return markdown }},
			{name: "with another question", edit: func(markdown string) string {
				return strings.Replace(markdown, "Which transport should we expose?", "Which transport should we drop?", 1)
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				parsed, err := Parse("Opening paragraph.\n\n" + ask + "\nClosing paragraph.\n")
				if err != nil {
					t.Fatal(err)
				}
				doc, frag, live := writeLiveDocument(t, parsed)
				markdown, err := Render(live)
				if err != nil {
					t.Fatal(err)
				}
				unnamed := strings.Replace(markdown, "{#ask-1 ", "{", 1)
				if unnamed == markdown {
					t.Fatalf("the rendered document names no ask-1:\n%s", markdown)
				}
				after, diff := uploadOver(t, doc, frag, live, test.edit(unnamed))
				t.Logf("diff update %d bytes", diff)
				oldAsk, newAsk := live.Children[1], after.Children[1]
				if newAsk.Type != "ask" {
					t.Fatalf("block 1 after the upload is a %s, want the ask", newAsk.Type)
				}
				if id, _ := storedBlockID(newAsk); id == "ask-1" || slices.Contains(blockIDsOf(live), id) {
					t.Fatalf("the ask naming no id has id %q, a live block's; want a fresh one", id)
				}
				oldInner := blockIDsOf(&Node{Type: "doc", Children: oldAsk.Children})
				for _, id := range blockIDsOf(&Node{Type: "doc", Children: newAsk.Children}) {
					if slices.Contains(oldInner, id) {
						t.Fatalf("a block inside the new ask took id %q from inside the replaced ask (%q)", id, oldInner)
					}
				}
			})
		}
	})

	t.Run("markdown naming a held typed block's id on another block is refused", func(t *testing.T) {
		parsed, err := Parse(":::callout{#note}\nA note.\n:::\n\nFirst paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		_, _, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseForWrite(markdown+"\n:::callout{#note}\nAnother note.\n:::\n", live)
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), `block id "note" would name two blocks`) {
			t.Fatalf("ParseForWrite = %v, want ErrSchema naming note twice", err)
		}
	})

	t.Run("a typed block naming an untyped live block's id keeps it", func(t *testing.T) {
		parsed, err := Parse("First paragraph.\n\nSecond paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		held, _ := storedBlockID(live.Children[0])
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		after, diff := uploadOver(t, doc, frag, live, fmt.Sprintf(":::callout{#%s}\nA note.\n:::\n\n", held)+markdown)
		t.Logf("diff update %d bytes", diff)
		if id, _ := storedBlockID(after.Children[0]); after.Children[0].Type != "callout" || id != held {
			t.Fatalf("block 0 after the upload is a %s with id %q, want the callout naming %q", after.Children[0].Type, id, held)
		}
		if id, _ := storedBlockID(after.Children[1]); id == held {
			t.Fatalf("the unchanged paragraph kept id %q, which the callout names", held)
		}
		if err := RepeatedBlockID(nil, after, after); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("two swapped paragraphs", func(t *testing.T) {
		parsed, err := Parse("Opening paragraph.\n\nAlpha paragraph.\n\nBeta paragraph.\n\nClosing paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		swapped := strings.Replace(markdown, "Alpha paragraph.\n\nBeta paragraph.", "Beta paragraph.\n\nAlpha paragraph.", 1)
		if swapped == markdown {
			t.Fatalf("the rendered document does not hold the two paragraphs in order:\n%s", markdown)
		}
		after, diff := uploadOver(t, doc, frag, live, swapped)
		t.Logf("diff update %d bytes", diff)
		liveIDs := blockIDsOf(live)
		for _, index := range []int{1, 2} {
			if id, _ := storedBlockID(after.Children[index]); slices.Contains(liveIDs, id) {
				t.Errorf("swapped paragraph %d (%q) has live id %q, want a fresh one", index, TextContent(after.Children[index]), id)
			}
		}
		for _, index := range []int{0, 3} {
			got, _ := storedBlockID(after.Children[index])
			if want, _ := storedBlockID(live.Children[index]); got != want {
				t.Errorf("unchanged paragraph %d (%q) has id %q, want live's %q", index, TextContent(after.Children[index]), got, want)
			}
		}
	})

	t.Run("a repeat live carries", func(t *testing.T) {
		parsed, err := Parse("First paragraph.\n\nSecond paragraph.\n\nThird paragraph.\n\nFourth paragraph r0.\n")
		if err != nil {
			t.Fatal(err)
		}
		repeated, _ := storedBlockID(parsed.Children[0])
		parsed.Children[1].Attrs[BlockIDAttr] = repeated
		doc, frag, live := writeLiveDocument(t, parsed)
		if id, _ := storedBlockID(live.Children[1]); id != repeated {
			t.Fatalf("live's second paragraph has id %q, want the repeat %q", id, repeated)
		}
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, "Fourth paragraph r0.", "Fourth paragraph r1.", 1))
		t.Logf("diff update %d bytes", diff)
		if id, _ := storedBlockID(after.Children[0]); id != repeated {
			t.Errorf("the repeat's first holder has id %q, want %q", id, repeated)
		}
		if id, _ := storedBlockID(after.Children[1]); slices.Contains(blockIDsOf(live), id) {
			t.Errorf("the repeat's second holder has live id %q, want a fresh one", id)
		}
		third, _ := storedBlockID(after.Children[2])
		if want, _ := storedBlockID(live.Children[2]); third != want {
			t.Errorf("the unchanged third paragraph has id %q, want live's %q", third, want)
		}
		if err := RepeatedBlockID(nil, after, after); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a paragraph retyped as a heading", func(t *testing.T) {
		parsed, err := Parse("Opening paragraph.\n\nRetyped text\n\nClosing paragraph.\n")
		if err != nil {
			t.Fatal(err)
		}
		doc, frag, live := writeLiveDocument(t, parsed)
		markdown, err := Render(live)
		if err != nil {
			t.Fatal(err)
		}
		retyped := strings.Replace(markdown, "Retyped text", "## Retyped text", 1)
		after, diff := uploadOver(t, doc, frag, live, retyped)
		t.Logf("diff update %d bytes", diff)
		if after.Children[1].Type != "heading" || TextContent(after.Children[1]) != "Retyped text" {
			t.Fatalf("block 1 after the upload is a %s holding %q, want the heading", after.Children[1].Type, TextContent(after.Children[1]))
		}
		if id, _ := storedBlockID(after.Children[1]); slices.Contains(blockIDsOf(live), id) {
			t.Fatalf("the retyped heading has live id %q, want a fresh one", id)
		}
	})
}

// An untyped container's identity is its place in the tree and not what it holds, so an upload
// that keeps one keeps its id though its contents changed. A list that gains, loses or alters an
// item is still that list, and an id minted for it would assert a new list exists where none
// does, reparenting every untouched sibling onto it. A changed block holding inline content is
// other content and still takes a fresh id, and so does a container the alignment does not keep:
// one of another kind in that place, one under another parent, and one live never held. A
// reference keyed on the container it keeps still names a block the document holds.
func TestAnUploadKeepsAnUntypedContainersIDWhenItsContentsChange(t *testing.T) {
	const listDocument = "Opening paragraph.\n\n- Alpha item\n- Beta item\n- Gamma item\n\nClosing paragraph.\n"

	t.Run("a list whose one item's text changed keeps its id", func(t *testing.T) {
		doc, frag, live, markdown := liveMarkdown(t, listDocument)
		after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, "Beta item", "Beta item, revised", 1))
		t.Logf("diff update %d bytes", diff)
		liveList, list := live.Children[1], after.Children[1]
		if list.Type != "bullet_list" || len(list.Children) != 3 {
			t.Fatalf("block 1 after the upload is a %s with %d children, want the three-item bullet list", list.Type, len(list.Children))
		}
		if got, want := blockIDOfNode(t, list), blockIDOfNode(t, liveList); got != want {
			t.Errorf("the list holding the changed item has id %q, want live's %q", got, want)
		}
		for index, item := range list.Children {
			if got, want := blockIDOfNode(t, item), blockIDOfNode(t, liveList.Children[index]); got != want {
				t.Errorf("item %d (%q) has id %q, want live's %q", index, TextContent(item), got, want)
			}
		}
	})

	t.Run("a changed leaf takes a fresh id", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			old     string
			edited  string
			changed func(after *Node) *Node
		}{
			{
				name:    "a top-level paragraph",
				old:     "Closing paragraph.",
				edited:  "Closing paragraph, revised.",
				changed: func(after *Node) *Node { return after.Children[2] },
			},
			{
				name:    "a paragraph inside the list item the upload keeps",
				old:     "Beta item",
				edited:  "Beta item, revised",
				changed: func(after *Node) *Node { return after.Children[1].Children[1].Children[0] },
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				doc, frag, live, markdown := liveMarkdown(t, listDocument)
				after, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, test.old, test.edited, 1))
				t.Logf("diff update %d bytes", diff)
				changed := test.changed(after)
				if changed.Type != "paragraph" || TextContent(changed) != test.edited {
					t.Fatalf("the block the upload changed is a %s holding %q, want the paragraph holding %q", changed.Type, TextContent(changed), test.edited)
				}
				if id := blockIDOfNode(t, changed); slices.Contains(blockIDsOf(live), id) {
					t.Errorf("the changed paragraph has live id %q, want a fresh one", id)
				}
			})
		}
	})

	t.Run("a container the upload does not keep takes a fresh id", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			document string
			old      string
			written  string
			list     func(after *Node) *Node
		}{
			{
				name:     "another kind of list in its place",
				document: listDocument,
				old:      "- Alpha item\n- Beta item\n- Gamma item",
				written:  "1. Alpha item\n2. Beta item\n3. Gamma item",
				list:     func(after *Node) *Node { return after.Children[1] },
			},
			{
				name:     "the same list under another parent",
				document: listDocument,
				old:      "- Alpha item\n- Beta item\n- Gamma item",
				written:  "> - Alpha item\n> - Beta item\n> - Gamma item",
				list:     func(after *Node) *Node { return after.Children[1].Children[0] },
			},
			{
				name:     "a list live never held",
				document: "Opening paragraph.\n\nClosing paragraph.\n",
				old:      "Closing paragraph.",
				written:  "- Alpha item\n- Beta item\n\nClosing paragraph.",
				list:     func(after *Node) *Node { return after.Children[1] },
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				doc, frag, live, markdown := liveMarkdown(t, test.document)
				replaced := strings.Replace(markdown, test.old, test.written, 1)
				if replaced == markdown {
					t.Fatalf("the rendered document does not hold %q:\n%s", test.old, markdown)
				}
				after, diff := uploadOver(t, doc, frag, live, replaced)
				t.Logf("diff update %d bytes", diff)
				list := test.list(after)
				if list.Type != "bullet_list" && list.Type != "ordered_list" {
					t.Fatalf("the block the upload wrote is a %s, want the list", list.Type)
				}
				if id := blockIDOfNode(t, list); slices.Contains(blockIDsOf(live), id) {
					t.Errorf("the %s has live id %q, want a fresh one", list.Type, id)
				}
			})
		}
	})

	t.Run("a reference naming the kept list still resolves", func(t *testing.T) {
		doc, frag, live, markdown := liveMarkdown(t, listDocument)
		listID := blockIDOfNode(t, live.Children[1])
		reference := "dispatch://LOAD-1/spec#b-" + listID
		cited, diff := uploadOver(t, doc, frag, live, strings.Replace(markdown, "Closing paragraph.", "Closing paragraph. See "+reference+".", 1))
		t.Logf("the upload citing the list: diff update %d bytes", diff)
		if got := blockIDOfNode(t, cited.Children[1]); got != listID {
			t.Fatalf("the list the citing upload left unchanged has id %q, want %q", got, listID)
		}
		citedMarkdown, err := Render(cited)
		if err != nil {
			t.Fatal(err)
		}
		after, diff := uploadOver(t, doc, frag, cited, strings.Replace(citedMarkdown, "Beta item", "Beta item, revised", 1))
		t.Logf("the upload editing the item: diff update %d bytes", diff)
		stored, err := Render(after)
		if err != nil {
			t.Fatal(err)
		}
		found := text.Extract(stored, "")
		if len(found) != 1 || found[0].Kind != "block" {
			t.Fatalf("the markdown the upload stored holds %d references (%v), want the one block reference:\n%s", len(found), found, stored)
		}
		if !slices.Contains(blockIDsOf(after), found[0].Block) {
			t.Fatalf("%s names a block the document no longer holds", text.TargetID(found[0]))
		}
		if got := blockIDOfNode(t, after.Children[1]); got != found[0].Block {
			t.Fatalf("%s resolves past the bullet list, whose id is %q", text.TargetID(found[0]), got)
		}
	})
}

// writeLiveDocument writes tree into a fresh document's fragment and returns the document, the
// fragment and the tree read back from it, as a room holds it.
func writeLiveDocument(t *testing.T, tree *Node) (*crdt.Doc, *crdt.YXmlFragment, *Node) {
	t.Helper()
	doc := crdt.New()
	frag := doc.GetXmlFragment("prosemirror")
	writeUpdate(t, doc, frag, tree)
	live, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	return doc, frag, live
}

// liveMarkdown parses markdown into a fresh document (writeLiveDocument) and returns that
// document, its fragment, the tree read back from it and the markdown that tree renders as,
// which is what an upload over it writes.
func liveMarkdown(t *testing.T, markdown string) (*crdt.Doc, *crdt.YXmlFragment, *Node, string) {
	t.Helper()
	parsed, err := Parse(markdown)
	if err != nil {
		t.Fatal(err)
	}
	doc, frag, live := writeLiveDocument(t, parsed)
	rendered, err := Render(live)
	if err != nil {
		t.Fatal(err)
	}
	return doc, frag, live, rendered
}

// uploadOver writes markdown over live as an upload does (ParseForWrite, then Update into the
// document holding live) and returns the tree read back and the size of the update the write
// added.
func uploadOver(t *testing.T, doc *crdt.Doc, frag *crdt.YXmlFragment, live *Node, markdown string) (*Node, int) {
	t.Helper()
	next, err := ParseForWrite(markdown, live)
	if err != nil {
		t.Fatalf("ParseForWrite: %v", err)
	}
	before := doc.StateVector()
	writeUpdate(t, doc, frag, next)
	after, err := Read(frag)
	if err != nil {
		t.Fatal(err)
	}
	return after, len(crdt.EncodeStateAsUpdateV1(doc, before))
}

// requireOnlyTheChangedLeafChangesID walks before and after in parallel, which must have one
// shape, and requires exactly one text node to differ, the blocks holding it to be wantTypes
// (outermost first), the innermost of them - the block the changed text sits in - to carry an id
// before did not, and every other block to keep its id, each untyped container above the changed
// text included: such a container's identity is its place in the tree and not its contents
// (carryKept).
func requireOnlyTheChangedLeafChangesID(t *testing.T, before, after *Node, wantTypes []string) {
	t.Helper()
	var changedText [][]int
	var changedIDs []string
	blocks := 0
	var walk func(a, b *Node, path []int)
	walk = func(a, b *Node, path []int) {
		if a.Type != b.Type || len(a.Children) != len(b.Children) {
			t.Fatalf("at %v the upload changed the shape: %s with %d children became %s with %d", path, a.Type, len(a.Children), b.Type, len(b.Children))
		}
		if a.Type == "text" && (a.Text != b.Text || fmt.Sprint(a.Marks) != fmt.Sprint(b.Marks)) {
			changedText = append(changedText, slices.Clone(path))
		}
		if a.Type != "doc" && !isInlineNodeType(a.Type) {
			blocks++
			if idA, _ := storedBlockID(a); idA != fmt.Sprint(b.Attrs[BlockIDAttr]) {
				changedIDs = append(changedIDs, fmt.Sprint(path))
			}
		}
		for index := range a.Children {
			walk(a.Children[index], b.Children[index], append(path, index))
		}
	}
	walk(before, after, nil)
	if len(changedText) != 1 {
		t.Fatalf("the upload changed %d text nodes (%v), want 1", len(changedText), changedText)
	}
	changed := changedText[0]
	var gotTypes []string
	node := before
	for depth := 1; depth < len(changed); depth++ {
		node = node.Children[changed[depth-1]]
		gotTypes = append(gotTypes, node.Type)
	}
	if !slices.Equal(gotTypes, wantTypes) {
		t.Fatalf("the changed text sits in %q, want %q", gotTypes, wantTypes)
	}
	leaf := changed[:len(changed)-1]
	if !slices.Equal(changedIDs, []string{fmt.Sprint(leaf)}) {
		shown := changedIDs[:min(len(changedIDs), 8)]
		t.Fatalf("%d of %d blocks changed id, want 1 (the block holding the changed text, at %v); first changed: %q", len(changedIDs), blocks, leaf, shown)
	}
	node = after
	for depth := 1; depth < len(changed); depth++ {
		node = node.Children[changed[depth-1]]
	}
	if id, ok := storedBlockID(node); !ok || slices.Contains(blockIDsOf(before), id) {
		t.Fatalf("the changed %s at %v has id %q, want a fresh one", node.Type, leaf, id)
	}
}

// blockIDsOf is the id of every block in tree that carries one, in document order.
func blockIDsOf(tree *Node) []string {
	var ids []string
	walkBlockIDs(tree, func(id string) bool {
		ids = append(ids, id)
		return true
	})
	return ids
}

// blockIDOfNode is node's stored block id, which every block of a written document carries
// (EnsureBlockIDs).
func blockIDOfNode(t *testing.T, node *Node) string {
	t.Helper()
	id, ok := storedBlockID(node)
	if !ok {
		t.Fatalf("the %s holding %q carries no block id", node.Type, TextContent(node))
	}
	return id
}
