// The fence that keeps the filter of an older interest row off a session's consumer, the budget
// it runs on, and the one step every write of a stored row's filter takes under it. Four writers
// take that step - the repair of a session's consumer, an unsubscribe, a registration this
// listener did not answer and the relay's restart - and each builds its write from a bucket read
// the row can move under.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// filterWriteOutcome is what one pass of a fenced filter write answers the fence driving it
// (fenceFilterWrite): what that pass left on the session's consumer, which is what decides
// whether the fence reads the bucket again and whether the filter the loop ends on is a window
// worth a line. Beside it a pass answers the KV revision of the interest row it built its write
// from, which a filterWriteLanded carries and the other three have no use for. The zero value is
// filterWriteLanded, the answer that keeps the fence judging rather than ending it.
type filterWriteOutcome int

const (
	// filterWriteLanded is a write of the pass's own row that stands on the consumer, or may: the
	// server answered the call, or its answer did not come back and the server may have applied
	// it all the same (consumerCallAnswered). The fence reads the bucket again, since a row
	// stored under that write makes the filter standing an older row's.
	filterWriteLanded filterWriteOutcome = iota
	// filterWriteSettled is a pass that left the fence nothing to judge: it sent nothing
	// because the consumer already filters on what its row asks, because the stream holds no
	// consumer to filter at all (bus.ErrSessionConsumerNotFound), which the session's next
	// registration creates, or because the record asks for no consumer of this session, a
	// record an earlier build left, which nothing delivers to and which leaves no delivery a
	// filter is a window in (registeredNoConsumerDelivery). The fence ends on it with no line
	// of its own, whichever pass it is: nothing standing on a consumer this session reads is
	// an older row's filter.
	filterWriteSettled
	// filterWriteSessionGone is a pass that found the session gone: the bucket holds no record
	// of it, so nothing of this listener's sets a filter this loop leaves, and the session
	// registers no more. Only the repair of a session's consumer answers it (repairRegistered),
	// and that repair runs right after the act it follows, under the same lock: a reaper pass
	// deletes the record before the consumer delete that the repair after a delete follows
	// (deleteConsumer), and the record a registration wrote is gone to the repair after that
	// registration only where such a delete ran after that write (confirmRegistered). The fence
	// ends on it by logging what stands rather than on the line that leaves the filter to the
	// next heartbeat, since this loop has none to wait on (fenceFilterWrite).
	filterWriteSessionGone
	// filterWriteNotMade is a pass that planned no write, or whose write the server refused with
	// an answer of its own: nothing of this pass reached the consumer. The fence ends on it, and
	// what stands is what the pass before it left - nothing at all on the first pass, and on a
	// later one a filter the bucket has already answered a newer row than, which is the window
	// fenceFilterWrite names.
	filterWriteNotMade
	// filterWriteUnanswered is a pass whose write's answer did not come back (consumerCallAnswered):
	// the server may still apply it, and may apply it after any read this attempt makes, since its
	// API workers give no order between a write and a later call (bus.UnansweredWrite). No read of
	// this attempt can confirm what stands, so the fence ends on it and the settle loop reads the
	// consumer again; the write itself is judged once its answer comes or is lost
	// (sessionDelivery.unansweredWrite).
	filterWriteUnanswered
)

// filterFenceAttempts bounds the writes one attempt of a fenced filter write makes: the first, and
// four more over rows that landed under it (fenceFilterWrite).
//
// The passes are what makes the filter converge inside one attempt, under the lock its caller
// holds. The registration that stores each of those rows sets the filter of the row it stores
// before it stores it (consumerFor) and judges that filter against the bucket once its entry is
// written (confirmRegistered); both of those are behind it by the time this listener's write
// lands over them, so that registration is answered and gone, and the topic it added stays
// filtered out of the consumer until something of this listener's writes the filter again. An
// attempt out of passes hands the session to the settle loop (settleAgain), which writes it again
// after a pause and holds no lock across it; what the window costs meanwhile is the messages
// stored on that topic, which fenceFilterWrite's doc states.
//
// Against that a pass is cheap: one bucket read and one consumer call, under that session's own
// lock and no other session's. The count is what the fence's own budget can really spend
// (fenceBudget, sessionConsumerCallTimeout): five passes are ten round trips to the server, and
// five seconds cover them at half a second each. One writer racing a repair settles in two
// passes, and five leaves room for the two listeners of a rolling deploy with an unsubscribe or
// a role claim of the same session between them; a row still moving under five writes in a row
// is written to continuously rather than raced once,
// which is what the line an attempt out of passes ends on names.
const filterFenceAttempts = 5

// fenceFilterWrite makes a write of a session consumer's filter and lets no write built from an
// interest row another writer has replaced stand.
//
// Every such write is built from a bucket read and lands at the server after it: the repair of a
// session's consumer (repairRegistered), the unsubscribe's refollow and the registration this
// listener did not answer, which both write through storedFilterWrite, and the relay restart
// (sessionRelays.settleFilter). A row stored between the two makes what lands an older row's
// filter, over the filter a newer registration set, and the session is then answered 200 for a
// topic its consumer does not filter on. What that costs is the messages stored on that topic
// while the older filter stands: the consumer's next pull request moves its position past them,
// whether or not it is answered any message, as does a message stored on a topic the filter
// keeps while a request waits, those two being when the server has the consumer look for its
// next message (bus.SessionConsumers.Ensure). A relay that is pulling sends a request at least
// every 30 s (bus's sessionPullExpires), so a session a relay pulls for loses those messages
// within one pull rather than at its next heartbeat, 90 s away (contracts.SessionHeartbeat); a
// session no relay pulls for is offered them still once the filter is set again, since neither
// a filter update nor the registration that heartbeat makes moves the position by itself
// (bus.SessionConsumers.UpdateFilter, bus.SessionConsumers.Ensure). The server holds no
// conditional consumer update to write that filter under, so the fence is the interest row's KV
// revision read again after the write (storedTopics): a write that landed answers the revision
// of the row it was built from, and while the bucket answers a higher one (rowMovedUnder) the
// loop plans and writes again, ending when the row the write was built from is still the newest.
//
// This function is one attempt of that loop, run under the session's lock its caller holds, and
// it answers how the attempt ended (fenceEnd); what it could not settle is the settle loop's
// (sessionDelivery.fence, settleAgain), which makes the next attempt after a pause and holds the
// lock for each attempt alone. An attempt that ends on an older row's filter logs that at WARN,
// naming the session, the revision it wrote from and the one the bucket answered. Passing again
// inside this attempt is the first retry: handing the session back to the repair path at once
// would buy nothing, since every pass already plans from the bucket's newest record and row. What
// the passes cost is a longer hold of that one session's lock; what stopping costs is the
// messages stored on the topics the two rows differ by, at the price stated above, until the
// settle loop's next attempt, and filterFenceAttempts is where the two are weighed.
//
// An attempt whose read of the bucket after a write that landed failed ends on a filter it cannot
// judge, and logs a line of its own at WARN, naming the session, the revision that write was
// built from and the read's failure. The filter standing is that row's, and whether the bucket
// holds a newer row than it is exactly what the fence could not read, so the line reports a
// filter that may be an older row's rather than one that is; the settle loop judges it again.
//
// The attempt runs on a budget of its own, which no caller's deadline shortens (fenceBudget). Every
// caller starts the fence right after an act of its own - a registration's plans and writes, a
// restart's or a delete's calls, an unsubscribe's removal - and under the session's lock, which
// it may have waited for, and a fence bounded by what that act and that wait left of their
// budget gives up exactly when they were slow, which on a loaded bus is also when the row was
// outrun: its first read or call meets the spent deadline, and the filter of a row the bucket no
// longer holds stands. The caller's cancellation still ends the attempt, with no line and no
// settle loop behind it (fenceCancelled), since it means the caller itself is ending (a relay
// whose run was cancelled, a loop the shutdown stopped). That cancellation is all a caller hands
// the fence (fenceStop), and it holds no deadline.
//
// What one attempt leaves open, each window carrying the cost stated above: a row stored after
// the attempt's last read stands unfenced, since nothing reads the bucket again to find it; a
// corrective pass that plans nothing, or whose write the server refused, leaves the filter of the
// pass before it standing, which the bucket has already answered a newer row than; an attempt out
// of passes leaves the filter of its last pass, under which the row moved once more; a read that
// failed after a write that landed ends the attempt unjudged; a first pass that wrote nothing
// leaves whatever the act before the fence left; and a write whose answer did not come back may
// land after any read of the attempt (bus.UnansweredWrite). The second, third and fourth end the
// attempt on the two lines stated above, the fifth on the failure the pass itself logged, the
// sixth on a line of its own, and the settle loop takes each of them, the sixth after the
// handler of the unanswered write has judged it too (sessionDelivery.unansweredWrite); the first
// is the other writer's own fence's to settle, since that writer stored the row. So an attempt
// ends settled only on what it read or on a write the server answered. A pass that finds
// the consumer already filtering on what its row asks, the stream holding no consumer at all, or
// the record asking for no consumer of this session opens none of these windows and ends the
// attempt with no line, whichever pass it is: nothing of such a pass stands in that session's
// delivery (filterWriteSettled).
//
// A pass that finds the session gone ends the attempt on a third line, which this fence writes
// and no caller of it can: that line reports whether a pass of this attempt landed a write and
// the revision it was built from, which is what landed and stands below hold and what nothing
// outside this attempt reads, so a caller keeping a copy of the pair would drift from the rule
// that sets it. The filter such a write left stands on a consumer no session reads, and that
// consumer is the delete's to take - or, where the pass that landed created it after that
// delete, the stream's inactive threshold's (deleteConsumer, confirmRegistered) - so the line
// names where that filter stands instead of promising a heartbeat that never comes. An attempt
// that landed nothing logs nothing: every reap repairs the session its own delete took the
// record of, and that repair's first pass finds the record gone with no
// write of this attempt's behind it (filterWriteSessionGone).
//
// Both reads are direct gets, so with ENVOY_NATS_REPLICAS above 1 a copy one write behind the
// leader can answer an older row, and an older revision, at either end of the attempt
// (bus.KeyValue.Get states that limit and why this build leaves it standing); rowMovedUnder is
// what keeps such an answer from starting a pass planned from the lagging row it carries.
//
// write plans its write from a bucket read made within ctx, the fence's own budget, budget long
// from the attempt's start (sessionDelivery.fenceAttemptBudget), makes it, and answers what that
// left on the consumer (filterWriteOutcome) with the revision of the interest row it was built
// from. The attempt goes on from filterWriteLanded alone, reading the bucket again to judge the
// filter that write left; filterWriteSettled ends it where nothing of any row stands in the
// session's delivery, filterWriteSessionGone where the session a filter of this attempt's would
// stand for is gone, filterWriteNotMade where nothing of this pass reached the consumer, which
// leaves what the pass before it left standing, and filterWriteUnanswered where the pass's write
// did not answer. Every failure is logged by write itself, which names what it was doing. A
// caller whose first write is already made answers that write from its first pass and writes
// nothing in it (sessionRelays.settleFilter), so that write is one of the passes below.
// after names what the write the fence drives follows, for the session-gone line, the one line
// of the fence's own that carries it: only the repair of a session's consumer answers that
// outcome, so the label that line names is a repair's (repairRegistered,
// repairAfterRegistration, repairAfterDelete). A caller whose passes go through
// storedFilterStep passes the label those passes log their own failures under, and the fence's
// other two lines leave it unnamed (storedFilterWrite, sessionRelays.settleFilter): the settle
// loop's own lines name it, since both end on a filter that loop writes again, the same act
// whatever the write followed.
func fenceFilterWrite(stop fenceStop, budget time.Duration, logger *logging.Logger, registry *store.Registry, sessionID, after string, write func(ctx context.Context) (uint64, filterWriteOutcome)) fenceEnd {
	ctx, cancel := fenceBudget(stop, budget)
	defer cancel()
	// stands is the revision of the row whose filter is on the consumer, and landed says a pass
	// put one there, which stands alone cannot: a write built for a session the bucket holds no
	// row for is fenced on revision 0. newest is what the bucket answered after that write.
	var stands, newest uint64
	landed := false
	// cancelled is the caller's cancellation, which the budget passes on as context.Canceled
	// while its own deadline ends it with context.DeadlineExceeded: an attempt its caller ended
	// is no window to report or to settle again.
	cancelled := func() bool { return errors.Is(ctx.Err(), context.Canceled) }
	for range filterFenceAttempts {
		wrote, outcome := write(ctx)
		if outcome == filterWriteSettled {
			// The consumer is as its row asks, there is none, or the record names none: no
			// filter of any row is a window in what this session is delivered through.
			return fenceSettled
		}
		if outcome == filterWriteSessionGone {
			// The session is gone, so the line leaving the filter to be written again is
			// wrong here. A write of this attempt's standing on the consumer it left behind is
			// what this line reports instead.
			if landed {
				logger.Warn("listener left the filter of an older interest row on the consumer of a session the bucket holds no record for; the session is gone, so no heartbeat sets that filter and the consumer goes with the delete that took the record, or with the stream's inactive threshold",
					slog.String("session_id", sessionID), slog.String("after", after), slog.Uint64("wrote_revision", stands))
			}
			return fenceSessionGone
		}
		if cancelled() {
			return fenceCancelled
		}
		if outcome == filterWriteUnanswered {
			logger.Warn(fenceUnansweredLine, slog.String("session_id", sessionID), slog.Uint64("wrote_revision", wrote))
			return fenceUnanswered
		}
		if outcome == filterWriteNotMade {
			if !landed {
				// Nothing of this attempt's landed, so no older row's filter of its own stands;
				// what stands is what the act before the fence left, which this pass meant to
				// judge and could not.
				return fenceNotWritten
			}
			// A corrective pass that wrote nothing leaves the pass before it standing, and the
			// bucket answered a newer row than that one: that is the line below's case too.
			break
		}
		var err error
		if _, newest, err = storedTopics(ctx, registry, sessionID); err != nil {
			if cancelled() {
				return fenceCancelled
			}
			logger.Warn(fenceReadFailureLine,
				slog.String("session_id", sessionID), slog.Uint64("wrote_revision", stands), slog.String("error", err.Error()))
			return fenceUnjudged
		}
		if !rowMovedUnder(newest, stands) {
			return fenceSettled
		}
	}
	logger.Warn(fenceExhaustionLine,
		slog.String("session_id", sessionID), slog.Uint64("wrote_revision", stands), slog.Uint64("newest_revision", newest))
	return fenceOutrun
}

// fenceExhaustionLine, fenceReadFailureLine and fenceUnansweredLine are the three WARN lines an
// attempt of a fenced filter write ends on when what it leaves is a filter the bucket's row may
// not ask for (fenceFilterWrite): an older row's, the row having moved under every pass or a
// corrective pass having written nothing; one the fence could not judge, its read after a write
// failing; and one whose write's answer did not come back.
const (
	fenceExhaustionLine  = "session consumer filter left on an older interest row than the bucket holds; the listener writes it again"
	fenceReadFailureLine = "session consumer filter written and left unjudged: the interest row could not be read again; the listener judges it again"
	fenceUnansweredLine  = "session consumer filter written and its answer did not come back: the server may apply it after any read of this attempt; the listener judges it again"
)

// fenceEnd is how one attempt of a fenced filter write ended (fenceFilterWrite), which decides
// whether the settle loop makes another (settleAgain).
type fenceEnd int

const (
	// fenceSettled: the filter standing is the row the bucket answered after the attempt's last
	// write, or nothing of any row stands in the session's delivery (filterWriteSettled).
	fenceSettled fenceEnd = iota
	// fenceSessionGone: the bucket holds no record of the session (filterWriteSessionGone), so
	// no filter of it is a window in any delivery and nothing is left to settle.
	fenceSessionGone
	// fenceCancelled: the caller's cancellation ended the attempt (fenceStop).
	fenceCancelled
	// fenceNotWritten: the attempt's first pass wrote nothing - what it would build went unread,
	// or the server refused its call - so what stands is what the act before the fence left.
	fenceNotWritten
	// fenceOutrun: an older row's filter stands, the attempt out of passes or its corrective
	// pass having written nothing (fenceExhaustionLine).
	fenceOutrun
	// fenceUnjudged: a write landed and the bucket could not be read after it
	// (fenceReadFailureLine).
	fenceUnjudged
	// fenceUnanswered: a pass's write did not answer (filterWriteUnanswered, fenceUnansweredLine).
	fenceUnanswered
)

// String is the end as the settle loop's line names it.
func (e fenceEnd) String() string {
	switch e {
	case fenceSettled:
		return "settled"
	case fenceSessionGone:
		return "session gone"
	case fenceCancelled:
		return "cancelled"
	case fenceNotWritten:
		return "nothing written"
	case fenceOutrun:
		return "left on an older row"
	case fenceUnjudged:
		return "left unjudged"
	case fenceUnanswered:
		return "left unanswered"
	}
	return "unknown"
}

// fenceStop is what a caller hands a fenced filter write to end it before its own budget does:
// the caller's cancellation, on a context that holds no deadline (newFenceStop). fenceFilterWrite
// says why no caller's deadline bounds an attempt. A context with a deadline could not even carry
// that cancellation: once its deadline passes, Go's context package detaches it from its parent
// (context.go, cancelCtx.cancel with removeFromParent, removeChild), so a cancellation of the
// parent made after that never reaches it, and a fence built on it would run on after its caller
// stopped. A stop that holds no deadline has nothing to detach it.
type fenceStop struct{ ctx context.Context }

// errFenceStopDeadline is newFenceStop's refusal of a context that holds a deadline.
var errFenceStopDeadline = errors.New("listener refused a fenced filter write's stop: the context holds a deadline, which would stop the fence from hearing its caller's cancellation once it passed; hand it the caller's untimed context")

// newFenceStop is the stop of a fenced filter write that ctx's cancellation ends. It refuses a ctx
// that holds a deadline (errFenceStopDeadline), naming that deadline.
func newFenceStop(ctx context.Context) (fenceStop, error) {
	if deadline, ok := ctx.Deadline(); ok {
		return fenceStop{}, fmt.Errorf("%w (deadline %s)", errFenceStopDeadline, deadline.UTC().Format(time.RFC3339Nano))
	}
	return fenceStop{ctx: ctx}, nil
}

// fenceWithoutStop is the stop of a fenced filter write nothing of its caller's ends: ctx with its
// cancellation dropped (context.WithoutCancel), which holds no deadline either. The writes that
// follow a change to the session's row the caller cannot take back take it (storedFilterWrite,
// confirmRegistered).
func fenceWithoutStop(ctx context.Context) fenceStop {
	return fenceStop{ctx: context.WithoutCancel(ctx)}
}

// fenceBudget is the context one attempt of a fenced filter write runs under: budget long from
// now, the fence's own, and ended at once by stop's cancellation. stop holds no deadline, so
// nothing of the caller's shortens the budget, and a stop cancelled already is a budget cancelled
// already. It answers context.Canceled for the stop and context.DeadlineExceeded for the budget.
func fenceBudget(stop fenceStop, budget time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(stop.ctx, budget)
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
//
// Of the two answers below wrote, only the 0 is one a test of this build can produce, and
// TestAFenceWhoseBucketAnswersNoRowLeavesTheFilterItWroteOnALiveConsumer produces it by deleting
// the row under a write, with the session's record and consumer left standing: against newest !=
// wrote that case empties a live session's consumer to its agent subject. A copy answering a
// revision between 1 and wrote takes a bucket of more than one replica and a read the queue
// group hands to a copy that is behind, which nothing here chooses (bus.KeyValue.Get, which
// records that no test of this build reads a lagging copy); a reader who wants to watch that
// half fail needs a cluster (internal/testnats.StartCluster) with ENVOY_NATS_REPLICAS above 1
// and a way to pin the read to a lagging member, or a bucket handle that answers the row at a
// lower revision than the write it follows.
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

// filterAfterUnsubscribe, filterAfterUnansweredRegistration, filterAfterRestart and
// filterAfterUnansweredWrite name what a write of a session consumer's filter built from the
// bucket's newest row follows, which its log line carries (storedFilterStep, settleAgain): the
// unsubscribe that removed topics from the row (refollow), the registration this listener could
// not answer (registerSession), the relay restart whose own filter write the fence is correcting
// (sessionRelays.settleFilter), or a consumer write whose answer did not come back
// (sessionDelivery.unansweredWrite).
const (
	filterAfterUnsubscribe            = "unsubscribe"
	filterAfterUnansweredRegistration = "unanswered registration"
	filterAfterRestart                = "relay restart"
	filterAfterUnansweredWrite        = "unanswered consumer write"
)

// fence runs one attempt of a fenced write of sessionID's consumer filter (fenceFilterWrite)
// under the session's lock its caller holds, and hands a filter that attempt could not settle to
// the settle loop (settleAgain), which makes the next attempt after a pause: stop is the
// caller's cancellation, which ends the attempt and leaves nothing to settle, since it means the
// caller is ending. ackFloor is what the settle loop's repair starts a consumer it must create
// again at, as repairRegistered's is: the floor the act before the fence read, or 0.
func (m *sessionDelivery) fence(stop fenceStop, registry *store.Registry, sessionID, after string, ackFloor uint64, write func(ctx context.Context) (uint64, filterWriteOutcome)) {
	end := fenceFilterWrite(stop, m.fenceAttemptBudget(), m.logger, registry, sessionID, after, write)
	switch end {
	case fenceSettled, fenceSessionGone, fenceCancelled:
		return
	}
	m.settleAgain(sessionID, after, ackFloor, end.String(), true)
}

// fenceAttemptBudget is how long one attempt of a fenced filter write may wait for the bus:
// sessionConsumerCallTimeout, or fenceBudget where a test sets it, to judge the rule an attempt
// follows apart from how fast the bus answers.
func (m *sessionDelivery) fenceAttemptBudget() time.Duration {
	if m.fenceBudget > 0 {
		return m.fenceBudget
	}
	return sessionConsumerCallTimeout
}

// storedFilterWrite holds the filter of sessionID's consumer to the topics the bucket's newest
// row holds, every pass of it fenced on the revision that row was read at (fenceFilterWrite,
// storedFilterStep). after names what the write follows, for the log.
//
// It owns the three things its callers would otherwise each repeat (refollow, registerSession):
// the listener that holds no session-consumer connection, which only a test builds and which
// writes nothing; the session's lock, which orders these writes against the registrations', the
// relay's restart and the delete a reaper or a stop makes of the same session, and which is taken
// before the fence starts its budget, so a wait behind another call of the same session spends
// none of it (fenceBudget); and the caller's cancellation, which it drops. Each caller writes the
// filter after a change to the session's row that it cannot take back - the topics an
// unsubscribe removed, the plans a registration filtered the consumer on and never stored - so
// the filter follows that change whatever the client does with its answer: a client whose
// attempt ended, which cancels its request, would otherwise leave the session filtered on topics
// it no longer holds. What one attempt could not settle the settle loop writes again (fence).
func (m *sessionDelivery) storedFilterWrite(ctx context.Context, registry *store.Registry, sessionID, after string) {
	if m == nil || m.consumers == nil {
		return
	}
	unlock := m.locks.lock(sessionID)
	defer unlock()
	m.fence(fenceWithoutStop(ctx), registry, sessionID, after, 0, func(ctx context.Context) (uint64, filterWriteOutcome) {
		return m.storedFilterStep(ctx, registry, sessionID, after)
	})
}

// settleAfterAnswer holds the filter of a registration this listener did not answer to the row
// the bucket holds (storedFilterWrite, filterAfterUnansweredRegistration), off the goroutine that
// answers it (runAfterAnswer), so that registration's 503 waits on neither the settle nor the
// session's lock. registerSession's doc says why the settle is needed.
//
// The answer does not wait because the two budgets cannot both be kept inside the client's
// attempt (LISTENER_CALL_TIMEOUT_MS, 5 s). The 503 is answered within the registration's store
// budget (sendRequestBudget, 3.5 s), and that budget is its plans': three contested plans on a
// loaded bus spend it. A settle made before the answer then has only what the plans left, and
// gives up at a deadline that comes from them and not from its own calls, leaving the session
// filtered on a plan nothing stored, with the topic the winning writer stored out of its
// consumer, until its next heartbeat 90 s away (contracts.SessionHeartbeat); a settle made before
// the answer on a budget of its own holds a 503 on a bus that stopped answering past the
// client's attempt (write_routes_frozen_bus_test.go). Off the handler it has its own budget and
// the answer keeps the store's.
//
// ctx's cancellation is dropped (storedFilterWrite), which the handler's own cancel makes
// necessary: it runs as the handler returns. A settle the listener is too late to start, because
// it is stopping, is logged by name and left to the session's next registration, which sets the
// filter of the row it stores.
func (m *sessionDelivery) settleAfterAnswer(ctx context.Context, registry *store.Registry, sessionID string) {
	if m == nil || m.consumers == nil {
		return
	}
	if !m.runAfterAnswer(func() { m.storedFilterWrite(ctx, registry, sessionID, filterAfterUnansweredRegistration) }) {
		m.logger.Warn("session consumer filter not settled after a registration this listener did not answer: the listener is stopping; the session's next registration sets it",
			slog.String("session_id", sessionID))
	}
}

// storedFilterStep is one pass of a fenced write of the filter the bucket's newest interest row
// asks for: it reads that row from registry, sets the consumer's filter to the subjects its
// topics build, and answers the fence driving it with what its call left on the consumer and the
// revision the row was read at (filterWriteOutcome, fenceFilterWrite). It creates no consumer:
// one the stream does not hold is left to the session's next registration, so the server's
// answer for a consumer that is gone (bus.ErrSessionConsumerNotFound) is logged by nothing here
// and ends the fence, there being no consumer for a filter of any row to stand on
// (filterWriteSettled). after names what the write follows, for the log.
//
// A row it could not read is a pass that planned nothing, and a call the server refused one that
// sent nothing: both leave what the pass before them left and end the fence
// (filterWriteNotMade). A call whose answer did not come back may have set this row's filter, or
// may set it later, so it ends the fence for the settle loop to judge (filterWriteUnanswered,
// consumerCallAnswered).
//
// It takes no lock and bounds nothing: the caller holds the session's lock over every pass of
// the fence, and ctx is the fence's own budget (fenceFilterWrite, fenceBudget).
func (m *sessionDelivery) storedFilterStep(ctx context.Context, registry *store.Registry, sessionID, after string) (uint64, filterWriteOutcome) {
	topics, revision, err := storedTopics(ctx, registry, sessionID)
	if err != nil {
		m.logger.Warn("session consumer filter left unchanged: the session's interests could not be read",
			slog.String("session_id", sessionID), slog.String("after", after), slog.String("error", err.Error()))
		return 0, filterWriteNotMade
	}
	if m.afterStoredRowRead != nil {
		m.afterStoredRowRead()
	}
	if err := m.consumers.UpdateFilter(ctx, sessionID, m.storedRowFilter(registry, sessionID, topics)); err != nil {
		if errors.Is(err, bus.ErrSessionConsumerNotFound) {
			return 0, filterWriteSettled
		}
		m.logger.Warn("session consumer filter update failed",
			slog.String("session_id", sessionID), slog.String("after", after), slog.String("error", err.Error()))
		if consumerCallAnswered(err) {
			return 0, filterWriteNotMade
		}
		return revision, filterWriteUnanswered
	}
	return revision, filterWriteLanded
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
// applied the call all the same, or may apply it later. A fenced filter write ends its attempt on
// one (filterWriteUnanswered), because an older row's filter left on a consumer with nothing to
// read it again is exactly what that fence is there to catch; the cost of being wrong is one
// pause, one bucket read and one consumer call that changes nothing.
func consumerCallAnswered(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) ||
		errors.Is(err, bus.ErrConsumerNotGranted) ||
		errors.Is(err, bus.ErrSessionConsumerNotFound) ||
		errors.Is(err, bus.ErrNoSessionConsumerName)
}
