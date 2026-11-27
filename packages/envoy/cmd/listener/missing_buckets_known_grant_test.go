package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

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
	known, err := session.OpenKnownSessions(t.Context(), storeClient.Conn, session.WithKnownSessionReplicas(1))
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
	refused := func() int { return refusedKnownReads(grant, cid) }
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

// refusedKnownReads counts the reads of the known-session bucket's stream the server refused the
// connection cid on grant's server: each is `Permissions Violation for Publish to
// "$JS.API.STREAM.INFO.KV_envoy_known_sessions"` on that connection's line of the server's trace.
func refusedKnownReads(grant *testnats.NkeyGrant, cid uint64) int {
	count := 0
	for line := range strings.SplitSeq(grant.Log(), "\n") {
		if strings.Contains(line, fmt.Sprintf(" cid:%d ", cid)) && strings.Contains(line, `->> [-ERR Permissions Violation for Publish to "$JS.API.STREAM.INFO.KV_`+bus.KnownSessionBucket+`"]`) {
			count++
		}
	}
	return count
}

// A start on a hub whose users were rendered before the known-session bucket's grant, with every
// other copied bucket there, goes on at once, and never asks for the known-session bucket: the
// start's wait does not read that bucket at all (awaitCopiedBuckets, optionalCopiedBucket), so the
// server refuses it no read. At b20f45e30a the wait read it once, the server refused that read
// (1 here), and the wait then left the bucket to its store; at c024fc6da2 the refused read waited
// out its whole ten-second bound, so such a listener opened /v1 about 12 s after it began
// listening (TestTheListenerOpensV1AndPicksUpTheBucketItMayNotOpenYet: 12090 ms, want within 5000).
func TestAStartGoesOnAtOnceOverAKnownSessionBucketTheServerRefuses(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, ">")
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{grant.URL}, bus.WithName("refused-start"))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	conn, js := client.Connection(), client.JS()
	copied := listenerCopiedBuckets(false)
	for _, bucket := range copied {
		if _, err := bus.CreateCopiedBucket(t.Context(), conn, js, bucket, 1); err != nil {
			t.Fatalf("create %s: %v", bucket, err)
		}
	}
	grant.Grant(t, grantBeforeKnownSessions()...)
	missing := &missingBuckets{}
	began := time.Now()
	if err := awaitCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{}), js, copied, missing); err != nil {
		t.Fatalf("the wait: %v", err)
	}
	within := 2 * time.Second
	if took := time.Since(began); took > within {
		t.Fatalf("the wait took %s over a known-session bucket the server refuses, every other bucket there, want within %s: the refusal ends the read", took.Round(time.Millisecond), within)
	}
	cid, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("read the connection's id: %v", err)
	}
	if n := refusedKnownReads(grant, cid); n != 0 {
		t.Fatalf("the server refused %d reads of KV_%s, want 0: the start's wait does not read the bucket it serves without", n, bus.KnownSessionBucket)
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing = %v, want none: no read found a bucket missing", names)
	}
}

// readOutcome is what a scripted read of one bucket's stream answers.
type readOutcome int

const (
	readPresent readOutcome = iota
	// readSlow is a read the hub answers present after slowRead, or one that ends with the read's
	// own context first, as a real read does.
	readSlow
)

// slowRead is how long a readSlow read takes: past the one-second bound the known-session read
// had before, well inside bucketCheckTimeout.
const slowRead = 2 * time.Second

// scriptedJetStream answers each read of a bucket's stream (StreamInfo, the one call readBuckets
// makes) by the script's outcome for the bucket's nth read, n counted per bucket from 1. Every
// other call panics: the start's wait makes none.
type scriptedJetStream struct {
	natsgo.JetStreamContext
	script func(bucket string, n int) readOutcome
	mu     sync.Mutex
	reads  map[string]int
}

func (s *scriptedJetStream) StreamInfo(stream string, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	bucket := strings.TrimPrefix(stream, "KV_")
	s.mu.Lock()
	s.reads[bucket]++
	outcome := s.script(bucket, s.reads[bucket])
	s.mu.Unlock()
	if outcome == readSlow {
		ctx := context.Background()
		for _, opt := range opts {
			if bounded, ok := opt.(natsgo.ContextOpt); ok {
				ctx = bounded.Context
			}
		}
		select {
		case <-time.After(slowRead):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &natsgo.StreamInfo{Config: natsgo.StreamConfig{Name: stream}}, nil
}

// A hub that is slower than one second on the known-session bucket's read holds no start: the
// start's wait does not read that bucket (awaitCopiedBuckets, optionalCopiedBucket). Here every
// read of that bucket would take slowRead, and the others answer at once. At ee51690e89 that read
// alone was bounded at one second, and since round 4 a failed read with no refusal held the turn,
// so the start was held for good on such a hub; until b20f45e30a the wait read it on every turn.
func TestAStartIsNotHeldByASlowKnownSessionRead(t *testing.T) {
	t.Parallel()
	client, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("slow-known-read"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := &scriptedJetStream{reads: map[string]int{}, script: func(bucket string, _ int) readOutcome {
		if bucket == bus.KnownSessionBucket {
			return readSlow
		}
		return readPresent
	}}
	missing := &missingBuckets{}
	done := make(chan error, 1)
	go func() {
		done <- awaitCopiedBuckets(context.Background(), logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{}), js, listenerCopiedBuckets(false), missing)
	}()
	within := 3*slowRead + 2*bucketWaitInterval
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the wait returned %v with every bucket present, want nil", err)
		}
	case <-time.After(within):
		js.mu.Lock()
		reads := js.reads[bus.KnownSessionBucket]
		js.mu.Unlock()
		t.Fatalf("the wait was still held %s into a start whose every bucket reads present, after %d reads of %s that each took %s: the read's bound is shorter than the hub", within, reads, bus.KnownSessionBucket, slowRead)
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing = %v after a start whose every bucket reads present, want none", names)
	}
	if n := js.reads[bus.KnownSessionBucket]; n != 0 {
		t.Fatalf("the wait read %s %d times, want 0: a start does not wait for the bucket it serves without", bus.KnownSessionBucket, n)
	}
}
