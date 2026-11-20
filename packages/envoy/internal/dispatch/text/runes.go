package text

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// HeadRunes returns value cut to its first limit runes; a value at or under the
// limit is returned unchanged. It reads value only up to the cut, so a long value costs no more
// than its first limit runes. A value that is no valid UTF-8 counts each byte that starts no rune
// as one rune, as Go's conversion to runes reads it, and the cut writes U+FFFD for each such byte.
func HeadRunes(value string, limit int) string {
	count := 0
	for at := range value {
		if count == limit {
			head := value[:at]
			if utf8.ValidString(head) {
				return head
			}
			return string([]rune(head))
		}
		count++
	}
	return value
}

// HeadWords returns value cut at a word boundary to at most its first limit runes, and whether
// it left a character of value out. Its boundaries are those of Unicode's default word rules
// (UAX #29, words.go), which part a value into words and the white space between them: a
// boundary falls at white space, beside each character of a script written without spaces
// between its words, such as Chinese, Japanese or Thai, and beside each emoji, symbol and
// punctuation mark, but never inside a run of letters and digits, so a word of a script written
// with spaces is one word, at any length. A run with no white space that holds a URL is one word
// too (isURL), so a cut never falls inside a link: the cut keeps the URL whole or falls before
// it. The cut falls at the last end of a word at or before the limit, so a word that ends at the
// limit itself is kept, and white space is never kept at the end of a cut.
//
// A word is never cut in two. The first word is the first word that holds a letter or a number,
// together with all that comes before it: white space, punctuation of any kind, a symbol or an
// emoji, however far that runs. Where it runs past the limit, the answer is that whole first word
// with all that leads into it, with no bound but the value's own length, so a value that starts
// with `/`, `./`, `...`, `-`, `(`, a backtick or a rule of 100 hyphens before a first word keeps
// that word whole and never ends with the lead-in alone, even where the lead-in itself runs past
// the limit. A value with no word of letters or numbers is cut after the last word that ends
// inside the limit; where none does, its first word, of any kind, such as a chain of pictographs
// joined by zero-width joiners, is kept whole with all that leads into it; and a value that holds
// no word at all, such as white space or marks alone, has no end of a word to cut at, and is
// returned whole. A later word of letters or numbers that runs past the limit is left out with the
// opening punctuation (openingPunctuation) before it from the start of its run with no white
// space, so the answer does not end with a backtick or a bracket that opens a word it leaves out.
// After a word of letters or numbers, other punctuation, such as a slash, is a word boundary
// rather than a lead-in, and any word that runs past the limit is left out.
//
// The answer is a prefix of value. It is a cut where it leaves out a character that is no white
// space, a mark alone among them (leftOut), and no cut where it leaves out white space alone; a
// value at or under the limit is returned unchanged and uncut, and so is a value with no word. A
// caller that marks a cut (an ellipsis) reads the second answer rather than comparing lengths,
// which white space the cut removed would make wrong. Once a word of letters or numbers has ended
// inside the limit, the walk stops at the first word that ends past it; before that, it stops at
// the first word of letters or numbers. So a long value is read only to the end of the run with no
// white space that holds that word, but a value whose lead-in runs past the limit is read on to its
// first word of letters or numbers, or to its end where it holds none. The line's caller
// (keepJudged in the api package) hands it no more than the words it judged, so this read is never
// longer than the judging before it.
func HeadWords(value string, limit int) (string, bool) {
	// end is the byte offset after the last word that ends inside the limit, 0 while none has;
	// where none has, a word before any letter word that runs past the limit sets it to that word's
	// end, the first word, kept whole where no letter word follows. lettered is whether a word that
	// holds a letter or a number ended inside the limit; count is the number of runes read. lead is
	// end as it stood where the current run with no white space began, while that run holds
	// whitespace and opening punctuation before a letter word, and -1 after any other word: a later
	// letter word that runs past the limit is left out with the opening punctuation before it. The
	// first letter word does not read it, since all that comes before that word leads into it.
	segments := wordSegments{s: value}
	end, count, lettered, lead, start := 0, 0, false, 0, 0
	for {
		next, runes, class, starts := segments.headSegment()
		if runes == 0 {
			break
		}
		segment := value[start:next]
		start = next
		if starts {
			lead = end
		}
		count += runes
		switch {
		case class < classSymbol:
			// White space, or marks with no character before them: no word.
		case count <= limit:
			end = next
			switch {
			case class == classLetter:
				lettered, lead = true, -1
			case class == classSymbol && !opensWord(segment):
				lead = -1
			}
		case !lettered && class != classLetter:
			// Before the first word of letters or numbers, a word past the limit leads into it as
			// any other does, so the walk goes on to that word.
			if end == 0 {
				end = next
			}
		case !lettered:
			// The first letter word runs past the limit: it is kept with all that leads into it.
			return value[:next], leftOut(value[next:])
		case class == classLetter && lead >= 0:
			// A later word runs past the limit: opening punctuation is left out with it.
			return value[:lead], true
		default:
			return value[:end], true
		}
	}
	if count <= limit || end == 0 {
		// The value is at or under the limit, or it holds no word.
		return value, false
	}
	// No letter word runs past the limit: the cut falls after the last word that ends inside it,
	// or after the first word where none does, and a mark with no character before it may follow.
	return value[:end], leftOut(value[end:])
}

// openingPunctuation is the punctuation that opens a word a reader normally reads with it: a
// code mark, bracket, parenthesis or quote. A slash, hyphen, full stop and the other punctuation
// that separates words is not here, so after a word of letters or numbers it remains a boundary
// inside the budget. Before the first such word, every mark leads into it (HeadWords).
const openingPunctuation = "`[({\"'“‘"

// opensWord reports whether every rune of segment is opening punctuation. segment is one UAX #29
// segment that ends inside the budget (HeadWords).
func opensWord(segment string) bool {
	return segment != "" && strings.IndexFunc(segment, func(r rune) bool {
		return !strings.ContainsRune(openingPunctuation, r)
	}) < 0
}

// headSegment reads the next segment as HeadWords cuts a value, and reports whether it starts a
// run with no white space: it is at the start of the value or after white space. Where it does,
// and that run holds a URL (isURL), the segment is the whole run, one word of the highest class of
// a rune it holds, and the word rules read each of its runes as segment does. Any other segment is
// segment's.
func (w *wordSegments) headSegment() (end, runes int, class runeClass, starts bool) {
	before, _ := utf8.DecodeLastRuneInString(w.s[:w.next])
	starts = w.next == 0 || unicode.IsSpace(before)
	if starts {
		run := w.s[w.next:]
		if space := strings.IndexFunc(run, unicode.IsSpace); space >= 0 {
			run = run[:space]
		}
		if isURL(run) {
			for _, r := range run {
				value, _ := wordBreakOf(r)
				w.read(value)
				runes++
				class = max(class, classOf(r, value))
			}
			w.next += len(run)
			return w.next, runes, class, starts
		}
	}
	end, runes, class = w.segment()
	return end, runes, class, starts
}

// isURL reports whether run, a run with no white space, holds a URL: a "://", as a check's words
// drop one (blockWords in the api package), or a "www." in any case at the run's start or after
// one of ( * _ [ ] ~. GitHub's markdown, cmark-gfm, links an address with no scheme only at a
// run's start or after one of * _ ~ (, and only in lower case (extensions/autolink.c of
// github/cmark-gfm at commit 46829933c3, www_match, lines 178-183), and makes the link run to the
// white space after it. isURL is wider on purpose, so no text GitHub links is ever cut: a run it
// reads as a URL that GitHub does not link is only kept whole or left out, and a blank stands
// before an ellipsis after it (EndsInURL).
func isURL(run string) bool {
	if strings.Contains(run, "://") {
		return true
	}
	for at := 0; at+len("www.") <= len(run); at++ {
		if (at == 0 || strings.IndexByte("(*_[]~", run[at-1]) >= 0) && strings.EqualFold(run[at:at+len("www.")], "www.") {
			return true
		}
	}
	return false
}

// EndsInURL reports whether the last run with no white space of value holds a URL (isURL). Where
// it does, a character written right after value, with no white space between, joins that link
// where GitHub renders it: GitHub's markdown makes a link of a URL up to the white space after it,
// and an ellipsis is none of the trailing punctuation it leaves out of the link.
func EndsInURL(value string) bool {
	if at := strings.LastIndexFunc(value, unicode.IsSpace); at >= 0 {
		_, size := utf8.DecodeRuneInString(value[at:])
		value = value[at+size:]
	}
	return isURL(value)
}

// leftOut reports whether rest, what a cut leaves out of a value, holds a character of it: a rune
// above white space (classOf), a mark alone among them.
func leftOut(rest string) bool {
	return strings.IndexFunc(rest, func(r rune) bool {
		value, _ := wordBreakOf(r)
		return classOf(r, value) > classSpace
	}) >= 0
}

// HoldsLetter reports whether value holds a letter or a number by the one rule of what a word is
// (classOf), so whether a word of value can be the first word HeadWords keeps whole with all that
// leads into it.
func HoldsLetter(value string) bool {
	return IndexLetter(value) >= 0
}

// IndexLetter is the byte offset in value of its first letter or number by the one rule of what a
// word is (classOf), or -1 where it holds none. classOf reads a rune as a letter or a number only
// where unicode.IsLetter or unicode.IsNumber does, so IndexLetter looks up the word-break value of
// those runes alone, and reads any other rune at the cost of those two tests: a caller that reads a
// long text only for its first letter pays little for each mark it passes.
func IndexLetter(value string) int {
	return strings.IndexFunc(value, func(r rune) bool {
		if !unicode.IsLetter(r) && !unicode.IsNumber(r) {
			return false
		}
		wb, _ := wordBreakOf(r)
		return classOf(r, wb) == classLetter
	})
}

// HoldsWord reports whether value holds a word of any kind by the one rule of what a word is
// (classOf): a rune that is neither white space nor a mark, a joiner or a format character, such as
// a letter, a digit, punctuation, a symbol or an emoji. A value of halfwidth sound marks alone
// (U+FF9E, U+FF9F), which are letters to unicode.IsLetter and marks to UAX #29, holds none.
func HoldsWord(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		wb, _ := wordBreakOf(r)
		return classOf(r, wb) > classJoiner
	}) >= 0
}
