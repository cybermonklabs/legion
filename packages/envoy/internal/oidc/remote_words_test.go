package oidc

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARefusedRevocationPrintsTheIssuersCodeAsTheRuleReadsIt: the OAuth error code a refused
// revocation names is the issuer's word. One that quotes a URL carrying a credential prints in
// Revoke's error as the rule for remote text reads it, with the status, and none of the
// credentials.
func TestARefusedRevocationPrintsTheIssuersCodeAsTheRuleReadsIt(t *testing.T) {
	for _, row := range []struct{ name, code, shown string }{
		{"user information and a query value", redacttest.Words, redacttest.Shown},
		{"a fragment's value", redacttest.FragmentWords, redacttest.FragmentShown},
		{"words the rule cannot read", redacttest.Unreadable, redact.WordsNotReadable},
	} {
		t.Run(row.name, func(t *testing.T) {
			issuer, flow := newCodeFlowFor(t)
			issuer.FailRevocation(http.StatusBadRequest, row.code)
			err := flow.Revoke(context.Background(), "plain-refresh-token")
			if err == nil {
				t.Fatal("Revoke answered no error for the issuer's refusal")
			}
			redacttest.RequireNone(t, "oidc.Revoke", err.Error())
			if want := "oidc: revoke: the issuer answered HTTP 400 " + row.shown; err.Error() != want {
				t.Errorf("Revoke = %q, want %q", err, want)
			}
		})
	}
}
