package textcheck

import (
	"regexp"
	"strings"
	"testing"
)

// visibleMarkdown is text as the rendered page shows it, which reads an HTML comment as CommonMark
// does: each comment outside a fenced block is blanked, its newlines kept, so that every line keeps
// its number. A line the page passes on as raw HTML hides from each "<!--" on it to the next "-->",
// wherever that is, and to the end of the text where none follows: a line that a "<!--" opens, and
// each line from one that a tag opens to the next blank line. A "<!--" inside a line of a paragraph
// is a comment only where a "-->" closes it before its paragraph ends (inlineCommentEnd); otherwise
// it is text the page shows. Where the reading and CommonMark part (a comment opened after a list
// item's marker, which the page ends with its item, or a paragraph that ends at a heading), the
// reading hides more than the page does, so a check refuses what a reader may see and never reads
// what a reader cannot. A fenced block, from a line opening "```" to the next, shows its lines as
// written, a "<!--" among them, and a fence inside a comment, or inside a block that a tag opens,
// opens nothing. A check reads the guide through this before it selects a line, so that a comment
// opened before that line still hides it.
func visibleMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	fenced, commented, html := false, false, false
	// i is moved past the lines of a comment inside a paragraph as well as by the loop.
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !commented && !html && strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		html = rawHTMLBlock(line, html)
		raw := commented || html || commentOpening.MatchString(line)
		var shown strings.Builder
		for line != "" {
			if commented {
				var closed bool
				_, line, closed = strings.Cut(line, "-->")
				commented = !closed
				continue
			}
			before, after, opens := strings.Cut(line, "<!--")
			shown.WriteString(before)
			if !opens {
				break
			}
			if raw {
				line, commented = after, true
				continue
			}
			end, rest, closed := inlineCommentEnd(lines, i, after)
			if !closed {
				shown.WriteString("<!--")
				line = after
				continue
			}
			// The comment's own lines show nothing; its last shows what follows its "-->".
			for ; i < end; i++ {
				lines[i] = shown.String()
				shown.Reset()
				html = rawHTMLBlock(lines[i+1], html)
			}
			raw = html || commentOpening.MatchString(lines[i])
			line = rest
		}
		lines[i] = shown.String()
	}
	return strings.Join(lines, "\n")
}

var (
	// commentOpening matches a line that a "<!--" opens, after its indent and any list item's or
	// quote's markers. It matches more lines than CommonMark opens an HTML block on (an indented
	// code line, an item that begins no list), and each is read as raw HTML, which hides more.
	commentOpening = regexp.MustCompile(`^[ \t]*(?:(?:[-*+>]|[0-9]{1,9}[.)])[ \t]*)*<!--`)
	// tagOpening matches a line that a tag or a declaration opens, after the same markers: an HTML
	// block of another kind than a comment's, which runs to the next blank line. A placeholder such
	// as "<name>" opening a line of prose matches too, and is read as raw HTML, which hides more.
	tagOpening = regexp.MustCompile(`^[ \t]*(?:(?:[-*+>]|[0-9]{1,9}[.)])[ \t]*)*<(?:[A-Za-z/?]|![A-Za-z]|!\[CDATA\[)`)
	// paragraphCommentOpening matches a line that a "<!--" opens where CommonMark ends a paragraph
	// before it: at most three spaces of indent and only markers that may interrupt a paragraph. It
	// matches fewer lines than CommonMark ends a paragraph at, so a comment inside a line runs on
	// past such a line, which hides more.
	paragraphCommentOpening = regexp.MustCompile(`^ {0,3}(?:(?:[-*+]|1[.)])[ \t]+|>[ \t]?)*<!--`)
)

// rawHTMLBlock is whether line is read as a line of an HTML block of another kind than a comment's:
// one that a tag opens, or one after it before a blank line. html is whether the line before it was.
func rawHTMLBlock(line string, html bool) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	return html || tagOpening.MatchString(line)
}

// inlineCommentEnd is where a comment opened inside line i of lines closes, after being the rest of
// that line: the number of the line holding its "-->" and what follows the "-->" there. Its
// paragraph ends before that at a blank line, a fence or a line a "<!--" opens
// (paragraphCommentOpening), and then closed is false: the page shows the "<!--" as text.
func inlineCommentEnd(lines []string, i int, after string) (end int, rest string, closed bool) {
	for {
		if _, rest, closed := strings.Cut(after, "-->"); closed {
			return i, rest, true
		}
		i++
		if i == len(lines) || strings.TrimSpace(lines[i]) == "" || strings.HasPrefix(lines[i], "```") ||
			paragraphCommentOpening.MatchString(lines[i]) {
			return 0, "", false
		}
		after = lines[i]
	}
}

// visibleMarkdown hides what the rendered page hides and keeps every line's number: a comment
// around a fenced block hides the block, its fences among it; a comment around a list hides the
// list; a comment inside a line hides that part of it, across the lines of its paragraph; a comment
// opened on a line of its own, after a list item's marker, or inside a line of a block that a tag
// opens, and never closed, hides the rest of the text. A "<!--" inside a line that its paragraph
// never closes is text the page shows, and so is a "<!--" inside a fenced block, so the lines after
// either stay as written. A fence inside a block that a tag opens is a line of that block and opens
// no fenced block, so a comment it is followed by hides as the block's would.
func TestVisibleMarkdownShowsWhatTheRenderedPageShows(t *testing.T) {
	for _, text := range []struct{ shape, written, shown string }{
		{"a comment around a fenced block",
			"Run this:\n<!--\n```sh\nlegion start\n```\n-->\nThen this.",
			"Run this:\n\n\n\n\n\nThen this."},
		{"a comment around a list",
			"The list:\n<!--\n- one\n- two\n-->\nAfter it.",
			"The list:\n\n\n\n\nAfter it."},
		{"a comment inside a line",
			"Shown <!-- hidden --> and shown.",
			"Shown  and shown."},
		{"a comment inside a line, closed on a later line of its paragraph",
			"Shown <!-- hidden\nhidden --> and shown.",
			"Shown \n and shown."},
		{"a comment never closed, opened on a line of its own",
			"Shown.\n<!-- hidden\nhidden too.",
			"Shown.\n\n"},
		{"a comment never closed, opened after a list item's marker",
			"- <!-- hidden\n- hidden too, though the page ends the comment with its item",
			"- \n"},
		{"a \"<!--\" inside a line that its paragraph never closes",
			"Shown. <!-- shown too\nshown.\n\nShown --> as well.",
			"Shown. <!-- shown too\nshown.\n\nShown --> as well."},
		{"a \"<!--\" inside a line before a comment on a line of its own, which ends the paragraph",
			"Shown <!-- shown\n<!-- hidden -->\nShown.",
			"Shown <!-- shown\n\nShown."},
		{"a comment never closed inside a line of a block that a tag opens",
			"<div>\nShown <!-- hidden\n\nhidden too.",
			"<div>\nShown \n\n"},
		{"a \"<!--\" inside a line after the blank line that ends a block a tag opens",
			"<div>\n\nShown <!-- shown too\n\nShown.",
			"<div>\n\nShown <!-- shown too\n\nShown."},
		{"a comment's opening inside a fenced block",
			"```html\n<!--\n```\nShown.",
			"```html\n<!--\n```\nShown."},
		{"a fence inside a block that a tag opens, which opens no fenced block",
			"<div>\n```\n<!-- hidden\nhidden too.",
			"<div>\n```\n\n"},
	} {
		if shown := visibleMarkdown(text.written); shown != text.shown {
			t.Errorf("%s: the page shows %q, want %q", text.shape, shown, text.shown)
		}
	}
}
