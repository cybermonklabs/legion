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
	"github.com/sjawhar/envoy/internal/store"
)

// With only the role bucket missing, the interest watcher is still checked and rewatched: the
// registry's two watchers are two caches, each over its one bucket (listenerCaches). Here the
// interest watcher is dead (its stream deleted and its bucket created again, which leaves the
// watcher ended, as in TestSelfHealthRebuildsAnInterestWatcherThatEndedWhileConnected), and then
// the role bucket is deleted. /healthz answers 503 naming the interest KV watcher, and a rewatch
// brings that watcher back, after which the copy takes an interest written to its bucket. At
// e788050f6f the registry was one cache reading both buckets and skipped whole while the role
// bucket was missing: /healthz answered 200 degraded over the dead watcher, and no rewatch reached
// it, so after a reconnect the interest copy of a bucket that exists stopped following.
func TestAMissingRoleBucketLeavesTheInterestWatcherInEveryCheck(t *testing.T) {
	client := setupTestNATS(t)
	const consumer = "listener-split-caches"
	bindBoundDurable(t, client, consumer, func(msg *natsgo.Msg) { _ = msg.Ack() })
	registry, err := store.Open(t.Context(), client.Conn, store.WithReplicas(1))
	if err != nil {
		t.Fatalf("open interest registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := registry.WaitForCacheReady(readyCtx); err != nil {
		t.Fatalf("wait for interest cache: %v", err)
	}
	// The interest watcher ends first, alone, so the wait reads its end and not the role
	// watcher's.
	endInterestWatcherWhileConnected(t, client, registry)
	interestConfig, _ := bus.CopiedBucketConfig(bus.InterestBucket, 1)
	if _, err := client.JS().CreateKeyValue(&interestConfig); err != nil {
		t.Fatalf("create the interest bucket again: %v", err)
	}
	if err := client.JS().DeleteKeyValue(bus.RoleClaimBucket); err != nil {
		t.Fatalf("delete the role bucket: %v", err)
	}
	missing := &missingBuckets{}
	checkCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{}), client.JS(), []string{bus.InterestBucket, bus.RoleClaimBucket}, missing)
	if names := missing.list(); len(names) != 1 || names[0] != bus.RoleClaimBucket {
		t.Fatalf("control: missing = %v, want [%s] alone", names, bus.RoleClaimBucket)
	}

	caches := listenerCaches(registry, nil, nil, missing)
	caches = caches[:len(caches)-1] // no session store in this test
	var state atomic.Pointer[listenerDeps]
	state.Store(&listenerDeps{client: client, consumer: consumer, caches: caches})
	recorder := httptest.NewRecorder()
	healthzHandler(&state, missing).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &body)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(body["error"].(string), "interest KV watcher") {
		t.Fatalf("/healthz with the interest watcher dead and only the role bucket missing = %d %v, want 503 naming the interest KV watcher", recorder.Code, body)
	}

	if err := rewatchListenerKVWatchers(context.Background(), client.Conn, caches); err != nil {
		t.Fatalf("rewatch with only the role bucket missing: %v", err)
	}
	interests, err := client.JS().KeyValue(bus.InterestBucket)
	if err != nil {
		t.Fatalf("open the interest bucket: %v", err)
	}
	row, err := json.Marshal(store.Interest{SessionID: "ses_after_split", MachineID: "m1", Topics: []string{"notifications.split"}, UpdatedAt: time.Now().UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := interests.Put("ses_after_split", row); err != nil {
		t.Fatalf("write an interest after the rewatch: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(registry.Match("m1", "notifications.split")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the interest copy never took an interest written after a rewatch with only the role bucket missing")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
