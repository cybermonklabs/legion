package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// The tests of how a role lane failure ranks among the self-health probe's other failures. They are
// apart from self_health_role_lane_test.go, which names nothing the listener lacked before the
// probe covered the lane but the probe's one call, so that file builds on such a tree with that call
// written for its signature, the negative control of the probe's own rows.

// checkSelfHealth checks the role lane first and reports its failure last. A lost durable is a
// failure the monitor rebuilds, decided from the error the probe returns
// (isUnrecoverableSelfHealthFailure), so with the lane refused and the durable gone the error is
// the durable's. The lane is still sent again: checked after the durable, which returns early, it
// would not be.
func TestTheSelfHealthProbeReportsALostDurableBeforeARefusedRoleLaneAndStillSendsTheLane(t *testing.T) {
	rig := newRoleLaneHealthRig(t, "role-lane-health-order")
	rig.stop()
	rig.revoke(t)
	waitFor(t, 30*time.Second, "the role lane to read the revocation", func() bool { return !rig.lane.CoreSubOK() })
	from := rig.traceFrom()
	err := checkSelfHealth(rig.caches, rig.lane, func() error { return natsgo.ErrConsumerNotFound })
	if !errors.Is(err, natsgo.ErrConsumerNotFound) || errors.Is(err, errRoleLaneInactive) {
		t.Fatalf("with the durable gone and the role lane refused, the probe returned %v; want the durable's error", err)
	}
	if !isUnrecoverableSelfHealthFailure(err, rig.client, rig.caches) {
		t.Fatalf("the monitor would not rebuild for %v", err)
	}
	if err := rig.lane.Conn.Flush(); err != nil {
		t.Fatalf("flush the role lane: %v", err)
	}
	if sent := rig.sent(from); sent != 2 {
		t.Fatalf("the probe sent the refused role lane's subscription %d times, want once in each of its two queue groups", sent)
	}
}

// A role lane failure is never one the monitor rebuilds, whatever it wraps. A send of the lane's
// subscription that fails because the lane's own connection closed under it wraps
// nats.ErrConnectionClosed, which for the main bus is a failure to rebuild, and the rebuild would
// rebuild the main bus for a fault of the lane's connection. The same error outside the lane is
// still one.
func TestARoleLaneFailureIsNeverOneTheMonitorRebuilds(t *testing.T) {
	client := setupTestNATS(t)
	laneClosed := fmt.Errorf("%w: sending it again failed: %w", errRoleLaneInactive, natsgo.ErrConnectionClosed)
	if isUnrecoverableSelfHealthFailure(laneClosed, client, nil) {
		t.Fatalf("the monitor would rebuild for the role lane's %v", laneClosed)
	}
	if !isUnrecoverableSelfHealthFailure(natsgo.ErrConnectionClosed, client, nil) {
		t.Fatal("the monitor would not rebuild for the main bus's closed connection; the check proves nothing")
	}
}
