package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/bus/bustest"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// The self-health monitor's tick opens a known-session store whose bucket was missing as soon as a
// read finds the bucket there (checkMonitoredBuckets, session.KnownSessions.BucketPresent). Here
// the store opens on a server without the bucket, on the production retry schedule (a second,
// doubling to five minutes), and the operator creates the bucket 16 s in, when the retry's next
// attempt is 15 s away. The store stays unopened for a 2 s control window, then the tick opens it
// within 3 s. Before the tick told the store, its registrations were refused until that attempt,
// and on a store that had been waiting longer, for up to five minutes, while /healthz and the
// alarm already read the bucket there.
func TestTheMonitorOpensAKnownSessionStoreWhoseBucketCameBack(t *testing.T) {
	t.Parallel()
	_, uri := testnats.Start(t)
	bustest.ProvisionURL(t, uri, 1)
	operator := testnats.Connect(t, uri)
	js, err := operator.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteKeyValue(bus.KnownSessionBucket); err != nil {
		t.Fatalf("delete the known-session bucket: %v", err)
	}
	known, err := session.OpenKnownSessions(t.Context(), testnats.Connect(t, uri), session.WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store: %v", err)
	}
	t.Cleanup(known.StopWatch)
	opened := time.Now()
	if known.Opened() || known.Unreadable() {
		t.Fatalf("control: the store did not find its bucket missing (opened %t, unreadable %t)", known.Opened(), known.Unreadable())
	}

	// The retry's attempts land about 1, 3, 7 and 15 s after the open, and the next one about 31 s.
	time.Sleep(time.Until(opened.Add(16 * time.Second)))
	config, _ := bus.CopiedBucketConfig(bus.KnownSessionBucket, 1)
	if _, err := js.CreateKeyValue(&config); err != nil {
		t.Fatalf("create the bucket, as `envoy-infra buckets` would: %v", err)
	}
	if testwait.Holds(2*time.Second, 50*time.Millisecond, known.Opened) {
		t.Fatalf("control: the store opened the created bucket on its own %s after its open, so this test says nothing about the tick", time.Since(opened).Round(time.Second))
	}

	missing := &missingBuckets{}
	checkMonitoredBuckets(context.Background(), logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{}), js, listenerCopiedBuckets(false), known, missing)
	if !testwait.Holds(3*time.Second, 50*time.Millisecond, known.Opened) {
		t.Fatalf("the store did not open its bucket within 3 s of a tick that read it there (%s after its open)", time.Since(opened).Round(time.Second))
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing = %v after a tick with every bucket there, want none", names)
	}
}
