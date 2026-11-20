package excerpt_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/excerpt"
	"github.com/sjawhar/envoy/internal/redact"
)

// The rows mirror excerpt.ts's own (packages/envoy-client/src/__tests__/dispatch-http.test.ts,
// which reads them through the Dispatch client's message), so the two languages name one body the
// same way.

// pieceBearer is shaped as Dispatch's tokens are, `dsp_` and 43 base64url characters, and ends in
// its own first two characters, so two copies of it can overlap.
const pieceBearer = "dsp_Q7vK2mXr9LtW4nBz8YcH1jPe5sAf3gUo6iNq0wEbTds"

// bearerPieces is every run of 8 of pieceBearer's characters text holds; any longer piece holds
// one.
func bearerPieces(text string) []string {
	var held []string
	for at := range len(pieceBearer) - 7 {
		if piece := pieceBearer[at : at+8]; strings.Contains(text, piece) {
			held = append(held, piece)
		}
	}
	return held
}

// A body that is not the server's own is named in one line of plain text: scripts, styles and tags
// dropped, whitespace collapsed, cut to 120 characters with an ellipsis, and quoted as JSON.
func TestBodyForTextNamesAPageInOnePlainLine(t *testing.T) {
	nginx := "<html>\r\n<head><title>502 Bad Gateway</title></head>\r\n<body>\r\n<center><h1>502 Bad Gateway</h1>" +
		"</center>\r\n<hr><center>nginx/1.25.3</center>\r\n</body>\r\n</html>\r\n"
	cloudflare := "<html><head><style>body{margin:0}</style><script>var x=1;</script></head><body>" +
		"<h1>Error 522</h1><p>Connection timed out</p></body></html>"
	const not = "a body that is not Dispatch's error JSON "
	for _, row := range []struct{ name, body, want string }{
		{"a gateway's page", nginx, not + `("502 Bad Gateway 502 Bad Gateway nginx/1.25.3")`},
		{"a page with a style and a script", cloudflare, not + `("Error 522 Connection timed out")`},
		{"an empty body", "", "an empty body"},
		{"a line longer than 120 characters", strings.Repeat("x", 500), not + `("` + strings.Repeat("x", 120) + `…")`},
		{"JSON of another shape", `{"message":"upstream timed out"}`, not + `("{\"message\":\"upstream timed out\"}")`},
		{"a 404 page", "<html><body><h1>404 Not Found</h1></body></html>", not + `("404 Not Found")`},
		{"an unclosed script", "<p>before</p><script>var leaked = 1; never closed", not + `("before")`},
		{"a closing tag of another name", "<style>a</script>b</style>c", not + `("c")`},
		{"a quote, a backslash and a control character", "say \"hi\" \\ \x01 there", not + `("say \"hi\" \\ \u0001 there")`},
		{"bytes that are not UTF-8", "a\xffb", not + "(\"a\uFFFDb\")"},
		{"Unicode whitespace", "a\u00a0\u2028\u3000b\ufeff", not + `("a b")`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := excerpt.BodyForText(row.body, "Dispatch's error JSON", "secret"); got != row.want {
				t.Fatalf("BodyForText = %q, want %q", got, row.want)
			}
		})
	}
}

// A misconfigured gateway echoes the request back, headers included. Two kinds of credential are
// redacted before anything is quoted: every piece of each token the request carried (trimmed, as
// the header sends it), and the value after `Authorization:` (its scheme kept), `Bearer` or
// `DPoP`, whoever's it is.
func TestTextRedactsTheTokenAndEveryAuthorizationValue(t *testing.T) {
	token := pieceBearer
	for _, row := range []struct {
		name   string
		text   string
		tokens []string
		want   string
	}{
		{"an echoed page", "<html><body><h1>401 Unauthorized</h1><pre>token=" + token + "\r\n" +
			"Authorization: Basic dXNlcjpwYXNz\r\nX-Forwarded: Bearer other.jwt-value</pre></body></html>",
			[]string{token},
			"401 Unauthorized token=[redacted] Authorization: Basic [redacted] X-Forwarded: Bearer [redacted]"},
		{"echoed JSON", `{"headers":{"authorization":"Bearer ` + token + `"}}`, []string{token},
			`{"headers":{"authorization":"Bearer [redacted]"}}`},
		{"a reason phrase", "Denied for " + token + " " + strings.Repeat("x", 200), []string{token},
			"Denied for [redacted] " + strings.Repeat("x", 98) + "…"},
		{"a token sent trimmed", "<p>token=" + token + "</p>", []string{" " + token + "\n"}, "token=[redacted]"},
		{"a DPoP authorization", "Authorization: DPoP eyJhbGciOi.payload.sig", nil, "Authorization: DPoP [redacted]"},
		{"a DPoP value", "x-seen: DPoP eyJhbGciOi.payload.sig", nil, "x-seen: DPoP [redacted]"},
		{"any case, and = for :", "AUTHORIZATION=token abc123; authorization: digest u=1", nil,
			"AUTHORIZATION=token [redacted]; authorization: digest [redacted]"},
		{"two tokens", "a=" + token + " b=gho_" + strings.Repeat("K7", 18), []string{token, "gho_" + strings.Repeat("K7", 18)},
			"a=[redacted] b=[redacted]"},
		{"a token shorter than a piece, only whole", "pw=abc12 and abc1", []string{"abc12"}, "pw=[redacted] and abc1"},
		{"no token", "nothing to hide " + token, []string{""}, "nothing to hide " + token},
	} {
		t.Run(row.name, func(t *testing.T) {
			if got := excerpt.Text(row.text, row.tokens...); got != row.want {
				t.Fatalf("Text = %q, want %q", got, row.want)
			}
		})
	}
}

// A gateway can echo the header cut short, split by markup, or twice with the copies overlapping,
// and an exact match finds none of those. Every run of 8 or more of the token's characters within
// what the excerpt reads is redacted; one that starts inside the scan limit and runs past it is
// in the word the limit cuts, which the excerpt drops whole.
func TestTextRedactsEveryPieceOfTheToken(t *testing.T) {
	for _, row := range []struct{ name, body, want string }{
		{"cut short by its last character", "<p>echo=" + pieceBearer[:len(pieceBearer)-1] + " end</p>", "echo=[redacted] end"},
		{"cut short by its last 10 characters", "<p>echo=" + pieceBearer[:len(pieceBearer)-10] + " end</p>", "echo=[redacted] end"},
		{"followed by a copy that starts inside its last characters", "<p>echo=" + pieceBearer + pieceBearer[2:] + " end</p>", "echo=[redacted] end"},
		{"split by a tag", "<p>echo=" + pieceBearer[:20] + "<wbr>" + pieceBearer[20:] + " end</p>", "echo=[redacted] [redacted] end"},
		// The copy starts 19 characters inside the 64 KiB scan limit and ends 17 past it.
		{"cut short, across the scan limit", "<style>" + strings.Repeat("s", excerpt.ScanLimit-75) + "</style>" +
			strings.Repeat("q", 39) + " " + pieceBearer[:len(pieceBearer)-10] + " after the limit", strings.Repeat("q", 39)},
	} {
		t.Run(row.name, func(t *testing.T) {
			if len(bearerPieces(row.body)) == 0 {
				t.Fatal("the body holds no piece of the bearer, so the row can see nothing")
			}
			got := excerpt.Text(row.body, pieceBearer)
			if got != row.want {
				t.Fatalf("Text = %q, want %q", got, row.want)
			}
			if held := bearerPieces(got); len(held) != 0 {
				t.Fatalf("Text kept pieces %q of the bearer", held)
			}
		})
	}
}

// A URL in the body can carry a credential the request never held as a token: a proxy's link back
// to a sign-in page with a password in its user info or a token in its query. The plain text, its
// markup gone, is read by redact.Remote, so each URL prints with no user info and no parameter's
// value, and no token need name it. A link inside a tag goes with the tag. (The rule reads a URL
// with user info only as the first URL, and one ending in a query only as the last words.)
func TestTextPrintsAURLWithoutItsCredentials(t *testing.T) {
	withUser := (&url.URL{Scheme: "https", User: url.UserPassword("ops", "s3cr3t-pass"), Host: "proxy.example", Path: "/help"}).String()
	body := `<html><body><a href="https://sso.example/login?access_token=abc123secret&next=/">` +
		`Sign in</a> or see ` + withUser + ` or https://proxy.example/help?token=zzz9secret;session=qq7secret</body></html>`
	want := "Sign in or see https://proxy.example/help or https://proxy.example/help?token=;session="
	if got := excerpt.Text(body, "unrelated-token"); got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	named := excerpt.BodyForText(body, "JSON")
	for _, secret := range []string{"abc123secret", "zzz9secret", "qq7secret", "s3cr3t-pass"} {
		if strings.Contains(named, secret) {
			t.Fatalf("BodyForText prints the URL credential %q: %q", secret, named)
		}
	}
}

// The rule fails closed: a plain text holding a URL it cannot read as clean, here one whose query
// holds an encoded separator, is printed as redact.WordsNotReadable and never as the text, nor as
// nothing. A Text that skipped the rule would print the token, and one that took its empty answer
// for the reading would print an empty body.
func TestTextPrintsAnUnreadableURLAsNotReadable(t *testing.T) {
	body := "<p>Moved: see https://proxy.example/login?next=%2Fhome&token=zzz9secret now</p>"
	if got := excerpt.Text(body); got != redact.WordsNotReadable {
		t.Fatalf("Text = %q, want %q", got, redact.WordsNotReadable)
	}
	if got, want := excerpt.BodyForText(body, "JSON"), "a body that is not JSON "+redact.WordsNotReadable; got != want {
		t.Fatalf("BodyForText = %q, want %q", got, want)
	}
	if got := excerpt.Text("<p>100% of the page, no URL</p>"); got != "100% of the page, no URL" {
		t.Fatalf("Text of a page with no URL = %q, want it read", got)
	}
}

// The excerpt reads at most the first 64 KiB characters of the body. Redacting shortens the text,
// so it runs on that slice and the cut comes after: nothing from past the limit is pulled in, and
// the word the limit falls inside, a token that starts inside the limit and runs past it here, is
// dropped whole, so none of it prints.
func TestTextScansAtMostTheFirst64KiB(t *testing.T) {
	bearer := "dsp_" + strings.Repeat("Zq9x", 10)
	styled := "<style>" + strings.Repeat(bearer, (excerpt.ScanLimit-80)/len(bearer)) + "</style>"
	shortOfLimit := excerpt.ScanLimit - len(styled)
	// Visible words end at the limit, and a token starts one character past it.
	past := excerpt.Text(styled+strings.Repeat("p", shortOfLimit)+" "+bearer+" after the limit", bearer)
	if want := strings.Repeat("p", shortOfLimit); past != want {
		t.Fatalf("past the limit: Text = %q, want %q", past, want)
	}
	// A token starts five characters short of the limit and ends past it.
	across := excerpt.Text(styled+strings.Repeat("q", shortOfLimit-6)+" "+bearer+" after the limit", bearer)
	if want := strings.Repeat("q", shortOfLimit-6); across != want {
		t.Fatalf("across the limit: Text = %q, want %q", across, want)
	}
}

// A body of nothing but markup is not empty: it is named by its size in bytes, and by what the
// excerpt reads where the body runs past that.
func TestBodyForTextNamesABodyOfMarkupAloneByItsSize(t *testing.T) {
	if got, want := excerpt.BodyForText("<p>\u00a0</p>", "JSON"), "a body of 9 bytes and no readable text"; got != want {
		t.Fatalf("BodyForText = %q, want %q", got, want)
	}
	if got, want := excerpt.BodyForText(strings.Repeat("<script>", 1<<17), "JSON"),
		"a body of 1,048,576 bytes and no readable text in its first 64 KiB"; got != want {
		t.Fatalf("BodyForText = %q, want %q", got, want)
	}
}

// Whatever answers controls the body, so the work an excerpt costs is bounded however large the
// body is and whatever its shape, and however the token is made: a megabyte of unclosed tags, and
// a token of one character repeated, which is thousands of pieces that are all the same.
func TestTextIsBoundedForAHostileBodyOrToken(t *testing.T) {
	began := time.Now()
	for _, unit := range []string{"<", "<script", "<script x", "<a <b", "</script>", "<script></script ", "secret", "<script>secret", "secre"} {
		hostile := strings.Repeat(unit, (1<<20)/len(unit)+1)
		if got := excerpt.BodyForText(hostile, "JSON", "secret"); !strings.HasPrefix(got, "a body") {
			t.Fatalf("BodyForText of %q repeated = %q", unit, got)
		}
	}
	if got := excerpt.Text(strings.Repeat("x", excerpt.ScanLimit), strings.Repeat("x", 2048)); got != "[redacted]" {
		t.Fatalf("Text of a repeated token = %q, want [redacted]", got)
	}
	if spent := time.Since(began); spent > 5*time.Second {
		t.Fatalf("the hostile bodies took %s, past the 5 s bound", spent)
	}
}
