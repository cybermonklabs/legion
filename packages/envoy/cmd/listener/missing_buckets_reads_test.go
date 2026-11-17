package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bustest"
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

	checkCopiedBuckets(logger, js, buckets, missing)
	if got := missing.list(); !slices.Equal(got, []string{bus.SessionBucket}) {
		t.Fatalf("after a read with the session bucket deleted, missing = %v, want [%s]", got, bus.SessionBucket)
	}

	closed := bustest.Connect(t, uri)
	closedJS, err := closed.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	closed.Close()
	if reads := readBuckets(closedJS, buckets); len(reads.unread) != len(buckets) {
		t.Fatalf("control: through a closed connection %d of %d reads failed, want every one", len(reads.unread), len(buckets))
	}
	logs = &cmdtest.LockedBuffer{}
	checkCopiedBuckets(logging.NewWithWriter("test-machine", logs), closedJS, buckets, missing)
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
	checkCopiedBuckets(logging.NewWithWriter("test-machine", logs), js, buckets, missing)
	if got := missing.list(); len(got) != 0 {
		t.Fatalf("after a read found the bucket, missing = %v, want none", got)
	}
	if strings.Contains(logs.String(), missingBucketLine) {
		t.Fatalf("the tick that found every bucket logged %q:\n%s", missingBucketLine, logs.String())
	}
}
