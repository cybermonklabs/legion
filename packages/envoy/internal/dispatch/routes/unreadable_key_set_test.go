package routes

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// A callback whose ID-token check meets a sign-in issuer's key set that cannot be read reached no
// verdict on the person: the code flow reads its keys through oidc.Keys, so the exchange fails as
// the issuer unavailable (oidc.ErrIssuerUnavailable), and the callback answers 503
// SIGNIN_UNAVAILABLE and no session, as it answers a token endpoint that cannot answer, never the
// 502 of a sign-in that failed. An ID token signed by another key under the published key id, while
// the set reads, is a sign-in that failed: 502, as before.
func TestSignInCallbackAnswersAnUnreadableKeySet503(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// spoil changes the issuer after the sign-in has started and before its callback.
		spoil  func(t *testing.T, issuer *oidctest.Issuer)
		status int
	}{
		{"unreadable", func(_ *testing.T, issuer *oidctest.Issuer) {
			// The start discovered the issuer and read no key set: the code flow reads its keys
			// for the first ID token it checks, the callback's.
			issuer.FailKeys(http.StatusServiceUnavailable)
		}, http.StatusServiceUnavailable},
		{"refused", func(t *testing.T, issuer *oidctest.Issuer) {
			// Under the published key's own id, so the key set resolves and only the signature
			// fails.
			issuer.EnableCodeFlow(oidctest.NewKey(t, "signing-key"), rigClientID, "dispatch-secret", "http://dispatch.test/auth/callback")
		}, http.StatusBadGateway},
	} {
		t.Run(c.name, func(t *testing.T) {
			rig := newSignInRig(t, "")
			rig.issuer.SignInAs(personClaims(rigGroup))
			authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start?next=/issues/CORE-1")
			c.spoil(t, rig.issuer)
			code, state := rig.issuer.Authorize(t, authURL.String())
			response := callbackOn(rig.handler, code, state, stateCookie)
			unavailable := strings.Contains(response.Body.String(), `"code":"SIGNIN_UNAVAILABLE"`)
			if response.Code != c.status || unavailable != (c.status == http.StatusServiceUnavailable) || sessionCookie(response) != nil {
				t.Fatalf("the %s row's callback: %d %s, want %d, SIGNIN_UNAVAILABLE %t, and no session",
					c.name, response.Code, response.Body.String(), c.status, c.status == http.StatusServiceUnavailable)
			}
		})
	}
}
