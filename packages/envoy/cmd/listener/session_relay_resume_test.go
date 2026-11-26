package main

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The bounds of the rows below: a receipt wait of 500 ms, and a pause backoff of 2 s that doubles
// to 4 s for the pause after the relay's first probe, which neither row waits out.
const (
	relayProbeReceipt    = 500 * time.Millisecond
	relayProbePauseFirst = 2 * time.Second
	relayProbePauseMax   = 30 * time.Second
)

// relayResumedPrefix begins the INFO line a relay writes when a registration, or at the edge a
// role claim, ends its pause before the backoff does (sessionRelays.relay). The relay writes that
// message as a literal in its one log call, so the rows hold their own copy of its start.
const relayResumedPrefix = "session relay resumed:"

// relayInAProbe brings sessionID to the moment R21 is about and returns there. The session
// registers and answers e0, and registers again while it answers, as its heartbeat does; then it
// stops answering, and its relay pauses on e1, the first frame it leaves unanswered. That pause
// is not cut short by the heartbeat, which came before e1 went out, so it waits its 2 s backoff;
// e2 is stored during it. The relay's first probe then sends the silent session e1 and e2 again,
// and relayInAProbe returns once both have arrived, inside that probe's 500 ms receipt wait: a
// registration made at once is heard before the pause after the probe, whose backoff is 4 s. It
// returns the rig, the silent stand-in, which keeps every frame the relay sends, the two messages
// and when the probe's first frame arrived.
func relayInAProbe(t *testing.T, sessionID string) (*relayRig, *sessionStandIn, contracts.Envelope, contracts.Envelope, time.Time) {
	t.Helper()
	rig := newRelayRigReceipt(t, relayProbePauseFirst, relayProbePauseMax, relayProbeReceipt)
	agent := contracts.LegacyAgentSubject(sessionID)
	silent := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	e0, e0Seq := rig.storeEvent(t, agent)
	requireFrames(t, silent.collect(t, 1, 5*time.Second), e0)
	awaitAcked(t, rig.consumers, sessionID, e0Seq)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	silent.silent.Store(true)
	e1, _ := rig.storeEvent(t, agent)
	unanswered := silent.collect(t, 1, 5*time.Second)[0]
	requireFrames(t, []relayedFrame{unanswered}, e1)
	// The probe's restart deletes the consumer and creates it again; nothing it delivers or takes
	// changes after this frame until then, since the paused relay pulls nothing.
	awaitSessionConsumerStored(t, rig.harness.client.Conn, sessionID)
	time.Sleep(time.Until(unanswered.arrived.Add(relayProbeReceipt + 300*time.Millisecond)))
	e2, _ := rig.storeEvent(t, agent)
	// The heartbeat did not end the pause: nothing is sent before the backoff has passed.
	silent.none(t, 900*time.Millisecond)
	probe := silent.collect(t, 2, relayProbePauseFirst+2*time.Second)
	requireFrames(t, probe, e1, e2)
	return rig, silent, e1, e2, probe[0].arrived
}

// requireInsideTheProbe fails the row unless the registration it just made answered inside the
// receipt wait of the probe whose first frame arrived at probed, so the relay heard it before the
// pause began. A registration answered after that wait may have been heard during the pause, and
// the relay ends a pause at once at a registration heard during it, as at one heard before it
// (relayRun.waitPause), so the row would not show that a registration heard before the pause ends
// it.
func requireInsideTheProbe(t *testing.T, probed time.Time) {
	t.Helper()
	if waited := time.Since(probed); waited >= relayProbeReceipt {
		t.Fatalf("the registration answered %s after the probe's first frame, past its %s receipt wait: it was not made while the probe waited for its receipt", waited, relayProbeReceipt)
	}
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: a registration inside a probe's receipt wait of 500 ms, and the session's messages
// inside 2 s of it.
//
// Check l19 of dispatch://INFRA-3589 (R21): a registration that holds an open relay subject
// resumes a relay the listener paused while the session could not answer, so the session has its
// messages at once rather than after the pause backoff. The session's connection that left e1 and
// e2 unanswered is gone; the session opens a new one and registers, with the epoch it holds, while
// the relay's probe waits for its receipt, and answers frames from then on. The relay pauses when
// that receipt wait ends, and the registration, heard after the frames that pause is about went
// out, ends it at once: e1 and e2 reach the session once each, in stream order, inside 2 s of the
// registration, where the pause would wait 4 s, and the listener logs the resume once, at INFO,
// naming the session and the backoff it cut short.
func TestARegistrationResumesARelayPausedWhileTheSessionCouldNotAnswer(t *testing.T) {
	const sessionID = "ses_resumed_in_probe"
	rig, gone, e1, e2, probed := relayInAProbe(t, sessionID)
	gone.close()
	returned := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	requireInsideTheProbe(t, probed)

	requireFrames(t, returned.collect(t, 2, 2*time.Second), e1, e2)
	returned.none(t, time.Second)
	lines := edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayResumedPrefix, sessionID)
	if len(lines) != 1 || lines[0]["backoff"] != (2*relayProbePauseFirst).String() {
		t.Fatalf("the resume was logged %+v, want one INFO line naming the %s backoff it cut short:\n%s", lines, 2*relayProbePauseFirst, rig.harness.logs.String())
	}
}

// Serial (listenerTestParallelism): it holds a wall-clock bound within a second or two of what it
// waits for: a registration inside a probe's receipt wait of 500 ms.
//
// A registration earns a paused relay one probe, not one each receipt wait: a session that
// registers while the relay's probe waits for its receipt, and still answers nothing, is sent its
// messages again at once, and then nothing more until the next backoff has passed. That
// registration came before the frames of the probe it earned, so the pause after that probe does
// not take it, and a session that registers and never answers costs no restart of its consumer
// every receipt wait.
func TestARegistrationEarnsAPausedRelayOneProbe(t *testing.T) {
	const sessionID = "ses_probed_once"
	rig, silent, e1, e2, probed := relayInAProbe(t, sessionID)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	requireInsideTheProbe(t, probed)

	requireFrames(t, silent.collect(t, 2, 2*time.Second), e1, e2)
	// The pause after this probe waits 8 s; a relay that took the registration again would probe
	// within the receipt wait.
	silent.none(t, 3*time.Second)
	if lines := edgeSessionLogLines(rig.harness.logs.String(), "INFO", relayResumedPrefix, sessionID); len(lines) != 1 {
		t.Fatalf("the resume was logged %d times, want once for the one registration:\n%s", len(lines), rig.harness.logs.String())
	}
}
