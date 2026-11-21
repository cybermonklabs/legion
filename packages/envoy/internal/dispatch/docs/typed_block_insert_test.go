package docs

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// typedInsertFixture holds a requirement, an ask with its question and two options, a check, a
// callout holding a list, and a paragraph at the document's own level.
const typedInsertFixture = ":::requirement{#r1}\nThe read succeeds.\n:::\n\n" +
	":::ask{#a1 urgency=\"med\" multiple=\"false\" state=\"open\"}\nWhich transport?\n\n- NATS\n- HTTP\n:::\n\n" +
	":::check{#k1 serves=\"dispatch://CORE-1/spec#b-r1\" surface=\"cli\" run=\"go test ./...\"}\nThe read returns 200.\n:::\n\n" +
	":::callout{#c1 kind=\"note\"}\n- one\n- two\n:::\n\n" +
	"After.\n"

// An insert anchored on a block inside a typed block lands beside that block, inside the typed
// block, whether the anchor names the block or quotes its text, after it or before it: the
// typed block's children are where the anchor names (dispatch://INFRA-3142). A list item is such
// a parent too, so a paragraph anchored on the text of an item of a list in a callout lands in
// that item, beside its text, where it was written after the whole list; list content anchored
// on an ask's option joins the options. An anchor at the document's own level, or on a typed
// block's own block, lands where it always did.
func TestAnInsertLandsInsideTheTypedBlockHoldingItsAnchor(t *testing.T) {
	for _, test := range []struct {
		name string
		op   func(t *testing.T, tree *pmdoc.Node) model.EditOp
		// old and new are the stretch of the fixture's rendering the insert rewrites.
		old, new string
	}{
		{
			name: "after a requirement's paragraph, by its block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "The read succeeds."), Markdown: "Within a second."}
			},
			old: "The read succeeds.\n:::\n", new: "The read succeeds.\n\nWithin a second.\n:::\n",
		},
		{
			name: "before a requirement's paragraph, by its block",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", Before: "block:" + blockIDOfText(t, tree, "The read succeeds."), Markdown: "First."}
			},
			old: "{#r1}\nThe read succeeds.\n", new: "{#r1}\nFirst.\n\nThe read succeeds.\n",
		},
		{
			name: "after a requirement's paragraph, by a quote of it",
			op: func(*testing.T, *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "read succeeds", Markdown: "Within a second."}
			},
			old: "The read succeeds.\n:::\n", new: "The read succeeds.\n\nWithin a second.\n:::\n",
		},
		{
			name: "after an ask's question",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "Which transport?"), Markdown: "The one the daemon holds."}
			},
			old: "Which transport?\n\n- NATS", new: "Which transport?\n\nThe one the daemon holds.\n\n- NATS",
		},
		{
			name: "an option after an ask's option",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "NATS"), Markdown: "- gRPC"}
			},
			old: "- NATS\n- HTTP\n", new: "- NATS\n- gRPC\n- HTTP\n",
		},
		{
			name: "after a check's paragraph",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "The read returns 200."), Markdown: "Within a second."}
			},
			old: "The read returns 200.\n:::\n", new: "The read returns 200.\n\nWithin a second.\n:::\n",
		},
		{
			name: "a paragraph after the text of an item of a callout's list",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "one"), Markdown: "Noted."}
			},
			old: "- one\n- two\n:::\n", new: "- one\n\n  Noted.\n- two\n:::\n",
		},
		{
			name: "after a paragraph at the document's level",
			op: func(t *testing.T, tree *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "After."), Markdown: "Closing."}
			},
			old: "After.\n", new: "After.\n\nClosing.\n",
		},
		{
			name: "after a requirement's own block",
			op: func(*testing.T, *pmdoc.Node) model.EditOp {
				return model.EditOp{Op: "insert", After: "block:r1", Markdown: "Beside it."}
			},
			old: "The read succeeds.\n:::\n\n", new: "The read succeeds.\n:::\n\nBeside it.\n\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(typedInsertFixture)
			if err != nil {
				t.Fatal(err)
			}
			before, err := renderTree(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(before, test.old) {
				t.Fatalf("the fixture renders %q, which holds no %q", before, test.old)
			}
			out, err := applyAlone(tree, test.op(t, tree))
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(out)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(before, test.old, test.new, 1); markdown != want {
				t.Fatalf("after the insert = %q, want %q", markdown, want)
			}
		})
	}
}

// An insert anchored inside a typed block whose content rule cannot hold its markdown there is
// refused, naming the anchor, the typed block, its rule and what breaks it, and the anchor that
// writes the markdown beside the typed block instead. A paragraph after an ask's options is
// anchored on the options list's own block: one anchored on an option's text lands in that
// option's item, beside its text (TestAnInsertLandsInsideTheListItemOrQuoteHoldingItsAnchor).
func TestAnInsertATypedBlocksRuleCannotHoldIsRefused(t *testing.T) {
	for _, test := range []struct {
		name, text, markdown, holder string
		// list anchors the insert on the list holding text, where text's own block is anchored
		// otherwise.
		before, list bool
		rule         pmdoc.BlockContentRule
		broken, says string
	}{
		{
			name: "a heading in a requirement", text: "The read succeeds.", markdown: "## Latency",
			holder: "r1", rule: pmdoc.BlockContentParagraphs, broken: "a heading",
			says: `lands inside the requirement block "r1", whose content rule, paragraph+, cannot hold a heading there; write what that rule holds there, or anchor the insert on the requirement block itself (after: "block:r1") to write the markdown beside it`,
		},
		{
			name: "a list in a check", text: "The read returns 200.", markdown: "- one more", before: true,
			holder: "k1", rule: pmdoc.BlockContentParagraphs, broken: "a bullet list",
			says: `(before: "block:k1")`,
		},
		{
			name: "a paragraph after an ask's options", text: "HTTP", list: true, markdown: "Pick one by Friday.",
			holder: "a1", rule: pmdoc.BlockContentParagraphsOptionalBulletList, broken: "a paragraph after its options",
			says: `cannot hold a paragraph after its options there`,
		},
		{
			name: "a second option list after an ask's question", text: "Which transport?", markdown: "- gRPC",
			holder: "a1", rule: pmdoc.BlockContentParagraphsOptionalBulletList, broken: "a second bullet list",
			says: `cannot hold a second bullet list there`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(typedInsertFixture)
			if err != nil {
				t.Fatal(err)
			}
			anchor := "block:" + blockIDOfText(t, tree, test.text)
			if test.list {
				anchor = "block:" + listIDOfText(t, tree, test.text)
			}
			op := model.EditOp{Op: "insert", After: anchor, Markdown: test.markdown}
			field := "after"
			if test.before {
				op, field = model.EditOp{Op: "insert", Before: anchor, Markdown: test.markdown}, "before"
			}
			_, err = applyAlone(tree, op)
			var refusal *ErrInsertDoesNotFit
			if !errors.As(err, &refusal) {
				t.Fatalf("insert of %q at %q: error = %v, want ErrInsertDoesNotFit", test.markdown, test.text, err)
			}
			if refusal.Field != field || refusal.Anchor != anchor || refusal.HolderID != test.holder || refusal.Rule != test.rule || refusal.Broken != test.broken {
				t.Fatalf("refusal = %+v, want %s %q in %q under %s, broken by %s", refusal, field, anchor, test.holder, test.rule, test.broken)
			}
			if !isEditRefusal(err) {
				t.Fatalf("isEditRefusal(%v) = false: the route would bury the refusal", err)
			}
			if !strings.Contains(err.Error(), test.says) || !strings.Contains(err.Error(), "insert "+field+" "+`"`+anchor+`"`) {
				t.Fatalf("refusal = %q, want it to name the anchor and say %q", err.Error(), test.says)
			}
		})
	}
}

// listIDOfText is the id of the innermost list holding text.
func listIDOfText(t *testing.T, tree *pmdoc.Node, text string) string {
	t.Helper()
	var id string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if (node.Type == "bullet_list" || node.Type == "ordered_list") && strings.Contains(pmdoc.TextContent(node), text) {
			id, _ = node.Attrs[pmdoc.BlockIDAttr].(string)
		}
		return true
	})
	if id == "" {
		t.Fatalf("no list holding %q carries an id", text)
	}
	return id
}

// listAndQuoteFixture holds a list of two items, a blockquote and a paragraph at the document's
// own level.
const listAndQuoteFixture = "- one\n- two\n\n> Quoted.\n\nAfter.\n"

// An insert goes next to the block its anchor names, inside that block's own parent, and a list
// item and a blockquote are parents as a typed block is: a paragraph anchored on an item's text
// lands in that item, beside its text, and one anchored on a quote's paragraph lands in the quote,
// where each was written after the whole list or the whole quote. An item holding a second block
// is written with a blank line inside it, which reads back as a spread item, and the write stores
// it so. List content anchored on an item's text still joins that item's list, as a sibling of the
// item; anchored on a quote's paragraph, it lands in the quote, as a paragraph does, where it was
// written after the quote.
func TestAnInsertLandsInsideTheListItemOrQuoteHoldingItsAnchor(t *testing.T) {
	for _, test := range []struct {
		name, text, markdown string
		before               bool
		// old and new are the stretch of the fixture's rendering the insert rewrites.
		old, new string
	}{
		{
			name: "a paragraph after an item's text", text: "one", markdown: "Noted.",
			old: "- one\n- two\n", new: "- one\n\n  Noted.\n- two\n",
		},
		{
			name: "a paragraph before an item's text", text: "two", markdown: "First.", before: true,
			old: "- one\n- two\n", new: "- one\n- First.\n\n  two\n",
		},
		{
			name: "a paragraph after a quote's paragraph", text: "Quoted.", markdown: "Noted.",
			old: "> Quoted.\n", new: "> Quoted.\n>\n> Noted.\n",
		},
		{
			name: "list content after an item's text joins its list", text: "one", markdown: "- three",
			old: "- one\n- two\n", new: "- one\n- three\n- two\n",
		},
		{
			name: "list content after a quote's paragraph lands in the quote", text: "Quoted.", markdown: "- three",
			old: "> Quoted.\n", new: "> Quoted.\n>\n> - three\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(listAndQuoteFixture)
			if err != nil {
				t.Fatal(err)
			}
			before, err := renderTree(tree)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(before, test.old) {
				t.Fatalf("the fixture renders %q, which holds no %q", before, test.old)
			}
			op := model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, test.text), Markdown: test.markdown}
			if test.before {
				op.After, op.Before = "", op.After
			}
			out, err := applyAlone(tree, op)
			if err != nil {
				t.Fatal(err)
			}
			markdown, err := renderTree(out)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.Replace(before, test.old, test.new, 1); markdown != want {
				t.Fatalf("after the insert = %q, want %q", markdown, want)
			}
		})
	}
}

// An insert anchored on a list item's text that would put a block other than a paragraph ahead
// of that text is refused: a list item's content rule, paragraph block*, opens with the item's
// text, so a heading before it cannot stand there. The refusal names the list item, its rule and
// what breaks it, and the anchor on the list item itself, which writes the markdown before the
// whole list. Such an insert was written before the whole list.
func TestAnInsertAListItemCannotHoldIsRefused(t *testing.T) {
	tree, err := parseInput(listAndQuoteFixture)
	if err != nil {
		t.Fatal(err)
	}
	anchor := "block:" + blockIDOfText(t, tree, "two")
	_, err = applyAlone(tree, model.EditOp{Op: "insert", Before: anchor, Markdown: "## Heading"})
	var refusal *ErrInsertDoesNotFit
	if !errors.As(err, &refusal) {
		t.Fatalf("insert of a heading before %q: error = %v, want ErrInsertDoesNotFit", anchor, err)
	}
	if refusal.Field != "before" || refusal.Anchor != anchor || refusal.HolderType != "list_item" || refusal.HolderID == "" ||
		refusal.Rule != "paragraph block*" || refusal.Broken != "a heading before its text" {
		t.Fatalf("refusal = %+v, want before %q in a list item under paragraph block*, broken by a heading before its text", refusal, anchor)
	}
	if !isEditRefusal(err) {
		t.Fatalf("isEditRefusal(%v) = false: the route would bury the refusal", err)
	}
	says := `insert before "` + anchor + `" lands inside the list item "` + refusal.HolderID + `", whose content rule, paragraph block*, cannot hold a heading before its text there; write what that rule holds there, or anchor the insert on the list item itself (before: "block:` + refusal.HolderID + `") to write the markdown before the list holding it`
	if !strings.Contains(err.Error(), says) {
		t.Fatalf("refusal = %q, want it to say %q", err.Error(), says)
	}
}

// An insert anchored on a list item's text lands inside that item, after its text, as a paragraph
// does (TestAnInsertLandsInsideTheListItemOrQuoteHoldingItsAnchor). A block other than a
// paragraph lands there too and nothing else is written: the item holds its text and then that
// block, with no empty paragraph between them. An empty paragraph renders as a line of
// indentation alone, which a read of the stored markdown drops, so the stored text and the tree
// it came from would disagree (INFRA-341, testing round 17, at 1808df5f).
func TestABlockInsertedAfterAListItemsTextWritesNoEmptyParagraph(t *testing.T) {
	for _, test := range []struct {
		name, markdown, firstLine, blockType string
	}{
		{name: "a heading", markdown: "## Heading", firstLine: "## Heading", blockType: "heading"},
		{name: "a code block", markdown: "```\ncode here\n```", firstLine: "```", blockType: "code_block"},
		{name: "a quote", markdown: "> Quoted aside.", firstLine: "> Quoted aside.", blockType: "blockquote"},
		{name: "a table", markdown: "| a | b |\n|---|---|\n| 1 | 2 |", firstLine: "| a", blockType: "table"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := parseInput(listAndQuoteFixture)
			if err != nil {
				t.Fatal(err)
			}
			op := model.EditOp{Op: "insert", After: "block:" + blockIDOfText(t, tree, "one"), Markdown: test.markdown}
			out, err := applyAlone(tree, op)
			if err != nil {
				t.Fatalf("insert of %s after the item's text: %v", test.name, err)
			}
			item := firstNodeOfType(out, "list_item")
			if item == nil {
				t.Fatal("the document holds no list item after the insert")
			}
			var kinds []string
			empty := false
			for _, child := range item.Children {
				kinds = append(kinds, child.Type)
				empty = empty || (child.Type == "paragraph" && strings.TrimSpace(textOf(child)) == "")
			}
			if empty {
				t.Errorf("the item holds an empty paragraph: its children are %v", kinds)
			}
			if len(kinds) != 2 || kinds[0] != "paragraph" || kinds[1] != test.blockType {
				t.Errorf("the item's children are %v, want [paragraph %s]", kinds, test.blockType)
			}
			markdown, err := renderTree(out)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(markdown, "\n") {
				if line != "" && strings.TrimSpace(line) == "" {
					t.Errorf("the stored markdown holds a line of indentation alone: %q", markdown)
					break
				}
			}
			if !strings.Contains(markdown, "\n  "+test.firstLine) {
				t.Errorf("the stored markdown %q does not hold %q inside the item", markdown, test.firstLine)
			}
			reread, err := parseInput(markdown)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := renderTree(reread); err != nil || again != markdown {
				t.Errorf("a read of the stored markdown renders %q, %v; want the same %q", again, err, markdown)
			}
		})
	}
}

// insertParentsFixture holds a callout, a requirement, a list item and a blockquote, each holding
// two paragraphs, and a paragraph at the document's own level.
const insertParentsFixture = ":::callout{#c1 kind=\"note\"}\nFirst.\n\nSecond.\n:::\n\n" +
	":::requirement{#r1}\nThe read succeeds.\n\nWithin a second.\n:::\n\n" +
	"- one\n\n  more\n- two\n\n" +
	"> Quoted.\n>\n> Again.\n\n" +
	"After.\n"

// An insert anchored on a child of a block that holds blocks - a typed block, a list item, a
// blockquote - writes the blocks its markdown holds and nothing else, before the first child,
// after it or after a later one: that block's children grow by exactly the blocks the markdown
// holds, in the place the anchor names; no empty paragraph stands anywhere; and the stored
// markdown holds no line of indentation alone and reads back as itself. A block the parent's
// content rule cannot hold there is refused as it was: anything but a paragraph in a
// requirement, whose rule is paragraph+, and anything but a paragraph ahead of a list item's
// text, whose rule is paragraph block* (INFRA-341).
func TestAnInsertIntoATypedBlockAListItemOrAQuoteWritesOnlyItsBlocks(t *testing.T) {
	parents := []struct {
		name string
		// children are the texts of the parent's two paragraphs, which the anchors name.
		children [2]string
		// broken is what a refusal names as breaking the parent's rule for blocks landing at index
		// among its children, the first of them that is no paragraph being noun ("" for paragraphs
		// alone), or "" where the rule holds them there.
		broken func(index int, noun string) string
	}{
		{name: "a callout", children: [2]string{"First.", "Second."}, broken: func(int, string) string { return "" }},
		{name: "a requirement", children: [2]string{"The read succeeds.", "Within a second."}, broken: func(_ int, noun string) string { return noun }},
		{name: "a list item", children: [2]string{"one", "more"}, broken: func(index int, noun string) string {
			if index > 0 || noun == "" {
				return ""
			}
			return noun + " before its text"
		}},
		{name: "a blockquote", children: [2]string{"Quoted.", "Again."}, broken: func(int, string) string { return "" }},
	}
	blocks := []struct {
		name, markdown string
		kinds          []string
		// noun names the first of kinds that is no paragraph, as a refusal names it, or is "" for
		// paragraphs alone.
		noun string
	}{
		{name: "a paragraph", markdown: "Noted.", kinds: []string{"paragraph"}},
		{name: "a heading", markdown: "## Heading", kinds: []string{"heading"}, noun: "a heading"},
		{name: "a code block", markdown: "```\ncode here\n```", kinds: []string{"code_block"}, noun: "a code block"},
		{name: "a quote", markdown: "> Quoted aside.", kinds: []string{"blockquote"}, noun: "a blockquote"},
		{name: "a table", markdown: "| a | b |\n|---|---|\n| 1 | 2 |", kinds: []string{"table"}, noun: "a table"},
		{name: "a heading and a paragraph", markdown: "## Heading\n\nNoted.", kinds: []string{"heading", "paragraph"}, noun: "a heading"},
	}
	places := []struct {
		name string
		// child is the index of the parent's child the anchor names, and index the place among the
		// parent's children the blocks take.
		child, index int
		after        bool
	}{
		{name: "before the first child", child: 0, index: 0},
		{name: "after the first child", child: 0, index: 1, after: true},
		{name: "after a later child", child: 1, index: 2, after: true},
	}
	for _, parent := range parents {
		for _, place := range places {
			for _, block := range blocks {
				t.Run(parent.name+", "+place.name+", "+block.name, func(t *testing.T) {
					tree, err := parseInput(insertParentsFixture)
					if err != nil {
						t.Fatal(err)
					}
					childID := blockIDOfText(t, tree, parent.children[place.child])
					anchor, holderID := "block:"+childID, parentIDOfBlock(t, tree, childID)
					var kinds []string
					for _, child := range blockByID(tree, holderID).Children {
						kinds = append(kinds, child.Type)
					}
					if len(kinds) != 2 {
						t.Fatalf("%s holds %v in the fixture, want two paragraphs", parent.name, kinds)
					}
					op, field := model.EditOp{Op: "insert", Before: anchor, Markdown: block.markdown}, "before"
					if place.after {
						op, field = model.EditOp{Op: "insert", After: anchor, Markdown: block.markdown}, "after"
					}
					out, err := applyAlone(tree, op)
					if broken := parent.broken(place.index, block.noun); broken != "" {
						var refusal *ErrInsertDoesNotFit
						if !errors.As(err, &refusal) {
							t.Fatalf("insert of %s: error = %v, want ErrInsertDoesNotFit", block.name, err)
						}
						if refusal.Field != field || refusal.Anchor != anchor || refusal.HolderID != holderID || refusal.Broken != broken {
							t.Fatalf("refusal = %+v, want %s %q in %q, broken by %s", refusal, field, anchor, holderID, broken)
						}
						return
					}
					if err != nil {
						t.Fatalf("insert of %s: %v", block.name, err)
					}
					want := append(append(append([]string(nil), kinds[:place.index]...), block.kinds...), kinds[place.index:]...)
					holder := blockByID(out, holderID)
					if holder == nil {
						t.Fatalf("the document holds no block %q after the insert", holderID)
					}
					var got []string
					for _, child := range holder.Children {
						got = append(got, child.Type)
					}
					if !slices.Equal(got, want) {
						t.Errorf("%s holds %v after the insert, want %v: its %d children and the %d blocks the markdown holds", parent.name, got, want, len(kinds), len(block.kinds))
					}
					empty := false
					pmdoc.Walk(out, func(node *pmdoc.Node) bool {
						empty = empty || node.Type == "paragraph" && strings.TrimSpace(textOf(node)) == ""
						return !empty
					})
					if empty {
						t.Errorf("the document holds an empty paragraph: %s holds %v", parent.name, got)
					}
					markdown, err := renderTree(out)
					if err != nil {
						t.Fatal(err)
					}
					for _, line := range strings.Split(markdown, "\n") {
						if line != "" && strings.TrimSpace(line) == "" {
							t.Errorf("the stored markdown holds a line of indentation alone: %q", markdown)
							break
						}
					}
					reread, err := parseInput(markdown)
					if err != nil {
						t.Fatal(err)
					}
					if again, err := renderTree(reread); err != nil || again != markdown {
						t.Errorf("a read of the stored markdown renders %q, %v; want the same %q", again, err, markdown)
					}
				})
			}
		}
	}
}

func firstNodeOfType(node *pmdoc.Node, kind string) *pmdoc.Node {
	if node.Type == kind {
		return node
	}
	for _, child := range node.Children {
		if found := firstNodeOfType(child, kind); found != nil {
			return found
		}
	}
	return nil
}

func textOf(node *pmdoc.Node) string {
	var b strings.Builder
	b.WriteString(node.Text)
	for _, child := range node.Children {
		b.WriteString(textOf(child))
	}
	return b.String()
}

// parentIDOfBlock is the id of the block whose children hold the block carrying childID.
func parentIDOfBlock(t *testing.T, tree *pmdoc.Node, childID string) string {
	t.Helper()
	var id string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		for _, child := range node.Children {
			if child.Attrs[pmdoc.BlockIDAttr] == childID {
				id, _ = node.Attrs[pmdoc.BlockIDAttr].(string)
			}
		}
		return id == ""
	})
	if id == "" {
		t.Fatalf("no block carrying an id holds the block %q", childID)
	}
	return id
}
