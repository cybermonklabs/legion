package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A relay restart that landed and whose settle then lost its connection - the read of the
// bucket's newest row the fence judges the restart's write by - returns relayLost at that loss,
// with the session's lock and its restart turn released, so the relay waits for the connection in
// its loop holding nothing (bus.Reconnected's rule) and restarts from the bucket's newest row once
// the connection is back. The hold cuts the connection and refuses every reconnect
// (testnats.ReplyHold.Cut) until the restart has returned, so a restart that waited for the
// connection inside itself, its turn held, could only end at its own budget's end and on the
// failure of what it did next, its pull. The row sees the order through the restart's own cause:
// the settle's loss, logged as such, while the listener is still disconnected. Once the connection
// is back the next restart pulls. Every wait is bounded by the test's deadline alone
// (testnats.Remaining).
//
// The control is the same row at 7a49d38c1c: the restart waited in bus.Reconnected with its turn
// held until its budget, sessionConsumerCallTimeout (5 s), ended, then its pull failed on the
// connection the cut still refused, and it returned relayLost for the pull, not for the settle.
func TestARestartWhoseSettleLostItsConnectionReturnsForTheWaitHoldingNothing(t *testing.T) {
	rig, hold, _, client := heldListener(t)
	relays, err := startSessionRelays(rig.deps.consumers, rig.harness.config)
	if err != nil {
		t.Fatalf("start the session relays: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
		defer cancel()
		relays.stop(ctx)
	})
	const sessionID = "ses_lost_settle"
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	// The restart reads the session's row once before its call (storedFilter) and once more to
	// settle the call it made (settleFilter, fenceFilterWrite); the hold is armed after the first
	// read, so it holds the settle's.
	var held atomic.Pointer[testnats.HeldReplies]
	var armed atomic.Bool
	rig.deps.consumers.afterStoredRowRead = func() {
		if armed.CompareAndSwap(false, true) {
			held.Store(hold.Hold("$JS.API.DIRECT.GET.KV_" + bus.InterestBucket + "."))
		}
	}
	run := &relayRun{sessionID: sessionID, done: make(chan struct{}), resume: make(chan struct{}, 1)}
	type answer struct {
		pull    *bus.SessionPull
		outcome relayOutcome
	}
	answered := make(chan answer, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
		defer cancel()
		_, pull, outcome := relays.restart(ctx, run)
		answered <- answer{pull, outcome}
	}()
	for held.Load() == nil && testnats.Remaining(t) > 0 {
		time.Sleep(time.Millisecond)
	}
	if held.Load() == nil {
		t.Fatal("the restart read no row by the test's deadline")
	}
	select {
	case <-held.Load().Withheld():
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the restart's settle read no row by the test's deadline")
	}
	dropped := time.Now()
	hold.Cut()
	var got answer
	select {
	case got = <-answered:
	case <-time.After(testnats.Remaining(t)):
		t.Fatal("the restart whose settle lost its connection had not returned by the test's deadline")
	}
	if client.Connected() {
		t.Fatalf("the restart returned %s after the drop only once the listener was connected again", time.Since(dropped))
	}
	const settleLost, pullLost = "session relay lost its NATS connection to settle the session's filter", "session relay lost its NATS connection to pull the session's consumer"
	logs := rig.harness.logs.String()
	if got.pull != nil || got.outcome != relayLost || !errors.Is(run.lost, bus.ErrConnectionLost) ||
		!strings.Contains(logs, settleLost) || strings.Contains(logs, pullLost) {
		t.Fatalf("the restart returned outcome %d, pull %t, lost %v %s after the drop; want relayLost for the settle's loss (%q in the listener log, and not %q)",
			got.outcome, got.pull != nil, run.lost, time.Since(dropped), settleLost, pullLost)
	}
	t.Logf("the restart returned relayLost for its settle's loss %s after the drop, the listener still disconnected", time.Since(dropped))

	// Once the connection is back, the relay's next restart settles and pulls. The listener's own
	// client and the session consumers' client reconnect each on its own wait, so the row restarts
	// as the relay's loop does: again after each relayLost, once that request's connection is back.
	rig.deps.consumers.afterStoredRowRead = nil
	hold.Restore()
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	for {
		if !bus.Reconnected(ctx, run.lost) {
			t.Fatalf("the connection the restart lost (%v) was not back by the test's deadline", run.lost)
		}
		run.lost = nil
		_, pull, outcome := relays.restart(ctx, run)
		if pull != nil {
			pull.Stop()
			return
		}
		if outcome != relayLost {
			t.Fatalf("the restart once the connection was back did not pull the session's consumer (outcome %d)", outcome)
		}
	}
}
