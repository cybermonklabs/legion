package main

import (
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A relay whose every probe read reads a false "down" - a box loaded past the probe's budget -
// backs off its restarts: each restart's first session-consumer call answers ErrConnectionLost at
// once (connForRequest), the relay waits in Reconnected, which reads nats.go connected and returns
// at once, and a second loss in a row within relayRetryFirst of its restart asks for the retry
// backoff (relay), which doubles. Its context is the relay's whole run, so no call budget bounds it,
// unlike a registration or a delete. The row counts by events (newCountedRelayRig): the waits come
// one lost restart apart, each twice the one before, from relayRetryFirst.
//
// The control is the relay restarting at once on every loss: it asks for no wait, and reads its
// probe without end.
func TestARelayWhoseProbeReadsDownBacksOffItsRestarts(t *testing.T) {
	rig, waits := newCountedRelayRig(t, newRelayClock())
	const sessionID = "ses_relay_false_down"
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	before, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	requireFrames(t, standIn.collect(t, 1, testnats.Remaining(t)), before)

	down, spun := countingDown()
	restore := rig.deps.consumers.consumers.ProbeReadsDownForTest(down)
	defer restore()
	rig.deps.consumers.consumers.CloseConnection()
	want, previous := relayRetryFirst, -1
	for i := range 4 {
		asked := waits.next(t, spun, "a relay whose probe reads down")
		if asked.d != want {
			t.Fatalf("wait %d is %s, want %s: the retry backoff does not double from %s", i+1, asked.d, want, relayRetryFirst)
		}
		if previous >= 0 && asked.losses-previous != 1 {
			t.Fatalf("wait %d came %d lost restarts after the one before it, want 1", i+1, asked.losses-previous)
		}
		want, previous = min(2*want, relayRetryMax), asked.losses
		if i == 3 {
			restore()
		}
		asked.release <- true
	}
}
