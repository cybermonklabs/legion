package oidc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/redact/redacttest"
)

// TestACheckClientsErrorPrintsNoneOfTheIssuersWords: a token endpoint that refuses the check at a
// refusal's status with an error code quoting a URL that carries a credential refuses the client
// (CheckClient: any refusal but invalid_grant is ClientCheckRejected), and the check's error, which
// quotes the code, prints none of the credential. One that answers the same code at an outage's
// status could not answer, and the check's error, which wraps x/oauth2's answer with %w, prints
// none of it either. A token endpoint that answers nothing reaches the check's other error, which
// wraps a transport failure and carries no answer at all.
func TestACheckClientsErrorPrintsNoneOfTheIssuersWords(t *testing.T) {
	issuer := oidctest.New(t)
	issuer.EnableCodeFlow(issuer.PublishKey(t, "signing-key"), checkClientID, checkClientSecret)
	for name, row := range map[string]struct {
		fail func()
		want ClientCheck
	}{
		"a refusal whose code quotes a credential's URL": {func() { issuer.FailToken(http.StatusBadRequest, redacttest.Words) }, ClientCheckRejected},
		"an outage whose code quotes a credential's URL": {func() { issuer.FailToken(http.StatusServiceUnavailable, redacttest.Words) }, ClientCheckIssuerUnreachable},
		"no answer at all": {issuer.DropToken, ClientCheckIssuerUnreachable},
	} {
		t.Run(name, func(t *testing.T) {
			row.fail()
			defer issuer.FailToken(0, "")
			check, err := CheckClient(context.Background(), issuer.URL(), checkClientID, checkClientSecret)
			if check != row.want || err == nil {
				t.Fatalf("CheckClient = (%q, %v), want %q and an error", check, err, row.want)
			}
			redacttest.RequireNone(t, "oidc.check-client/error", err.Error())
			redacttest.RequireNone(t, "oidc.check-client/plus-v", fmt.Sprintf("%+v", err))
		})
	}
}

// TestADiscoveryErrorPrintsNoneOfTheIssuersWords: an issuer whose discovery document answers 500
// with a body that quotes a URL carrying a credential cannot be asked, and the check's error, which
// wraps go-oidc's discovery error (`<status>: <body>`) with %w, prints none of the credential.
func TestADiscoveryErrorPrintsNoneOfTheIssuersWords(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, redacttest.Words, http.StatusInternalServerError)
	}))
	defer down.Close()
	check, err := CheckClient(context.Background(), down.URL, checkClientID, checkClientSecret)
	if check != ClientCheckIssuerUnreachable || err == nil {
		t.Fatalf("CheckClient = (%q, %v), want %q and an error", check, err, ClientCheckIssuerUnreachable)
	}
	redacttest.RequireNone(t, "oidc.discovery/error", err.Error())
	redacttest.RequireNone(t, "oidc.discovery/plus-v", fmt.Sprintf("%+v", err))
}
