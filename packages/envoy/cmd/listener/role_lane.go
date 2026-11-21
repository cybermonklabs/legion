package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
)

// roleForwardWaitLimit bounds the role messages the role lane holds at once: queued behind their
// holder's earlier forwards, waiting for their outcome, or held for another delivery attempt. When
// it is reached the role lane's handler waits for one to end, which only holds role messages in
// the lane's subscription, whose pending limits are far above any backlog that wait can build
// (bus.ConnectRoleLane).
const roleForwardWaitLimit = 4096

// roleHoldFirst and roleHoldMax bound the wait before a held role message's next delivery attempt
// (roleForwarder.hold): it doubles from the first to the max. The first is short because the most
// common hold is a forward whose window a busy process or a reconnect spent, which the next attempt
// usually finishes; the max keeps a listener whose NATS stays down to one attempt per held message
// every 5 s.
const (
	roleHoldFirst = 100 * time.Millisecond
	roleHoldMax   = 5 * time.Second
)

// roleHeldLine is the line of a role message the lane holds for another delivery attempt, and
// roleHeldDeliveryStatus its delivery_status: a NATS step of its delivery failed and the listener
// does not know that the stream holds a copy (roleForwarder.hold). A message's first hold logs it
// at ERROR, once, since the lane could not store the message when it took it; every later attempt
// logs it at DEBUG, so a message held for hours is one ERROR line and not one line per attempt.
// The message's end is its own line: forwarded, kept, or a refusal with its exception.
const (
	roleHeldLine           = "listener role message held for another delivery attempt"
	roleHeldDeliveryStatus = "held"
)

// roleLaneFullLine and roleLaneRoomLine are the two ERROR lines of a role lane whose forward slots
// are all held (roleForwarder.acquire): the first when the handler first has to wait for a slot,
// the second when at most half the slots are held again (roleForwarder.release), with how long the
// lane was full. While the slots are full the handler takes nothing from the lane's subscription,
// and its role messages wait there. The gauge envoy_role_forwards_outstanding, on the listener's
// /metrics, reads the slots held, which are at the limit (roleForwardWaitLimit, 4,096) while the
// lane is full; no alert in this repository reads it, so these two lines are the lane's report.
const (
	roleLaneFullLine = "role lane forward slots are all held; the role lane takes no more role messages until one ends"
	roleLaneRoomLine = "role lane forward slots have room again"
)

// roleParkedLine is the line of a held message the listener's stop parked: it makes no further
// attempt, and is kept once the lane's own client has drained (roleForwarder.keepWhenTheLaneHasDrained).
const roleParkedLine = "listener role message parked until the role lane drains"

// rolePinDroppedLine is the INFO line of a held message whose pinned holder's session has no record
// any more (pinnedHolderStands): a copy an earlier forward stored on its subject reaches nobody, so
// the message's holder is resolved again (deliverRoleMessage).
const rolePinDroppedLine = "listener role message's pinned holder has no session record; its holder is resolved again"

// roleKeptAfterUnknownForwardLine is the WARN line of a message the lane keeps on its role's kept
// lane after a forward of it to its holder had an outcome the listener never learned
// (roleForwarder.keepHeld): if that forward was stored and the role then moves, the kept copy's
// handover gives the new holder a second copy.
const roleKeptAfterUnknownForwardLine = "listener kept a role message whose forward to its holder may already be stored"

// roleHoldBound is how long the lane holds one message for further attempts, counted from when the
// lane took it (roleHeld.since) by the listener's clock: the stream's duplicate window
// (contracts.DeliveryDuplicateWindow, 72 h) less the kept lane's hour (roleKeepWindow). Every
// forward of a message to its holder carries one Nats-Msg-Id, which the stream drops as a duplicate
// only inside that window, counted from when it stored the first, which is after the lane took the
// message. So a forward the stream stored while its answer was lost would be stored a second time
// by an attempt after the window. No attempt starts past this bound: each is judged by it where it
// starts - a held message's wait ending (hold), a forward its holder's worker takes from the
// queue (publish), the pinned forward a message makes once more before its keep (keepHeld) - and
// a message past it is kept on its role's kept lane instead (keepHeld, under
// roleKeptPastHoldBound), so the kept copy's hour, and the forward its handover makes, end inside
// the window.
const roleHoldBound = contracts.DeliveryDuplicateWindow - roleKeepWindow

// roleHeldUnkeptLine is the ERROR line of a role message the lane stopped holding - the listener
// stopped, or the message reached roleHoldBound - and could not keep on its role's kept lane either:
// the one end of a held message that leaves no copy anywhere (roleForwarder.keepHeld).
const roleHeldUnkeptLine = "listener role message the lane stopped holding was not kept"

// roleStopWait is how long the role lane's drain waits, once the lane's own client has drained,
// for every role message handed over to end (roleLane.drain): the forward window, since a forward
// made before the stop has its outcome within it. The held messages' ends at the stop run under
// one deadline inside it (roleForwarder.keepHeldNow), so none outlasts the wait. It is the second
// of the two waits stopLaneGrace adds (main.go), so a change here moves every pod's grace.
const roleStopWait = roleForwardTimeout

// roleStopLineReserve is the end of roleStopWait no held message's step at the stop runs into
// (roleForwarder.keepHeldNow): the time its last line and the release of its slot take once its
// last step has ended, so the drain's wait sees it end, and the main client's drain, after which
// the process exits, cuts no message short of its line.
const roleStopLineReserve = 250 * time.Millisecond

// roleStopForwardMin is the least time a pinned message's forward once more at the stop is made
// in (reforwardShare): a round trip of a connection that answers takes milliseconds, and a share
// shorter than this would mostly spend the keep's time on a forward that cannot finish, so the
// message is kept at once instead.
const roleStopForwardMin = 100 * time.Millisecond

// roleLaneSlowConsumerInterval is how often the role lane's slow-consumer count is read for a change
// to log (watchRoleLaneSlowConsumers).
const roleLaneSlowConsumerInterval = 5 * time.Second

// roleForward is one role message the role lane resolved to its holder, every guard passed: item is
// the message as it arrived, sessionID its holder and held the attempts it has been held through.
// resolvedAt is when the resolution that chose that holder started, on the listener's clock, which
// the forward is judged by when its publish starts (roleForwarder.publish, roles.ForwardFenceAge);
// the kept lane's walk, which reads the role's claim before each copy, leaves it zero, and so does
// a held message's forward pinned to its holder (roleHeld.pinned), which is the same forward again
// and makes no resolution.
type roleForward struct {
	item       contracts.Envelope
	sessionID  string
	resolvedAt time.Time
	held       roleHeld
}

// roleHeld is one role message's place among the messages the role lane holds. attempts counts the
// delivery attempts that ended on a transport failure, a NATS step after which the listener does
// not know that the stream holds a copy; since is when the lane took the message, by the
// listener's clock (cfg.now), which roleHoldBound counts from; and slot says the message holds one
// of the forwarder's slots, which is what envoy_role_forwards_outstanding counts. The zero value
// is a message the lane just took: it takes a slot when it is queued (roleForwarder.enqueue) or
// when a failed resolution holds it (roleForwarder.hold), and keeps that slot through every
// attempt until its delivery ends, when the slot is freed exactly once (releaseHeld).
//
// pinned is the holder a forward of the message was made to whose outcome the listener did not
// learn: the stream may hold that forward's copy. Every later attempt is that same forward again -
// the same holder, its agent subject and the same Nats-Msg-Id (roleForward.msgID) - with no new
// resolution of the role's holder, so the stream drops the repeat when the first was stored and
// the message is stored once, for that holder, as it would have been had the acknowledgement come.
// A resolution that picked another holder after a move of the role would forward under another
// subject and another id, which the stream stores beside the first.
type roleHeld struct {
	attempts int
	since    time.Time
	slot     bool
	pinned   string
}

// pastBound reports whether an attempt starting at now, by the listener's clock, starts at or past
// roleHoldBound from when the lane took the message (since): the check each attempt makes where it
// starts, so a wait that ended late, or a forward that queued behind its holder's earlier ones,
// makes no attempt past the bound. A message with no since has no bound.
func (h roleHeld) pastBound(now time.Time) bool {
	return !h.since.IsZero() && !now.Before(h.since.Add(roleHoldBound))
}

// roleForwarder runs the role lane's forwards off its NATS handler, so a forward the stream is slow
// to acknowledge holds up no other role message. A holder's forwards are published in the order
// they were queued, by a worker per holder with forwards queued; a message's first forward is
// queued as the handler hands it over, and a later attempt of a held message, or a forward whose
// resolution was too old to forward on (publish), is queued again behind whatever came meanwhile,
// so under a failure one holder's messages can reach it out of the order they arrived in. Each
// forward's wait for its outcome runs on a goroutine of its own, and the outcome becomes the
// forward's delivery status and exception (finishRoleForward). A forward, a read or a keep that
// fails on NATS leaves the message held for another delivery attempt (hold), so once the lane
// takes a role message the stream ends up holding a copy of it or the forwarder holds the message
// itself. Messages queued, waiting and held together are bounded by the forwarder's slots,
// roleForwardWaitLimit in the listener: at that bound the lane's handler waits for a slot, which
// leaves the role messages NATS delivers after it in the lane's own subscription
// (bus.ConnectRoleLane) and drops none.
type roleForwarder struct {
	// cfg is the delivery configuration the forwards are published (forwardRole) and reported
	// through, and a held message delivered again (deliverRoleMessage); its roleForwards is this
	// forwarder.
	cfg   listenerDeliveryHandlerConfig
	slots chan struct{}
	// pending counts the messages that hold a slot, for wait.
	pending sync.WaitGroup
	// stop is closed when the listener stops (stopHolding): no further slot is given out, a
	// message the lane takes from then on is kept at once, and a held message waiting for its
	// next attempt parks until keepHeldNow.
	stop     chan struct{}
	stopOnce sync.Once
	// keepNow is closed once the lane's own client has drained (keepHeldNow), and only then does
	// each parked held message make its keep: the keeps of up to roleForwardWaitLimit held
	// messages and the lane handler's own keeps of the messages its subscription still holds share
	// the main connection, and held messages that start first spend the window the handler's need
	// to be kept at all.
	keepNow     chan struct{}
	keepNowOnce sync.Once
	// keepBy is the deadline every parked message's end runs under once keepNow is closed
	// (keepHeldNow), set before the close, so a message that reads it after the close reads it set.
	keepBy time.Time
	// full is set while the handler has had to wait for a slot and the slots have not fallen to
	// half since, fullSince when it was set (acquire, release): the pair of ERROR lines that report
	// a full lane are written once each per spell.
	full      atomic.Bool
	fullSince atomic.Int64

	mu sync.Mutex
	// queues holds each holder's forwards not yet published, in arrival order. A holder has a key
	// exactly while its worker runs.
	queues map[string][]roleForward
}

func newRoleForwarder(cfg listenerDeliveryHandlerConfig) *roleForwarder {
	return newRoleForwarderLimited(cfg, roleForwardWaitLimit)
}

// newRoleForwarderLimited is newRoleForwarder with limit slots, the bound on the role messages it
// holds at once; a test that fills the bound passes a small one.
func newRoleForwarderLimited(cfg listenerDeliveryHandlerConfig, limit int) *roleForwarder {
	f := &roleForwarder{slots: make(chan struct{}, limit), stop: make(chan struct{}), keepNow: make(chan struct{}), queues: map[string][]roleForward{}}
	cfg.roleForwards = f
	f.cfg = cfg
	return f
}

// forwarded is the envelope published on the holder's agent subject: the message under the key
// contracts.RoleForwardDedupeKey names for this holder, which the durable path recognises and
// acknowledges. That is the role-forward dedupe key for every key but the holder's own agent
// send key, which the forward carries as it is: Dispatch's outbox keys the copy of a comment by
// the failed mention of the session that mention names, and the host of that session, which
// claimed the key when the mention's send landed, drops the copy rather than showing the comment
// twice. A copy kept for a role nobody held carries each mentioned session's own settled
// mention's key instead (contracts.Envelope.MentionDedupeKeys) - a mention delivered as much as
// one that failed, since a delivered mention's key is claimed at that session's host too - the
// holder it would reach being unknown when it was kept, and the forward to a holder that map
// names goes under that session's key alone, so whichever of the comment's mentioned sessions
// claims the role is the one whose host drops the copy. Dispatch's outbox keys its own forwards
// by the same rule
// (internal/dispatch/outbox/role.go, roleForward.forward), so the two writers of a role's kept
// lane forward one copy under one key, and so under one msgID.
func (f roleForward) forwarded() contracts.Envelope {
	forwarded := f.item
	forwarded.DedupeKey = contracts.RoleForwardDedupeKey(f.item, f.sessionID)
	return forwarded
}

// msgID is the forward's Nats-Msg-Id on subject: the name under which the stream keeps one copy
// of this message for this holder, however often it is forwarded - by a retry, or by each
// listener of the machine groups an older grant leaves the lane in, which all forward the same
// message. A forward whose envelope's own key names its event
// (contracts.DedupeKeyNamesTheUpstreamEvent, asked of the message as it arrived: the forward's
// prefix hides a webhook key's shape) is named by the forward's dedupe key and the subject, as
// the publish route's forwards are (bus.ForwardMsgID); a takeover's copy carries such a key
// (takenFrameDedupeKey). Any other key is its sender's choice and two distinct envelopes can share
// one, so such a forward is named by its event id and the subject instead: a repeat of one message
// is still stored once, and two messages under one key are two messages, which the stream stores
// both of.
func (f roleForward) msgID(subject string) string {
	if contracts.DedupeKeyNamesTheUpstreamEvent(f.item) {
		return bus.ForwardMsgID(f.forwarded().DedupeKey, subject)
	}
	return bus.ForwardMsgID(f.item.EventID, subject)
}

// enqueue queues forward behind its holder's earlier forwards, starting the holder's worker when it
// has none. A message that holds no slot yet waits for one and records it (roleHeld.slot); a held
// one carries its own. A listener that stops while the message waits for a slot keeps it on its
// role's kept lane at once instead (keepHeld), and clears the attempt its delivery recorded, since
// no forward is made.
func (f *roleForwarder) enqueue(forward roleForward) {
	if !forward.held.slot {
		if !f.acquire() {
			f.cfg.attemptCache.Clear(forward.item.DedupeKey, forward.sessionID)
			f.keepHeld(forward.item, forward.held, roleKeptAtShutdown, nil, time.Time{})
			return
		}
		forward.held.slot = true
	}
	f.mu.Lock()
	queue, running := f.queues[forward.sessionID]
	f.queues[forward.sessionID] = append(queue, forward)
	f.mu.Unlock()
	if !running {
		go f.publish(forward.sessionID)
	}
}

// acquire takes a slot for one role message, waiting while every slot is taken, and reports whether
// it took one; its caller records that on the message (roleHeld.slot). It takes none once the
// listener stops (stopHolding): the role lane's handler, blocked here with every slot held, must go
// on taking the messages its subscription holds, or the lane's drain runs out and closes the
// connection under them. The first wait of a spell in which every slot is held logs
// roleLaneFullLine at ERROR (laneFull).
func (f *roleForwarder) acquire() bool {
	if f.stopped() {
		return false
	}
	select {
	case f.slots <- struct{}{}:
		f.pending.Add(1)
		return true
	default:
	}
	f.laneFull()
	select {
	case f.slots <- struct{}{}:
		f.pending.Add(1)
		return true
	case <-f.stop:
		return false
	}
}

// laneFull logs roleLaneFullLine at ERROR when a spell of full slots begins: once, until release
// logs the lane has room again.
func (f *roleForwarder) laneFull() {
	if !f.full.CompareAndSwap(false, true) {
		return
	}
	f.fullSince.Store(f.cfg.now().UnixNano())
	f.cfg.logger.Error(roleLaneFullLine,
		slog.Int("slots", cap(f.slots)),
		slog.Int("outstanding", f.outstanding()))
}

// stopped reports whether the listener has stopped the forwarder's attempts (stopHolding).
func (f *roleForwarder) stopped() bool {
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

// release frees the slot of one role message that has ended. A lane whose slots were full logs
// roleLaneRoomLine at ERROR once at most half its slots are held again, with how long it was full:
// half, rather than the first free slot, so a lane that stays at its bound logs one pair of lines
// for the spell and not one for each slot that frees and is taken again at once.
func (f *roleForwarder) release() {
	<-f.slots
	f.pending.Done()
	if f.full.Load() && f.outstanding() <= cap(f.slots)/2 && f.full.CompareAndSwap(true, false) {
		f.cfg.logger.Error(roleLaneRoomLine,
			slog.Int("slots", cap(f.slots)),
			slog.Int("outstanding", f.outstanding()),
			slog.Duration("full_for", f.cfg.now().Sub(time.Unix(0, f.fullSince.Load()))))
	}
}

// releaseHeld frees the slot a message's delivery holds, when it holds one (roleHeld.slot).
func (f *roleForwarder) releaseHeld(held roleHeld) {
	if held.slot {
		f.release()
	}
}

// publish is sessionID's worker: it publishes the holder's queued forwards in order, hands each
// one's wait to a goroutine of its own, and ends once the queue is empty. A forward it takes from
// the queue once the listener has stopped (stopHolding) is not made: it parks with its slot, as a
// held message does, and is kept once the lane's own client has drained
// (keepWhenTheLaneHasDrained), its attempt record cleared since no forward goes out. So every
// message the lane took ends forwarded, kept, or refused under a rule, and none is left queued
// behind a worker when the forwarder's wait runs out. A forward it takes from the queue at or past
// the message's hold bound (roleHeld.pastBound) is not made either: the message is kept on its
// role's kept lane instead (keepHeld, under roleKeptPastHoldBound), since a forward that queued
// that long behind its holder's earlier ones would start past the stream's duplicate window.
//
// A forward whose resolution is roles.ForwardFenceAge old or older when its publish starts - one
// that waited that long behind its holder's earlier forwards - is not made either: its message is
// delivered again (deliverRoleMessage), with its slot and its place among the held messages,
// which resolves the role's holder again and forwards to the holder that resolution finds, or
// keeps, holds or refuses the message as the lane does on that resolution: a takeover's copy
// reads the previous holder's subject for its record's life, and a forward started on a fresher
// resolution is stored where that copy reads it. A pinned forward carries no resolution and is
// never too old: it is the same forward to the same holder again, and a copy of it stored late on
// a previous holder's subject is the one a takeover's copy reads.
func (f *roleForwarder) publish(sessionID string) {
	for {
		f.mu.Lock()
		queue := f.queues[sessionID]
		if len(queue) == 0 {
			delete(f.queues, sessionID)
			f.mu.Unlock()
			return
		}
		forward := queue[0]
		queue[0] = roleForward{}
		f.queues[sessionID] = queue[1:]
		f.mu.Unlock()

		if f.stopped() {
			f.cfg.attemptCache.Clear(forward.item.DedupeKey, forward.sessionID)
			f.keepWhenTheLaneHasDrained(forward.item, forward.sessionID, forward.held, nil)
			continue
		}
		if forward.held.pastBound(f.cfg.now()) {
			f.cfg.attemptCache.Clear(forward.item.DedupeKey, forward.sessionID)
			f.keepHeld(forward.item, forward.held, roleKeptPastHoldBound, nil, time.Time{})
			continue
		}
		if roles.ResolutionTooOld(forward.resolvedAt, f.cfg.now()) {
			f.cfg.attemptCache.Clear(forward.item.DedupeKey, forward.sessionID)
			f.cfg.logger.LogEvent(slog.LevelInfo, "listener role forward resolves its role's holder again: its resolution is too old to forward on",
				slog.String("event_id", forward.item.EventID),
				slog.String("session_id", forward.sessionID),
				slog.String("topic", forward.item.Topic),
				slog.Duration("age", f.cfg.now().Sub(forward.resolvedAt)))
			deliverRoleMessage(f.cfg, forward.item, forward.held)
			continue
		}
		subject := contracts.LegacyAgentSubject(forward.sessionID)
		wait, err := f.cfg.forwardRole(subject, forward.forwarded(), forward.msgID(subject), roleForwardTimeout)
		if err != nil {
			f.finish(forward, err)
			continue
		}
		go func() { f.finish(forward, wait()) }()
	}
}

// finish ends forward with its outcome: a forward the stream acknowledged, or one an error ends
// for good (roleForwardEnded), is reported and frees its slot (finishRoleForward). Any other error
// leaves the listener not knowing whether the stream holds a copy, so the message is held for
// another delivery attempt with its slot, and its attempt record is cleared so that attempt is not
// taken for a repeat of it. The message is pinned to this holder (roleHeld.pinned) only when this
// forward may have been stored (forwardMayBeStored): a forward that left nothing on the wire, or
// that the stream answered it did not store, leaves the holder to be resolved again.
func (f *roleForwarder) finish(forward roleForward, err error) {
	if err != nil && !roleForwardEnded(f.cfg.client, contracts.LegacyAgentSubject(forward.sessionID), err, roleForwardTimeout) {
		f.cfg.attemptCache.Clear(forward.item.DedupeKey, forward.sessionID)
		held := forward.held
		if forwardMayBeStored(err) {
			held.pinned = forward.sessionID
		}
		f.hold(forward.item, forward.sessionID, held, err)
		return
	}
	finishRoleForward(f.cfg, forward, err)
	f.release()
}

// forwardMayBeStored reports whether a forward that ended on err, which no rule ends
// (roleForwardEnded), may have been stored by the stream: whether a later attempt to another
// holder could leave a second copy of the message. A forward that left nothing on the wire
// (bus.ErrNotSent: the window spent before the publish, the connection not back, the reply inbox
// not subscribed, nats.go's refusal to write it) was not stored. Nor was one the stream answered
// with its error, or one the server answered no responders: nothing took it. The stream's answer
// that an earlier publish of the same Nats-Msg-Id is still in process (bus.ErrDuplicateInProcess)
// says that publish may be stored. Every other error - a flush that failed or ran out after the
// publish, a publish not known to have been accepted, an acknowledgement that did not come, a
// reply read that failed - leaves it unknown, so the forward may have been stored.
func forwardMayBeStored(err error) bool {
	if errors.Is(err, bus.ErrNotSent) {
		return false
	}
	if errors.Is(err, bus.ErrDuplicateInProcess) {
		return true
	}
	var answered *nats.APIError
	return !errors.As(err, &answered) && !errors.Is(err, nats.ErrNoResponders)
}

// pinnedHolderStands reports whether the session a held message is pinned to still has a session
// record, read within ctx and roleHolderReadTimeout from the leader of the session bucket's stream
// (session.SessionRegistry.RefreshFromLeader), never from this listener's cache of it, which
// trails a deletion, nor from whichever copy of the bucket a direct get reaches, which can answer
// a record it has not applied yet as missing. A session with a record has its own consumer, which
// delivers a copy the stream stored on its agent subject; a session without one has none, so a
// copy stored there reaches nobody, and the message must be resolved to the role's holder again.
// A miss here drops the pin, so a record read as missing that stands would give the role's new
// holder a second copy of a message the pinned holder already has. nats.ErrKeyNotFound is the
// registry's word that no record stands; any other error is a read that did not answer.
func pinnedHolderStands(ctx context.Context, cfg listenerDeliveryHandlerConfig, sessionID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, roleHolderReadTimeout)
	defer cancel()
	_, err := cfg.sessions.RefreshFromLeader(ctx, sessionID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, nats.ErrKeyNotFound):
		return false, nil
	default:
		return false, err
	}
}

// roleForwardEnded reports whether err ends a role forward to subject for good, under a rule no
// other attempt changes, rather than leaving unknown whether the stream will ever hold a copy. Its
// errors are every one bus.Client.StartRequestStoredTo and PendingForward.Wait return:
//   - Ended: NATS refuses the publish under a rule another attempt repeats (busRefusesForGood, the
//     one list a keep ends on too): it refuses the envelope or the subject however often it is
//     sent (bus.ErrRefused: no message id, a subject NATS does not accept, a message past the
//     server's max payload), the listener's grant denies the subject (bus.ErrPublishDenied), or the
//     stream refuses the message itself (bus.StreamRefusesMessage: a message or headers past the
//     stream's maxima, a store that refuses it as too large, a sealed stream) and no send of it
//     may have stored it (mayHaveStored).
//   - Ended: no acknowledgement came, or the server answered no responders, for a publish the
//     server took, and a read of the deployed stream made now, within readWindow, says it does not
//     capture subject (bus.Client.ReadDeployedStreamCaptures), so no acknowledgement will come. A
//     readWindow already spent makes no read, and settles nothing.
//
// Every other error ends nothing, and the message is held for another attempt:
//   - the window spent before the publish, a connection not back in time, a reply subscription or
//     a flush that failed or ran out, a publish not known to have been accepted
//     (bus.ErrPublishUnconfirmed), a reply read that failed: the forward may never have left;
//   - any other answer of the stream, which can change with the stream's state: a full stream
//     (bus.ErrStreamFull) or any other store failure, an id still in process under an earlier
//     publish (bus.ErrDuplicateInProcess), insufficient resources, a clustered stream's sequence
//     mismatch or offline replica;
//   - no responders, or no acknowledgement, for a subject the stream captures or whose capture no
//     read could settle: a clustered stream changing its leader answers no responders for a subject
//     it captures, and an acknowledgement may have been lost after the stream stored the forward,
//     which the next attempt, under the same Nats-Msg-Id, finds as the duplicate it then is.
func roleForwardEnded(client *bus.Client, subject string, err error, readWindow time.Duration) bool {
	var answered *nats.APIError
	switch {
	case busRefusesForGood(err):
		return true
	case !errors.Is(err, bus.ErrForwardNotStored), errors.As(err, &answered), errors.Is(err, bus.ErrPublishUnconfirmed), readWindow <= 0:
		return false
	}
	captures, read := client.ReadDeployedStreamCaptures(subject, readWindow)
	return read && !captures
}

// hold holds item for another delivery attempt after cause, a NATS step of its delivery that
// failed without the listener knowing that the stream holds a copy: a forward (finish), a read of
// the role's claim or of its holder's session, or a keep (deliverRoleMessage). The message takes a
// slot if it holds none (roleHeld.slot) and keeps it until its delivery ends, so
// envoy_role_forwards_outstanding counts it while it waits. It publishes no delivery exception,
// since the message is not given up, and logs roleHeldLine, naming holder when one was resolved:
// at ERROR on the message's first hold and at DEBUG on every later one. After a wait that starts
// at roleHoldFirst and doubles to roleHoldMax, the delivery runs again (deliverRoleMessage). A
// message no forward of which has been made resolves its holder again, so a holder that changed
// gets it and a role that lost its holder keeps it on its kept lane. A message a forward of which
// has been made (roleHeld.pinned) is forwarded again to that holder under the same Nats-Msg-Id, so
// the stream stores it once. A message whose next attempt would start past roleHoldBound, counted
// from when the lane took it (roleHeld.since), is kept now instead (keepHeld), and so is one whose
// wait ends past it: a wait can end later than it was set for - a process paused, a clock that
// moved - so the bound is judged again where the attempt starts.
//
// A listener that stops makes no further attempt: the message parks, and keeps once the lane's own
// client has drained (keepHeldNow), so the handler's keeps of the messages the subscription still
// holds go first. The wait for that is unbounded by design, and the shutdown bounds it: the drain
// waits for every message handed over, within roleStopWait, and then goes on (roleLane.drain).
func (f *roleForwarder) hold(item contracts.Envelope, holder string, held roleHeld, cause error) {
	if !held.slot {
		if !f.acquire() {
			f.keepHeld(item, held, roleKeptAtShutdown, cause, time.Time{})
			return
		}
		held.slot = true
	}
	if held.since.IsZero() {
		held.since = f.cfg.now()
	}
	held.attempts++
	if f.stopped() {
		f.keepWhenTheLaneHasDrained(item, holder, held, cause)
		return
	}
	delay := roleHoldDelay(held.attempts)
	if held.pastBound(f.cfg.now().Add(delay)) {
		f.keepHeld(item, held, roleKeptPastHoldBound, cause, time.Time{})
		return
	}
	level := slog.LevelDebug
	if held.attempts == 1 {
		level = slog.LevelError
	}
	f.cfg.logger.DeliveryLog(level, roleHeldLine, holder, item.Topic, item.EventID, roleHeldDeliveryStatus,
		slog.Int("attempt", held.attempts),
		slog.Duration("next_attempt_in", delay),
		slog.String("error", cause.Error()))
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-f.stop:
		}
		// A stop that came with the timer still wins: the listener is going, so the message is
		// kept rather than tried once more.
		select {
		case <-f.stop:
			f.keepWhenTheLaneHasDrained(item, holder, held, cause)
		default:
			if held.pastBound(f.cfg.now()) {
				f.keepHeld(item, held, roleKeptPastHoldBound, cause, time.Time{})
				return
			}
			deliverRoleMessage(f.cfg, item, held)
		}
	}()
}

// keepWhenTheLaneHasDrained parks a held message until the lane's own client has drained
// (keepHeldNow) and then keeps it on its role's kept lane under roleKeptAtShutdown, within the
// stop's deadline (roleForwarder.keepBy), logging roleParkedLine, naming holder when one was
// resolved, when it parks. The handler keeps each message the subscription still holds while the
// drain runs, on the same main connection: a held message that kept at the stop instead would
// spend the window those need, and a listener holding its bound would lose them.
func (f *roleForwarder) keepWhenTheLaneHasDrained(item contracts.Envelope, holder string, held roleHeld, cause error) {
	f.cfg.logger.DeliveryLog(slog.LevelInfo, roleParkedLine, holder, item.Topic, item.EventID, roleHeldDeliveryStatus,
		slog.Int("attempts", held.attempts))
	go func() {
		<-f.keepNow
		f.keepHeld(item, held, roleKeptAtShutdown, cause, f.keepBy)
	}()
}

// roleHoldDelay is the wait before a held message's next delivery attempt, after attempts holds:
// roleHoldFirst, doubled for each hold after the first, at most roleHoldMax.
func roleHoldDelay(attempts int) time.Duration {
	delay := roleHoldFirst
	for range attempts - 1 {
		if delay >= roleHoldMax {
			return roleHoldMax
		}
		delay *= 2
	}
	return min(delay, roleHoldMax)
}

// roleKeepRefusedLine is the WARN line of a role message the lane stopped holding, or took after
// the listener stopped, whose keep a rule refused (keepFailureHolds): it is given up as
// delivery_failed, with the exception that tells its sender.
const roleKeepRefusedLine = "listener role message refused by its keep"

// keepHeld is a held message's end when the lane stops holding it - the listener stops
// (roleKeptAtShutdown), or it reached roleHoldBound (roleKeptPastHoldBound, with cause, the failure
// that would have held it once more) - and the end of a message the lane takes once the listener
// stopped. Its slot is freed when it holds one. by is the deadline the whole end runs under: the
// stop's (keepHeldNow) for a message parked at the stop, so its end, its own line included, comes
// before the drain's wait does; zero for every other end, each of whose steps takes its own window.
//
// A message a forward of which may already be stored (roleHeld.pinned) makes that forward once more
// first, while it can (forwardPinnedOnceMore), and is kept only when that forward did not end it.
//
// Every other message is kept once on its role's kept lane (keepRoleLaneMessage), which the role's
// next live holder is handed by a handover of any listener. A keep a rule refuses
// (keepFailureHolds: a role token no claim can name, a refused envelope, a denied subject, a stream
// that refuses the message itself) gives the message up as delivery_failed, with its exception, as
// the lane gives up a message a rule refuses anywhere. A keep that fails on NATS, or that by leaves
// no time for, leaves no copy anywhere and no rule to tell the sender by, which roleHeldUnkeptLine
// reports at ERROR with the event id and the reason.
func (f *roleForwarder) keepHeld(item contracts.Envelope, held roleHeld, reason roleKeepReason, cause error, by time.Time) {
	defer f.releaseHeld(held)
	ctx := context.Background()
	if !by.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, by)
		defer cancel()
	}
	if held.pinned != "" && f.forwardPinnedOnceMore(ctx, item, held, reason) {
		return
	}
	// The lane reads a message's role by the one rule the relay and a takeover pass read it by.
	role, _ := roleOfLaneTopic(item.Topic)
	err := keepRoleLaneMessage(ctx, f.cfg, role, item, reason, "")
	if err == nil {
		return
	}
	attrs := []slog.Attr{slog.String("reason", string(reason)), slog.Int("attempts", held.attempts), slog.String("error", err.Error())}
	if cause != nil {
		attrs = append(attrs, slog.String("last_attempt_error", cause.Error()))
	}
	if !keepFailureHolds(err) {
		applyDeliveryOutcome(f.cfg, item, deliveryOutcome{
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, roleKeepRefusedLine, "", item.Topic, item.EventID, "failed", attrs...)
			},
			exceptionReason: "delivery_failed",
		})
		return
	}
	f.cfg.logger.DeliveryLog(slog.LevelError, roleHeldUnkeptLine, "", item.Topic, item.EventID, "failed", attrs...)
}

// forwardPinnedOnceMore makes a pinned message's forward once more before its keep (keepHeld), and
// reports whether that forward ended the message. The forward goes to the pinned holder under the
// same Nats-Msg-Id while that holder's session still has a record (pinnedHolderStands) and the
// message is inside roleHoldBound where the forward starts (roleHeld.pastBound): an
// acknowledgement, a duplicate's included, ends the message forwarded, and a refusal under a rule
// ends it as a forward's refusal does. Under the stop's deadline the read and the forward share
// the part of ctx's time reforwardShare leaves before the keep's, and none is made when that part
// cannot fit a forward. Every other way leaves the message to the keep, with
// roleKeptAfterUnknownForwardLine at WARN, naming why no forward was made where none was: the kept
// copy's handover reaches whoever holds the role then, and a holder other than the pinned one gets
// a second copy if the first forward was stored. A pinned holder whose session has no record, or
// whose record could not be read, gets no forward: a copy stored on its subject reaches nobody, so
// the message goes to the keep, whose handover reaches the role's live holder.
func (f *roleForwarder) forwardPinnedOnceMore(ctx context.Context, item contracts.Envelope, held roleHeld, reason roleKeepReason) bool {
	attrs := []slog.Attr{slog.String("reason", string(reason)), slog.Int("attempts", held.attempts)}
	share, cancel, fits := reforwardShare(ctx)
	defer cancel()
	var stands bool
	var err error
	if fits {
		stands, err = pinnedHolderStands(share, f.cfg, held.pinned)
	} else {
		attrs = append(attrs, slog.String("no_forward", "the stop left too little time for a forward before the keep"))
	}
	switch {
	case stands && held.pastBound(f.cfg.now()):
		attrs = append(attrs, slog.String("no_forward", "the message reached its hold bound"))
	case stands:
		forward := roleForward{item: item, sessionID: held.pinned, held: held}
		subject := contracts.LegacyAgentSubject(held.pinned)
		err = f.cfg.forwardAndWait(subject, forward.forwarded(), forward.msgID(subject), forwardWindow(share))
		if err == nil || roleForwardEnded(f.cfg.client, subject, err, forwardWindow(share)) {
			finishRoleForward(f.cfg, forward, err)
			return true
		}
	}
	attrs = append(attrs, slog.Bool("pinned_holder_stands", stands))
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	f.cfg.logger.DeliveryLog(slog.LevelWarn, roleKeptAfterUnknownForwardLine, held.pinned, item.Topic, item.EventID, roleHeldDeliveryStatus, attrs...)
	return false
}

// reforwardShare is the part of ctx's time a pinned message's forward once more may take before
// its keep (forwardPinnedOnceMore): what is left before ctx's deadline less the keep's share, which
// is half of it and at most roleKeepPublishWindow, so a keep after a forward whose outcome is again
// unknown still has time to store the message. It reports false, and the message is kept at once,
// when that part is shorter than roleStopForwardMin. A ctx with no deadline - every end but the
// stop's - shares nothing: the read and the forward take their own windows.
func reforwardShare(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}, true
	}
	left := time.Until(deadline)
	share := left - min(roleKeepPublishWindow, left/2)
	if share < roleStopForwardMin {
		return ctx, func() {}, false
	}
	shared, cancel := context.WithTimeout(ctx, share)
	return shared, cancel, true
}

// stopHolding ends the forwarder's further delivery attempts: no slot is given out from now on, a
// message the lane's handler takes from now on, or is waiting for a slot for, is kept on its
// role's kept lane at once (keepHeld), and a message the lane holds parks until keepHeldNow. It
// runs once however often it is called.
func (f *roleForwarder) stopHolding() {
	f.stopOnce.Do(func() { close(f.stop) })
}

// keepHeldNow keeps every message parked by the stop, and every one parked after it
// (keepWhenTheLaneHasDrained), and returns when the drain's wait for them ends: roleStopWait from
// now. Each of those ends runs under one deadline, keepBy, roleStopLineReserve before that, so
// every step of it - a pinned message's read and forward once more, then its keep - ends in time
// for its own line before the wait does (keepHeld). The shutdown runs it once the lane's own
// client has drained, so the handler's keeps of the messages the subscription still held are
// already made: both sets of keeps go over the main connection, and a listener holding its bound
// has up to roleForwardWaitLimit held messages against a handful the handler still holds. It runs
// once however often it is called, each call returning the same end, and a forwarder no shutdown
// drains keeps nothing.
func (f *roleForwarder) keepHeldNow() time.Time {
	f.keepNowOnce.Do(func() {
		f.keepBy = time.Now().Add(roleStopWait - roleStopLineReserve)
		close(f.keepNow)
	})
	return f.keepBy.Add(roleStopLineReserve)
}

// outstanding is how many role messages are queued, waiting for their outcome, or held for another
// delivery attempt.
func (f *roleForwarder) outstanding() int {
	return len(f.slots)
}

// wait waits, within ctx, for every message handed over to end. The shutdown runs it once the
// forwarder stopped holding (stopHolding), the role lane drained, and the held messages were
// released to keep (keepHeldNow), so no message takes a slot meanwhile and every held one is being
// kept, and before the main client drains, since the forwards, the keeps and their exceptions are
// published through it. It reports whether all ended.
func (f *roleForwarder) wait(ctx context.Context) bool {
	return waitWithin(ctx, &f.pending)
}

// startRoleForward is forwardRole over client: every forward is stored-only
// (bus.Client.StartRequestStoredTo), so it counts as delivered once the stream acknowledges
// storing it. No session holds a core subscription any more, so no holder answers a receipt: the
// stream's copy on the holder's agent subject is how the forward reaches it, from its session
// consumer, with acknowledgement and redelivery (roleForwardCopyDelivery). msgID is the forward's
// Nats-Msg-Id when it is not empty, which is what keeps the stream to one copy per message and
// holder however many attempts a send makes (roleForward.msgID).
func startRoleForward(client *bus.Client) func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
	return func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (func() error, error) {
		forward, err := client.StartRequestStoredTo(subject, item, msgID, timeout)
		if err != nil {
			return nil, err
		}
		return forward.Wait, nil
	}
}

// roleLane is the listener's role lane at shutdown: its own client (bus.ConnectRoleLane), the
// forwards its handler hands over, and stopWatch, which ends watchRoleLaneSlowConsumers.
type roleLane struct {
	client    *bus.Client
	forwards  *roleForwarder
	stopWatch func()
}

// roleLaneUndrainedLine is the ERROR line of role messages the lane's subscription still held when
// its drain ran out (bus.UndrainedError.Pending, the one its handler was on included): the
// connection's close drops every one the handler had not taken, and the lane held the only copy.
const roleLaneUndrainedLine = "role lane drain ended before its handler finished the role messages it holds; those not taken are lost"

// drain ends the role lane at shutdown, in the one order that keeps every message the lane holds.
// It stops the forwarder's further delivery attempts first (stopHolding), so each message the
// handler takes from then on is kept on its role's kept lane at once and with no slot to wait for:
// a handler blocked on a slot while every slot is held would otherwise take nothing more, and the
// drain would run out and close the connection under the messages its subscription holds. Then it
// drains the lane's client within timeout, so the handler takes every role message the server
// already delivered and no more, and logs at ERROR how many the drain left untaken
// (bus.UndrainedError). Only then does it release the messages the lane holds to keep
// (keepHeldNow): up to roleForwardWaitLimit keeps on the main connection, which the handler's own
// keeps would otherwise queue behind. Last it waits for the messages handed over, until
// roleStopWait from that release: a forward published by then has its outcome within it, and each
// held message's end - a pinned message's forward once more and its keep among them - runs under
// one deadline inside it (keepHeldNow), so it ends with its own line before the wait does. A
// message still waiting then is logged and left to the main client's drain, which a forward's
// outcome and a held message's keep run through.
func (l roleLane) drain(logger *logging.Logger, timeout time.Duration) {
	l.stopWatch()
	l.forwards.stopHolding()
	if err := l.client.Drain(timeout); err != nil {
		var undrained *bus.UndrainedError
		if errors.As(err, &undrained) && undrained.Pending > 0 {
			logger.Error(roleLaneUndrainedLine, slog.Int("pending", undrained.Pending), slog.String("error", err.Error()))
		} else {
			logger.Warn("role lane drain error", slog.String("error", err.Error()))
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), l.forwards.keepHeldNow())
	defer cancel()
	if !l.forwards.wait(ctx) {
		logger.Warn("role forwards still waiting at shutdown", slog.Int("outstanding", l.forwards.outstanding()))
	}
}

// watchRoleLaneSlowConsumers logs each change of the role lane's slow-consumer count until ctx ends.
// The lane's pending limits are far above any backlog its handler can build, so the count stays 0
// unless nats.go reports a slow consumer anyway; the count is also the
// envoy_role_lane_slow_consumers metric.
func watchRoleLaneSlowConsumers(ctx context.Context, lane *bus.Client, logger *logging.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var seen uint64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if count := lane.SlowConsumers(); count != seen {
			logger.Warn("role lane slow consumers", slog.Uint64("total", count), slog.Uint64("new", count-seen))
			seen = count
		}
	}
}
