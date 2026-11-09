package main

import (
	"context"
	"testing"
	"time"
)

// A relay whose session's record lapsed hands the messages it held to the consumer that stands,
// and the session is delivered them when it registers again. A record lapses out of the bucket
// five minutes after the last heartbeat (session.BucketMaxAge), short of the hour past which the
// reaper deletes a record and its consumer together (session.GoneHorizon), and the reaper judges
// the records the cache holds, so nothing takes that consumer away: a sleeping laptop's session
// is exactly this case. The delete of the record alone is what the row makes of that lapse, since
// the cache reports a record deleted and one lapsed the same way and the run's cancel reads
// session_gone for both.
//
// The row holds the two facts the hand-over line's second half rests on - the consumer stands
// after the hand-over, and the message the relay gave back reaches the session that registers
// again - against the clean stop's
// (TestACleanStopsRelayHandOverNeverPromisesAFurtherDelivery), where the stop deleted the
// consumer with the record and nobody is delivered them.
func TestARelayHandsOverALapsedSessionsMessagesToTheConsumerThatStands(t *testing.T) {
	rig, sessionID, silent, stored := relayHoldingOneMessage(t, "ses_lapsed_handover_")

	// The record alone, as a lapse takes it: no clean stop and no reap, so nothing deletes the
	// session's consumer.
	if err := rig.harness.sessions.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("delete the record of %s, as the lapse of that record does: %v", sessionID, err)
	}

	requireHandOverOfAGoneSession(t, rig, sessionID, "lapsed")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := rig.consumers.Info(ctx, sessionID); err != nil {
		t.Fatalf("read the consumer of %s after the hand-over: %v; want it standing, which is what a lapse leaves and what holds the naked message", sessionID, err)
	}

	// The session registers again, as a laptop that woke registers, and is delivered the message
	// the hand-over gave back. The first stand-in's connection goes first, so the frame reaches
	// the one that returned and no other.
	silent.conn.Close()
	returned := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	awaitRelayFrame(t, returned, stored, 20*time.Second)
}
