package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds: those that have not started and
// those in flight, each of which keeps its place so it can park if it must wait for an earlier
// event (mentionSender.held, park). A send past it is not queued: the claim its attempt
// committed with is given back, so the attempt is pending and unclaimed, and the delivery sweep
// sends it once it has waited a claim lease (resumeOwedDeliveries).
const maxQueuedMentionSends = 4096

// mentionClaimRefresh is how often the mention sender writes the claims of the sends waiting on
// its lanes again (refreshHeldClaims). It is well inside model.DeliveryClaimLapse, so a claim this
// sender holds never lapses while the process is alive however long its lane is held, and a
// process that stops or dies writes none again, so every claim it still held lapses within one
// lease and the delivery sweep takes those attempts.
const mentionClaimRefresh = model.DeliveryClaimLapse / 3

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
	// its own, as its resolution is the sender's own (replayComment), and so does a send that
	// waited for an earlier event, which made no try.
	takeover takeoverRetry
	// parks is how many times this send has waited for an earlier event of its comment's owner,
	// which paces its lane's next try (mentionOrderRetry).
	parks int
	// heldFor is the session this send last waited to reach, heldBy the earlier event it waited
	// behind, and heldSince when it first waited, all zero for a send that has not waited; warned
	// is that a park logged its WARN (mentionSender.park, reportLate). A send that waited reads the
	// rule again for heldFor before it tries once more (sendCommentMention).
	heldFor   string
	heldBy    model.MentionCopyHolder
	heldSince time.Time
	warned    bool
}

// held records that the send waited behind h.by to reach the session h names, from now when it
// had not waited before.
func (m *mentionSend) held(h *mentionHeldForOrder) {
	m.heldBy = h.by
	if h.target.SessionID != nil {
		m.heldFor = *h.target.SessionID
	}
	if m.heldSince.IsZero() {
		m.heldSince = time.Now()
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

// mentionClaim is one attempt the sender holds and the claim it last wrote on it.
type mentionClaim struct {
	key       mentionKey
	claimedAt time.Time
}

func (m mentionSend) claim() mentionClaim {
	return mentionClaim{key: m.key(), claimedAt: m.claimed}
}

// mentionClaimState is what the mention sender records of one attempt it holds
// (mentionSender.held).
type mentionClaimState struct {
	// claimed is the claim this sender holds on the attempt, as it last wrote it, so one statement
	// writes the claims of every attempt it holds again: every send that has not started, and a
	// send in flight until it ends (next). A try that claims the attempt for itself takes that
	// claim out of the statements' reach, as each matches only the claim recorded here, and park
	// records the try's; a try that reads the rule alone (sendCommentMention) writes no claim, so
	// its send's row still carries the one recorded here, and a refresh that writes it meanwhile
	// records what it wrote (refreshHeldClaims).
	claimed time.Time
	// unsure is that a refresh named claimed and got no answer, so the row may carry a claim this
	// sender wrote and never learned; the next refresh or try re-reads it (refreshHeldClaims,
	// claimKnown), and a claim recorded from an answer ends it.
	unsure bool
	// lost counts the refreshes whose answer was lost while they named this attempt. A re-read
	// applies what it read only while the count is the one it began under: a refresh that lost
	// its answer after the read took the row may have written a claim the read never saw.
	lost uint64
	// trying is that a try of the attempt is in flight (mentionSender.markTrying), from before the try
	// until its park records its claim or the send ends (next), which the re-read of the unsure
	// claims leaves alone (rereadUnsure).
	trying bool
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
	// mentionClaimRefresh, which only a test shortens.
	claimRefresh time.Duration
	// heldWarnAfter is how long a send waits behind an earlier event before park logs its one WARN:
	// model.MentionCopyHeldWarnAfter, which only a test shortens.
	heldWarnAfter time.Duration
	// after starts the timer that ends a park (unpark), once no goroutine serves the parked lane
	// (serve): time.AfterFunc, which only a test replaces.
	after func(time.Duration, func())

	mu sync.Mutex
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
// is the delivery sweep's (resumeOwedDeliveries).
func (s *server) startMentionSender() *mentionSender {
	return newMentionSender(s.lifetime, mentionClaimRefresh,
		s.sendCommentMention, s.refreshMentionClaims, s.rereadMentionClaims, s.releaseMentionClaims)
}

func newMentionSender(
	lifetime context.Context,
	claimRefresh time.Duration,
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
		after: func(delay time.Duration, f func()) { time.AfterFunc(delay, f) },
		lanes: map[laneKey][]mentionSend{}, waits: map[laneKey]heldWait{},
		held: map[mentionKey]mentionClaimState{}, stopped: make(chan struct{}),
	}
	m.done.Add(1)
	go m.keepClaims()
	return m
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
			// earlier event (park): this one goes behind them, so the owner's order holds, and
			// waits from now (heldSamples).
			lane = held
			if item.heldSince.IsZero() {
				item.heldSince = time.Now()
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
// read fails makes no try and parks as a send that waited does. A send that waited for an earlier
// event of its comment's owner parks the owner's sends to its recipient (park), and the timer that
// ends the park (unpark) starts only once no goroutine serves the parked lane: at once when the
// park made a held lane beside the one this goroutine serves, and, when the parked lane is the one
// this goroutine serves - a held lane whose head waited again - only once this goroutine has given
// it up (next) and taken a lane that waits, or ended. So no two goroutines ever serve one lane,
// and a parked lane's timer is pending exactly while no goroutine is on it. A send that leaves a
// held lane past heldWarnAfter with no WARN logged for it logs its WARN as it goes (reportLate).
func (m *mentionSender) serve(lane laneKey) {
	defer m.done.Done()
	var finished mentionKey
	// givenUp starts the timer of the lane this goroutine parked and gives up, once it has.
	var givenUp func()
	for {
		item, next, ok := m.next(lane, finished)
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
			// recorded, which finds the attempt settled if the try sent it.
			var moved bool
			if item.claimed, moved = m.claimedDuring(item); !moved {
				break
			}
		}
		if !waits {
			finished = item.key()
			m.reportLate(lane, item)
			continue
		}
		finished = mentionKey{}
		claimed := !again.claimed.Equal(item.claimed)
		held, delay := m.park(lane, again, claimed, known)
		wake := func() { m.after(delay, func() { m.unpark(held) }) }
		if held == lane {
			lane, givenUp = laneKey{}, wake
			continue
		}
		wake()
	}
}

// next releases finished, the attempt whose send the goroutine serving lane has just finished (the
// zero key when it has finished none), and takes that goroutine's next send and the lane it now
// serves. lane is the zero laneKey when the goroutine gave its lane up to a park (serve), and it
// then takes a lane that waits. The send it takes carries the claim this sender records for it,
// which stays among the claims the refresh writes again until the send ends: a try that reads
// the rule alone writes no claim, so the claim its row carries is still that one, and a refresh
// that writes the row while the try is in flight records the claim it wrote (held). ok is false
// when the goroutine is done: the sender stopped, or no lane has a send left for it.
func (m *mentionSender) next(lane laneKey, finished mentionKey) (mentionSend, laneKey, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, finished)
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
	return item, lane, true
}

// markTrying marks key's try in flight: the one writer of this sender's own its row can have
// beside the refresh, which rereadUnsure leaves alone while it runs. The mark stays until the try's
// park clears it, under the lock it records the try's claim under (park), or the send ends (next).
// A send that only reads its claim (claimKnown) writes nothing, and is not marked.
func (m *mentionSender) markTrying(key mentionKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, mine := m.held[key]; mine {
		state.trying = true
		m.held[key] = state
	}
}

// claimedDuring answers the claim this sender records for item's attempt, and whether it moved
// while item's try was in flight, which carried item.claimed: a hand-off took a newer claim for the
// send (queueLocked), or a refresh wrote the row before the try's take did. A try that ended not
// waiting under the older claim may have been refused for the newer one, and one under the newer
// claim finds the attempt settled if the first sent it; a re-read records nothing while a try is in
// flight.
func (m *mentionSender) claimedDuring(item mentionSend) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, mine := m.held[item.key()]
	if !mine || state.claimed.Equal(item.claimed) {
		return item.claimed, false
	}
	return state.claimed, true
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
// (heldSamples), each from when it started waiting there, and each send held heldWarnAfter since it
// first waited - the one that waited and every send of its owner queued behind it - is logged once
// at WARN (warnHeld), naming its owner, the earlier event it waits behind and that event's last
// publish error, beside the ERROR the outbox writes for that event's own failed publish, which is
// the cause: here when a park finds it past the threshold, else as it leaves the lane (reportLate).
func (m *mentionSender) park(lane laneKey, item mentionSend, claimed, waited bool) (held laneKey, delay time.Duration) {
	item.parks++
	now := time.Now()
	if item.heldSince.IsZero() {
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
			if queued.heldSince.IsZero() {
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

// claimKnown is item with the claim its row carries, read before its try acts on it when its
// recorded claim is unsure: a refresh named that claim and got no answer, so the row may carry one
// the refresh wrote (mentionSender's doc). The read answers a row's claim only while it is this
// sender's own - at or past the claim recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL) - and this goroutine's is the one try of the attempt, so no other
// try's claim can be among them. The claim read is recorded unless one was recorded from an
// answer meanwhile, which item then carries instead; no claim read leaves the record as it is,
// the row's claim being no longer this sender's to carry, and the try finds it so. ok is false
// when the read fails: the claim is still unknown, and the send makes no try.
func (m *mentionSender) claimKnown(item mentionSend) (_ mentionSend, ok bool) {
	key := item.key()
	m.mu.Lock()
	began := m.held[key]
	m.mu.Unlock()
	if !began.unsure {
		return item, true
	}
	read, err := m.reread(m.ctx, []mentionClaim{item.claim()})
	if err != nil {
		slog.Error("dispatch: read the claim of a comment mention a lost refresh may have written",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
		return item, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.held[key]
	switch {
	case !state.claimed.Equal(item.claimed):
		item.claimed = state.claimed
	case state.lost != began.lost:
		// Another refresh lost its answer after the read: the claim stays unknown, and the try
		// carries the one recorded, which the row's claim, if this sender's, is at or past.
	default:
		state.unsure = false
		if len(read) == 1 {
			item.claimed = read[0].claimedAt
			state.claimed = item.claimed
		}
		m.held[key] = state
	}
	return item, true
}

// heldWait is what the sends of a held lane wait behind: the session the lane's head send last
// waited to reach and the earlier event it found that session must take first.
type heldWait struct {
	session string
	by      model.MentionCopyHolder
}

// warnHeld logs the one WARN of send, held behind wait since send.heldSince, as of now: its owner,
// the earlier event it waits behind and that event's last publish error, beside the ERROR the
// outbox writes for that event's own failed publish, which is the cause.
func warnHeld(send mentionSend, wait heldWait, now time.Time) {
	slog.Warn("dispatch: a comment mention's send waits behind an earlier event of its owner",
		"owner", send.owner(), "comment", send.comment.ID, "target", send.mention.Target, "session", wait.session,
		"event_id", wait.by.EventID, "publish_error", deref(wait.by.PublishError),
		"held_for", now.Sub(send.heldSince).Round(time.Second))
}

// reportLate logs the WARN of item, a send that has just left lane, a held lane, with no wait
// left, when it had waited there heldWarnAfter and no WARN was logged for it: its wait passed the
// threshold while it was in flight, or while it waited behind a send whose try was in flight,
// which no park saw. Every send held past the threshold is so reported once, however its wait
// ends. It names what the lane last waited behind, or, when the lane has no wait recorded, what
// item itself last waited behind.
func (m *mentionSender) reportLate(lane laneKey, item mentionSend) {
	now := time.Now()
	if item.heldSince.IsZero() || item.warned || now.Sub(item.heldSince) < m.heldWarnAfter {
		return
	}
	m.mu.Lock()
	wait, ok := m.waits[lane]
	m.mu.Unlock()
	if !ok {
		wait = heldWait{session: item.heldFor, by: item.heldBy}
	}
	warnHeld(item, wait, now)
}

// heldSamples is, by owner, how many sends wait on a held lane behind an earlier event of their
// owner - the one that waited and every send of its owner and recipient queued behind it - and how
// long the oldest has, in whole seconds, as of now: what the held gauges read (registerHeldGauges).
// A held lane's head is out of the count for the moment a try of it is in flight.
func (m *mentionSender) heldSamples(now time.Time) (count, oldest []metrics.GaugeSample) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := map[string]int64{}
	ages := map[string]time.Duration{}
	for lane := range m.waits {
		for _, send := range m.lanes[lane] {
			owner := send.owner()
			counts[owner]++
			ages[owner] = max(ages[owner], now.Sub(send.heldSince))
		}
	}
	for owner, held := range counts {
		labels := [][2]string{{"owner", owner}}
		count = append(count, metrics.GaugeSample{Labels: labels, Value: held})
		oldest = append(oldest, metrics.GaugeSample{Labels: labels, Value: int64(ages[owner] / time.Second)})
	}
	return count, oldest
}

// registerHeldGauges adds to registry the two gauges of the sends this server's mention sender
// holds behind an earlier event, by owner: envoy_dispatch_mention_sends_held and
// envoy_dispatch_mention_send_held_oldest_seconds. They read the sender register built, so a test
// that replaces it registers none (withMentionClaimRefresh) - nor does a nil registry.
func (s *server) registerHeldGauges(registry *metrics.Registry) {
	if registry == nil {
		return
	}
	sender := s.mentions
	registry.NewGaugeSetFunc("envoy_dispatch_mention_sends_held",
		"Comment mention sends that wait behind an earlier event of their owner the outbox still owes a send their session may take, by owner",
		func() []metrics.GaugeSample { count, _ := sender.heldSamples(time.Now()); return count })
	registry.NewGaugeSetFunc("envoy_dispatch_mention_send_held_oldest_seconds",
		"How long the oldest comment mention send has waited behind an earlier event of its owner, in whole seconds, by owner",
		func() []metrics.GaugeSample { _, oldest := sender.heldSamples(time.Now()); return oldest })
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

// keepClaims writes the claims of the sends waiting on the lanes again every claimRefresh
// (mentionClaimRefresh), until the lifetime ends or wait runs. Nothing is written again after
// that, which is what hands a stopped process's queue to the delivery sweep a lease later.
func (m *mentionSender) keepClaims() {
	defer m.done.Done()
	refresh := time.NewTicker(m.claimRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-m.stopped:
			return
		case <-refresh.C:
			m.refreshHeldClaims(m.ctx)
		}
	}
}

// refreshHeldClaims writes the claim of every attempt this sender holds again, in one statement,
// and records what that statement wrote: the next refresh and the lane's own send both read the
// claim this sender last took. The statement moves only a row that still carries the claim this
// sender recorded, so it never moves the claim a try has taken for itself out from under that
// try's settle, and it goes on writing the row of a send in flight whose try reads the rule alone
// and writes none (held).
//
// The statement runs without the lock, so for its length a row carries a newer claim than the map
// records. That gap is safe: the row's claim only moves forward, so the delivery sweep, which
// reads committed rows past a lease, sees it live sooner, never later, and the lane's bound send
// accepts any claim at or past the one it carries within a lease (model.DeliveryClaimSinceSQL).
// Its result is recorded only for a key whose claim the map still holds as the snapshot took it,
// so a claim written meanwhile - a keyed repeat's (queueLocked) - is never set back.
//
// A statement that answers an error may have committed all the same, so it leaves every claim it
// named that the map still holds unknown (mentionClaimState.unsure), never assumed, and the
// refresh re-reads those rows at once (rereadUnsure), leaving a send whose try is in flight to
// its park, which records the claim that try took; the next refresh or try re-reads whatever is
// still unknown before it acts on it (claimKnown). A read proves a row's claim this sender's own
// while it is less than a lease after the claim recorded (model.DeliveryClaimSinceSQL), and a lost
// refresh wrote at most one claim past the one recorded, less than a lease after it, since it
// writes only while the claim it names is live and every refresh after it names the recorded one,
// which the row no longer carries.
func (m *mentionSender) refreshHeldClaims(ctx context.Context) {
	m.rereadUnsure(ctx)
	m.mu.Lock()
	queued := make([]mentionClaim, 0, len(m.held))
	snapshot := make(map[mentionKey]time.Time, len(m.held))
	for key, state := range m.held {
		queued = append(queued, mentionClaim{key: key, claimedAt: state.claimed})
		snapshot[key] = state.claimed
	}
	m.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	refreshed, err := m.refresh(ctx, queued)
	if err != nil {
		slog.Error("dispatch: write the claims of the queued comment mentions again; the claims it named are unknown until read",
			"attempts", len(queued), "error", err)
	}
	m.mu.Lock()
	if err != nil {
		for key, claimed := range snapshot {
			if state, mine := m.held[key]; mine && state.claimed.Equal(claimed) {
				state.unsure = true
				state.lost++
				m.held[key] = state
			}
		}
		m.mu.Unlock()
		m.rereadUnsure(ctx)
		return
	}
	defer m.mu.Unlock()
	for _, claim := range refreshed {
		if state, mine := m.held[claim.key]; mine && state.claimed.Equal(snapshot[claim.key]) {
			state.claimed, state.unsure = claim.claimedAt, false
			m.held[claim.key] = state
		}
	}
}

// rereadUnsure reads the claims the rows of the unsure attempts carry, for those no try is in
// flight for, and records each (mentionSender's doc). The read answers a row's claim only while
// it is this sender's own - at or past the claim recorded and less than a lease after it
// (model.DeliveryClaimSinceSQL) - and a claim is recorded only while its attempt is no try's and
// its record is the one the read named, so a try's own claim, which a try that started after the
// read began may have taken, is never recorded here: that try records its own (park), or ends
// (next). An attempt it answers no row for is no longer this sender's to write, and stays as it
// is. A read that fails leaves every one unsure for the next refresh.
func (m *mentionSender) rereadUnsure(ctx context.Context) {
	m.mu.Lock()
	var unsure []mentionClaim
	lost := map[mentionKey]uint64{}
	for key, state := range m.held {
		if state.unsure && !state.trying {
			unsure = append(unsure, mentionClaim{key: key, claimedAt: state.claimed})
			lost[key] = state.lost
		}
	}
	m.mu.Unlock()
	if len(unsure) == 0 {
		return
	}
	read, err := m.reread(ctx, unsure)
	if err != nil {
		slog.Error("dispatch: read the claims of the comment mentions a lost refresh may have written",
			"attempts", len(unsure), "error", err)
		return
	}
	rows := make(map[mentionKey]time.Time, len(read))
	for _, claim := range read {
		rows[claim.key] = claim.claimedAt
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, claim := range unsure {
		state, mine := m.held[claim.key]
		if !mine || !state.unsure || state.trying || !state.claimed.Equal(claim.claimedAt) || state.lost != lost[claim.key] {
			continue
		}
		state.unsure = false
		if row, ok := rows[claim.key]; ok {
			state.claimed = row
		}
		m.held[claim.key] = state
	}
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
// owner (mentionHeldForOrder) answers the send to take up again - the same attempt, under the
// claim that send renewed, with a take-over window of its own - which its lane parks (park).
//
// A send that waited before reads the rule alone first, for the session it waited to reach
// (mentionCopyHolderFor), and parks again while the earlier event still owes its send: a parked
// lane's try then costs the rule's indexed read - two when the first finds no event, the second
// reading the role routes the earlier events owe - and, for each such route, one listener lookup
// of the role's holder (roleHolderMayBe, within mentionLookupTimeout); no claim and no write,
// while its claim is the one the sender writes again for every queued send. Only a send the rule
// lets go makes the whole delivery, which claims, resolves and asks the rule once more.
func (s *server) sendCommentMention(ctx context.Context, item mentionSend) (mentionSend, bool) {
	ctx = store.WithTransactionTracking(ctx)
	if item.heldFor != "" {
		if by, err := s.mentionCopyHolderFor(ctx, item.created, item.heldFor); err == nil && by != nil {
			item.heldBy = *by
			return item, true
		}
	}
	_, err := s.deliverResolvedCommentMention(
		ctx, item.comment, item.created, item.mention, item.actor,
		attemptClaim{attempt: item.attempt, claimedAt: item.claimed}, item.takeover,
	)
	if held, ok := errors.AsType[*mentionHeldForOrder](err); ok {
		item.claimed, item.takeover = held.claim.claimedAt, s.newTakeoverRetry()
		item.held(held)
		return item, true
	}
	if err != nil {
		slog.Error("dispatch: comment mention delivery",
			"comment", item.comment.ID, "target", item.mention.Target, "error", err)
	}
	return mentionSend{}, false
}
