package oidc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// The rows of what the code flow and the verifier print of words an issuer wrote: an ID token's
// issuer and audience claims print only as the rule for remote text reads them, and a
// revocation_endpoint that does not parse prints only its class. The helpers they share with the
// configured issuer's rows are in configured_issuer_test.go.

// TestAnIDTokensIssuerClaimPrintsAsTheRuleReadsIt: an ID token whose iss claim is a URL carrying
// the probe's credentials is refused as another issuer's, and the refusal prints the claim as the
// rule for remote text reads it, through Verify (verify), the code flow's exchange (exchange) and
// its refresh (refresh). Captures oidc.id-token-issuer-claim/<row>.
func TestAnIDTokensIssuerClaimPrintsAsTheRuleReadsIt(t *testing.T) {
	claimRows(t, "oidc.id-token-issuer-claim", ErrIssuer, func(claims map[string]any) { claims["iss"] = redacttest.Code })
}

// TestAnIDTokensAudienceClaimPrintsAsTheRuleReadsIt: an ID token whose aud claim holds a URL
// carrying the probe's credentials, and not the verifier's audience, is refused as another
// audience's, and the refusal prints each audience as the rule for remote text reads it, through
// Verify (verify), the code flow's exchange (exchange) and its refresh (refresh). Captures
// oidc.id-token-audience-claim/<row>.
func TestAnIDTokensAudienceClaimPrintsAsTheRuleReadsIt(t *testing.T) {
	claimRows(t, "oidc.id-token-audience-claim", ErrAudience, func(claims map[string]any) { claims["aud"] = []string{"another-client", redacttest.Code} })
}

// claimRows runs the three rows of a claim spoil changes: a verifier's refusal, the exchange's and
// the refresh's, each of which must match want and print the claim only as the rule reads it.
func claimRows(t *testing.T, capture string, want error, spoil func(claims map[string]any)) {
	t.Helper()
	t.Run("verify", func(t *testing.T) {
		issuer := oidctest.New(t)
		verifier, signing := newVerifierFor(t, issuer)
		claims := issuer.Claims(testSubject, testAudience)
		spoil(claims)
		_, err := verifier.Verify(context.Background(), issuer.Mint(t, signing, claims))
		requireClaimRefusal(t, capture+"/verify", err, want)
	})
	t.Run("exchange", func(t *testing.T) {
		issuer, flow := newCodeFlowFor(t)
		issuer.SpoilIDTokens(spoil)
		_, err := signIn(t, issuer, flow)
		requireClaimRefusal(t, capture+"/exchange", err, want)
	})
	t.Run("refresh", func(t *testing.T) {
		issuer, flow := newCodeFlowFor(t)
		session, err := signIn(t, issuer, flow)
		if err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		issuer.SpoilIDTokens(spoil)
		_, err = flow.Refresh(context.Background(), session.RefreshToken)
		if !errors.Is(err, ErrIssuerUnavailable) {
			t.Fatalf("Refresh = %v, want %v", err, ErrIssuerUnavailable)
		}
		requireClaimRefusal(t, capture+"/refresh", err, want)
	})
}

// requireClaimRefusal fails t unless err matches want, prints none of the probe's credentials, and
// prints the spoiled claim as the rule reads it (redacttest.CodeShown).
func requireClaimRefusal(t *testing.T, capture string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("the error = %v, want %v", err, want)
	}
	redacttest.RequireNone(t, capture+"/error", err.Error())
	redacttest.RequireNone(t, capture+"/plus-v", fmt.Sprintf("%+v", err))
	if !strings.Contains(err.Error(), redacttest.CodeShown) {
		t.Errorf("the error does not print the claim as the rule reads it, %q: %q", redacttest.CodeShown, err)
	}
}

// TestARevocationEndpointThatDoesNotParsePrintsNoCredential: an issuer whose discovery document
// names a revocation_endpoint that carries the probe's credentials and does not parse fails
// Revoke before any request, and the error names the failure's class and none of the endpoint.
// Captures oidc.revoke-endpoint.
func TestARevocationEndpointThatDoesNotParsePrintsNoCredential(t *testing.T) {
	endpoint := "https://" + redacttest.User + ":" + redacttest.Password + "@issuer.example/revoke%zz?" + probeParam + "=" + redacttest.Token
	base := documentServer(t, func(base string) map[string]any {
		document := signInDocument(base, base)
		document["revocation_endpoint"] = endpoint
		return document
	})
	err := NewCodeFlow(base, flowClientID, flowClientSecret, "").Revoke(context.Background(), "plain-refresh-token")
	requireRefused(t, "oidc.revoke-endpoint", err, "oidc: revoke: ")
	if !strings.Contains(err.Error(), redact.ClassNotAURL) {
		t.Errorf("Revoke's error does not name the class %q: %q", redact.ClassNotAURL, err)
	}
}
