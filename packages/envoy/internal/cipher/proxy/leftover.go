// packages/envoy/internal/cipher/proxy/leftover.go
// The refusal of a request that still carries a placeholder after the swap (serveBound runs
// leftoverPlaceholder last, before anything goes upstream): what is read as a placeholder, where
// it is looked for, and the grant its refusal line names.
package proxy

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/placeholder"
)

// placeholderLeft is the words a request that still carries a placeholder after the swap is
// refused with (leftoverPlaceholder). They name no placeholder and no grant: the refusal line
// names the grant.
const placeholderLeft = "this request still carries a placeholder after the swap - one no live grant of this session swaps on this host, or one outside the headers its entry names - so it is not sent: send a live grant's placeholder, in the header its entry names"

// leftoverPlaceholder is the first placeholder a request about to go upstream still carries, and
// false when it carries none: in a header or a trailer, in the value itself or inside the Basic
// credential it is - which reads at least what the swap reads, a Basic scheme in any case, a
// payload with or without base64 padding and one that is no UTF-8 among them, the safe side for
// a refusal - and then in the path, its raw form, the query as written and the query unescaped
// (lenientUnescape), so no escape that is no escape, and no pair url.ParseQuery would skip,
// hides a placeholder. A placeholder is one as the broker mints it (placeholderIn). Each map is
// read in headersOf's order only once one of its values has been found to carry one, so which
// placeholder a line names does not depend on the run, and a request that carries none - every
// request the swap served - costs no sort. The unescape is read last and only where it says
// something the query as written did not: a query the unescape leaves as written has been read
// already, and one it changes is not unescaped at all once an earlier text has answered.
//
// The trailer map holds the trailers of a request whose body this proxy has already read - an
// AWS call it signs, a Limited name's post it reads - which net/http fills as it reads the last
// chunk. A request whose body is streamed to the service sends its trailers after this point,
// so they are not in the map and no scan before the forward could hold them (the comment at the
// call in serveBound says which paths those are).
func leftoverPlaceholder(header, trailer http.Header, path, rawPath, rawQuery string) (string, bool) {
	for _, fields := range [...]http.Header{header, trailer} {
		if placeholder, carried := fieldsPlaceholder(fields); carried {
			return placeholder, true
		}
	}
	for _, text := range [...]string{path, rawPath, rawQuery} {
		if placeholder := placeholderIn(text); placeholder != "" {
			return placeholder, true
		}
	}
	if unescaped := lenientUnescape(rawQuery); unescaped != rawQuery {
		if placeholder := placeholderIn(unescaped); placeholder != "" {
			return placeholder, true
		}
	}
	return "", false
}

// fieldsPlaceholder is the first placeholder one map of header fields carries, read in
// headersOf's order once any value of the map has been found to carry one, and "" when none
// does. The two passes are what keeps the line's placeholder the same between two runs of one
// request while a request that carries none sorts nothing.
func fieldsPlaceholder(fields http.Header) (string, bool) {
	for _, values := range fields {
		for _, v := range values {
			if headerPlaceholder(v) == "" {
				continue
			}
			for _, h := range headersOf(fields) {
				if placeholder := headerPlaceholder(h[1]); placeholder != "" {
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
func headerPlaceholder(v string) string {
	if placeholder := placeholderIn(v); placeholder != "" {
		return placeholder
	}
	if scheme, payload, ok := strings.Cut(v, " "); ok && strings.EqualFold(scheme, "basic") {
		payload = strings.Trim(payload, " \t")
		for _, encoding := range [...]*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
			if decoded, err := encoding.DecodeString(payload); err == nil {
				return placeholderIn(string(decoded))
			}
		}
	}
	return ""
}

// placeholderIn is the first placeholder text holds, and "" when it holds none. A placeholder is
// one as the broker mints it (placeholderAt): a secret's release, placeholder.Prefix and Length
// letters and digits of Alphabet, and a minted capability's grant, the same after a kind's word
// and hyphen (placeholder.MintedPrefix). The prefix with fewer of them after it is no placeholder
// and opens nothing - a code search for the prefix itself is one - so it is not read as one. A
// whole placeholder is, whether or not a grant holds it: the broker deletes an ended grant's rows,
// so a placeholder it no longer knows and one it never minted cannot be told apart.
//
// It scans placeholder.Prefix alone, the one prefix every placeholder has today, a minted kind's
// among them. A kind that sets a prefix of its own (placeholder_prefix) adds that prefix to what
// this scans, or a placeholder of that kind left after the swap would go upstream.
func placeholderIn(text string) string {
	for rest := text; ; {
		i := strings.Index(rest, placeholder.Prefix)
		if i < 0 {
			return ""
		}
		rest = rest[i:]
		if found := placeholderAt(rest); found != "" {
			return found
		}
		rest = rest[len(placeholder.Prefix):]
	}
}

// placeholderAt is the whole placeholder text opens with, and "" when it opens with none: the
// prefix; where 1 to MaxMintedWord lower-case letters and a hyphen follow it, that minted kind's
// word and hyphen, as the log mask reads one; and then Length characters of Alphabet, whatever
// follows them. A release's placeholder holds no hyphen, so a word is read only where a minted
// kind's prefix put one.
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
