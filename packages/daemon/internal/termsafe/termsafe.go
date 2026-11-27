// Package termsafe writes text a process or a push author chose so that a log or a terminal
// shows it as it is and acts on none of it.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ellipsis marks where Escaped cut its text.
const ellipsis = "…"

// Escaped is s with each control character, and each other character that does not print, as a
// Go escape (`\n`, `\x1b`, `\u202e`, `\U000e0041`), and each byte that is not UTF-8 as `\xNN`, so
// the text can neither split a line, nor move the cursor, nor colour or reorder what follows it. A
// backslash is written as it is: text another printer already escaped passes unchanged. With max
// above 0 it is at most max bytes long: text that would run past max is cut after the last
// character, or the last escape, that leaves room for an ellipsis, which marks the cut and counts
// in max, so no escape is cut in two. With max 0 or less it is not cut.
func Escaped(s string, max int) string {
	var b strings.Builder
	fits := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < utf8.RuneSelf && !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case !unicode.IsPrint(r) && r <= 0xFFFF:
			fmt.Fprintf(&b, `\u%04x`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\U%08x`, r)
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
		if max <= 0 {
			continue
		}
		if b.Len() > max {
			return b.String()[:fits] + ellipsis
		}
		if b.Len() <= max-len(ellipsis) {
			fits = b.Len()
		}
	}
	return b.String()
}
