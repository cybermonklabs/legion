package redact

import (
	"encoding/base64"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// TokenPiece is the shortest run of a credential's characters Tokens redacts wherever it stands.
// Something in a server's place, a gateway or a proxy, can echo the request back with a credential
// cut short, split by markup, or written twice with the copies overlapping (a token can end in its
// own first characters), so redacting only whole copies leaves most of one behind; a credential
// shorter than this is redacted only whole.
const TokenPiece = 8

// Redacted is what Tokens writes in place of a run of a credential's characters.
const Redacted = "[redacted]"

// Tokens is text with each run of at least TokenPiece characters of any of tokens (trimmed, as a
// header sends it; one shorter than TokenPiece only whole) replaced with one Redacted, for text
// another party wrote that can echo a request back: a refusal's body, its reason phrase. A
// character is a code point, and a byte that is not UTF-8 counts as one.
//
// limit bounds the text read, in characters; a negative limit reads it whole. Every run that
// starts inside the limit is redacted whole, one that runs past it read up to TokenPiece-1
// characters beyond, and the answer is cut to limit characters afterwards, since redaction changes
// the length; a run that starts past the limit is never reached, so nothing from past it moves in.
// The work is linear in the text read: each character is one lookup for each distinct piece
// length, at most TokenPiece of them, however many tokens or pieces there are.
func Tokens(text string, limit int, tokens ...string) string {
	pieces := map[string]struct{}{}
	lengths := map[int]struct{}{}
	for _, token := range tokens {
		characters := []rune(strings.TrimSpace(token))
		length := min(TokenPiece, len(characters))
		if length == 0 {
			continue
		}
		lengths[length] = struct{}{}
		for at := range len(characters) - length + 1 {
			pieces[string(characters[at:at+length])] = struct{}{}
		}
	}
	whole := limit < 0
	if whole {
		limit = utf8.RuneCountInString(text)
	}
	if len(pieces) == 0 {
		return text[:characterOffset(text, limit)]
	}
	// starts is the byte offset of each character read (the limit's and TokenPiece-1 more, for a
	// run that starts inside it), and one past the last.
	starts := make([]int, 0, min(len(text), limit+TokenPiece)+1)
	scanned := len(text)
	for at := range text {
		if len(starts) == limit+TokenPiece-1 {
			scanned = at
			break
		}
		starts = append(starts, at)
	}
	starts = append(starts, scanned)
	covered := make([]bool, scanned)
	for at := range len(starts) - 1 {
		for length := range lengths {
			if at+length >= len(starts) {
				continue
			}
			from, to := starts[at], starts[at+length]
			if _, ok := pieces[text[from:to]]; ok {
				for b := from; b < to; b++ {
					covered[b] = true
				}
			}
		}
	}
	end := characterOffset(text, limit)
	var out strings.Builder
	kept := 0
	for at := 0; at < end; at++ {
		if !covered[at] {
			continue
		}
		run := at
		for run < scanned && covered[run] {
			run++
		}
		out.WriteString(text[kept:at])
		out.WriteString(Redacted)
		kept, at = run, run
	}
	if kept < end {
		out.WriteString(text[kept:end])
	}
	answer := out.String()
	if whole {
		return answer
	}
	return answer[:characterOffset(answer, limit)]
}

// characterOffset is the byte offset in s just past its first n characters, or len(s) where it
// holds no more than n; a byte that is not UTF-8 counts as one character.
func characterOffset(s string, n int) int {
	for at := range s {
		if n == 0 {
			return at
		}
		n--
	}
	return len(s)
}

// credentialHeaders are the headers a request carries a credential on: a bearer, a client's
// Basic pair or a DPoP proof.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "DPoP"}

// credentialFields are the form and query fields a token request carries a credential in (RFC
// 6749, 7009, 7523, 7636, 8628 and 8693, and Google's token exchange), and the signed request
// object a device authorization carries (RFC 9101's `request`, which the broker's machine login
// takes as its proof of the machine's key).
var credentialFields = []string{
	"access_token", "actor_token", "assertion", "client_assertion", "client_secret", "code",
	"code_verifier", "device_code", "password", "refresh_token", "request", "subject_token", "token",
}

// requestTokens is each credential r carries, for Tokens: the value of each of credentialHeaders
// after its scheme (the whole value where it names none, as a DPoP proof does), a Basic pair's
// password as sent and unescaped (x/oauth2 escapes it), the password in r's URL, and every value
// of the credentialFields in its query and in its form, read through GetBody so the request is
// left as it was. A form x/oauth2 or net/http builds always has GetBody; one without it names no
// field.
func requestTokens(r *http.Request) []string {
	if r == nil {
		return nil
	}
	var tokens []string
	for _, name := range credentialHeaders {
		for _, value := range r.Header.Values(name) {
			scheme, credential, ok := strings.Cut(strings.TrimSpace(value), " ")
			if !ok {
				tokens = append(tokens, value)
				continue
			}
			tokens = append(tokens, credential)
			decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(credential))
			if !strings.EqualFold(scheme, "Basic") || err != nil {
				continue
			}
			if _, password, ok := strings.Cut(string(decoded), ":"); ok {
				tokens = append(tokens, password)
				if unescaped, err := url.QueryUnescape(password); err == nil && unescaped != password {
					tokens = append(tokens, unescaped)
				}
			}
		}
	}
	if r.URL != nil {
		if password, ok := r.URL.User.Password(); ok {
			tokens = append(tokens, password)
		}
		query := r.URL.Query()
		for _, field := range credentialFields {
			tokens = append(tokens, query[field]...)
		}
	}
	form := requestForm(r)
	for _, field := range credentialFields {
		tokens = append(tokens, form[field]...)
	}
	return tokens
}

// requestForm is r's form body, read again through GetBody (at most refusalBodyLimit), or nil
// when r carries no form or cannot give its body again.
func requestForm(r *http.Request) url.Values {
	if r.GetBody == nil {
		return nil
	}
	if media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); media != "application/x-www-form-urlencoded" {
		return nil
	}
	body, err := r.GetBody()
	if err != nil {
		return nil
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, refusalBodyLimit))
	if err != nil {
		return nil
	}
	form, _ := url.ParseQuery(string(data))
	return form
}

// refusalBodyLimit is the most of a refusal's body RefusalClient reads, the bound x/oauth2 reads a
// token endpoint's answer to (golang.org/x/oauth2/internal/token.go).
const refusalBodyLimit = 1 << 20

// RefusalClient is base (nil is http.DefaultClient), its timeout, redirect rule and cookie jar as
// they are, whose answers outside 2xx hold every credential their own request carried redacted
// (Tokens with requestTokens): their reason phrase, and their body, read to refusalBodyLimit. It is
// for a library that builds its requests' credentials itself and prints a refusal's body in an
// error of text alone, with no request left to read them from: golang.org/x/oauth2/google's
// external_account exchange sends the subject token it read and the client's Basic pair, then
// spends the access token it minted on the impersonation call, and prints `status code <n>:
// <body>` for either refusal, which OAuth2 can then only read by the rule (LibraryError). A 2xx is
// left as it is.
func RefusalClient(base *http.Client) *http.Client {
	client := new(http.Client)
	if base != nil {
		*client = *base
	}
	client.Transport = refusalTransport{client.Transport}
	return client
}

// refusalTransport sends each request through base (nil is http.DefaultTransport) and redacts an
// answer outside 2xx as RefusalClient says.
type refusalTransport struct{ base http.RoundTripper }

func (t refusalTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	tokens := requestTokens(r)
	response, err := base.RoundTrip(r)
	if err != nil || response.StatusCode >= 200 && response.StatusCode < 300 {
		return response, err
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, refusalBodyLimit))
	_ = response.Body.Close()
	if readErr != nil {
		body = nil
	}
	redacted := Tokens(string(body), -1, tokens...)
	response.Status = Tokens(response.Status, -1, tokens...)
	response.Body = io.NopCloser(strings.NewReader(redacted))
	response.ContentLength = int64(len(redacted))
	response.Header.Del("Content-Length")
	return response, nil
}
