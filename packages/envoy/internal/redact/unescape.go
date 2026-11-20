package redact

import "html"

// UnescapeHTML is text with its HTML character references decoded until none is left, as a
// reader of the page sees it: &quest; is '?', &commat; is '@', and &amp;quest; is '?' too, since
// a page can write a reference's '&' as a reference. Each decoding that changes the text
// shortens it, so the loop ends. The boundary reads an answer's body through it before the rule
// (OAuth2), and excerpt.Text a page's text, so neither can hide a URL's separators from the rule.
func UnescapeHTML(text string) string {
	for {
		decoded := html.UnescapeString(text)
		if decoded == text {
			return text
		}
		text = decoded
	}
}
