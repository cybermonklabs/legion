package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/oidc"
)

// /healthz's issuer fields name each configured issuer's discovery and leave the answer they are
// added to as it was: a pair that is not configured is null, a configured one is false until its
// issuer is read, and the status code and every other field are the answer's own, a 503 included,
// so the fields never decide the status. healthzHandler's ways of writing an answer are read: its
// own (the starting answer), writeWaitingForBuckets' (a start waiting for a missing bucket),
// writeMissingBuckets' (a running listener whose bucket was deleted) and writeDependencyHealth's
// (an unhealthy one).
func TestTheHealthzIssuerFieldsNeverDecideTheAnswer(t *testing.T) {
	t.Parallel()
	auth := listenerAPIAuth{verifier: oidc.NewLazyVerifier("http://127.0.0.1:1", listenerTestAudience)}
	issuers := func() map[string]any { return issuerHealthFields(auth) }
	var starting atomic.Pointer[listenerDeps]
	waiting := &missingBuckets{}
	waiting.observe(bucketReads{missing: []string{bus.InterestBucket}})
	waitingError, err := json.Marshal((&bucketsMissingError{buckets: []string{bus.InterestBucket}}).Error() + "; the listener's start waits for it and serves no route until it exists")
	if err != nil {
		t.Fatal(err)
	}
	// A running listener whose role bucket was deleted under it: every other check passes (the
	// server answers and the durable is bound), and the self-health monitor's read found the bucket
	// missing, so /healthz answers 200 "degraded" through writeMissingBuckets.
	client := setupTestNATS(t)
	const consumer = "listener-healthz-issuers-with-a-bucket-missing"
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err := bustest.Provision(client.Conn, 1); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := client.JS().DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	gone := &missingBuckets{}
	gone.observe(readBuckets(t.Context(), client.JS(), []string{bus.RoleClaimBucket}))
	if !gone.holdsAnyOf([]string{bus.RoleClaimBucket}) {
		t.Fatalf("control: a read of the deleted role bucket did not find it missing")
	}
	var running atomic.Pointer[listenerDeps]
	running.Store(&listenerDeps{client: client, consumer: consumer, caches: []listenerCache{
		{name: "role claim", cache: pingStreamGone{}, buckets: []string{bus.RoleClaimBucket}, missing: gone},
	}})
	degradedError, err := json.Marshal((&bucketsMissingError{buckets: []string{bus.RoleClaimBucket}}).Error())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		answer func(w http.ResponseWriter)
		status int
		fields map[string]string
	}{
		{
			name: "the starting answer",
			answer: func(w http.ResponseWriter) {
				healthzHandler(&starting, nil, issuers).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			},
			status: http.StatusOK,
			fields: map[string]string{"status": `"starting"`},
		},
		{
			name: "the waiting answer",
			answer: func(w http.ResponseWriter) {
				healthzHandler(&starting, waiting, issuers).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			},
			status: http.StatusServiceUnavailable,
			fields: map[string]string{"status": `"waiting"`, "error": string(waitingError), "missing_buckets": `["` + bus.InterestBucket + `"]`},
		},
		{
			name: "the degraded answer for a missing bucket",
			answer: func(w http.ResponseWriter) {
				healthzHandler(&running, gone, issuers).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			},
			status: http.StatusOK,
			fields: map[string]string{"status": `"degraded"`, "error": string(degradedError), "missing_buckets": `["` + bus.RoleClaimBucket + `"]`},
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
