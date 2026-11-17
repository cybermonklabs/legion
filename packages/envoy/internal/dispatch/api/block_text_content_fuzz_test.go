package api

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The content blockTextContent cuts from a block of a comment, a message or an ask reads as the
// item's own parse reads what the block holds, for every text of three corpora: the hand-written
// texts and the nested texts of the deep review of PR #4178, and a fixed-seed generator of blocks
// whose opening line stands one to three spaces in. It needs no database.
// A text is read only where it reads as one typed block, the shape blockTextContent cuts; the
// others it answers as they stand. Of the texts it cuts, one whose block holds only blank lines
// has no content, and every other is compared but one shape:
//
//   - where the item's tree holds no tab in a text, the content's tree is the item's exactly;
//   - where it holds one, the content can hold spaces in its place (columnsOff's limit), so the
//     two trees are compared with each run of spaces and tabs in a text as one space;
//   - where a line of only spaces and tabs ends an indented code block before the fence, the
//     item's code holds that line and the content's does not (blockTextContent), so the text is
//     left out and counted.
func TestBlockTextContentReadsTheItemCorporaAsTheItemDoes(t *testing.T) {
	for _, corpus := range []struct {
		name                       string
		texts                      []string
		exactFloor, structureFloor int
	}{
		{"the review's hand-written texts", reviewHandTexts(), 200, 25},
		{"the review's nested texts", reviewNestedTexts, 5, 1},
		{"the random texts", randomItemTexts(7000), 3000, 1000},
	} {
		t.Run(corpus.name, func(t *testing.T) {
			var cut, empty, exact, structure, leftOut int
			var failures []string
			for _, whole := range corpus.texts {
				if tree, err := pmdoc.ParseRendering(whole); err != nil || len(tree.Children) != 1 || !pmdoc.IsTypedBlock(tree.Children[0].Type) {
					continue
				}
				cut++
				content := blockTextContent(whole, false)
				if content == "" {
					empty++
					continue
				}
				want := sameShape(t, heldByItsOneBlock(t, whole))
				if endsInABlankLine(whole) && endsCodeInABlankLine(t, want) {
					leftOut++
					continue
				}
				got := sameShape(t, treeContent(t, pmdoc.DisplayJSON(content, pmdoc.DisplayLatest)))
				if holdsATab(t, want) {
					structure++
					got, want = whitespaceCollapsed(t, got), whitespaceCollapsed(t, want)
				} else {
					exact++
				}
				if !bytes.Equal(got, want) {
					failures = append(failures, fmt.Sprintf("\n%q\n\tcut %q\n\treads as %s\n\twant     %s", whole, content, got, want))
				}
			}
			t.Logf("%d texts, %d read as one typed block, %d of those holding only blank lines; %d compared exactly, "+
				"%d with whitespace collapsed (a tab in a text of the item's tree), %d left out (a whitespace-only "+
				"line ending an indented code block before the fence)",
				len(corpus.texts), cut, empty, exact, structure, leftOut)
			if exact < corpus.exactFloor || structure < corpus.structureFloor {
				t.Fatalf("%d texts compared exactly and %d with whitespace collapsed, want at least %d and %d",
					exact, structure, corpus.exactFloor, corpus.structureFloor)
			}
			if len(failures) > 0 {
				t.Errorf("%d of %d contents read otherwise than the item's own parse reads its block; the first:%s",
					len(failures), exact+structure, strings.Join(failures[:min(len(failures), 5)], ""))
			}
		})
	}
}

// randomItemTexts is count texts of a block whose opening line `:::callout{}` stands one to three
// spaces in and whose fence `:::` stands at the same indentation, drawn from a fixed seed. Each
// holds one to five lines: blank, whitespace only, or up to three runs of spaces or a tab,
// usually after the opener's own spaces, then up to two container markers (a list marker or
// `>`), each followed by spaces or tabs, then a word, a rule `***` or nothing. It writes no fence,
// so every code block of its texts is indented.
func randomItemTexts(count int) []string {
	random := rand.New(rand.NewPCG(175, 3910))
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
				text.WriteString(pick("word", "***", ""))
			}
			text.WriteByte('\n')
		}
		texts = append(texts, text.String()+opener+":::")
	}
	return texts
}

// reviewHandTexts is the deep review's 1,207 hand-written texts in the order they ran: 1,152 of an
// opening line at one of eight indentations, over one line of sixteen, under a fence at one of
// nine, then reviewExtraTexts.
func reviewHandTexts() []string {
	var texts []string
	for _, opener := range []string{"", " ", "  ", "   ", "    ", "\t", " \t", "  \t"} {
		for _, line := range []string{"Para.", " Para.", "  Para.", "   Para.", "    Code.", "     Code.", "      Code.", "\tTab.", " \tTab.", "  \tTab.", "   \tTab.", "\t\tTwo tabs.", " \t\tCode.", "  \t\tCode.", "  \t\t\tCode3.", "   \t\t\tCode3."} {
			for _, fence := range []string{"", " ", "  ", "   ", "    ", "     ", "      ", "\t", " \t"} {
				texts = append(texts, opener+":::callout{}\n"+line+"\n"+fence+":::")
			}
		}
	}
	return append(texts, reviewExtraTexts...)
}

// reviewExtraTexts is the 55 shapes the deep review added after the 1,152: tabs in fenced code,
// after a list marker and after `>`, nested blocks, blank lines, CRLF, lists, fences, tabs inside
// a line, setext headings, HTML, opening lines and lazy lines.
var reviewExtraTexts = []string{
	"  :::callout{}\n  ```\n  \tcode\n  ```\n  :::",
	" :::callout{}\n ```\n \tcode\n ```\n :::",
	"   :::callout{}\n   ```\n   \tcode\n   ```\n   :::",
	"  :::callout{}\n  ```\n    \tcode\n  ```\n  :::",
	"  :::callout{}\n  ```\n\tcode\n  ```\n  :::",
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
	// nested typed block
	"  ::::callout{}\n  :::note{}\n  Inner.\n  :::\n  ::::",
	"  ::::callout{}\n    :::note{}\n    Inner.\n    :::\n  ::::",
	"  ::::callout{}\n  :::note{}\n  \tInnerTab.\n  :::\n  ::::",
	"  ::::callout{}\n   :::note{}\n   \tInnerTab.\n   :::\n  ::::",
	"  ::::callout{}\n  :::note{}\n      InnerCode.\n  :::\n  ::::",
	"  ::::callout{}\n      :::note{}\n  ::::",
	"  ::::callout{}\n     :::\n  ::::",
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
	// list
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
	// mid-line tabs
	"  :::callout{}\n  a\tb\n  :::",
	"  :::callout{}\n      a\tb\n  :::",
	"  :::callout{}\n  | a\t| b |\n  |---|---|\n  :::",
	// heading / setext
	"  :::callout{}\n  Title\n  \t===\n  :::",
	"  :::callout{}\n  Title\n  ===\n  :::",
	// html
	"  :::callout{}\n  <div>\n  \tx\n  </div>\n  :::",
	// opener with trailing space
	"  :::callout{} \n  T.\n  :::",
	"  :::callout{title=\"x\"}\n  T.\n  :::",
	// lazy continuation
	"   :::callout{}\n   > q\n lazy\n   :::",
	"  :::callout{}\n  para\n      lazy code?\n  :::",
}

// reviewNestedTexts is the deep review's 12 texts of a typed block nested in one under an
// indented opening line.
var reviewNestedTexts = []string{
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
	"  ::::callout{}\n     :::\n  ::::",
	"  :::callout{}\n     ::::\n  :::",
}

// endsInABlankLine reports whether the last line of whole before its fence holds only spaces and
// tabs.
func endsInABlankLine(whole string) bool {
	lines := strings.Split(whole, "\n")
	return len(lines) > 2 && strings.Trim(lines[len(lines)-2], " \t") == ""
}

// endsCodeInABlankLine reports whether a code block of tree, a block's content as the display
// read encodes it, ends in a line of only spaces and tabs.
func endsCodeInABlankLine(t *testing.T, tree []byte) bool {
	t.Helper()
	ends := false
	displayTexts(t, tree, func(node map[string]any, inCode bool) {
		text := node["text"].(string)
		if at := strings.LastIndexByte(text, '\n'); inCode && at >= 0 && strings.Trim(text[at+1:], " \t") == "" {
			ends = true
		}
	})
	return ends
}
