package main

// The claim route's takeover pass runs on the pod that served the claim, which may be a standby,
// while the lease holder's sweep runs the passes of the same record (runTakenRoleDrain,
// keptSweepConfig). Two passes over one record store each copy once, under its message id
// (takenFrameMsgID), and the new holder receives each message once. And the claim route's pass is
// bounded by its handover window, not by the request: a pass the window cuts off stores nothing
// more, and the holder's sweep goes on from what it left.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

const (
	// servingHolder is the role's holder whose process stops, servingTaker the session that takes
	// the role from it and servingRole the role.
	servingHolder = "ses_serving_holder"
	servingTaker  = "ses_serving_taker"
	servingRole   = "serving-role"
)

// startServingTakeover has servingHolder hold servingRole and receive n role messages it answers
// no `done` to, stops its process, and has servingTaker claim the role through the listener's
// route, which writes the takeover record of servingHolder. It returns the taker's stand-in and the
// unread messages' event ids. The row's seams decide what the claim route's own pass does.
func startServingTakeover(t *testing.T, rig *copiesRig, n int) (*sessionStandIn, []string) {
	t.Helper()
	holder := rig.startHolder(t, servingHolder)
	rig.claim(t, servingHolder, servingRole, false)
	var unread []string
	for i := range n {
		unread = append(unread, rig.publish(t, servingRole, "", fmt.Sprintf("unread role message %d", i+1), servingHolder))
	}
	holder.collect(t, n, copiesWatchBound)
	awaitAckState(t, rig.consumers, servingHolder, n, 0)
	rig.crash(t, servingHolder, holder)
	taker := rig.startTaker(t, servingTaker)
	rig.claim(t, servingTaker, servingRole, false)
	return taker, unread
}

// servingRecords is every takeover record of servingRole the stream holds.
func servingRecords(t *testing.T, rig *copiesRig) []bus.StoredTakeoverRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	records, err := rig.harness.client.TakeoverRecords(ctx, servingRole)
	if err != nil {
		t.Fatalf("read the takeover records of %s: %v", servingRole, err)
	}
	return records
}

// servingRequireOnce fails the row for each of eventIDs the new holder did not receive exactly
// once, or whose copy the role's kept lane does not hold exactly once.
func servingRequireOnce(t *testing.T, rig *copiesRig, received copiesReceived, eventIDs []string) {
	t.Helper()
	for _, eventID := range eventIDs {
		if n := received[eventID]; n != 1 {
			t.Errorf("%s received event %s %d times; want exactly once", servingTaker, eventID, n)
		}
		if n := rig.keptCopiesOf(t, servingRole, eventID); n != 1 {
			t.Errorf("role %s's kept lane holds %d copies of %s; want exactly one", servingRole, n, eventID)
		}
	}
}

// The claim route's pass on a standby and the lease holder's sweep pass run over the same unread
// role messages at once, each pod with its own role guards and its own ledger boot, both passes
// held until each has reached its first copy: the role's kept lane holds one copy of each message
// and the new holder receives each exactly once. What makes the two passes one is the copy's own
// identity: its message id (takenFrameMsgID), which the stream's duplicate window keeps for 72
// hours, and its minted dedupe key (takenFrameDedupeKey), which names the forward to the holder.
func TestAClaimRoutePassOnAStandbyAndTheHoldersSweepPassGiveTheNewHolderEachMessageOnce(t *testing.T) {
	t.Parallel()
	rig := newCopiesRig(t)
	met := make(chan struct{})
	var arrivals sync.WaitGroup
	arrivals.Add(2)
	go func() {
		arrivals.Wait()
		close(met)
	}()
	// meet holds a pass at its first copy until the other pass has reached its own, or
	// copiesSignalBound has passed.
	meet := func(first *sync.Once) func(string, string, uint64) error {
		return func(string, string, uint64) error {
			first.Do(func() {
				arrivals.Done()
				copiesAwait(met)
			})
			return nil
		}
	}
	var holderFirst, standbyFirst sync.Once
	// Whichever pass walks the kept lane second finds it busy - the other walk's claim of a copy
	// outstanding, or the cursor taken from under it - and waits before it walks again, which
	// deployed is bus.KeptRoleClaimWindow, 60 s, since a claim may belong to a walk still
	// forwarding (handOverKeptRole). The walk holding the lane here ends each claim it writes
	// before it reads its next copy: it confirms the copy it forwarded, {kept: that copy,
	// claiming: 0}, or releases the claim of one it does not forward (walkKeptRole). Only a cursor
	// write that failed would leave a claim behind, and this row fails none, so that walk leaves no
	// claim outstanding when it ends. The walk after a short pause then finds the lane drained, or
	// goes on from where that walk is, as it would after the full window: the passes and the new
	// holder's copies are the same, a minute sooner. Each pass's fault set shortens that pause
	// (laneBusyPause): the standby's own, and the holder's, which the sweep's copy of its
	// configuration shares (keptSweepConfig). No other handover of the holder's listener runs in
	// this row: its claim route starts none, and its relay copies no frame of the role.
	const busyPause = 500 * time.Millisecond
	// The holder's own claim route starts no handover: the claim is served by the standby, whose
	// pass this row runs itself.
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
		relayCopy:         copiesRelayCopyHeld(servingRole),
		skipClaimHandover: func(string) bool { return true },
		beforeCopy:        meet(&holderFirst),
		laneBusyPause:     busyPause,
	})
	standby := *rig.deps.delivery
	standby.roleKeepDrains = newRoleKeepGuards()
	standby.ledger = testDeliveryLedger(t, rig.harness.client.Conn, "test-machine", "boot-standby")
	standby.takeoverFaults = &takeoverFaultHooks{}
	standby.takeoverFaults.set.Store(&takeoverFaultSet{
		relayCopy:     copiesRelayCopyHeld(servingRole),
		beforeCopy:    meet(&standbyFirst),
		laneBusyPause: busyPause,
	})
	taker, unread := startServingTakeover(t, rig, 3)
	records := servingRecords(t, rig)
	if len(records) != 1 {
		t.Fatalf("the stream holds %d takeover records of %s; want the claim's one", len(records), servingRole)
	}

	sweep := keptSweepConfig(*rig.deps.delivery, rig.relays)
	var passes sync.WaitGroup
	passes.Add(2)
	go func() {
		defer passes.Done()
		runTakenRoleDrain(standby, servingRole)
	}()
	go func() {
		defer passes.Done()
		release, held := sweep.roleKeepDrains.tryHold(servingRole)
		if !held {
			t.Errorf("the holder's sweep could not take role %s's guard", servingRole)
			arrivals.Done()
			return
		}
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
		defer cancel()
		handOverSweptLane(ctx, sweep, servingRole, records)
	}()
	passes.Wait()
	select {
	case <-met:
	default:
		t.Fatal("the two passes never both reached a copy, so they did not run over the messages at once")
	}

	received := rig.watch(t, taker, servingTaker, servingRole, unread...)
	servingRequireOnce(t, rig, received, unread)
}

// The claim route's pass runs on past the request that started it, under its handover window and
// not the request's bound (runTakenRoleDrain): a pass the window ends while it is still copying
// stores no copy after that, and its record stands as its last write left it. The holder's sweep
// then goes on from there, and the new holder receives each message exactly once. The window here is
// the claimHandoverWindow seam's, 2 s, where the deployed one is roleKeepHandoverWindow.
// It must not run in parallel: the pass's first copy must land inside its 2 s handover window.
func TestAClaimRoutePassCutByItsHandoverWindowLeavesTheRestToTheHoldersSweep(t *testing.T) {
	rig := newCopiesRig(t)
	const window = 2 * time.Second
	var claimPass atomic.Bool
	claimPass.Store(true)
	var copies atomic.Int32
	cut := make(chan struct{})
	setTakeoverFaults(t, rig.harness.config, takeoverFaultSet{
		relayCopy:           copiesRelayCopyHeld(servingRole),
		claimHandoverWindow: window,
		beforeCopy: func(string, string, uint64) error {
			if claimPass.Load() && copies.Add(1) == 2 {
				// The claim route's pass waits here past its window: its copy of the second
				// message starts after the window has ended.
				time.Sleep(window + time.Second)
				copiesSignal(cut)
			}
			return nil
		},
	})
	taker, unread := startServingTakeover(t, rig, 3)
	if !copiesAwait(cut) {
		t.Fatal("the claim route's pass never reached its second copy")
	}
	rig.awaitHandoverIdle(t, servingRole)
	claimPass.Store(false)

	t.Run("case: the claim route's pass stored no copy after its window ended", func(t *testing.T) {
		if n := rig.keptCopiesOf(t, servingRole, unread[0]); n != 1 {
			t.Errorf("role %s's kept lane holds %d copies of %s, which the pass copied inside its window; want 1", servingRole, n, unread[0])
		}
		for _, eventID := range unread[1:] {
			if n := rig.keptCopiesOf(t, servingRole, eventID); n != 0 {
				t.Errorf("role %s's kept lane holds %d copies of %s after the claim route's pass; want none: its window ended before that copy", servingRole, n, eventID)
			}
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	sweepOnce(ctx, keptSweepConfig(*rig.deps.delivery, rig.relays))
	received := rig.watch(t, taker, servingTaker, servingRole, unread...)
	t.Run("case: the holder's sweep went on from what the pass left, and the new holder received each message exactly once", func(t *testing.T) {
		servingRequireOnce(t, rig, received, unread)
	})
}
