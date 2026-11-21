package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A bucket the monitor found missing stays missing until a read finds it present: a read that
// fails - here every read, through a closed connection - says nothing of it, so the set, the
// envoy_kv_buckets_missing gauge it feeds and the ERROR line the alarm counts all hold. Once a read
// finds the bucket again, it leaves the set. At 7b262c2e0a the tick replaced the set with the
// buckets its read found missing, so a failed read emptied it and stopped the line.
func TestABucketStaysMissingThroughAFailedReadUntilAReadFindsIt(t *testing.T) {
	t.Parallel()
	uri := testnats.URL(t)
	conn := bustest.Connect(t, uri)
	if err := bustest.Provision(conn, 1); err != nil {
		t.Fatalf("provision: %v", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.SessionBucket); err != nil {
		t.Fatalf("delete the session bucket: %v", err)
	}
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test-machine", logs)
	missing := &missingBuckets{}
	buckets := listenerCopiedBuckets(false)

	checkCopiedBuckets(context.Background(), logger, js, buckets, missing)
	if got := missing.list(); !slices.Equal(got, []string{bus.SessionBucket}) {
		t.Fatalf("after a read with the session bucket deleted, missing = %v, want [%s]", got, bus.SessionBucket)
	}

	closed := bustest.Connect(t, uri)
	closedJS, err := closed.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	closed.Close()
	if reads := readBuckets(context.Background(), closedJS, buckets); len(reads.unread) != len(buckets) {
		t.Fatalf("control: through a closed connection %d of %d reads failed, want every one", len(reads.unread), len(buckets))
	}
	logs = &cmdtest.LockedBuffer{}
	checkCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", logs), closedJS, buckets, missing)
	if got := missing.list(); !slices.Equal(got, []string{bus.SessionBucket}) {
		t.Fatalf("after a read that failed, missing = %v, want [%s] still", got, bus.SessionBucket)
	}
	if !strings.Contains(logs.String(), missingBucketLine) || !strings.Contains(logs.String(), bus.SessionBucket) {
		t.Fatalf("the tick whose read failed did not log %q for %s:\n%s", missingBucketLine, bus.SessionBucket, logs.String())
	}

	config, _ := bus.CopiedBucketConfig(bus.SessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the session bucket again: %v", err)
	}
	logs = &cmdtest.LockedBuffer{}
	checkCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", logs), js, buckets, missing)
	if got := missing.list(); len(got) != 0 {
		t.Fatalf("after a read found the bucket, missing = %v, want none", got)
	}
	if strings.Contains(logs.String(), missingBucketLine) {
		t.Fatalf("the tick that found every bucket logged %q:\n%s", missingBucketLine, logs.String())
	}
}

// A tick's reads of the copied buckets are asked together and end with the context they are
// given. Here the JetStream API answers nothing: its requests go to a subscriber that takes them
// and never replies. Every bucket's read must arrive well inside one read's timeout, and cancelling
// the context, as the shutdown's monitorCancel does, must end them all at once, with no WARN and
// no change to what the listener holds missing. At b71de8a4d0 the reads ran one after another on
// context.Background, bucketCheckTimeout (10 s) each, so a stalled hub held one tick for about
// 41 s and spent the whole 10 s stop budget in the shutdown's wait for the monitor.
func TestTheBucketReadsRunTogetherAndEndWithTheirContext(t *testing.T) {
	t.Parallel()
	conn := bustest.Connect(t, testnats.URL(t))
	asked := make(chan string, 64)
	if _, err := conn.Subscribe("stalled.>", func(msg *natsgo.Msg) { asked <- msg.Subject }); err != nil {
		t.Fatalf("subscribe the silent API: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	js, err := conn.JetStream(natsgo.APIPrefix("stalled"))
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	buckets := listenerCopiedBuckets(true)
	missing := &missingBuckets{}
	missing.observe(bucketReads{missing: []string{bus.SessionBucket}})
	logs := &cmdtest.LockedBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checkCopiedBuckets(ctx, logging.NewWithWriter("test-machine", logs), js, buckets, missing)
	}()

	var got []string
	window := time.After(bucketCheckTimeout / 2)
	for len(got) < len(buckets) {
		select {
		case subject := <-asked:
			got = append(got, subject)
		case <-window:
			cancel()
			t.Fatalf("within %s of the tick %d of %d bucket reads were asked (%v), want every one asked together", bucketCheckTimeout/2, len(got), len(buckets), got)
		}
	}
	for _, bucket := range buckets {
		if !slices.Contains(got, "stalled.STREAM.INFO.KV_"+bucket) {
			t.Fatalf("the tick asked %v, want a read of KV_%s among them", got, bucket)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the tick's reads were still running 2 s after their context was cancelled, want them ended with it")
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("a tick its stop cut logged a WARN:\n%s", logs.String())
	}
	if names := missing.list(); !slices.Equal(names, []string{bus.SessionBucket}) {
		t.Fatalf("after a tick its stop cut, missing = %v, want [%s] as it was", names, bus.SessionBucket)
	}
}
