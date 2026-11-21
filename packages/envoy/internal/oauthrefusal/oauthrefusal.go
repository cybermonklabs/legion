// Package oauthrefusal holds the rules this module reads an OAuth token endpoint's refusal by:
// which answers name an error code a caller may act on (RFC 6749 section 5.2), which one ends a
// grant, and which are a server that could not answer now, whatever its body says. Every reader of
// a token endpoint's error code that decides something on it - a sign-in refused, a client
// rejected, a login denied or ended - reads it through Refusal or EndsGrant, so each rule has one
// definition. It imports only the standard library and golang.org/x/oauth2, so a command that links
// no OpenID Connect verifier can use it.
package oauthrefusal

import (
	"errors"
	"net/http"

	"golang.org/x/oauth2"
)

// Refuses reports whether status is one a token endpoint refuses a request with: 400 or 401. Any
// other status, a 5xx or a 429 among them, is a server that could not answer now, and says
// nothing about the request.
func Refuses(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnauthorized
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

// EndsGrant reports whether err is a token endpoint's answer 400 or 401 naming invalid_grant (RFC
// 6749 section 5.2): the refresh token or code is expired, revoked or not the client's, so the
// grant is over. It is narrower than Refusal on purpose. Ending a grant is the strongest verdict a
// reader draws from an answer - it ends a person's sessions or deletes a machine's login - and
// those are the statuses section 5.2 gives a refusal, so an invalid_grant at any other status,
// which Refusal may still read as a code, ends nothing.
func EndsGrant(err error) bool {
	var retrieve *oauth2.RetrieveError
	if !errors.As(err, &retrieve) || retrieve.Response == nil || retrieve.ErrorCode != "invalid_grant" {
		return false
	}
	status := retrieve.Response.StatusCode
	return status == http.StatusBadRequest || status == http.StatusUnauthorized
}
