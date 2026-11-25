package main

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestADomainSignInUnderAnotherIssuerPrintsNoneOfIt: a DISPATCH_SIGNIN_DOMAIN beside a
// DISPATCH_SIGNIN_ISSUER that is not Google's, carrying redacttest's credentials in its user
// information and its query, is refused naming both settings and Google's issuer, and the refusal
// holds no run of a credential. The refusal named the issuer as written. The twin shows the issuer
// holds every credential and is still refused naming DISPATCH_SIGNIN_ISSUER, so the row can see one
// printed. Captures dispatch.signin-issuer/other-issuer.
func TestADomainSignInUnderAnotherIssuerPrintsNoneOfIt(t *testing.T) {
	issuer := "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/pool?access" + "_token=" + redacttest.Token
	env := googleSignInEnvironment(map[string]string{"DISPATCH_SIGNIN_ISSUER": issuer})
	t.Run("twin", func(t *testing.T) {
		if held := redacttest.Held(issuer); len(held) != len(redacttest.Secrets) {
			t.Errorf("the issuer %q holds %v; want every credential, so the row can see one printed", issuer, held)
		}
		if _, err := resolveBootConfig(envGetter(env)); err == nil || !strings.Contains(err.Error(), "DISPATCH_SIGNIN_ISSUER") {
			t.Errorf("resolveBootConfig = %v; want a refusal naming DISPATCH_SIGNIN_ISSUER", err)
		}
	})
	t.Run("other-issuer", func(t *testing.T) {
		_, err := resolveBootConfig(envGetter(env))
		if err == nil {
			t.Fatalf("resolveBootConfig took DISPATCH_SIGNIN_ISSUER=%q beside a domain; want it refused", issuer)
		}
		if !strings.Contains(err.Error(), "DISPATCH_SIGNIN_DOMAIN needs DISPATCH_SIGNIN_ISSUER") {
			t.Errorf("resolveBootConfig = %v; want the domain's refusal of another issuer", err)
		}
		redacttest.RequireNoPiece(t, "dispatch.signin-issuer/other-issuer", err.Error(), redacttest.User, redacttest.Password, redacttest.Token)
	})
}
