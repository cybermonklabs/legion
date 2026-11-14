package main

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// slowReadKV is a bucket each read of which answers, after delay: a bus that is slow, not silent.
type slowReadKV struct {
	natsgo.KeyValue
	delay time.Duration
}

func (kv slowReadKV) Get(key string) (natsgo.KeyValueEntry, error) {
	time.Sleep(kv.delay)
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
func TestTheRoleLaneBoundsItsClaimReadAndItsHoldersSessionReadEachOnItsOwn(t *testing.T) {
	const (
		role   = "slow-reads"
		holder = "ses_slow_reads"
	)
	claimDelay, sessionDelay := roleHolderReadTimeout*3/4, roleHolderReadTimeout/2
	harness := newListenerDeliveryHarness(t)
	// The cache stops following the bucket before the holder registers, so the resolution finds
	// no live record of the holder in it and reads the session bucket.
	harness.sessions.StopWatch()
	storeSessionHeartbeat(t, harness.client, holder, time.Now())
	if _, err := harness.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim role %s for %s: %v", role, holder, err)
	}
	setRegistryRoleKV(t, harness.registry, slowReadKV{KeyValue: registryRoleKV(t, harness.registry), delay: claimDelay})
	swapSessionBucket(t, harness.sessions, func(kv natsgo.KeyValue) natsgo.KeyValue {
		return slowReadKV{KeyValue: kv, delay: sessionDelay}
	})
	forwards := make(chan string, 4)
	cfg := harness.config
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

	started := time.Now()
	coreNATSDeliveryHandler(cfg)(&natsgo.Msg{Data: marshalListenerEnvelope(t, item)})
	elapsed := time.Since(started)
	cfg.roleForwards.settled(t)
	t.Logf("the claim read answered after %s and the session read after %s, each bounded by %s; the delivery took %s",
		claimDelay, sessionDelay, roleHolderReadTimeout, elapsed)

	select {
	case subject := <-forwards:
		if want := contracts.AgentSubject(holder); subject != want {
			t.Fatalf("the role message was forwarded to %s, want %s", subject, want)
		}
	default:
		t.Fatalf("the role message reached no holder: a claim read that answered after %s left the session read less than its own %s bound:\n%s",
			claimDelay, roleHolderReadTimeout, harness.logs.String())
	}
	noException(t, probe)
	assertNothingKept(t, harness.client, role)
	if elapsed < claimDelay+sessionDelay {
		t.Fatalf("the delivery took %s, less than the two reads' delays of %s together: one of them did not reach its slow bucket, so the case did not hold",
			elapsed, claimDelay+sessionDelay)
	}
}
