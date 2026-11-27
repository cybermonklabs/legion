package identity

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/auth"
	"github.com/sjawhar/envoy/internal/oidc"
)

// IAPAssertionHeader is the header Identity-Aware Proxy puts its signed assertion in, on each
// request it lets through to the app.
const IAPAssertionHeader = "X-Goog-IAP-JWT-Assertion"

// HasIAPAssertion reports whether a request carries an IAP assertion, the browser credential an
// IAP deployment's requests present in place of a session cookie.
func HasIAPAssertion(r *http.Request) bool {
	return r.Header.Get(IAPAssertionHeader) != ""
}

// IAPIdentity resolves the person Identity-Aware Proxy signed in, from the assertion IAP signs for
// each request (IAPAssertionHeader, oidc.IAPVerifier). It never reads
// X-Goog-Authenticated-User-Email, or any other header IAP writes unsigned: a request that reached
// Dispatch around IAP can carry any of them, and only the signature tells IAP's word from a
// caller's. Rule decides who may use Dispatch, as it decides for a sign-in (DomainRule: an account
// of the Workspace domain, by the assertion's hd). IAP signs a fresh assertion for every request,
// so each request is decided on its own, and no session or refresh token is kept. Every person it
// admits is recorded as having signed in.
type IAPIdentity struct {
	Verifier *oidc.IAPVerifier
	Rule     SignInRule
	People   auth.PeopleStore
}

// Login returns the lowercased email of the person a valid assertion names, when Rule admits
// them; ErrNoIdentity for a request with no assertion, one that does not verify, or a person Rule
// refuses; ErrCredentialExpired, which is ErrNoIdentity too, for an assertion that is past its exp
// and sound in every other respect (oidc.IAPExpiredError: IAP's key set verifies its signature, it
// is IAP's to this audience, its iat holds) and whose person Rule still admits; an error matching
// ErrSignInUnavailable when IAP's key set could not be read, so no verdict was reached; or the
// people store's own error. An expired assertion that is wrong in any other way is refused for
// that, at WARN, as a fresh one would be.
//
// An expired assertion is logged at DEBUG, not WARN: IAP signs a fresh one for every request it
// passes, so the one request that carries an old assertion is a websocket's heartbeat, which
// re-checks the upgrade's assertion every few seconds and finds it expired once its ten minutes
// are up (api.whileCallerResolves), at every socket of every person. Its error is logged whole,
// since it is built only from a verified assertion and names only the exp IAP signed.
//
// A refused assertion is logged by its class alone (oidc.Reason), never its error: for an
// assertion whose signature failed, the error quotes the token's own unverified iss or aud
// (oidc.Verifier's classify), and Login runs before any authorization, so anyone who reaches
// Dispatch around IAP would choose that text on every request. The person refusals log the email
// and hd of a verified assertion, which IAP wrote.
func (i IAPIdentity) Login(r *http.Request) (string, error) {
	raw := r.Header.Get(IAPAssertionHeader)
	if raw == "" {
		return "", ErrNoIdentity
	}
	claims, err := i.Verifier.Verify(r.Context(), raw)
	var expired *oidc.IAPExpiredError
	switch {
	case errors.Is(err, oidc.ErrIssuerUnavailable):
		return "", fmt.Errorf("%w: %w", ErrSignInUnavailable, err)
	case errors.As(err, &expired):
		claims = expired.Claims
	case err != nil:
		slog.Warn("dispatch: IAP assertion refused", "reason", oidc.Reason(err))
		return "", ErrNoIdentity
	}
	signedIn, refused := i.Rule.Person(r.Context(), claims)
	if refused != nil {
		slog.Warn("dispatch: IAP person refused", append(i.Rule.RefusalLogFields(claims), "reason", refused)...)
		return "", ErrNoIdentity
	}
	if expired != nil {
		slog.Debug("dispatch: IAP assertion expired", "error", err)
		return "", ErrCredentialExpired
	}
	if err := i.People.Record(r.Context(), signedIn.Email); err != nil {
		return "", err
	}
	return signedIn.Email, nil
}
