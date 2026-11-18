package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/metrics"
)

// mentionSendLanes is how many recipients the mention sender sends to at once.
const mentionSendLanes = 8

// maxQueuedMentionSends bounds the sends the mention sender holds: those that have not started and
// those in flight, each of which keeps its place so it can park if it must wait for an earlier
// event (mentionSender.inflight, park). A send past it is not queued: the claim its attempt
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

// lane is the recipient a send is ordered by: the session the mention pinned when its lookup
// found one, and the target as written otherwise. role:X shares session:<X's holder>'s lane only
// when its lookup found the holder; an unanswered role:X is keyed by its string, so two comments
// to one recipient, one naming it by role and one by session, can then arrive in either order.
func (m mentionSend) lane() string {
	if m.mention.SessionID != nil {
		return "session:" + *m.mention.SessionID
	}
	return m.mention.Target
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
// The statements the sender makes go through its three functions, which register binds to the
// server: send (sendCommentMention), refresh (refreshMentionClaims) and release
// (releaseMentionClaims).
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
	// refresh writes the claims of the sends that have not started again and answers what it
	// wrote; release gives claims back.
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error)
	release func(context.Context, []mentionClaim) error
	// capacity is maxQueuedMentionSends; only a test lowers it.
	capacity int
	// claimRefresh is how often keepClaims writes the claims of the queued sends again:
	// mentionClaimRefresh, which only a test shortens.
	claimRefresh time.Duration
	// heldWarnAfter is how long a send waits behind an earlier event before park logs its one WARN:
	// model.MentionCopyHeldWarnAfter, which only a test shortens.
	heldWarnAfter time.Duration

	mu sync.Mutex
	// lanes holds each lane's sends that have not started. A key is present while its lane is
	// served, waiting in ready or parked, even once its last send has started.
	lanes map[string][]mentionSend
	// parked is the lanes whose head send waits for an earlier event of its comment's owner: no
	// goroutine serves one, and none is in ready, until unpark.
	parked map[string]struct{}
	// waits is, for each held lane (heldLane), what its sends wait behind: the session and the
	// earlier event its head send last found the session must take first (park). It goes with the
	// lane (next), and each send's WARN names it (warnHeld).
	waits map[string]heldWait
	// ready is the lanes waiting for a goroutine, in the order they started waiting.
	ready []string
	// serving is how many goroutines serve lanes.
	serving int
	// inflight is how many sends goroutines have started and not finished: each keeps its place
	// in the queue's capacity, so a send that waits for an earlier event always finds room to park
	// (park) and the owner's later sends never pass it.
	inflight int
	// claims is the claim this sender holds on each send that has not started, as it last wrote
	// it, so one statement writes them all again and no statement of its own ever touches the
	// claim a send has taken over. Its size is how many sends are queued, which capacity bounds.
	claims map[mentionKey]time.Time
	// held is every attempt this sender queued whose send has not finished, so that one attempt is
	// never queued twice: a keyed repeat of a comment answers while its first send is still queued.
	held map[mentionKey]struct{}
	// closed is set by wait; nothing starts after it, and stopped is closed with it.
	closed  bool
	stopped chan struct{}
	done    sync.WaitGroup
}

// startMentionSender builds the server's mention sender (register), which sends on the server's
// lifetime and drains for at most mentionDrainTimeout once it ends. An attempt no sender reaches
// is the delivery sweep's (resumeOwedDeliveries).
func (s *server) startMentionSender() *mentionSender {
	return newMentionSender(
		s.lifetime, mentionClaimRefresh, s.sendCommentMention, s.refreshMentionClaims, s.releaseMentionClaims)
}

func newMentionSender(
	lifetime context.Context,
	claimRefresh time.Duration,
	send func(context.Context, mentionSend) (mentionSend, bool),
	refresh func(context.Context, []mentionClaim) ([]mentionClaim, error),
	release func(context.Context, []mentionClaim) error,
) *mentionSender {
	ctx, cancel := context.WithCancel(context.WithoutCancel(lifetime))
	context.AfterFunc(lifetime, func() { time.AfterFunc(mentionDrainTimeout, cancel) })
	m := &mentionSender{
		lifetime: lifetime, ctx: ctx, cancel: cancel, send: send, refresh: refresh, release: release,
		capacity: maxQueuedMentionSends, claimRefresh: claimRefresh, heldWarnAfter: model.MentionCopyHeldWarnAfter,
		lanes: map[string][]mentionSend{}, parked: map[string]struct{}{}, waits: map[string]heldWait{},
		claims: map[mentionKey]time.Time{}, held: map[mentionKey]struct{}{}, stopped: make(chan struct{}),
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
	if _, mine := m.held[key]; mine {
		// A keyed repeat claimed the attempt anew before it asked (claimQueuedMention), so the
		// claim its send waits under is the one that repeat just wrote, and the refreshes and the
		// drain's release move that one. A send a lane has already started is not among the claims
		// this sender writes: its claim is the send's own, and nothing here touches it.
		if _, waiting := m.claims[key]; waiting {
			m.claims[key] = item.claimed
		}
		return true, false
	}
	if len(m.claims)+m.inflight >= m.capacity {
		return false, true
	}
	m.held[key] = struct{}{}
	m.claims[key] = item.claimed
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

// serve sends lane's queue, then any lane waiting for a goroutine, until none waits. A send that
// waited for an earlier event of its comment's owner parks the owner's sends to its recipient
// (park), and the goroutine goes on with what is left of its lane, or a lane that waits. A send
// that leaves a held lane past heldWarnAfter with no WARN logged for it logs its WARN as it goes
// (reportLate).
func (m *mentionSender) serve(lane string) {
	defer m.done.Done()
	var finished mentionKey
	for {
		item, next, ok := m.next(lane, finished)
		if !ok {
			return
		}
		lane, finished = next, item.key()
		if again, waits := m.send(m.ctx, item); waits {
			m.park(lane, again)
			finished = mentionKey{}
		} else {
			m.reportLate(lane, item)
		}
	}
}

// next releases finished, the attempt whose send the goroutine serving lane has just finished (the
// zero key when it has finished none), and takes that goroutine's next send and the lane it now
// serves. The send it takes leaves the claims this sender writes again: from here its attempt's
// claim is the send's own. A lane parked since its last send is no longer this goroutine's, which
// takes a lane that waits instead. ok is false when the goroutine is done: the sender stopped, or
// no lane has a send left for it.
func (m *mentionSender) next(lane string, finished mentionKey) (mentionSend, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.held, finished)
	if finished != (mentionKey{}) {
		m.inflight--
	}
	if m.stoppedLocked() {
		m.serving--
		return mentionSend{}, "", false
	}
	_, parked := m.parked[lane]
	if !parked && len(m.lanes[lane]) > 0 && len(m.ready) > 0 {
		m.ready = append(m.ready, lane)
		lane, m.ready = m.ready[0], m.ready[1:]
	}
	for parked || len(m.lanes[lane]) == 0 {
		if !parked {
			delete(m.lanes, lane)
			delete(m.waits, lane)
		}
		if len(m.ready) == 0 {
			m.serving--
			return mentionSend{}, "", false
		}
		// A lane in ready is never parked: only unpark puts a parked lane there.
		lane, m.ready, parked = m.ready[0], m.ready[1:], false
	}
	item := m.lanes[lane][0]
	m.lanes[lane][0] = mentionSend{}
	m.lanes[lane] = m.lanes[lane][1:]
	item.claimed = m.claims[item.key()]
	delete(m.claims, item.key())
	m.inflight++
	return item, lane, true
}

// heldLane is the lane the sends of owner to the recipient lane names wait on while one of them
// waits for an earlier event of owner (park): a lane of its own beside lane, so a recipient's sends
// of other owners pass them. A lane that is one already is its own.
func heldLane(lane, owner string) string {
	if strings.Contains(lane, "\x00") {
		return lane
	}
	return lane + "\x00" + owner
}

// park puts item, the send lane just started that waited for an earlier event of its comment's
// owner, at the head of that owner's held lane for the recipient (heldLane) under the claim it
// holds now, which this sender writes again from here as it writes every queued send's
// (refreshHeldClaims), and leaves the held lane with no goroutine until mentionOrderRetry of
// item's parks has passed (unpark). Every send of the same owner still queued on lane moves with
// it, in order, and every later one queues behind them (queueLocked), so the owner's sends keep
// their order; the recipient's sends of other owners stay on lane and go on, as an earlier event
// of one owner holds no event of another (planChains keeps order per owner). The held lane is
// deleted once it is empty and served (next), after which the owner's sends join lane again. Its
// attempt stays held, so a keyed repeat of the comment queues it no second time. A park once the
// sender stopped still records the claim, which the drain gives back with the rest (wait). Its
// send kept its place in the queue while it was in flight (inflight), so the park never passes the
// queue's capacity and never has to give its place to the owner's later sends.
//
// The wait has no bound, so it shows instead: the held gauges count every send on a held lane
// (heldSamples), each from when it started waiting there, and each send held heldWarnAfter since it
// first waited - the one that waited and every send of its owner queued behind it - is logged once
// at WARN (warnHeld), naming its owner, the earlier event it waits behind and that event's last
// publish error, beside the ERROR the outbox writes for that event's own failed publish, which is
// the cause: here when a park finds it past the threshold, else as it leaves the lane (reportLate).
func (m *mentionSender) park(lane string, item mentionSend) {
	item.parks++
	now := time.Now()
	if item.heldSince.IsZero() {
		item.heldSince = now
	}
	delay := mentionOrderRetry(item.parks)
	owner := item.owner()
	held := heldLane(lane, owner)
	m.mu.Lock()
	m.inflight--
	m.claims[item.key()] = item.claimed
	if held == lane {
		m.lanes[held] = append([]mentionSend{item}, m.lanes[held]...)
	} else {
		moved := []mentionSend{item}
		kept := make([]mentionSend, 0, len(m.lanes[lane]))
		for _, queued := range m.lanes[lane] {
			if queued.owner() == owner {
				if queued.heldSince.IsZero() {
					queued.heldSince = now
				}
				moved = append(moved, queued)
			} else {
				kept = append(kept, queued)
			}
		}
		m.lanes[lane] = kept
		m.lanes[held] = moved
	}
	wait := heldWait{session: item.heldFor, by: item.heldBy}
	m.waits[held] = wait
	var warned []mentionSend
	for index := range m.lanes[held] {
		send := &m.lanes[held][index]
		if !send.warned && now.Sub(send.heldSince) >= m.heldWarnAfter {
			send.warned = true
			warned = append(warned, *send)
		}
	}
	m.parked[held] = struct{}{}
	m.mu.Unlock()
	for _, send := range warned {
		warnHeld(send, wait, now)
	}
	time.AfterFunc(delay, func() { m.unpark(held) })
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
// ends.
func (m *mentionSender) reportLate(lane string, item mentionSend) {
	now := time.Now()
	if item.heldSince.IsZero() || item.warned || now.Sub(item.heldSince) < m.heldWarnAfter {
		return
	}
	m.mu.Lock()
	wait := m.waits[lane]
	m.mu.Unlock()
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
	for lane, sends := range m.lanes {
		if !strings.Contains(lane, "\x00") {
			continue
		}
		for _, send := range sends {
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
func (m *mentionSender) unpark(lane string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.parked, lane)
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

// refreshHeldClaims writes the claim of every send that has not started again, in one statement,
// and records what that statement wrote: the next refresh and the lane's own send both read the
// claim this sender last took. A send the lanes have handed out is not in the snapshot, and the
// statement moves only a row that still carries the claim this sender wrote, so a refresh can
// never move the claim a starting send has just taken out from under that send's settle.
//
// The statement runs without the lock, so for its length a row carries a newer claim than the map
// records. That gap is safe: the row's claim only moves forward, so the delivery sweep, which
// reads committed rows past a lease, sees it live sooner, never later, and the lane's bound send
// accepts any claim at or past the one it carries (model.DeliveryClaimMineSQL). Its result is
// recorded only for a key whose claim the map still holds as the snapshot took it, so a claim
// written meanwhile - a keyed repeat's (queueLocked) - is never set back.
func (m *mentionSender) refreshHeldClaims(ctx context.Context) {
	m.mu.Lock()
	queued := make([]mentionClaim, 0, len(m.claims))
	snapshot := make(map[mentionKey]time.Time, len(m.claims))
	for key, claimed := range m.claims {
		queued = append(queued, mentionClaim{key: key, claimedAt: claimed})
		snapshot[key] = claimed
	}
	m.mu.Unlock()
	if len(queued) == 0 {
		return
	}
	refreshed, err := m.refresh(ctx, queued)
	if err != nil {
		slog.Error("dispatch: write the claims of the queued comment mentions again",
			"attempts", len(queued), "error", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, claim := range refreshed {
		if held, waiting := m.claims[claim.key]; waiting && held.Equal(snapshot[claim.key]) {
			m.claims[claim.key] = claim.claimedAt
		}
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
	unstarted := make([]mentionClaim, 0, len(m.claims))
	for key, claimed := range m.claims {
		unstarted = append(unstarted, mentionClaim{key: key, claimedAt: claimed})
	}
	clear(m.claims)
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
// (mentionCopyHolderFor), and parks again on that one statement while the earlier event still
// owes its send: a parked lane's try then costs one indexed read, no claim, no listener lookup and
// no write, while its claim is the one the sender writes again for every queued send. Only a send
// the rule lets go makes the whole delivery, which claims, resolves and asks the rule once more.
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

// mentionClaimArrays is claims as the four arrays the sender's statements match a comment_deliveries
// row against: its comment, target and attempt, and the claim this sender wrote on it.
func mentionClaimArrays(claims []mentionClaim) (comments, targets []string, attempts []int32, claimed []time.Time) {
	comments = make([]string, len(claims))
	targets = make([]string, len(claims))
	attempts = make([]int32, len(claims))
	claimed = make([]time.Time, len(claims))
	for i, claim := range claims {
		comments[i], targets[i] = claim.key.comment, claim.key.target
		attempts[i], claimed[i] = int32(claim.key.attempt), claim.claimedAt
	}
	return comments, targets, attempts, claimed
}

// refreshMentionClaims is the mention sender's refresh: one statement writes the claim of every
// attempt whose send is still waiting on a lane, and answers the claims it wrote. A row that no
// longer carries the claim this sender wrote is left alone and not answered - its send has taken
// it over, or another sender has, or it has settled - so refreshHeldClaims leaves that send's
// recorded claim as it was, and every later statement of this sender's, the drain's release
// included, passes over the row too, since each is scoped to the claim it holds.
func (s *server) refreshMentionClaims(ctx context.Context, queued []mentionClaim) ([]mentionClaim, error) {
	comments, targets, attempts, claimed := mentionClaimArrays(queued)
	rows, err := s.deps.Store.Pool.Query(ctx, `
		update comment_deliveries d set claimed_at = now()
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as queued(comment_id, target, attempt, claimed_at)
		where d.comment_id = queued.comment_id and d.target = queued.target and d.attempt = queued.attempt
		  and d.state = 'pending' and d.claimed_at = queued.claimed_at
		returning d.comment_id::text, d.target, d.attempt, d.claimed_at
	`, comments, targets, attempts, claimed)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (mentionClaim, error) {
		var claim mentionClaim
		err := row.Scan(&claim.key.comment, &claim.key.target, &claim.key.attempt, &claim.claimedAt)
		return claim, err
	})
}

// releaseMentionClaims is the mention sender's release: one statement gives back the claims the
// attempts were committed with, so an attempt no lane took, or none started, is pending and
// unclaimed. Each is scoped to the claim this sender wrote, so an attempt another sender has
// since taken keeps that sender's claim.
func (s *server) releaseMentionClaims(ctx context.Context, held []mentionClaim) error {
	comments, targets, attempts, claimed := mentionClaimArrays(held)
	_, err := s.deps.Store.Pool.Exec(ctx, `
		update comment_deliveries d set claimed_at = null
		from unnest($1::uuid[], $2::text[], $3::int[], $4::timestamptz[])
			as held(comment_id, target, attempt, claimed_at)
		where d.comment_id = held.comment_id and d.target = held.target and d.attempt = held.attempt
		  and d.state = 'pending' and d.claimed_at = held.claimed_at
	`, comments, targets, attempts, claimed)
	return err
}

// claimQueuedMention takes the claim on an owed attempt a keyed repeat is about to put back on a
// lane (replayComment), so the delivery sweep leaves it to this process exactly as it leaves the
// attempt a comment of this process's own queued. ok is false when another sender holds it: that
// sender owns the send, and nothing is queued for it here.
func (s *server) claimQueuedMention(ctx context.Context, key mentionKey) (claimed time.Time, ok bool, err error) {
	err = s.deps.Store.Pool.QueryRow(ctx, `
		update comment_deliveries set claimed_at = now()
		where comment_id = $1 and target = $2 and attempt = $3 and state = 'pending' and `+model.DeliveryClaimFreeSQL+`
		returning claimed_at
	`, key.comment, key.target, key.attempt).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return claimed, true, nil
}
