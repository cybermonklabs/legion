package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

// /healthz's issuer fields name each configured issuer's discovery and leave the answer they are
// added to as it was: a pair that is not configured is null, a configured one is false until its
// issuer is read, and the status code and every other field pass through, a 503 included, so the
// fields never decide the status.
func TestTheHealthzIssuerFieldsNeverDecideTheAnswer(t *testing.T) {
	unhealthy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy","error":"nats unavailable","num_pending":9007199254740993}`))
	})
	auth := listenerAPIAuth{verifier: oidc.NewLazyVerifier("http://127.0.0.1:1", listenerTestAudience)}
	recorder := httptest.NewRecorder()
	withIssuerHealth(unhealthy, auth).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("/healthz with the issuer fields = %d (%q), want the 503 and the JSON type it was given", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("/healthz answered %q: %v", recorder.Body, err)
	}
	for field, want := range map[string]string{
		"status":      `"unhealthy"`,
		"error":       `"nats unavailable"`,
		"num_pending": `9007199254740993`,
		"oidc":        `false`,
		"google":      `null`,
	} {
		if got := string(body[field]); got != want {
			t.Errorf("/healthz %q = %s, want %s (answer %s)", field, got, want, recorder.Body)
		}
	}
}
