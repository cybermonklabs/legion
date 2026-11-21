// Package oauthrefusal holds the rules this module reads an OAuth token endpoint's refusal by:
// which answers name an error code a caller may act on (RFC 6749 section 5.2), which one ends a
// grant, and which are a server that could not answer now, whatever its body says. Every reader of
// a token endpoint's error code that decides something on it - a sign-in refused, a client
// rejected, a login denied or ended - reads it through Refusal or EndsGrant, so each rule has one
// definition. The broker's minters read GitHub's and Google's statuses by Refuses too
// (internal/cipher/secrets, mintStatusError), beside their own rate-limit reading. It imports only
// the standard library and golang.org/x/oauth2, so a command that links no OpenID Connect verifier
// can use it.
package oauthrefusal

import (
	"errors"
	"net/http"

	"golang.org/x/oauth2"
)

// Refuses reports whether status is one a token endpoint refuses a request with: any 4xx but 408
// Request Timeout and 429 Too Many Requests. RFC 6749 section 5.2 has an error answer be 400
// "unless specified otherwise", and lets invalid_client be 401; an OAuth library may specify
// otherwise, as the broker's own, authelia.com/provider/oauth2, answers access_denied 403 (RFC 8628
// section 3.5 answers a device code poll with the same error answer). A 408 and a 429 are a server
// that did not take the request now, and a 5xx one that could not answer it: none of them says
// anything about the request, whatever its body names.
func Refuses(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests
}

// Refusal is the OAuth error code err names when err is a token endpoint's answer
// (*oauth2.RetrieveError, wrapped or not) whose status Refuses, and empty for every other failure:
// an answer of any other status, whatever error its body names, an answer that names no error, or
// no answer at all.
func Refusal(err error) string {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) || retrieve.Response == nil || !Refuses(retrieve.Response.StatusCode) {
		return ""
	}
	return retrieve.ErrorCode
}

// EndsGrant reports whether err is a token endpoint's answer 400 or 401 naming invalid_grant: the
// refresh token or code is expired, revoked or not the client's, so the grant is over. It is
// narrower than Refusal on purpose. Ending a grant is the strongest verdict a reader draws from an
// answer - it ends a person's sessions or deletes a machine's login - so it is drawn only from the
// statuses RFC 6749 section 5.2 itself gives a refusal, 400 and 401 for invalid_client. An
// invalid_grant at any other status, which Refusal reads as a code, ends nothing: a 403 one, which
// Dispatch's refresh has pinned as no end of a sign-in since it was written, among them.
func EndsGrant(err error) bool {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) || retrieve.Response == nil || retrieve.ErrorCode != "invalid_grant" {
		return false
	}
	status := retrieve.Response.StatusCode
	return status == http.StatusBadRequest || status == http.StatusUnauthorized
}
