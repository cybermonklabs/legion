// Package excerpt is one line of someone else's text, safe to print: a body or a reason phrase
// that a gateway, a proxy or anything else in a server's place wrote. Such a writer can echo the
// request back, its headers included, so every Go client that names a body that is not its
// server's own names it through BodyForText, with the tokens its request carried.
//
// It is @legion/envoy-client/excerpt (packages/envoy-client/src/excerpt.ts), rule for rule, so a
// Go client and a TypeScript one name one body the same way, with one rule after them: the plain
// text is then read by redact.Remote, which prints each URL with no user info and no parameter's
// value and fails closed, so a text it cannot read prints as redact.WordsNotReadable. The token
// rule is internal/redact's own (redact.Tokens), the one #4368's OAuth boundary names a refusal's
// body with. Three differences are deliberate. A character here is a Unicode code point, where the
// TypeScript counts UTF-16 units, and the two differ only past the Basic Multilingual Plane, where
// a cut here never splits a character. And this one decodes a page's HTML character references
// before the rule reads its text, and drops the word the scan limit falls inside, so neither a
// reference nor the limit can hide a URL's credential from the rule.
//
// It sits outside internal/ because the Legion daemon, a module of its own, reads it as Envoy's
// own commands do (packages/daemon/internal/dispatch, internal/notify and the rest); it may read
// Envoy's own internal/redact, since it is a package of the same module.
package excerpt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sjawhar/envoy/internal/redact"
)

// ScanLimit is the most of a body an excerpt scans, in characters (EXCERPT_SCAN_LIMIT). A gateway
// page's inline styles and scripts can run to tens of kilobytes before its first words, so this
// reaches well past them, and it bounds the work a body of any size can cost.
const ScanLimit = 64 * 1024

// headLength is the most characters an excerpt keeps, its ellipsis aside (textHead's HEAD_LENGTH).
const headLength = 120

const redacted = redact.Redacted

// space is the characters JavaScript's \s and String.prototype.trim read as whitespace, as a
// character class's contents. Go's \s is ASCII alone, so the patterns name these instead.
const space = `\t\n\x0B\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// isSpace reports whether r is one of the characters space names.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// The patterns excerpt.ts's excerpt runs, in its order. Go's regular expressions run in linear time
// whatever the text, and RE2 has no backreference, so the script-or-style block is spelled once for
// each name. A block left open runs to the end.
var (
	scriptOrStyle = regexp.MustCompile(`(?i)<(?:script\b[^<>]*>(?:[\s\S]*?</script[` + space + `]*>|[\s\S]*$)|` +
		`style\b[^<>]*>(?:[\s\S]*?</style[` + space + `]*>|[\s\S]*$))`)
	tag                = regexp.MustCompile(`<[^<>]*>`)
	authorizationValue = regexp.MustCompile(`(?i)(\bauthorization["']?[` + space + `]*[:=][` + space + `]*["']?` +
		`(?:(?:bearer|basic|digest|token|dpop)[` + space + `]+)?)[^` + space + `"'<>,;]+`)
	bearerValue = regexp.MustCompile(`(?i)(\bbearer[` + space + `]+)[^` + space + `"'<>,;]+`)
	dpopValue   = regexp.MustCompile(`(?i)(\bdpop[` + space + `]+)[^` + space + `"'<>,;]+`)
)

// Text is text a gateway chose (a body, or a reason phrase) as one line of plain text safe to
// quote (excerpt). Only the first ScanLimit characters are read, and where the limit falls inside
// a word, as it can inside a URL, that word is dropped: the excerpt ends at the last whitespace
// before the limit. That cut is made first, on the text as it came, so no redaction can move it.
// Three kinds of credential are then redacted: every piece of each of tokens, trimmed as a header
// sends it (redact.Tokens); after scripts, styles and tags are dropped and the page's character
// references decoded until none is left (redact.UnescapeHTML: &quest;, &#64;, &amp;quest; and
// the rest), each URL's user info and parameter values (redact.Remote, which reads the plain text
// as a reader of the page reads it); then the value after `Authorization:` (its scheme kept:
// Bearer, Basic, Digest, token or DPoP), after `Bearer` and after `DPoP`. A plain text Remote
// cannot read as clean is redact.WordsNotReadable, never the text. All of it runs before the cut
// to one line, so that cut never leaves a credential's first characters behind. Bytes that are
// not UTF-8 read as U+FFFD, and every rule runs in linear time, so a body of any size or shape
// costs a bounded amount of work.
func Text(text string, tokens ...string) string {
	read := text[:runeOffset(text, ScanLimit)]
	if rest := text[len(read):]; rest != "" {
		if after, _ := utf8.DecodeRuneInString(rest); !isSpace(after) {
			read = read[:max(strings.LastIndexFunc(read, isSpace), 0)]
		}
	}
	plain := redact.Tokens(strings.ToValidUTF8(read, "\uFFFD"), -1, tokens...)
	plain = scriptOrStyle.ReplaceAllString(plain, " ")
	plain = tag.ReplaceAllString(plain, " ")
	plain, readable := redact.Remote(redact.UnescapeHTML(plain))
	if !readable {
		return redact.WordsNotReadable
	}
	plain = authorizationValue.ReplaceAllString(plain, "${1}"+redacted)
	plain = bearerValue.ReplaceAllString(plain, "${1}"+redacted)
	plain = dpopValue.ReplaceAllString(plain, "${1}"+redacted)
	return head(plain)
}

// BodyForText is text, an answer's body that is not what its server writes (notWhat, such as
// "Dispatch's error JSON"), as an error names it (bodyForText): `a body that is not <notWhat>
// ("<excerpt>")`, one line with each of tokens and any Authorization, Bearer or DPoP value
// redacted (Text), quoted as JSON.stringify quotes it; `a body that is not <notWhat>
// (not printed: …)` where its words hold a URL the rule cannot read (redact.WordsNotReadable);
// `an empty body`; or, where the excerpt reads no text, the body's size in bytes and that it holds
// none (within the first ScanLimit characters where it runs past them). Never the body whole.
func BodyForText(text, notWhat string, tokens ...string) string {
	quoted := Text(text, tokens...)
	if quoted == redact.WordsNotReadable {
		return "a body that is not " + notWhat + " " + redact.WordsNotReadable
	}
	if quoted != "" {
		return "a body that is not " + notWhat + " (" + jsonQuote(quoted) + ")"
	}
	if text == "" {
		return "an empty body"
	}
	within := ""
	if runeOffset(text, ScanLimit) < len(text) {
		within = fmt.Sprintf(" in its first %d KiB", ScanLimit/1024)
	}
	return "a body of " + grouped(len(text)) + " bytes and no readable text" + within
}

// ErrorJSON is body as a message names a server's own error JSON: an object whose "error" is a
// string, as Dispatch, the Envoy listener and the secrets broker write a refusal. Whatever stands
// in the server's place can answer such an object too, echoing the request back in another member
// or quoting a URL that carries a credential, so only its "error" and a string "code" print, each
// with every piece of tokens redacted (redact.Tokens) and then read by the rule (redact.Field), as
// an object of those members alone. It answers false, and nothing, for any other body, which
// BodyForText names.
func ErrorJSON(body []byte, tokens ...string) (string, bool) {
	var refusal struct {
		Error *string         `json:"error"`
		Code  json.RawMessage `json:"code"`
	}
	if json.Unmarshal(body, &refusal) != nil || refusal.Error == nil {
		return "", false
	}
	read := func(field string) string { return redact.Field(redact.Tokens(field, -1, tokens...)) }
	shown := struct {
		Error string `json:"error"`
		Code  string `json:"code,omitempty"`
	}{Error: read(*refusal.Error)}
	var code string
	if json.Unmarshal(refusal.Code, &code) == nil {
		shown.Code = read(code)
	}
	var out strings.Builder
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(shown); err != nil {
		return "", false
	}
	return strings.TrimSuffix(out.String(), "\n"), true
}

// head is text on one line, its whitespace collapsed and trimmed, cut to headLength characters
// with an ellipsis when it runs past them (textHead).
func head(text string) string {
	var flat strings.Builder
	gap := false
	for _, r := range text {
		if isSpace(r) {
			gap = flat.Len() > 0
			continue
		}
		if gap {
			flat.WriteByte(' ')
			gap = false
		}
		flat.WriteRune(r)
	}
	line := flat.String()
	if cut := runeOffset(line, headLength); cut < len(line) {
		return line[:cut] + "…"
	}
	return line
}

// jsonQuote is s as JSON.stringify writes a string: a quote, a backslash and each control
// character escaped, every other character as it is (no HTML escape, unlike encoding/json).
func jsonQuote(s string) string {
	var quoted strings.Builder
	quoted.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			quoted.WriteString(`\"`)
		case '\\':
			quoted.WriteString(`\\`)
		case '\b':
			quoted.WriteString(`\b`)
		case '\f':
			quoted.WriteString(`\f`)
		case '\n':
			quoted.WriteString(`\n`)
		case '\r':
			quoted.WriteString(`\r`)
		case '\t':
			quoted.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&quoted, `\u%04x`, r)
			} else {
				quoted.WriteRune(r)
			}
		}
	}
	quoted.WriteByte('"')
	return quoted.String()
}

// grouped is n with a comma between each group of three digits, as toLocaleString("en-US")
// writes it.
func grouped(n int) string {
	digits := strconv.Itoa(n)
	var out strings.Builder
	for at, digit := range digits {
		if at > 0 && (len(digits)-at)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

// runeOffset is the byte offset in s just past its first n characters, or len(s) where it holds
// no more than n. A byte that is not UTF-8 counts as one character.
func runeOffset(s string, n int) int {
	for at := range s {
		if n == 0 {
			return at
		}
		n--
	}
	return len(s)
}
