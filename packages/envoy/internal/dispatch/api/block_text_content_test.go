package api

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// blockTextContent answers every shape of a block's stored text found so far with the content
// its text_tree is read from, and reads no database. A document's block, which can stand in a
// list item or a footnote definition, has the closing fence's indentation taken out of its lines.
// A block of a comment, a message or an ask, stored from the start of its line, has its opening
// line's indentation, up to three spaces, taken off it and up to as many columns off each later
// line, and its content reads as the item's own parse reads what the block holds, node by node as
// sameNodes compares them. A text that does not read back as one typed block is answered as it
// stands. The documents' texts are slices of a rendering, as the block index stores them; the
// items' texts are as a comment writes them.
func TestBlockTextContentReadsEveryShape(t *testing.T) {
	const document, item = true, false
	// quoteLimit begins the name of a row at the limit columnsOff names for a quote whose `>`
	// stands four columns or more into a list item. Such a row's content is checked to read
	// otherwise than the item, so a change that brings it to the item's reading shows.
	const quoteLimit = "at the limit of a quote in a list item: "
	for _, row := range []struct {
		name     string
		document bool
		whole    string
		want     string
	}{
		{"a document's top-level block", document,
			":::requirement{#r1}\nAn answered ask leaves the list.\n:::",
			"An answered ask leaves the list.\n"},
		{"a document's block in a list item, its lines two spaces in", document,
			":::requirement{#l1}\n  Nested in a list.\n  :::",
			"Nested in a list.\n"},
		{"a document's block after the marker `10. `, its two paragraphs four spaces in", document,
			":::requirement{#l10}\n    Nested in item 10.\n\n    Its second paragraph.\n    :::",
			"Nested in item 10.\n\nIts second paragraph.\n"},
		{"a document's block two levels deep in a list, its lines four spaces in", document,
			":::requirement{#r2}\n    Deep in a nested list.\n    :::",
			"Deep in a nested list.\n"},
		{"a document's block in a footnote definition, its lines four spaces in", document,
			":::requirement{#f1}\n    In a footnote.\n    :::",
			"In a footnote.\n"},
		{"a document's block in a list item with a blank line written without the indentation, which is kept", document,
			":::requirement{#l2}\n  First.\n\n  Second.\n  :::",
			"First.\n\nSecond.\n"},
		{"a document's block with a line that does not carry the fence's indentation", document,
			":::requirement{#r1}\nNot carried.\n  :::",
			":::requirement{#r1}\nNot carried.\n  :::"},
		{"a document's block in a quote, answered with its opening line", document,
			":::requirement{#q1}\n> Nested in a quote.\n> :::",
			":::requirement{#q1}\n> Nested in a quote.\n> :::"},
		{"a document's empty block", document,
			":::requirement{#e1}\n\n:::",
			""},

		{"an item's block holding another, neither written with an id", item,
			"::::callout{}\n:::callout{}\nInner words.\n:::\n::::",
			":::callout{}\nInner words.\n:::\n"},
		{"an item's block opening with a rule `---` and holding a second one", item,
			":::callout{}\n---\n\nAfter a rule.\n\n---\n\nTail.\n:::",
			"***\n\nAfter a rule.\n\n---\n\nTail.\n"},
		{"an item's block whose fence stands two spaces in, over a line at column 0", item,
			":::callout{}\nText of the callout.\n  :::",
			"Text of the callout.\n"},
		{"an item's block whose fence stands three spaces in, over a line at column 0", item,
			":::callout{}\nText of the callout, its fence three spaces in.\n   :::",
			"Text of the callout, its fence three spaces in.\n"},
		{"an item's block whose fence and lines stand two spaces in, with a line four spaces in that the item reads as code", item,
			":::callout{}\n  Intro\n\n    # Not a heading\n  :::",
			"  Intro\n\n    # Not a heading\n"},
		{"an item's block whose fence and every line stand two spaces in", item,
			":::callout{}\n  Every line carries the spaces.\n  :::",
			"  Every line carries the spaces.\n"},
		{"an item's block whose fence stands four spaces in, over a line at column 0", item,
			":::callout{}\nFour in.\n    :::",
			":::callout{}\nFour in.\n    :::"},
		{"an item's block whose fence stands four spaces in, over a line four spaces in", item,
			":::callout{}\n    Four in.\n    :::",
			":::callout{}\n    Four in.\n    :::"},
		{"an item's block whose fence stands a tab in, over a line at column 0", item,
			":::callout{}\nA tab in.\n\t:::",
			":::callout{}\nA tab in.\n\t:::"},
		{"an item's block whose fence stands a tab in, over a line a tab in", item,
			":::callout{}\n\tA tab in.\n\t:::",
			":::callout{}\n\tA tab in.\n\t:::"},
		{"an item's block whose fence is shorter than its opening", item,
			"::::callout{}\nShorter fence.\n:::",
			"::::callout{}\nShorter fence.\n:::"},
		{"an item's text that is no typed block", item,
			"Not a typed block.",
			"Not a typed block."},
		{"an item's block with CRLF line ends", item,
			":::callout{}\r\nCRLF.\r\n:::",
			":::callout{}\r\nCRLF.\r\n:::"},
		{"an item's block holding `:::` in a fenced code block", item,
			"::::callout{}\n```\n:::\n```\n::::",
			"```\n:::\n```\n"},
		{"an item's block with a line feed after its fence", item,
			":::callout{}\nThen a line feed.\n:::\n",
			":::callout{}\nThen a line feed.\n:::\n"},
		{"an item's block in a quote, answered with its opening line", item,
			"> :::callout{}\n> Quoted.\n> :::",
			"> :::callout{}\n> Quoted.\n> :::"},
		{"an item's empty block", item,
			":::callout{}\n:::",
			""},
		// A comment's or a message's block is its body from the start of the line it starts on
		// (itemblocks.Split), so the block of `Intro.\n\n  :::callout{}\n  Text.\n  :::` is stored
		// from the callout's two spaces on, without the paragraph. An ask's part is the ask's
		// question, an option or its answer whole, as written (refs.AskParts), so a question that
		// is such a callout is a part of that same text.
		{"an item's block whose opening line stands two spaces in, after a paragraph", item,
			"  :::callout{}\n  Text.\n  :::",
			"Text.\n"},
		{"an item's block whose opening line stands three spaces in, over a line at column 0", item,
			"   :::callout{}\nAt column 0.\n   :::",
			"At column 0.\n"},
		{"an item's text whose opening line stands four spaces in, which opens no typed block", item,
			"    :::callout{}\n    Four in.\n    :::",
			"    :::callout{}\n    Four in.\n    :::"},
		{"an item's text whose opening line stands a tab in, which opens no typed block", item,
			"\t:::callout{}\n\tA tab in.\n\t:::",
			"\t:::callout{}\n\tA tab in.\n\t:::"},
		{"an item's block whose opening line stands two spaces in, over a line four spaces in", item,
			"  :::callout{}\n    Para four in.\n  :::",
			"  Para four in.\n"},
		{"an item's block whose opening line stands two spaces in, over a line six spaces in", item,
			"  :::callout{}\n      Code six in.\n  :::",
			"    Code six in.\n"},
		{"an item's block whose opening line stands one space in, over a line a tab in", item,
			" :::callout{}\n\tTabbed.\n :::",
			"   Tabbed.\n"},
		{"an item's block whose opening line stands two spaces in, over a line two spaces and a tab in", item,
			"  :::callout{}\n  \tTab past two.\n  :::",
			"  Tab past two.\n"},
		{"an item's block whose opening line stands one space in, over a code line a space and two tabs in", item,
			" :::callout{}\n \t\tCode across a tab.\n :::",
			"       Code across a tab.\n"},
		{"an item's block whose opening line stands two spaces in, over a list item whose marker a tab follows and a second paragraph of that item", item,
			"  :::callout{}\n  -\tItem.\n\n    Second para.\n  :::",
			"- Item.\n\n  Second para.\n"},
		{"an item's block whose opening line stands two spaces in, over an ordered item whose three-digit marker a tab follows and a second paragraph of that item", item,
			"  :::callout{}\n  100.\tFirst.\n\n        Second.\n  :::",
			"100.  First.\n\n      Second.\n"},
		{"an item's block whose opening line stands two spaces in, over an ordered item whose nine-digit marker a tab follows and a second paragraph of that item", item,
			"  :::callout{}\n  123456789.\tFirst.\n\n              Second.\n  :::",
			"123456789.    First.\n\n            Second.\n"},
		{"an item's block whose opening line stands one space in, over a list item whose marker a tab follows and a rule a space and a tab in", item,
			" :::callout{}\n -\titem\n \t***\n   :::",
			"-  item\n   ***\n"},
		{"an item's block whose opening line stands two spaces in, over a list item whose marker two tabs follow", item,
			"  :::callout{}\n  -\t\tCodeInItem.\n  :::",
			"-     CodeInItem.\n"},
		// Past a run five columns or more wide the walk reads markers still. Here the second `-`
		// and its tab are code, which the item holds with the tab: the limit columnsOff names, so
		// sameNodes compares that text with its whitespace collapsed.
		{"an item's block whose opening line stands two spaces in, over a list item whose marker two tabs follow, then in its code a second marker and a tab", item,
			"  :::callout{}\n  -\t\t-\tx\n  :::",
			"-     -   x\n"},
		{"an item's block whose opening line stands one space in, over a list item in a quote, then a line whose `>` two tabs follow and whose `-` opens a list in that item", item,
			" :::callout{}\n > 1. a\n >\t\t-\t\tb\n :::",
			"> 1. a\n>      -       b\n"},
		{"an item's block whose opening line stands two spaces in, over a quote whose `>` two tabs follow", item,
			"  :::callout{}\n  >\t\tCodeInItem.\n  :::",
			">     CodeInItem.\n"},
		{"an item's block whose opening line stands one space in, over code in a quote whose `>` two tabs follow, a line of that `>` and two tabs alone, and more of that code", item,
			" :::callout{}\n >\t\tcode\n >\t\t\n >\t\tmore\n :::",
			">      code\n>      \n>      more\n"},
		{"an item's block whose opening line stands one space in, over code in a quote in a quote whose second `>` stands four columns past the first, a line of those `>` and two tabs alone, and more of that code", item,
			" :::callout{}\n >    >\t\tcode\n >    >\t\t\n >    >\t\tmore\n :::",
			">    >     code\n>    >     \n>    >     more\n"},
		// A `>` that columnsOff cannot be sure opens or continues a quote can be a paragraph's
		// text, so its run to the line's end is kept as it stands: written as spaces, it would end
		// the paragraph's line in a hard break.
		{"an item's block whose opening line stands two spaces in, over a paragraph whose lazy line is a `>` six spaces in and two tabs", item,
			"  :::callout{}\n  foo\n        >\t\t\n  bar\n  :::",
			"foo\n      >\t\t\nbar\n"},
		{"an item's block whose opening line stands two spaces in, over a paragraph and a line of `2) `, which opens no list there, a `>` and two tabs", item,
			"  :::callout{}\n  foo\n  2) >\t\t\n  bar\n  :::",
			"foo\n2) >\t\t\nbar\n"},
		// The limit columnsOff names for a quote whose `>` stands four columns or more into a
		// list item: the walk does not take that `>` for a quote's, so on a line of only spaces
		// and tabs in the quote's code its run is kept as it stands, and that line holds up to
		// three columns more or fewer of whitespace than the item's, the structure the same.
		// These rows hold the cut as it is today.
		{quoteLimit + "an item's block whose opening line stands one space in, over code in a quote after `10. `, a line of that `>` and two tabs alone, and more of that code", item,
			" :::callout{}\n 10. >\t\tcode\n     >\t\t\n     >\t\tmore\n :::",
			"10. >      code\n    >\t\t\n    >      more\n"},
		{quoteLimit + "an item's block whose opening line stands one space in, over code in a quote after `-   `, a line of that `>` and two tabs alone, and more of that code", item,
			" :::callout{}\n -   >\t\tcode\n     >\t\t\n     >\t\tmore\n :::",
			"-   >      code\n    >\t\t\n    >      more\n"},
		{quoteLimit + "an item's block whose opening line stands one space in, over code in a quote after `> 1.  `, a line of those `>` and two tabs alone, and more of that code", item,
			" :::callout{}\n > 1.  >\t\tcode\n >     >\t\t\n >     >\t\tmore\n :::",
			"> 1.  >        code\n>     >\t\t\n>     >        more\n"},
		{"an item's block whose opening line stands two spaces in, over a code line six spaces in, a line of spaces and another code line", item,
			"  :::callout{}\n      a\n        \n      b\n  :::",
			"    a\n      \n    b\n"},
		// The item keeps the tab in its code's text, and the cut writes it as spaces: the limit
		// columnsOff names, so sameNodes compares that text with its whitespace collapsed.
		{"an item's block whose opening line stands two spaces in, over a fenced code line a tab past the fence", item,
			"  :::callout{}\n  ```\n  \tcode\n  ```\n  :::",
			"```\n  code\n```\n"},
	} {
		t.Run(row.name, func(t *testing.T) {
			content := blockTextContent(row.whole, row.document)
			if content != row.want {
				t.Fatalf("blockTextContent(%q, document=%t) = %q, want %q", row.whole, row.document, content, row.want)
			}
			if row.document || content == row.whole || content == "" {
				return
			}
			got := treeContent(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest))
			want := heldByItsOneBlock(t, row.whole)
			same, _ := sameNodes(decodedTree(t, got), decodedTree(t, want))
			if atTheLimit := strings.HasPrefix(row.name, quoteLimit); atTheLimit && same {
				t.Errorf("the content %q reads as the item's own parse reads the block, %s, so the row is no longer at the limit", content, got)
			} else if !atTheLimit && !same {
				t.Errorf("the content %q reads as\n\t%s\nwant what the item's own parse reads in the block\n\t%s", content, got, want)
			}
		})
	}
}

// treeContent is the content of the node tree encodes: a document's blocks, or a block's own.
func treeContent(t *testing.T, tree json.RawMessage) json.RawMessage {
	t.Helper()
	var node struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(tree, &node); err != nil {
		t.Fatalf("decode the tree %s: %v", tree, err)
	}
	return node.Content
}

// heldByItsOneBlock is the tree of what the one block of whole holds, as the display read of
// whole itself reads it: the reading the item's own parse gives that block's lines.
func heldByItsOneBlock(t *testing.T, whole string) json.RawMessage {
	t.Helper()
	var blocks []json.RawMessage
	tree := pmdoc.DisplayJSON(whole, pmdoc.DisplayLatest)
	if err := json.Unmarshal(treeContent(t, tree), &blocks); err != nil || len(blocks) != 1 {
		t.Fatalf("%q reads as %s, want one block (%v)", whole, tree, err)
	}
	return treeContent(t, blocks[0])
}

// decodedTree is tree, a node's content as the display read encodes it, decoded.
func decodedTree(t *testing.T, tree json.RawMessage) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(tree, &value); err != nil {
		t.Fatalf("decode the tree %s: %v", tree, err)
	}
	return value
}

// sameNodes walks content, the decoded tree a block's content reads as, and item, the decoded
// tree the item's own parse gives what the block holds, together, and reports whether they are
// the same node by node. Where a text node of item holds a tab, its text and the text of
// content's node in its place are compared with each run of spaces and tabs as one space, since
// there the content can hold spaces where the item holds a tab (columnsOff). Everything else is
// compared exactly: every other text, each node's type, attrs and marks, and the shape, each
// node's keys and each list's count of nodes. collapsed reports whether a text was compared with
// its whitespace collapsed.
func sameNodes(content, item any) (same, collapsed bool) {
	switch item := item.(type) {
	case []any:
		nodes, isList := content.([]any)
		if !isList || len(nodes) != len(item) {
			return false, false
		}
		for index, child := range item {
			sameChild, collapsedChild := sameNodes(nodes[index], child)
			collapsed = collapsed || collapsedChild
			if !sameChild {
				return false, collapsed
			}
		}
		return true, collapsed
	case map[string]any:
		node, isNode := content.(map[string]any)
		if !isNode || len(node) != len(item) {
			return false, false
		}
		for key, value := range item {
			other, has := node[key]
			if !has {
				return false, collapsed
			}
			if text, _ := value.(string); key == "text" && item["type"] == "text" && strings.Contains(text, "\t") {
				otherText, isText := other.(string)
				if !isText || whitespaceRuns.ReplaceAllString(otherText, " ") != whitespaceRuns.ReplaceAllString(text, " ") {
					return false, true
				}
				collapsed = true
				continue
			}
			sameValue, collapsedValue := sameNodes(other, value)
			collapsed = collapsed || collapsedValue
			if !sameValue {
				return false, collapsed
			}
		}
		return true, collapsed
	default:
		return content == item, false
	}
}

// whitespaceRuns is a run of spaces and tabs.
var whitespaceRuns = regexp.MustCompile(`[ \t]+`)
