// Package oauthrefusal holds the rules this module reads an OAuth token endpoint's answer by. The
// status alone decides whether the endpoint answered: a 5xx, a 408 and a 429 are a server that
// could not answer now (Outage), as no answer at all is, and every other answer is the endpoint
// answering, whatever its body says; among those, a refusal is a 4xx (Refuses, Refused), and its
// OAuth error code (RFC 6749 section 5.2) only names the refusal's reason (Refusal). Ending a grant
// is narrower still (EndsGrant). Every reader of a token endpoint's answer that decides something
// on it - a sign-in refused, a client rejected, a login denied or ended - reads it through these,
// so each rule has one definition: the broker client's device flow among them, which ends a
// person's sign-in on this machine by EndsGrant (internal/cipherclient/device.go,
// DeviceFlow.Refresh). The broker's minters read GitHub's and Google's statuses by Refuses too
// (internal/cipher/secrets, mintStatusError), beside their own rate-limit reading. It imports only
// the standard library and golang.org/x/oauth2, so a command that links no OpenID Connect verifier
// can use it.
package oauthrefusal

import (
	"errors"
	"net/http"

	"golang.org/x/oauth2"
)

// Outage reports whether status is a server that could not answer the request now: any 5xx, a 408
// Request Timeout (it did not take the request) and a 429 Too Many Requests (it will not take one
// yet). The set is closed: every other status is the server answering, whatever its body names.
func Outage(status int) bool {
	return status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests
}

// Refuses reports whether status is one a token endpoint refuses a request with: any 4xx that is no
// Outage. RFC 6749 section 5.2 has an error answer be 400 "unless specified otherwise", and lets
// invalid_client be 401; an OAuth library may specify otherwise, as the broker's own,
// authelia.com/provider/oauth2, answers access_denied 403 (RFC 8628 section 3.5 answers a device
// code poll with the same error answer).
func Refuses(status int) bool {
	return status >= 400 && status < 500 && !Outage(status)
}

// Refused reports whether err is a token endpoint's answer (*oauth2.RetrieveError, wrapped or not)
// whose status Refuses, whatever OAuth error its body names, or none: the status alone decides a
// refusal, and the code only names its reason (Refusal).
func Refused(err error) bool {
	var retrieve *oauth2.RetrieveError
	return errors.As(err, &retrieve) && retrieve.Response != nil && Refuses(retrieve.Response.StatusCode)
}

// Refusal is the OAuth error code a refusal names (Refused), and empty for every other failure: an
// answer of any other status, whatever error its body names, and no answer at all. A refusal that
// names no error is Refused with an empty Refusal.
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
