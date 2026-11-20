package routes

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestASignInWhoseExchangeTheIssuerRefusedAnswers502WithNoStatusLineItWrote: the sign-in pool's
// token endpoint answers the callback's code exchange with no OAuth error code, under a reason
// phrase that quotes a credential's URL. The callback answers 502 with the exchange's error in its
// body (authCallback), and that body prints none of the phrase the issuer wrote: the exchange's
// answer crossed the boundary, which keeps the status code's standard phrase. The designated
// assertion is routes.callback-502.
func TestASignInWhoseExchangeTheIssuerRefusedAnswers502WithNoStatusLineItWrote(t *testing.T) {
	rig := newSignInRig(t, "")
	rig.issuer.SignInAs(personClaims(rigGroup))
	authURL, stateCookie := rig.start(t, "http://dispatch.test/auth/start")
	code, state := rig.issuer.Authorize(t, authURL.String())
	rig.issuer.FailTokenWithStatusLine("400 see https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/help?session=" + redacttest.Token)
	response := callbackOn(rig.handler, code, state, stateCookie)
	if response.Code != http.StatusBadGateway || sessionCookie(response) != nil {
		t.Fatalf("callback status = %d body=%s, want 502 and no session", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "sign-in failed: oidc: exchange code") {
		t.Fatalf("the 502 body %q is not the exchange's error, so it shows nothing of the route", body)
	}
	redacttest.RequireNone(t, "routes.callback-502", body)
}
