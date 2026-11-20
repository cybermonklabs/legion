package main

import (
	"encoding/base64"
	"net/url"

	"github.com/sjawhar/envoy/excerpt"
)

// answerText is body, an answer of Dispatch's or the listener's, as a message names it: an error
// JSON (Dispatch's writeError, {"error": ..., "code": ...}; the listener's writeJSONError,
// {"error": ..., "expected": [...]}) as its error and code read by the rule with each of tokens,
// the credentials the request carried, redacted (excerpt.ErrorJSON), its first 300 bytes (trunc),
// since whatever answered in the server's place can write one too. Any other body was written by
// something in the server's place, such as a gateway or a proxy, which can echo the request back,
// so it is named as not being notWhat, in one line with each of tokens and any Authorization,
// Bearer or DPoP value redacted (excerpt.BodyForText): never the body whole.
func answerText(body []byte, notWhat string, tokens ...string) string {
	if text, ok := excerpt.ErrorJSON(body, tokens...); ok {
		return trunc([]byte(text))
	}
	return excerpt.BodyForText(string(body), notWhat, tokens...)
}

// bodyText is res's body, the answer to a request a made, as a message names it (answerText),
// with every credential a request of a's can carry redacted from a body that is not Dispatch's
// own: a's agent bearer, which a request naming a person does not send and redacting does no
// harm to, and whatever a's base URL carries (urlCredentials). A refusal's body that is not
// Dispatch's error JSON is named as not being it; a 2xx body, which a message prints only when it
// did not hold what the caller asked for, as not being that answer. A request that got no answer
// has no body, so it names nothing, as it always did.
func (a *api) bodyText(res callResult) string {
	if res.status == 0 {
		return ""
	}
	notWhat := "Dispatch's error JSON"
	if res.status/100 == 2 {
		notWhat = "the answer asked for"
	}
	return answerText(res.body, notWhat, append(urlCredentials(a.base), a.token)...)
}

// urlCredentials is what a request to raw carries of the credentials its URL holds: net/http
// sends raw's userinfo as an Authorization: Basic header, so the header's value, the user name and
// the password each; and a password query parameter goes out in the request line. A message
// naming a body that is not the server's own redacts each of them (excerpt.BodyForText), since
// whatever answered in the server's place, such as a gateway, can echo the request back. A value
// that does not parse sends no request, so it carries nothing.
func urlCredentials(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	var carried []string
	if u.User != nil {
		password, _ := u.User.Password()
		basic := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		carried = append(carried, basic, u.User.Username(), password)
	}
	if password := u.Query().Get("password"); password != "" {
		carried = append(carried, password)
	}
	return carried
}
