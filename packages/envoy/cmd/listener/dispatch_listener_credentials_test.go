package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/oidc/oidctest"
)

// dispatchListenerGoldens are the pods gcp:Dispatch renders for its listener Deployment, by the
// NATS credential they connect with: the seed, and natsAudience's projected token. read is the
// path from this package's directory and path the same file from the repository root, for a
// failure read in CI. The component lives in packages/envoy/infra, a Go module of its own, so this
// test reads what it renders rather than calling it.
var dispatchListenerGoldens = []struct{ read, path string }{
	{"../../infra/gcp/testdata/dispatch-listener-pod.golden", "packages/envoy/infra/gcp/testdata/dispatch-listener-pod.golden"},
	{"../../infra/gcp/testdata/dispatch-listener-pod-with-service-identity.golden", "packages/envoy/infra/gcp/testdata/dispatch-listener-pod-with-service-identity.golden"},
	{"../../infra/gcp/testdata/dispatch-listener-pod-private.golden", "packages/envoy/infra/gcp/testdata/dispatch-listener-pod-private.golden"},
}

// dispatchListenerGoldenEnv reads the envoy-listener container's environment out of the pod golden at
// read: each variable's value, and for one read from a Secret the empty string, its name a key of
// fromSecret. path names the golden from the repository root, for a failure read in CI.
func dispatchListenerGoldenEnv(t *testing.T, read, path string) (env map[string]string, fromSecret map[string]bool) {
	t.Helper()
	golden, err := os.ReadFile(read)
	if err != nil {
		t.Fatalf("read %s, a pod gcp:Dispatch renders for this command: %v", path, err)
	}
	var pod struct {
		Spec struct {
			Containers []struct {
				Name string `json:"name"`
				Env  []struct {
					Name      string `json:"name"`
					Value     string `json:"value"`
					ValueFrom *struct {
						SecretKeyRef *struct{} `json:"secretKeyRef"`
					} `json:"valueFrom"`
				} `json:"env"`
			} `json:"containers"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(golden, &pod); err != nil {
		t.Fatalf("parse %s as the pod JSON it records: %v", path, err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != "envoy-listener" {
			continue
		}
		env, fromSecret = map[string]string{}, map[string]bool{}
		for _, variable := range container.Env {
			env[variable.Name] = variable.Value
			if variable.ValueFrom != nil && variable.ValueFrom.SecretKeyRef != nil {
				fromSecret[variable.Name] = true
			}
		}
		return env, fromSecret
	}
	t.Fatalf("%s holds no envoy-listener container", path)
	return nil, nil
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
		t.Run(golden.path, func(t *testing.T) {
			rendered, fromSecret := dispatchListenerGoldenEnv(t, golden.read, golden.path)
			if !fromSecret["ENVOY_API_TOKEN"] {
				t.Fatalf("%s renders no ENVOY_API_TOKEN from a Secret: the listener would refuse the shared bearer an app pod of the release before presents", golden.path)
			}
			for _, name := range []string{"ENVOY_OIDC_ISSUER", "ENVOY_OIDC_AUDIENCE", "ENVOY_OIDC_ACCOUNTS", "ENVOY_DISPATCH_PRINCIPAL"} {
				if rendered[name] == "" {
					t.Fatalf("%s renders no %s: the listener would refuse Dispatch's own token", golden.path, name)
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
