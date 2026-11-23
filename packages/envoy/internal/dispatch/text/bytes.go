package text

import "unicode/utf8"

// HeadBytes returns value cut to at most limit bytes at a whole character, and whether it cut. A
// value at or under the limit is returned unchanged and uncut; a longer one ends before the
// character the limit lands in, so a multibyte character across the cut is left out whole and
// the answer is never more than limit bytes. HeadRunes bounds a text in characters; this bounds
// it in bytes, for a store or a reader that counts bytes. A caller that marks a cut adds its own
// marker, in its own place, and reads the second answer to know whether to add it.
func HeadBytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}
