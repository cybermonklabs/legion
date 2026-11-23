package redact

import "html"

// UnescapeHTML is text with its HTML character references decoded as a reader of the page sees
// them, in at most ReferencePasses passes that change it, and true: &quest; is '?', &commat; is
// '@', and &amp;quest; is '?' too, since a page can write a reference's '&' as a reference. Where
// the text still changes on the pass after them, its references nested deeper than a page writes
// them, it answers "" and false, and the caller prints the text as one the rule cannot read. The
// boundary reads an answer's body through it before the rule (OAuth2), and excerpt.Text a page's
// text, so neither can hide a URL's separators from the rule.
func UnescapeHTML(text string) (string, bool) {
	for pass := 0; ; pass++ {
		decoded := html.UnescapeString(text)
		if decoded == text {
			return text, true
		}
		if pass == ReferencePasses {
			return "", false
		}
		text = decoded
	}
}
