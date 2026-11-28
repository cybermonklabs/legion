package main

import (
	"context"
	"slices"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testwait"
)

// slowReadKV is a bucket each read of which answers after delay on clock, the clock the role
// lane's read bounds run on: a bus that is slow, not silent. A read waits until the test moves
// clock delay past the moment the read began, or until ctx, the test's, ends.
type slowReadKV struct {
	natsgo.KeyValue
	ctx   context.Context
	clock *clocktest.Fake
	delay time.Duration
}

func (kv slowReadKV) Get(key string) (natsgo.KeyValueEntry, error) {
	_ = kv.clock.Sleep(kv.ctx, kv.delay)
	return kv.KeyValue.Get(key)
}

// The role lane bounds its claim read and its holder's session read each on its own
// (roleLaneClaims, roleHolderSessions.Holder), not the two under one bound: a claim read that
// answers late inside its bound leaves the session read the whole of its own. Under one shared
// bound the session read had only what the claim read left of it, failed, and the lane reported
// delivery_failed with the message neither forwarded nor kept. Here the claim read answers after
// three quarters of roleHolderReadTimeout and the session read, which goes to the bucket because
// the session cache does not hold the holder, after half of it: each inside its own bound, the
// two together past one. The message must reach the holder, with no delivery exception and
// nothing kept.
//
// Both bounds and both slow buckets run on the lane's clock (cfg.clock), a fake the test moves:
// it moves the fake to each bucket's answer only once that bucket and the bound of its read are
// both armed, so each read is seen to reach its slow bucket under a bound armed when the read
// began. A session read under one bound with the claim read would find that bound armed at the
// claim read's start, which the move to the session bucket's answer passes, and would fail.
func TestTheRoleLaneBoundsItsClaimReadAndItsHoldersSessionReadEachOnItsOwn(t *testing.T) {
	t.Parallel()
	const (
		role   = "slow-reads"
		holder = "ses_slow_reads"
	)
	claimDelay, sessionDelay := roleHolderReadTimeout*3/4, roleHolderReadTimeout/2
	start := time.Now()
	clk := clocktest.New(start)
	harness := newListenerDeliveryHarness(t)
	// The cache stops following the bucket before the holder registers, so the resolution finds
	// no live record of the holder in it and reads the session bucket.
	harness.sessions.StopWatch()
	storeSessionHeartbeat(t, harness.client, holder, time.Now())
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role %s for %s: %v", role, holder, err)
	}
	setRegistryRoleKV(t, harness.registry, slowReadKV{KeyValue: registryRoleKV(t, harness.registry), ctx: t.Context(), clock: clk, delay: claimDelay})
	swapSessionBucket(t, harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
		return slowReadKV{KeyValue: kv, ctx: t.Context(), clock: clk, delay: sessionDelay}
	})
	forwards := make(chan string, 4)
	cfg := harness.config
	cfg.clock = clk
	cfg.forwardRole = inlineForward(func(subject string, _ contracts.Envelope, _ string, _ time.Duration) error {
		forwards <- subject
		return nil
	})
	cfg = wireTestRoleForwarder(t, cfg)
	item := listenerTestEnvelope(contracts.RoleTopicPrefix+role, "slow-reads-delivery")
	probe, err := harness.client.Conn.SubscribeSync(bus.ExceptionTopicPrefix + item.Topic)
	if err != nil {
		t.Fatalf("subscribe the exception probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Unsubscribe() })
	if err := harness.client.Conn.Flush(); err != nil {
		t.Fatalf("flush the exception probe: %v", err)
	}

	data := marshalListenerEnvelope(t, item)
	delivered := make(chan struct{})
	go func() {
		defer close(delivered)
		coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: data})
	}()

	// The claim read: its slow bucket answers three quarters of the way to the read's own bound.
	claimAnswers := start.Add(claimDelay)
	armed := clk.AwaitArmedAt(t, "the claim read's slow bucket, answering three quarters of the way to the read's bound", claimAnswers)
	if bound := start.Add(roleHolderReadTimeout); !slices.Contains(armed, bound) {
		t.Fatalf("the claim read waits in its slow bucket with no bound at %s armed on the lane's clock (armed: %v), so the case does not hold", bound, armed)
	}
	clk.Advance(claimDelay)

	// The session read begins as the claim read answers, and its slow bucket answers half way to
	// a bound of its own: past the bound the claim read began under.
	sessionAnswers := claimAnswers.Add(sessionDelay)
	armed = clk.AwaitArmedAt(t, "the holder's session read's slow bucket, answering half way to the read's bound", sessionAnswers)
	if bound := claimAnswers.Add(roleHolderReadTimeout); !slices.Contains(armed, bound) {
		t.Fatalf("the session read, begun after the claim read answered %s in, waits in its slow bucket with no bound of its own at %s on the lane's clock (armed: %v): it is bounded by what the claim read left of one bound",
			claimDelay, bound, armed)
	}
	clk.Advance(sessionDelay)

	testwait.Receive(t, "the role lane's delivery to return once both reads answered", delivered)
	if !cfg.roleForwards.wait(testwait.Context(t)) {
		t.Fatalf("%d role forwards were still outstanding when the test's waits gave up", cfg.roleForwards.outstanding())
	}
	select {
	case subject := <-forwards:
		if want := contracts.LegacyAgentSubject(holder); subject != want {
			t.Fatalf("the role message was forwarded to %s, want %s", subject, want)
		}
	default:
		t.Fatalf("the role message reached no holder: a claim read that answered after %s left the session read less than its own %s bound:\n%s",
			claimDelay, roleHolderReadTimeout, harness.logs.String())
	}
	// The delivery has returned and its forward has been reported, so every exception it publishes
	// has been published.
	noException(t, probe)
	assertNothingKept(t, harness.client, role)
}
