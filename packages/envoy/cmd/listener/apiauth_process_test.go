package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The settings reach the handler the listener binary serves, which the handler's own tests, each
// building its configuration by hand, cannot show: started with ENVOY_OIDC_ACCOUNTS beside a
// local issuer, the binary admits the listed subject's projected token and refuses another's, and
// its own /metrics counts each admission under its credential and route. Started with no list,
// the same binary admits both, as it did before the list existed.
func TestTheListenerBinaryHoldsProjectedTokensToItsListAndCountsThem(t *testing.T) {
	issuer := oidctest.New(t)
	key := issuer.PublishKey(t, "listener-process-key")
	listed := issuer.Mint(t, key, issuer.Claims(listenerListedSubject, listenerTestAudience))
	unlisted := issuer.Mint(t, key, issuer.Claims(listenerUnlistedSubject, listenerTestAudience))
	binary := cmdtest.Build(t, "envoy-listener")
	uri := testnats.URL(t)
	pair := []string{"ENVOY_OIDC_ISSUER=" + issuer.URL(), "ENVOY_OIDC_AUDIENCE=" + listenerTestAudience}

	for _, tc := range []struct {
		name, machineID string
		env             []string
		wantUnlisted    int
		wantOIDC        int
	}{
		{name: "held to ENVOY_OIDC_ACCOUNTS", machineID: "oidc-accounts-held", env: append([]string{"ENVOY_OIDC_ACCOUNTS=" + listenerListedSubject}, pair...),
			wantUnlisted: http.StatusUnauthorized, wantOIDC: 1},
		{name: "with no list, as before", machineID: "oidc-accounts-unset", env: pair,
			wantUnlisted: http.StatusOK, wantOIDC: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := startListenerProcess(t, binary, uri, tc.machineID, tc.env...)
			listener.waitHealthy(t)
			base := "http://127.0.0.1:" + strconv.Itoa(listener.port)
			call := func(bearer string) int {
				t.Helper()
				request := mustRequest(t, http.MethodGet, base+"/v1/sessions")
				request.Header.Set("Authorization", "Bearer "+bearer)
				status, _ := do(t, request)
				return status
			}

			if status := call(listed); status != http.StatusOK {
				t.Fatalf("the listed subject's token: status %d, want 200\n%s", status, listener.Output.String())
			}
			if status := call(unlisted); status != tc.wantUnlisted {
				t.Fatalf("the unlisted subject's token: status %d, want %d\n%s", status, tc.wantUnlisted, listener.Output.String())
			}
			if status := call(listenerTestToken); status != http.StatusOK {
				t.Fatalf("the shared token: status %d, want 200", status)
			}

			_, metrics := do(t, mustRequest(t, http.MethodGet, base+"/metrics"))
			for _, want := range []string{
				`envoy_listener_requests_total{credential="oidc",path="/v1/sessions"} ` + strconv.Itoa(tc.wantOIDC),
				`envoy_listener_requests_total{credential="shared_token",path="/v1/sessions"} 1`,
			} {
				if !strings.Contains(metrics, want+"\n") {
					t.Fatalf("/metrics does not carry %s:\n%s", want, metrics)
				}
			}
		})
	}
}
