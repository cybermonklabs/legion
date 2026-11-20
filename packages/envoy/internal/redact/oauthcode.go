package redact

import "golang.org/x/oauth2"

// OAuthCode is an OAuth error code read as a closed set: the codes RFC 6749 Section 5.2, RFC 8628
// Section 3.5 and RFC 9449 name, and redirect_uri_mismatch, which Google answers a redirect URI it
// does not know. It is decided from the code as it came, before any redaction, and it holds no
// text another party wrote, so it prints only its own name. A caller that tells OAuth refusals
// apart branches on it (RefusalCode) rather than on an answer's ErrorCode.
type OAuthCode uint8

const (
	// OAuthNone is no OAuth error answer, or one that names no code.
	OAuthNone OAuthCode = iota
	// OAuthOther is a code this set does not name.
	OAuthOther
	InvalidRequest
	InvalidClient
	InvalidGrant
	UnauthorizedClient
	UnsupportedGrantType
	InvalidScope
	AccessDenied
	ExpiredToken
	AuthorizationPending
	SlowDown
	InvalidDPoPProof
	UseDPoPNonce
	RedirectURIMismatch
)

// oauthCodeNames is each named code as the wire spells it.
var oauthCodeNames = [...]string{
	InvalidRequest:       "invalid_request",
	InvalidClient:        "invalid_client",
	InvalidGrant:         "invalid_grant",
	UnauthorizedClient:   "unauthorized_client",
	UnsupportedGrantType: "unsupported_grant_type",
	InvalidScope:         "invalid_scope",
	AccessDenied:         "access_denied",
	ExpiredToken:         "expired_token",
	AuthorizationPending: "authorization_pending",
	SlowDown:             "slow_down",
	InvalidDPoPProof:     "invalid_dpop_proof",
	UseDPoPNonce:         "use_dpop_nonce",
	RedirectURIMismatch:  "redirect_uri_mismatch",
}

// String is a named code as the wire spells it, and for OAuthNone, OAuthOther and a value outside
// the set, fixed words naming the case.
func (c OAuthCode) String() string {
	if int(c) < len(oauthCodeNames) && oauthCodeNames[c] != "" {
		return oauthCodeNames[c]
	}
	if c == OAuthNone {
		return "(no OAuth error code)"
	}
	return "(an OAuth error code this client does not name)"
}

// RefusalCode is the code of the first OAuth error answer down err's chain (errors.Unwrap, and
// each error of an errors.Join in turn), decided from the code as it came. An error that holds the
// code itself and reads it (a RefusalCode method, as brokerclient's RefusedError has) answers
// first, since the *oauth2.RetrieveError it unwraps to holds the rule's reading of the code and not
// the code (NewRetrieveError); x/oauth2's own *oauth2.RetrieveError answers from the ErrorCode
// x/oauth2 read off the answer. OAuthNone where the chain holds neither.
//
// The rule never changes which of the named codes a field is: a named code is plain lowercase
// letters and underscores, which the rule prints as they came, and the rule's answer for any other
// field is WordsNotReadable, a quoted string, or text that keeps each URL's scheme and slashes,
// none of which is a named code. So the RetrieveError NewRetrieveError builds answers the same code
// here as the field it was built from.
func RefusalCode(err error) OAuthCode {
	code, _ := refusalCode(err)
	return code
}

func refusalCode(err error) (OAuthCode, bool) {
	for err != nil {
		switch e := err.(type) {
		case interface{ RefusalCode() OAuthCode }:
			return e.RefusalCode(), true
		case *oauth2.RetrieveError:
			return NewRemoteText(e.ErrorCode).OAuthCode(), true
		}
		switch u := err.(type) {
		case interface{ Unwrap() error }:
			err = u.Unwrap()
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				if code, ok := refusalCode(inner); ok {
					return code, true
				}
			}
			return OAuthNone, false
		default:
			return OAuthNone, false
		}
	}
	return OAuthNone, false
}
