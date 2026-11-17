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

// The repair a listener names for a missing bucket, in /healthz's error and in the action of the
// ERROR line the alarm pages on, is the runbook's: `envoy-infra buckets create`, with --break-glass
// until the auth service's cutover, which needs no listener stopped. At 0668a6db6e both said "stop
// every listener of the deployment, then run `envoy-infra buckets create`": before the cutover no
// hub holds the key that create needs, so an operator who followed it took a full outage for a
// create the hub then refused.
func TestTheMissingBucketRepairIsTheRunbooksBreakGlassCreate(t *testing.T) {
	t.Parallel()
	logs := &cmdtest.LockedBuffer{}
	logMissingBuckets(logging.NewWithWriter("test-machine", logs), []string{bus.SessionBucket})
	var line struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &line); err != nil {
		t.Fatalf("read the ERROR line %q: %v", logs.String(), err)
	}
	for name, text := range map[string]string{
		"/healthz error":        (&bucketsMissingError{buckets: []string{bus.SessionBucket}}).Error(),
		"the ERROR line action": line.Action,
	} {
		for _, want := range []string{"`envoy-infra buckets create`", "`--break-glass`", "packages/envoy/infra/README.md#envoy-infra-buckets"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s = %q, want it to name %s", name, text, want)
			}
		}
		if strings.Contains(text, "stop every listener") {
			t.Errorf("%s = %q, want no step that stops every listener: a break-glass create needs none", name, text)
		}
	}
}
