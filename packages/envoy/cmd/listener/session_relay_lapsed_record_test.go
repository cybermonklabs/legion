package main

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
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
// (TestACleanStopsRelayHandOverSaysTheMessagesItHeldAreDeliveredToNobody), where the stop deleted
// the consumer with the record and nobody is delivered them.
func TestARelayHandsOverALapsedSessionsMessagesToTheConsumerThatStands(t *testing.T) {
	// The receipt wait is longer than the row, so the relay cannot pause, and so forget the
	// frame it holds, before the record goes.
	rig := newRelayRigReceipt(t, relayPauseFirst, relayPauseMax, 30*time.Second)
	sessionID := "ses_lapsed_handover_" + id.New()
	// A nil answer leaves the stand-in silent, so the frame stays outstanding and its message
	// stays delivered and unacked: the relay holds it when the record goes.
	silent := startSessionStandIn(t, rig.url, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	stored, _ := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	requireFrames(t, silent.collect(t, 1, 10*time.Second), stored)
	awaitAckState(t, rig.consumers, sessionID, 1, 0)

	// The record alone, as a lapse takes it: no clean stop and no reap, so nothing deletes the
	// session's consumer.
	if err := rig.harness.sessions.Delete(context.Background(), sessionID); err != nil {
		t.Fatalf("delete the record of %s, as the lapse of that record does: %v", sessionID, err)
	}

	const lapsed = "session relay stopped; the session has no record, so nothing relays it; the messages it held stand on the session's consumer, which delivers them when the session registers again, and nobody is delivered them where a stop or a reap deleted that consumer with the record"
	awaitLogLine(t, rig, "session relay stopped;", 15*time.Second)
	report := handOverLineNaming(t, rig.harness.logs.String(), sessionID)
	if report["msg"] != lapsed {
		t.Fatalf("the hand-over line of the lapsed session %s reads %q, want %q", sessionID, report["msg"], lapsed)
	}
	if report["reason"] != "session_gone" {
		t.Fatalf("the hand-over line of the lapsed session %s names reason %#v, want session_gone: the cache holds no record of it", sessionID, report["reason"])
	}
	// A number of a log record decodes from JSON as a float64, so the count reads as one here.
	if returned, ok := report["returned"].(float64); !ok || returned != 1 {
		t.Fatalf("the hand-over line of the lapsed session %s counts returned %#v, want the number 1: the relay held one message:\n%v", sessionID, report["returned"], report)
	}

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
