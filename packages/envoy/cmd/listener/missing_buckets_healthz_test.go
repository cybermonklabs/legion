package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
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
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(t.Context(), client.Conn, session.WithSessionReplicas(1))
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
	handler := healthzHandler(&state, missing, nil)
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

// pingStreamGone is a cache whose watch is clean and whose ping answers that a stream is gone: the
// interest cache the moment either of its two buckets is deleted, before its watcher or the
// self-health monitor has seen the delete.
type pingStreamGone struct{ kvCache }

func (pingStreamGone) WatchErr() error { return nil }
func (pingStreamGone) Ping() error     { return natsgo.ErrStreamNotFound }

// /healthz names only the buckets a read finds missing. A cache reads two buckets, and the role
// bucket alone is deleted: its ping answers stream-not-found before the monitor's next read, and
// /healthz names envoy_roles and not envoy_interests, which is still there. At 0668a6db6e it named
// every bucket of the cache, so an operator was told to create a bucket the hub still held.
func TestHealthzNamesOnlyTheBucketsAReadFindsMissing(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	const consumer = "listener-healthz-names-the-missing-bucket"
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err := bustest.Provision(client.Conn, 1); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := client.JS().DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	if _, err := client.JS().StreamInfo("KV_" + bus.InterestBucket); err != nil {
		t.Fatalf("control: the interest bucket is not there to be named wrongly: %v", err)
	}
	missing := &missingBuckets{}
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, consumer: consumer, caches: []listenerCache{
		{name: "interest", cache: pingStreamGone{}, buckets: []string{bus.InterestBucket, bus.RoleClaimBucket}, missing: missing},
	}})
	recorder := httptest.NewRecorder()
	healthzHandler(&state, missing, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body struct {
		Status  string   `json:"status"`
		Missing []string `json:"missing_buckets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("read /healthz %q: %v", recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusOK || body.Status != "degraded" || !slices.Equal(body.Missing, []string{bus.RoleClaimBucket}) {
		t.Fatalf("/healthz with only %s deleted = %d %s, want 200 degraded naming [%s] alone", bus.RoleClaimBucket, recorder.Code, recorder.Body.String(), bus.RoleClaimBucket)
	}
}

// watchEnded is a cache whose watcher has ended with err, as a kvwatch watcher does the moment its
// bucket is deleted under it, before the self-health monitor's next read.
type watchEnded struct {
	kvCache
	err error
}

func (c watchEnded) WatchErr() error { return c.err }
func (c watchEnded) Ping() error     { return c.err }

// A watcher that ended because its bucket was deleted is answered as the missing bucket, 200
// "degraded" naming it, not as the watcher's end, 503 "<cache> KV watcher": a read of the cache's
// streams runs before /healthz answers the watcher's error. A watcher that ended while its bucket
// is still there is still 503. At a7fc913efa the deleted case answered 503 until the monitor's
// next tick (up to 30 s) named the bucket, so every listener's readiness probe could fail at once.
func TestAWatcherEndedByADeletedBucketIsAnsweredAsTheMissingBucket(t *testing.T) {
	t.Parallel()
	client := setupTestNATS(t)
	const consumer = "listener-healthz-watcher-ended"
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	if err := bustest.Provision(client.Conn, 1); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := client.JS().DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	ended := errors.New("session KV bucket was deleted under its watcher")
	answer := func(bucket string) (int, string, []string) {
		missing := &missingBuckets{}
		var state atomic.Pointer[listenerDeps]
		state.Store(&listenerDeps{client: client, consumer: consumer, caches: []listenerCache{
			{name: "session", cache: watchEnded{err: ended}, buckets: []string{bucket}, missing: missing},
		}})
		recorder := httptest.NewRecorder()
		healthzHandler(&state, missing, nil).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		var body struct {
			Status  string   `json:"status"`
			Error   string   `json:"error"`
			Missing []string `json:"missing_buckets"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		if body.Status == "unhealthy" {
			return recorder.Code, body.Error, body.Missing
		}
		return recorder.Code, body.Status, body.Missing
	}
	if code, status, missing := answer(bus.SessionBucket); code != http.StatusOK || status != "degraded" || !slices.Equal(missing, []string{bus.SessionBucket}) {
		t.Fatalf("/healthz over a watcher its deleted bucket ended = %d %q %v, want 200 degraded naming [%s]", code, status, missing, bus.SessionBucket)
	}
	if code, reported, _ := answer(bus.InterestBucket); code != http.StatusServiceUnavailable || !strings.Contains(reported, "KV watcher") {
		t.Fatalf("/healthz over a watcher that ended with its bucket still there = %d %q, want 503 naming the KV watcher", code, reported)
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
	missing := &missingBuckets{}
	missing.observe(bucketReads{missing: []string{bus.SessionBucket}})
	missing.report(logging.NewWithWriter("test-machine", logs))
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
