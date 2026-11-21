package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// This row names the stop's deadline and its shares (keepHeldNow's end, roleStopLineReserve,
// roleStopForwardMin), which the forwarder before it did not have; the rows that build against it
// are in role_lane_deadline_rows_test.go.

// A pinned message the stop reaches with too little of its deadline left for a forward once more
// before its keep (reforwardShare, roleStopForwardMin) makes no forward and is kept at once, with
// the WARN naming why no forward was made. The forward that pins it is still in flight at the
// stop, and ends only when what is left of the stop's deadline is under roleStopForwardMin.
func TestAPinnedMessageTheStopLeavesNoTimeToForwardIsKeptAtOnce(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	const role = "pinned-at-the-stop-late"
	claimFor(t, harness, role, "ses_pinned_late")
	entered, release := make(chan struct{}), make(chan struct{})
	var forwards atomic.Int32
	cfg := harness.config
	cfg.forwardRole = func(string, contracts.Envelope, string, time.Duration) (func() error, error) {
		if forwards.Add(1) > 1 {
			return func() error { return nil }, nil
		}
		return func() error {
			close(entered)
			<-release
			return stalledForward
		}, nil
	}
	cfg = wireTestRoleForwarder(t, cfg)
	forwarder := cfg.roleForwards
	item, msg := laneMessage(t, role, "pinned-at-the-stop-late")

	coreNATSDeliveryHandler(cfg)(msg)
	<-entered
	forwarder.stopHolding()
	keepBy := forwarder.keepHeldNow().Add(-roleStopLineReserve)
	time.Sleep(time.Until(keepBy.Add(-roleStopForwardMin)))
	close(release)
	forwarder.settled(t)

	logs := harness.logs.String()
	if got := forwards.Load(); got != 1 {
		t.Fatalf("the lane made %d forwards, want only the one in flight at the stop:\n%s", got, logs)
	}
	warned := linesIn(logs, roleKeptAfterUnknownForwardLine, map[string]any{"event_id": item.EventID})
	if len(warned) != 1 || warned[0]["no_forward"] != "the stop left too little time for a forward before the keep" {
		t.Fatalf("lines %v, want one WARN that the stop left no time for a forward:\n%s", warned, logs)
	}
	kept := len(linesIn(logs, keptLine, map[string]any{"event_id": item.EventID, "reason": string(roleKeptAtShutdown)}))
	if ends := kept + countLogged(logs, roleHeldUnkeptLine, item.EventID); ends != 1 {
		t.Fatalf("the message ended %d times, want once: kept, or the line that it was not:\n%s", ends, logs)
	}
}
