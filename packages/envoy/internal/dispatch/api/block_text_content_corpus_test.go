package api

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The content blockTextContent cuts from a block of a comment, a message or an ask reads as the
// item's own parse reads what the block holds, for every text of three fixed corpora: an opening
// line at one of eight indentations over one line of sixteen under a fence at one of nine; tabs,
// containers, nested typed blocks and other shapes written by hand; and a fixed-seed generator of
// blocks whose opening line stands one to three spaces in. It needs no database.
// A text is read only where it reads as one typed block, the shape blockTextContent cuts; the
// others it answers as they stand. Of the texts it cuts, one whose block holds only blank lines
// has no content, and the item's block must then hold nothing (blockHoldingNothing). Every other
// content's tree is compared with the item's node by node (sameNodes), which allows for the first
// of the content's limits, columnsOff's: a text can hold spaces where the item's holds a tab, each
// run as wide as columnsOff can write it. The trees may differ in two other ways, each allowed for
// alone and counted. The limit blockTextContent names: the item's parse holds in its code the
// lines of only spaces and tabs, one or several, that end a code block which ends the content, an
// indented block or a fenced one its own fence does not close, and the content drops them; a text
// whose trees are the same once those lines are taken off the item's code block is set aside. And
// columnsOff's limit for a code span that crosses lines, one of whose ends a run written as spaces
// decides: such a span can differ from the item's by one space or line ending at each end, either
// way, and a text whose trees are the same once each such span is given the item's text
// (alignSpanEnds) is counted at the span's ends. Any other difference fails. The fourth limit,
// columnsOff's for a quote whose `>` stands four columns or more into a list item, is allowed for
// nowhere here, since no text of these corpora reaches it; TestBlockTextContentReadsEveryShape
// holds its shapes. Every count of each corpus is pinned, so a text that moves from one count to
// another fails too.
func TestBlockTextContentReadsTheItemCorporaAsTheItemDoes(t *testing.T) {
	t.Parallel()
	for _, corpus := range []struct {
		name  string
		texts []string
		want  corpusCounts
	}{
		{"openers by lines by fences", openerLineFenceTexts(),
			corpusCounts{texts: 1152, cut: 256, empty: 0, exact: 212, byRuns: 44, spanEnds: 0, setAside: 0}},
		{"tabs, containers and nested blocks", tabAndContainerTexts,
			corpusCounts{texts: 66, cut: 55, empty: 0, exact: 47, byRuns: 8, spanEnds: 0, setAside: 0}},
		{"random openers, markers and blank lines", randomItemTexts(7000),
			corpusCounts{texts: 7000, cut: 6947, empty: 636, exact: 3983, byRuns: 2224, spanEnds: 48, setAside: 56}},
	} {
		t.Run(corpus.name, func(t *testing.T) {
			counts := corpusCounts{texts: len(corpus.texts)}
			var failures []string
			for _, whole := range corpus.texts {
				if tree, err := pmdoc.ParseRendering(whole); err != nil || len(tree.Children) != 1 || !pmdoc.IsTypedBlock(tree.Children[0].Type) {
					continue
				}
				counts.cut++
				content, held := blockTextContent(whole, false), heldByItsOneBlock(t, whole)
				if content == "" {
					counts.empty++
					if same, _ := sameNodes(decodedTree(t, held), decodedTree(t, blockHoldingNothing)); !same {
						failures = append(failures, fmt.Sprintf("\n%q\n\tcut to nothing\n\twant     %s", whole, held))
					}
					continue
				}
				got := treeContent(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest))
				read, item := decodedTree(t, got), decodedTree(t, held)
				if same, byRuns := sameNodes(read, item); same && byRuns {
					counts.byRuns++
					continue
				} else if same {
					counts.exact++
					continue
				}
				if alignSpanEnds(read, item) {
					if same, _ := sameNodes(read, item); same {
						counts.spanEnds++
						continue
					}
				}
				if read := decodedTree(t, got); withoutBlankCodeEnd(item) {
					if same, _ := sameNodes(read, item); same {
						counts.setAside++
						continue
					}
				}
				failures = append(failures, fmt.Sprintf("\n%q\n\tcut %q\n\treads as %s\n\twant     %s", whole, content, got, held))
			}
			t.Logf("%d texts, %d read as one typed block, %d of those holding only blank lines; %d compared exactly, "+
				"%d compared per node with a text compared run by run (a tab in a text of the item's tree), %d with "+
				"a code span differing at its ends (one space or line ending at each end, either way), %d set aside "+
				"(whitespace-only lines ending a code block that ends the content)",
				counts.texts, counts.cut, counts.empty, counts.exact, counts.byRuns, counts.spanEnds, counts.setAside)
			if counts != corpus.want {
				t.Errorf("the counts are %+v, want %+v", counts, corpus.want)
			}
			if len(failures) > 0 {
				t.Errorf("%d of %d texts read otherwise than the item's own parse reads its block; the first:%s",
					len(failures), counts.cut, strings.Join(failures[:min(len(failures), 5)], ""))
			}
		})
	}
}

// corpusCounts is what the texts of a corpus come to: how many it holds, how many of them read as
// one typed block, and of those how many hold only blank lines, read as the item does compared
// exactly, read so with a text compared run by run, read so with a code span's stripped ends given
// back, and are set aside at the code block's end.
type corpusCounts struct {
	texts, cut, empty, exact, byRuns, spanEnds, setAside int
}

// blockHoldingNothing is what a typed block that holds nothing holds as the display read encodes
// it: the one empty paragraph the item's own parse reads a container holding nothing as holding
// (pmdoc's emptyParagraphFirst).
var blockHoldingNothing = json.RawMessage(`[{"type":"paragraph"}]`)

// withoutBlankCodeEnd takes off item, the decoded tree the item's own parse gives what a block
// holds, the lines of only spaces and tabs that end the code block ending item: its last node,
// or the last node of that node and so on down, fenced or indented. It reports whether it took
// any line off.
func withoutBlankCodeEnd(item any) bool {
	nodes, _ := item.([]any)
	if len(nodes) == 0 {
		return false
	}
	last, _ := nodes[len(nodes)-1].(map[string]any)
	if last["type"] != "code_block" {
		return withoutBlankCodeEnd(last["content"])
	}
	texts, _ := last["content"].([]any)
	if len(texts) != 1 {
		return false
	}
	node, _ := texts[0].(map[string]any)
	text, _ := node["text"].(string)
	lines := strings.Split(text, "\n")
	kept := len(lines)
	for kept > 0 && strings.Trim(lines[kept-1], " \t") == "" {
		kept--
	}
	if kept == len(lines) {
		return false
	}
	if kept == 0 {
		delete(last, "content")
	} else {
		node["text"] = strings.Join(lines[:kept], "\n")
	}
	return true
}

// randomItemTexts is count texts of a block whose opening line `:::callout{}` stands one to three
// spaces in and whose fence `:::` stands at the same indentation, drawn from a fixed seed. Each
// holds one to five lines: blank, whitespace only, or up to three runs of spaces or a tab,
// usually after the opener's own spaces, then up to two container markers (a list marker or
// `>`), each followed by spaces or tabs, then a word, a rule `***`, a backtick or nothing. It
// writes no fence, so every code block of its texts is indented, and a backtick at one line's end
// can open a code span that a backtick on a later line closes.
func randomItemTexts(count int) []string {
	random := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 1))
	pick := func(from ...string) string { return from[random.IntN(len(from))] }
	texts := make([]string, 0, count)
	for range count {
		opener := strings.Repeat(" ", 1+random.IntN(3))
		var text strings.Builder
		text.WriteString(opener + ":::callout{}\n")
		for range 1 + random.IntN(5) {
			switch random.IntN(8) {
			case 0:
			case 1:
				for range 1 + random.IntN(3) {
					text.WriteString(pick(" ", "  ", "\t"))
				}
			default:
				if random.IntN(4) != 0 {
					text.WriteString(opener)
				}
				for range random.IntN(4) {
					text.WriteString(pick(" ", "  ", "\t"))
				}
				for range random.IntN(3) {
					text.WriteString(pick("-", "*", "+", "1.", "2)", ">"))
					text.WriteString(pick(" ", "  ", "\t", " \t", "\t\t"))
				}
				text.WriteString(pick("word", "***", "", "`"))
			}
			text.WriteByte('\n')
		}
		texts = append(texts, text.String()+opener+":::")
	}
	return texts
}

// openerLineFenceTexts is the 1,152 texts of an opening line `:::callout{}` at one of eight
// indentations, over one line of sixteen, under a fence `:::` at one of nine.
func openerLineFenceTexts() []string {
	var texts []string
	for _, opener := range []string{"", " ", "  ", "   ", "    ", "\t", " \t", "  \t"} {
		for _, line := range []string{"Para.", " Para.", "  Para.", "   Para.", "    Code.", "     Code.", "      Code.", "\tTab.", " \tTab.", "  \tTab.", "   \tTab.", "\t\tTwo tabs.", " \t\tCode.", "  \t\tCode.", "  \t\t\tCode3.", "   \t\t\tCode3."} {
			for _, fence := range []string{"", " ", "  ", "   ", "    ", "     ", "      ", "\t", " \t"} {
				texts = append(texts, opener+":::callout{}\n"+line+"\n"+fence+":::")
			}
		}
	}
	return texts
}

// tabAndContainerTexts is 66 texts of tabs in fenced code, after a list marker and after `>`,
// of typed blocks nested in one, and of blank lines, CRLF, lists, fences, tabs inside a line,
// setext headings, HTML, opening lines and lazy lines.
var tabAndContainerTexts = []string{
	// tabs in fenced code
	"  :::callout{}\n  ```\n  \tcode\n  ```\n  :::",
	" :::callout{}\n ```\n \tcode\n ```\n :::",
	"   :::callout{}\n   ```\n   \tcode\n   ```\n   :::",
	"  :::callout{}\n  ```\n    \tcode\n  ```\n  :::",
	"  :::callout{}\n  ```\n\tcode\n  ```\n  :::",
	// tabs after a list marker or `>`
	"  :::callout{}\n  -\tItem.\n\n    Second para.\n  :::",
	"  :::callout{}\n  -\tItem.\n\n      Second para.\n  :::",
	"  :::callout{}\n  -\tItem.\n\n     Second para.\n  :::",
	"  :::callout{}\n  - Item.\n\n    Second para.\n  :::",
	"  :::callout{}\n  1.\tItem.\n\n     Second para.\n  :::",
	"  :::callout{}\n  1.\tItem.\n\n    Second para.\n  :::",
	" :::callout{}\n -\tItem.\n\n   Second para.\n :::",
	" :::callout{}\n -\tItem.\n\n    Second para.\n :::",
	"   :::callout{}\n   -\tItem.\n\n     Second para.\n   :::",
	"  :::callout{}\n  -\t\tCodeInItem.\n  :::",
	"  :::callout{}\n  >\tQuote.\n  :::",
	"  :::callout{}\n  >\t\tQuoteCode.\n  :::",
	" :::callout{}\n >\t\tQuoteCode.\n :::",
	"  :::callout{}\n  > -\tItem.\n  :::",
	// nested typed blocks
	"  ::::callout{}\n  :::note{}\n  Inner.\n  :::\n  ::::",
	"  ::::callout{}\n    :::note{}\n    Inner.\n    :::\n  ::::",
	"  ::::callout{}\n  :::note{}\n  \tInnerTab.\n  :::\n  ::::",
	"  ::::callout{}\n   :::note{}\n   \tInnerTab.\n   :::\n  ::::",
	"  ::::callout{}\n  :::note{}\n      InnerCode.\n  :::\n  ::::",
	"  ::::callout{}\n      :::note{}\n  ::::",
	"  ::::callout{}\n     :::\n  ::::",
	"  ::::callout{}\n  :::callout{}\n  Inner.\n  :::\n  ::::",
	"  ::::callout{}\n    :::callout{}\n    Inner.\n    :::\n  ::::",
	"  ::::callout{}\n  :::callout{}\n  \tInnerTab.\n  :::\n  ::::",
	"  ::::callout{}\n   :::callout{}\n   \tInnerTab.\n   :::\n  ::::",
	"  ::::callout{}\n  :::callout{}\n      InnerCode.\n  :::\n  ::::",
	"  ::::callout{}\n  :::callout{}\n  ```\n  \tx\n  ```\n  :::\n  ::::",
	" ::::callout{}\n   :::callout{}\n   Inner.\n     :::\n ::::",
	"   ::::callout{}\n\t:::callout{}\n\tInner.\n\t:::\n   ::::",
	"  ::::callout{}\n  \t:::callout{}\n  \tInner.\n  \t:::\n  ::::",
	"  ::::callout{}\n      :::\n  ::::",
	"  :::callout{}\n     ::::\n  :::",
	// blank lines
	"  :::callout{}\n\n  Para.\n\n  :::",
	"  :::callout{}\n  Para.\n   \n  Two.\n  :::",
	"  :::callout{}\n      a\n        \n      b\n  :::",
	"  :::callout{}\n      a\n  \t\t\n      b\n  :::",
	"  :::callout{}\n      a\n\t\n      b\n  :::",
	"  :::callout{}\n  ```\n  a\n     \n  b\n  ```\n  :::",
	"  :::callout{}\n  ```\n  a\n  \t\n  b\n  ```\n  :::",
	// CRLF
	"  :::callout{}\r\n  CRLF.\r\n  :::",
	"  :::callout{}\r\n  Para.\r\n  :::\r",
	// lists
	"  :::callout{}\n  - a\n  - b\n\n    more\n  :::",
	"  :::callout{}\n  - a\n    - b\n  :::",
	"  :::callout{}\n  - a\n\t- b\n  :::",
	"  :::callout{}\n  - a\n  \t- b\n  :::",
	// code fence indentation
	"  :::callout{}\n    ```\n    x\n    ```\n  :::",
	"  :::callout{}\n   ```\n  x\n   ```\n  :::",
	"  :::callout{}\n  ~~~\n   x\n  ~~~\n  :::",
	"  :::callout{}\n  ```\n  :::\n  ```\n  :::",
	"  ::::callout{}\n  ```\n  :::\n  ```\n  ::::",
	"  ::::callout{}\n  ```\n     :::\n  ```\n  ::::",
	// tabs inside a line
	"  :::callout{}\n  a\tb\n  :::",
	"  :::callout{}\n      a\tb\n  :::",
	"  :::callout{}\n  | a\t| b |\n  |---|---|\n  :::",
	// setext headings
	"  :::callout{}\n  Title\n  \t===\n  :::",
	"  :::callout{}\n  Title\n  ===\n  :::",
	// HTML
	"  :::callout{}\n  <div>\n  \tx\n  </div>\n  :::",
	// opening lines
	"  :::callout{} \n  T.\n  :::",
	"  :::callout{title=\"x\"}\n  T.\n  :::",
	// lazy lines
	"   :::callout{}\n   > q\n lazy\n   :::",
	"  :::callout{}\n  para\n      lazy code?\n  :::",
}
