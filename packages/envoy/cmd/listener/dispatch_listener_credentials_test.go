package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// dispatchListenerGoldens are the pods gcp:Dispatch renders for its listener Deployment, by the
// NATS credential they connect with: the seed, and natsAudience's projected token. The component
// lives in packages/envoy/infra, a Go module of its own, so this test reads what it renders
// (renderedListenerEnv) rather than calling it.
var dispatchListenerGoldens = []string{
	"dispatch-listener-pod.golden",
	"dispatch-listener-pod-with-service-identity.golden",
}

// Dispatch's listener is a Deployment of its own, which rolls apart from the app's, so in the
// release that moves Dispatch onto its own identity the listener admits both credentials: the
// shared bearer (ENVOY_API_TOKEN), which an app pod of the release before presents, and Dispatch's
// projected service-account token, which the app presents from this release on. A release that
// switched the listener's credential and Dispatch's at once would refuse every call between an old
// pod of one and a new pod of the other 401, which Dispatch settles as failed. The shared bearer
// goes in a later release, once envoy_listener_requests_total{credential="shared_token"} has read
// zero.
//
// Each listener environment gcp:Dispatch renders, with the seed and with natsAudience, is run
// through the listener's own settings readers and apiAuth, with the Secret's bearer and the
// cluster's issuer stood in for by a test's: both credentials are admitted, and the controls, a
// token of another account for the same audience and no credential at all, are refused.
func TestDispatchsListenerAdmitsBothCredentialsThisRelease(t *testing.T) {
	for _, golden := range dispatchListenerGoldens {
		t.Run(golden, func(t *testing.T) {
			rendered, fromSecret := renderedListenerEnv(t, golden, "envoy-listener")
			if !fromSecret["ENVOY_API_TOKEN"] {
				t.Fatalf("%s renders no ENVOY_API_TOKEN from a Secret: the listener would refuse the shared bearer an app pod of the release before presents", golden)
			}
			for _, name := range []string{"ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_ACCOUNTS", "ENVOY_DISPATCH_PRINCIPAL"} {
				if rendered[name] == "" {
					t.Fatalf("%s renders no %s: the listener would refuse Dispatch's own token", golden, name)
				}
			}
			issuer := oidctest.New(t)
			key := issuer.PublishKey(t, "dispatch-listener-key")
			shared := rand.Text()
			env := map[string]string{}
			for name, value := range rendered {
				env[name] = value
			}
			env["ENVOY_API_TOKEN"], env["ENVOY_OIDC_ISSUER"] = shared, issuer.URL()
			getenv := func(name string) string { return env[name] }

			oidcIssuer, audience, err := resolveListenerOIDCConfig(getenv)
			if err != nil {
				t.Fatal(err)
			}
			accounts, err := resolveListenerOIDCAccounts(getenv, oidcIssuer)
			if err != nil {
				t.Fatal(err)
			}
			principal, err := resolveListenerDispatchPrincipal(getenv, oidcIssuer, accounts, nil)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := discoverListenerVerifier(context.Background(), oidcIssuer, audience)
			if err != nil {
				t.Fatal(err)
			}
			var logged bytes.Buffer
			auth := apiAuth(listenerAPIAuth{token: getenv("ENVOY_API_TOKEN"), verifier: verifier, accounts: accounts, logger: logging.NewWithWriter("test-machine", &logged)}, served)

			for _, row := range []struct {
				name, authorize string
				want            int
			}{
				{"the shared bearer, which an app pod of the release before presents", "Bearer " + shared, http.StatusNoContent},
				{"Dispatch's own projected token for the listener's audience", "Bearer " + issuer.Mint(t, key, issuer.Claims(principal, audience)), http.StatusNoContent},
				{"control: another account's token for the same audience", "Bearer " + issuer.Mint(t, key, issuer.Claims(listenerUnlistedSubject, audience)), http.StatusUnauthorized},
				{"control: no credential", "", http.StatusUnauthorized},
			} {
				recorder := httptest.NewRecorder()
				auth.ServeHTTP(recorder, bearerRequest("/v1/sessions", row.authorize))
				if recorder.Code != row.want {
					t.Errorf("%s: status %d, want %d; body %s; log: %s", row.name, recorder.Code, row.want, recorder.Body.String(), logged.String())
				}
			}
		})
	}
}
