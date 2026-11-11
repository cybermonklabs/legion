// The delete of a session's consumer: the one body every delete this listener makes goes
// through (deleteConsumer), the goroutine a deregistration hands its own delete to
// (deleteAfterAnswer), and the shutdown's wait for those goroutines (awaitDeletes). The fields
// that wait counts and holds off (deleteMu, deletes, deletesOff) stay where the type they
// belong to is declared (sessionDelivery, session_delivery.go).
//
// Three callers reach deleteConsumer, and each gives it the consumer's info when it holds one:
//   - the deregistration, through deleteAfterAnswer, off the goroutine that answered the client
//     (deleteSessionHandler, api.go), holding no info;
//   - the sweep over the stream's consumer list, which holds the info that list answered
//     (sessionConsumerSweep.pass, session_consumer_sweep.go);
//   - the session reaper, for each record a pass deleted, holding no info
//     (session.SessionReaper.DeleteConsumer, wired in main.go).
//
// Each delete confirms under the session's lock that the session bucket holds no record of the
// session, records the consumer's resume floor and removes the consumer in one bus call
// (bus.SessionConsumers.Delete), and gives the consumer back at that floor to a session that
// registered meanwhile (repairRegistered, session_delivery.go).

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// awaitDeletes stops starting consumer deletes off a handler's goroutine and waits, within ctx,
// for those already running (deleteAfterAnswer). The shutdown calls it once the HTTP server has
// stopped and before it drains the session-consumer connection those deletes run on, so a
// deregistration that answered a moment before the stop still frees its session's slot of the
// stream's consumer cap. A delete still running when ctx ends is left where it is and said so:
// the connection's drain ends it, and the sweep deletes the consumer it did not.
func (m *sessionDelivery) awaitDeletes(ctx context.Context) {
	if m == nil {
		return
	}
	m.deleteMu.Lock()
	m.deletesOff = true
	m.deleteMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.deletes.Wait()
	}()
	select {
	case <-done:
	case <-ctx.Done():
		m.logger.Warn("a session consumer delete was still running at shutdown; the sweep deletes that consumer")
	}
}

// deleteAfterAnswer deletes sessionID's consumer (deleteConsumer) off the goroutine that asked
// for it, so a deregistration answers its caller first: a handler holds its written response
// until it returns, and this work waits on the session's lock with no bound of its own and then
// makes the bucket read that confirms the session is gone, the delete, which records the
// consumer's resume floor, and the repair's own calls, which together can outlast the attempt
// the client gave the request (LISTENER_CALL_TIMEOUT_MS, 5 s) after a KV delete that may already
// have spent sendRequestBudget. ctx is the caller's context with its cancellation dropped
// (context.WithoutCancel), since the registration is gone whatever the caller does next.
//
// The deregistration holds no consumer info, so the delete reads it for itself and records the
// resume floor from that read (bus.SessionConsumers.Delete).
//
// The shutdown waits for every delete started here before it drains the connection they run on
// (awaitDeletes). One that is not started because the listener is stopping is logged rather
// than lost silently: the sweep deletes the consumer of every session the registry judges gone,
// and it is the backstop for this one.
func (m *sessionDelivery) deleteAfterAnswer(ctx context.Context, d *listenerDeps, sessionID string) {
	if m == nil || m.consumers == nil {
		return
	}
	m.deleteMu.Lock()
	if m.deletesOff {
		m.deleteMu.Unlock()
		m.logger.Warn("session consumer delete not started: the listener is stopping; the sweep deletes that consumer",
			slog.String("session_id", sessionID))
		return
	}
	m.deletes.Add(1)
	m.deleteMu.Unlock()
	go func() {
		defer m.deletes.Done()
		m.deleteConsumer(ctx, d, sessionID, nil)
	}()
}

// deleteConsumer removes sessionID's consumer once the session's registration is gone, so the
// consumer holds a slot of the stream's consumer cap no longer rather than waiting out the
// server's inactivity threshold (inactiveThresholdFor, main.go). listed is the consumer's info
// when the caller holds it already, as the sweep holds what its consumer list answered, and nil
// where reading it is the bus call's to do (bus.SessionConsumers.Delete). It reports whether the
// consumer is gone, which is what the sweep counts; a failure is logged, not returned, since the
// deregistration that asked for it answers the same either way, and the listener's sweep and that
// threshold each reach the consumer again.
//
// Under the session's lock the bucket is read again (listenerDeps.sessions), and only
// nats.ErrKeyNotFound - no record for the session at all - lets the delete through. A record
// that stands keeps the consumer: the registration behind it is live and that consumer is what
// delivers to it. A read that did not answer keeps it too, for the sweep's next pass, and is
// logged as a warning: a reaped or deregistered session whose consumer stands for that reason
// has no other line anywhere, where a record the bucket does hold is the ordinary outcome of
// every pass of the sweep and is logged below it.
//
// That read and the lock together are what order this delete against a registration of this
// listener's, and neither half carries it alone. The lock: the subscribe path holds it from its
// consumer call through the write of its entry (register, api.go), so a registration in flight
// is either not started here, or finished by the time this reads. The read: it is the leader
// of the session bucket's stream that answers it (session.SessionRegistry.RefreshFromLeader),
// not the direct get every other read of that bucket makes, because on a replicated bucket a
// direct get is answered by whichever copy is near enough to current, and a copy that has not
// applied the registration's own write answers not found for a session that is live. Without
// both halves a clean stop answered session.EntryDeleted on the entry a relaunch replaced
// would take the live relaunch's consumer, and its relay would pause on
// bus.ErrSessionConsumerNotFound until the session registered again.
//
// Against anything outside this process the lock settles nothing, and the bucket read is what
// narrows the rest: a second listener task - a rolling deploy's other task, or the one a
// heartbeat races to - can register the session and create the consumer again between this read
// and this delete, and the repair below, not this lock, is what gives that session its consumer
// back. Inside the process the lock orders the delete against this listener's own consumer
// calls for the session: a registration's create or filter update, an unsubscribe's update, and
// a relay restart, which the lock keeps whole (locks). A restart already past its wanted check
// on another goroutine finds the consumer missing with no pending start, the delete having
// dropped it, and ends in bus.ErrSessionConsumerNotFound, which pauses its relay rather than
// creating anything.
//
// The floor the repair uses is the floor the delete recorded, and it is one value because one
// call records it and answers it (bus.SessionConsumers.Delete): the consumer it was read from
// is gone, so nothing moves that floor again, and the consumer that comes back comes back where
// the session's own resume record says it stopped. A delete that recorded nothing - a session
// id that names no consumer, a consumer the stream no longer held with no pending start behind
// it - answers 0, and the repair then starts at the start the session's record names alone,
// which is that session's own answer and never above its floor (registeredTarget).
//
// A delete that went through reads the session's record again from the bucket and gives the
// consumer back to a session whose record is there (repairRegistered), at that floor. Every
// delete of a consumer follows the delete of that session's record, so a record that is there
// again was written by a registration that landed between the read above and this delete - on
// another listener, whose registration this process's lock does not order - and that
// registration was answered 200 with consumer delivery. Without that repair such a session
// holds no consumer until its next heartbeat, and a consumer is the one thing a session is
// delivered through. d is the listener's stores that read and the create after it go through,
// which the caller holds, as refollow's caller holds them.
//
// The repair is this side of the invariant a session answered 200 with consumer delivery holds
// a consumer, whatever delete raced it, on whatever listener. The other side is the
// registration's own read of the consumer, made once it has written that entry
// (confirmRegistered). The two meet in the bucket and not in the lock, which orders only the
// calls of this process: whichever of the two acts last reads what the other wrote and repairs.
// A delete whose record read comes after the registration's entry write finds that entry, keeps
// the consumer and deletes nothing; one whose read comes before it deletes and restores
// nothing, and the registration's own read, which is after that entry write and so after this
// delete took the consumer, finds the consumer gone and creates it again itself.
//
// One budget covers the record read, the delete and the resume record it writes first, and the
// record read and create that may follow them, as one budget covers a registration's whole pass.
//
// A listener whose NATS user may not manage session consumers deletes nothing while its refusal
// episode stands (grantRefused): the delete would be refused, and the episode's one warning has
// been logged already.
func (m *sessionDelivery) deleteConsumer(ctx context.Context, d *listenerDeps, sessionID string, listed *jetstream.ConsumerInfo) bool {
	if m == nil || m.consumers == nil || m.grantRefused() {
		return false
	}
	// The wait for the session's lock has no bound of its own - a registration's write of its
	// entry or a relay restart holds it - so the budget for this delete's own calls starts once
	// the lock is held, as a relay restart times its calls from its turn and its lock
	// (sessionRelays.restart). A budget taken before the wait could be spent in it, and the
	// read below would then fail for a delay that cost the server nothing, leaving the consumer
	// to the sweep.
	unlock := m.locks.lock(sessionID)
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	switch _, err := d.sessions.RefreshFromLeader(ctx, sessionID); {
	case errors.Is(err, nats.ErrKeyNotFound):
	case err != nil:
		m.logger.Warn("session consumer kept: the session's record could not be read",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return false
	default:
		m.logger.LogEvent(slog.LevelDebug, "session consumer kept: the session bucket holds a registration for it",
			slog.String("session_id", sessionID))
		return false
	}
	floor, err := m.consumers.Delete(ctx, sessionID, listed)
	if err != nil {
		if !m.grantRefusal("delete a session consumer", sessionID, err) {
			m.logger.Warn("session consumer delete failed", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		}
		return false
	}
	m.granted()
	// The repair. A record the bucket holds now that this session's consumer is gone was
	// written by a registration that landed between the read above and this delete, and that
	// registration was answered 200 with consumer delivery, so the consumer comes back on the
	// filter and the start the record and the bucket's newest row ask for (registeredTarget),
	// at the floor the delete recorded.
	m.repairRegistered(ctx, d, sessionID, floor, repairAfterDelete)
	return true
}
