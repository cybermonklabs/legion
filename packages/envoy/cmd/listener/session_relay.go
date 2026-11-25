package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/natsgrant"
)

// The headers of a relay frame, the core message the listener publishes on a session's relay
// subject (bus.DeliverSubject) for each message of its consumer. The frame's data is the stored
// message's bytes, unchanged.
const (
	// relayHeaderSubject is the subject the stream stored the message on.
	relayHeaderSubject = "Envoy-Subject"
	// relayHeaderStreamSeq is the message's stream sequence, in decimal.
	relayHeaderStreamSeq = "Envoy-Stream-Seq"
	// relayHeaderStreamID is the stream's identity (bus.StreamIDMetadataKey).
	relayHeaderStreamID = "Envoy-Stream-Id"
	// relayHeaderDelivered is how many times the consumer has delivered the message, 1 the first.
	relayHeaderDelivered = "Envoy-Delivered"
	// relayHeaderStoredAt is when the stream stored the message, RFC 3339 with nanoseconds, UTC.
	relayHeaderStoredAt = "Envoy-Stored-At"
	// relayHeaderGap is "true" on the first frame after a restart of the consumer that started below
	// the stream's first sequence: messages the session had not taken are gone. An edge listener
	// sets it too on the first frame after GitHub events it withheld while it could read no fresh
	// repository answer for the session's projects, and names that gap in relayHeaderGapReason.
	relayHeaderGap = "Envoy-Gap"
	// relayHeaderGapReason names a gap that is not the stream's own loss, beside relayHeaderGap:
	// relayGapRepositoryWithheld for the edge's withheld GitHub events (sessionRelays.edgeWithholds).
	// A gap mark with no reason is the stream's retention gap.
	relayHeaderGapReason = "Envoy-Gap-Reason"
	// relayGapRepositoryWithheld is relayHeaderGapReason's value for a gap of GitHub events the
	// edge withheld because no fresh repository answer could say they were the session's.
	relayGapRepositoryWithheld = "repository_withheld"
)

// The replies a session sends on a frame's reply subject, each exactly one ASCII word.
const (
	// relayReplyReceived: the frame decoded, before any host work. The receipt.
	relayReplyReceived = "received"
	// relayReplyProgress: the frame is held unwritten; its ack wait starts again.
	relayReplyProgress = "progress"
	// relayReplyDone: the frame is written (or skipped); the message is acked. Also a receipt.
	relayReplyDone = "done"
	// relayReplyNak: the session cannot take the frame; the server delivers it again after the
	// delay the message's own delivery count earns (relayNakDelayFor).
	relayReplyNak = "nak"
)

const (
	// relayReceiptWait is how long the oldest frame a session has not answered may wait before the
	// relay takes the session for unreachable and pauses: it sends nothing more until it probes the
	// session again after relayPauseFirst, or the session registers.
	relayReceiptWait = 3 * time.Second
	// relayPauseFirst and relayPauseMax bound the wait before a paused relay restarts at the ack
	// floor and sends the session its unanswered messages again; it doubles from the first to the
	// max while the session answers none of them, and a frame the session answers puts it back to
	// the first (sessionRelays.relay).
	//
	// The first wait is a little under the receipt wait, so a session that was merely slow -- on the
	// 2-CPU load-test box a receipt took 2.1 s at the 99th percentile, late but alive -- has its
	// messages again about 5 s after the frame it missed, and a session that is truly gone costs one
	// restart of its consumer every 5 s at first. The max is relayRetryMax, so a relay's longest
	// idle wait is the same whatever stopped it, and it is three times under the 90 s heartbeat
	// whose registration used to be the only resume: a session that comes back has its messages
	// within 33 s of the probe that found it silent, against 85 s measured before (the waits run
	// 2, 4, 8, 16, then 30 s, each after a 3 s receipt wait, so the fifth probe is 42 s in).
	relayPauseFirst = 2 * time.Second
	relayPauseMax   = 30 * time.Second
	// relayNakDelay is how long the server waits to deliver a message a session refused (nak)
	// again on that message's first delivery. The wait doubles with the message's delivery count
	// and stops at relayNakDelayMax, so one frame the session goes on refusing comes back 1, 2, 4,
	// 8, 16 s and then 30 s apart (relayNakDelayFor), while a session that is merely busy sees its
	// first retry a second after it refused, as before.
	//
	// The ladder is the whole bound on a frame nobody can take -- a host that is gone, a frame the
	// session refuses every time: the session consumer carries MaxDeliver -1
	// (internal/bus/session_consumers.go, applySettings), so the server delivers such a frame for
	// as long as the stream holds it, and over the stream's 72-hour retention the ladder leaves
	// room for at most 8,644 relays of it to one consumer, about two a minute, against the 259,201
	// a flat second costs (TestARefusedFrameIsRelayedAtMost8644TimesOverTheStreamsRetention). A
	// relay restart recreates a consumer whose frame still awaits an ack, so the frame comes back
	// as delivery 1 and the ladder climbs again: each restart adds the five relays of its first
	// rungs.
	relayNakDelay = time.Second
	// relayNakDelayMax is the ladder's ceiling, the delay of every nak from a message's sixth
	// delivery on. It is the ladder's own rather than the pause backoff's relayPauseMax, whose
	// value a relay carries as sessionRelays.pauseMax and a test shortens: 30 s each, two bounds.
	relayNakDelayMax = 30 * time.Second
	// relayNakShiftMax is the most relayNakDelay is doubled: relayNakDelay << relayNakShiftMax
	// (32 s) is the first rung at or above relayNakDelayMax, so a delivery count past the rung
	// after it takes the ceiling and the shift overflows no time.Duration (relayNakDelayFor).
	relayNakShiftMax = 5
	// relayRetryFirst and relayRetryMax bound the wait before a relay whose consumer call or pull
	// failed restarts at the ack floor; it doubles from the first to the max.
	relayRetryFirst = time.Second
	relayRetryMax   = 30 * time.Second
	// relayTermPoll is how often a relay whose pull is past its bound (pullBound) reads the bound
	// again while it waits for its term's end to cancel it (relayNATS). The bound comes back above
	// zero within the same term only when a renewal that started inside the lease's validity
	// lands after it, which the lease's renewal interval (leaseRenewEvery) paces.
	relayTermPoll = 100 * time.Millisecond
	// relayPruneInterval is how often the relay drops lapsed session entries from the cache
	// (session.SessionRegistry.PruneLapsed), so a session that stopped registering stops being
	// relayed: a lapse has no watch event.
	relayPruneInterval = 30 * time.Second
	// relayRestartLimit is how many relays of one listener may restart their consumer at once
	// (sessionRelays.restart); the others wait their turn. A listener that starts relays every
	// session it owns at once, and a thousand restarts sent together made the server answer none
	// within the call timeout: on the 2-CPU load-test box most of the 1,000 timed out on their info
	// or delete. Restarting 1,000 consumers on the in-process server of that box, each by a delete
	// and a create (a message awaiting an ack; a restart with none sends neither), 1,000 at once took
	// 4.6 s with every call waiting 3.7 s at the median and 4.5 s at worst; 8 at a time took 4.6 s
	// too, at 33 ms a restart; 32 took 4.0 s, the most restarts a second (250), at 126 ms a restart
	// (190 ms p99); 64 took 4.2 s at 257 ms. So the server's throughput is reached from about 8 on,
	// and more only lengthens each call's wait at the server, which a registration's consumer calls
	// wait behind too. 32 keeps that wait near 200 ms on a saturated server, 25 times under
	// sessionConsumerCallTimeout, and leaves room for a hub across the network, where a restart is
	// about ten round trips and the round trip, not the server, bounds a restart: 32 at once still
	// restart 1,000 sessions in about 10 s at a 30 ms round trip, where 8 would take 40 s.
	relayRestartLimit = 32
	// relayRefusedReplyLogEvery is how often the relay writes its line for a reply that names
	// another session than its frame's (sessionRelays.onReply): the first such reply, then at most
	// one a minute, each saying how many it held back since the line before. Any publisher on the
	// listener's reply inbox can send such a reply. A session on its own grant (natsgrant.Session)
	// publishes bus.SessionReplies alone, so its reply names that session, and is refused only for
	// a token of another session's frame, which the session cannot read. A holder of the shared
	// session user or of a host user can send any. The line names the frame's session and the
	// session the reply names, never the sender, which the relay cannot see. A sender can send such
	// replies as fast as it likes, so the line must not grow with them; one a minute still tells an
	// operator it is going on, as admissionWarnEvery does for the listener's shared-token admissions.
	relayRefusedReplyLogEvery = time.Minute
)

// relayEvent is what the relay loop is told about a session; the flags of the events it has not
// handled yet merge.
type relayEvent uint8

const (
	// relayCheck: the session's entry changed; start or stop its relay to match it.
	relayCheck relayEvent = 0
	// relayResume: the session registered, or at the edge claimed a role through this listener; a relay that
	// paused or ended starts again.
	relayResume relayEvent = 1 << iota
	// relayRestart: the session registered with a new epoch; its relay restarts at the ack floor.
	relayRestart
)

// relayNotice is what the relay loop has been told about a session since it last took its events:
// their flags, and heard, the highest mark (sessionDelivery.mark) of the registrations with a new
// epoch among them.
type relayNotice struct {
	event relayEvent
	heard uint64
}

// relayOutcome is how one pull of a session's consumer ended (sessionRelays.relayNATS).
type relayOutcome uint8

const (
	// relayEnded: the relay was stopped, or it may start again only at the session's next
	// registration, or, at the edge, a role claim.
	relayEnded relayOutcome = iota
	// relayRetry: a consumer call or the pull failed; the relay restarts at the ack floor after a
	// wait while it is still this listener's.
	relayRetry
	// relayReconnected: the pull's request was lost with its connection
	// (bus.ErrSessionPullReconnected), or a consumer call of the restart before it was
	// (bus.ErrRequestLostWithConnection), which is no failure of the session's or of its
	// consumer's. The relay restarts at the ack floor at once, with no wait, and its retry backoff
	// starts again from relayRetryFirst: the failures before this reconnect say nothing about the
	// calls and the pull the new connection makes (sessionRelays.relay).
	relayReconnected
	// relayPaused: the session answered no frame within relayReceiptWait; the relay restarts at the
	// ack floor after the pause backoff, at the session's next registration, or, at the edge, a role
	// claim, whichever is first.
	relayPaused
)

// sessionRelays relays the session consumers this listener owns (sessionRelays.wanted): one relay
// per session, which restarts the session's consumer at its ack floor whenever it starts, then pulls
// it and hands each message on in stream order, as a core frame on the session's relay subject,
// answered on a reply subject of its own under the listener's one reply inbox, which names the
// session the frame was sent to (bus.SessionReplySubject).
//
// Among the tasks of one machine id, only the one serving the machine's receive term relays
// (setOwned), so a rolling deploy's second task relays nothing until the first lets go. The relay
// set follows the watched session cache (session.SessionRegistry.OnChange), so a session that
// moves to another machine id stops being relayed here at once, and a listener that starts relays
// every session the cache holds for it with no registration. A relay cancelled that way returns
// every message it still holds to the server before its run ends (sessionRelays.release), so the
// listener the session moved to is delivered them at once rather than after the consumer's ack
// wait, and writes one line saying it has.
//
// The relay judges no frame by the listener's caches, with one exception: a frame that is a role
// message is read against the role's claim, and one whose role another session holds, or none
// does, is copied to the role's kept lane and acknowledged rather than delivered (takeOverFrame).
// Every other frame it pulls is published whatever the caches hold.
//
// One loop goroutine owns the set of relays (relays) and applies the events it is sent in order;
// each relay runs on a goroutine of its own.
type sessionRelays struct {
	delivery *sessionDelivery
	// cfg is the delivery configuration a role message whose role another session holds is
	// copied through (takeOverFrame): the role's claim read, the copy to its kept lane and the
	// handover after it.
	cfg      listenerDeliveryHandlerConfig
	registry *store.Registry
	// client is the listener's main connection, the one registry reads the bucket through: a
	// restart waits for it to be connected before it reads the session's stored topics
	// (storedFilter), since the session-consumer connection reconnects on its own.
	client *bus.Client
	// sessions is the session cache this relay reads its entries from; it is the same registry
	// the durable path's fan-out judges a followed topic against (judgeFollowedTopic).
	sessions  *session.SessionRegistry
	machineID string
	logger    *logging.Logger
	// inbox is the listener's reply inbox, or an edge listener's receipt subject under its
	// machine's family (natsgrant.EdgeRelayReplyPrefix; startSessionRelays), and replyPrefix it and
	// a dot: a frame's reply subject is replyPrefix<session id>.<token> (bus.SessionReplySubject),
	// and one subscription to the inbox's wildcard hears every reply.
	inbox       string
	replyPrefix string
	// pauseFirst and pauseMax are the bounds of a paused relay's backoff, relayPauseFirst and
	// relayPauseMax; a test shortens them. receiptWait is relayReceiptWait, how long the oldest
	// unanswered frame may wait before the relay pauses, and a test shortens it the same way, so
	// a row about what an unreachable session leaves behind spends its own short wait rather
	// than three seconds of the clock. Set before the first relay starts and read-only after.
	pauseFirst  time.Duration
	pauseMax    time.Duration
	receiptWait time.Duration
	// formerSettle is how long a former holder that released the lease may still be settling the
	// frames it had unanswered, counted from this pod's acquisition (lease.FormerSettleWait): a
	// relay waits it out before it restarts a session's consumer at its ack floor, so the acks
	// the stopping pod is still writing have moved that floor as far as it will go before the
	// restart reads it. A restart sends again every frame left above the floor (the README's
	// "When a session can see a message twice"). main reads it from the pod's lease, which answers
	// zero while the pod has none and follows a lease it joins later; nil in a rig that takes
	// nothing over, which waits for nothing.
	formerSettle func() time.Duration
	// repositories is the edge listener's repository reader (edge_repositories.go), which holds
	// each GitHub frame to its session's projects (relayNATS); nil on every listener but an edge
	// one, which relays every frame as it always has. Set before the first relay starts and
	// read-only after.
	repositories *edgeRepositories
	// repositoryGapsMu guards repositoryGaps: each session owed the gap mark of GitHub events
	// the edge withheld while no fresh repository answer stood (relayNATS), as this process noted
	// it, with the mark this process wrote on the session's entry (noteRepositoryGap). It is kept
	// across the session's relay runs, so a run that restarts before the session's next frame - a
	// new delivery epoch, a pull or a frame-publish error, a receipt pause - still marks that
	// frame, and the session's entry carries the same mark for the next listener to relay it
	// (session.SessionEntry.RepositoryGap). It is cleared once a frame carrying the mark is
	// published (repositoryGapSent), or once the session is no longer this listener's to relay
	// (forgetRepositoryGap, and stopAll for every session when the machine's receive term ends),
	// which leaves the entry's mark to the next owner.
	repositoryGapsMu sync.Mutex
	repositoryGaps   map[string]string
	// givenBack counts, for each session whose entry could not record the gap mark of a withheld
	// GitHub event, the events given back since (giveBackWithheld), until a mark write of the
	// session lands (noteRepositoryGap), a GitHub event of the session is decided by a fresh
	// repository answer (endGiveBacks), or the session is no longer this listener's to relay
	// (apply, and stopAll for every session when the machine's receive term ends): a run of
	// give-backs is logged when it starts and when it ends, however often the server delivers an
	// event again meanwhile. repositoryGapsMu guards it too.
	givenBack map[string]int
	// reMark re-marks a session whose nak'd frame carried a gap mark, off the receipt callback
	// (onReply, reMarkWithheldGap). startSessionRelays sets it; onReply runs it in a goroutine so
	// the KV read and write it makes never stalls the receipts the one callback carries. A test
	// sets it to hold the write, so it can show onReply does not wait on it.
	reMark func(sessionID string)
	// reMarkMu guards reMarking and reMarkAgain, which hold one re-mark in flight per session:
	// onReply runs each nak'd gap-marked frame's re-mark off its callback (scheduleReMark), and a
	// re-mark asked while one runs for the session is folded into a single further run after it, so
	// two naks cannot run noteRepositoryGap's read and write at once and leave the in-memory mark
	// and the entry's mark diverged, which would send the session one redundant gap notice.
	reMarkMu    sync.Mutex
	reMarking   map[string]bool
	reMarkAgain map[string]bool

	ctx    context.Context
	cancel context.CancelCauseFunc
	// owned is whether this task serves its machine's receive term (setOwned), for the readers
	// that take no lock; it is written under eventsMu, with term.
	owned atomic.Bool
	// stopChange unregisters the session cache's change callback.
	stopChange func()
	// runs counts the relay goroutines; loopDone is closed once the loop has returned.
	runs     inflight
	loopDone chan struct{}

	// eventsMu guards events and ownershipChanged, which the loop takes each time wake fires, and
	// term, the receive term this task serves, nil while it serves none: setOwned changes it under
	// eventsMu, and a restart reads it there just before it acts on a session's consumer
	// (termContext).
	eventsMu         sync.Mutex
	events           map[string]relayNotice
	ownershipChanged bool
	term             *relayTerm
	wake             chan struct{}

	// relays is each session's current relay; only the loop reads or writes it.
	relays map[string]*relayRun
	// listed holds, by session id, the consumers listed for the relays that the receive term's
	// start starts together (listConsumers), until each relay's start takes its own; nil
	// otherwise. listedAt is the mark taken before that list. listRefusedLogged is set once a list
	// refused for want of the grant is logged. Only the loop reads or writes them.
	listed            map[string]*jetstream.ConsumerInfo
	listedAt          uint64
	listRefusedLogged bool

	// restarts holds a token for each relay restarting its consumer, relayRestartLimit at most.
	restarts chan struct{}

	// framesMu guards frames, each relay's outstanding and unanswered frames, and refusedLoggedAt
	// and refusedHeld: when onReply last wrote its line for a reply naming another session than
	// its frame's, and how many such replies it has held back since (relayRefusedReplyLogEvery).
	framesMu sync.Mutex
	// frames maps the token of every frame awaiting done or nak to it: the random last token of
	// its reply subject. Among sessions on per-session credentials (the NATS auth service's grant,
	// which subscribes a session's own relay subject alone) only the session the frame was sent to
	// can read that subject. A holder of the shared session user or of a host user reads every
	// session's relay subject, frames' reply subjects with them, and may publish to each, so neither
	// the token nor onReply's check of the session the subject names stops it.
	frames          map[string]*relayFrame
	refusedLoggedAt time.Time
	refusedHeld     int
}

// relayTerm is the receive term a relay set serves (sessionRelays.setOwned), as the receive loop
// hands it over when the term begins (durableReceiver.begin). ctx ends when the term does, which
// the receive loop hears from the lease a moment after the lease itself ends the term. untilLoss
// is how long is left before another pod may hold the machine's lease (lease.UntilLoss), and nil
// for a listener without a lease, whose term only ctx ends.
type relayTerm struct {
	ctx       context.Context
	untilLoss func() time.Duration
}

// relayCancelReason is why the loop cancelled a run. It is the cause the cancellation carries
// (context.WithCancelCause), which sets it atomically with the cancellation and propagates the
// set's own cause to every run under it, so the run's goroutine reads the reason from its context
// once it has returned, the line it writes then names it, and a person reading that line needs no
// source to hand. It is an unsigned integer over named constants rather than a distinct error per
// reason because this file already names its small sets that way (relayEvent, relayOutcome) and
// the zero value is a constant of the set.
type relayCancelReason uint32

const (
	// relayCancelUnknown: the context carries no cause of this set, which no cancellation of a
	// run or of the set leaves behind; the word keeps the log field readable rather than empty.
	relayCancelUnknown relayCancelReason = iota
	// relayCancelNotOurs: the session is no longer this listener's to relay, because it
	// registered again through another listener, its entry left consumer delivery, or this
	// machine's receive term ended. Its record stands and so does its consumer, so the messages
	// the run gives back go to that consumer's next pull at once rather than after its ack wait.
	relayCancelNotOurs
	// relayCancelSessionGone: the session has no record in the cache, so no listener relays it
	// (sessionRelays.wanted). A clean stop (sessionDelivery.deleteAfterAnswer,
	// session_consumer_delete.go) and the session reaper (session.SessionReaper.DeleteConsumer)
	// each delete the session's consumer with the record they delete, which is where most relays
	// end, a clean stop being where most sessions end; the messages the run gives back are naked
	// into a consumer the server no longer holds, which drops them, and nothing reports that, a
	// nak being a publish the server answers nothing to. A record that lapsed out of the bucket
	// instead leaves the consumer standing with those messages on it for as long as it stands:
	// the reaper judges the records the cache holds, and a record whose bucket age is under the
	// gone horizon - five minutes against an hour where the deployment sets no
	// ENVOY_SESSION_MAX_AGE (session.BucketMaxAge, session.GoneHorizon) - leaves that cache
	// before a pass of it judges the session gone. The sweep is what ends such a consumer, one
	// pass after the record left the bucket (sessionConsumerSweep), and the stream's inactive
	// threshold (bus.SessionConsumerInactiveThreshold) is the backstop behind the sweep; until
	// one of them takes it, the session is delivered those messages when it registers again,
	// which is what a laptop that slept through its heartbeats does. The cache reports a record
	// deleted and one lapsed the same way, so the hand-over line names both (relayHandOverLines).
	relayCancelSessionGone
	// relayCancelNewEpoch: the session registered with a new epoch, so a run starting at the ack
	// floor replaced this one.
	relayCancelNewEpoch
	// relayCancelListenerStopping: this listener is stopping, so every relay stops with it.
	relayCancelListenerStopping
	// relayCancelTermEnded: this machine's receive term ended, so this task relays nothing until
	// it serves a term again.
	relayCancelTermEnded
)

// String is the word the log line carries for the reason.
func (r relayCancelReason) String() string {
	switch r {
	case relayCancelNotOurs:
		return "session_not_ours"
	case relayCancelSessionGone:
		return "session_gone"
	case relayCancelNewEpoch:
		return "new_epoch"
	case relayCancelListenerStopping:
		return "listener_stopping"
	case relayCancelTermEnded:
		return "term_ended"
	default:
		return "unknown"
	}
}

// Error is the same word as String, so a reason is usable as the cause of a cancellation
// (context.WithCancelCause), which takes an error.
func (r relayCancelReason) Error() string { return r.String() }

// relayHandOverLines is what a cancelled run's one hand-over line says for the reason it was
// cancelled for: whole once it has given the server back every message it held, failed when a
// nak of one of them, or the drain of its pull, did not reach the server (sessionRelays.release,
// bus.SessionPull.Release).
//
// A session whose record is gone gets lines of its own because no listener relays it
// (sessionRelays.wanted), so the messages the run gives back wait on its consumer instead of
// going to a next owner. What becomes of them turns on how the record went: a stop or a reap
// took the consumer with the record and the server drops the naks, while a record that lapsed
// leaves the consumer standing and the session is delivered those messages when it registers
// again (relayCancelSessionGone). The line names both, since the run has nothing to tell the
// two apart by and a sleeping laptop's line reads as loss otherwise. The other reasons leave
// the session's record and its consumer where they are, so the naks have the server deliver
// those messages to that consumer's next pull at once.
func relayHandOverLines(reason relayCancelReason) (whole, failed string) {
	if reason == relayCancelSessionGone {
		return "session relay stopped; the session has no record, so nothing relays it; the messages it held stand on the session's consumer, which delivers them when the session registers again, and nobody is delivered them where a stop or a reap deleted that consumer with the record",
			"session relay stopped; it could not give the server back every message it held, and the session has no record, so nothing relays it; the messages it held stand on the session's consumer, those it could not give back only once their ack wait passes, and nobody is delivered them where a stop or a reap deleted that consumer with the record"
	}
	return "session relay stopped; it gave the server back the messages it held, so they are delivered again at once rather than after the consumer's ack wait",
		"session relay stopped; it could not give the server back every message it held, and each one it did not is delivered again only after the consumer's ack wait"
}

// relayCancelledFor is why the run on ctx was cancelled, as the line a stopped run writes names
// it. A stop cancels the whole set's context (sessionRelays.stop), whose cause every run's
// context carries, so a run that returned before the loop reached stopAll still says that the
// listener is stopping.
func relayCancelledFor(ctx context.Context) relayCancelReason {
	reason, ok := context.Cause(ctx).(relayCancelReason)
	if !ok {
		return relayCancelUnknown
	}
	return reason
}

// relayRun is one start of a session's relay: it runs until it is stopped or replaced. A run whose
// session stops answering pauses (relayPaused) and goes on probing it.
type relayRun struct {
	sessionID string
	cancel    context.CancelCauseFunc
	// done is closed once the run's goroutine has returned.
	done chan struct{}
	// restartedAt is the mark (sessionDelivery.mark) the run's last successful restart at the ack
	// floor took before it read the session's topics and consumer, 0 before the first one
	// (relayRun.restartedBefore).
	restartedAt atomic.Uint64
	// resume carries, at most one at a time, the registrations heard for the session: a paused run
	// takes one instead of waiting its backoff out (sessionRelays.apply, relayRun.waitPause).
	resume chan struct{}
	// answered is set whenever the session answers a frame of the run and cleared each time the run
	// pauses, so the pause reads whether the session answered anything since the last one: the
	// backoff starts again from the first wait and the pause is logged as a new episode.
	answered atomic.Bool
	// listed is the session's consumer as the burst that started the run listed it
	// (sessionRelays.listed), which its first restart keeps without a call when it can
	// (bus.SessionConsumers.KeepListed); nil for a run started alone, and after that first restart.
	// listedAt is the mark taken before that list.
	listed   *jetstream.ConsumerInfo
	listedAt uint64
	// outstanding holds the run's frames awaiting done or nak, by token; unanswered holds, in the
	// order they were sent, its frames the session may not have answered yet. Both under
	// sessionRelays.framesMu.
	outstanding map[string]*relayFrame
	unanswered  []*relayFrame
	// exitMu orders the run's return against the loop's wakes. exiting is set under it once the
	// run returns, and a wake the loop sends after that is refused (relayRun.wake), so the loop starts
	// a run in its place; a wake sent before it is taken by the exit (relayRun.exit), which asks the
	// loop for that run. A registration heard while the run was returning is never lost.
	exitMu  sync.Mutex
	exiting bool
}

// wake has a paused run, or one waiting to retry, restart at once: the session registered. A wake
// sent while the run relays is dropped by its next pause, which names a registration heard before
// it, and so is one sent before a restart that starts the run's pull (dropWake); one sent after
// that restart is kept, and ends the retry wait of a failure after it at once (waitRetry). It
// reports false, sending nothing, once the run is returning: the caller then starts a run in its
// place.
func (r *relayRun) wake() bool {
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	if r.exiting {
		return false
	}
	select {
	case r.resume <- struct{}{}:
	default:
	}
	return true
}

// exit marks the run as returning, and reports whether a wake was waiting for it: a registration,
// or at the edge a role claim, the loop heard since the run's last pause began, or since its last
// restart started its pull, for which no run would start otherwise.
func (r *relayRun) exit() bool {
	r.exitMu.Lock()
	defer r.exitMu.Unlock()
	r.exiting = true
	select {
	case <-r.resume:
		return true
	default:
		return false
	}
}

// restartedBefore reports whether the run's last restart at the ack floor took its mark before
// heard, a registration's (sessionDelivery.mark), so the registration's new epoch restarts it: that
// restart may have read the session's topics and consumer, and the run sent frames, before the
// plugin subscribed the connection the registration names. A run that has not restarted yet reads
// both after, and so does one whose restart took its mark after heard, which every run that
// registration's own entry write started does: the loop can hear the registration only after such
// a run has restarted and sent frames, and a second restart would send the session every unacked
// message again and forget the frames it is answering.
func (r *relayRun) restartedBefore(heard uint64) bool {
	at := r.restartedAt.Load()
	return at != 0 && at < heard
}

// dropWake drops a wake that is waiting for the run, if one is. Each caller states why the wake it
// drops is owed nothing: the two pull restarts (sessionRelays.restart) and the start of a pause
// (waitPause).
func (r *relayRun) dropWake() {
	select {
	case <-r.resume:
	default:
	}
}

// waitPause waits out a paused run's backoff, d, the session's next registration (or, at the edge, a
// role claim), or the end of the run, and reports whether the run goes on. A wake sent before the
// pause does not end it: the pause drops it as it begins (dropWake), so only a registration, or at
// the edge a role claim, heard once the pause began cuts the backoff short.
func (r *relayRun) waitPause(ctx context.Context, d time.Duration) bool {
	r.dropWake()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.resume:
	case <-timer.C:
	}
	return true
}

// waitRetry waits out a run's retry backoff after a failed call or pull, d, a wake, or the end of
// the run, and reports whether the run goes on. Unlike a pause it takes a wake sent before it began:
// a registration, or at the edge a role claim, heard since the run's last restart started its pull,
// while the call or pull that failed ran, restarts the run at once. A wake from before that restart
// is gone (dropWake), so it costs the run no step of its backoff.
func (r *relayRun) waitRetry(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-r.resume:
	case <-timer.C:
	}
	return true
}

// ended reports whether the run's goroutine has returned: it was stopped, or it may start again
// only at the session's next registration, or at the edge a role claim it makes through this listener.
func (r *relayRun) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// relayFrame is a frame a NATS relay sent and the session has not answered with done or nak.
type relayFrame struct {
	run    *relayRun
	acker  bus.SessionAcker
	sentAt time.Time
	// subject, streamSeq and data are the stored message the frame carries, kept for the one
	// line a frame the settle grace cut earns (settleStoppedFrames). data is the message's own
	// bytes, which the frame was published from; nothing on the relay's hot path reads it back.
	subject   string
	streamSeq uint64
	data      []byte
	// delivered is the message's delivery count, 1 the first, as the consumer reported it to the
	// pull that published the frame (bus.SessionMessage.Delivered, relayHeaderDelivered). The
	// count is the message's and not the reply's, so the frame carries it from publishFrame to
	// the nak onReply sends (relayNakDelayFor).
	delivered uint64
	// answered is set once the session sent any reply for the frame, under framesMu.
	answered bool
	// gapMark is the repository gap mark the frame carried (the owed mark publishFrame was given),
	// "" for a frame that carried none and for a stream retention gap. The relay clears the mark
	// when it publishes the frame; a nak means the session did not take it (a slow consumer, or the
	// socket withholding it on a replica with no fresh answer), so onReply re-marks the session by
	// this, and the mark is not lost to a frame that never reached the client.
	gapMark string
}

// envelope is the stored message a frame carries, decoded for the one line a frame in doubt
// earns; an envelope that will not decode is the zero one, which names no event.
func (f *relayFrame) envelope() contracts.Envelope {
	var item contracts.Envelope
	_ = decodeStoredEnvelope(f.data, &item)
	return item
}

// relayEdgeInFlight is how many of a session's frames an edge listener's relay holds in flight,
// awaiting done or nak: its session consumers' MaxAckPending (bus.SessionConsumers.SetMaxAckPending),
// in place of bus.SessionConsumerMaxAckPending. A relay socket buffers what a client has not read
// yet (relaySocketPendingMsgs, relaySocketPendingBytes), so a restart at the ack floor sends a
// prompt client no more than its socket holds, however large the session's backlog.
const relayEdgeInFlight = 16

// startSessionRelays starts the relay of delivery's session consumers and makes it delivery's
// relay (sessionDelivery.relay). It relays nothing until a receive term is set (setOwned). A reply
// subscription the server refuses, under the NATS grant or otherwise, is its error, and it starts
// nothing then: main closes delivery and tries both again (openStartConnection).
//
// An edge listener's relay (cfg.edge set) takes its receipts under its own machine's receipt
// family (natsgrant.EdgeRelayReplyPrefix) and a nuid of its own rather than under a reply inbox:
// the session of an edge listener answers its frames over its relay socket, whose replica may be
// another machine's, and the edge's grant publishes every machine's receipts and subscribes only
// its own machine's, and no reply inbox (natsgrant.Edge). So the replica serving the socket
// publishes the client's receipt on the owner's subject, which the owner alone reads. Its
// consumers hold at most relayEdgeInFlight frames in flight; it is started before /v1 opens, so
// before any consumer is created, and each start try sets that bound on its own connection's
// consumers.
func startSessionRelays(delivery *sessionDelivery, cfg listenerDeliveryHandlerConfig) (*sessionRelays, error) {
	inbox := delivery.consumers.NewInbox()
	if cfg.edge != nil {
		inbox = natsgrant.EdgeRelayReplyPrefix(cfg.machineID) + "." + nuid.Next()
		delivery.consumers.SetMaxAckPending(relayEdgeInFlight)
	}
	m := &sessionRelays{
		delivery:    delivery,
		cfg:         cfg,
		registry:    cfg.registry,
		client:      cfg.client,
		sessions:    cfg.sessions,
		machineID:   cfg.machineID,
		logger:      cfg.logger,
		inbox:       inbox,
		replyPrefix: inbox + ".",
		pauseFirst:  relayPauseFirst,
		pauseMax:    relayPauseMax,
		receiptWait: relayReceiptWait,
		loopDone:    make(chan struct{}),
		events:      map[string]relayNotice{},
		wake:        make(chan struct{}, 1),
		relays:      map[string]*relayRun{},
		restarts:    make(chan struct{}, relayRestartLimit),
		frames:      map[string]*relayFrame{},
	}
	m.reMark = m.reMarkWithheldGap
	if err := delivery.consumers.SubscribeReplies(inbox+".>", m.onReply); err != nil {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancelCause(context.Background())
	m.stopChange = cfg.sessions.OnChange(m.onChange)
	delivery.relay = m
	go m.loop()
	return m, nil
}

// setOwned records which receive term of its machine this task serves: term, or none when term is
// nil. Gaining one starts the relay of every session the cache holds for this machine, each
// restarted at its ack floor; losing it stops every relay.
//
// A term whose context has already ended is not taken and changes nothing: it ended after the
// receive loop prepared the durable and before its receiving began (durableReceiver.begin), so
// this task relays nothing for it and starts no relay. The check is made under eventsMu, where the
// term is set and where every restart reads it (termContext). A term that ends just after the
// check is taken all the same: each restart under it finds its context ended and acts on no
// consumer, and the term's end then stops every relay (durableReceiver.end).
func (m *sessionRelays) setOwned(term *relayTerm) {
	if m == nil {
		return
	}
	m.eventsMu.Lock()
	if term != nil && term.ctx.Err() != nil {
		m.eventsMu.Unlock()
		return
	}
	m.term = term
	owned := term != nil
	changed := m.owned.Swap(owned) != owned
	if changed {
		m.ownershipChanged = true
	}
	m.eventsMu.Unlock()
	if !changed {
		return
	}
	m.logger.Info("session relays follow the receive term", slog.Bool("relaying", owned))
	m.signal()
}

// registered implements sessionRelay: the session registered, or at the edge claimed a role through this
// listener, so a relay that paused or ended starts again and a new epoch restarts it at the ack
// floor, unless it restarted after heard (relayRun.restartedBefore).
func (m *sessionRelays) registered(sessionID string, epochChanged bool, heard uint64) {
	if epochChanged {
		m.enqueue(sessionID, relayResume|relayRestart, heard)
		return
	}
	m.enqueue(sessionID, relayResume, 0)
}

// onChange hears every change to a session's cached entry.
func (m *sessionRelays) onChange(sessionID string) {
	if m.owned.Load() {
		m.enqueue(sessionID, relayCheck, 0)
	}
}

// enqueue tells the loop event about sessionID, with heard, the mark of a registration with a new
// epoch, or 0. The flags merge with those the loop has not taken yet, and the highest mark stays.
func (m *sessionRelays) enqueue(sessionID string, event relayEvent, heard uint64) {
	m.eventsMu.Lock()
	if m.events == nil {
		m.eventsMu.Unlock()
		return
	}
	notice := m.events[sessionID]
	notice.event |= event
	notice.heard = max(notice.heard, heard)
	m.events[sessionID] = notice
	m.eventsMu.Unlock()
	m.signal()
}

func (m *sessionRelays) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// stop stops every relay and waits, within ctx, for the relay loop and every run to return; a run
// stopped while it pulls returns the messages it holds with a nak before it ends (release). When
// ctx ends first, stop warns and returns while that work still runs, so a run still going may
// then publish a frame, have one acked on the session's done reply (onReply) or nak what it
// holds. The session-consumer connection is drained after it (sessionDelivery.drain).
func (m *sessionRelays) stop(ctx context.Context) {
	if m == nil {
		return
	}
	m.stopChange()
	m.eventsMu.Lock()
	m.events = nil
	m.eventsMu.Unlock()
	m.cancel(relayCancelListenerStopping)
	if !closedWithin(ctx, m.loopDone) {
		m.logger.Warn("session relay loop still running at shutdown")
		return
	}
	if !m.runs.wait(ctx) {
		m.logger.Warn("session relays still running at shutdown")
	}
}

// loop applies the relay's events in order until the relay stops, and prunes the lapsed entries of
// the session cache every relayPruneInterval.
func (m *sessionRelays) loop() {
	defer close(m.loopDone)
	prune := time.NewTicker(relayPruneInterval)
	defer prune.Stop()
	for {
		select {
		case <-m.ctx.Done():
			m.stopAll(relayCancelListenerStopping)
			return
		case now := <-prune.C:
			// Each lapse it finds is a change, which onChange hears.
			m.sessions.PruneLapsed()
			m.pruneAdmissions(now)
			continue
		case <-m.wake:
		}
		m.eventsMu.Lock()
		events, ownershipChanged := m.events, m.ownershipChanged
		m.ownershipChanged = false
		if events == nil {
			// stop has begun; the cancellation it makes next ends the loop.
			m.eventsMu.Unlock()
			continue
		}
		m.events = map[string]relayNotice{}
		m.eventsMu.Unlock()
		if ownershipChanged {
			if !m.owned.Load() {
				m.stopAll(relayCancelTermEnded)
				continue
			}
			entries, err := m.sessions.List()
			if err != nil {
				m.logger.Warn("session relays could not list the session cache", slog.String("error", err.Error()))
			}
			// A session whose entry is not on consumer delivery and that has no relay to stop needs
			// nothing (wanted).
			for _, entry := range entries {
				if _, ok := events[entry.SessionID]; !ok && (entry.Delivery == session.DeliveryConsumer || m.relays[entry.SessionID] != nil) {
					events[entry.SessionID] = relayNotice{}
				}
			}
			m.listedAt = m.delivery.mark()
			m.listed = m.listConsumers()
		}
		for sessionID, notice := range events {
			m.apply(sessionID, notice)
		}
		m.listed = nil
	}
}

// apply makes sessionID's relay match its entry and notice: started when the session is this
// listener's to relay, stopped when it is not, started again when it registered or, at the edge, claimed a role
// here and the run ended or is returning on its own (relayRun.wake), restarted at the ack floor on
// a new epoch registered after the run's last restart (relayRun.restartedBefore), and, for a run
// waiting out a pause or a retry, woken by that resume so it restarts at once.
func (m *sessionRelays) apply(sessionID string, notice relayNotice) {
	event := notice.event
	run := m.relays[sessionID]
	if ours, why := m.wanted(sessionID); !ours {
		if run != nil {
			run.cancel(why)
			delete(m.relays, sessionID)
		}
		m.forgetRepositoryGap(sessionID)
		// A run of give-backs of a session that leaves ends unlogged: no mark write of it lands here.
		m.repositoryGapsMu.Lock()
		delete(m.givenBack, sessionID)
		m.repositoryGapsMu.Unlock()
		return
	}
	switch {
	case run == nil:
		m.start(sessionID, nil)
	case run.ended():
		if event&relayResume != 0 {
			m.start(sessionID, run.done)
		}
	case event&relayRestart != 0 && run.restartedBefore(notice.heard):
		run.cancel(relayCancelNewEpoch)
		m.start(sessionID, run.done)
	case event&relayResume != 0:
		if !run.wake() {
			// The run is returning on its own: a run in its place takes the registration.
			m.start(sessionID, run.done)
		}
	}
}

// pruneAdmissions forgets, as of now, the admission of each session the cache holds no entry of
// (session.SessionRegistry.Get) that this replica admitted more than edgeAdmissionGrace ago
// (edgeAdmissions.prune): a session that deregistered, or whose entry lapsed, keeps no list here.
// Every listener that is no edge records no admission, and this forgets nothing there.
func (m *sessionRelays) pruneAdmissions(now time.Time) {
	m.delivery.admissions.prune(now.Add(-edgeAdmissionGrace), func(sessionID string) bool {
		_, err := m.sessions.Get(sessionID)
		return err != nil
	})
}

// stopAll stops every relay, each cancelled for reason: the listener is stopping, or the
// machine's receive term ended. Each caller names its own, so the line a stopped run writes says
// which happened. No session is this listener's to relay after it, so every gap mark this process
// noted is forgotten (forgetRepositoryGap's rule): the entries keep theirs for the pod that relays
// the sessions next, and this one reads them again from the entries if its term comes back. Every
// run of give-backs ends with it too, unlogged, as apply ends the run of a session that leaves
// (givenBack): a give-back under a later term starts a run of its own and logs it. It cancels the
// runs and waits for none of them, so a run its term's end cancelled during a repository gap
// write (noteRepositoryGap) may still note that mark, or count the give-back that follows a failed
// write (giveBackWithheld), here after the clear. The session's entry carries the same mark,
// written before it, so nothing acts on the noted one that would not act on the entry's. A re-mark
// after a nak (reMarkWithheldGap) runs on no term and is not waited for either, and one whose
// write fails notes a fresh mark the entry does not carry, after the clear too: where this
// listener's term comes back and it relays the session again, the run sends that mark from this
// process (owedRepositoryGap), and no other listener learns of it.
func (m *sessionRelays) stopAll(reason relayCancelReason) {
	for sessionID, run := range m.relays {
		run.cancel(reason)
		delete(m.relays, sessionID)
	}
	m.repositoryGapsMu.Lock()
	defer m.repositoryGapsMu.Unlock()
	clear(m.repositoryGaps)
	// Without this clear a give-back under a later term would count on as a later give-back of a
	// run this term began: giveBackWithheld would log no line for it, and the next mark write or
	// fresh repository answer would log an end that counts this term's give-backs as well.
	clear(m.givenBack)
}

// wanted reports whether this listener relays sessionID: it serves its machine's receive term,
// and the session's entry records consumer delivery and names this machine. The reason it
// answers beside that is why the relay of a session it does not want is cancelled, taken from
// the very lookup that decided it rather than from a second one, which the session could have
// changed under; it is relayCancelUnknown for a session this listener does relay.
func (m *sessionRelays) wanted(sessionID string) (bool, relayCancelReason) {
	if !m.owned.Load() {
		return false, relayCancelTermEnded
	}
	// Get answers nats.ErrKeyNotFound and nothing else: the cache holds no record of the
	// session, or holds one the bucket's maximum age has passed (session.SessionRegistry.Get).
	entry, err := m.sessions.Get(sessionID)
	if err != nil {
		return false, relayCancelSessionGone
	}
	if entry.Delivery != session.DeliveryConsumer || entry.MachineID != m.machineID {
		return false, relayCancelNotOurs
	}
	return true, relayCancelUnknown
}

// listConsumers reads every session consumer at once (bus.SessionConsumers.ListSessionConsumers)
// for the relays that gaining the receive term starts together, so a relay whose consumer a
// restart would keep unchanged starts pulling with no call of its own (sessionRelays.restart). It
// returns nil when the list fails, and each relay then reads its own consumer; a list the NATS
// user may not make is logged once.
func (m *sessionRelays) listConsumers() map[string]*jetstream.ConsumerInfo {
	ctx, cancel := context.WithTimeout(m.ctx, sessionConsumerCallTimeout)
	defer cancel()
	listed, err := m.delivery.consumers.ListSessionConsumers(ctx)
	switch {
	case err == nil:
		return listed
	case consumerGrantRefused(err):
		if !m.listRefusedLogged {
			m.listRefusedLogged = true
			m.logger.Warn("the NATS user may not list session consumers; each relay reads its own consumer at the listener's start", slog.String("error", err.Error()))
		}
	default:
		m.logger.Warn("session relays could not list the session consumers; each relay reads its own", slog.String("error", err.Error()))
	}
	return nil
}

// start starts a run of sessionID's relay, once the run it replaces, previous, has returned. A run
// started by the burst of gaining the receive term takes the session's listed consumer.
func (m *sessionRelays) start(sessionID string, previous <-chan struct{}) {
	ctx, cancel := context.WithCancelCause(m.ctx)
	run := &relayRun{sessionID: sessionID, cancel: cancel, done: make(chan struct{}), resume: make(chan struct{}, 1), listed: m.listed[sessionID], listedAt: m.listedAt}
	delete(m.listed, sessionID)
	m.relays[sessionID] = run
	m.runs.Go(func() {
		defer close(run.done)
		// A run replaced is always stopped first, so it returns promptly; the wait keeps two pulls
		// of one consumer from overlapping.
		if previous != nil {
			<-previous
		}
		m.relay(ctx, run)
		if run.exit() && ctx.Err() == nil {
			// The loop heard a registration while the run returned on its own.
			m.enqueue(sessionID, relayResume, 0)
		}
	})
}

// relay runs one session's relay until it is stopped, restarting at the ack floor after a failed
// call or pull, with a wait that doubles from relayRetryFirst to relayRetryMax, and after a pause
// (relayPaused), with a wait that doubles from pauseFirst to pauseMax, both cut short by the
// session's next registration, or at the edge a role claim it makes through this listener. Both only while
// the session is still this listener's.
//
// A pull the connection's reconnect ended, or a restart's call lost with its connection
// (relayReconnected: pullEnded, restartFailed), waits for neither: the relay restarts at the
// session's ack floor at once, and the wait a run's earlier failures had raised starts again from
// relayRetryFirst, since a request lost with its connection says nothing about the session or its
// consumer. Without that, a run that had met a handful of failures would cost its session
// relayRetryMax, 30 s, at every later reconnect.
//
// The pause is logged once an episode: the line is written for the first pause since the session
// last answered a frame, not for each probe that found it silent again.
func (m *sessionRelays) relay(ctx context.Context, run *relayRun) {
	if wait := m.formerSettleWait(); wait > 0 {
		// A pod that released this machine's lease has its stopped relays settling the frames
		// they had unanswered (settleStoppedFrames); restarting the session's consumer at its
		// ack floor before that is over would publish a frame it is still acking a second time.
		m.logger.LogEvent(slog.LevelDebug, "session relay waits out the former holder's settle bound before it restarts the session's consumer",
			slog.String("session_id", run.sessionID), slog.String("wait", wait.String()))
		if waitUnlessDone(ctx, wait) != nil {
			return
		}
	}
	retry, pause, paused := relayRetryFirst, m.pauseFirst, false
	for {
		outcome := m.relayNATS(ctx, run)
		switch outcome {
		case relayEnded:
			return
		case relayReconnected:
			retry = relayRetryFirst
		case relayPaused:
			if run.answered.Swap(false) {
				// The session answered a frame since the last pause: this one is a new episode.
				pause, paused = m.pauseFirst, false
			}
			if !paused {
				paused = true
				m.logger.Info("session relay paused: the session answered no frame within the receipt wait; it sends again after the pause backoff, at the session's next registration, or at the edge a role claim",
					slog.String("session_id", run.sessionID),
					slog.String("receipt_wait", m.receiptWait.String()),
					slog.String("backoff", pause.String()))
			}
			if !run.waitPause(ctx, pause) {
				return
			}
			pause = min(pause*2, m.pauseMax)
		default:
			if !run.waitRetry(ctx, retry) {
				return
			}
			retry = min(retry*2, relayRetryMax)
		}
		if ours, _ := m.wanted(run.sessionID); !ours {
			return
		}
	}
}

// formerSettleWait is how long a former holder that released this machine's lease may still be
// settling the frames it had unanswered (lease.FormerSettleWait), zero for a listener with no
// lease and once the wait has passed.
func (m *sessionRelays) formerSettleWait() time.Duration {
	if m.formerSettle == nil {
		return 0
	}
	return m.formerSettle()
}

// restart restarts the session's consumer at its ack floor with its current filter and starts
// pulling it. A pull of nil comes with the outcome to return: relayEnded when the relay was stopped
// or pauses (the session has no consumer, which its next registration creates, or the grant does
// not carry session consumers), relayRetry otherwise.
//
// A restart that read the floor and then failed, its delete or its create, leaves the consumer's
// start pending (bus.SessionConsumers.RestartAtFloor): the retry creates a consumer it finds
// missing at that start rather than pausing, and so does a registration's create meanwhile, so no
// restart loses the session's position. Only a consumer missing with no start pending (never
// created, or removed after its inactive threshold) pauses the relay until the next registration.
//
// The restart reads the session's stored topics from the bucket under the session's lock, as an
// unsubscribe reads them before it writes the filter, and a registration holds that lock from the
// filter it sets, on the plan it is about to store, through that plan's write (registerPass): so
// the restart never reads the row between a registration's filter and that registration's write,
// and the filter it writes is the row as the bucket held it when it read it under that lock. The
// lock orders only this listener's calls, so the write is fenced on the row it was built from
// against a registration made on another listener (storedFilter, settleFilter). The restart takes
// its mark (sessionDelivery.mark) first, under the lock, and records it once it succeeds
// (relayRun.restartedAt).
//
// Just before its RestartAtFloor the restart checks that this task's receive term is current, and
// sends that call and the settle of its filter on a context that ends with the term or at the
// lease's loss deadline, whichever is first (termContext). Its pull starts only while the term is
// still current, by the same bound (pullBound): a term that ended, or a lease that another pod may
// hold, leaves the consumer to the pod that takes the lease next. Whether the term ended before
// the RestartAtFloor, during it, or during the settle after it, the restart sends no further call,
// starts no pull, and answers relayRetry, so the relay tries again after its retry wait while the
// session is still this listener's, and the term's end stops it meanwhile. A call it had already
// sent can still land (termContext).
//
// The first restart of a run the burst of gaining the receive term started has the session's
// consumer as that burst listed it (relayRun.listed): when the restart would keep it unchanged
// (bus.SessionConsumers.KeepListed), the relay takes no turn and sends no call before it pulls,
// and its mark is the one taken before that list (relayRun.listedAt).
func (m *sessionRelays) restart(ctx context.Context, run *relayRun) (bus.SessionConsumerStart, *bus.SessionPull, relayOutcome) {
	sessionID := run.sessionID
	if listed := run.listed; listed != nil {
		run.listed = nil
		unlock := m.delivery.locks.lock(sessionID)
		callCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
		defer cancel()
		filter, _, ok := m.storedFilter(callCtx, sessionID)
		if !ok {
			unlock()
			return bus.SessionConsumerStart{}, nil, relayRetry
		}
		// A keep sends no call at all (bus.SessionConsumers.KeepListed): nothing of this filter
		// lands on the server, so there is nothing for a fence to hold. A restart the row moved
		// under is the one below.
		start, kept, err := m.delivery.consumers.KeepListed(callCtx, sessionID, filter, listed)
		unlock()
		if err == nil && kept {
			run.restartedAt.Store(run.listedAt)
			var pull *bus.SessionPull
			if pull, err = m.delivery.consumers.Pull(callCtx, sessionID, m.pullBound); err == nil {
				// Once the pull has started, an earlier registration is already reflected: it
				// wrote its row before this restart read the session's stored topics under the
				// session's lock, or made its own consumer calls under that lock after the
				// restart's. So its wake is dropped, and a retry after a later failure has nothing
				// to start at once for: kept, it ended the next failure's retry wait at once, and
				// the failure after that waited the doubled backoff in full.
				run.dropWake()
				return start, pull, relayRetry
			}
		}
		if err != nil {
			return m.restartFailed(ctx, sessionID, err)
		}
	}
	// The restart takes a turn (relayRestartLimit) first and its session's lock second, and only
	// then starts its calls' timeout, so the time it waited in line times nothing out. A
	// registration takes no turn, only the lock, so its consumer calls never wait behind the
	// restarts waiting for one.
	select {
	case m.restarts <- struct{}{}:
	case <-ctx.Done():
		return bus.SessionConsumerStart{}, nil, relayEnded
	}
	defer func() { <-m.restarts }()
	unlock := m.delivery.locks.lock(sessionID)
	at := m.delivery.mark()
	callCtx, cancel := context.WithTimeout(ctx, sessionConsumerCallTimeout)
	defer cancel()
	filter, revision, ok := m.storedFilter(callCtx, sessionID)
	if !ok {
		unlock()
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
	restartCtx, stopRestart, current := m.termContext(callCtx)
	if !current {
		unlock()
		m.logger.Info("session relay did not restart the session's consumer: this pod's receive term is over, or another pod may hold the machine's lease; it tries again after its retry wait while the session is still this listener's",
			slog.String("session_id", sessionID))
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
	defer stopRestart()
	start, err := m.delivery.consumers.RestartAtFloor(restartCtx, sessionID, filter)
	if err != nil && restartCtx.Err() != nil && callCtx.Err() == nil {
		unlock()
		m.logger.Info("session relay restart cut off: this pod's receive term ended, or another pod may hold the machine's lease, before the restart was over; a call it had already sent can still land, and it tries again after its retry wait while the session is still this listener's",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
	// A restart the server answered with a refusal left no filter of this row on the consumer; a
	// restart whose answer did not come back may have created it on that filter, so it is settled
	// against the bucket's newest row like one that answered (consumerCallAnswered).
	if err == nil || !consumerCallAnswered(err) {
		m.settleFilter(restartCtx, sessionID, revision)
	}
	unlock()
	if err == nil {
		run.restartedAt.Store(at)
		var pull *bus.SessionPull
		if pull, err = m.delivery.consumers.Pull(callCtx, sessionID, m.pullBound); err == nil {
			// Once the pull has started, an earlier registration is already reflected, as after
			// the keep above, so its wake is dropped.
			run.dropWake()
			return start, pull, relayRetry
		}
	}
	return m.restartFailed(ctx, sessionID, err)
}

// termContext is ctx bounded by the receive term this task serves (setOwned), for a restart of a
// session's consumer (restart), for the delete of one by the sweep or the session reaper
// (sessionDelivery.deleteConsumer), and for the relay's judgement of a role message it pulled,
// whose ack or give-back acts on the consumer (takeOverFrame): it ends when ctx does, when the
// term's context does, or at the lease's loss deadline (lease.UntilLoss), whichever comes first, so
// none acts once another pod may hold the lease, as each step of a dispatch intake's take-over ends
// at its hold's loss deadline (heldDurable.retryHeld). The deadline is the lease's at the check, 3
// to 4 s ahead on a holder whose renewals land, so it can also come before the restart's call
// timeout (sessionConsumerCallTimeout). It answers false, and no context, when no term is current:
// this task serves none, the term's context has ended, or the loss deadline has passed. The lease
// ends a term (stepDown, a release, a lapse, a give-up) a moment before the receive loop hears it
// and stops this task's relays (durableReceiver.end), and a session event in that moment starts a
// relay all the same (apply), which this check stops at its restart. The caller calls the
// returned stop once the restart, the delete or the judgement is over.
//
// What this leaves is a call the restart or the delete had already sent when its context ended:
// the server can still apply it, since the end of a context stops only what is not sent yet. The
// pod that takes the lease next is what bounds it, by when it may act on the same consumer. A pod
// that takes a record this pod released (a graceful stop, a give-up, a receive connection lost)
// finds this boot released in the record's former_boots, and waits relayFormerSettle (1 s) from
// its own term's start before any of its relays restarts a session's consumer
// (lease.FormerSettleWait, relay). A pod that takes the record over by force waits for nothing,
// but it takes the record no earlier than a full ttl after this pod's last landed renewal, which
// is leaseMargin (1 s) after the loss deadline that cut the call (lease.poll), while the two pods'
// clock rates differ by less than 20 %. So a call still in flight lands within about a second of
// the cut, or races the next holder's own calls for that consumer: no fence holds it, since a
// consumer call carries no term.
func (m *sessionRelays) termContext(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	term, left := m.currentTerm()
	if left <= 0 {
		return nil, nil, false
	}
	var bounded context.Context
	var cancel context.CancelFunc
	if term.untilLoss == nil {
		bounded, cancel = context.WithCancel(ctx)
	} else {
		bounded, cancel = context.WithTimeout(ctx, left)
	}
	unwatch := context.AfterFunc(term.ctx, cancel)
	return bounded, func() {
		unwatch()
		cancel()
	}, true
}

// currentTerm is the receive term this task serves (setOwned) and how long that term may still act
// on a session's consumer: what is left before the lease's loss deadline (lease.UntilLoss), the
// longest duration there is for a term no lease bounds, and zero, with no term, when none is
// current, because this task serves none or the term's context has ended.
func (m *sessionRelays) currentTerm() (*relayTerm, time.Duration) {
	m.eventsMu.Lock()
	term := m.term
	m.eventsMu.Unlock()
	if term == nil || term.ctx.Err() != nil {
		return nil, 0
	}
	if term.untilLoss == nil {
		return term, math.MaxInt64
	}
	return term, term.untilLoss()
}

// pullBound is how long a pull of this task's relays may still take messages (bus.PullBound): how
// long its term may still act on a session's consumer (currentTerm). So no pull request is sent
// once the term's context has ended or the lease's loss deadline has passed, and a message that
// arrives after that is neither forwarded nor answered (relayNATS).
func (m *sessionRelays) pullBound() time.Duration {
	_, left := m.currentTerm()
	return left
}

// storedFilter is the filter of sessionID's consumer for its stored interest topics, with the KV
// revision of the interest row it was built from, read within ctx under the session's lock, which
// the caller holds; false, logged, when they cannot be read.
//
// The row is read from the bucket and not from the watched cache (storedTopics): a registration
// on another listener may have added a topic this listener's watch has not delivered
// yet, and a restart filtering on the cached row would drop a topic that registration was
// answered 200 for while keeping the consumer's position, at the cost fenceFilterWrite's doc
// states. The revision is what the write of this filter is fenced on (settleFilter).
//
// The read waits, within ctx, for the connection it goes through to be connected
// (bus.Client.AwaitConnected): the session-consumer connection reconnects on its own, so the
// restart its reconnect starts can come while the main connection is still reconnecting, and a
// read sent then fails at once, the reconnect buffer being off, and costs the session a retry
// backoff for a connection that was back a moment later.
func (m *sessionRelays) storedFilter(ctx context.Context, sessionID string) ([]string, uint64, bool) {
	err := m.client.AwaitConnected(ctx)
	var topics []string
	var revision uint64
	if err == nil {
		topics, revision, err = storedTopics(ctx, m.registry, sessionID)
	}
	if err != nil {
		m.logger.Warn("session relay could not read the session's interests; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return nil, 0, false
	}
	if m.delivery.afterStoredRowRead != nil {
		m.delivery.afterStoredRowRead()
	}
	filtered, err := m.delivery.storedRowFilter(ctx, m.registry, sessionID, topics)
	if err != nil {
		m.logger.Warn("session relay could not filter the session's stored interests; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return nil, 0, false
	}
	return filtered, revision, true
}

// settleFilter holds a restart's filter write to the interest row it was built from: wrote is the
// KV revision of that row, and while the bucket answers a higher one the consumer's filter is set
// again from the newest row (fenceFilterWrite), since the restart's write may be an older row's
// filter over the one a registration on another listener set. The correction is a filter update
// and no second restart: the consumer the restart left keeps the position it has, which is where
// the restart put it.
//
// The caller holds the session's lock, as it does for the restart's own call, so the corrections
// are ordered with every other consumer call this listener makes for the session, and each one
// is the step every writer of the stored row's filter takes (storedFilterStep): a consumer gone
// by the time a correction is sent is left to the session's next registration, as the restart
// itself leaves it. A restart whose answer did not come back is settled here too, since it may
// have created the consumer on the filter it carried (restart, consumerCallAnswered).
func (m *sessionRelays) settleFilter(ctx context.Context, sessionID string, wrote uint64) {
	// The restart's own write is this fence's first pass: it is made, or may have been made,
	// before the fence runs, so that pass answers it as the write it is (filterWriteLanded) and
	// sends nothing, and the passes after it correct it from the bucket's newest row.
	restart := true
	fenceFilterWrite(ctx, m.logger, m.registry, sessionID, filterAfterRestart, func() (uint64, filterWriteOutcome) {
		if restart {
			restart = false
			return wrote, filterWriteLanded
		}
		return m.delivery.storedFilterStep(ctx, m.registry, sessionID, filterAfterRestart)
	})
}

// restartFailed is the outcome of a restart (sessionRelays.restart) whose call or pull failed
// with err. A call lost with its connection (bus.ErrRequestLostWithConnection) restarts at once,
// as a pull lost that way does (pullEnded): the call is sent again on the connection that came
// back, and the backoff a failure of the session's own would earn says nothing about it.
func (m *sessionRelays) restartFailed(ctx context.Context, sessionID string, err error) (bus.SessionConsumerStart, *bus.SessionPull, relayOutcome) {
	switch {
	case ctx.Err() != nil:
		return bus.SessionConsumerStart{}, nil, relayEnded
	case errors.Is(err, bus.ErrSessionPullBound):
		m.logger.Info("session relay started no pull of the session's consumer: this pod's receive term is over, or another pod may hold the machine's lease; it tries again after its retry wait while the session is still this listener's",
			slog.String("session_id", sessionID))
		return bus.SessionConsumerStart{}, nil, relayRetry
	case errors.Is(err, bus.ErrSessionConsumerNotFound) || errors.Is(err, bus.ErrNoSessionConsumerName):
		m.logger.Info("session relay paused: the session has no consumer; its next registration creates one", slog.String("session_id", sessionID))
		return bus.SessionConsumerStart{}, nil, relayEnded
	case errors.Is(err, bus.ErrRequestLostWithConnection):
		m.logger.Info("session relay restart lost a call with its connection; restarting at the ack floor at once", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return bus.SessionConsumerStart{}, nil, relayReconnected
	case consumerGrantRefused(err):
		m.delivery.refused(sessionID, err)
		return bus.SessionConsumerStart{}, nil, relayEnded
	default:
		m.logger.Warn("session relay could not restart the session's consumer; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
}

// relayNATS restarts a NATS session's consumer at its ack floor and publishes each message it pulls
// as a frame on the session's relay subject, in stream order, with up to the consumer's
// MaxAckPending in flight. It pauses, sending nothing more, once the oldest frame the session has
// not answered has waited the receipt wait (receiptWait): the frames in flight are left to their
// ack wait, and the relay starts again at the ack floor after its pause backoff or at the
// session's next registration, whichever comes first (sessionRelays.relay), so the messages it
// was sending reach a session that was only slow without waiting for a registration.
//
// The pull is held to this task's receive term (pullBound). Its requests wait the pull's own 30 s
// at the server, but once the term's context has ended or the lease's loss deadline has passed,
// it sends no request and hands out no message, and the run unsubscribes its inbox at once
// (bus.SessionPull.Detach): its waits end with the term's context, and the pull wakes at the loss
// deadline. The server drops a waiting request whose inbox has no subscriber, at the next message
// it would deliver to it or the next time it reads its waiting requests, without handing it that
// message (nats-server v2.15.1 server/consumer.go:4519-4559, nextWaiting, and :5143-5157,
// processWaiting). Measured on the in-process server, a message stored right after the
// unsubscribe reached no one until the next holder's pull took it, and the consumer counted no
// waiting request 1.5 ms after the unsubscribe began. The next holder's restart at the ack floor
// ends this pod's waiting request only where it deletes the consumer, since a message awaits an
// ack (releaseAnyPendingRequests, :3231-3247, answers it 409 Consumer Deleted). Where nothing awaits
// one, the restart updates the consumer in place (updateConfig, :2607, keeps every waiting request)
// and the server goes on sharing messages between that request and the next holder's: measured,
// each got every other message. So the unsubscribe is what leaves the next holder the only puller.
//
// Two limits of that unsubscribe, and what holds each. First, on a clustered hub the unsubscribe
// crosses the routes before the server that holds the consumer sees it, and that delay was not
// measured. Second, a pod cut off from the server whose connection the server still holds cannot
// unsubscribe at all, so its waiting request stays until the server drops the connection or the
// request's 30 s expire. In both cases the drop on arrival below keeps a message the server hands
// this pod from being forwarded, so the session never gets it twice, and the message reaches the
// next holder after the session consumer's ack wait (60 s) or at that holder's next restart of the
// consumer at its ack floor, whichever comes first.
//
// A message the pull receives once the term is over, before that unsubscribe lands, is not
// forwarded to the session and is not answered, with no done, nak or progress. The server
// delivers it again once the session consumer's ack wait has passed, 60 s
// (bus.SessionConsumerAckWait, which bus.SessionConsumers.applySettings sets as the AckWait of
// every session consumer). The next holder's restart of the consumer at its ack floor delivers it
// sooner, since a message that awaits an ack makes that restart a delete and a create
// (bus.SessionConsumers.KeepListed keeps no consumer whose NumAckPending is not zero).
//
// When the pull got a message is what tells the two apart. The pull reads its bound as each
// message arrives, and again as Next hands the message out, and this run publishes a message as
// a frame as soon as Next hands it out. So a frame is a message that arrived, and was handed out,
// while the term was current. A role message among them is judged within the term as well
// (takeOverFrame): one the term ended before, or under the read of its role's claim or its copy,
// is left at the consumer, neither sent nor answered. A message that arrives after the term is
// over is dropped where it arrives, and one that arrived before and is not handed out stays in the
// pull. The frames keep the settle a stopped relay gives them (endFrames, settleStoppedFrames): a
// run whose pull is past its bound does not return, but waits for the term's end to cancel it,
// reading the bound again every relayTermPoll. Its cancellation then settles its frames, as
// before, and returns to the server what the pull got during the term and did not hand out
// (bus.SessionPull.Release). A run whose term is current again, as after a renewal that landed
// late, starts again at the ack floor at once with a new pull (relayReconnected).
func (m *sessionRelays) relayNATS(ctx context.Context, run *relayRun) relayOutcome {
	sessionID := run.sessionID
	// An edge listener relays a session only on the projects of the token it admitted the
	// session's registration or role claim on, which it holds in memory (edgeAdmissions) and never
	// reads back from the session's record. A session no registration or claim of reached this
	// listener since it started - one this replica's restart forgot, one whose record names this
	// machine and was written by neither here, or one whose admission was forgotten after its entry
	// left the cache (pruneAdmissions) - is relayed nothing, and its messages wait in its consumer
	// until it registers or claims a role through this listener, which records the admission and
	// starts the relay again (apply).
	if m.repositories != nil {
		if _, admitted := m.delivery.admissions.of(sessionID); !admitted {
			m.logger.Info("edge relay holds the session's messages in its consumer until the session registers or claims a role through this listener",
				slog.String("session_id", sessionID))
			return relayEnded
		}
	}
	start, pull, outcome := m.restart(ctx, run)
	if pull == nil {
		return outcome
	}
	// A run whose context was cancelled lost its session: the session was deregistered or
	// reaped, it moved to another listener, the machine's receive term ended, the listener is
	// stopping, or a new epoch replaced the run. Such a run returns everything it holds; one
	// that ended for a reason of its own leaves it where it is.
	defer func() {
		if ctx.Err() != nil {
			// The inbox is unsubscribed before the frames settle, so the server hands the pull
			// nothing more meanwhile (relayNATS's doc).
			_ = pull.Detach()
		}
		// dropped is what endFrames left (its doc, and settleStoppedFrames' for a term's end);
		// a cancelled run returns it below (release).
		dropped := m.endFrames(ctx, run)
		if ctx.Err() == nil {
			pull.Stop()
			return
		}
		// The pull is detached before anything is naked: above already for a run cancelled when
		// it returned, here for one cancelled since. A cancelled pull still has a pull request
		// standing at the server, so a nak sent while that pull is subscribed can be delivered
		// straight back into the pull this run is giving up; its drain queues the message and the
		// Release below naks it again, so the next owner gets it once more, as a further delivery
		// and behind messages of higher sequences. Detached, the pull has no interest left and the
		// frames reach the session's next owner, where it has one, in the order they are naked.
		// TestSessionConsumerRelayLosingItsSessionReturnsEveryMessageItHoldsInStreamOrder
		// holds that order. The detach's error is not read here: Release calls Detach again,
		// which answers the same result, and joins it into the error it returns.
		_ = pull.Detach()
		// Every frame the run published was read from the pull's queue before anything still in
		// it, so the frames are naked first and the queue after, each in the order it was sent or
		// queued.
		returned, err := m.release(sessionID, dropped)
		fromPull, pullErr := pull.Release()
		returned += fromPull
		// What the line says depends on the reason, because a session with no record is
		// delivered these messages only when it registers again, and not at all where the
		// delete that took the record took its consumer: the line for every other reason
		// promises the delivery of a next pull that neither case makes (relayHandOverLines).
		reason := relayCancelledFor(ctx)
		whole, failed := relayHandOverLines(reason)
		// The run writes its hand-over line itself rather than the loop that cancelled it: only
		// once the run's goroutine has given the messages back is it true that this listener
		// relays the session no more, so a reader, and a test waiting for the hand-over, can
		// order on it. One line per run stopped while it was pulling, so a listener that stops
		// writes one for each session it was pulling for; a run cancelled before its pull
		// opened, or while it waited out a pause or a retry, held nothing and writes none.
		//
		// The logger stamps every line with the machine id of the process it runs in; the
		// relay_machine_id field is the relay set's own, which a test rig names apart from its
		// logger, and a second machine_id key in one record would leave a reader to guess which
		// won.
		if err := errors.Join(err, pullErr); err != nil {
			m.logger.Warn(failed,
				slog.String("session_id", sessionID),
				slog.String("relay_machine_id", m.machineID),
				slog.String("reason", reason.String()),
				slog.Int("returned", returned),
				slog.String("error", err.Error()))
			return
		}
		m.logger.Info(whole,
			slog.String("session_id", sessionID),
			slog.String("relay_machine_id", m.machineID),
			slog.String("reason", reason.String()),
			slog.Int("returned", returned))
	}()
	if start.StreamID == "" {
		m.logger.Warn("session relay could not read the stream's identity; retrying", slog.String("session_id", sessionID), slog.String("error", bus.ErrNoStreamID.Error()))
		return relayRetry
	}
	// gapReason names the gap the next frame marks, "" for the stream's retention gap
	// (relayHeaderGapReason). owed is the mark the next frame carries for GitHub events withheld
	// with no frame sent after them, which the frame clears once it is published
	// (repositoryGapSent), "" for none: the events are acknowledged, so the stream's start says
	// nothing of them. It is the mark this run noted for an event it withheld, and before each
	// frame that owes none, the mark this listener's copy of the session's entry carries
	// (owedRepositoryGap): one an earlier run of the session wrote and sent no frame after, or one
	// another relay of the session wrote while this run is live, as two relays of one session run
	// while it moves, which rides the first frame after the copy shows it. carried is the last
	// mark a frame of this run carried, which the run does not take from the copy again: the copy
	// can trail the clear, and the session would be sent the same gap notice twice.
	streamID, gap, gapReason, owed, carried := start.StreamID, start.Gap, "", "", ""
	// The pull's waits end with the term's context as well as the run's, so a term that ends wakes
	// the run at once; the pull itself wakes at the loss deadline (bus.PullBound).
	pullCtx, cancelPull := context.WithCancel(ctx)
	defer cancelPull()
	if term, _ := m.currentTerm(); term != nil {
		unwatch := context.AfterFunc(term.ctx, cancelPull)
		defer unwatch()
	}
	for {
		next := pullCtx
		cancelNext := context.CancelFunc(func() {})
		if deadline, waiting := m.receiptDeadline(run); waiting {
			if !time.Now().Before(deadline) {
				return relayPaused
			}
			next, cancelNext = context.WithDeadline(pullCtx, deadline)
		}
		msg, err := pull.Next(next)
		cancelNext()
		if err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			if errors.Is(err, bus.ErrSessionPullBound) || pullCtx.Err() != nil {
				// The term is over, or another pod may hold the lease: the inbox is unsubscribed at
				// once, so the server hands the pull nothing more, and nothing more is forwarded.
				// The run waits for the term's end to cancel it, so its frames settle.
				_ = pull.Detach()
				for m.pullBound() <= 0 {
					if waitUnlessDone(ctx, relayTermPoll) != nil {
						return relayEnded
					}
				}
				m.logger.Info("session relay starts again at the ack floor with a new pull: the term its pull was held to ended, and this task serves a term again",
					slog.String("session_id", sessionID))
				return relayReconnected
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return m.pullEnded(sessionID, err)
		}
		// At the edge a GitHub event reaches the session only while a fresh answer of its
		// token's projects names the repository the event itself names (relayVerdict). One that
		// does not is acknowledged undelivered and counted, and while no such answer can be read
		// the session's next frame carries the gap mark, since the events withheld then are ones
		// the session may have been owed, with the reason repository_withheld; a retention gap
		// the same frame already marks keeps no reason, since it lost more. Such an event is
		// acknowledged only once the session's entry carries the mark (noteRepositoryGap): an
		// entry that could not be marked has the event given back to the server, as takeOverFrame
		// gives back a frame whose copy did not store, so a redelivery marks the entry and a move
		// or a restart before the session's next frame still sends the mark.
		verdict := m.edgeWithholds(sessionID, msg)
		switch verdict.action {
		case frameHold:
			// A frame of a session this replica admitted no registration of is left
			// unacknowledged, for the consumer to deliver it again once the session registers
			// here; never acknowledged away.
			continue
		case frameAnswerUnknown:
			mark, err := m.noteRepositoryGap(ctx, sessionID)
			if err != nil {
				m.giveBackWithheld(sessionID, msg, err)
				continue
			}
			if !gap {
				gapReason = relayGapRepositoryWithheld
			}
			gap, owed = true, mark
			m.acknowledgeWithheld(sessionID, msg, verdict.reason)
			continue
		case frameDrop:
			m.endGiveBacks(sessionID, verdict)
			m.acknowledgeWithheld(sessionID, msg, verdict.reason)
			continue
		}
		// A role message whose role another session holds, or none does, is copied to the role's
		// kept lane rather than sent, under this task's term (takeOverFrame, which bounds itself by
		// sessionRelays.termContext); a gap the restart named stays on the next frame this run sends.
		if m.takeOverFrame(ctx, sessionID, msg) {
			continue
		}
		if owed == "" {
			if mark := m.owedRepositoryGap(sessionID); mark != "" && mark != carried {
				owed = mark
				if !gap {
					gap, gapReason = true, relayGapRepositoryWithheld
				}
			}
		}
		m.endGiveBacks(sessionID, verdict)
		if err := m.publishFrame(ctx, run, msg, streamID, gap, gapReason, owed); err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			m.logger.Warn("session relay could not publish a frame; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		if owed != "" {
			// owed is set only together with gap (the frameAnswerUnknown case and the
			// owedRepositoryGap adoption above), so a frame carrying a gap mark clears that mark as
			// sent and becomes the one carried onto the next frame that owes none.
			m.repositoryGapSent(ctx, sessionID, owed)
			carried = owed
		}
		gap, gapReason, owed = false, "", ""
	}
}

// noteRepositoryGap records that sessionID is owed the gap mark of a GitHub event the edge
// withholds while no fresh repository answer stands, until a frame carrying a gap mark is
// published (repositoryGapSent), and answers the mark: on the session's entry first, which the
// next listener to relay the session reads after a move or a restart
// (session.SessionEntry.RepositoryGap), then in this process. The mark is a new id for
// every withheld event, so the clear after a frame removes the mark that frame carried alone: a
// mark written since, by this relay or by another relay of the session, as two relays of one
// session run while it moves, stays on the entry for a later frame. A relay that adopted the
// entry's mark (owedRepositoryGap) and clears it after its frame therefore never clears the mark
// of an event another relay withheld later. The entry is read from the bucket for the write, within
// repositoryGapWriteTimeout of ctx (session.SessionRegistry.SetRepositoryGap), not judged from
// this listener's copy, which can trail a clear another listener made. A write that
// failed or did not answer in time, and an entry the bucket does not hold, are the error, and
// nothing is noted: the caller gives the event back (giveBackWithheld), since a mark this process
// alone kept would be lost with a move.
func (m *sessionRelays) noteRepositoryGap(ctx context.Context, sessionID string) (string, error) {
	mark := id.New()
	writeCtx, cancel := context.WithTimeout(ctx, repositoryGapWriteTimeout)
	defer cancel()
	if err := m.sessions.SetRepositoryGap(writeCtx, sessionID, mark); err != nil {
		return "", err
	}
	m.repositoryGapsMu.Lock()
	given := m.givenBack[sessionID]
	delete(m.givenBack, sessionID)
	if m.repositoryGaps == nil {
		m.repositoryGaps = map[string]string{}
	}
	m.repositoryGaps[sessionID] = mark
	m.repositoryGapsMu.Unlock()
	if given > 0 {
		m.logger.Info("edge listener recorded a gap mark on the session's entry again, after giving back the GitHub events it withheld while the entry could not record one",
			slog.String("session_id", sessionID), slog.Int("given_back", given))
	}
	return mark, nil
}

// repositoryGapWriteTimeout bounds each write a relay makes of its session's gap mark, the mark
// (noteRepositoryGap) and the clear after a frame (repositoryGapSent): a read and a write of the
// session's entry, each of which answers in milliseconds on a bus that answers at all. The relay
// run's own context has no deadline, and the session's frames wait behind the write, so a bucket
// that stops answering holds them this long at most: the event is then given back, or the clear
// logged and the mark sent again by the session's next run.
const repositoryGapWriteTimeout = 2 * time.Second

// owedRepositoryGap is the mark sessionID is owed, "" for none: the one this process noted, else
// the one the session's entry carries (noteRepositoryGap).
func (m *sessionRelays) owedRepositoryGap(sessionID string) string {
	m.repositoryGapsMu.Lock()
	mark, noted := m.repositoryGaps[sessionID]
	m.repositoryGapsMu.Unlock()
	if noted {
		return mark
	}
	entry, _ := m.sessions.Lookup(sessionID)
	return entry.RepositoryGap
}

// repositoryGapSent clears mark, the one sessionID was owed, a frame carrying a gap mark having
// been published: in this process, and on the session's entry while the entry carries that mark
// (session.SessionRegistry.ClearRepositoryGap), within repositoryGapWriteTimeout of ctx. A mark
// written since, for an event withheld later, is a later frame's to clear. A write of the entry
// that fails or does not answer in time is logged; the entry then keeps the mark, and the
// session's next relay run sends it once more.
// mark is never empty: relayNATS calls this only for a frame that owed a mark (owed != "").
func (m *sessionRelays) repositoryGapSent(ctx context.Context, sessionID, mark string) {
	m.repositoryGapsMu.Lock()
	if m.repositoryGaps[sessionID] == mark {
		delete(m.repositoryGaps, sessionID)
	}
	m.repositoryGapsMu.Unlock()
	if entry, _ := m.sessions.Lookup(sessionID); entry.RepositoryGap == "" {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, repositoryGapWriteTimeout)
	defer cancel()
	if err := m.sessions.ClearRepositoryGap(writeCtx, sessionID, mark); err != nil {
		m.logger.Warn("edge listener could not clear the session's repository gap on its entry; its next relay run sends the mark again",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// forgetRepositoryGap clears what this process noted for sessionID and leaves the session's entry
// as it is: the session is no longer this listener's to relay, and the next listener that relays
// it reads the mark there.
func (m *sessionRelays) forgetRepositoryGap(sessionID string) {
	m.repositoryGapsMu.Lock()
	defer m.repositoryGapsMu.Unlock()
	delete(m.repositoryGaps, sessionID)
}

// edgeWithholds reports whether an edge listener withholds msg from sessionID, and why: a frame
// reaches the session only for a project of the token it was admitted on, as its own message, or
// as a GitHub event whose own repository a fresh answer of its token's projects names
// (edgeFrameVerdict). The projects are the token's this replica admitted the session's
// registration on (edgeAdmissions), never the session's stored record, so no record the bus can
// forge widens them; a session this replica admitted no registration of withholds every frame
// unacknowledged (withheldNotAdmitted). Every other listener withholds nothing. A withheld message
// is acknowledged (acknowledgeWithheld) or given back (giveBackWithheld) by the relay loop, which
// decides which. Both answer the message with no check of the receive term, which takeOverFrame
// holds every answer of a frame to, because an edge listener holds no lease and runs one pod: an
// edge standby would need that withhold fenced by the lease, with the change to the edge's grant
// that its owner approves.
func (m *sessionRelays) edgeWithholds(sessionID string, msg bus.SessionMessage) frameVerdict {
	if m.repositories == nil {
		return framePasses()
	}
	projects, admitted := m.delivery.admissions.of(sessionID)
	if !admitted {
		return frameHolds()
	}
	return edgeFrameVerdict(m.repositories, sessionID, projects, msg.Subject, msg.Data)
}

// acknowledgeWithheld acknowledges msg, a frame the edge withholds from sessionID for
// reason, undelivered, counts it and logs it: the consumer holds it no longer, and nothing else is
// ever sent for it.
func (m *sessionRelays) acknowledgeWithheld(sessionID string, msg bus.SessionMessage, reason string) {
	m.repositories.withheld.Inc([2]string{"reason", reason})
	if err := msg.Ack(); err != nil {
		m.logger.Warn("edge listener could not acknowledge a frame it withholds; the consumer delivers it again",
			slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("error", err.Error()))
		return
	}
	m.logger.Info("edge listener withheld a frame from a session",
		slog.String("session_id", sessionID), slog.String("subject", msg.Subject), slog.String("reason", reason))
}

// giveBackWithheld hands msg, a GitHub event the edge withholds from sessionID while no fresh
// repository answer stands, back to the server with the delay its deliveries earn
// (relayNakDelayFor), neither acknowledged nor counted: the session's entry could not be given
// the gap mark the event owes (err, noteRepositoryGap), so the event stays the consumer's until a
// redelivery marks the entry. Only the first give-back of a run logs err (givenBack): the server
// delivers the event again within seconds, and each delivery is given back while the entry takes
// no mark, so a line at each would fill the log for as long as the bucket refuses the write. A nak
// that fails is logged at every give-back: the server then holds the event for the consumer's ack
// wait rather than the nak's delay, which is no repeat of the mark's failure.
func (m *sessionRelays) giveBackWithheld(sessionID string, msg bus.SessionMessage, err error) {
	nakErr := msg.NakWithDelay(relayNakDelayFor(msg.Delivered))
	m.repositoryGapsMu.Lock()
	if m.givenBack == nil {
		m.givenBack = map[string]int{}
	}
	m.givenBack[sessionID]++
	first := m.givenBack[sessionID] == 1
	m.repositoryGapsMu.Unlock()
	if first {
		m.logger.Warn("edge listener gave a withheld GitHub event back to the server: the session's entry could not record its gap mark; the server delivers it again, and each withheld event of the session is given back unlogged until a mark write of the session lands or a fresh repository answer decides its GitHub events, which is logged",
			slog.String("session_id", sessionID),
			slog.String("subject", msg.Subject),
			slog.Uint64("stream_seq", msg.StreamSeq),
			slog.String("error", err.Error()))
	}
	if nakErr != nil {
		m.logger.Warn("edge listener could not give a withheld GitHub event back to the server: its nak failed, so the consumer delivers it again once its ack wait has passed",
			slog.String("session_id", sessionID),
			slog.String("subject", msg.Subject),
			slog.Uint64("stream_seq", msg.StreamSeq),
			slog.String("error", nakErr.Error()))
	}
}

// endGiveBacks ends sessionID's run of give-backs (givenBack) once a fresh repository answer decides
// a GitHub event of the session, verdict being the edge's decision on the event: an answer that
// relays it or drops it (frameVerdict.answered) means the session's GitHub events are no longer
// withheld for want of an answer, and a give-back in a later outage starts a run of its own and logs
// it. Any other verdict says nothing of the answer and ends no run: a frame that is no GitHub event,
// and a drop of an event that names no repository or whose envelope is no GitHub event's. A
// listener that is no edge gives nothing back and decides nothing so.
func (m *sessionRelays) endGiveBacks(sessionID string, verdict frameVerdict) {
	if !verdict.answered {
		return
	}
	m.repositoryGapsMu.Lock()
	given := m.givenBack[sessionID]
	delete(m.givenBack, sessionID)
	m.repositoryGapsMu.Unlock()
	if given > 0 {
		m.logger.Info("edge listener decides the session's GitHub events by a fresh repository answer again, after giving back the events it withheld while the session's entry could not record a gap mark",
			slog.String("session_id", sessionID), slog.Int("given_back", given))
	}
}

// pullEnded is what a relay does when its pull ended with err while the relay itself goes on: a
// pull lost with its connection starts again at the ack floor at once (relayReconnected), since
// the failures before the reconnect say nothing about the pull the new connection makes, and any
// other end is retried after the relay's backoff (relayRetry). It logs the line that matches.
func (m *sessionRelays) pullEnded(sessionID string, err error) relayOutcome {
	if errors.Is(err, bus.ErrSessionPullReconnected) {
		m.logger.Info("session relay pull lost with its connection; restarting at the ack floor at once", slog.String("session_id", sessionID))
		return relayReconnected
	}
	m.logger.Warn("session relay pull ended; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
	return relayRetry
}

// publishFrame publishes msg as a frame on the session's relay subject, its reply subject one of
// its own under the listener's reply inbox that names the session (bus.SessionReplySubject). gap
// marks the frame as the first after a gap, and gapReason names that gap when it is not the
// stream's retention gap.
func (m *sessionRelays) publishFrame(ctx context.Context, run *relayRun, msg bus.SessionMessage, streamID string, gap bool, gapReason, owed string) error {
	reply := bus.SessionReplySubject(m.inbox, run.sessionID)
	token := reply[strings.LastIndexByte(reply, '.')+1:]
	frame := &relayFrame{run: run, acker: msg.SessionAcker, sentAt: time.Now(), subject: msg.Subject, streamSeq: msg.StreamSeq, data: msg.Data, delivered: msg.Delivered, gapMark: owed}
	m.framesMu.Lock()
	if run.outstanding == nil {
		run.outstanding = map[string]*relayFrame{}
	}
	run.outstanding[token] = frame
	run.unanswered = append(run.unanswered, frame)
	m.frames[token] = frame
	m.framesMu.Unlock()
	header := nats.Header{}
	header.Set(relayHeaderSubject, msg.Subject)
	header.Set(relayHeaderStreamSeq, strconv.FormatUint(msg.StreamSeq, 10))
	header.Set(relayHeaderStreamID, streamID)
	header.Set(relayHeaderDelivered, strconv.FormatUint(msg.Delivered, 10))
	header.Set(relayHeaderStoredAt, msg.StoredAt.UTC().Format(time.RFC3339Nano))
	if gap {
		header.Set(relayHeaderGap, "true")
		if gapReason != "" {
			header.Set(relayHeaderGapReason, gapReason)
		}
	}
	return m.delivery.consumers.Publish(ctx, &nats.Msg{
		Subject: bus.DeliverSubject(run.sessionID),
		Reply:   reply,
		Header:  header,
		Data:    msg.Data,
	})
}

// receiptDeadline is when the oldest frame of run the session has not answered reaches the
// receipt wait (receiptWait), and whether there is one.
func (m *sessionRelays) receiptDeadline(run *relayRun) (time.Time, bool) {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	for len(run.unanswered) > 0 && run.unanswered[0].answered {
		run.unanswered[0] = nil
		run.unanswered = run.unanswered[1:]
	}
	if len(run.unanswered) == 0 {
		return time.Time{}, false
	}
	return run.unanswered[0].sentAt.Add(m.receiptWait), true
}

// forget drops every frame of run still awaiting an answer and returns them: a reply to one is
// ignored from here on. It answers nothing to the server, so each dropped frame's message is
// delivered again only once its ack wait has passed, or at the next restart at the ack floor.
// That is right for a run that ended for a reason of its own; a run that was cancelled gives
// what it dropped back to the server instead (release), and a run a term's end stopped settles
// first (settleStoppedFrames, whose doc has that rule).
//
// A frame leaves the run here or at the done or nak that ends it (onReply), under framesMu and
// once, so a frame this returns has not been acked and will not be.
//
// The frames come back in no order, since they are the run's outstanding map: a caller that
// sends them anywhere puts them in order itself (release).
func (m *sessionRelays) forget(run *relayRun) []*relayFrame {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	dropped := make([]*relayFrame, 0, len(run.outstanding))
	for token, frame := range run.outstanding {
		delete(m.frames, token)
		dropped = append(dropped, frame)
	}
	run.outstanding = nil
	run.unanswered = nil
	return dropped
}

// outstanding is how many frames of run the session has not answered done or nak.
func (m *sessionRelays) outstanding(run *relayRun) int {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	return len(run.outstanding)
}

// release returns the message of every frame in dropped to the server, so the session's next
// owner is delivered them at once rather than after their ack wait (bus.SessionConsumerAckWait).
// dropped is what ending the run's frames left (endFrames): everything the run had out, or, on a
// term's end, what the settle left unanswered (settleStoppedFrames, whose doc has that rule). A
// session whose record is gone has no owner at all, since no listener relays a session with no
// record: the naks of such a hand-over reach the consumer a lapsed record leaves standing, which
// holds those messages for the session's next registration, and are publishes the server drops
// where a stop or a reap took the consumer with the record; the run's line says both
// (relayCancelSessionGone, relayHandOverLines). A nak is safe to repeat as far as the server
// goes, since the session consumer sets MaxDeliver = -1 (internal/bus/session_consumers.go,
// applySettings): a message returned here spends no server redelivery budget. Each delivery
// still raises the message's delivery count, which the nak ladder climbs by (relayNakDelayFor),
// so a message handed over is naked a rung higher the next time the session refuses it. It
// returns how many messages it returned, and the naks that failed as one error, so that the
// run's one hand-over line can say whether the hand-over was whole rather than claiming it was.
func (m *sessionRelays) release(sessionID string, dropped []*relayFrame) (int, error) {
	// The frames are naked in the order they were sent, which is stream order except for a
	// message the session naked and the relay sent again. No nak is sent under framesMu: it is a
	// publish, and the lock answers every session's replies.
	slices.SortFunc(dropped, func(a, b *relayFrame) int { return a.sentAt.Compare(b.sentAt) })
	held := make([]bus.SessionAcker, 0, len(dropped))
	for _, frame := range dropped {
		held = append(held, frame.acker)
	}
	return bus.ReturnMessages("the relay of session "+sessionID, held)
}

// relayNakDelayFor is how long the server waits before it delivers a frame the session refused
// (nak) again: relayNakDelay for a message on its first delivery, doubled for every delivery it
// has had before this one, and relayNakDelayMax from the rung that reaches that ceiling on, so
// one frame the session goes on refusing comes back 1, 2, 4, 8, 16 s and then 30 s apart.
//
// delivered is the message's own delivery count, which the frame carried from the pull that
// published it (relayFrame.delivered); a pulled message is on its first delivery at 1. A count
// of 0 is no count at all, and so is one past relayNakShiftMax+1: both take the ceiling, rather
// than a delay shorter than the rung before it or one the shift overflowed.
func relayNakDelayFor(delivered uint64) time.Duration {
	if delivered == 0 || delivered > relayNakShiftMax+1 {
		return relayNakDelayMax
	}
	return min(relayNakDelay<<(delivered-1), relayNakDelayMax)
}

// onReply hears a session's reply to a frame: received marks the frame answered, progress restarts
// its ack wait, done acks it and nak has the server deliver it again after the delay the message's
// delivery count earns (relayNakDelayFor); done and nak end the frame. Any of them is the session
// answering, which puts the run's pause backoff back to its first wait. A reply to a token no
// frame awaits, or with any other body, is ignored.
//
// A reply moves only the frame of the session its subject names, the session the frame was sent
// to: one whose subject names another session moves nothing, so a session that learned another's
// token cannot acknowledge that session's message away under its own reply prefix, the one its
// grant lets it publish to (natsgrant.Session publishes bus.SessionReplies in place of every
// _INBOX. subject). The first such reply is logged at WARN, then at most one every
// relayRefusedReplyLogEvery, each line counting the replies held back before it.
func (m *sessionRelays) onReply(msg *nats.Msg) {
	rest, ok := strings.CutPrefix(msg.Subject, m.replyPrefix)
	if !ok {
		return
	}
	// A session id holds no dot (bus.SessionConsumerName), so the session is the first token and
	// the token the rest; a rest with a dot of its own is a token no frame holds.
	sessionID, token, ok := strings.Cut(rest, ".")
	if !ok {
		return
	}
	reply := string(bytes.TrimSpace(msg.Data))
	switch reply {
	case relayReplyReceived, relayReplyProgress, relayReplyDone, relayReplyNak:
	default:
		return
	}
	m.framesMu.Lock()
	frame := m.frames[token]
	if frame == nil {
		m.framesMu.Unlock()
		return
	}
	if frame.run.sessionID != sessionID {
		now := time.Now()
		logLine := m.refusedLoggedAt.IsZero() || now.Sub(m.refusedLoggedAt) >= relayRefusedReplyLogEvery
		held := m.refusedHeld
		if logLine {
			m.refusedLoggedAt, m.refusedHeld = now, 0
		} else {
			m.refusedHeld++
		}
		m.framesMu.Unlock()
		if logLine {
			m.logger.Warn("session relay refused a reply naming another session than the one its frame was sent to; the frame is unchanged",
				slog.String("session_id", frame.run.sessionID), slog.String("reply_session_id", sessionID), slog.String("reply", reply),
				slog.Int("held", held))
		}
		return
	}
	frame.answered = true
	if reply == relayReplyDone || reply == relayReplyNak {
		delete(m.frames, token)
		delete(frame.run.outstanding, token)
	}
	frame.run.answered.Store(true)
	m.framesMu.Unlock()
	var err error
	switch reply {
	case relayReplyProgress:
		err = frame.acker.InProgress()
	case relayReplyDone:
		err = frame.acker.Ack()
	case relayReplyNak:
		err = frame.acker.NakWithDelay(relayNakDelayFor(frame.delivered))
		if frame.gapMark != "" {
			// onReply is the one callback for every session's receipts on this connection's core
			// subscription, run one at a time (nats.go), so the re-mark's KV read and write
			// (noteRepositoryGap, up to sendRequestBudget) runs off the callback: a slow write
			// stalls no other session's receipt. It runs with one re-mark in flight per session
			// (scheduleReMark), so two naks of one session cannot interleave that read and write.
			// The owed mark is on the frame already (gapMark), so the relay loop needs nothing back
			// from the write; a write that fails is logged, and the session's next relay run sends
			// the mark (owedRepositoryGap).
			m.scheduleReMark(frame.run.sessionID)
		}
	}
	if err != nil {
		m.logger.Warn("session relay could not answer the consumer for a frame", slog.String("session_id", frame.run.sessionID), slog.String("reply", reply), slog.String("error", err.Error()))
	}
}

// reMarkWithheldGap re-marks sessionID's entry with a fresh repository gap mark, a frame that carried
// a mark having been answered nak (onReply): the frame did not reach the client, so the mark is not
// lost to it. It writes a fresh id, never the frame's own mark, since noteRepositoryGap mints a new
// id for every mark, so the relay loop's clear of that mark as sent (repositoryGapSent) can never
// take the re-mark's. Where its write lands, the worst case is then one redundant notice at a later
// run, never a lost one.
// While its write is in flight the mark this process noted, if any, stands, and the relay loop's
// owedRepositoryGap reads it rather than this listener's copy of the entry, which can trail the
// write: a mark noteRepositoryGap noted is on the entry too, since it writes the entry before it
// notes the mark, while one a failed re-mark noted (below) is in this process alone. It runs off
// onReply's callback, in its own goroutine, within sendRequestBudget, since the write is a KV read
// and write that must not stall the other sessions' receipts that one callback carries. A write
// that fails notes a fresh id in this process alone (the loop may have cleared the frame's own mark
// as sent, and noteRepositoryGap notes nothing when its write fails), so this listener's next relay
// run still sends it (owedRepositoryGap); a move to another listener, or the end of this
// listener's receive term (stopAll's clear), between the note and that run loses it, since the
// entry carries no mark.
func (m *sessionRelays) reMarkWithheldGap(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), sendRequestBudget)
	defer cancel()
	if _, err := m.noteRepositoryGap(ctx, sessionID); err != nil {
		// noteRepositoryGap notes nothing when its write fails, so this notes the fresh id itself,
		// as noteRepositoryGap's own tail does: the lazy-map create and the store.
		m.repositoryGapsMu.Lock()
		if m.repositoryGaps == nil {
			m.repositoryGaps = map[string]string{}
		}
		m.repositoryGaps[sessionID] = id.New()
		m.repositoryGapsMu.Unlock()
		m.logger.Warn("session relay could not re-mark a withheld gap frame's session; its next run sends a fresh mark it noted",
			slog.String("session_id", sessionID), slog.String("error", err.Error()))
	}
}

// scheduleReMark runs sessionID's withheld-gap re-mark off onReply's callback (reMarkWithheldGap)
// with one in flight per session: a re-mark asked while one runs for the session is folded into a
// single further run after it. onReply's own goroutine let two naks of one session re-mark at once,
// so noteRepositoryGap's read and write could interleave and leave the in-memory mark and the
// entry's mark different, sending the session one redundant gap notice; holding one in flight keeps
// the re-marks of one session serialized against each other. Against the relay loop's own
// noteRepositoryGap they are not serialized, but a re-mark writes a fresh id (reMarkWithheldGap), so
// the worst their overlap can do is leave one redundant gap notice for a later run, never lose one.
func (m *sessionRelays) scheduleReMark(sessionID string) {
	m.reMarkMu.Lock()
	if m.reMarking == nil {
		m.reMarking, m.reMarkAgain = map[string]bool{}, map[string]bool{}
	}
	if m.reMarking[sessionID] {
		m.reMarkAgain[sessionID] = true
		m.reMarkMu.Unlock()
		return
	}
	m.reMarking[sessionID] = true
	m.reMarkMu.Unlock()
	go func() {
		for {
			m.reMark(sessionID)
			m.reMarkMu.Lock()
			if m.reMarkAgain[sessionID] {
				delete(m.reMarkAgain, sessionID)
				m.reMarkMu.Unlock()
				continue
			}
			delete(m.reMarking, sessionID)
			m.reMarkMu.Unlock()
			return
		}
	}()
}

// decodeStoredEnvelope decodes a stored message into item under the durable path's rules
// (listenerDeliveryHandler): JSON, the wire rules, then the envelope's own.
func decodeStoredEnvelope(data []byte, item *contracts.Envelope) error {
	if err := json.Unmarshal(data, item); err != nil {
		return err
	}
	if err := contracts.ValidateWire(data); err != nil {
		return err
	}
	return item.Validate()
}
