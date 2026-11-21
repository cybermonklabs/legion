package session

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// A store whose open found its bucket missing opens it as soon as the listener says a read found
// the bucket there (BucketPresent), not at its retry's next attempt. The retry's waits here are an
// hour, standing in for the five-minute cap the production retry backs off to: the store stays
// unopened for the whole control window once the bucket is created, and opens within seconds of
// BucketPresent. Before BucketPresent nothing woke such a store but its retry or a connection event,
// so a registration was refused for up to five minutes after /healthz read the bucket there.
func TestAStoreWhoseBucketWasMissingOpensWhenToldItIsThere(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	conn := testnats.Connect(t, uri)
	known, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1), withKnownSessionRetry(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if known.Opened() || !known.bucketMissing.Load() {
		t.Fatalf("control: the store did not find its bucket missing (opened %t, missing %t), so this test says nothing", known.Opened(), known.bucketMissing.Load())
	}
	bustest.ProvisionURL(t, uri, 1)
	if testwait.Holds(2*time.Second, 50*time.Millisecond, known.Opened) {
		t.Fatal("control: the store opened the created bucket on its own, inside its retry's hour, so this test says nothing about BucketPresent")
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_before_present"}); err == nil {
		t.Fatal("control: a record was written before the store opened its bucket")
	}

	known.BucketPresent(context.Background())
	if !testwait.Holds(10*time.Second, 50*time.Millisecond, known.Opened) {
		t.Fatal("the store did not open its bucket within 10 s of BucketPresent, with the bucket there")
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_after_present"}); err != nil {
		t.Fatalf("a record after the store opened its bucket: %v", err)
	}
}

// A store that opened its bucket needs no word from the listener when the bucket is deleted under
// it and created again: its watcher follows the new bucket by itself, with its retry's waits an
// hour so nothing else moves it, and BucketPresent leaves such a store alone. This is why
// BucketPresent wakes only a store that never opened its bucket.
func TestAnOpenStoreFollowsItsBucketCreatedAgainWithNoWord(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	bustest.ProvisionURL(t, uri, 1)
	conn := testnats.Connect(t, uri)
	known, err := OpenKnownSessions(t.Context(), conn, WithKnownSessionReplicas(1), withKnownSessionRetry(time.Hour, time.Hour))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if !testwait.Holds(10*time.Second, 50*time.Millisecond, known.Opened) {
		t.Fatal("the store never opened its provisioned bucket")
	}
	ended := func() bool { w := known.watch(); return w != nil && w.Err() != nil }
	operator := testnats.Connect(t, uri)
	js, err := operator.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.KnownSessionBucket); err != nil {
		t.Fatalf("delete the known-session bucket: %v", err)
	}
	if !testwait.Holds(30*time.Second, 50*time.Millisecond, ended) {
		t.Fatal("the store's watcher never ended after its bucket was deleted")
	}
	config, _ := bus.CopiedBucketConfig(bus.KnownSessionBucket, 1)
	kv, err := js.CreateKeyValue(&config)
	if err != nil {
		t.Fatalf("create the bucket, as `envoy-infra buckets` would: %v", err)
	}
	if !testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { return !ended() }) {
		t.Fatal("the open store's watcher did not follow its bucket created again, with no word from the listener")
	}
	record := `{"session_id":"ses_written_elsewhere","last_seen":` + strconv.FormatInt(time.Now().UnixMilli(), 10) + `}`
	if _, err := kv.Put("ses_written_elsewhere", []byte(record)); err != nil {
		t.Fatalf("write a record through another connection: %v", err)
	}
	if !testwait.Holds(10*time.Second, 50*time.Millisecond, func() bool { _, ok := known.cached("ses_written_elsewhere"); return ok }) {
		t.Fatal("the store's copy did not take a record written to the bucket created again")
	}
	if err := known.RecordWithin(context.Background(), KnownSession{SessionID: "ses_own_write"}); err != nil {
		t.Fatalf("the store's own write to the bucket created again: %v", err)
	}
}
