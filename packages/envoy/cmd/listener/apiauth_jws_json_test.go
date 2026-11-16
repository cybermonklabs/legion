package main

// Regression test from the final review of cybermonklabs/legion#4160. At 17da83d280 it failed
// with EXPLOIT: an unlisted account's genuine Google identity token, re-sent as JWS JSON with a
// forged claim set between dots, was admitted as a listed account (204, no refusal line). It
// passes now that the Google branch reads every claim from the verified payload and only the
// compact serialization verifies.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

func TestRedTeamUnlistedGoogleIdentityAsJWSJSON(t *testing.T) {
	google := newGoogleStandIn(t, listenerGoogleAccount)
	cluster := newClusterIssuer(t)
	genuine := google.token(t, listenerGoogleOtherEmail, true, listenerGoogleAudience) // unlisted account
	parts := strings.Split(genuine, ".")
	forgedClaims, _ := json.Marshal(map[string]any{
		"iss":            google.issuer.URL(), // production: "https://accounts.google.com"
		"email":          listenerGoogleAccount,
		"email_verified": true,
	})
	envelope, _ := json.Marshal(map[string]string{
		"protected": parts[0], "payload": parts[1], "signature": parts[2],
		"x": "." + base64.RawURLEncoding.EncodeToString(forgedClaims) + ".",
	})
	forged := string(envelope)

	for _, tc := range []struct {
		name string
		auth listenerAPIAuth
	}{
		{"Google identities alone", listenerAPIAuth{google: google.identities}},
		{"beside a listed projected-token verifier and the shared token", listenerAPIAuth{token: "expected-token", verifier: cluster.verifier, accounts: []string{listenerListedSubject}, google: google.identities}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged bytes.Buffer
			met := metrics.New()
			auth := tc.auth
			auth.logger = logging.NewWithWriter("test-machine", &logged)
			auth.requests = newListenerRequests(met)

			control := httptest.NewRecorder()
			apiAuth(auth, served).ServeHTTP(control, bearerRequest("/v1/sessions", "Bearer "+genuine))
			if control.Code != http.StatusUnauthorized {
				t.Fatalf("control: the unlisted account's compact token got %d, want 401", control.Code)
			}

			recorder := httptest.NewRecorder()
			apiAuth(auth, served).ServeHTTP(recorder, bearerRequest("/v1/sessions", "Bearer "+forged))
			if recorder.Code != http.StatusUnauthorized {
				var scrape bytes.Buffer
				met.Handler().ServeHTTP(&responseBuffer{header: http.Header{}, body: &scrape}, httptest.NewRequest(http.MethodGet, "/metrics", nil))
				t.Fatalf("EXPLOIT: the unlisted account's token as JWS JSON got %d; log: %s; metrics: %s", recorder.Code, logged.String(), scrape.String())
			}
		})
	}
}

type responseBuffer struct {
	header http.Header
	body   *bytes.Buffer
}

func (r *responseBuffer) Header() http.Header         { return r.header }
func (r *responseBuffer) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *responseBuffer) WriteHeader(int)             {}
