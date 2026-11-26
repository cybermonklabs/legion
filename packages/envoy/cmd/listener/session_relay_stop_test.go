package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// stopRig is a relay rig on a NATS server of the row's own, whose link the row can freeze, with
// one session whose relay holds a frame: the session's stand-in never answers, so the frame stays
// outstanding and its message delivered and unacked. setup runs on the relays before the first
// relay starts, as their settings require. The flushes the relays' pulls send in their hand-overs
// are recorded (flushEnds).
func stopRig(t *testing.T, prefix string, setup func(*sessionRelays)) (*relayRig, *testnats.Server, string, *flushEnds) {
	t.Helper()
	srv, url := testnats.Start(t)
	rig := newRelayRigUnownedOn(t, url, relayPauseFirst, relayPauseMax, nil)
	// The receipt wait is longer than any row, so the relay cannot pause, and so forget the frame
	// it holds, before the stop.
	rig.relays.receiptWait = 10 * relayReceiptWait
	ends := &flushEnds{}
	rig.deps.consumers.consumers.SetFlushObserver(ends.add)
	setup(rig.relays)
	rig.relays.setOwned(true)
	sessionID := prefix + id.New()
	standIn := startSessionStandIn(t, rig.url, sessionID, nil)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	stored, _ := rig.storeEvent(t, contracts.LegacyAgentSubject(sessionID))
	frame := testwait.Receive(t, "the relay to hand the session its frame", standIn.frames)
	requireFrames(t, []relayedFrame{frame}, stored)
	return rig, srv, sessionID, ends
}

// flushEnds records how each flush a hand-over's Detach sent ended (bus.SessionPullFlushEnd).
type flushEnds struct {
	mu   sync.Mutex
	ends []bus.SessionPullFlushEnd
}

func (f *flushEnds) add(end bus.SessionPullFlushEnd) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, end)
}

// require fails the row unless the one hand-over's flush ended as want. A stop that waited out
// nats.go's own 10 s bound on a flush could still write the line a stop's wait writes, since
// Detach's flush and the drain's own flush time out together, so the line alone cannot tell the
// two apart; how the flush ended can.
func (f *flushEnds) require(t *testing.T, want bus.SessionPullFlushEnd) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ends) != 1 || f.ends[0] != want {
		t.Fatalf("the hand-over's flushes ended %v, want one, %s", f.ends, want)
	}
}

// stopRelays stops rig's relays with no deadline of the stop's own, so how long the hand-overs take
// is the relays' own doing, and waits for the stop with testwait.Receive.
func stopRelays(t *testing.T, rig *relayRig) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		rig.relays.stop(context.Background())
		close(stopped)
	}()
	testwait.Receive(t, "the relays' stop to return", stopped)
}

// handOverRecords is every record of logs whose msg is line and whose session_id is sessionID.
func handOverRecords(logs, line, sessionID string) []map[string]any {
	return linesIn(logs, line, map[string]any{"session_id": sessionID})
}

// requireStopEndedHandOver requires the one hand-over line of sessionID to be a WARN of a failed
// hand-over for reason, whose error says the server had not confirmed the drain when the stop's
// wait (sessionRelays.handOver) ended it.
func requireStopEndedHandOver(t *testing.T, rig *relayRig, sessionID string, reason relayCancelReason) {
	t.Helper()
	_, failed := relayHandOverLines(reason)
	logs := rig.harness.logs.String()
	records := handOverRecords(logs, failed, sessionID)
	if len(records) != 1 {
		t.Fatalf("%d records report a failed hand-over of session %s, want exactly 1:\n%s", len(records), sessionID, logs)
	}
	record := records[0]
	failure, _ := record["error"].(string)
	if record["level"] != "WARN" || record["reason"] != reason.String() ||
		!strings.Contains(failure, "had not confirmed the end of the inbox subscription") ||
		!strings.Contains(failure, "when its caller stopped waiting (the listener's stop gave its relays' hand-overs 1s)") {
		t.Fatalf("the hand-over line of session %s is %v; want a WARN for %s whose error says the server had not confirmed the drain when the stop's wait ended", sessionID, record, reason)
	}
}

// A listener that stops while its hub answers nothing stops no later than it did when every drain
// had a fixed bound: the hand-overs wait under one context that ends bus.SessionPullDrainWait after
// the stop began (sessionRelays.handOver), and a hand-over that wait ended is written as failed,
// naming what the server had not confirmed. The frozen relay leaves every connection CONNECTED, so
// nothing but that wait ends the hand-over before nats.go's own 10 s bound on the drain's flush.
func TestAStopOnASilentHubEndsItsHandOversAtTheStopsWait(t *testing.T) {
	t.Parallel()
	rig, srv, sessionID, ends := stopRig(t, "ses_stop_silent_", func(*sessionRelays) {})
	t.Cleanup(srv.Freeze(t))
	stopRelays(t, rig)
	ends.require(t, bus.SessionPullFlushEndedByCaller)
	requireStopEndedHandOver(t, rig, sessionID, relayCancelListenerStopping)
}

// A stop also ends a hand-over that another cancellation began and the silent hub holds: a run
// cancelled first for its binding keeps that reason (context.WithCancelCause keeps the first
// cause), and its hand-over waits under the same context the stop ends. The row waits for that
// hand-over to begin (handOverBegins) before it stops the relays.
func TestAStopEndsAHandOverAnotherCancellationBegan(t *testing.T) {
	t.Parallel()
	began := make(chan string, 1)
	rig, srv, sessionID, ends := stopRig(t, "ses_stop_under_way_", func(relays *sessionRelays) {
		relays.handOverBegins = func(sessionID string, reason relayCancelReason) {
			if reason == relayCancelBindingLost {
				began <- sessionID
			}
		}
	})
	t.Cleanup(srv.Freeze(t))
	rig.relays.setOwned(false)
	if got := testwait.Receive(t, "the hand-over the lost binding began", began); got != sessionID {
		t.Fatalf("the lost binding began the hand-over of session %s, want %s", got, sessionID)
	}
	stopRelays(t, rig)
	ends.require(t, bus.SessionPullFlushEndedByCaller)
	requireStopEndedHandOver(t, rig, sessionID, relayCancelBindingLost)
}

// A listener that stops while its hub answers gives back every message its relays held, and says
// so: the stop's wait ends no hand-over the server confirms. The row lengthens that wait
// (handOverWait) past its own deadline, so its verdict does not turn on how fast the machine
// answers; the silent hub's row above holds the production wait.
func TestAStopOnALiveHubConfirmsItsHandOvers(t *testing.T) {
	t.Parallel()
	rig, _, sessionID, ends := stopRig(t, "ses_stop_live_", func(relays *sessionRelays) { relays.handOverWait = 24 * time.Hour })
	stopRelays(t, rig)
	ends.require(t, bus.SessionPullFlushAnswered)

	whole, _ := relayHandOverLines(relayCancelListenerStopping)
	logs := rig.harness.logs.String()
	records := handOverRecords(logs, whole, sessionID)
	if len(records) != 1 {
		t.Fatalf("%d records report a whole hand-over of session %s, want exactly 1:\n%s", len(records), sessionID, logs)
	}
	if record := records[0]; record["reason"] != "listener_stopping" || record["returned"] != float64(1) {
		t.Fatalf("the hand-over line of session %s is %v; want it for listener_stopping with the one message the relay held returned", sessionID, record)
	}
}
