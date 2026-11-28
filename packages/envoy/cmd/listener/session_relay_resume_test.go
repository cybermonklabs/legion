package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/clock/clocktest"
	"github.com/sjawhar/envoy/internal/contracts"
)

// relayResumedPrefix begins the INFO line a relay writes when a registration, or at the edge a
// role claim, ends its pause before the backoff does (sessionRelays.relay). The relay writes that
// message as a literal in its one log call, so the rows hold their own copy of its start.
const relayResumedPrefix = "session relay resumed:"

// relayInAProbe answers e0, then sends a heartbeat before e1 goes out. That heartbeat must
// not end the pause on e1. The test fires the pause timer to get the first probe of e1 and e2.
func relayInAProbe(t *testing.T, sessionID string) (*relayRig, *clocktest.Fake, *sessionStandIn, contracts.Envelope, contracts.Envelope) {
	t.Helper()
	rig, fake := newRelayRigOnFakeClock(t)
	agent := contracts.LegacyAgentSubject(sessionID)
	silent := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	e0, e0Seq := rig.storeEvent(t, agent)
	requireFrames(t, silent.receive(t, 1), e0)
	awaitAcked(t, rig.consumers, sessionID, e0Seq)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	silent.silent.Store(true)
	e1, _ := rig.storeEvent(t, agent)
	requireFrames(t, silent.receive(t, 1), e1)
	// The probe recreates this consumer at its persisted ack floor.
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
	expireProbeReceipt(t, fake)
	backoff := fake.Now().Add(relayPauseFirst)
	fake.AwaitArmedAt(t, "the first pause backoff", backoff)
	e2, _ := rig.storeEvent(t, agent)
	fake.Set(backoff.Add(-time.Nanosecond))
	requireRelayStillPaused(t, fake, backoff, silent)
	fake.Set(backoff)
	requireFrames(t, silent.receive(t, 2), e1, e2)
	return rig, fake, silent, e1, e2
}

// expireProbeReceipt lets the relay pause only after it arms the receipt deadline. Until
// this call, the fake clock holds the receipt wait open for any registration the test makes.
func expireProbeReceipt(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	receipt := fake.Now().Add(relayReceiptWait)
	fake.AwaitArmedAt(t, "the unanswered frame's receipt deadline", receipt)
	fake.Set(receipt)
}

// Check l19 of dispatch://INFRA-3589 (R21): a registration that holds an open relay subject
// resumes a relay the listener paused while the session could not answer, so the session has its
// messages at once rather than after the pause backoff. The session's connection that left e1 and
// e2 unanswered is gone; the session opens a new one and registers, with the epoch it holds, while
// the relay's probe waits for its receipt, and answers frames from then on. The relay pauses when
// that receipt wait ends, and the registration, heard after the frames that pause is about went
// out, ends it before the test advances the backoff: e1 and e2 reach the session once each,
// in stream order. The listener logs one INFO resume naming the backoff it cut short.
func TestARegistrationResumesARelayPausedWhileTheSessionCouldNotAnswer(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_resumed_in_probe"
	rig, fake, gone, e1, e2 := relayInAProbe(t, sessionID)
	gone.close()
	returned := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	expireProbeReceipt(t, fake)

	requireFrames(t, returned.receive(t, 2), e1, e2)
	rig.noneThroughMarker(t, returned, sessionID)
	requireOnePause(t, rig)
	lines := edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayResumedPrefix, sessionID)
	if len(lines) != 1 || lines[0]["backoff"] != (2*relayPauseFirst).String() {
		t.Fatalf("the resume was logged %+v, want one INFO line naming the %s backoff it cut short:\n%s", lines, 2*relayPauseFirst, rig.harness.logs.String())
	}
}

// A registration earns a paused relay one probe, not one each receipt wait: a session that
// registers while the relay's probe waits for its receipt, and still answers nothing, is sent its
// messages again at once, and then nothing more until the next backoff has passed. That
// registration came before the frames of the probe it earned, so the pause after that probe does
// not take it, and a session that registers and never answers costs no restart of its consumer
// every receipt wait.
func TestARegistrationEarnsAPausedRelayOneProbe(t *testing.T) {
	t.Parallel()
	const sessionID = "ses_probed_once"
	rig, fake, silent, e1, e2 := relayInAProbe(t, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	expireProbeReceipt(t, fake)

	requireFrames(t, silent.receive(t, 2), e1, e2)
	expireProbeReceipt(t, fake)
	// Neither the first timer nor the registration can end this pause. The receipt wait
	// ended, but the silent session must now wait for the third rung of the backoff.
	backoff := fake.Now().Add(4 * relayPauseFirst)
	fake.AwaitArmedAt(t, "the pause after the one earned probe", backoff)
	requireRelayStillPaused(t, fake, backoff, silent)
	requireOnePause(t, rig)
	if lines := edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayResumedPrefix, sessionID); len(lines) != 1 {
		t.Fatalf("the resume was logged %d times, want once for the one registration:\n%s", len(lines), rig.harness.logs.String())
	}
}
