package main

import (
	"context"
	"testing"
)

// TestAHoldMarksItsGapOnce: the client tells one notice for each frame marked Envoy-Gap with
// reason privacy_hold, so a hold marks one relayed frame however many frames it drops, and frames
// relayed between its drops (a frame of no project flows during a hold) carry no second mark. A
// hold ends when a read answers (decideHeld); one that begins anew marks one frame again (the
// control: a rule that never re-opened would leave the second hold's drops untold), and the gap of
// a hold that ended before any frame carried it is still told.
func TestAHoldMarksItsGapOnce(t *testing.T) {
	hold := &sessionHold{lanes: map[string]*heldLane{}}
	relays := &sessionRelays{holds: &privacyHolds{sessions: map[string]*sessionHold{"ses_a": hold}}}
	relayed := func() bool { return relays.takeGap("ses_a") }

	hold.begin()
	for range 5 {
		relays.dropLocked(hold, heldDroppedLane, 1)
	}
	if !relayed() {
		t.Fatal("the first frame relayed after a hold's drops carries no gap mark")
	}
	for range 5 {
		relays.dropLocked(hold, heldDroppedSession, 1)
		hold.begin()
		if relayed() {
			t.Fatal("a frame relayed during the same hold carries a second gap mark")
		}
	}

	// A read answers: the hold ends. A new hold's drops mark one frame again.
	relays.decideHeld(context.Background(), "ses_a", privateSet{})
	hold.begin()
	relays.dropLocked(hold, heldDroppedLimit, 3)
	if !relayed() {
		t.Fatal("a hold that began anew after one ended marks no frame: its drops are never told")
	}
	if relayed() {
		t.Fatal("the second hold marks two frames")
	}

	// A third hold, whose drops no frame carried before a read ended it, is still told once.
	relays.decideHeld(context.Background(), "ses_a", privateSet{})
	hold.begin()
	relays.dropLocked(hold, heldDroppedLane, 1)
	relays.decideHeld(context.Background(), "ses_a", privateSet{})
	if !relayed() || relayed() {
		t.Fatal("the drops of a hold that ended before any frame carried them are not told exactly once")
	}
}
