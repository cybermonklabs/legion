package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// On a hub whose users were rendered before the known-session bucket's grant, the self-health
// monitor leaves that bucket out of its tick while the store cannot read it (monitoredBuckets):
// the server refuses and drops the read, so the tick could learn nothing from it, and each tick
// would cost the server's permissions violation, the connection's `envoy nats async error` ERROR
// and checkCopiedBuckets' WARN for a failed read - every 30 s, ten times the store's own retry. At
// 66dc40e22d each of three ticks made one refused read and logged one WARN. The second half is the
// control that the instrument can see: once the grant arrives the store's next attempt finds the
// bucket (missing here, since the user may not create it), and the tick reads it again and names
// it.
func TestTheMonitorLeavesTheKnownSessionBucketToAStoreThatCannotReadIt(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, grantBeforeKnownSessions()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	storeClient, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1), bus.WithName("known-store"))
	if err != nil {
		t.Fatalf("connect the store's user: %v", err)
	}
	t.Cleanup(storeClient.Close)
	known, err := session.OpenKnownSessions(storeClient.Conn, session.WithKnownSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the known-session store under a grant that does not carry its bucket: %v", err)
	}
	t.Cleanup(known.StopWatch)
	if !known.Unreadable() {
		t.Fatal("the store does not report its bucket unreadable under a grant that does not carry it, so this test says nothing about that hub")
	}
	// The monitor's reads go on a connection of their own, so the server's log tells them from the
	// store's own attempts by the connection's id.
	monitorClient, err := bus.ConnectOwningStream([]string{grant.URL}, bus.WithReplicas(1), bus.WithName("monitor"))
	if err != nil {
		t.Fatalf("connect the monitor's user: %v", err)
	}
	t.Cleanup(monitorClient.Close)
	cid, err := monitorClient.Conn.GetClientID()
	if err != nil {
		t.Fatalf("read the monitor connection's id: %v", err)
	}
	refused := func() int {
		count := 0
		for line := range strings.SplitSeq(grant.Log(), "\n") {
			if strings.Contains(line, fmt.Sprintf(" cid:%d ", cid)) && strings.Contains(line, `->> [-ERR Permissions Violation for Publish to "$JS.API.STREAM.INFO.KV_`+bus.KnownSessionBucket+`"]`) {
				count++
			}
		}
		return count
	}
	logs := &cmdtest.LockedBuffer{}
	logger := logging.NewWithWriter("test-machine", logs)
	copied := listenerCopiedBuckets(false)
	missing := &missingBuckets{}
	tick := func() {
		checkCopiedBuckets(context.Background(), logger, monitorClient.JS(), monitoredBuckets(copied, known), missing)
	}

	for range 3 {
		tick()
	}
	if n := refused(); n != 0 {
		t.Errorf("three ticks sent %d reads of KV_%s the server refused, want 0 while the store cannot read it", n, bus.KnownSessionBucket)
	}
	if n := strings.Count(logs.String(), "listener could not read whether its KV buckets exist"); n != 0 {
		t.Errorf("three ticks logged %d WARNs of a failed read, want 0:\n%s", n, logs.String())
	}
	if slices.Contains(missing.list(), bus.KnownSessionBucket) {
		t.Errorf("missing = %v names %s, which no read found missing", missing.list(), bus.KnownSessionBucket)
	}

	grant.Grant(t, append(grantBeforeKnownSessions(), bucketGrant(bus.KnownSessionBucket)...)...)
	// A connection event wakes the store's retry at once (Rewatch on a store whose bucket is not
	// open), as the listener's reconnect hook does after a reload.
	_ = known.Rewatch(context.Background(), storeClient.Conn)
	deadline := time.Now().Add(15 * time.Second)
	for !slices.Contains(missing.list(), bus.KnownSessionBucket) {
		if time.Now().After(deadline) {
			t.Fatalf("15 s after the grant arrived the tick still names %v, want %s among them: the store found the bucket missing and the tick never read it (Unreadable %t)", missing.list(), bus.KnownSessionBucket, known.Unreadable())
		}
		time.Sleep(100 * time.Millisecond)
		tick()
	}
	if n := refused(); n != 0 {
		t.Errorf("the ticks sent %d reads of KV_%s the server refused, want 0", n, bus.KnownSessionBucket)
	}
}
