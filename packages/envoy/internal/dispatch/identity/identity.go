// Package identity resolves the person attached to a Dispatch request, named by lowercase email.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
)

// ErrNoIdentity means the request did not prove a person: no cookie or header, a cookie whose
// session has ended, or a person whose membership could not be confirmed.
var ErrNoIdentity = errors.New("no identity")

// ErrCredentialExpired means a request's credential has expired and nothing else is wrong with it:
// an Identity-Aware Proxy assertion past its exp that IAP signed, whose signature IAP's key set
// verifies, for this deployment's audience, naming a person Rule still admits (IAPIdentity,
// oidc.IAPExpiredError). An expired assertion wrong in any other way is plain ErrNoIdentity. It is
// ErrNoIdentity to every caller, so WriteError answers it 401 NO_IDENTITY and it admits nobody. A
// websocket's heartbeat re-checks the assertion its upgrade verified, and reads this as the end of
// that assertion's life rather than of the person's: the socket closes with a code on which the
// page reopens it at once, and the reopened socket's fresh assertion is judged again
// (api.whileCallerResolves).
var ErrCredentialExpired = fmt.Errorf("%w: the credential has expired", ErrNoIdentity)

// ErrSignInUnavailable means a sign-in needed the sign-in issuer and could not reach it: a sign-in
// starting or completing before this process has discovered the issuer, or a lapsed confirmation
// the issuer could not renew (Membership), whose person stays signed in. Its error chain keeps the
// issuer's own failure. It is the sign-in issuer's alone: a service-account token's issuer being
// down is the API's OIDC_ISSUER_UNAVAILABLE, never this.
var ErrSignInUnavailable = errors.New("the sign-in issuer cannot be reached")

// The reasons a 401 NO_IDENTITY tells a person whose sign-in has ended (SignedOut.Reason), each
// read by the sign-in rule that no longer admits them (SignInRule.SignedOutReason), and spelled as
// AUTH_SIGNED_OUT_REASONS in packages/contracts/src/dispatch-api-limits.ts spells them.
const (
	// SignedOutNotListed: the person was a restricted member, from outside the sign-in domain, and
	// no project's access list names them any more (OutsideRule).
	SignedOutNotListed = "NOT_LISTED"
	// SignedOutOutsideOff: the person was a restricted member, from outside the sign-in domain, and
	// the server admits nobody from outside any more (DomainRule: a deployment that no longer
	// admits restricted members).
	SignedOutOutsideOff = "OUTSIDE_SIGNIN_OFF"
	// SignedOutWorkspaceAddress: the person was a restricted member, from outside the sign-in
	// domain, and their address is now on a domain the Google Workspace owns (OutsideRule:
	// DISPATCH_SIGNIN_DOMAIN, a domain DISPATCH_SIGNIN_WORKSPACE_DOMAINS lists, or a subdomain of
	// one), which signs in only as a Workspace account (ErrWorkspaceAddress).
	SignedOutWorkspaceAddress = "WORKSPACE_ADDRESS"
)

// SignedOut is ErrNoIdentity for a request whose session has ended, told with the reason the
// person is given (one of the SignedOut* codes) and the address whose sign-in ended. It matches
// ErrNoIdentity (errors.Is), so every caller that refuses no identity refuses it alike, and
// WriteError adds the reason and the address to the 401.
type SignedOut struct {
	Email  string
	Reason string
}

// Error words the reason as the 401's error does.
func (e *SignedOut) Error() string {
	switch e.Reason {
	case SignedOutOutsideOff:
		return "signed out: Dispatch no longer signs in " + e.Email + " or any other account outside its Google Workspace domain"
	case SignedOutWorkspaceAddress:
		return "signed out: " + e.Email + " is an address on a domain the Google Workspace owns, which signs in only as a Workspace account"
	}
	return "signed out: no Dispatch project's access list names " + e.Email + " any more"
}

// Is matches ErrNoIdentity.
func (e *SignedOut) Is(target error) bool {
	return target == ErrNoIdentity
}

// Identity resolves the person a request comes from, by lowercase email.
type Identity interface {
	Login(r *http.Request) (email string, err error)
}

// CookieIdentity resolves a signed Dispatch session cookie, signed with any key of SigningKey.
type CookieIdentity struct {
	SigningKey auth.SigningKeys
	Sessions   auth.SessionStore
	// Membership confirms, at least hourly, that the person a cookie names may still use Dispatch,
	// and ends their session when the sign-in issuer no longer says so. It is nil only on the local
	// dev sign-in server, whose people never signed in through an issuer: a cookie server with no
	// sign-in configured gets NoBrowserIdentity instead, never a CookieIdentity of its own.
	Membership *Membership
}

// Login returns the email of a valid session cookie whose generation is current and whose person
// is still a member; otherwise ErrNoIdentity (a SignedOut where the cookie's session has ended
// and Membership says why: CookieIdentity.ended), an error matching ErrSignInUnavailable when a
// lapsed confirmation could not be renewed (Membership.Confirm), or the store's own error. A
// cookie that does not verify, or has expired, is ErrNoIdentity alone: no reason is read for it.
func (i CookieIdentity) Login(r *http.Request) (string, error) {
	session, ok := auth.SessionFromRequest(r, i.SigningKey)
	if !ok {
		return "", ErrNoIdentity
	}
	if i.Sessions == nil {
		return "", ErrNoIdentity
	}
	generation, found, err := i.Sessions.CurrentSessionGeneration(r.Context(), session.Login)
	if err != nil {
		return "", err
	}
	if !found || generation != session.Generation {
		return "", i.ended(r.Context(), session.Login)
	}
	if i.Membership != nil {
		if err := i.Membership.Confirm(r.Context(), session.Login); err != nil {
			if errors.Is(err, ErrNoIdentity) {
				return "", i.ended(r.Context(), session.Login)
			}
			return "", err
		}
	}
	return session.Login, nil
}

// ended is ErrNoIdentity for email, whose session has ended, told as SignedOut where Membership
// says why (Membership.signedOut). It reads the reason afresh at every request rather than keeping
// it with the request that ended the session, so whichever request of the person's runs the hourly
// confirmation (a page's read, its live socket's heartbeat, whoami itself) and whichever server
// process answers the next one, that one says why. The local dev sign-in has no Membership and
// says nothing.
func (i CookieIdentity) ended(ctx context.Context, email string) error {
	if i.Membership == nil {
		return ErrNoIdentity
	}
	return i.Membership.signedOut(ctx, email)
}

// SessionCookies is an Identity whose person is proved by a signed session cookie
// (CookieIdentity), so a caller holding a request it resolved can later ask whether that cookie's
// session is still current: the agent-token mint does, under the person's people row lock.
type SessionCookies interface {
	// SessionCookie answers the login and session generation the request's session cookie
	// carries, verified under the identity's keys and read from nothing else; ok is false when the
	// request holds no cookie that verifies.
	SessionCookie(r *http.Request) (login string, generation int64, ok bool)
}

// SessionCookie answers the login and generation of r's session cookie, verified under the key
// set and with no read (SessionCookies).
func (i CookieIdentity) SessionCookie(r *http.Request) (string, int64, bool) {
	session, ok := auth.SessionFromRequest(r, i.SigningKey)
	return session.Login, session.Generation, ok
}

// HeaderIdentity resolves the person a test or local harness names in Header, by email. Every
// person it resolves is recorded as having signed in.
type HeaderIdentity struct {
	Header string
	People auth.PeopleStore
}

// Login returns the header's person, lowercased, or ErrNoIdentity when the header is absent.
func (i HeaderIdentity) Login(r *http.Request) (string, error) {
	email := strings.ToLower(strings.TrimSpace(r.Header.Get(i.Header)))
	if email == "" {
		return "", ErrNoIdentity
	}
	if err := i.People.Record(r.Context(), email); err != nil {
		return "", err
	}
	return email, nil
}

// NoBrowserIdentity is the identity of a cookie server with no sign-in configured: nobody can sign
// in there, and a cookie minted while sign-in was on must not outlive that configuration, since
// the key signing it is the deployment's and lives on. Every request that needs a person is
// then 401 NO_IDENTITY and the agent-token API is all the server serves.
type NoBrowserIdentity struct{}

// Login always returns ErrNoIdentity: no request names a person on a server nobody signs in to.
func (NoBrowserIdentity) Login(*http.Request) (string, error) {
	return "", ErrNoIdentity
}

// signInUnavailableMessage is what WriteError answers, with the code SIGNIN_UNAVAILABLE, a request
// that needs the sign-in issuer while the issuer cannot be reached.
const signInUnavailableMessage = "the sign-in issuer cannot be reached; try again in a minute"

// WriteError renders an Identity error, or a sign-in's, as Dispatch's JSON error response: 401
// NO_IDENTITY for ErrNoIdentity, with the reason and the address as `reason` and `login` for a
// SignedOut; 503 SIGNIN_UNAVAILABLE for ErrSignInUnavailable (Membership: the person stays signed
// in, and the first request after the issuer answers is served); and 500 IDENTITY_ERROR for
// anything else.
func WriteError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	body := map[string]string{"error": "identity resolution failed", "code": "IDENTITY_ERROR"}
	var signedOut *SignedOut
	switch {
	case errors.As(err, &signedOut):
		status = http.StatusUnauthorized
		body = map[string]string{"error": signedOut.Error(), "code": "NO_IDENTITY", "reason": signedOut.Reason, "login": signedOut.Email}
	case errors.Is(err, ErrNoIdentity):
		status = http.StatusUnauthorized
		body = map[string]string{"error": "unauthorized", "code": "NO_IDENTITY"}
	case errors.Is(err, ErrSignInUnavailable):
		status = http.StatusServiceUnavailable
		body = map[string]string{"error": signInUnavailableMessage, "code": "SIGNIN_UNAVAILABLE"}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
