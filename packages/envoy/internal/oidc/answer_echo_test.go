package oidc

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestARefusedRevocationPrintsNoTokenItsAnswerEchoes: whatever answers for the issuer's revocation
// endpoint refuses the revocation with an OAuth error code that repeats the refresh token the
// request carried. Revoke's error holds none of it, and still names the status: it reads the code
// through the record of the request it sent (redact.Carried), as every client of this repository
// reads a refused answer. Captures oidc.revoke-echo.
func TestARefusedRevocationPrintsNoTokenItsAnswerEchoes(t *testing.T) {
	issuer, flow := newCodeFlowFor(t)
	issuer.FailRevocation(http.StatusBadRequest, "the token "+redacttest.Token+" is not one this issuer issued")
	err := flow.Revoke(context.Background(), redacttest.Token)
	if err == nil {
		t.Fatal("Revoke answered no error for the issuer's refusal")
	}
	redacttest.RequireNone(t, "oidc.revoke-echo", err.Error())
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("Revoke = %q, want its status", err)
	}
}
