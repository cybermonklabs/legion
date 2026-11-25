package chips

import (
	"slices"
	"strings"
	"unicode"
)

// KeepLinkWords is answer, an answer's markdown, written so that no words the model gave a link to
// an item are hidden behind the item's chip, which the bar draws in place of them (Words). The
// prompt tells the model to write every fact outside the link, and the eval measured that it does
// not always: "[Implementer of OPS-4](dispatch://OPS-4) is unresponsive" reached the person as the
// chip of OPS-4 and "is unresponsive", the session's name lost, and the words of a link to a
// comment as the comment's own head, cut at 60 characters (c13 at 9ae5605b, status-02, status-05,
// status-06 and status-13).
//
// So a link whose words say more than its chip draws (says) is written as those words, as text,
// then a space and, in parentheses, the link with the item's short form as its words (Short, an
// issue's key) and its address and title as written: "Implementer of OPS-4
// ([OPS-4](dispatch://OPS-4)) is unresponsive", which the bar draws as the words and then the
// chip. The words stand before the chip since they are the sentence's own, which the chip, in
// parentheses, names the item of. The link's words are the short form because the bar draws them
// where it draws no chip - a link to an issue that already has its chip in the answer, and an item
// the person may not read - and the short form names the item and nothing more; and a link whose
// words are its short form keeps them, so KeepLinkWords of what it wrote is what it wrote.
//
// Every other link stays as written: one whose words the chip draws, an autolink, whose words are
// its address, and a link the bar draws as no item - a time (dispatch-time:), a web address, a
// reference the bar does not read - as do pictures, code and all other text (RewriteLinks).
func KeepLinkWords(answer string, items Items) string {
	return RewriteLinks(answer, func(out *strings.Builder, written, words, href string) {
		named, ok := ParseTarget(href)
		if !ok || !strings.HasPrefix(written, "[") || !named.says(words, href, items) {
			out.WriteString(written)
			return
		}
		// The words as written, but the spaces they end in, which the parenthesis stands after.
		out.WriteString(strings.TrimRightFunc(words, unicode.IsSpace))
		out.WriteString(" ([")
		out.WriteString(named.Short())
		// What follows the words as written: their closing bracket, the address and the title.
		out.WriteString(written[1+len(words):])
		out.WriteString(")")
	})
}

// says reports whether words, the words a link to the target was given, say more than the bar
// draws for it, case and punctuation aside (terms). Words that are a run of the words of one of
// these say no more: what the bar draws (drawn) - an issue's key, its status and its title, the
// head of an ask's, a comment's or a message's text, a document's name; the key of an issue the
// bar draws with its title, either way round, "OPS-4: Rotate the keys"; and the link's own
// reference, as href writes it, bare (Reference) or short (Short). No words say nothing.
func (t Target) says(words, href string, items Items) bool {
	said := terms(words)
	if len(said) == 0 {
		return false
	}
	drawn, key, title := t.drawn(items)
	for _, shown := range []string{drawn, key + " " + title, title + " " + key, href, t.Reference(), t.Short()} {
		if holdsRun(terms(shown), said) {
			return false
		}
	}
	return true
}

// terms is the words of written, case and punctuation aside: each run of its letters and digits,
// in lower case.
func terms(written string) []string {
	return strings.FieldsFunc(strings.ToLower(written), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// holdsRun reports whether run stands in words, its words one after another.
func holdsRun(words, run []string) bool {
	for at := 0; at+len(run) <= len(words); at++ {
		if slices.Equal(words[at:at+len(run)], run) {
			return true
		}
	}
	return false
}
