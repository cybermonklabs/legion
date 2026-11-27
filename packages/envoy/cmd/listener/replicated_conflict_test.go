package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A conditional write that lost to a write of its key still being replicated, on a bucket of three
// copies, is the store's own answer, as one that lost to a write already stored is: the bucket
// answers it 10164, which nats.ErrKeyExists does not match, where a bucket of one copy answers
// 10071, which it does. Before, a claim or a delete that lost such a race and reached a write
// route's error was answered as a bus that did not answer, a 503 with Retry-After: 1.
func TestALostConditionalWriteOnAReplicatedBucketIsTheStoresAnswer(t *testing.T) {
	t.Parallel()
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

// leaderReadListenerState opens the listener state bucket with three copies on cluster, on a
// connection to member 0, takes direct gets off its stream, has member 0 lead it, and returns a
// handle on the bucket opened after that, whose reads go to that leader. A Create whose first write
// is refused reads its key back to tell a delete marker from a value, and while a raced write is
// held the cluster's routes are frozen (testnats.Cluster.HoldReplicatingWrite): a direct get may be
// handed to another copy across a frozen route and wait out its JetStream wait, and the Create then
// answers its first write's refusal rather than its write over the marker. The bus package's
// TestACreateOverADeleteMarkerThatLosesToAReplicatingWriteIsACASConflict does the same.
func leaderReadListenerState(t *testing.T, cluster *testnats.Cluster) bus.KeyValue {
	t.Helper()
	conn := testnats.Connect(t, cluster.Servers[0].URL())
	t.Cleanup(conn.Close)
	opened, err := openListenerState(t.Context(), conn, 3, listenerStateRequestWait)
	if err != nil {
		t.Fatalf("open the listener state bucket with three copies: %v", err)
	}
	js, err := conn.JetStream(nats.MaxWait(listenerStateRequestWait))
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	stream := "KV_" + bus.ListenerStateBucket
	info, err := js.StreamInfo(stream)
	if err != nil {
		t.Fatalf("read the bucket's stream: %v", err)
	}
	undirected := info.Config
	undirected.AllowDirect = false
	testnats.UpdateStream(t, js, undirected)
	cluster.MoveStreamLeader(t, stream, 0)
	legacy, err := js.KeyValue(bus.ListenerStateBucket)
	if err != nil {
		t.Fatalf("open the bucket with no direct gets: %v", err)
	}
	return bus.NewKeyValue(legacy, opened.WatchHandle(), opened.Streams())
}

// A standby's take of a deleted lease record that loses to a write of the key still being
// replicated, on a bucket of three copies, is a lost race, as one that loses to a write already
// stored is. A read answers no record for a key whose last message is a delete marker, so the take
// is a Create, and nats.go's Create sends its write again at the marker's revision and returns that
// write's 10164 as it is, which nats.ErrKeyExists does not match. The standby reads the record again
// at once and notes no failure, and its next take holds the lease. Before, the lost take was noted
// as a failed bucket call and the standby waited a whole poll.
// It must not run in parallel: routes frozen past the 4 s election timeout let a follower campaign.
func TestALeaseTakeOverADeletedRecordThatLosesToAReplicatingWriteIsALostRace(t *testing.T) {
	cluster := testnats.StartCluster(t, 3)
	kv := leaderReadListenerState(t, cluster)
	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677"}`)
	if err := kv.Delete(leaseTestKey); err != nil {
		t.Fatalf("delete the lease record: %v", err)
	}
	cluster.AwaitReplicated(t, "KV_"+bus.ListenerStateBucket)
	race := cluster.RaceKeyValue(t, kv.Legacy(), 1)
	var logs lockedBuffer
	a := newLease(bus.NewKeyValue(race, kv.WatchHandle(), kv.Streams()), leaseTestMachine, "pod-a", productionLeaseConfig(), newFakeLeaseClock().now, logging.NewWithWriter(leaseTestMachine, &logs))

	if wait := a.step(); wait != 0 {
		t.Fatalf("step after a take that lost the race waits %s, want 0: read the record again at once; the log is:\n%s", wait, logs.String())
	}
	if race.Lost() != 1 {
		t.Fatalf("%d takes lost the race, want 1", race.Lost())
	}
	if status := a.Status(); status.Holding || status.Term != 0 {
		t.Fatalf("status = %+v, want not holding, no term", status)
	}
	if a.failing || strings.Contains(logs.String(), "listener lease state unavailable") {
		t.Fatalf("the lost take was noted as a failed bucket call; the log is:\n%s", logs.String())
	}

	if wait := a.step(); wait != leaseRenewEvery {
		t.Fatalf("step after the race was spent waits %s, want the renewal interval %s; the log is:\n%s", wait, leaseRenewEvery, logs.String())
	}
	if status := a.Status(); !status.Holding || status.Term != 1 || status.How != leaseTakenAbsent {
		t.Fatalf("status = %+v, want holding term 1 taken from an absent record", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-a" {
		t.Fatalf("record = %+v, want pod-a's", record.leaseFields)
	}
}
