// packages/envoy/internal/cipher/proxy/leftover.go
// The refusal of a whole placeholder the proxy did not replace (serveBound runs
// leftoverPlaceholder last, before anything goes upstream): what the command wrote is scanned,
// where it is looked for, and the grant its refusal line names.
package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
	"github.com/sjawhar/envoy/internal/cipher/proxyrules"
)

// placeholderLeft is the words a request that still carries a placeholder after the swap is
// refused with (leftoverPlaceholder). They name no placeholder and no grant: the refusal line
// names the grant.
const placeholderLeft = "this request still carries a placeholder after the swap - one no live grant of this session swaps on this host, or one outside the headers its kind names - so it is not sent: send a live grant's placeholder, in the header its kind names"

// placeholderPrefix is one prefix a leftover scan recognizes. The default prefix is unbounded:
// a placeholder immediately after a letter is still a proxy password. A kind's own prefix is
// bounded on the left, since a short service prefix such as tml- can occur inside a word (html-).
type placeholderPrefix struct {
	text        string
	leftBounded bool
}

// placeholderPrefixes is the default prefix and every distinct placeholder_prefix the kinds part
// names. The default remains first and unbounded; a kind prefix equal to it does not add a bounded
// second interpretation.
func placeholderPrefixes(rules proxyrules.Rules) []placeholderPrefix {
	prefixes := []placeholderPrefix{{text: placeholder.Prefix}}
	for _, kind := range rules.Kinds {
		if kind.PlaceholderPrefix == "" || kind.PlaceholderPrefix == placeholder.Prefix {
			continue
		}
		if !slices.ContainsFunc(prefixes, func(prefix placeholderPrefix) bool { return prefix.text == kind.PlaceholderPrefix }) {
			prefixes = append(prefixes, placeholderPrefix{text: kind.PlaceholderPrefix, leftBounded: true})
		}
	}
	return prefixes
}

// leftoverPlaceholder is the first whole placeholder a request would send, and false when it
// carries none. It scans the command-written headers with only the swapped placeholder removed
// (Rewrite with ""), trailers where the body is already read, and the path and query forms.
// prefixes holds the default prefix and every kind prefix of the rules this request was decided
// under.
func leftoverPlaceholder(prefixes []placeholderPrefix, header, trailer http.Header, path, rawPath, rawQuery string) (string, bool) {
	for _, fields := range [...]http.Header{header, trailer} {
		if placeholder, carried := fieldsPlaceholder(prefixes, fields); carried {
			return placeholder, true
		}
	}
	for _, text := range [...]string{path, rawPath, rawQuery} {
		if placeholder := placeholderInPrefixes(text, prefixes); placeholder != "" {
			return placeholder, true
		}
	}
	if unescaped := lenientUnescape(rawQuery); unescaped != rawQuery {
		if placeholder := placeholderInPrefixes(unescaped, prefixes); placeholder != "" {
			return placeholder, true
		}
	}
	return "", false
}

// fieldsPlaceholder is the first placeholder one map of header fields carries, read in
// headersOf's order once any value of the map has been found to carry one, and "" when none does.
func fieldsPlaceholder(prefixes []placeholderPrefix, fields http.Header) (string, bool) {
	for _, values := range fields {
		for _, v := range values {
			if headerPlaceholder(prefixes, v) == "" {
				continue
			}
			for _, h := range headersOf(fields) {
				if placeholder := headerPlaceholder(prefixes, h[1]); placeholder != "" {
					return placeholder, true
				}
			}
		}
	}
	return "", false
}

// lenientUnescape is query with each '+' read as a space and each valid %XX escape decoded, as
// url.QueryUnescape decodes them, but with an escape that is no escape kept as written rather
// than failing the whole: so the unescape of one query reads every pair of it, a pair holding ';'
// and the ten-thousandth among them, whatever else the query holds.
func lenientUnescape(query string) string {
	if !strings.ContainsAny(query, "%+") {
		return query
	}
	var b strings.Builder
	b.Grow(len(query))
	for i := 0; i < len(query); i++ {
		switch c := query[i]; {
		case c == '+':
			b.WriteByte(' ')
		case c == '%' && i+2 < len(query) && ishex(query[i+1]) && ishex(query[i+2]):
			b.WriteByte(unhex(query[i+1])<<4 | unhex(query[i+2]))
			i += 2
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ishex is whether c is a hexadecimal digit.
func ishex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// unhex is the value of hexadecimal digit c.
func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}

// headerPlaceholder is the first placeholder a header value carries, in the value itself or in
// the Basic credential it is, as git sends a token, with its base64 padded or not, and "" when it
// carries none.
func headerPlaceholder(prefixes []placeholderPrefix, v string) string {
	if placeholder := placeholderInPrefixes(v, prefixes); placeholder != "" {
		return placeholder
	}
	if scheme, payload, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "basic") {
		payload = strings.Trim(payload, " \t")
		for _, encoding := range [...]*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
			if decoded, err := encoding.DecodeString(payload); err == nil {
				return placeholderInPrefixes(string(decoded), prefixes)
			}
		}
	}
	return ""
}

// placeholderInPrefixes is the first placeholder text holds under prefixes, and "" when it holds
// none. A kind prefix needs a non-letter, non-digit byte before it; the default prefix does not.
func placeholderInPrefixes(text string, prefixes []placeholderPrefix) string {
	for i := range len(text) {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(text[i:], prefix.text) || prefix.leftBounded && i > 0 && asciiLetterOrDigit(text[i-1]) {
				continue
			}
			if found := placeholderAtPrefix(text[i:], prefix); found != "" {
				return found
			}
		}
	}
	return ""
}

// placeholderAtPrefix is the whole placeholder text opens with under prefix, and "" when it opens
// with none. The default prefix also recognizes #5190's minted form; a kind prefix takes its own
// 32 letters and digits directly.
func placeholderAtPrefix(text string, prefix placeholderPrefix) string {
	if prefix.text == placeholder.Prefix {
		return placeholderAt(text)
	}
	end := len(prefix.text) + placeholder.Length
	if end > len(text) || !alphabetOnly(text[len(prefix.text):end]) {
		return ""
	}
	return text[:end]
}

// asciiLetterOrDigit is whether b is one ASCII letter or digit, the left-boundary rule for a
// kind's own placeholder_prefix. logmask carries the same check on a rune for the log mask's
// placeholder scan; the two must agree.
func asciiLetterOrDigit(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// placeholderAt is the whole default-prefix placeholder text opens with, and "" when it opens
// with none: the prefix; where 1 to MaxMintedWord lower-case letters and a hyphen follow it, that
// minted kind's word and hyphen; and then Length characters of Alphabet, whatever follows them.
func placeholderAt(text string) string {
	opening := len(placeholder.Prefix)
	word := 0
	for word < placeholder.MaxMintedWord && opening+word < len(text) && 'a' <= text[opening+word] && text[opening+word] <= 'z' {
		word++
	}
	if word > 0 && opening+word < len(text) && text[opening+word] == '-' {
		opening += word + 1
	}
	end := opening + placeholder.Length
	if end > len(text) || !alphabetOnly(text[opening:end]) {
		return ""
	}
	return text[:end]
}

// alphabetOnly is whether every byte of s is one of placeholder.Alphabet.
func alphabetOnly(s string) bool {
	for i := range len(s) {
		if strings.IndexByte(placeholder.Alphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

// placeholderGrant is the grant the broker minted placeholder for, which the refusal of a request
// that still carried it after the swap names, and "" where the broker knows none: a release's
// placeholder while its row is held, a minted capability's while its grant lives
// (ProxyPlaceholderGrant). A read that failed is logged and names nothing: the request is refused
// either way.
func (s *Server) placeholderGrant(ctx context.Context, placeholder string) string {
	grantID, err := s.grants.ProxyPlaceholderGrant(ctx, placeholder)
	if err != nil {
		s.log.Error("proxy: the grant of a placeholder left after the swap could not be read", "error", err)
		return ""
	}
	return grantID
}
