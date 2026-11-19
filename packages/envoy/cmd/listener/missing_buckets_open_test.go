package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A copied bucket deleted after the start's wait found it and before its store opened it holds the
// start, and never ends it (openExisting): the open answers bus.ErrBucketMissing, the start waits
// for the bucket again as its first wait does, /healthz answers 503 "waiting" naming it, nothing
// creates it, and once the operator creates it the open completes and the store serves.
//
// The order is forced: the wait runs and finds the bucket, the test deletes it, and only then does
// the store open. On the start before openExisting, the store's open error went to log.Fatal and
// the listener exited, so this test's open returns that error at once and fails its first check.
func TestABucketDeletedBetweenTheStartsWaitAndItsOpenHoldsTheStart(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn := testnats.Connect(t, uri)
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the session bucket: %v", err)
	}
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test-machine", logs)
	missing := &missingBuckets{}
	wait := func() error {
		return awaitCopiedBuckets(context.Background(), logger, conn, js, []string{bus.SessionBucket}, missing)
	}
	if err := wait(); err != nil {
		t.Fatalf("the start's wait with the bucket there: %v", err)
	}

	// Deleted between the start's wait and the store's open.
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	type opened struct {
		sessions *session.SessionRegistry
		err      error
	}
	done := make(chan opened, 1)
	go func() {
		sessions, err := openExisting(wait, func() (*session.SessionRegistry, error) {
			return session.OpenSessionRegistry(t.Context(), conn, session.WithSessionReplicas(1))
		})
		done <- opened{sessions, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(missing.list(), []string{bus.SessionBucket}) {
		select {
		case got := <-done:
			t.Fatalf("the store's open returned (%v) with its bucket deleted after the start's wait, want it waiting for the bucket", got.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wait never named the deleted bucket; missing = %v", missing.list())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var deps atomic.Pointer[listenerDeps]
	recorder := httptest.NewRecorder()
	healthzHandler(&deps, missing).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health struct {
		Status  string   `json:"status"`
		Missing []string `json:"missing_buckets"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &health)
	if recorder.Code != http.StatusServiceUnavailable || health.Status != "waiting" || !slices.Equal(health.Missing, []string{bus.SessionBucket}) {
		t.Fatalf("/healthz while the open waits = %d %s, want 503 waiting naming %s: the listener serves no route until the bucket exists", recorder.Code, recorder.Body.String(), bus.SessionBucket)
	}
	select {
	case got := <-done:
		t.Fatalf("the store's open returned (%v) while its bucket is missing, want it waiting", got.err)
	case <-time.After(3 * bucketWaitInterval):
	}
	requireBucketMissing(t, js, bus.SessionBucket, "while the open waits")
	if !strings.Contains(logs.String(), missingBucketLine) {
		t.Fatalf("the wait never logged %q:\n%s", missingBucketLine, logs.String())
	}

	// The operator creates it, as `envoy-infra buckets create` does.
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the session bucket again: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the store's open once the bucket is back: %v", got.err)
		}
		t.Cleanup(got.sessions.StopWatch)
		if err := got.sessions.Put("ses_after_wait", session.SessionEntry{MachineID: "test-machine", Title: "after the wait"}); err != nil {
			t.Fatalf("register through the opened store: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the store's open never completed once the bucket was back")
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing after the open = %v, want none", names)
	}
}
