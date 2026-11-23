package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A handover the sweep started with a role's takeover records, whose first try ends on the
// connection's loss, reads the records again under the guard before it runs again
// (handOverSweptLane). When that read fails other than by the loss, the handover keeps the records
// it already read and runs their pass again, and logs the failed read at WARN, as the takeover
// drain does. The first pass's read of the previous holder's floor answers the client's own loss
// (the floorRead seam answers what the client's LostNow says while the probe's seam reads a false
// "down"), so that try ends on the loss; the re-read of the records fails (the recordsRead seam);
// and the second pass's floor read is refused, ending that pass. The row counts the floor reads.
//
// The control is the re-read at abc56ca0c7, which dropped the read's error, so the records became
// none: the second try runs no takeover pass and logs no reason, and the row fails at its count of
// the floor reads.
func TestASweptHandoverWhoseRecordsReadFailsKeepsTheRecordsItRead(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const (
		role     = "kept-sweep-records-reread"
		holder   = "ses_kept_sweep_records_reread"
		previous = "ses_kept_sweep_records_previous"
	)
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	var floorReads, recordsReads atomic.Int32
	var down atomic.Bool
	restore := harness.client.ProbeReadsDownForTest(down.Load)
	defer restore()
	setTakeoverFaults(t, cfg, takeoverFaultSet{
		floorRead: func(p string) error {
			if p != previous {
				return nil
			}
			if floorReads.Add(1) == 1 {
				down.Store(true)
				lost := harness.client.LostNow()
				down.Store(false)
				if lost == nil {
					t.Error("the client's LostNow answered no loss while the probe read down")
				}
				return lost
			}
			return errors.New("the second pass's floor read is refused, which ends it")
		},
		recordsRead: func(r string) error {
			if r != role {
				return nil
			}
			recordsReads.Add(1)
			return errors.New("the takeover records could not be read")
		},
	})
	records := []bus.StoredTakeoverRecord{{
		Subject: bus.TakeoverSubject(role, previous, "attempt-1"),
		Record:  bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: time.Now().UnixMilli()},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	release, taken := cfg.roleKeepDrains.tryHold(role)
	if !taken {
		t.Fatal("the row could not take the role's guard")
	}
	handOverSweptLane(ctx, cfg, role, records, release)

	if n := recordsReads.Load(); n != 1 {
		t.Fatalf("the handover read the role's takeover records %d times after the loss, want once", n)
	}
	if n := floorReads.Load(); n != 2 {
		t.Fatalf("the takeover pass read the previous holder's floor %d times, want 2: the try after the loss ran no pass with the records it had read; log:\n%s", n, harness.logs.String())
	}
	if logs := harness.logs.String(); !strings.Contains(logs, "listener takeover copy could not read the role's takeover records") {
		t.Fatalf("the failed read of the records was not logged:\n%s", logs)
	}
}

// The takeover drain the claim route starts (runTakenRoleDrain) recovers from a loss at each of
// the two reads this merge makes under the role's guard: a read of the role's takeover records that
// answers the loss gives the guard back, waits for the connection and reads again, and a takeover
// pass whose read answers the loss ends that try, which runs again (Merge order item 7's first two
// changes). A takeover record is stored for the role; the first read of the records and the first
// pass's read of the previous holder's floor each answer the client's own loss (the seams answer
// what the client's LostNow says while the probe's seam reads a false "down"), and the second floor
// read is refused, which ends that pass. The row counts the reads.
//
// The controls are either loss read as the end of the drain: with the records read's loss handed
// over with no records, or the pass's loss logged and passed over, the second floor read never
// comes, and the row fails at its count of the floor reads.
func TestATakeoverDrainRecoversFromALossAtEachReadUnderTheGuard(t *testing.T) {
	harness := newListenerDeliveryHarness(t)
	cfg := harness.config
	const (
		role     = "kept-taken-drain-losses"
		holder   = "ses_kept_taken_drain_losses"
		previous = "ses_kept_taken_drain_previous"
	)
	claimRoleWith(t, harness, holder, role, session.SessionEntry{MachineID: "test-machine", SelfSubscribed: true})
	ctx, cancel := context.WithTimeout(context.Background(), testnats.Remaining(t))
	defer cancel()
	record := bus.TakeoverRecord{Role: role, Previous: previous, ClaimedAt: time.Now().UnixMilli()}
	if err := writeTakeoverRecord(ctx, cfg, bus.TakeoverSubject(role, previous, "attempt-1"), record); err != nil {
		t.Fatalf("store the takeover record: %v", err)
	}
	var down atomic.Bool
	restore := harness.client.ProbeReadsDownForTest(down.Load)
	defer restore()
	lost := func() error {
		down.Store(true)
		defer down.Store(false)
		err := harness.client.LostNow()
		if err == nil {
			t.Error("the client's LostNow answered no loss while the probe read down")
		}
		return err
	}
	var floorReads, recordsReads atomic.Int32
	setTakeoverFaults(t, cfg, takeoverFaultSet{
		recordsRead: func(r string) error {
			if r == role && recordsReads.Add(1) == 1 {
				return lost()
			}
			return nil
		},
		floorRead: func(p string) error {
			if p != previous {
				return nil
			}
			if floorReads.Add(1) == 1 {
				return lost()
			}
			return errors.New("the second pass's floor read is refused, which ends it")
		},
	})

	runTakenRoleDrain(ctx, cfg, role)

	if n := recordsReads.Load(); n != 3 {
		t.Fatalf("the drain read the role's takeover records %d times, want 3: once lost, once before the lost pass, once after it; log:\n%s", n, harness.logs.String())
	}
	if n := floorReads.Load(); n != 2 {
		t.Fatalf("the takeover pass read the previous holder's floor %d times, want 2: once lost, once after the retry; log:\n%s", n, harness.logs.String())
	}
	if logs := harness.logs.String(); strings.Contains(logs, "listener takeover copy could not read the role's takeover records") {
		t.Fatalf("the drain logged the records read's loss as a failed read, where it retries:\n%s", logs)
	}
}
