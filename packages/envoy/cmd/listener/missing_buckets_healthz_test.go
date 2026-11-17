package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// A missing copied bucket skips only the checks of the cache that reads it: /healthz answers 200
// "degraded" naming it while every other check passes, and 503 for any other check that fails -
// here the durable consumer, deleted. At 7b262c2e0a a missing bucket answered 200 "degraded" before
// the other caches and the durable were checked, so ECS and Kubernetes saw a 200 over a listener
// whose durable was gone.
func TestAMissingBucketDoesNotHideAnotherFailureFromHealthz(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	const consumer = "listener-healthz-missing-bucket"
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	registry, err := store.Open(client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the registry's caches: %v", err)
	}
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("wait for the session cache: %v", err)
	}
	missing := &missingBuckets{}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, registry: registry, sessions: sessions, caches: listenerCaches(registry, sessions, nil, missing), consumer: consumer})
	handler := healthzHandler(&state, missing)
	get := func() (int, map[string]any) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body
	}

	if err := client.JS().DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	checkCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{}), client.JS(), listenerCopiedBuckets(false), missing)
	if code, body := get(); code != http.StatusOK || body["status"] != "degraded" || !strings.Contains(body["error"].(string), bus.SessionBucket) {
		t.Fatalf("/healthz with the session bucket missing and every other check passing = %d %v, want 200 degraded naming %s", code, body, bus.SessionBucket)
	}

	if err := client.JS().DeleteConsumer(bus.Stream, consumer); err != nil {
		t.Fatalf("delete the durable consumer: %v", err)
	}
	if code, body := get(); code != http.StatusServiceUnavailable || !strings.Contains(body["error"].(string), "durable consumer") {
		t.Fatalf("/healthz with the session bucket missing and the durable deleted = %d %v, want 503 for the durable consumer", code, body)
	}
}
