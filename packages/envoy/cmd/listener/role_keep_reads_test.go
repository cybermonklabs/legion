package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
)

// A kept copy that carries an expects_reply word this build does not know is one the kept lane's
// walk keeps for the role's next holder, since the walk reads a copy field by field
// (judgeKeptRoleCopy, readRoleFrame). The two other readers of a kept copy read it the same way: the
// keep that met the copy as a duplicate finds it held (heldKeptRoleCopy), rather than storing one
// more copy after it, and the sweep's wake scan counts it as a copy that wakes the role
// (keptLaneWakesItsRole).
func TestAKeptCopyWithAnExpectsReplyWordThisBuildDoesNotKnowIsReadAlikeByTheWalkTheKeepAndTheWakeScan(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "kept-unknown-expects-reply"
	var frame markedFrame
	for _, shape := range roleFramesOfEveryShape(t, role) {
		if shape.shape == "a newer build's expects_reply word" {
			frame = shape
		}
	}
	if frame.data == nil {
		t.Fatal("roleFramesOfEveryShape holds no frame with an expects_reply word this build does not know")
	}
	ack, err := harness.client.JS().Publish(bus.KeptRoleSubject(role), frame.data)
	if err != nil {
		t.Fatalf("store the copy on role %s's kept lane: %v", role, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stored, err := harness.client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), ack.Sequence)
	if err != nil || stored.Sequence != ack.Sequence {
		t.Fatalf("read the copy back at sequence %d: %v (read sequence %d)", ack.Sequence, err, stored.Sequence)
	}

	walk := judgeKeptRoleCopy(listenerDeliveryHandlerConfig{}, role, "", session.SessionEntry{}, stored,
		keptCopyClaimOf(bus.KeptRoleCursor{}, stored.Sequence), time.Now, func(string, string) bool { return false })
	if walk.step != keptCopyWait {
		t.Errorf("the walk with no holder judges the copy as step %d, want %d: it waits for the role's next holder", walk.step, keptCopyWait)
	}
	kept, held, err := heldKeptRoleCopy(ctx, harness.config, role, ack.Sequence)
	if err != nil {
		t.Fatalf("judge the copy a duplicate keep met: %v", err)
	}
	if !held {
		t.Errorf("the keep judges the copy at sequence %d ended, want it held: the walk keeps it for the next holder", ack.Sequence)
	} else if kept.sequence != ack.Sequence {
		t.Errorf("the keep holds the copy at sequence %d, want %d", kept.sequence, ack.Sequence)
	}
	wakes, err := keptLaneWakesItsRole(ctx, harness.client, role, ack.Sequence, time.Now())
	if err != nil {
		t.Fatalf("scan role %s's kept lane: %v", role, err)
	}
	if !wakes {
		t.Errorf("the wake scan does not count the copy, want it to wake role %s: the walk keeps it for the next holder", role)
	}
}
