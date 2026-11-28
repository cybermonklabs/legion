package main

import (
	"context"
	"testing"
)

// stale_binding is set by a holder that subscribed beside a push binding it did not wait out;
// every renewal, the release and the next take-over carry it, and that take-over's term must
// settle though the record was released; a holder that finds the durable not push-bound clears
// it. A call for a term that is no longer current changes nothing.
func TestLeaseStaleBindingIsSetCarriedAndCleared(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	a.SetStaleBinding(a.Status().Term, true)
	for renewal := range 2 {
		clock.advance(leaseRenewEvery)
		a.step()
		if record, _ := readLease(t, kv); !record.StaleBinding || record.Unsettled {
			t.Fatalf("record after renewal %d = %+v, want stale_binding set and not unsettled", renewal+1, record.leaseFields)
		}
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.StaleBinding || record.Unsettled {
		t.Fatalf("released record = %+v, want released carrying stale_binding", record.leaseFields)
	}

	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("status = %+v, want pod-b holding from a released record, a term that must settle for its stale binding", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.StaleBinding || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's carrying stale_binding, unsettled", record.leaseFields)
	}
	b.SetStaleBinding(status.Term-1, false)
	b.Settled(status.Term - 1)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); !record.StaleBinding || !record.Unsettled {
		t.Fatalf("record after calls for an earlier term = %+v, want both flags kept", record.leaseFields)
	}
	b.SetStaleBinding(status.Term, false)
	b.Settled(status.Term)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); record.StaleBinding || record.Unsettled {
		t.Fatalf("record after the durable read not push-bound and the hold ended = %+v, want both flags cleared", record.leaseFields)
	}
}

// unsettled is written from the acquiring write of a term that must settle. A release before that
// term's settle hold ends keeps it, so the next holder's term must settle too, though it takes a
// released record; once the hold ends it is cleared, and a release then, with nothing handed back
// (no Unsettle), hands over a term that need not settle.
func TestLeaseUnsettledIsHandedOnByAReleaseBeforeTheHoldEnds(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-dead","boot":"0011223344556677","released":false}`)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	c := testLease(kv, "pod-c", productionLeaseConfig(), clock.now)
	a.step()
	clock.advance(leaseTTL)
	a.step()
	if status := a.Status(); !status.Holding || status.How != leaseTakenExpired || !status.Settle {
		t.Fatalf("pod-a status = %+v, want held over an expired record, settling", status)
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled || record.StaleBinding {
		t.Fatalf("record released mid-hold = %+v, want released, still unsettled", record.leaseFields)
	}

	b.step()
	status := b.Status()
	if !status.Holding || status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling for the unsettled term before it", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.Unsettled {
		t.Fatalf("record = %+v, want pod-b's, unsettled", record.leaseFields)
	}
	b.Settled(status.Term)
	clock.advance(leaseRenewEvery)
	b.step()
	if record, _ := readLease(t, kv); record.Unsettled {
		t.Fatalf("record after pod-b's hold ended = %+v, want unsettled cleared", record.leaseFields)
	}
	if err := b.Release(context.Background()); err != nil {
		t.Fatalf("release pod-b: %v", err)
	}

	c.step()
	if status := c.Status(); !status.Holding || status.How != leaseTakenReleased || status.Settle {
		t.Fatalf("pod-c status = %+v, want held from a released record, not settling", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-c" || record.Unsettled || record.StaleBinding {
		t.Fatalf("record = %+v, want pod-c's with neither flag", record.leaseFields)
	}
}

// A term stopped while its renewals have lapsed releases the record unsettled, whatever its own
// flags stand at: the lapse dropped what it held and what the durable pushed it without an ack,
// and those come back only at their ack wait, so the next holder must settle as it would have over
// the expired record this release replaces. A release that wrote the lapsed term's own clean flags
// would let the standby that takes the record deliver new messages at once, ahead of the dropped
// ones.
func TestLeaseAStopDuringALapseReleasesTheRecordUnsettled(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	status := a.Status()
	if !status.Holding {
		t.Fatalf("pod-a status = %+v, want held from an absent record", status)
	}
	// Its settle hold ended: the term holds nothing back.
	a.Settled(status.Term)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.Unsettled {
		t.Fatalf("record of a settled term = %+v, want not unsettled", record.leaseFields)
	}

	// The lease connection stalls past the term's validity: the renewals lapse, and SIGTERM
	// arrives before one resumes.
	clock.advance(leaseTTL)
	if a.Holding() {
		t.Fatal("pod-a still holds the lease past its validity")
	}
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled {
		t.Fatalf("record released during a lapse = %+v, want released, unsettled", record.leaseFields)
	}

	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling", next)
	}
}

// A graceful stop that hands messages back marks its term unsettled (Unsettle), so its release
// carries unsettled and the next holder's term settles; no renewal before it sets the flag. A call
// for a term that is no longer current changes nothing.
func TestLeaseAGracefulReleaseCarriesUnsettled(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":true}`)
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	a.step()
	status := a.Status()
	if !status.Holding || status.How != leaseTakenReleased || status.Settle {
		t.Fatalf("pod-a status = %+v, want held from a released record, not settling", status)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-a" || record.Unsettled {
		t.Fatalf("record after pod-a's acquisition = %+v, want pod-a's, not unsettled", record.leaseFields)
	}
	for renewal := range 2 {
		clock.advance(leaseRenewEvery)
		a.step()
		if record, _ := readLease(t, kv); record.Unsettled {
			t.Fatalf("record after renewal %d = %+v, want not unsettled", renewal+1, record.leaseFields)
		}
	}
	a.Unsettle(status.Term - 1)
	clock.advance(leaseRenewEvery)
	a.step()
	if record, _ := readLease(t, kv); record.Unsettled {
		t.Fatalf("record after Unsettle for an earlier term = %+v, want not unsettled", record.leaseFields)
	}

	a.Unsettle(status.Term)
	if err := a.Release(context.Background()); err != nil {
		t.Fatalf("release pod-a: %v", err)
	}
	if record, _ := readLease(t, kv); !record.Released || !record.Unsettled {
		t.Fatalf("record after the graceful release = %+v, want released, unsettled", record.leaseFields)
	}

	b.step()
	next := b.Status()
	if !next.Holding || next.How != leaseTakenReleased || !next.Settle {
		t.Fatalf("pod-b status = %+v, want held from a released record, settling for the messages handed back", next)
	}
	if record, _ := readLease(t, kv); record.Holder != "pod-b" || !record.Unsettled {
		t.Fatalf("record after pod-b's acquisition = %+v, want pod-b's, unsettled", record.leaseFields)
	}
}
