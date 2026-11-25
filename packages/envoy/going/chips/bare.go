package chips

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// LinkBareKeys is answer, an answer's markdown, with each issue key it writes bare that returned
// reports true for written as the link to the issue, [KEY](dispatch://KEY), which the bar draws
// as the issue's chip. The prompt tells the model to link each item it names, and the preview at
// e2b341f7 measured that it does not always: answer-01 wrote "DEMO-21 is a P3 backlog feature
// with no assignee" with DEMO-21 as text, so the bar drew no chip, and neither the person nor the
// eval's judge read the issue's title. returned says whether a read of the run returned the key,
// so an answer still links only items its run read: a key no read returned stays text.
//
// A bare key stands in the answer's plain text - outside a link's words and its href, an
// autolink, a picture, code and an escaped character (rewriteMarkdown) - as a whole token
// (wholeKey). So DEMO-3 in "DEMO-3/spec", "https://x/DEMO-3", "$DEMO-3" or "DEMO-30" stays as
// written, and DEMO-3 before a full stop, a comma, a parenthesis or a space is bare. Every link
// stays as written, and a link with a key as its words is no bare key, so LinkBareKeys of what it
// wrote is what it wrote.
func LinkBareKeys(answer string, returned func(key string) bool) string {
	return rewriteMarkdown(answer, writeLink, func(out *strings.Builder, markdown string, start, end int) {
		written := start
		for _, found := range issueKey.FindAllStringIndex(markdown[start:end], -1) {
			keyStart, keyEnd := start+found[0], start+found[1]
			key := markdown[keyStart:keyEnd]
			if !wholeKey(markdown, keyStart, keyEnd) || !returned(key) {
				continue
			}
			out.WriteString(markdown[written:keyStart])
			out.WriteString("[" + key + "](dispatch://" + key + ")")
			written = keyEnd
		}
		out.WriteString(markdown[written:end])
	})
}

// HoldsKey reports whether key, an issue's key, stands in text as a whole token, as LinkBareKeys
// reads a bare key (wholeKey): DEMO-3 stands in `"key": "DEMO-3"` and in "DEMO-3, DEMO-4", not
// in "DEMO-30" or "DEMO-3/spec". The server reads with it whether a tool result of a run returned
// a key the run's answer writes bare.
func HoldsKey(text, key string) bool {
	for at := 0; at < len(text); {
		found := strings.Index(text[at:], key)
		if found < 0 {
			return false
		}
		start := at + found
		if wholeKey(text, start, start+len(key)) {
			return true
		}
		at = start + 1
	}
	return false
}

// writeLink writes a link as it was written.
func writeLink(out *strings.Builder, written, _, _ string) {
	out.WriteString(written)
}

// issueKey is an issue's key as Dispatch writes one (going/eval's timeLinkKey), wherever it
// stands.
var issueKey = regexp.MustCompile(`[A-Z][A-Z0-9]{1,9}-[1-9][0-9]*`)

// wholeKey reports whether the key that stands from start to end of text is a whole token: no
// letter, digit or mark that joins it to a path, an address, a name or a variable - / : - _ . #
// @ $ - stands before it, and no letter, digit, - _ or / after it. So a key is not read inside a
// longer key, DEMO-30 or XDEMO-3, a reference, DEMO-3/spec or dispatch://DEMO-3, or an address,
// https://x/DEMO-3, which a link written in its place would break.
func wholeKey(text string, start, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(text[:start])
	after, _ := utf8.DecodeRuneInString(text[end:])
	return !joinsKey(before, "/:-_.#@$") && !joinsKey(after, "-_/")
}

// joinsKey reports whether r, standing beside a key, is part of the token the key stands in: a
// letter, a digit or one of marks. No rune, at the text's start or end, joins it.
func joinsKey(r rune, marks string) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(marks, r)
}
