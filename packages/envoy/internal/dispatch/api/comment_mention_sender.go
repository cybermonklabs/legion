package api

import (
	"cmp"
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/clock"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds: those that have not started and
// those in flight, each of which keeps its place so it can park if it must wait for an earlier
// event (mentionSender.held, park). A send past it is not queued: the claim its attempt
// committed with is given back, so the attempt is pending and unclaimed, and the delivery sweep
// sends it once it has waited a claim lease (resumeOwedDeliveries).
const maxQueuedMentionSends = 4096

// mentionDrainTimeout is how long the sends in flight when the server's lifetime ends may run
// before their context is cancelled. It overlaps the HTTP and document drain inside the runtime's
// stop grace (cmd/dispatch/shutdown.go).
const mentionDrainTimeout = 10 * time.Second

// mentionReleaseTimeout bounds the statement the drain makes to give back the claims of the sends
// it never started (wait). It runs on a context the lifetime's end does not cancel, so a stop
// that takes longer than mentionDrainTimeout still leaves those attempts pending and unclaimed
// for the next delivery sweep pass, and it is short because the pool closes behind it.
const mentionReleaseTimeout = 5 * time.Second

// mentionOrderRetryFirst and mentionOrderRetryMax pace a lane whose head send waits for an earlier
// event of its comment's owner (mentionHeldForOrder): the lane is parked, with no goroutine on
// it, and ready again after mentionOrderRetryFirst, a wait that doubles at each park of the same
// send up to mentionOrderRetryMax. The first is short because the outbox publishes an event some
// milliseconds after it commits, so a comment written directly after another event of its issue
// usually waits one retry. The longest is the outbox's own wait before it reads a copy it holds
// for a mention again (mentionPendingDelay in internal/dispatch/outbox): a lane parked through an
// outage reads once each two seconds. Nothing bounds the parks themselves: a bound would let the
// send pass the earlier event, and the wait ends when the outbox sends that event, as the
// outbox's own later sends of the owner wait for it.
const (
	mentionOrderRetryFirst = 50 * time.Millisecond
	mentionOrderRetryMax   = 2 * time.Second
)

// mentionOrderRetry is how long a lane parks after its head send waited for an earlier event the
// parks'th time: mentionOrderRetryFirst doubled for each park before it, at most
// mentionOrderRetryMax.
func mentionOrderRetry(parks int) time.Duration {
	delay := mentionOrderRetryFirst
	for park := 1; park < parks && delay < mentionOrderRetryMax; park++ {
		delay *= 2
	}
	return min(delay, mentionOrderRetryMax)
}

// mentionSend is one comment mention for the mention sender to deliver.
type mentionSend struct {
	comment model.Comment
	created model.Event
	mention ResolvedMention
	actor   model.Actor
	// attempt is the number of the comment_deliveries row the send was queued for: 1 for the
	// send its comment queues, the owed row's for a send a keyed repeat of that comment queues.
	attempt int
	// claimed is the claim this process holds on that row: taken when the row was committed
	// (createCommentFor) or when a keyed repeat put an owed row back on a lane (replayComment),
	// written again while the send waits on its lane (refreshHeldClaims), given back when the
	// sender refuses the send (enqueue), taken over by the send itself when its lane starts it
	// (claimCommentDeliveryAttempt), and handed back to the lane by a send that waited for an
	// earlier event of its comment's owner (mentionHeldForOrder), which renewed it.
	claimed time.Time
	// takeover paces this send's tries at a listener being replaced. The send its comment queues
	// carries the pacing that comment's own resolution started (resolveThroughTakeover), so the
	// resolution and the send share one window; a send a keyed repeat queues starts a window of
	// its own, as its resolution is the sender's own (replayComment), and so does a send handed to
	// the sender held (mentionHeldForOrder.send). A send taken from a held lane starts its window
	// again when its whole delivery starts (sendCommentMention): it waited there - behind an
	// earlier event, a failed read of the rule or of its claim, or a send of its owner ahead of it
	// on the lane - for as long as that took, which has no bound, so a window that started before
	// the wait would have closed before the try it paces, and the first take-over answer would
	// settle the attempt failed with no try again.
	takeover takeoverRetry
	// fromHeldLane is that the lane this send was last taken from is a held lane (next), so its
	// whole delivery starts its take-over window again (takeover).
	fromHeldLane bool
	// parks is how many times this send has waited for an earlier event of its comment's owner,
	// which paces its lane's next try (mentionOrderRetry).
	parks int
	// heldFor is the session this send last waited to reach, behind an earlier event or, its read of
	// the rule having failed, behind none; heldBy is the earlier event it last waited behind, and
	// heldSince when it first waited behind one, both zero until it has; warned is that a park
	// logged its WARN (mentionSender.park, reportLate). A send that waited reads the rule again
	// for heldFor, alone, before it tries once more (sendCommentMention). heldFor is the session the
	// send's attempt is pinned to: the one its last whole delivery resolved and recorded on the
	// attempt's row. A later whole delivery rechecks that session and resolves no role again
	// (mentionResolutionFor), so for a role: mention whose role has moved since, heldFor names the
	// role's previous holder, and the send still goes to that holder. The lone read waits on the
	// events owed to the send's own recipient, and a WARN may name it.
	heldFor   string
	heldBy    model.MentionCopyHolder
	heldSince time.Time
	warned    bool
	// unread is that this send's last try could not read the copy rule, in its whole delivery
	// (mentionHeldForOrder.unread) or alone (sendCommentMention): it waits all the same, behind no
	// event, so its park is no held copy's (serve, park).
	unread bool
}

// held records what h says the send waited for: to reach the session h names, behind h.by, from
// now when it had not waited behind an event before; or, when h's read of the rule failed
// (h.unread), behind no event, which leaves what the send last waited behind as it was and stamps
// no wait. Either way its next try reads the rule alone for that session (sendCommentMention),
// so a send whose read keeps failing makes the whole delivery no more than one the rule holds.
func (m *mentionSend) held(h *mentionHeldForOrder, now time.Time) {
	m.unread = h.unread
	if h.target.SessionID != nil {
		m.heldFor = *h.target.SessionID
	}
	if h.unread {
		return
	}
	m.heldBy = h.by
	if m.heldSince.IsZero() {
		m.heldSince = now
	}
}

// owner is the issue or document the send's comment belongs to, the owner the outbox orders its
// events among.
func (m mentionSend) owner() string {
	_, owner := commentEventOwner(m.comment)
	return owner
}

// mentionKey names one attempt: its comment, its target and its number.
type mentionKey struct {
	comment, target string
	attempt         int
}

func (m mentionSend) key() mentionKey {
	return mentionKey{comment: m.comment.ID, target: m.mention.Target, attempt: m.attempt}
}

// laneKey names a lane of the mention sender. A recipient's own lane names the recipient alone
// (mentionSend.lane); a held lane (heldLane) names the recipient and the owner whose sends to it
// wait there behind an earlier event of that owner, held set, so a comment that names no owner
// still has a held lane apart from its recipient's own.
type laneKey struct {
	recipient string
	owner     string
	held      bool
}

// lane is the recipient a send is ordered by: the session the mention pinned when its lookup
// found one, and the target as written otherwise. role:X shares session:<X's holder>'s lane only
// when its lookup found the holder; an unanswered role:X is keyed by its string, so two comments
// to one recipient, one naming it by role and one by session, can then arrive in either order.
func (m mentionSend) lane() laneKey {
	if m.mention.SessionID != nil {
		return laneKey{recipient: "session:" + *m.mention.SessionID}
	}
	return laneKey{recipient: m.mention.Target}
}

// mentionSender delivers comment mentions after the comment's response, one FIFO lane per
// recipient (mentionSend.lane), so each recipient gets the comments this process accepted on one
// lane in the order it accepted them. role:X shares its holder's lane only when its lookup found
// the holder; an unanswered role:X is keyed by its string, so two comments to one recipient can
// then arrive in either order. A lane is served by at most one goroutine and at most
// mentionSendLanes lanes send at once; a goroutine whose lane still has sends while another lane
// waits moves on to the waiting lane, so one busy recipient cannot hold the others back.
//
// A queued send holds its attempt's claim, so nothing else sends it: the attempt is committed
// claimed (createCommentFor), this sender writes that claim again every mentionClaimRefresh while
// the send waits (refreshHeldClaims), and the lane's own send takes it over
// (claimCommentDeliveryAttempt, which accepts this sender's live claim and refuses another
// sender's). That is what keeps a held lane in order: however long a recipient's first send
// takes, the comments queued behind it are not owed to the delivery sweep, which would otherwise
// send them past it (resumeOwedDeliveries). The outbox counts each of them live for the first
// model.DeliveryClaimLapse of its attempt's life, holding the mentioned session's copy back; past
// that lease it counts the attempt resumed (model.MentionResumedSQL) and keeps that copy as a
// comment_fallback_copies row, which the outbox's fallback pass settles with nothing published
// once the lane's send lands, and publishes should that send fail (publishFallbackCopies in
// internal/dispatch/outbox).
//
// A send also waits for every earlier event of its comment's owner that the outbox still owes the
// mentioned session (mentionHeldForOrder, model.MentionCopyHolderSQL), so the session takes the
// comment after them. Such a send makes no try: it hands its claim back and goes, with every send
// of the same owner queued behind it on its lane, to a held lane of that owner and recipient
// (heldLane), which parks, with no goroutine on it, until mentionOrderRetry of that send's parks
// has passed (park, unpark). The owner's later sends to the recipient queue behind it there; the
// recipient's sends of other owners stay on its lane and go on, since one owner's earlier event
// holds back no other owner's. While a held lane exists the recipient can have two sends in
// flight, one of each lane, and the order between two owners' comments is the order they went in.
// A parked lane sends nothing, so it counts against mentionSendLanes no more than a lane with
// nothing queued, and its sends stay among the queued ones maxQueuedMentionSends bounds, their
// claims written again as every queued send's are.
//
// The statements the sender makes go through its four functions, which register binds to the
// server: send (sendCommentMention), refresh (refreshMentionClaims), reread
// (rereadMentionClaims) and release (releaseMentionClaims).
//
// A refresh whose answer is lost leaves the claim unknown, never assumed, and the sender re-reads
// the row before it acts on that send. A refresh that errs may have committed - the connection
// can drop after its statement did - and then the row carries a claim the sender never learned,
// which no later refresh matches, since each writes only a row that carries the claim the sender
// records: left so, the claim lapses with the send still held, the lane's own send is refused as
// another sender's, and the owner's next comment reaches the recipient first. So every attempt
// whose recorded claim such a refresh named is unsure, and the next refresh (refreshHeldClaims)
// or the send's next try (claimKnown), whichever comes first, reads the row's claim and records
// it before it writes the claim or the try carries it.
//
// Sends run on the server's lifetime without its cancellation: once the lifetime ends nothing more
// is queued, no queued send starts and no claim is written again, and the sends in flight get
// mentionDrainTimeout before their context is cancelled. A send refused once the sender stopped -
// a comment that commits during the HTTP drain - or for the queue's bound gives its attempt's
// claim back, and so does the drain for every send it never started (wait), leaving each attempt
// pending and unclaimed. A send in flight when the process is killed, and a queued one a kill
// leaves no drain for, leave their attempt pending under a claim nothing writes again, which
// lapses a lease after it was last written. Every such attempt is owed, and the
// delivery sweep sends it once its claim has lapsed and it has waited a lease, whichever process
// left it (delivery_sweep.go). Order is kept within one process only: an attempt the sweep
// resumes, after a crash, a handoff or a full queue, can reach its recipient after comments
// accepted later.
type mentionSender struct {
	// lifetime ends when the server shuts down; ctx is what sends run on.
	lifetime context.Context
	ctx      context.Context
	cancel   context.CancelFunc
	// send makes one send, and answers the send to take up again and true when the send waited
	// for an earlier event of its comment's owner and made no try (sendCommentMention).
	send func(context.Context, mentionSend) (mentionSend, bool)
	// refresh writes the claims of the attempts this sender holds again and answers what it
	// wrote; reread answers the claims their rows carry that are this sender's own; release gives
	// claims back.
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error)
	reread  func(context.Context, []mentionClaim) ([]mentionClaim, error)
	release func(context.Context, []mentionClaim) error
	// capacity is maxQueuedMentionSends; only a test lowers it.
	capacity int
	// claimRefresh is how often keepClaims writes the claims of the queued sends again:
	// mentionClaimRefresh, which only a test shortens. Its ticks run on the clock newMentionSender
	// is given for them, not on clock below.
	claimRefresh time.Duration
	// heldWarnAfter is how long a send waits behind an earlier event before park logs its one WARN:
	// model.MentionCopyHeldWarnAfter, which only a test shortens.
	heldWarnAfter time.Duration
	// after starts the timer that ends a park (unpark), once no goroutine serves the parked lane
	// (serve): an AfterFunc on clock, which only a test replaces.
	after func(time.Duration, func())
	// clock is what the parks' timers run on (after) and every moment a held send is stamped and
	// judged with is read from (park, reportLate, heldSamples, and the server's stamps of a send
	// handed back held, sendCommentMention), through sendClock: nil, which every sender
	// newMentionSender builds starts with, is clock.Real(). It is the sender's own and not the
	// server's (Deps.Clock), so a server whose listener windows a test holds still
	// (clocktest.Held) still parks and retries on the wall clock. A test that moves through the
	// parks or past heldWarnAfter itself sets a clocktest fake here before its first send is
	// queued, under mu, as it sets after and heldWarnAfter.
	clock clock.Clock

	// mu guards everything below it. Every process's sender locks a plain *sync.Mutex
	// (newMentionSender); only the claim model gives it a lock of its own, which makes each of the
	// sender's locked sections a step of the model (TestTheMentionSenderClaimModel).
	mu sync.Locker
	// lanes holds each lane's sends that have not started. A key is present while its lane is
	// served, waiting in ready or parked - no goroutine on it, its timer to run (park, unpark) -
	// even once its last send has started.
	lanes map[laneKey][]mentionSend
	// waits is, for each held lane (heldLane), what its sends wait behind: the session and the
	// earlier event its head send last found the session must take first (park). It goes with the
	// lane (next); the held gauges count its lanes' sends (heldSamples), and each send's WARN names
	// it (warnHeld).
	waits map[laneKey]heldWait
	// ready is the lanes waiting for a goroutine, in the order they started waiting.
	ready []laneKey
	// serving is how many goroutines serve lanes.
	serving int
	// held is every attempt this sender queued whose send has not finished - queued, parked or in
	// flight - and what it records of each (mentionClaimState), so that one attempt is never queued
	// twice: a keyed repeat of a comment answers while its first send is still queued. Its size is
	// what capacity bounds, so a send in flight keeps its place, a send that must wait always has
	// room to park (park), and the owner's later sends never pass it.
	held map[mentionKey]mentionClaimState
	// closed is set by wait; nothing starts after it, and stopped is closed with it.
	closed  bool
	stopped chan struct{}
	done    sync.WaitGroup
}

// startMentionSender builds the server's mention sender (register), which sends on the server's
// lifetime and drains for at most mentionDrainTimeout once it ends. An attempt no sender reaches
// is the delivery sweep's (resumeOwedDeliveries). Its claim refreshes run on the real clock.
func (s *server) startMentionSender() *mentionSender {
	return newMentionSender(s.lifetime, mentionClaimRefresh, nil,
		s.sendCommentMention, s.refreshMentionClaims, s.rereadMentionClaims, s.releaseMentionClaims)
}

// newMentionSender builds a mention sender whose keepClaims writes its claims again every
// claimRefresh on refreshes: nil, as startMentionSender passes, is clock.Real(). The refreshes
// have a clock of their own, apart from the server's (Deps.Clock) and the sender's parks (clock),
// so a test that holds or moves either still counts every timer it armed there, and a test of the
// refresh loop passes a fake it moves to each refresh (withMentionClaimRefresh).
func newMentionSender(
	lifetime context.Context,
	claimRefresh time.Duration,
	refreshes clock.Clock,
	send func(context.Context, mentionSend) (mentionSend, bool),
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error),
	reread func(context.Context, []mentionClaim) ([]mentionClaim, error),
	release func(context.Context, []mentionClaim) error,
) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	m := &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send, refresh: refresh, reread: reread, release: release,
		capacity: maxQueuedMentionSends, claimRefresh: claimRefresh, heldWarnAfter: model.MentionCopyHeldWarnAfter,
		mu:    new(sync.Mutex),
		lanes: map[laneKey][]mentionSend{}, waits: map[laneKey]heldWait{},
		held: map[mentionKey]mentionClaimState{}, stopped: make(chan struct{}),
	}
	m.after = func(delay time.Duration, f func()) { m.sendClock().AfterFunc(delay, f) }
	m.done.Add(1)
	go m.keepClaims(cmp.Or[clock.Clock](refreshes, clock.Real()))
	return m
}

// sendClock is the clock the sender times its parks and reads its moments on: its clock, and
// clock.Real() while that is nil, as production's is.
func (m *mentionSender) sendClock() clock.Clock {
	if m.clock == nil {
		return clock.Real()
	}
	return m.clock
}

// stoppedLocked reports whether the sender starts nothing more: the lifetime ended, or wait ran.
func (m *mentionSender) stoppedLocked() bool {
	return m.closed || m.lifetime.Err() != nil
}

// enqueue queues one send behind the earlier sends of its lane. A send refused - the sender has
// stopped, or the queue is full - gives its attempt's claim back, so the attempt is pending with
// no claim, as it is when no sender ever held it.
func (m *mentionSender) enqueue(ctx context.Context, item mentionSend) {
	m.mu.Lock()
	queued, full := m.queueLocked(item)
	m.mu.Unlock()
	if full {
		slog.Warn("dispatch: comment mention queue full; the delivery sweep sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
	}
	if queued {
		return
	}
	if err := m.release(ctx, []mentionClaim{item.claim()}); err != nil {
		slog.Error("dispatch: give back the claim of a comment mention this sender refused",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
}

// queueLocked queues item unless the sender stopped or the queue is full, which it reports, and
// starts a goroutine for its lane when the lane has none and fewer than mentionSendLanes run. It
// reports whether this sender holds item's attempt once it returns: it queued it, or it holds it
// already, a keyed repeat answering while the first send is still queued, whose fresh claim this
// sender takes over for it. A send refused because the sender stopped is logged here.
func (m *mentionSender) queueLocked(item mentionSend) (queued, full bool) {
	if m.stoppedLocked() {
		slog.Warn("dispatch: comment mention not sent: the mention sender has stopped; the delivery sweep sends the pending attempt",
			"comment", item.comment.ID, "target", item.mention.Target)
		return false, false
	}
	key := item.key()
	if state, mine := m.held[key]; mine {
		// A keyed repeat claimed the attempt anew before it asked (claimQueuedMention), so the
		// claim its send waits under is the one that repeat just wrote, and the refreshes and the
		// drain's release move that one. A send in flight is among them too, and parks under that
		// claim unless its try claims the attempt for itself (park).
		state.claimed, state.unsure = item.claimed, false
		m.held[key] = state
		return true, false
	}
	if len(m.held) >= m.capacity {
		return false, true
	}
	m.held[key] = mentionClaimState{claimed: item.claimed}
	lane := item.lane()
	if held := heldLane(lane, item.owner()); held != lane {
		if _, waits := m.lanes[held]; waits {
			// The owner's sends to this recipient wait behind one of them that waits for an
			// earlier event (park), or whose claim could not be read: this one goes behind them,
			// so the owner's order holds, and waits from now (heldSamples) when the lane waits
			// behind an event.
			lane = held
			if _, waiting := m.waits[held]; waiting && item.heldSince.IsZero() {
				item.heldSince = m.sendClock().Now()
			}
		}
	}
	waiting, scheduled := m.lanes[lane]
	m.lanes[lane] = append(waiting, item)
	switch {
	case scheduled:
	case m.serving < mentionSendLanes:
		m.serving++
		m.done.Add(1)
		go m.serve(lane)
	default:
		m.ready = append(m.ready, lane)
	}
	return true, false
}

// serve sends lane's queue, then any lane waiting for a goroutine, until none waits. A send whose
// recorded claim is unsure is tried only once its row's claim is read (claimKnown); one whose
// read fails makes no try and parks as a send that waited does, behind no event, and so does one
// whose try could not read the copy rule (mentionSend.unread). A send that waited for an earlier
// event of its comment's owner parks the owner's sends to its recipient (park), and the timer that
// ends the park (unpark) starts only once no goroutine serves the parked lane: at once when the
// park made a held lane beside the one this goroutine serves, and, when the parked lane is the one
// this goroutine serves - a held lane whose head waited again - only once this goroutine has given
// it up (next) and taken a lane that waits, or ended. So no two goroutines ever serve one lane,
// and a parked lane's timer is pending exactly while no goroutine is on it. A send that leaves a
// held lane past heldWarnAfter with no WARN logged for it logs its WARN as it goes (reportLate).
func (m *mentionSender) serve(lane laneKey) {
	defer m.done.Done()
	// givenUp starts the timer of the lane this goroutine parked and gives up, once it has.
	var givenUp func()
	for {
		item, next, ok := m.next(lane)
		if givenUp != nil {
			givenUp()
			givenUp = nil
		}
		if !ok {
			return
		}
		lane = next
		item, known := m.claimKnown(item)
		again, waits := item, true
		for known {
			// The try stays marked in flight until park records its claim, under park's lock, so
			// no re-read records the try's own claim in between (markTrying, park).
			m.markTrying(item.key())
			if again, waits = m.send(m.ctx, item); waits {
				break
			}
			// A hand-off recorded a newer claim while the try was in flight - the delivery sweep's
			// or a keyed repeat's (queueLocked), which this send stands for - and the try, carrying
			// the older one, may have been refused for it: the send is tried again under the claim
			// recorded, which finds the attempt settled if the try sent it. When none did, the
			// send has ended (claimedDuring).
			var moved bool
			if item.claimed, moved = m.claimedDuring(item); !moved {
				break
			}
		}
		if !waits {
			m.reportLate(lane, item)
			continue
		}
		claimed := !again.claimed.Equal(item.claimed)
		held, delay := m.park(lane, again, claimed, known && !again.unread)
		wake := func() { m.after(delay, func() { m.unpark(held) }) }
		if held == lane {
			lane, givenUp = laneKey{}, wake
			continue
		}
		wake()
	}
}

// next takes the next send of the goroutine serving lane, and the lane it now serves. lane is the
// zero laneKey when the goroutine gave its lane up to a park (serve), and it then takes a lane that
// waits. The send it takes carries the claim this sender records for it, which stays among the
// claims the refresh writes again until the send ends (claimedDuring): a try that reads the rule
// alone writes no claim, so the claim its row carries is still that one, and a refresh that writes
// the row while the try is in flight records the claim it wrote (held). It carries too whether
// the lane it takes the send from is a held lane, where the send waited (mentionSend.fromHeldLane).
// ok is false when the goroutine is done: the sender stopped, or no lane has a send left for it.
func (m *mentionSender) next(lane laneKey) (mentionSend, laneKey, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stoppedLocked() {
		m.serving--
		return mentionSend{}, laneKey{}, false
	}
	if len(m.lanes[lane]) > 0 && len(m.ready) > 0 {
		m.ready = append(m.ready, lane)
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	for len(m.lanes[lane]) == 0 {
		delete(m.lanes, lane)
		delete(m.waits, lane)
		if len(m.ready) == 0 {
			m.serving--
			return mentionSend{}, laneKey{}, false
		}
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	item := m.lanes[lane][0]
	m.lanes[lane][0] = mentionSend{}
	m.lanes[lane] = m.lanes[lane][1:]
	item.claimed = m.held[item.key()].claimed
	item.fromHeldLane = lane.held
	return item, lane, true
}

// claimedDuring answers the claim this sender records for item's attempt, and whether it moved
// while item's try was in flight, which carried item.claimed: a hand-off took a newer claim for the
// send (queueLocked), or a refresh wrote the row before the try's take did. A try that ended not
// waiting under the older claim may have been refused for the newer one, and one under the newer
// claim finds the attempt settled if the first sent it; a re-read records nothing while a try is in
// flight. When the claim did not move the send has ended, and its attempt is released here, under
// the lock that found no hand-off: one that lands after this queues the attempt anew (queueLocked)
// rather than into the send that ends, which would take it along unsent.
func (m *mentionSender) claimedDuring(item mentionSend) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := item.key()
	if state, mine := m.held[key]; mine && !state.claimed.Equal(item.claimed) {
		return state.claimed, true
	}
	delete(m.held, key)
	return item.claimed, false
}

// heldLane is the lane the sends of owner to lane's recipient wait on while one of them waits for
// an earlier event of owner (park): a lane of its own beside lane, so a recipient's sends of other
// owners pass them. A lane that is one already is its own.
func heldLane(lane laneKey, owner string) laneKey {
	if lane.held {
		return lane
	}
	return laneKey{recipient: lane.recipient, owner: owner, held: true}
}

// park puts item, the send lane just started that waited for an earlier event of its comment's
// owner, or whose unsure claim could not be read (serve), at the head of that owner's held lane
// for the recipient (heldLane), and leaves the held lane with no goroutine until
// mentionOrderRetry of item's parks has passed (unpark). Its claim is the one its try took for
// itself when claimed is set, which this sender records and writes again from here as it writes
// every queued send's (refreshHeldClaims), and otherwise the one it records already: a try that
// read the rule alone took none, and a refresh that wrote its row while it was in flight recorded
// what it wrote, which item, carrying the claim the try started with, would set back. Every send
// of the same owner still queued on lane moves with it, in order, and every later one queues
// behind them (queueLocked), so the owner's sends keep their order; the recipient's sends of
// other owners stay on lane and go on, as an earlier event of one owner holds no event of
// another (planChains keeps order per owner). The held lane is deleted once it is empty and
// served (next), after which the owner's sends join lane again. Its attempt stays held, so a
// keyed repeat of the comment queues it no second time. A park once the sender stopped still
// records the claim, which the drain gives back with the rest (wait). Its send kept its place in
// the queue while it was in flight (held), so the park never passes the queue's capacity and
// never has to give its place to the owner's later sends. It answers the held lane and how long
// it parks: its caller starts the timer that ends the park (unpark) once no goroutine serves that
// lane, which is at once unless it is lane itself (serve).
//
// The wait has no bound, so it shows instead: the held gauges count every send on a held lane
// that waits behind an earlier event (heldSamples), each from when it started waiting there, and
// each send held heldWarnAfter since it first waited - the one that waited and every send of its
// owner queued behind it - is logged once at WARN (warnHeld), naming its owner, the earlier event
// it waits behind and that event's last publish error, beside the ERROR the outbox writes for that
// event's own failed publish, which is the cause: here when a park finds it past the threshold,
// else as it leaves the lane (reportLate). A send parked because its claim could not be read waits
// behind no event: it is stamped as waiting (heldSince) only once a park behind one finds it on the
// held lane, so it is counted and warned of only from then.
func (m *mentionSender) park(lane laneKey, item mentionSend, claimed, waited bool) (held laneKey, delay time.Duration) {
	item.parks++
	now := m.sendClock().Now()
	if waited && item.heldSince.IsZero() {
		item.heldSince = now
	}
	delay = mentionOrderRetry(item.parks)
	owner := item.owner()
	held = heldLane(lane, owner)
	m.mu.Lock()
	state := m.held[item.key()]
	state.trying = false
	if claimed {
		state.claimed, state.unsure = item.claimed, false
	}
	m.held[item.key()] = state
	if held == lane {
		m.lanes[held] = append([]mentionSend{item}, m.lanes[held]...)
	} else {
		moved := []mentionSend{item}
		waiting := m.lanes[lane]
		kept := waiting[:0]
		for _, queued := range waiting {
			if queued.owner() != owner {
				kept = append(kept, queued)
				continue
			}
			if waited && queued.heldSince.IsZero() {
				queued.heldSince = now
			}
			moved = append(moved, queued)
		}
		clear(waiting[len(kept):])
		m.lanes[lane] = kept
		m.lanes[held] = moved
	}
	// A send parked because its claim could not be read waited behind no event: it is no held
	// copy the gauges count, and it logs no WARN naming an earlier event.
	wait := heldWait{session: item.heldFor, by: item.heldBy}
	var warned []mentionSend
	if waited {
		m.waits[held] = wait
		for index := range m.lanes[held] {
			send := &m.lanes[held][index]
			if send.heldSince.IsZero() {
				send.heldSince = now
			}
			if !send.warned && now.Sub(send.heldSince) >= m.heldWarnAfter {
				send.warned = true
				warned = append(warned, *send)
			}
		}
	}
	m.mu.Unlock()
	for _, send := range warned {
		warnHeld(send, wait, now)
	}
	return held, delay
}

// unpark makes a parked lane ready again: a goroutine starts on it when fewer than
// mentionSendLanes run, and otherwise it waits in ready behind the lanes already there. Once the
// sender stopped it starts nothing, the lane's claims being the drain's to give back (wait).
func (m *mentionSender) unpark(lane laneKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stoppedLocked() {
		return
	}
	if m.serving < mentionSendLanes {
		m.serving++
		m.done.Add(1)
		go m.serve(lane)
		return
	}
	m.ready = append(m.ready, lane)
}

// wait stops the sender from starting anything more, waits for the sends in flight, and gives
// back the claims of the sends that never started: a process that stops knows it will send none
// of them, so their attempts are left pending and unclaimed and the next delivery sweep pass
// sends them once they have waited a lease, rather than a lease after this process last wrote
// their claims. That release runs on a context of its own, which the lifetime's end does not
// cancel and mentionReleaseTimeout bounds, so a stop that takes longer than mentionDrainTimeout
// still gives the claims back rather than failing cancelled. Call it once the lifetime has ended,
// before the pool closes.
func (m *mentionSender) wait() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.stopped)
	}
	m.mu.Unlock()
	m.done.Wait()
	// No goroutine serves a lane now and nothing more is queued, so what is left is final.
	m.mu.Lock()
	unstarted := make([]mentionClaim, 0, len(m.held))
	for key, state := range m.held {
		unstarted = append(unstarted, mentionClaim{key: key, claimedAt: state.claimed})
	}
	clear(m.held)
	m.mu.Unlock()
	if len(unstarted) > 0 {
		release, cancelRelease := context.WithTimeout(
			context.WithoutCancel(m.lifetime), mentionReleaseTimeout)
		defer cancelRelease()
		if err := m.release(release, unstarted); err != nil {
			slog.Error("dispatch: give back the claims of the comment mentions this sender never started",
				"attempts", len(unstarted), "error", err)
		}
	}
	m.cancel()
}

// sendCommentMention is the mention sender's send: deliverResolvedCommentMention bound to the
// attempt the send was queued for, to the claim this sender holds on it, so it takes that
// attempt over rather than opening another, and to the take-over pacing the send carries. A
// failure is logged and leaves the attempt pending, as deliverBroadcast's does, for the delivery
// sweep once the claim it took lapses. A send that waited for an earlier event of its comment's
// owner, or whose read of that rule failed (mentionHeldForOrder), answers the send to take up
// again, which its lane parks (park) on a held lane: the same attempt, under the claim that send
// renewed. A send taken from a held lane starts its take-over window when its whole delivery
// starts (mentionSend.takeover), since its wait there has no bound.
//
// A send that waited before - behind an earlier event, or behind none because its read of the rule
// failed - reads the rule alone first, for the session it waited to reach (mentionCopyHolderFor),
// and parks again while the earlier event still owes its send, or while that read fails: a parked
// lane's try then costs the rule's one statement (copyrule.Holder) and, for each role route the
// earlier events owe, one listener lookup of the role's holder (roleHolderMayBe, within
// mentionLookupTimeout); no claim and no write, while its claim is the one the sender writes again
// for every queued send. A read that fails parks the send as one whose whole delivery could not
// read the rule does (mentionSend.unread): behind no event, keeping its claim and its place, so
// the owner's later comments to the session stay behind it. Only a send the rule lets go makes the
// whole delivery, which claims, resolves and asks the rule once more.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) (mentionSend, bool) {
	ctx = store.WithTransactionTracking(ctx)
	if item.heldFor != "" {
		by, err := s.mentionCopyHolderFor(ctx, item.created, item.heldFor)
		switch {
		case err != nil:
			slog.Error("dispatch: read whether a comment mention's session must take an earlier event first; the send waits",
				"comment", item.comment.ID, "target", item.mention.Target, "attempt", item.attempt, "error", err)
			item.unread = true
			return item, true
		case by != nil:
			item.heldBy, item.unread = *by, false
			return item, true
		}
	}
	if item.fromHeldLane {
		item.takeover = s.newTakeoverRetry()
	}
	_, held, err := s.deliverResolvedCommentMention(
		ctx, item.comment, item.created, item.mention, item.actor,
		attemptClaim{attempt: item.attempt, claimedAt: item.claimed}, item.takeover,
	)
	if held != nil {
		item.claimed = held.claim.claimedAt
		item.held(held, s.mentions.sendClock().Now())
		return item, true
	}
	if err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
	return mentionSend{}, false
}
