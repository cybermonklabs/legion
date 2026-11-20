package excerpt_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/excerpt"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestTextPrintsNoCredentialAPageEchoes: a gateway's page that holds the token its request
// carried, bare, and words quoting a URL that carries a user, a password and a token. Text's
// excerpt of each holds none of them: the first by its redaction of the tokens it is handed
// (redact.Tokens), the second by the rule for remote text (redact.Remote). Each is a designated
// assertion of the check's mutation gate: excerpt.text-tokens and excerpt.text-remote.
func TestTextPrintsNoCredentialAPageEchoes(t *testing.T) {
	redacttest.RequireNone(t, "excerpt.text-tokens", excerpt.Text("<pre>\n"+redacttest.Token+"\n</pre>", redacttest.Token))
	redacttest.RequireNone(t, "excerpt.text-remote", excerpt.Text("<p>"+redacttest.Words+"</p>"))
}

// cutAt is a body whose first excerpt.ScanLimit characters end right after head, which tail
// follows: spaces before head, or, styled, a gateway page's inline style block.
func cutAt(head, tail string, styled bool) string {
	if !styled {
		return strings.Repeat(" ", excerpt.ScanLimit-len(head)) + head + tail
	}
	open, close := "<html><head><style>", "</style></head><body><p>see "
	return open + strings.Repeat("a", excerpt.ScanLimit-len(open)-len(close)-len(head)) + close + head + tail + "</p></body></html>"
}

// TestTextPrintsNoCredentialAReferenceOrTheScanLimitHides: a page can write a URL's separators as
// HTML character references, which Text decodes once the tags are gone, before the rule reads
// the text; the scan limit can fall inside a URL, so Text drops the last word of a body it cut;
// and a tag inside a URL's start leaves a gap the rule's second reading reads across. Each row's
// excerpt holds none of the credentials (the review of #4368 at db7e4a0226: finding 4 and the red
// team's excerpt probes).
func TestTextPrintsNoCredentialAReferenceOrTheScanLimitHides(t *testing.T) {
	for _, row := range []struct{ name, body string }{
		{"reference-question-equals", "<html><p>https://idp.example/cb&quest;session&equals;" + redacttest.Token + "</p></html>"},
		{"reference-at", "<html><p>https://" + redacttest.Token + "&commat;idp.example/</p></html>"},
		{"reference-numeric-at", "<html><p>https://" + redacttest.User + ":" + redacttest.Password + "&#64;idp.example/</p></html>"},
		{"cut-before-at", cutAt("https://"+redacttest.Token, "@idp.example/x", false)},
		{"cut-before-at-styled", cutAt("https://"+redacttest.Token, "@idp.example/x", true)},
		{"cut-after-user-colon", cutAt("https://"+redacttest.User+":", redacttest.Password+"@idp.example/x", true)},
		{"tag-in-start", "<p>Could not reach https<wbr>://idp.example/cb?code=" + redacttest.Token + "</p>"},
	} {
		t.Run(row.name, func(t *testing.T) {
			redacttest.RequireNone(t, "excerpt.hidden/"+row.name, excerpt.Text(row.body, "unrelated-bearer-000000"))
		})
	}
}

// shiftedBody is a page whose style block runs until url ends exactly at character ScanLimit,
// followed by after, with words, each a short credential the request carried, before url.
func shiftedBody(t *testing.T, words []string, url, after string) string {
	t.Helper()
	lead, rest := "<style>", "</style> "+strings.Join(words, " ")+" "
	fill := excerpt.ScanLimit - len(lead) - len(rest) - len(url)
	if fill <= 0 {
		t.Fatal("no room for the fill")
	}
	return lead + strings.Repeat("a", fill) + rest + url + after
}

// TestTextCutsBeforeItRedactsAndDecodesEveryReference: the cut at the scan limit is made on the
// text as it came, so a short credential the request carried, which redact.Tokens writes as the
// longer [redacted], cannot move it inside a URL to just before its '@'; and a page's character
// references are decoded until none is left (redact.UnescapeHTML), so a double-encoded '@' or
// '?' does not hide a URL's credential from the rule; and a split query after a URL, behind a
// no-break space or an em dash a page writes as a reference, is not readable (the review of #4368
// at fd07192703: the red team's excerpt probes).
func TestTextCutsBeforeItRedactsAndDecodesEveryReference(t *testing.T) {
	url := "https://" + redacttest.Token + "@gl.example/xy"
	for _, row := range []struct {
		name, body string
		carried    []string
	}{
		{"short-token-shifts-the-cut", shiftedBody(t, []string{"ada", "ada"}, url, " after the limit"), []string{"ada"}},
		{"double-encoded-at", "<p>clone https://" + redacttest.Token + "&amp;commat;github.example/o/r failed</p>", nil},
		{"double-encoded-query", "<p>Continue at https://idp.example/cb&amp;quest;session&amp;equals;" + redacttest.Token + " now</p>", nil},
		{"nbsp-before-split-query", "<table><tr><td>https://idp.example/cb</td><td>then</td><td>x&nbsp;</td><td>?session=" + redacttest.Token + "</td></tr></table>", nil},
		{"mdash-before-split-query", "<p>https://idp.example/cb &mdash; then ?session=" + redacttest.Token + "</p>", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			redacttest.RequireNone(t, "excerpt.shift/"+row.name, excerpt.Text(row.body, append(row.carried, "unrelated-bearer-000000")...))
		})
	}
}
