package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A conditional write that lost to a write of its key still being replicated, on a bucket of three
// copies, is the store's own answer, as one that lost to a write already stored is: the bucket
// answers it 10164, which nats.ErrKeyExists does not match, where a bucket of one copy answers
// 10071, which it does. Before, a claim or a delete that lost such a race and reached a write
// route's error was answered as a bus that did not answer, a 503 with Retry-After: 1.
func TestALostConditionalWriteOnAReplicatedBucketIsTheStoresAnswer(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	js, err := conn.JetStream(nats.MaxWait(bus.RequestTimeout))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	const bucket = "envoy_listener_replicated_conflict_test"
	kv, err := bus.EnsureKeyValue(t.Context(), conn, js, &nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage, Replicas: 3})
	if err != nil {
		t.Fatalf("create the three-copy bucket: %v", err)
	}
	cluster.MoveStreamLeader(t, "KV_"+bucket, 0)
	revision, err := kv.Put("role", []byte("claim"))
	if err != nil {
		t.Fatalf("put the record: %v", err)
	}
	race := cluster.RaceKeyValue(t, kv.Legacy(), 1)
	_, lost := race.Update("role", []byte("another claim"), revision)
	if race.Lost() != 1 {
		t.Fatalf("%d writes lost the race, want 1", race.Lost())
	}

	err = fmt.Errorf("write role role's claim: %w", lost)
	if !storeAnswered(context.Background(), err) {
		t.Fatalf("storeAnswered(%q) = false, want the store's answer", err)
	}
	recorder := httptest.NewRecorder()
	writeStoreError(context.Background(), recorder, err, http.StatusInternalServerError)
	if code, retry := recorder.Code, recorder.Header().Get("Retry-After"); code != http.StatusInternalServerError || retry != "" {
		t.Fatalf("the store's answer %q was answered %d with Retry-After %q, want the route's own 500", err, code, retry)
	}
}
