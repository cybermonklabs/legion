package text

import (
	"strings"
	"unicode/utf8"
)

// HeadRunes returns value cut to its first limit runes; a value at or under the
// limit is returned unchanged. The cut is a slice of value, so it copies nothing,
// unless the part it keeps holds a byte that is not UTF-8: that part is converted
// through []rune, which writes each such byte as U+FFFD.
func HeadRunes(value string, limit int) string {
	invalid := false
	for offset, r := range value {
		if limit <= 0 {
			if invalid {
				return string([]rune(value[:offset]))
			}
			return value[:offset]
		}
		if r == utf8.RuneError && !strings.HasPrefix(value[offset:], "\uFFFD") {
			invalid = true
		}
		limit--
	}
	return value
}

// HeadWords returns value cut to its first limit runes at a word boundary, and whether it cut:
// the cut falls back to the last space inside the limit, so the answer ends at a whole word,
// and a value whose first limit runes hold no space is cut at the limit itself, since there is
// no boundary to fall back to. A value at or under the limit is returned unchanged and uncut.
// A caller that marks a cut (an ellipsis) reads the second answer rather than comparing
// lengths, which a trailing space the cut removed would make wrong.
func HeadWords(value string, limit int) (string, bool) {
	runes := []rune(value)
	if len(runes) <= limit {
		return value, false
	}
	head := string(runes[:limit])
	if space := strings.LastIndexAny(head, " \t\n"); space > 0 {
		head = head[:space]
	}
	return strings.TrimRight(head, " \t\n"), true
}
