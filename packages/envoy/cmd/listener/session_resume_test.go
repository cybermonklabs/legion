package main

import (
	"encoding/json"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// consumerResumeRegistration is the registration body of a session in consumer mode at epoch
// asking to resume after stream sequence seq of stream streamID, as the plugin sends it when it
// comes back (packages/envoy-client/src/consumer-delivery.ts, registration).
func consumerResumeRegistration(sessionID, epoch, streamID string, seq uint64) string {
	body, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"topics":          []string{},
		"delivery": map[string]any{
			"consumer": true,
			"epoch":    epoch,
			"resume":   map[string]any{"stream_id": streamID, "seq": seq},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(body)
}

// A session that took and acked messages and then deregistered — which deletes its consumer —
// comes back with the resume point its plugin holds, the highest stream sequence it wrote. Every
// message stored while it was away is relayed to it once, and nothing it had acked is relayed
// again: the consumer is created at the sequence after that point.
func TestSessionConsumerResumePointAfterADeregistrationDeliversOnlyWhatWasStoredWhileItWasAway(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_resume_after_deregister"
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	var acked []contracts.Envelope
	var wrote uint64
	for range 2 {
		item, seq := rig.storeEvent(t, agent)
		acked = append(acked, item)
		wrote = seq
	}
	requireFrames(t, standIn.collect(t, len(acked), 5*time.Second), acked...)
	awaitAcked(t, rig.consumers, sessionID, wrote)

	rig.deregister(t, sessionID, "")
	rig.awaitNoConsumer(t, sessionID)
	var away []contracts.Envelope
	for range 2 {
		item, _ := rig.storeEvent(t, agent)
		away = append(away, item)
	}

	rig.subscribe(t, consumerResumeRegistration(sessionID, "epoch-2", rig.streamID(t), wrote))

	requireFrames(t, standIn.collect(t, len(away), 5*time.Second), away...)
	standIn.none(t, time.Second)
}

// A registration whose resume point is below the stream's first sequence is answered gap: true:
// what its consumer would have delivered from that point has aged out of the stream or been
// discarded, and the session is told so rather than left to believe it was given everything.
func TestSessionConsumerResumePointBelowTheStreamsFirstSequenceIsAnsweredAGap(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_resume_gap"
	agent := contracts.AgentSubject(sessionID)
	gone := rig.store(t, agent)
	kept := rig.store(t, agent)
	if err := rig.harness.client.JS().PurgeStream(bus.Stream, &natsgo.StreamPurgeRequest{Sequence: kept}); err != nil {
		t.Fatalf("purge the stream below %d: %v", kept, err)
	}

	answer := rig.subscribe(t, consumerResumeRegistration(sessionID, "e", rig.streamID(t), gone-1))

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	if answer.Delivery.Gap == nil || !*answer.Delivery.Gap {
		t.Fatalf("answer %s, want gap true: the consumer starts at %d, below the stream's first sequence %d", answer.raw, gone, kept)
	}
	if start := rig.startSeq(t, sessionID); start != gone {
		t.Fatalf("the consumer of %s starts at %d, want %d, the sequence after its resume point", sessionID, start, gone)
	}
}

// A session that received nothing and acked nothing holds no resume point of its own: all it has
// is the floor its registration's answer gave it, the sequence its consumer was created at
// (consumer_start). Its next registration asks to resume at that start less one, which creates
// the consumer again exactly where the first one stood, so a message stored while the session was
// away reaches it — where a registration with no resume point at all would start at the stream's
// end and never deliver it.
func TestSessionConsumerStartTheAnswerCarriesIsWhereASessionThatReceivedNothingComesBack(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_resume_consumer_start"
	// Another session's message, so the consumer's start is above the stream's first sequence
	// and a floor of consumer_start - 1 names a message of the stream.
	rig.store(t, contracts.AgentSubject("ses_resume_bystander"))
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)

	first := rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	if first.Delivery == nil || first.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", first.raw)
	}
	if first.Delivery.ConsumerStart == 0 {
		t.Fatalf("answer %s carries no consumer_start: a session that receives nothing has no other floor to come back with", first.raw)
	}
	if start := rig.startSeq(t, sessionID); first.Delivery.ConsumerStart != start {
		t.Fatalf("the answer carries consumer_start %d, want %d, the sequence its consumer was created at", first.Delivery.ConsumerStart, start)
	}
	// It is handed nothing: the floor below is all it has.
	standIn.none(t, 200*time.Millisecond)

	rig.deregister(t, sessionID, "")
	// The delete runs after the answer, and this row is about what the registration below finds
	// once it has run: the consumer gone and its floor on the resume lane.
	rig.awaitNoConsumer(t, sessionID)
	away, awaySeq := rig.storeEvent(t, contracts.AgentSubject(sessionID))
	rig.subscribe(t, consumerResumeRegistration(sessionID, "epoch-2", rig.streamID(t), first.Delivery.ConsumerStart-1))

	if start := rig.startSeq(t, sessionID); start != first.Delivery.ConsumerStart {
		t.Fatalf("the consumer of %s starts again at %d, want %d, the start its first answer carried", sessionID, start, first.Delivery.ConsumerStart)
	}
	frames := standIn.collect(t, 1, 5*time.Second)
	requireFrames(t, frames, away)
	if frames[0].seq != awaySeq {
		t.Fatalf("the frame carries stream sequence %d, want %d, the message stored while the session was away", frames[0].seq, awaySeq)
	}
}

// The rollout guard: the listener's deletes meet sessions whose plugin predates the
// consumer_start floor, since the hub is pinned before a fleet's sessions relaunch on a new
// pi-envoy or claude-envoy. Such a session has received nothing, so it holds no resume point of
// its own and its registration carries none, and the listener alone keeps its position: the
// floor recorded on the stream before the delete (bus.SessionResumeSubject) is where its next
// consumer is created. Without that record the consumer would start at the stream's end and
// every message stored while the session was away would be lost to it.
//
// The messages below are a followed issue topic's and the session's own inbox, which is where a
// role message reaches a holder too: the listener forwards a role message onto the holder's
// agent subject, which the stream stores, where the role lane itself is core NATS and in no
// consumer's filter.
func TestASessionWhosePluginSendsNoResumePointLosesNothingToTheDelete(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_resume_old_plugin"
	const followed = "notifications.dispatch.issue.INFRA-2365.comment"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", followed))
	// It is handed nothing at all, which is the population this guard is for.
	standIn.none(t, 200*time.Millisecond)

	rig.deregister(t, sessionID, "")
	rig.awaitNoConsumer(t, sessionID)
	onTopic, topicSeq := rig.storeEvent(t, followed)
	toSession, sessionSeq := rig.storeEvent(t, contracts.AgentSubject(sessionID))

	// The old plugin's registration: a consumer, and no resume point of any kind.
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-2", followed))

	frames := standIn.collect(t, 2, 5*time.Second)
	requireFrames(t, frames, onTopic, toSession)
	if frames[0].seq != topicSeq || frames[1].seq != sessionSeq {
		t.Fatalf("the frames carry stream sequences %d and %d, want %d and %d, the two messages stored while the session was away",
			frames[0].seq, frames[1].seq, topicSeq, sessionSeq)
	}
	standIn.none(t, time.Second)
}
