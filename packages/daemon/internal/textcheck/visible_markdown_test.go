package textcheck

import (
	"strings"
	"testing"
)

// visibleMarkdown is text as the rendered page shows it: each HTML comment outside a fenced block
// is blanked, its newlines kept, so that every line keeps its number. A comment never closed runs
// to the end of the text, as one that opens a line does on the page. A fenced block, from a line
// opening "```" to the next, shows its lines as written, a "<!--" among them, and a fence inside a
// comment opens nothing. A check reads the guide through this before it selects a line, so that a
// comment opened before that line still hides it.
func visibleMarkdown(text string) string {
	lines := strings.Split(text, "\n")
	fenced, commented := false, false
	for i, line := range lines {
		if !commented && strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		var shown strings.Builder
		for line != "" {
			if commented {
				var closed bool
				_, line, closed = strings.Cut(line, "-->")
				commented = !closed
				continue
			}
			var before string
			before, line, commented = strings.Cut(line, "<!--")
			shown.WriteString(before)
		}
		lines[i] = shown.String()
	}
	return strings.Join(lines, "\n")
}

// visibleMarkdown hides what the rendered page hides and keeps every line's number: a comment
// around a fenced block hides the block, its fences among it; a comment around a list hides the
// list; a comment inside a line hides that part of it; a comment never closed hides the rest of
// the text. A "<!--" inside a fenced block opens no comment, so the block and the lines after it
// stay as written.
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
		{"a comment never closed",
			"Shown. <!-- hidden\nhidden too.",
			"Shown. \n"},
		{"a comment's opening inside a fenced block",
			"```html\n<!--\n```\nShown.",
			"```html\n<!--\n```\nShown."},
	} {
		if shown := visibleMarkdown(text.written); shown != text.shown {
			t.Errorf("%s: the page shows %q, want %q", text.shape, shown, text.shown)
		}
	}
}
