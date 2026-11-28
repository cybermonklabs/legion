package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// receiveSignal is a receiver's signal the test moves by hand (lease.SetReceiveSignal): the lease
// reads it once a round, from its loop, so an atomic is all it needs.
type receiveSignal struct {
	up atomic.Bool
}

func newReceiveSignal() *receiveSignal {
	signal := &receiveSignal{}
	signal.up.Store(true)
	return signal
}

func (s *receiveSignal) get() bool { return s.up.Load() }
func (s *receiveSignal) down()     { s.up.Store(false) }
func (s *receiveSignal) backUp()   { s.up.Store(true) }

// A holder whose receive connection stays down releases the record once it has been down for the
// bound, and not before: its renewals land all the while, so nothing but the signal ends the
// term. The record it releases is unsettled and carries a stale binding - the dead connection's
// subscription can still be bound at the server - so the pod that takes it settles beside that
// binding. The pod that released it takes it back from no poll while the connection is down, and
// stands by again once it is up.
func TestLeaseHolderReleasesTheRecordOnceItsReceiveConnectionHasBeenDownForTheBound(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.SetReceiveSignal(signal.get)
	a.step()
	if !a.Holding() {
		t.Fatal("pod-a did not take the lease")
	}
	drainChanges(a)

	signal.down()
	for down := time.Duration(0); down < leaseReceiveLost; down += leaseRenewEvery {
		if wait := a.step(); wait > leaseReceiveLost-down {
			t.Fatalf("the holder waits %s with its receive connection down for %s, past the bound %s", wait, down, leaseReceiveLost)
		}
		if !a.Holding() {
			t.Fatalf("the holder let the lease go %s after its receive connection went down, before the bound %s", down, leaseReceiveLost)
		}
		if record, _ := readLease(t, kv); record.Released {
			t.Fatalf("the record is released %s after the receive connection went down, before the bound %s", down, leaseReceiveLost)
		}
		clock.advance(leaseRenewEvery)
	}

	wait := a.step()
	if a.Holding() {
		t.Fatalf("the holder still holds the lease %s after its receive connection went down", leaseReceiveLost)
	}
	if wait != leaseStandbyPoll {
		t.Fatalf("the release waits %s, want the standby poll %s", wait, leaseStandbyPoll)
	}
	if !drainChanges(a) {
		t.Fatal("the release signalled no change, so the receive loop would not end its term")
	}
	record, _ := readLease(t, kv)
	if !record.Released || record.Holder != "pod-a" || !record.Unsettled || !record.StaleBinding {
		t.Fatalf("record = %+v, want pod-a's released record, unsettled, with a stale binding", record.leaseFields)
	}

	// No bounce: this pod takes no record back while its receive connection is down, however long
	// the released record lies there.
	for waited := time.Duration(0); waited < 2*leaseTTL; waited += leaseStandbyPoll {
		a.step()
		if a.Holding() {
			t.Fatalf("the pod took its own released record back %s after the release, with its receive connection still down", waited)
		}
		clock.advance(leaseStandbyPoll)
	}

	signal.backUp()
	a.step()
	if !a.Holding() {
		t.Fatal("the pod took no record once its receive connection was back")
	}
	if status := a.Status(); status.How != leaseTakenReleased || !status.Settle {
		t.Fatalf("status = %+v, want a term taken from the released record that must settle", status)
	}
}

// releaseGateKV is the listener state bucket as a holder sees it when the bucket's answer to its
// release is slow or lost: an Update that writes a released record says so on reached and waits
// until the row closes gate, or, when fail is set, fails at once, as a release the bucket does not
// take. Every other call passes, and updates counts every Update the lease sends.
type releaseGateKV struct {
	natsgo.KeyValue
	fail    bool
	reached chan struct{}
	gate    chan struct{}
	updates atomic.Int32
}

func (k *releaseGateKV) Update(key string, value []byte, last uint64) (uint64, error) {
	k.updates.Add(1)
	if record, err := decodeLeaseRecord(value); err == nil && record.Released {
		if k.fail {
			return 0, natsgo.ErrTimeout
		}
		select {
		case k.reached <- struct{}{}:
		default:
		}
		<-k.gate
	}
	return k.KeyValue.Update(key, value, last)
}

// receiveLostHolder runs pod-a's lease over a fresh bucket through gate until its receive
// connection has been down for the bound, renewing all the while, so that its next step releases
// the record (lease.releaseReceiveLost). Its validity then runs a further 3 s on the fake clock: a
// pod that did not clear its hold before the release's write would still count itself the holder.
// The connection stays down, so the pod takes no record afterwards. It returns the lease, the
// clock and the bucket as every other pod sees it.
func receiveLostHolder(t *testing.T, gate *releaseGateKV) (*lease, *fakeLeaseClock, bus.KeyValue) {
	t.Helper()
	kv := leaseBucket(t)
	gate.KeyValue = kv.Legacy()
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	a := testLease(bus.NewKeyValue(gate, kv.WatchHandle(), kv.Streams()), "pod-a", productionLeaseConfig(), clock.now)
	a.SetReceiveSignal(signal.get)
	a.step()
	drainChanges(a)
	signal.down()
	for down := time.Duration(0); down < leaseReceiveLost; down += leaseRenewEvery {
		a.step()
		clock.advance(leaseRenewEvery)
	}
	if !a.Holding() {
		t.Fatal("pod-a lost the lease before its receive connection had been down for the bound")
	}
	if drainChanges(a) {
		t.Fatal("pod-a signalled a change before its release")
	}
	if record, _ := readLease(t, kv); record.Released || record.Holder != "pod-a" {
		t.Fatalf("record = %+v before the release, want pod-a's unreleased record", record.leaseFields)
	}
	return a, clock, kv
}

// A holder whose receive connection has been down for the bound stops holding before it writes
// the release (lease.releaseReceiveLost), as letGo does. A release that lands while its reply is
// slow or lost hands the record to a standby that can subscribe at once, so a pod that counted
// itself the holder until the reply came would deliver beside that standby once its receive
// connection came back in place. A release the bucket does not take ends the hold all the same,
// with no renewal after it: the record stays unreleased, and a standby takes it as an expired one
// once the ttl has passed, and settles. Its negative control clears the hold only after the write
// returns and renews after a failed one: the first case then fails on Holding reading true while
// the write is held, and the second on the hold kept after the failed release.
func TestAReceiveLostReleaseStandsThePodDownBeforeItWritesTheRelease(t *testing.T) {
	t.Parallel()
	t.Run("held in flight", func(t *testing.T) {
		t.Parallel()
		gate := &releaseGateKV{reached: make(chan struct{}, 1), gate: make(chan struct{})}
		a, _, kv := receiveLostHolder(t, gate)
		_, renewed := readLease(t, kv)
		var open sync.Once
		letGo := func() { open.Do(func() { close(gate.gate) }) }
		var wait time.Duration
		done := make(chan struct{})
		go func() {
			defer close(done)
			wait = a.step()
		}()
		t.Cleanup(func() {
			letGo()
			<-done
		})

		select {
		case <-gate.reached:
		case <-done:
			t.Fatal("pod-a's step past the bound wrote no released record")
		case <-time.After(10 * time.Second):
			t.Fatal("pod-a's release did not reach the bucket within 10s")
		}
		if a.Holding() {
			t.Fatal("pod-a counts itself the holder while its release is in flight: a release that lands with its reply slow or lost lets a standby subscribe while this pod still holds")
		}
		letGo()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("pod-a's step did not return within 10s of its release landing")
		}
		if wait != leaseStandbyPoll || a.Holding() {
			t.Fatalf("after the release pod-a waits %s holding %v, want the standby poll %s and no hold", wait, a.Holding(), leaseStandbyPoll)
		}
		if !drainChanges(a) {
			t.Fatal("the release signalled no change, so the receive loop would not end its term")
		}
		record, entry := readLease(t, kv)
		if !record.Released || record.Holder != "pod-a" || !record.Unsettled || !record.StaleBinding || entry.Revision() == renewed.Revision() {
			t.Fatalf("record = %+v at revision %d, want pod-a's released record over revision %d, unsettled, with a stale binding",
				record.leaseFields, entry.Revision(), renewed.Revision())
		}
	})
	t.Run("not taken by the bucket", func(t *testing.T) {
		t.Parallel()
		gate := &releaseGateKV{fail: true, reached: make(chan struct{}, 1), gate: make(chan struct{})}
		a, clock, kv := receiveLostHolder(t, gate)
		_, renewed := readLease(t, kv)
		before := gate.updates.Load()

		if wait := a.step(); wait != leaseStandbyPoll || a.Holding() {
			t.Fatalf("after a release the bucket did not take pod-a waits %s holding %v, want the standby poll %s and no hold", wait, a.Holding(), leaseStandbyPoll)
		}
		if !drainChanges(a) {
			t.Fatal("the failed release signalled no change, so the receive loop would not end its term")
		}
		// Its receive connection stays down, so from here on it reads the record and writes none.
		for waited := time.Duration(0); waited < leaseTTL; waited += leaseStandbyPoll {
			clock.advance(leaseStandbyPoll)
			a.step()
			if a.Holding() {
				t.Fatalf("pod-a holds the lease %s after a release the bucket did not take", waited+leaseStandbyPoll)
			}
		}
		if writes := gate.updates.Load() - before; writes != 1 {
			t.Fatalf("pod-a sent %d writes of the record from its release on, want the release alone and no renewal", writes)
		}
		record, entry := readLease(t, kv)
		if record.Released || record.Holder != "pod-a" || entry.Revision() != renewed.Revision() {
			t.Fatalf("record = %+v at revision %d, want pod-a's unreleased record at its last renewal's revision %d",
				record.leaseFields, entry.Revision(), renewed.Revision())
		}

		b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
		b.step()
		clock.advance(leaseTTL)
		b.step()
		if status := b.Status(); !status.Holding || status.How != leaseTakenExpired || !status.Settle {
			t.Fatalf("pod-b's status = %+v a ttl after it first read the record, want it holding over the expired record, settling", status)
		}
	})
}

// A receive connection back inside the bound costs the holder nothing: the same term holds the
// lease on, the record is never released, and the next cut is timed from itself.
func TestLeaseHolderKeepsTheLeaseWhenItsReceiveConnectionComesBackInsideTheBound(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	a := testLease(kv, "pod-a", productionLeaseConfig(), clock.now)
	a.SetReceiveSignal(signal.get)
	a.step()
	term := a.Status().Term

	signal.down()
	a.step()
	clock.advance(leaseReceiveLost - leaseRenewEvery)
	a.step()
	signal.backUp()
	clock.advance(leaseRenewEvery)

	// Past the bound, counted from the moment the connection went down: a lease that timed the
	// reconnect out would release here.
	for waited := time.Duration(0); waited < 2*leaseReceiveLost; waited += leaseRenewEvery {
		a.step()
		if !a.Holding() {
			t.Fatalf("the holder let the lease go %s after its receive connection came back", waited)
		}
		clock.advance(leaseRenewEvery)
	}
	if status := a.Status(); status.Term != term {
		t.Fatalf("the holder is in term %d after the reconnect, want the term it held across it, %d", status.Term, term)
	}
	if record, _ := readLease(t, kv); record.Released || record.Holder != "pod-a" {
		t.Fatalf("record = %+v, want pod-a's unreleased record", record.leaseFields)
	}
}

// A standby whose receive connection is down takes no record: not an absent one, and not one that
// has lain unchanged for the ttl. It reads the record all the while, so its /healthz still names
// the holder, and takes one again once the connection is back.
func TestLeaseStandbyTakesNoRecordWhileItsReceiveConnectionIsDown(t *testing.T) {
	t.Parallel()
	kv := leaseBucket(t)
	clock := newFakeLeaseClock()
	signal := newReceiveSignal()
	b := testLease(kv, "pod-b", productionLeaseConfig(), clock.now)
	b.SetReceiveSignal(signal.get)

	signal.down()
	if wait := b.step(); wait != leaseStandbyPoll || b.Holding() {
		t.Fatalf("the standby's poll of an absent record waits %s holding %v, want the poll interval and no hold", wait, b.Holding())
	}
	if _, err := kv.Get(leaseTestKey); err == nil {
		t.Fatal("the standby created a record while its receive connection was down")
	}

	putLease(t, kv, `{"holder":"pod-gone","boot":"0011223344556677","released":false}`)
	for waited := time.Duration(0); waited < 2*leaseTTL; waited += leaseStandbyPoll {
		b.step()
		if b.Holding() {
			t.Fatalf("the standby took the expired record %s on, with its receive connection down", waited)
		}
		clock.advance(leaseStandbyPoll)
	}
	if active := b.Status().ActiveInstance; active != "pod-gone" {
		t.Fatalf("the standby names active instance %q, want the record's holder pod-gone: it reads the record while it takes none", active)
	}

	signal.backUp()
	firstSeen := clock.now()
	for clock.now().Sub(firstSeen) < leaseTTL {
		b.step()
		if b.Holding() {
			t.Fatalf("the standby took the record %s after its receive connection came back, before the ttl %s", clock.now().Sub(firstSeen), leaseTTL)
		}
		clock.advance(leaseStandbyPoll)
	}
	b.step()
	if status := b.Status(); !status.Holding || status.How != leaseTakenExpired {
		t.Fatalf("status = %+v, want pod-b holding over the expired record once its receive connection is back", status)
	}
}
