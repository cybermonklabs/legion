package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/routes"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestBrokerAccessAccountBootsWithoutOIDC: DISPATCH_ACCESS_ACCOUNTS takes no DISPATCH_OIDC_*
// setting. A boot naming the broker's account beside the broker Dispatch checks read tokens at
// (DISPATCH_AGENT_SECRETS_URL) resolves, builds no service-token verifier, and hands the router the
// list; the router it builds answers the person route to that service's read token with its proof.
// An access account need not be one DISPATCH_OIDC_ACCOUNTS admits, since it names a read token's
// service, which the service-token check never reads. With neither a broker nor a service-token
// verifier the list could admit nobody, so the boot refuses it.
//
// Controls: checkAccessSettings refusing the list without DISPATCH_OIDC_AUDIENCE, as before: the
// first boot is refused. requireAccessAccount reading only the projected token's actor, as before:
// the read token is refused 403 ACCESS_ACCOUNT_REQUIRED.
func TestBrokerAccessAccountBootsWithoutOIDC(t *testing.T) {
	const broker, reader = "system:serviceaccount:dispatch:envoy-broker", "system:serviceaccount:legion:reader"
	stub := brokertokentest.NewStubBroker(t)
	settings := map[string]string{
		"DISPATCH_AGENT_SECRETS_URL":                stub.Server.URL,
		"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": writeFile(t, "account-token", "dispatchs-own-account-token"),
		"DISPATCH_ACCESS_ACCOUNTS":                  broker,
	}
	boot := resolveWith(t, settings)
	if !slices.Equal(boot.AccessAccounts, []string{broker}) {
		t.Fatalf("AccessAccounts = %q, want %q", boot.AccessAccounts, []string{broker})
	}
	if boot.OIDCIssuer != "" || boot.OIDCAudience != "" || serviceTokenVerifier(boot) != nil {
		t.Fatalf("a boot with no DISPATCH_OIDC_* setting built a service-token verifier (issuer %q, audience %q)", boot.OIDCIssuer, boot.OIDCAudience)
	}
	if got := appContextOptions(boot, nil, routes.AppContextOptions{}).AccessAccounts; !slices.Equal(got, []string{broker}) {
		t.Fatalf("appContextOptions hands the router AccessAccounts %q, want %q", got, []string{broker})
	}

	database := storetest.Open(t)
	const origin = "https://dispatch.example"
	verifier, err := brokerTokenVerifier(boot, database, origin)
	if err != nil || verifier == nil {
		t.Fatalf("brokerTokenVerifier = %v, %v; want the broker-token check DISPATCH_AGENT_SECRETS_URL turns on", verifier, err)
	}
	handler := routerFor(t, boot, routes.AppContextOptions{Store: database, BrokerTokens: verifier})
	caller := brokertokentest.NewCaller(t)
	const token = "the-brokers-own-read-token"
	stub.Says(token, brokertoken.Introspection{
		Active: true, Session: "broker-hold", Service: broker, ProjectScope: brokertoken.ProjectScopeNone,
		Scope: brokertoken.ScopeRead, ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: brokertoken.ExchangeClientID, Thumbprint: caller.JKT(),
	})
	const path = "/api/v1/access/person?person=alice@example.com"
	signed := caller.Signed(t, http.MethodGet, origin+path, token)
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", signed.Header.Get("Authorization"))
	request.Header.Set("DPoP", signed.Header.Get("DPoP"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s with %s's read token on a boot with no DISPATCH_OIDC_* setting = %d %s, want 200", path, broker, response.Code, response.Body.String())
	}

	withOIDC := map[string]string{"DISPATCH_OIDC_ISSUER": "https://issuer.example", "DISPATCH_OIDC_AUDIENCE": "dispatch", "DISPATCH_OIDC_ACCOUNTS": reader}
	for name, value := range settings {
		withOIDC[name] = value
	}
	if boot := resolveWith(t, withOIDC); !slices.Equal(boot.AccessAccounts, []string{broker}) {
		t.Errorf("DISPATCH_OIDC_ACCOUNTS=%s beside DISPATCH_ACCESS_ACCOUNTS=%s: AccessAccounts = %q", reader, broker, boot.AccessAccounts)
	}
	refusedWith(t, map[string]string{"DISPATCH_ACCESS_ACCOUNTS": broker}, "DISPATCH_ACCESS_ACCOUNTS is set but neither DISPATCH_AGENT_SECRETS_URL nor DISPATCH_OIDC_AUDIENCE is")
}
