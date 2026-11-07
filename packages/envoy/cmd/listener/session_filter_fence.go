// The fence that keeps the filter of an older interest row off a session's consumer, and the one
// step every write of a stored row's filter takes under it. Four writers take that step - the
// repair of a session's consumer, an unsubscribe, a registration this listener did not answer and
// the relay's restart - and each builds its write from a bucket read the row can move under.

package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// filterFenceAttempts bounds the writes one fenced filter write makes: the first, and four more
// over rows that landed under it (fenceFilterWrite).
//
// The passes are what makes the filter converge, since nothing outside the loop repairs what it
// leaves. The registration that stores each of those rows sets the filter of the row it stores
// before it stores it (consumerFor) and judges that filter against the bucket once its entry is
// written (confirmRegistered); both of those are behind it by the time this listener's write
// lands over them, so that registration is answered and gone, and the topic it added stays
// filtered out of the consumer until the session's next heartbeat, 90 s away
// (contracts.SessionHeartbeat), every message published on it in between skipped for good, since
// a filter update keeps the consumer's position.
//
// Against that a pass is cheap: one bucket read and one consumer call, under that session's own
// lock and no other session's. The count is what the caller's budget can really spend, since the
// whole loop runs inside the caller's five-second call timeout (sessionConsumerCallTimeout), a
// relay restart's own calls included: five passes are ten round trips to the server, and that
// budget covers them at a quarter of a second each with the restart's beside them. One
// writer racing a repair settles in two passes, and five leaves room for the two listeners of a
// rolling deploy with an unsubscribe or a role claim of the same session between them; a row
// still moving under five writes in a row is written to continuously rather than raced once,
// which is what the line the loop ends on names.
const filterFenceAttempts = 5

// fenceFilterWrite makes a write of a session consumer's filter and lets no write built from an
// interest row another writer has replaced stand.
//
// Every such write is built from a bucket read and lands at the server after it
// (repairRegistered, storedFilterWrite, sessionRelays.settleFilter). A row stored between the two
// makes what lands an older row's filter, over the filter a newer registration set, and the
// session is then answered 200 for a topic its consumer does not filter on; because a filter
// update keeps the consumer's position, the messages that pass in that window are skipped for
// good. The server holds no conditional consumer update to write that filter under, so the fence
// is the interest row's KV revision read again after the write (storedTopics): write answers the
// revision of the row it built its write from, and while the bucket answers a higher one
// (rowMovedUnder) the loop plans and writes again, ending when the row the write was built from
// is still the newest.
//
// A loop that ends on an older row's filter converges no further here: it logs that at WARN,
// naming the session, the revision it wrote from and the one the bucket answered, and leaves the
// filter to the session's next heartbeat, which registers again and writes both the row and the
// filter of it. Passing again inside this loop is the whole of the retry: handing the session
// back to the repair path instead would buy nothing, since every pass already plans from the
// bucket's newest record and row and a hand-back would only take the session's lock again to do
// the same thing. What the passes cost is a longer hold of that one session's lock; what stopping
// costs is the messages on the topics the two rows differ by until that heartbeat, and
// filterFenceAttempts is where the two are weighed.
//
// What the fence leaves open, each window costing those messages until that heartbeat: a row
// stored after the loop's last read stands unfenced, since nothing reads the bucket again to
// find it; a corrective pass that plans nothing, or whose write the server refused, leaves the
// filter of the pass before it standing, which the bucket has already answered a newer row
// than; a loop out of passes leaves the filter of its last pass, under which the row moved once
// more; and a read that failed ends the loop unjudged, since a fence that cannot read the
// bucket cannot say whether the write it made is an older row's. The second and third log the
// line below; the first is not known here, and the fourth has no revision to name.
//
// Both reads are direct gets, so with ENVOY_NATS_REPLICAS above 1 a copy one write behind the
// leader can answer an older row, and an older revision, at either end of the loop
// (bus.KeyValue.Get states that limit and why this build leaves it standing); rowMovedUnder is
// what keeps such an answer from starting a pass planned from the lagging row it carries.
//
// write plans its write from a bucket read, makes it, and answers the KV revision of the interest
// row it was built from with whether the loop goes on: false where nothing landed - it planned no
// write, the server answered its call with a refusal, or the call found the consumer already as
// the row asks - since a write that did not land puts no older row's filter over anything. A
// call whose answer did not come back is not such an answer: the server may have applied it, so
// write answers true for one and the loop judges the filter it may have left
// (consumerCallAnswered). Every failure is logged by write itself, which names what it was doing.
// A caller whose first write is already made answers that write from its first pass and writes
// nothing in it (sessionRelays.settleFilter), so that write is one of the passes below.
func fenceFilterWrite(ctx context.Context, logger *logging.Logger, registry *store.Registry, sessionID string, write func() (uint64, bool)) {
	// stands is the revision of the row whose filter is on the consumer, 0 until a pass writes
	// one; newest is what the bucket answered after that write.
	var stands, newest uint64
	for pass := range filterFenceAttempts {
		wrote, ok := write()
		if !ok {
			if pass == 0 {
				return // nothing of this loop's landed, so no older row's filter stands
			}
			// A corrective pass that wrote nothing leaves the pass before it standing, and the
			// bucket answered a newer row than that one: that is the line below's case too.
			break
		}
		stands = wrote
		var err error
		if _, newest, err = storedTopics(ctx, registry, sessionID); err != nil {
			return
		}
		if !rowMovedUnder(newest, stands) {
			return
		}
	}
	logger.Warn("session consumer filter left on an older interest row than the bucket holds; the session's next heartbeat sets it",
		slog.String("session_id", sessionID), slog.Uint64("wrote_revision", stands), slog.Uint64("newest_revision", newest))
}

// rowMovedUnder reports whether the interest row the bucket answered at revision newest was
// stored after the row a filter write was built from, at revision wrote, so that a pass planned
// from the newest row is a correction and not a step back: a bucket's revisions only rise, so a
// strictly higher one is a write of that key the bucket took after the one wrote names.
//
// Equal is the answer the loop is looking for and ends it. Lower ends it too, and that is what
// makes this a comparison rather than newest != wrote: both of the loop's reads are direct gets,
// so with ENVOY_NATS_REPLICAS above 1 the one after the write can be answered by a copy up to one
// write behind the leader (bus.KeyValue.Get), and a pass started from such an answer plans from a
// row older than the one just written and writes that older row's filter, which is the loss this
// fence exists to prevent, with the next pass's lagging read free to do it again.
//
// A newest of 0 ends it as well, where the bucket holds no row for the session: a delete of the
// row, or that same lagging copy answering for a key it has not got yet. The two costs of
// reading it wrong are not comparable. A filter left naming the topics of a row since deleted
// delivers frames the session follows none of, which its plugin drops (refollow), and the delete
// of a session's record takes its consumer with it anyway (deleteConsumer); a filter emptied to
// the agent subject under a live session whose row a lagging copy has not got drops every topic
// it follows until its next heartbeat.
func rowMovedUnder(newest, wrote uint64) bool { return newest > wrote }

// storedTopics is the topics of sessionID's interest row as the bucket holds it now, with the KV
// revision it was read at: none where the session has no row (nats.ErrKeyNotFound), and none
// where the row is one this build cannot decode (store.ErrMalformedInterest), whose topics
// nothing routes. Both carry the revision all the same - a row this build cannot decode is
// fenced on its own revision, and a session with no row answers 0 - so a write built from either
// is fenced on what the bucket answered. Every other failure is the caller's to answer for, and
// carries no revision.
//
// The row is read from the bucket and never from the cache (store.Registry.RefreshAt): a caller
// writing the session consumer's filter from what it reads must hold a registration this
// listener's watch need not have delivered yet, and the revision is what that write is fenced
// on, which the fence reads this way too to see whether the row has moved (fenceFilterWrite).
func storedTopics(ctx context.Context, registry *store.Registry, sessionID string) ([]string, uint64, error) {
	row, revision, err := registry.RefreshAt(ctx, sessionID)
	if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, store.ErrMalformedInterest) {
		return nil, revision, nil
	}
	if err != nil {
		return nil, 0, err
	}
	return row.Topics, revision, nil
}

// filterAfterUnsubscribe, filterAfterUnansweredRegistration and filterAfterRestart name what a
// write of a session consumer's filter built from the bucket's newest row follows, which its log
// line carries (storedFilterStep): the unsubscribe that removed topics from the row (refollow),
// the registration this listener could not answer (settleUnanswered), or the relay restart whose
// own filter write the fence is correcting (sessionRelays.settleFilter).
const (
	filterAfterUnsubscribe            = "unsubscribe"
	filterAfterUnansweredRegistration = "unanswered registration"
	filterAfterRestart                = "relay restart"
)

// storedFilterWrite holds the filter of sessionID's consumer to the topics the bucket's newest
// row holds, every pass of it fenced on the revision that row was read at (fenceFilterWrite,
// storedFilterStep). after names what the write follows, for the log.
//
// It owns the three things its callers would otherwise each repeat (refollow, settleUnanswered):
// the listener that holds no session-consumer connection, which only a test builds and which
// writes nothing; the budget every consumer call this listener makes is bounded by
// (sessionConsumerCallTimeout), which here covers the whole loop, as it covers a relay restart's
// calls and the corrections after them; and the session's lock, which orders these writes against
// the registrations', the relay's restart and the delete a reaper or a stop makes of the same
// session.
func (m *sessionDelivery) storedFilterWrite(ctx context.Context, registry *store.Registry, sessionID, after string) {
	if m == nil || m.consumers == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	unlock := m.locks.lock(sessionID)
	defer unlock()
	fenceFilterWrite(ctx, m.logger, registry, sessionID, func() (uint64, bool) {
		return m.storedFilterStep(ctx, registry, sessionID, after)
	})
}

// storedFilterStep is one pass of a fenced write of the filter the bucket's newest interest row
// asks for: it reads that row from registry, sets the consumer's filter to the subjects its
// topics build, and answers the fence driving it with the revision the row was read at and
// whether a filter of that row may now stand on the consumer (fenceFilterWrite). It creates no
// consumer: one the stream does not hold is left to the session's next registration, so the
// server's answer for a consumer that is gone (bus.ErrSessionConsumerNotFound) is logged by
// nothing here. after names what the write follows, for the log.
//
// It takes no lock and bounds nothing: the caller holds the session's lock over every pass of
// the fence and bounds ctx (storedFilterWrite, and sessionRelays.settleFilter under a restart).
func (m *sessionDelivery) storedFilterStep(ctx context.Context, registry *store.Registry, sessionID, after string) (uint64, bool) {
	topics, revision, err := storedTopics(ctx, registry, sessionID)
	if err != nil {
		m.logger.Warn("session consumer filter left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("after", after), slog.String("error", err.Error()))
		return 0, false
	}
	if err := m.consumers.UpdateFilter(ctx, sessionID, sessionConsumerFilter(sessionID, topics)); err != nil {
		if !errors.Is(err, bus.ErrSessionConsumerNotFound) {
			m.logger.Warn("session consumer filter update failed",
				slog.String("session_id", sessionID), slog.String("after", after), slog.String("error", err.Error()))
		}
		// A call the server answered set no filter; one whose answer did not come back may
		// have set this row's, and the loop judges it (consumerCallAnswered).
		return revision, !consumerCallAnswered(err)
	}
	return revision, true
}

// consumerCallAnswered reports whether err is the server's own answer to a consumer call, which
// says that call changed nothing: a JetStream API error (a create or an update the server
// refused, bus.ErrConsumerLimitReached among them), a call the connection's grant may not make
// (bus.ErrConsumerNotGranted, which wraps bus.ErrPublishDenied), a consumer the stream does not
// hold (bus.ErrSessionConsumerNotFound), and a session id no consumer can be named after
// (bus.ErrNoSessionConsumerName), which is refused before anything is sent.
//
// Every other failure leaves the call ambiguous: a request that timed out, a context that ended
// under it, a connection that went are answers that did not come back, and the server may have
// applied the call all the same. A fenced filter write treats one as a write that may stand
// (fenceFilterWrite), because an older row's filter left on a consumer with nothing to read it
// again is exactly what that fence is there to catch; the cost of being wrong is one bucket read
// and one consumer call that changes nothing.
func consumerCallAnswered(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) ||
		errors.Is(err, bus.ErrConsumerNotGranted) ||
		errors.Is(err, bus.ErrSessionConsumerNotFound) ||
		errors.Is(err, bus.ErrNoSessionConsumerName)
}
