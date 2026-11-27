// The policy for a message on the agent subject of a session this listener finds no record of:
// the one wait before the session's wake is reported, the set that caps how many first deliveries
// of such messages wait at once and expires a slot whose message never comes back, the report
// itself, and a read of the session record that did not answer (unreadAgentDelivery).

package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// unplaceableDeferralCap bounds how many first deliveries of agent messages this listener holds
// back for the agent wait at one time, across every handler it builds (unplaceableDeferrals). A
// message that waits is NAKed rather than ACKed, so it keeps one of the durable's MaxAckPending
// slots (consumerMaxAckPending, durable.go: 256) for the whole wait instead of giving it back at
// once: without a bound, enough messages to sessions with no record would hold every slot this
// machine's durable has and stall it for every topic. A quarter of the budget leaves three
// quarters for that traffic. A first delivery that arrives while the cap is reached reports its
// wake at once. A message sent back after a report the stream did not store, or after a read of
// the record that did not answer, takes a free slot when there is one and is sent back whether or
// not it got one (reportNoRecordWake, unreadAgentDelivery), so the cap bounds the first
// deliveries' waits alone. TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the relation.
const unplaceableDeferralCap = consumerMaxAckPending / 4

// defaultUnplaceableRetryWindow is the agent wait main.go uses when ENVOY_UNPLACEABLE_RETRY_WINDOW
// is unset or unreadable: a minute, long enough for a session whose process is restarting to
// register again, so its consumer delivers the message and no wake is reported.
const defaultUnplaceableRetryWindow = 60 * time.Second

// unplaceableDeferralExpiry is how long a slot in unplaceableDeferrals lasts after the delivery
// that took it, for a set whose wait is window: the longest a message that waits can stay pending
// before a delivery of it reaches this listener's handler again and gives the slot back. It is the
// sum of three waits:
//   - the window (none when it is at or below zero), the NAK delay the wait asks for: the stream
//     redelivers no sooner;
//   - consumerAckWait (durable.go, 60 s): finalizeAfter's NAK is fire-and-forget, and one lost on
//     the wire leaves the message pending until the AckWait of the delivery that took the slot
//     runs out, which began before the slot was taken, so it ends inside this term;
//   - a margin of one more consumerAckWait for the redelivery to wait in this listener's client
//     buffer behind the deliveries ahead of it before its handler runs the release. A handler that
//     is further behind than that has let the stream redeliver the message again in the meantime.
//
// 180 s at the default window. A slot older than that belongs to a message that is not coming
// back here - the stream dropped or purged it, or its redelivery reached another subscriber - and
// take reclaims it. TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins that it exceeds the
// first two terms, so a message that is still pending never has its slot reclaimed early.
func unplaceableDeferralExpiry(window time.Duration) time.Duration {
	return max(window, 0) + 2*consumerAckWait
}

// unplaceableDeferrals is the set of agent messages this listener holds back for the agent wait
// right now, keyed on each message's stream sequence (nats.MsgMetadata.Sequence.Stream), and the
// budget unplaceableDeferralCap bounds. It is keyed rather than counted so that only a slot this
// listener took ever comes back: a redelivery is no evidence that this listener held the message
// back - an expired AckWait redelivers too - so a count released on every redelivery could read
// zero while the cap's worth of messages were still held.
//
// What the set guarantees: it never holds more than unplaceableDeferralCap live slots, and a slot
// is added only by a wait this listener started. A slot leaves by one of two routes: that same
// message's next delivery releases it, or it expires unplaceableDeferralExpiry after the delivery
// that took it, and the next take or size sweeps it out. So at any moment the first deliveries
// this process holds back, within that bound, number at most the cap. A message the not-stored
// and unread paths send back is sent back whether or not it got a slot, so the cap does not bound
// those messages. What it cannot know, since it is this process's memory and not the stream's:
//   - a listener restart forgets it. The previous process's waiting messages stay pending in the
//     durable until each one's NAK delay ends and it is redelivered; until then the durable can
//     hold the old process's cap beside the new one's, which
//     TestUnplaceableRetryWindowFitsTheBoundsItDependsOn keeps under consumerMaxAckPending (twice
//     the cap is below it).
//   - a message held back and never delivered here again - one the stream drops or purges first,
//     or whose redelivery reaches some other subscriber - keeps its slot until it expires. Such a
//     slot holds no slot of the durable's; until it expires it holds one of the set's, so a cap's
//     worth of them has each new message report its wake at once, for at most
//     unplaceableDeferralExpiry.
//   - a message still pending past unplaceableDeferralExpiry - a handler further behind its
//     redeliveries than the expiry's margin allows - loses its slot while the durable still holds
//     it, and the set undercounts by one such message until its delivery arrives.
//
// unread is the second record the set keeps, on the same keys and the same expiry: the messages
// for which this listener has logged that a read of the session record did not answer, so it logs
// that once per message however many deliveries the failing read spans (firstUnread).
type unplaceableDeferrals struct {
	mu sync.Mutex
	// expiry is how long a slot or an unread mark lasts after the delivery that took it
	// (unplaceableDeferralExpiry).
	expiry time.Duration
	// held maps each held message's stream sequence to the time its slot expires.
	held map[uint64]time.Time
	// unread maps the stream sequence of each message whose unanswered read was logged to the
	// time that mark expires.
	unread map[uint64]time.Time
	// now is the clock the expiries are read against: time.Now, which a test replaces to step
	// past an expiry without waiting it out.
	now func() time.Time
}

// newUnplaceableDeferrals is the set for a listener whose agent wait is window.
func newUnplaceableDeferrals(window time.Duration) *unplaceableDeferrals {
	return &unplaceableDeferrals{
		expiry: unplaceableDeferralExpiry(window),
		held:   make(map[uint64]time.Time, unplaceableDeferralCap),
		unread: make(map[uint64]time.Time),
		now:    time.Now,
	}
}

// take records the message at stream sequence seq as held back and reports whether it fits inside
// unplaceableDeferralCap. It first sweeps out every slot and mark whose expiry has come, so
// neither the cap check nor the held check below counts a message that is not coming back; an
// expired entry for seq itself is swept like any other, and seq is taken afresh. Past the cap it
// records nothing and reports false. A sequence already held reports true without a second slot:
// one message holds one slot however many deliveries of it race, and its expiry is counted again
// from this delivery, whose NAK restarts the wait the expiry bounds. The sweep, the check and the
// insert are one critical section, so two deliveries racing for the last slot cannot both take it.
func (d *unplaceableDeferrals) take(seq uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.sweepLocked(now)
	if _, ok := d.held[seq]; !ok && len(d.held) >= unplaceableDeferralCap {
		return false
	}
	d.held[seq] = now.Add(d.expiry)
	return true
}

// release gives back the slot the message at stream sequence seq holds, and nothing when it holds
// none: a delivery this listener never held back has no slot to return.
func (d *unplaceableDeferrals) release(seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, seq)
}

// firstUnread marks the message at stream sequence seq as one whose unanswered read this listener
// logged, and reports whether it was not marked yet, which is when the caller logs it. A mark
// already there is kept, its expiry counted again from this delivery, so a read that keeps failing
// is logged once for as long as the message keeps coming back.
func (d *unplaceableDeferrals) firstUnread(seq uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.sweepLocked(now)
	_, marked := d.unread[seq]
	d.unread[seq] = now.Add(d.expiry)
	return !marked
}

// size reports how many live slots are held, for a test to read the budget back. It sweeps the
// expired ones out first, as take does, so it reports the count take's cap check would see.
func (d *unplaceableDeferrals) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(d.now())
	return len(d.held)
}

// sweepLocked deletes every slot and mark whose expiry is at or before now. The caller holds d.mu.
func (d *unplaceableDeferrals) sweepLocked(now time.Time) {
	for _, entries := range []map[uint64]time.Time{d.held, d.unread} {
		for seq, expiry := range entries {
			if !now.Before(expiry) {
				delete(entries, seq)
			}
		}
	}
}

// agentWait is the one wait of a message on the agent subject of a session with no record
// (unplaceableRetryWindow), and zero for a window at or below zero, which is no wait.
func (cfg listenerDeliveryHandlerConfig) agentWait() time.Duration {
	return max(cfg.unplaceableRetryWindow, 0)
}

// noRecordAgentDelivery decides a message on the agent subject of a session with no record, or
// with a record past the gone horizon (session.AgentNoRecord). Its first delivery waits once: it
// takes a slot in the set and is sent back for the agent wait, with no line logged, so that a
// session that is only restarting registers again and its consumer delivers the message with no
// wake. Its next delivery reads the record again (agentSubjectDelivery), and when there is still
// none it reports the session's wake (reportNoRecordWake). A message that has no wait to take
// reports at once: under a window at or below zero; on a core delivery, or a durable one whose
// metadata does not parse, since nothing bounds its wait; and on a first delivery that finds
// unplaceableDeferralCap messages already waiting. Every later delivery reports at once too: the
// one wait is spent, whether it was this path's or the one an unanswered read took
// (unreadAgentDelivery).
func noRecordAgentDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) {
	if metadata := message.metadata; metadata != nil && metadata.NumDelivered == 1 && cfg.agentWait() > 0 &&
		cfg.deferredUnplaceable.take(metadata.Sequence.Stream) {
		sendAgentMessageBack(cfg, message, item, sessionID)
		return
	}
	reportNoRecordWake(cfg, message, item, sessionID)
}

// reportNoRecordWake reports the wake of sessionID, a session with no record, with one publish the
// stream must store (storeAgentWake), and settles the message on its answer. A stored report is
// logged and counted delivery_status="wake_reported", and the message is acknowledged; its attempt
// entry stays, so a redelivery an acknowledgement lost on the wire brings is an attempt-dedupe
// skip and reports no second wake. A report the stream did not store sends the message back for
// the agent wait, so its next delivery reads the record again and tries the report again; it takes
// a free slot when there is one, and is sent back whether or not it got one. At the stream's last
// delivery (consumerMaxDeliver), or with nothing that redelivers or bounds the message - a core
// delivery, or metadata that does not parse - a report not stored is logged and the message is
// acknowledged, since the stream sends it no more.
//
// An edge listener reports no wake (cfg.edge): its grant may not publish the wake topic
// (natsgrant.Edge), so the stream would never store the report and the message would be sent
// back at every delivery. It acknowledges the message with no publish, logged and counted
// delivery_status="wake_left_to_shared". The shared listeners read the same message from their own
// durables and report the wake when no listener holds the session's interest row. A session whose
// row the edge holds gets no wake from any listener.
func reportNoRecordWake(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) {
	if cfg.edge != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "wake_left_to_shared",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "edge listener left the agent wake to the shared listeners", sessionID, item.Topic, item.EventID, "wake_left_to_shared")
			},
		})
		message.finalize(false)
		return
	}
	err := storeAgentWake(cfg.client, cfg.logger, sessionID)
	if err == nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "wake_reported",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener agent wake reported", sessionID, item.Topic, item.EventID, "wake_reported")
			},
		})
		message.finalize(false)
		return
	}
	if metadata := message.metadata; metadata != nil && metadata.NumDelivered < consumerMaxDeliver {
		cfg.deferredUnplaceable.take(metadata.Sequence.Stream)
		sendAgentMessageBack(cfg, message, item, sessionID)
		return
	}
	cfg.logger.DeliveryLog(slog.LevelWarn, "listener agent wake not stored", sessionID, item.Topic, item.EventID, "wake_not_stored",
		slog.String("error", err.Error()))
	message.finalize(false)
}

// unreadAgentDelivery decides a message whose session record could not be read: the read erred or
// timed out (session.AgentRecordUnread), so this listener cannot tell a live session from a gone
// one. It logs first and then decides. The first such answer for a message logs one WARN with the
// read's error, and no later one does (unplaceableDeferrals.firstUnread), at the last delivery
// too. A core delivery, or a durable one whose metadata does not parse, has no stream sequence to
// mark, so it logs the WARN at once. Below the stream's last delivery (consumerMaxDeliver) the
// message takes a free slot when there is one, is sent back for the agent wait whether or not it
// got one, and is decided again at its next delivery. At the last delivery the message is decided
// as one with no record (reportNoRecordWake); so is a core delivery, or a durable one whose
// metadata does not parse, which has no next delivery to decide it. The wake comes only from a
// last delivery this process settles: JetStream stops at max_deliver with no word, so a 20th
// delivery the server counted but this process never settled (a crash, a kill, the bounded
// shutdown while it runs) ends the message with no wake and no line.
func unreadAgentDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, readErr error) {
	metadata := message.metadata
	if metadata == nil || cfg.deferredUnplaceable.firstUnread(metadata.Sequence.Stream) {
		cfg.logger.DeliveryLog(slog.LevelWarn, "listener agent record unread", sessionID, item.Topic, item.EventID, "record_unread",
			slog.String("error", readErr.Error()))
	}
	if metadata == nil || metadata.NumDelivered >= consumerMaxDeliver {
		reportNoRecordWake(cfg, message, item, sessionID)
		return
	}
	cfg.deferredUnplaceable.take(metadata.Sequence.Stream)
	sendAgentMessageBack(cfg, message, item, sessionID)
}

// sendAgentMessageBack asks the stream to deliver an agent message again after the agent wait. The
// attempt entry agentSubjectDelivery recorded goes first, or the redelivery would be skipped as one
// already on its way, and the message is counted NAKed.
func sendAgentMessageBack(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) {
	cfg.attemptCache.Clear(item.DedupeKey, sessionID)
	cfg.messagesNAKed.Inc()
	message.finalizeAfter(cfg.agentWait())
}
