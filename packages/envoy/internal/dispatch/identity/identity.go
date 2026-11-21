// Package identity resolves the person attached to a Dispatch request, named by lowercase email.
package identity

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
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
// is still a member; otherwise ErrNoIdentity, an error matching ErrSignInUnavailable when a lapsed
// confirmation could not be renewed (Membership.Confirm), or the store's own error.
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
		return "", ErrNoIdentity
	}
	if i.Membership != nil {
		if err := i.Membership.Confirm(r.Context(), session.Login); err != nil {
			return "", err
		}
	}
	return session.Login, nil
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

// signInUnavailableMessage is what Refusal answers, with the code SIGNIN_UNAVAILABLE, a request
// that needs the sign-in issuer while the issuer cannot be reached.
const signInUnavailableMessage = "the sign-in issuer cannot be reached; try again in a minute"

// Refusal is the code and message an Identity error, or a sign-in's, is refused with: 401
// NO_IDENTITY for ErrNoIdentity; 503 SIGNIN_UNAVAILABLE for ErrSignInUnavailable (Membership: the
// person stays signed in, and the first request after the issuer answers is served); and 500
// IDENTITY_ERROR for anything else. The API's routes answer it through their own error writer, in
// their version; WriteError answers it everywhere else.
func Refusal(err error) (apicode.Code, string) {
	switch {
	case errors.Is(err, ErrNoIdentity):
		return apicode.NoIdentity, "unauthorized"
	case errors.Is(err, ErrSignInUnavailable):
		return apicode.SigninUnavailable, signInUnavailableMessage
	}
	return apicode.IdentityError, "identity resolution failed"
}

// WriteError renders an Identity error, or a sign-in's, as Dispatch's JSON error response: the
// code and message of Refusal, under the name and status /api/v1 answers that code with, which
// are the /auth routes' and a document socket's too.
func WriteError(w http.ResponseWriter, err error) {
	code, message := Refusal(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code.V1Status())
	_ = json.NewEncoder(w).Encode(map[string]string{"code": code.V1Name(), "error": message})
}
