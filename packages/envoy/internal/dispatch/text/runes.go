package text

import "strings"

// HeadRunes returns value cut to its first limit runes; a value at or under the
// limit is returned unchanged. It walks only as far as the limit, rather than
// converting the whole value, and a cut value reads as a []rune conversion of it
// would: each byte that is no UTF-8 is one rune, written back as U+FFFD.
func HeadRunes(value string, limit int) string {
	count := 0
	for index := range value {
		if count == limit {
			return string([]rune(value[:index]))
		}
		count++
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
