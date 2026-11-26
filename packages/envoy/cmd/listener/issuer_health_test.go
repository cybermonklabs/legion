package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/envoy/internal/oidc"
)

// /healthz's issuer fields name each configured issuer's discovery and leave the answer they are
// added to as it was: a pair that is not configured is null, a configured one is false until its
// issuer is read, and the status code and every other field are the answer's own, a 503 included,
// so the fields never decide the status. Both of healthzHandler's ways of writing an answer are
// read: its own (the starting answer) and writeDependencyHealth's (an unhealthy one).
func TestTheHealthzIssuerFieldsNeverDecideTheAnswer(t *testing.T) {
	t.Parallel()
	auth := listenerAPIAuth{verifier: oidc.NewLazyVerifier("http://127.0.0.1:1", listenerTestAudience)}
	issuers := func() map[string]any { return issuerHealthFields(auth) }
	var starting atomic.Pointer[listenerDeps]
	for _, c := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		status int
		fields map[string]string
	}{
		{
			name: "the starting answer",
			answer: func(w http.ResponseWriter) {
				healthzHandler(&starting, issuers).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			},
			status: http.StatusOK,
			fields: map[string]string{"status": `"starting"`},
		},
		{
			name: "an unhealthy dependency",
			answer: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				writeDependencyHealth(w, "session kv", errors.New("probe failed"), true, issuers)
			},
			status: http.StatusServiceUnavailable,
			fields: map[string]string{"status": `"unhealthy"`, "error": `"session kv unavailable: probe failed"`},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c.answer(recorder)
			if recorder.Code != c.status || recorder.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("/healthz with the issuer fields = %d (%q), want %d and the JSON type", recorder.Code, recorder.Header().Get("Content-Type"), c.status)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("/healthz answered %q: %v", recorder.Body, err)
			}
			want := map[string]string{"oidc": `false`, "google": `null`}
			for field, value := range c.fields {
				want[field] = value
			}
			if len(body) != len(want) {
				t.Errorf("/healthz answered %d fields, want %d: %s", len(body), len(want), recorder.Body)
			}
			for field, value := range want {
				if got := string(body[field]); got != value {
					t.Errorf("/healthz %q = %s, want %s (answer %s)", field, got, value, recorder.Body)
				}
			}
		})
	}
}
