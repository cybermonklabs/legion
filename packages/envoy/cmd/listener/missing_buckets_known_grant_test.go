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

// On a hub whose users were rendered before the known-session bucket's grant, a start that another
// missing bucket holds at its gates reads the known-session bucket once: the listener's JetStream
// context returns the refusal as bus.ErrPublishDenied, and the wait leaves the bucket to its store
// from then on (awaitCopiedBuckets). Each read the server refuses is a permissions violation and
// the connection's `envoy nats async error` ERROR, so a wait that read it on every turn logged one
// each turn for as long as the start was held: at 91c75f9e8d this test's wait made 3 refused reads
// in 9 s. The wait is still the start's: it holds while the other buckets are missing, and returns
// once they exist.
func TestAHeldStartReadsAnUnreadableKnownSessionBucketOnce(t *testing.T) {
	seed, public := testnats.User(t)
	grant := testnats.StartNkeyGranted(t, public, grantBeforeKnownSessions()...)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{grant.URL}, bus.WithName("held-start"))
	if err != nil {
		t.Fatalf("connect the listener's user: %v", err)
	}
	t.Cleanup(client.Close)
	conn, js := client.Connection(), client.JS()
	cid, err := conn.GetClientID()
	if err != nil {
		t.Fatalf("read the connection's id: %v", err)
	}
	copied := listenerCopiedBuckets(false)
	if !slices.Contains(copied, bus.KnownSessionBucket) {
		t.Fatalf("the listener's copied buckets %v do not name %s", copied, bus.KnownSessionBucket)
	}
	missing := &missingBuckets{}
	logger := logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- awaitCopiedBuckets(ctx, logger, js, copied, missing) }()

	// Four turns of the wait, with every bucket but the known-session one missing.
	held := 4*bucketWaitInterval + time.Second
	select {
	case err := <-done:
		t.Fatalf("the wait returned %v with %v missing, want it held", err, missing.list())
	case <-time.After(held):
	}
	if n := refusedKnownReads(grant, cid); n > 1 {
		t.Fatalf("the wait made %d reads of KV_%s the server refused in %s, want at most 1: the first read tells it the bucket cannot be read", n, bus.KnownSessionBucket, held)
	} else if n == 0 {
		t.Fatalf("the wait made no read of KV_%s the server refused, so this test says nothing about an unreadable bucket", bus.KnownSessionBucket)
	}
	if names := missing.list(); !slices.Contains(names, bus.SessionBucket) || slices.Contains(names, bus.KnownSessionBucket) {
		t.Fatalf("the held wait holds %v missing, want %s among them and not %s, which no read found missing", names, bus.SessionBucket, bus.KnownSessionBucket)
	}

	// The operator creates the missing buckets: the wait returns, with no further read of the
	// known-session bucket.
	for _, bucket := range copied {
		if bucket == bus.KnownSessionBucket {
			continue
		}
		if _, err := bus.CreateCopiedBucket(t.Context(), conn, js, bucket, 1); err != nil {
			t.Fatalf("create %s: %v", bucket, err)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the wait returned %v once the buckets existed, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the wait did not return within 30 s of the buckets' creation; missing = %v", missing.list())
	}
	if n := refusedKnownReads(grant, cid); n != 1 {
		t.Fatalf("over the whole wait the server refused %d reads of KV_%s, want 1", n, bus.KnownSessionBucket)
	}
}

// A start on a hub whose users were rendered before the known-session bucket's grant, with every
// other copied bucket there, goes on at once: the listener's JetStream context returns the
// known-session refusal as bus.ErrPublishDenied, rather than waiting for bucketCheckTimeout, and
// the wait then holds nothing for that bucket. At c024fc6da2 the refused read waited out its whole
// ten-second bound, so such a listener opened /v1 about 12 s after it began listening, every
// caller refused 503 meanwhile (TestTheListenerOpensV1AndPicksUpTheBucketItMayNotOpenYet:
// 12090 ms, want within 5000).
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
	if n := refusedKnownReads(grant, cid); n != 1 {
		t.Fatalf("the server refused %d reads of KV_%s, want 1: the test says nothing about a refused read otherwise", n, bus.KnownSessionBucket)
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing = %v, want none: no read found a bucket missing", names)
	}
}

// A known-session bucket the start's wait could not read because the hub answered nothing is read
// again, and a missing one then holds the start: the wait leaves the bucket to its store only
// after the listener's JetStream context returned bus.ErrPublishDenied
// (TestAHeldStartReadsAnUnreadableKnownSessionBucketOnce), and a hub that does not answer returns
// no such error. Here the hub holds every copied bucket but the known-session one, and answers
// nothing for the wait's whole first turn, so every read of that turn fails; then it answers, a
// turn finds the known-session bucket missing, and the start is held until the operator creates it.
// Whether the thaw lands before the second turn's reads or after them (the known-session read then
// waits out its bound while the others are answered), a read that failed holds its turn, so the
// outcome is the same either way; the order is forced, each way, in
// TestAFailedKnownSessionReadHoldsTheStartsTurn. At 3dc1ad81d8 the first failed read dropped the
// bucket whatever its cause, and the second turn, finding the others present, let the start go
// with the bucket missing.
func TestAStartTheHubDidNotAnswerAtFirstIsHeldForAMissingKnownSessionBucket(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri}, bus.WithName("unanswered-start"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	conn, js := client.Connection(), client.JS()
	copied := listenerCopiedBuckets(false)
	for _, bucket := range copied {
		if bucket == bus.KnownSessionBucket {
			continue
		}
		if _, err := bus.CreateCopiedBucket(t.Context(), conn, js, bucket, 1); err != nil {
			t.Fatalf("create %s: %v", bucket, err)
		}
	}
	missing := &missingBuckets{}
	logger := logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	thaw := srv.Freeze(t)
	thawed := false
	defer func() {
		if !thawed {
			thaw()
		}
	}()
	done := make(chan error, 1)
	go func() { done <- awaitCopiedBuckets(ctx, logger, js, copied, missing) }()
	// The first turn's reads wait out bucketCheckTimeout; the hub answers none of them.
	select {
	case err := <-done:
		t.Fatalf("the wait returned %v while the hub answered nothing", err)
	case <-time.After(bucketCheckTimeout + 2*time.Second):
	}
	if names := missing.list(); len(names) != 0 {
		t.Fatalf("missing = %v after a turn the hub answered nothing of, want none: the first turn must learn nothing", names)
	}
	thaw()
	thawed = true

	// The hub answers again: the next turn finds the known-session bucket missing and holds.
	deadline := time.Now().Add(3 * (bucketCheckTimeout + bucketWaitInterval))
	for !slices.Contains(missing.list(), bus.KnownSessionBucket) {
		select {
		case err := <-done:
			t.Fatalf("the wait returned %v once the hub answered, with %s missing, want it held", err, bus.KnownSessionBucket)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the wait did not find %s missing within %s of the hub answering; missing = %v", bus.KnownSessionBucket, 3*(bucketCheckTimeout+bucketWaitInterval), missing.list())
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("the wait returned %v with %s missing, want it held", err, bus.KnownSessionBucket)
	case <-time.After(2 * (bucketCheckTimeout + bucketWaitInterval)):
	}

	if _, err := bus.CreateCopiedBucket(t.Context(), conn, js, bus.KnownSessionBucket, 1); err != nil {
		t.Fatalf("create %s: %v", bus.KnownSessionBucket, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the wait returned %v once the bucket existed, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the wait did not return within 30 s of the bucket's creation; missing = %v", missing.list())
	}
}

// readOutcome is what a scripted read of one bucket's stream answers.
type readOutcome int

const (
	readPresent readOutcome = iota
	readMissing
	// readUnanswered is a read the hub did not answer in time, which no refusal explains: a hub
	// that answers nothing, or one slower than the read's bound.
	readUnanswered
	// readSlow is a read the hub answers present after slowRead, or one that ends with the read's
	// own context first, as a real read does.
	readSlow
)

// slowRead is how long a readSlow read takes: past the one-second bound the known-session read
// had before, well inside bucketCheckTimeout.
const slowRead = 2 * time.Second

// scriptedJetStream answers each read of a bucket's stream (StreamInfo, the one call readBuckets
// makes) by the script's outcome for the bucket's nth read, n counted per bucket from 1, and for a
// bucket created (create) present from then on. Every other call panics: the start's wait makes
// none.
type scriptedJetStream struct {
	natsgo.JetStreamContext
	script  func(bucket string, n int) readOutcome
	mu      sync.Mutex
	reads   map[string]int
	created map[string]bool
}

func (s *scriptedJetStream) StreamInfo(stream string, opts ...natsgo.JSOpt) (*natsgo.StreamInfo, error) {
	bucket := strings.TrimPrefix(stream, "KV_")
	s.mu.Lock()
	s.reads[bucket]++
	outcome := s.script(bucket, s.reads[bucket])
	if s.created[bucket] {
		outcome = readPresent
	}
	s.mu.Unlock()
	switch outcome {
	case readMissing:
		return nil, natsgo.ErrStreamNotFound
	case readUnanswered:
		return nil, natsgo.ErrTimeout
	case readSlow:
		ctx := context.Background()
		for _, opt := range opts {
			if bounded, ok := opt.(natsgo.ContextOpt); ok {
				ctx = bounded.Context
			}
		}
		select {
		case <-time.After(slowRead):
			return &natsgo.StreamInfo{Config: natsgo.StreamConfig{Name: stream}}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
		return &natsgo.StreamInfo{Config: natsgo.StreamConfig{Name: stream}}, nil
	}
}

func (s *scriptedJetStream) create(bucket string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created[bucket] = true
}

// A hub that is always slower than one second on the known-session bucket's read lets a start go
// once every bucket reads present: every read takes the same bound, bucketCheckTimeout. Here every
// read of that bucket takes slowRead and answers present, and the others answer at once. At
// ee51690e89 that read alone was bounded at one second, and since round 4 a failed read with no
// refusal holds the turn, so the start was held for good on such a hub.
func TestAStartIsNotHeldByASlowKnownSessionRead(t *testing.T) {
	t.Parallel()
	client, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("slow-known-read"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	js := &scriptedJetStream{reads: map[string]int{}, created: map[string]bool{}, script: func(bucket string, _ int) readOutcome {
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
}

// A read of the known-session bucket that fails with no refusal from the server holds the start's
// turn, as a failed read of any other bucket does, so the start never goes on with that bucket
// missing because a read of it was slow. Each case forces the order the real hub only sometimes
// gives (TestAStartTheHubDidNotAnswerAtFirstIsHeldForAMissingKnownSessionBucket): its reads are
// scripted, per bucket and turn, and none is refused. The known-session bucket is missing
// throughout until the test creates it, and the wait must not return before that.
//
// At eb20fc252c a failed read of that bucket held nothing whatever its cause, so the case where it
// waits out its bound while the hub answers the others (the late thaw a review injected into the
// real-hub test) returned at that turn with the bucket missing; at 3dc1ad81d8 the first failed read
// dropped the bucket for good, so every case returned at the first turn the others read present.
func TestAFailedKnownSessionReadHoldsTheStartsTurn(t *testing.T) {
	t.Parallel()
	client, err := bus.Connect([]string{testnats.URL(t)}, bus.WithName("scripted-start"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	copied := listenerCopiedBuckets(false)
	for _, c := range []struct {
		name string
		// known is the outcome of the known-session bucket's nth read; every other bucket reads
		// unanswered at its first read when hubSilentAtFirst, and present after.
		known            func(n int) readOutcome
		hubSilentAtFirst bool
	}{
		{
			name:  "the known-session read is slow from the first turn while the others are answered",
			known: func(n int) readOutcome { return map[bool]readOutcome{true: readUnanswered, false: readMissing}[n == 1] },
		},
		{
			name:             "the hub answers nothing at first, then the known-session read is slow while the others are answered",
			known:            func(n int) readOutcome { return map[bool]readOutcome{true: readUnanswered, false: readMissing}[n <= 2] },
			hubSilentAtFirst: true,
		},
		{
			name:             "the hub answers nothing at first, then finds the known-session bucket missing",
			known:            func(n int) readOutcome { return map[bool]readOutcome{true: readUnanswered, false: readMissing}[n == 1] },
			hubSilentAtFirst: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			js := &scriptedJetStream{reads: map[string]int{}, created: map[string]bool{}, script: func(bucket string, n int) readOutcome {
				if bucket == bus.KnownSessionBucket {
					return c.known(n)
				}
				if c.hubSilentAtFirst && n == 1 {
					return readUnanswered
				}
				return readPresent
			}}
			missing := &missingBuckets{}
			logger := logging.NewWithWriter("test-machine", &cmdtest.LockedBuffer{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- awaitCopiedBuckets(ctx, logger, js, copied, missing) }()

			// Every scripted turn but the last, then one turn that reads the bucket missing.
			turns := 3
			deadline := time.Now().Add(time.Duration(turns+2) * (bucketWaitInterval + time.Second))
			for {
				js.mu.Lock()
				n := js.reads[bus.KnownSessionBucket]
				js.mu.Unlock()
				select {
				case err := <-done:
					t.Fatalf("the wait returned %v after %d reads of %s, which never read present, want it held", err, n, bus.KnownSessionBucket)
				default:
				}
				if n >= turns && slices.Contains(missing.list(), bus.KnownSessionBucket) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("after %d reads of %s, missing = %v; want %d reads and the bucket named missing", n, bus.KnownSessionBucket, missing.list(), turns)
				}
				time.Sleep(10 * time.Millisecond)
			}

			js.create(bus.KnownSessionBucket)
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the wait returned %v once the bucket existed, want nil", err)
				}
			case <-time.After(2 * (bucketWaitInterval + time.Second)):
				t.Fatalf("the wait did not return within two turns of the bucket's creation; missing = %v", missing.list())
			}
		})
	}
}
