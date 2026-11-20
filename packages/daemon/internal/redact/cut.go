package redact

import "unicode/utf8"

// Runes is text cut to at most n runes, n at least 1, once URLCredentials has read it whole: what
// the rule leaves of it when that is n runes or fewer, and otherwise its first n-1 runes and "…".
// The rule reads first because a cut made first can end inside a URL's userinfo, before its "@",
// where the rule then finds no userinfo and keeps the start of a user name or a password as the
// URL's host. A rune is what utf8.DecodeRuneInString reads, so a byte that starts no valid
// character counts as one rune and is kept as it is.
func Runes(text string, n int) string {
	text = URLCredentials(text)
	if len(text) <= n {
		return text
	}
	runes, end := 0, 0
	for at := range text {
		switch runes {
		case n - 1:
			end = at
		case n:
			return text[:end] + "…"
		}
		runes++
	}
	return text
}

// Bytes is text cut to at most n bytes, n at least 0, once URLCredentials has read it whole, and
// whether it was cut: what the rule leaves of it when that is n bytes or fewer, and otherwise the
// longest start of that of at most n bytes that ends where a rune starts, so a cut never leaves
// part of a valid character behind. The rule reads first for the reason Runes gives. Bytes writes
// no ellipsis: a caller that wants one appends it where cut is true.
func Bytes(text string, n int) (kept string, cut bool) {
	text = URLCredentials(text)
	if len(text) <= n {
		return text, false
	}
	end := n
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}

// Tail is the end of text, at most n bytes of it, n at least 0, once URLCredentials has read it
// whole, and how many bytes of what the rule leaves come before it, 0 when none do. The rule reads
// first because a cut that keeps a text's end can start inside a URL's userinfo or its query,
// after the "://" or the "?", where the rule then finds neither and keeps what the cut left of
// them. Where it cuts, the start moves forward to where a rune starts, looking at most
// utf8.UTFMax bytes ahead and staying put where none starts there, so a valid character is never
// split and a run of bytes that start no character cannot take the whole tail.
func Tail(text string, n int) (tail string, cut int) {
	text = URLCredentials(text)
	if len(text) <= n {
		return text, 0
	}
	cut = len(text) - n
	for i := cut; i < min(cut+utf8.UTFMax, len(text)); i++ {
		if utf8.RuneStart(text[i]) {
			cut = i
			break
		}
	}
	return text[cut:], cut
}

// Read is the text a bounded read kept, through URLCredentials, where cut says whether the read's
// bound cut it: whether there was more to read than the read took. Such a cut comes before any
// rule can read the text, and can stop inside a URL before the "@" that ends its userinfo, which
// the rule then cannot find, so where cut is true everything after the last byte that ends a URL
// (EndsURLByte) goes first: the run the bound stopped inside, since every URL before that byte
// ends at or before it. Where no byte of the text ends a URL, nothing is kept. The rule then reads
// what is left, which Read answers; with cut false, that is the whole text.
//
// A read tells whether its bound cut it by reading one byte past the bound: a caller reads limit+1
// bytes and gives Read the first limit of them, with cut true where it read more than limit. A
// read that ended on an error stopped where the error came, which is as unknown a place as the
// bound's, so its caller gives cut true too.
func Read(text string, cut bool) string {
	if cut {
		end := len(text)
		for end > 0 && !EndsURLByte(text[end-1]) {
			end--
		}
		text = text[:end]
	}
	return URLCredentials(text)
}
