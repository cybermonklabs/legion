package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
)

// The delivery lease's timing. Every pod of one machine runs a lease over one record; the pod
// holding it is the machine's one deliverer and every other pod is a standby. The holder renews
// every leaseRenewEvery and counts itself the holder until the start of its last successful
// renewal + leaseTTL - leaseMargin, on its monotonic clock. A standby reads the record every
// leaseStandbyPoll and takes it once the same revision has stayed unchanged for a full leaseTTL
// on its own monotonic clock, measured from when it first saw that revision. No wall clocks are
// compared: the holder's validity ends at its renewal's start + 4 s, and a standby takes no
// earlier than that write + 5 s, which is safe while the two pods' clock rates differ by less
// than 20 %. Renewing is not the whole of holding: a holder whose receive connection has been
// down for leaseReceiveLost releases the record however well its own renewals land, and a
// standby takes none while its receive connection is down (lease.receiveDown).
const (
	leaseRenewEvery  = time.Second
	leaseTTL         = 5 * time.Second
	leaseMargin      = time.Second
	leaseStandbyPoll = 500 * time.Millisecond
)

// leaseReceiveLost is how long this pod's receive connection may be down before the holder stops
// renewing and releases the record (lease.releaseReceiveLost): a pod that is not receiving the
// durable delivers nothing, however well its own connection to the lease bucket renews.
//
// 3 s is chosen between the two bounds it sits in. Below it: nats.go notices a silent cut in two
// to three seconds (bus.WithSilentCutDetection) and reconnects about a second after that, a
// second attempt about a second later again, so a link that comes back keeps the lease and only
// one that stays down loses it. Above it: the holder reads the signal once a renewEvery, the
// standby takes the released record at its next poll (leaseStandbyPoll) and then holds its
// settle cap (7 s), so a message published as the connection was cut reaches its session about
// 14.5 s after the cut at worst (3 s detection, up to 1 s before the signal is read down, this
// bound, the 0.5 s poll and the 7 s hold, plus the subscribe and the delivery), inside the 15 s
// a hard take-over is bounded at.
const leaseReceiveLost = 3 * time.Second

// leaseConfig carries the lease's timing, so a test of this package shortens it;
// productionLeaseConfig is the only value the listener runs with.
type leaseConfig struct {
	renewEvery  time.Duration
	ttl         time.Duration
	margin      time.Duration
	standbyPoll time.Duration
	// receiveLost is how long the holder's receive connection may be down before it releases the
	// record (leaseReceiveLost).
	receiveLost time.Duration
}

func productionLeaseConfig() leaseConfig {
	return leaseConfig{
		renewEvery:  leaseRenewEvery,
		ttl:         leaseTTL,
		margin:      leaseMargin,
		standbyPoll: leaseStandbyPoll,
		receiveLost: leaseReceiveLost,
	}
}

// leaseAcquisition is how a pod took the lease: what the record was when it wrote over it. The
// receive loop acts on it through leaseStatus.Settle. A term taken over an expired record that was
// not released (the holder was killed or lost its node, or this pod's own renewals lapsed and then
// resumed) must settle: the receive loop subscribes at once, beside any push binding, which it
// counts as the dead holder's, and holds what it receives until every message the durable has
// outstanding is in its hands, then delivers it in stream order, so the messages the old holder
// had in hand go before newer ones. So must a term taken from an absent or released record that
// carries stale_binding or unsettled. A term taken from an absent or released record with neither
// flag subscribes and delivers at once, and counts a push-bound durable as a pre-lease build's.
type leaseAcquisition int

const (
	leaseTakenAbsent leaseAcquisition = iota + 1
	leaseTakenReleased
	leaseTakenExpired
)

func (a leaseAcquisition) String() string {
	switch a {
	case leaseTakenAbsent:
		return "absent"
	case leaseTakenReleased:
		return "released"
	case leaseTakenExpired:
		return "expired"
	default:
		return "none"
	}
}

// leaseStatus is a lease at one instant.
type leaseStatus struct {
	// Holding is whether this process holds the lease now: it holds the record and is inside its
	// validity on the monotonic clock.
	Holding bool
	// Term counts this process's acquisitions, a resume after its renewals lapsed included, so a
	// term the receive loop subscribed for differs from a later one even when no change between
	// them reached it.
	Term uint64
	// How is how Term's acquisition was taken; zero before the first.
	How leaseAcquisition
	// ActiveInstance is the holder the record names as this process last wrote or read it, empty
	// when there is no record or it is released: a standby's /healthz names it.
	ActiveInstance string
	// ServiceSince is the record's service_since as last written or read, zero when no record
	// carried one.
	ServiceSince time.Time
	// Settle is whether Term must settle before it delivers (leaseAcquisition): it was taken over
	// an expired record, resumed after a lapse, or taken from a record carrying stale_binding or
	// unsettled.
	Settle bool
	// Resumed is whether Term is this process's own record resumed after its renewals lapsed (the
	// lapse branch of renew), rather than a record another pod wrote: no other pod held the lease
	// between the term before it and this one. Only the lease knows that, and the receive loop
	// needs it to tell its own resume from a take-over over a dead holder's record, both of which
	// How reports as expired.
	Resumed bool
}

// lease is one pod's view of its machine's delivery lease, the record lease.<durable> in the
// listener state bucket. Start runs it; Holding and Status read it at any time; Changes signals
// each acquisition and each loss; Release gives it up on a graceful stop.
type lease struct {
	kv       bus.KeyValue
	key      string
	instance string
	boot     string
	cfg      leaseConfig
	now      func() time.Time
	logger   *logging.Logger
	changes  chan struct{}
	// renewals asks the running loop to give the lease up now rather than at its next turn
	// (GiveUp).
	renewals chan struct{}

	// The loop's own state, read and written only by step (and by Release once the loop ended):
	// the revision a standby is timing and when it first saw it, whether the holder's lapse has
	// been signalled, whether a streak of failed bucket calls has been logged, the revision
	// of the record this process last released on giving the lease up and when (GiveUp), and
	// when this pod's receive connection went down, zero while it is up (receiveDown).
	seenRev          uint64
	seenAt           time.Time
	lapseSignalled   bool
	failing          bool
	givenUpRev       uint64
	givenUpAt        time.Time
	receiveDownSince time.Time

	mu             sync.Mutex
	held           bool
	rev            uint64
	validUntil     time.Time
	term           uint64
	how            leaseAcquisition
	settle         bool
	resumed        bool
	record         leaseRecord
	deliverSubject string
	// staleBinding and unsettled are the holder's stale_binding and unsettled, written into the
	// record from the next renewal or release on (SetStaleBinding, Settled, Unsettle).
	staleBinding bool
	unsettled    bool
	// giveUpTerm is the term GiveUp asked the loop to give up.
	giveUpTerm uint64
	// receiveUp is the receiver's signal, nil until it is wired (SetReceiveSignal).
	receiveUp func() bool
	stop      context.CancelFunc
	done      chan struct{}
}

// newLease makes machineID's lease over kv for this process, instance, with timing cfg and now
// as its clock. now must carry a monotonic reading (time.Now in production): the lease only
// subtracts its readings and never compares them with another pod's.
func newLease(kv bus.KeyValue, machineID, instance string, cfg leaseConfig, now func() time.Time, logger *logging.Logger) *lease {
	var boot [8]byte
	_, _ = rand.Read(boot[:]) // crypto/rand.Read never returns an error
	return &lease{
		kv:       kv,
		key:      bus.ListenerLeaseKey(machineID),
		instance: instance,
		boot:     hex.EncodeToString(boot[:]),
		cfg:      cfg,
		now:      now,
		logger:   logger,
		changes:  make(chan struct{}, 1),
		renewals: make(chan struct{}, 1),
	}
}

// SetReceiveSignal gives the lease the one signal it judges this pod's receiving by: up reports
// whether this pod's receive connection is up now, which only the receive loop knows
// (durableReceiver.receiveUp). The lease reads nothing of the receiver itself; a lease with no
// signal - a test's, or one whose receiver is not wired yet - is never down. It is set once,
// before Start.
func (l *lease) SetReceiveSignal(up func() bool) {
	l.mu.Lock()
	l.receiveUp = up
	l.mu.Unlock()
}

// Start runs the lease until ctx ends or Release stops it: a standby polls and takes the record,
// a holder renews it. Start is called once.
func (l *lease) Start(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	l.mu.Lock()
	l.stop, l.done = stop, done
	l.mu.Unlock()
	go func() {
		defer close(done)
		for {
			timer := time.NewTimer(l.step())
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-l.renewals:
				timer.Stop()
			}
		}
	}()
}

// Changes signals that the lease was acquired or lost. It holds at most one signal, so changes
// that come faster than the reader collapse into one: the reader reads Status after each signal
// and acts on the state there (a Term that differs from the one it acted on is a new
// acquisition). A loss is signalled when the holder's validity runs out without a renewal (as
// soon as the loop next wakes, which is no later than the validity's end unless a bucket call is
// in flight then), when a renewal finds another pod's record, and on Release. Holding is exact at
// every instant and is what gates each message.
func (l *lease) Changes() <-chan struct{} { return l.changes }

// Holding reports whether this process holds the lease now.
func (l *lease) Holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holdingLocked()
}

func (l *lease) holdingLocked() bool {
	return l.held && l.now().Before(l.validUntil)
}

// Status reads the lease now.
func (l *lease) Status() leaseStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := leaseStatus{
		Holding:      l.holdingLocked(),
		Term:         l.term,
		How:          l.how,
		ServiceSince: l.record.ServiceSince,
		Settle:       l.settle,
		Resumed:      l.resumed,
	}
	if !l.record.Released {
		status.ActiveInstance = l.record.Holder
	}
	return status
}

// SetDeliverSubject records the durable's deliver subject this process receives on, written
// into the record from the next renewal or acquisition on.
func (l *lease) SetDeliverSubject(subject string) {
	l.mu.Lock()
	l.deliverSubject = subject
	l.mu.Unlock()
}

// SetStaleBinding records stale_binding for term (leaseFields): whether its holder subscribed
// while the durable was push-bound by a binding it did not wait out, written into the record from
// the next renewal or release on. A term that is no longer current changes nothing.
func (l *lease) SetStaleBinding(term uint64, stale bool) {
	l.mu.Lock()
	if l.term == term {
		l.staleBinding = stale
	}
	l.mu.Unlock()
}

// Settled records that term's settle hold ended: every message the previous holder can still have
// outstanding is in this one's hands, so it clears unsettled from the next renewal or release on.
// A term that is no longer current changes nothing.
func (l *lease) Settled(term uint64) {
	l.mu.Lock()
	if l.term == term {
		l.unsettled = false
	}
	l.mu.Unlock()
}

// Unsettle records that term's holder handed messages back on a graceful stop, setting unsettled
// from the next renewal or release on, so the next holder settles. A term that is no longer
// current changes nothing.
func (l *lease) Unsettle(term uint64) {
	l.mu.Lock()
	if l.term == term {
		l.unsettled = true
	}
	l.mu.Unlock()
}

// GiveUp has the running loop release term's record, as Release does, and go on as a standby, so
// another pod takes the lease at its next poll: the receive loop gives the lease up when it cannot
// prepare or subscribe to the durable (durableReceiver.abandon). The loop does not take back the
// record it released for two standby polls, time for every other running standby to read and
// take it; a pod that runs alone takes it back after that and tries again. A term that is no
// longer current, or no longer held, changes nothing.
func (l *lease) GiveUp(term uint64) {
	l.mu.Lock()
	set := l.held && l.term == term
	if set {
		l.giveUpTerm = term
	}
	l.mu.Unlock()
	if set {
		select {
		case l.renewals <- struct{}{}:
		default:
		}
	}
}

// Release stops the lease and, when this process holds the record, marks it released at its
// revision, keeping service_since and stale_binding as they stand and writing unsettled as the
// term stands, so a standby takes it at its next poll with no TTL wait, and settles in its turn
// when this term had not settled or handed messages back. A term whose validity had run out when
// it was stopped - a lapse the renewals never resumed - releases the record unsettled, whatever
// its own flags say: the lapse dropped what the term had in hand and what the durable pushed it
// without an ack, and those come back only at their ack wait, as they would have from the expired
// record this release replaces. A record another pod has already taken is left as it is, and that
// is no error. It returns ctx.Err() when ctx ends before the loop has stopped, and the write's
// error when the record could not be marked; either way this process no longer holds the lease,
// and a record left unreleased is taken as an expired one.
//
// A loop that has already stopped is read again at ctx's end (awaitDone, closedWithin): the wait
// selects over done and ctx, and once ctx has ended it reads done again, so a loop stopped by then
// reads stopped. The receiver's own stop calls Release again with the shutdown context a graceful
// stop can have run out of (durableReceiver.Stop, after handBack's release timed out), and a
// select over a closed done and an ended ctx picks either, so without that second read half of
// those stops would skip the write and leave a standby to wait the record out as an expired one.
func (l *lease) Release(ctx context.Context) error {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.mu.Unlock()
	if stop != nil {
		stop()
		if err := awaitDone(ctx, done); err != nil {
			return err
		}
	}
	_, err := l.letGo()
	return err
}

// letGo ends this process's hold and, when it held the record, marks the record released at its
// revision, keeping service_since and stale_binding as they stand and writing unsettled as the
// term stands, or set when the term had lapsed (Release). It returns the released record's
// revision, or zero when it wrote none: it did not hold the record, another pod had already taken
// it (no error), or the write failed (its error).
func (l *lease) letGo() (uint64, error) {
	l.mu.Lock()
	held, rev := l.held, l.rev
	record := l.releasedRecordLocked(l.held && !l.holdingLocked())
	l.held, l.validUntil = false, time.Time{}
	l.mu.Unlock()
	if !held {
		return 0, nil
	}
	defer l.signal()
	return l.writeRelease(record, rev)
}

// releasedRecordLocked is the record a release of this term writes: the record as this process
// last wrote or read it, marked released, keeping service_since and stale_binding as they stand
// and writing unsettled as the term stands, or set when owesAckWait says this release owes its
// successor a full ack wait whatever the term's own flags said. A term whose validity had run out
// owes one (letGo): what it had in hand comes back only at the ack wait. So does a release for a
// receive connection that is down (releaseReceiveLost), on a term that has not lapsed at all.
// Callers must hold l.mu.
func (l *lease) releasedRecordLocked(owesAckWait bool) leaseRecord {
	record := l.record
	record.StaleBinding, record.Unsettled = l.staleBinding, l.unsettled || owesAckWait
	record.Released = true
	return record
}

// writeRelease writes record, a released record of this term, over revision rev. It returns the
// released record's revision, or zero when it wrote none: another pod had already taken the record
// (no error), or the write failed (its error).
func (l *lease) writeRelease(record leaseRecord, rev uint64) (uint64, error) {
	value, err := record.encode()
	if err != nil {
		return 0, fmt.Errorf("encode the released lease record: %w", err)
	}
	next, err := l.kv.Update(l.key, value, rev)
	switch {
	case err == nil:
		l.mu.Lock()
		l.rev, l.record = next, record
		l.mu.Unlock()
		l.logger.Info("listener lease released", slog.String("lease", l.key), slog.Uint64("revision", next))
		return next, nil
	case bus.IsCASConflict(err):
		return 0, nil
	default:
		return 0, fmt.Errorf("mark lease %s released: %w", l.key, err)
	}
}

// signal records a change for Changes, without waiting for a reader.
func (l *lease) signal() {
	select {
	case l.changes <- struct{}{}:
	default:
	}
}

// receiveDown reports whether this pod's receive connection is down at now and how long it has
// been down. It is the one place the receiver's signal is judged (SetReceiveSignal), for the
// holder's release (releaseReceiveLost) and the standby's take (poll) alike, and only the loop
// calls it, so the moment it went down needs no lock. A connection that is up forgets that
// moment, so a reconnect inside the bound costs the holder nothing and the next cut is timed
// from itself.
func (l *lease) receiveDown(now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	up := l.receiveUp
	l.mu.Unlock()
	if up == nil || up() {
		l.receiveDownSince = time.Time{}
		return false, 0
	}
	if l.receiveDownSince.IsZero() {
		l.receiveDownSince = now
	}
	return true, now.Sub(l.receiveDownSince)
}

// step does one round of the lease, renewing it as the holder, giving it up when GiveUp asked for
// the current term, releasing it when this pod's receive connection has been down for the bound,
// or reading and maybe taking it as a standby, and returns how long to wait before the next. It
// is where the receiver's signal is read, once a round, for the holder and the standby alike
// (receiveDown).
func (l *lease) step() time.Duration {
	down, lasted := l.receiveDown(l.now())
	l.mu.Lock()
	held, givingUp := l.held, l.held && l.giveUpTerm == l.term
	l.mu.Unlock()
	switch {
	case givingUp:
		return l.giveUp()
	case held && down && lasted >= l.cfg.receiveLost:
		return l.releaseReceiveLost(lasted)
	case held:
		wait := l.renew()
		if down {
			// Wake at the bound rather than at the next renewal, so the release lands at it:
			// every renewal past it renews a record this pod delivers nothing from.
			wait = min(wait, l.cfg.receiveLost-lasted)
		}
		return wait
	}
	return l.poll(down)
}

// giveUp releases the record as letGo does and leaves the loop a standby that does not take that
// released revision back for two standby polls (GiveUp). A record the release could not mark is
// taken, by this process too, once it expires.
func (l *lease) giveUp() time.Duration {
	rev, err := l.letGo()
	if err != nil {
		l.logger.Warn("listener lease not released; a standby takes it once it expires", slog.String("error", err.Error()))
	}
	return l.standDown(rev)
}

// standDown leaves the loop a standby after this term let the record go at revision rev, zero
// when no release was written: the loop reads the record afresh, does not take rev back for two
// standby polls (poll), and times its next round at the standby poll it returns. Only the loop
// calls it, as it calls giveUp and releaseReceiveLost, so the fields it writes need no lock.
func (l *lease) standDown(rev uint64) time.Duration {
	l.seenRev, l.lapseSignalled = 0, false
	l.givenUpRev, l.givenUpAt = rev, l.now()
	return l.cfg.standbyPoll
}

// releaseReceiveLost stops renewing and releases the record of a holder whose receive connection
// has been down for cfg.receiveLost (receiveDown), down: this pod receives nothing of the
// durable, so the machine's lease must go to a pod that can. The record goes unsettled, owing its
// successor an ack wait and carrying stale_binding, as a lapse's release does and whatever this
// term's own flags stood at: what the durable pushed to the lost connection comes back only at
// its ack wait, and that connection's subscription can still be bound at the server. So the pod
// that takes it settles for the full ack wait, subscribing at once beside that binding. The loop
// does not take the record back for two standby polls, as a give-up does, and takes none at
// all while the connection stays down (poll). A record another pod has already taken writes
// nothing and is no error: the revision logged is then zero, and this pod stops holding it all
// the same. The term is this pod's own hold, read by step in this goroutine a moment ago, so
// there is no un-held case to answer here.
//
// A release the bucket does not take keeps the hold: nothing was handed over, so this pod goes on
// as a holder whose bucket calls fail - its renewals lapse and a standby takes its record once it
// expires, as before this bound - and it tries the release again at its next turn. The receive
// loop ends the term as it ends any lost lease, dropping the subscription on the dead connection
// with no hand-back possible on it (durableReceiver.endLost).
func (l *lease) releaseReceiveLost(down time.Duration) time.Duration {
	l.mu.Lock()
	rev := l.rev
	record := l.releasedRecordLocked(true)
	record.StaleBinding = true
	l.mu.Unlock()
	next, err := l.writeRelease(record, rev)
	if err != nil {
		l.noteFailure("release after the receive connection went down", err)
		return l.renew()
	}
	l.mu.Lock()
	l.held, l.validUntil = false, time.Time{}
	l.mu.Unlock()
	l.logger.Warn("listener lease released: the receive connection is down",
		slog.String("lease", l.key), slog.Int64("down_ms", down.Milliseconds()),
		slog.Uint64("revision", next))
	l.signal()
	return l.standDown(next)
}

// renew rewrites the holder's record at the revision it last wrote. A renewal that lands counts
// from its own start; one that started after the validity ran out is a resume, a new term taken
// as over an expired record, which must settle and so writes unsettled. The resume is this
// process's own record (Resumed), which the receive loop tells from a take-over over another
// pod's expired record. A wrong revision means another pod took the record, and this
// process steps down; any other failure is retried every renewEvery, sooner when the validity ends
// first, so its end is signalled.
func (l *lease) renew() time.Duration {
	start := l.now()
	l.mu.Lock()
	lapsed := !start.Before(l.validUntil)
	record, rev := l.record, l.rev
	record.DeliverSubject = l.deliverSubject
	record.StaleBinding, record.Unsettled = l.staleBinding, l.unsettled || lapsed
	l.mu.Unlock()
	if lapsed && !l.lapseSignalled {
		l.lapseSignalled = true
		l.logger.Warn("listener lease lapsed: its renewals failed for the lease's validity", slog.String("lease", l.key))
		l.signal()
	}
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.renewEvery
	}
	next, err := l.kv.Update(l.key, value, rev)
	if bus.IsCASConflict(err) {
		own, mine, err := l.ownRevision()
		if err != nil {
			l.noteFailure("read after a conflict", err)
			return l.cfg.renewEvery
		}
		if mine {
			// An earlier renewal landed though its reply was lost: the record is this process's
			// own write, so renew again at that revision. The validity is not extended until a
			// renewal's reply arrives.
			l.mu.Lock()
			l.rev = own
			l.mu.Unlock()
			return 0
		}
		l.stepDown()
		return 0
	}
	if err != nil {
		l.noteFailure("renew", err)
		l.mu.Lock()
		wait := min(l.cfg.renewEvery, l.validUntil.Sub(l.now()))
		l.mu.Unlock()
		if wait <= 0 {
			wait = l.cfg.renewEvery
		}
		return wait
	}
	l.recovered()
	l.lapseSignalled = false
	l.mu.Lock()
	l.rev, l.record = next, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	if lapsed {
		l.startTermLocked(leaseTakenExpired, true, true)
	}
	term := l.term
	l.mu.Unlock()
	if lapsed {
		l.logger.Info("listener lease resumed: a renewal after its validity found its own record current",
			slog.String("lease", l.key), slog.Uint64("term", term), slog.Uint64("revision", next))
		l.signal()
	}
	return l.cfg.renewEvery
}

// startTermLocked begins a term of this process's hold and is where every term's state is set,
// whichever way it was taken: the take of a record (acquire) and the resume of this process's own
// after its renewals lapsed (renew). It counts the acquisition, records how it was taken, whether
// it must settle and whether it is a resume (leaseStatus), and sets the flag that follows from
// settle: a term that must settle is unsettled from here until its settle hold ends (Settled).
// acquire records stale_binding itself, which it carries over from the record it took. Callers
// must hold l.mu.
func (l *lease) startTermLocked(how leaseAcquisition, settle, resumed bool) {
	l.term++
	l.how = how
	l.settle, l.resumed = settle, resumed
	l.unsettled = settle
}

// stepDown ends the holder's hold after a renewal found another pod's record.
func (l *lease) stepDown() {
	l.mu.Lock()
	l.held, l.rev, l.validUntil = false, 0, time.Time{}
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Warn("listener lease lost: another listener holds it", slog.String("lease", l.key))
	l.signal()
}

// ownRevision reads the record after a renewal's conflict and reports its revision and whether it
// is this process's own unreleased write (its boot), which only a renewal whose reply was lost
// leaves behind.
func (l *lease) ownRevision() (uint64, bool, error) {
	entry, err := l.kv.Get(l.key)
	if errors.Is(err, nats.ErrKeyNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		return 0, false, nil
	}
	return entry.Revision(), record.Boot == l.boot && !record.Released, nil
}

// poll reads the record as a standby and takes it when it is absent (Create), released (Update
// at its revision, at once) or unchanged for a full ttl since this process first saw its
// revision (Update at that revision). The released revision this process wrote on giving the
// lease up it takes only two standby polls after that (GiveUp), so another standby takes it first.
//
// down is whether this pod's receive connection is down (receiveDown): it then reads the record,
// so its /healthz still names the holder, and takes none, since a pod that receives nothing would
// hold the machine's lease and deliver nothing from it. It forgets the revision it was timing
// with it, so once the connection is back it stands by again from that poll on.
func (l *lease) poll(down bool) time.Duration {
	entry, err := l.kv.Get(l.key)
	seen := l.now()
	if errors.Is(err, nats.ErrKeyNotFound) {
		l.recovered()
		l.mu.Lock()
		l.record = leaseRecord{}
		l.mu.Unlock()
		l.seenRev = 0
		if down {
			return l.cfg.standbyPoll
		}
		return l.acquire(nil, leaseRecord{}, leaseTakenAbsent)
	}
	if err != nil {
		l.noteFailure("read", err)
		return l.cfg.standbyPoll
	}
	l.recovered()
	record, err := decodeLeaseRecord(entry.Value())
	if err != nil {
		// An unreadable record is taken as one that is not released, once it stays unchanged for
		// the ttl, and nothing of it is kept.
		if entry.Revision() != l.seenRev {
			l.logger.Warn("listener lease record unreadable", slog.String("lease", l.key),
				slog.Uint64("revision", entry.Revision()), slog.String("error", err.Error()))
		}
		record = leaseRecord{}
	}
	l.mu.Lock()
	l.record = record
	l.mu.Unlock()
	switch {
	case down:
		l.seenRev = 0
		return l.cfg.standbyPoll
	case record.Released && entry.Revision() == l.givenUpRev && seen.Sub(l.givenUpAt) < 2*l.cfg.standbyPoll:
		return l.cfg.standbyPoll
	case record.Released:
		return l.acquire(entry, record, leaseTakenReleased)
	case entry.Revision() != l.seenRev:
		l.seenRev, l.seenAt = entry.Revision(), seen
		return l.cfg.standbyPoll
	case seen.Sub(l.seenAt) < l.cfg.ttl:
		return l.cfg.standbyPoll
	default:
		return l.acquire(entry, record, leaseTakenExpired)
	}
}

// acquire writes this process's record over previous (nil: none) and, once it reads back the
// write's server timestamp, holds the lease from the write's start. service_since starts again
// at that timestamp when no record was there, when it carried none, or when its last write is
// more than two ttl older than this one, since then no pod of the machine was serving; otherwise
// it is kept. stale_binding is carried over and unsettled is written when the term must settle
// (leaseAcquisition). The record is renewed at once to store service_since and acquired_at. The
// write is read back by key, a direct get on the key's own subject, which the listener's grant
// allows; a get by revision is a direct get on the stream's bare subject, which it does not. A
// read back that finds another revision, or no record, means another write landed first: the
// lease is not held and the record is polled again at once. A write whose timestamp cannot be read
// back is not counted as held: the record then expires as any unreleased one does, and a standby,
// this one included, takes it after the ttl.
func (l *lease) acquire(previous nats.KeyValueEntry, record leaseRecord, how leaseAcquisition) time.Duration {
	start := l.now()
	l.mu.Lock()
	record.DeliverSubject = l.deliverSubject
	l.mu.Unlock()
	record.Holder, record.Boot, record.Released, record.AcquiredAt = l.instance, l.boot, false, time.Time{}
	settle := how == leaseTakenExpired || record.StaleBinding || record.Unsettled
	record.Unsettled = settle
	value, err := record.encode()
	if err != nil {
		l.noteFailure("encode", err)
		return l.cfg.standbyPoll
	}
	var rev uint64
	if previous == nil {
		rev, err = l.kv.Create(l.key, value)
	} else {
		rev, err = l.kv.Update(l.key, value, previous.Revision())
	}
	if bus.IsCASConflict(err) {
		l.seenRev = 0
		return 0
	}
	if err != nil {
		l.noteFailure("take", err)
		return l.cfg.standbyPoll
	}
	written, err := l.kv.Get(l.key)
	if err != nil || written.Revision() != rev {
		l.seenRev, l.seenAt = rev, l.now()
		if err == nil || errors.Is(err, nats.ErrKeyNotFound) {
			return 0
		}
		l.noteFailure("read back", err)
		return l.cfg.standbyPoll
	}
	record.AcquiredAt = written.Created()
	if previous == nil || record.ServiceSince.IsZero() || written.Created().Sub(previous.Created()) > 2*l.cfg.ttl {
		record.ServiceSince = written.Created()
	}
	l.mu.Lock()
	l.held, l.rev, l.record = true, rev, record
	l.validUntil = start.Add(l.cfg.ttl - l.cfg.margin)
	l.startTermLocked(how, settle, false)
	l.staleBinding = record.StaleBinding
	term := l.term
	l.mu.Unlock()
	l.seenRev, l.lapseSignalled = 0, false
	l.logger.Info("listener lease acquired", slog.String("lease", l.key), slog.String("from", how.String()),
		slog.Uint64("term", term), slog.Uint64("revision", rev), slog.Time("service_since", record.ServiceSince),
		slog.Bool("settle", settle), slog.Bool("stale_binding", record.StaleBinding))
	l.signal()
	return l.renew()
}

// noteFailure logs the first failed bucket call of a streak; recovered logs its end.
func (l *lease) noteFailure(action string, err error) {
	if l.failing {
		return
	}
	l.failing = true
	l.logger.Warn("listener lease state unavailable", slog.String("lease", l.key),
		slog.String("action", action), slog.String("error", err.Error()))
}

func (l *lease) recovered() {
	if !l.failing {
		return
	}
	l.failing = false
	l.logger.Info("listener lease state available again", slog.String("lease", l.key))
}
