package oidc

import (
	"context"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// quotingStatusLine is a token endpoint's status line whose reason phrase quotes the probe's URL,
// credentials and all.
const quotingStatusLine = "400 see https://" + redacttest.User + ":" + redacttest.Password + "@idp.example/help?session=" + redacttest.Token

// TestAnIssuersStatusLinePrintsInNoErrorOfTheClientCheckOrTheExchange: a token endpoint that
// answers with no OAuth error code, under a reason phrase that quotes a credential's URL, gives an
// answer whose text x/oauth2 makes of its status line. The client check's error, which the broker
// logs, and the code exchange's, which Dispatch's sign-in callback writes in its 502 body, each
// wrap that answer as it crossed the boundary, and print none of the phrase the issuer wrote. The
// designated assertions are oidc.status-line/check-client and oidc.status-line/exchange.
func TestAnIssuersStatusLinePrintsInNoErrorOfTheClientCheckOrTheExchange(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	issuer.FailTokenWithStatusLine(quotingStatusLine)
	check, err := CheckClient(context.Background(), issuer.URL(), checkClientID, checkClientSecret)
	if check != ClientCheckIssuerUnreachable || err == nil {
		t.Fatalf("CheckClient = (%q, %v), want %q and an error", check, err, ClientCheckIssuerUnreachable)
	}
	redacttest.RequireNone(t, "oidc.status-line/check-client", err.Error())
	redacttest.RequireNone(t, "oidc.status-line/check-client", fmt.Sprintf("%+v", err))
	flow := NewCodeFlow(issuer.URL(), checkClientID, checkClientSecret, "")
	if _, err := flow.Exchange(context.Background(), "https://dispatch.example/auth/callback", "code", "nonce"); err == nil {
		t.Fatal("the exchange answered no error")
	} else {
		redacttest.RequireNone(t, "oidc.status-line/exchange", err.Error())
		redacttest.RequireNone(t, "oidc.status-line/exchange", fmt.Sprintf("%+v", err))
	}
}
