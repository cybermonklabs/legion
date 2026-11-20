package redact

import (
	"io"
	"strings"
	"unicode/utf8"
)

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
// whether it was cut: what the rule leaves of it when that is n bytes or fewer, and otherwise its
// first n bytes, the end moved back to where a rune starts, looking at most utf8.UTFMax-1 bytes
// back and staying put where none starts there, so a valid character is never split and a run of
// bytes that start no character cannot take the whole result. The rule reads first for the reason
// Runes gives. Bytes writes no ellipsis: a caller that wants one appends it where cut is true.
func Bytes(text string, n int) (kept string, cut bool) {
	text = URLCredentials(text)
	if len(text) <= n {
		return text, false
	}
	end := n
	for i := n; i >= max(0, n-utf8.UTFMax+1); i-- {
		if utf8.RuneStart(text[i]) {
			end = i
			break
		}
	}
	return text[:end], true
}

// Tail is the end of text, at most n bytes of it, n at least 0, once URLCredentials has read it
// whole, and how many bytes of what the rule leaves come before it, 0 when none do. The rule reads
// first because a cut that keeps a text's end can start inside a URL's userinfo or its query,
// after the "://" or the "?", where the rule then finds neither and keeps what the cut left of
// them. Where it cuts, the start moves forward to where a rune starts, looking at most
// utf8.UTFMax-1 bytes ahead and staying put where none starts there, so a valid character is
// never split and a run of bytes that start no character cannot take the whole tail.
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
// the rule then cannot find. So where cut is true, Read looks at the run after the last byte that
// ends a URL (EndsURLByte), the whole text where none does: the run the bound stopped inside. It
// drops that run where it holds a "://", since it may then be a URL whose "@" was never read, and
// keeps it otherwise: a URL holds no byte that ends a URL, so one that reaches into the run starts
// in it, its "://" with it, and a run with no "://" holds no part of a URL's userinfo or query.
// Every URL before the run ends at or before the byte before it, as it does in the whole text. The
// rule then reads what is left, which Read answers; with cut false, that is the whole text.
//
// A read tells whether its bound cut it by reading one byte past the bound, as ReadBounded does.
// A read that ended on an error stopped where the error came, which is as unknown a place as the
// bound's, so it is cut too.
func Read(text string, cut bool) string {
	if cut {
		end := len(text)
		for end > 0 && !EndsURLByte(text[end-1]) {
			end--
		}
		if strings.Contains(text[end:], "://") {
			text = text[:end]
		}
	}
	return URLCredentials(text)
}

// ReadBounded reads r to its end or to limit bytes, limit at least 0, and one byte more, so a read
// its bound cut is told from one read whole: it answers the first limit bytes, whether they were
// cut, and the read's error. A read that ended on an error is cut as well, where the error came:
// that can be inside a URL before its "@", as the bound's place can, so a caller that quotes what
// was kept and drops the error still gives it to Read as cut.
func ReadBounded(r io.Reader, limit int) (kept []byte, cut bool, err error) {
	kept, err = io.ReadAll(io.LimitReader(r, int64(limit)+1))
	cut = len(kept) > limit || err != nil
	return kept[:min(len(kept), limit)], cut, err
}
