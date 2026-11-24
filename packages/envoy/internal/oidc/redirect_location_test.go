package oidc

import (
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestCodeFlowRevokePrintsNoLocationOfARedirect: whatever answers at the issuer's revocation
// endpoint answers the revocation, which carries the client secret in its Basic authorization,
// with a redirect whose Location quotes that secret and the probe's words, and does not parse. Go
// parses a Location before the client's CheckRedirect is asked, so net/http's error quotes it
// though Revoke follows no redirect; Revoke's error, which Dispatch logs, holds no run of the
// secret, none of the probe's words and no word of the Location. Captures oidc.revoke-redirect.
func TestCodeFlowRevokePrintsNoLocationOfARedirect(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.RedirectRevocation("/moved%zz" + flowClientSecret + " " + redacttest.Words)
	err := flow.Revoke(context.Background(), "plain-refresh-token")
	if err == nil {
		t.Fatal("Revoke answered no error for a Location that does not parse")
	}
	redacttest.RequireNoPiece(t, "oidc.revoke-redirect", err.Error(), flowClientSecret)
	if strings.Contains(err.Error(), "moved") {
		t.Errorf("Revoke's error names the Location: %q", err)
	}
}
