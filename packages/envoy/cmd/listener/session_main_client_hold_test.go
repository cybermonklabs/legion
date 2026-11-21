package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A delete of a gone session's consumer made while the hub holds the handshake of the listener's
// MAIN client's reconnect - the client that owns the session and interest buckets, not the session
// consumers' - releases the session's lock within the probe's budget, while the handshake is still
// held: the delete's first call under the lock is the session bucket's leader read on the main
// client. Two protections guard it: the read asks that client's probe first
// (sessionDelivery.mainLost, bus.Client.LostNow), and the read is made under the context the
// delete marks as made under a lock (bus.WithLockHeld), so the request's own read of its
// connection answers within the probe's budget too (bus.Client.connForRequest, requestRead). Either
// one answers the loss, and the delete releases the lock and waits for the connection with nothing
// held (relockAfterReconnect). The session consumers' client is direct, so only the main client's
// handshake is held. Once the hold is released (testnats.HandshakeHold) the delete goes on.
//
// The control removes both protections: the leader read made without asking the main client first,
// on the delete's context without its mark. Removing either alone leaves the other, and the row
// passes. With both removed the delete holds the session's lock while the main client's handshake
// is held until its call budget ends (5 s), then gives the lock up and reports the consumer
// standing, and the row fails at its check of the delete's outcome. The lock's wait is bounded only
// by the test's deadline.
func TestADeleteDuringAHeldMainClientHandshakeReleasesTheSessionsLock(t *testing.T) {
	url := testnats.URL(t)
	hold := testnats.StartHandshakeHold(t, url)
	client, err := bus.ConnectOwningStream([]string{hold.URL()}, bus.WithReplicas(1), bus.WithConnectTimeout(testnats.Remaining(t)))
	if err != nil {
		t.Fatalf("connect the main client through the handshake hold: %v", err)
	}
	t.Cleanup(client.Close)
	t.Cleanup(hold.Release)
	now := &stateClock{}
	rig := sweepOver(newSessionConsumerRigOn(t, client, url, sweepSessionOptions(sweepRecordTTL, now)...), now)
	const sessionID = "ses_delete_main_held_handshake"
	rig.register(t, sessionID)
	rig.now.advance(sweepLapse)
	rig.requireState(t, sessionID, session.Gone)
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()

	select {
	case <-hold.Hold():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("no reconnect's CONNECT of the main client reached the server through the hold by the test's deadline")
	}
	locks := &rig.deps.consumers.locks
	held := locks.lock(sessionID)
	deleted := make(chan bool, 1)
	go func() { deleted <- rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID, nil) }()
	awaitLockUsers(t, locks, sessionID, 2)
	held()
	for deadline := time.Now().Add(testnats.Remaining(t)); lockUsers(locks, sessionID) != 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			hold.Release()
			t.Fatal("the delete still held the session's lock at the test's deadline, during the main client's held handshake: it waited for that client's connection lock with the session's lock held")
		}
	}
	select {
	case <-hold.Ended():
		t.Fatal("the delete released the session's lock only once the main client's held handshake had ended")
	default:
	}

	hold.Release()
	select {
	case gone := <-deleted:
		if !gone {
			t.Fatalf("the delete made once the main client was back reported the consumer standing; log: %s", rig.harness.logs.String())
		}
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the delete was still waiting at the test's deadline")
	}
	rig.requireNoConsumer(t, sessionID)
}
