package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
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
	// the stream's first sequence: messages the session had not taken are gone.
	relayHeaderGap = "Envoy-Gap"
)

// The replies a session sends on a frame's reply subject, each exactly one ASCII word.
const (
	// relayReplyReceived: the frame decoded, before any host work. The receipt.
	relayReplyReceived = "received"
	// relayReplyProgress: the frame is held unwritten; its ack wait starts again.
	relayReplyProgress = "progress"
	// relayReplyDone: the frame is written (or skipped); the message is acked. Also a receipt.
	relayReplyDone = "done"
	// relayReplyNak: the session cannot take the frame; the server delivers it again after
	// relayNakDelay.
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
	// relayNakDelay is how long the server waits to deliver a message a session refused (nak).
	relayNakDelay = time.Second
	// relayRetryFirst and relayRetryMax bound the wait before a relay whose consumer call or pull
	// failed restarts at the ack floor; it doubles from the first to the max.
	relayRetryFirst = time.Second
	relayRetryMax   = 30 * time.Second
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
)

// relayEvent is what the relay loop is told about a session; the flags of the events it has not
// handled yet merge.
type relayEvent uint8

const (
	// relayCheck: the session's entry changed; start or stop its relay to match it.
	relayCheck relayEvent = 0
	// relayResume: the session registered; a relay that paused starts again.
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
	// registration.
	relayEnded relayOutcome = iota
	// relayRetry: a consumer call or the pull failed; the relay restarts at the ack floor after a
	// wait while it is still this listener's.
	relayRetry
	// relayPaused: the session answered no frame within relayReceiptWait; the relay restarts at the
	// ack floor after the pause backoff, or at the session's next registration, whichever is first.
	relayPaused
)

// sessionRelays relays the session consumers this listener owns (sessionRelays.wanted): one relay
// per session, which restarts the session's consumer at its ack floor whenever it starts, then pulls
// it and hands each message on in stream order, as a core frame on the session's relay subject,
// answered on a reply subject under the listener's one reply inbox.
//
// Among the tasks of one machine id, only the one holding the machine's durable binding relays
// (setOwned), so a rolling deploy's second task relays nothing until the first lets go. The relay
// set follows the watched session cache (session.SessionRegistry.OnChange), so a session that
// moves to another machine id stops being relayed here at once, and a listener that starts relays
// every session the cache holds for it with no registration. A relay cancelled that way returns
// every message it still holds to the server before its run ends (sessionRelays.release), so the
// listener the session moved to is delivered them at once rather than after the consumer's ack
// wait, and writes one line saying it has.
//
// One loop goroutine owns the set of relays (relays) and applies the events it is sent in order;
// each relay runs on a goroutine of its own.
type sessionRelays struct {
	delivery *sessionDelivery
	registry *store.Registry
	// sessions is the session cache this relay reads its entries from; it is the same registry
	// the durable path's fan-out judges a followed topic against (judgeFollowedTopic).
	sessions  *session.SessionRegistry
	machineID string
	logger    *logging.Logger
	// replyPrefix is the listener's reply inbox and a dot: a frame's reply subject is
	// replyPrefix<token>, and one subscription to the inbox's wildcard hears every reply.
	replyPrefix string
	// pauseFirst and pauseMax are the bounds of a paused relay's backoff, relayPauseFirst and
	// relayPauseMax; a test shortens them. receiptWait is relayReceiptWait, how long the oldest
	// unanswered frame may wait before the relay pauses, and a test shortens it the same way, so
	// a row about what an unreachable session leaves behind spends its own short wait rather
	// than three seconds of the clock. Set before the first relay starts and read-only after.
	pauseFirst  time.Duration
	pauseMax    time.Duration
	receiptWait time.Duration

	ctx    context.Context
	cancel context.CancelCauseFunc
	owned  atomic.Bool
	// stopChange unregisters the session cache's change callback.
	stopChange func()
	// runs counts the relay goroutines; loopDone is closed once the loop has returned.
	runs     sync.WaitGroup
	loopDone chan struct{}

	// eventsMu guards events and ownershipChanged, which the loop takes each time wake fires.
	eventsMu         sync.Mutex
	events           map[string]relayNotice
	ownershipChanged bool
	wake             chan struct{}

	// relays is each session's current relay; only the loop reads or writes it.
	relays map[string]*relayRun
	// listed holds, by session id, the consumers listed for the relays that gaining the durable
	// binding starts together (listConsumers), until each relay's start takes its own; nil
	// otherwise. listedAt is the mark taken before that list. listRefusedLogged is set once a list
	// refused for want of the grant is logged. Only the loop reads or writes them.
	listed            map[string]*jetstream.ConsumerInfo
	listedAt          uint64
	listRefusedLogged bool

	// restarts holds a token for each relay restarting its consumer, relayRestartLimit at most.
	restarts chan struct{}

	// framesMu guards frames and each relay's outstanding and unanswered frames.
	framesMu sync.Mutex
	// frames maps the token of every frame awaiting done or nak to it.
	frames    map[string]*relayFrame
	nextToken atomic.Uint64
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
	// relayCancelNotOurs: the session is no longer this listener's to relay, because it registered
	// again through another listener, its entry left consumer delivery, or this task lost its
	// machine's durable binding.
	relayCancelNotOurs
	// relayCancelNewEpoch: the session registered with a new epoch, so a run starting at the ack
	// floor replaced this one.
	relayCancelNewEpoch
	// relayCancelListenerStopping: this listener is stopping, so every relay stops with it.
	relayCancelListenerStopping
	// relayCancelBindingLost: this task lost its machine's durable binding, so it relays nothing
	// until it holds the binding again.
	relayCancelBindingLost
)

// String is the word the log line carries for the reason.
func (r relayCancelReason) String() string {
	switch r {
	case relayCancelNotOurs:
		return "session_not_ours"
	case relayCancelNewEpoch:
		return "new_epoch"
	case relayCancelListenerStopping:
		return "listener_stopping"
	case relayCancelBindingLost:
		return "binding_lost"
	default:
		return "unknown"
	}
}

// Error is the same word as String, so a reason is usable as the cause of a cancellation
// (context.WithCancelCause), which takes an error.
func (r relayCancelReason) Error() string { return r.String() }

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
// it. It reports false, sending nothing, once the run is returning: the caller then starts a run in
// its place.
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

// exit marks the run as returning, and reports whether a wake was waiting for it: a registration
// the loop heard since the run's last pause began, for which no run would start otherwise.
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

// waitPause waits out a paused run's backoff, d, the session's next registration, or the end of the
// run, and reports whether the run goes on.
func (r *relayRun) waitPause(ctx context.Context, d time.Duration) bool {
	select {
	case <-r.resume:
	default:
	}
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
// a registration heard while the failed call or pull ran restarts the run at once.
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
// only at the session's next registration.
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
	// answered is set once the session sent any reply for the frame, under framesMu.
	answered bool
}

// startSessionRelays starts the relay of delivery's session consumers and makes it delivery's
// relay (sessionDelivery.relay). It relays nothing until setOwned(true).
func startSessionRelays(delivery *sessionDelivery, cfg listenerDeliveryHandlerConfig) (*sessionRelays, error) {
	inbox := nats.NewInbox()
	m := &sessionRelays{
		delivery:    delivery,
		registry:    cfg.registry,
		sessions:    cfg.sessions,
		machineID:   cfg.machineID,
		logger:      cfg.logger,
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
	if err := delivery.consumers.SubscribeReplies(inbox+".>", m.onReply); err != nil {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancelCause(context.Background())
	m.stopChange = cfg.sessions.OnChange(m.onChange)
	delivery.relay = m
	go m.loop()
	return m, nil
}

// setOwned records whether this task holds its machine's durable binding. Gaining it starts the
// relay of every session the cache holds for this machine, each restarted at its ack floor; losing
// it stops every relay.
func (m *sessionRelays) setOwned(owned bool) {
	if m == nil || m.owned.Swap(owned) == owned {
		return
	}
	m.logger.Info("session relays follow the durable binding", slog.Bool("relaying", owned))
	m.eventsMu.Lock()
	m.ownershipChanged = true
	m.eventsMu.Unlock()
	m.signal()
}

// registered implements sessionRelay: the session registered, so a relay that paused resumes and a
// new epoch restarts it at the ack floor, unless it restarted after heard (relayRun.restartedBefore).
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

// stop stops every relay and waits, within ctx, for their goroutines to return, so no relay acks or
// publishes after it; the session-consumer connection is drained after it (sessionDelivery.drain).
func (m *sessionRelays) stop(ctx context.Context) {
	if m == nil {
		return
	}
	m.stopChange()
	m.eventsMu.Lock()
	m.events = nil
	m.eventsMu.Unlock()
	m.cancel(relayCancelListenerStopping)
	select {
	case <-m.loopDone:
	case <-ctx.Done():
		m.logger.Warn("session relay loop still running at shutdown")
		return
	}
	if !waitWithin(ctx, &m.runs) {
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
		case <-prune.C:
			// Each lapse it finds is a change, which onChange hears.
			m.sessions.PruneLapsed()
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
				m.stopAll(relayCancelBindingLost)
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
// listener's to relay, stopped when it is not, started again when it registered and the run
// ended or is returning on its own (relayRun.wake), restarted at the ack floor on a new epoch
// registered after the run's last restart (relayRun.restartedBefore), and, for a run waiting out a
// pause or a retry, woken by the registration so it restarts at once.
func (m *sessionRelays) apply(sessionID string, notice relayNotice) {
	event := notice.event
	run := m.relays[sessionID]
	if !m.wanted(sessionID) {
		if run != nil {
			run.cancel(relayCancelNotOurs)
			delete(m.relays, sessionID)
		}
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

// stopAll stops every relay, each cancelled for reason: the listener is stopping, or the machine's
// durable binding went. Each caller names its own, so the line a stopped run writes says which
// happened.
func (m *sessionRelays) stopAll(reason relayCancelReason) {
	for sessionID, run := range m.relays {
		run.cancel(reason)
		delete(m.relays, sessionID)
	}
}

// wanted reports whether this listener relays sessionID: it holds its machine's durable binding,
// and the session's entry records consumer delivery and names this machine.
func (m *sessionRelays) wanted(sessionID string) bool {
	if !m.owned.Load() {
		return false
	}
	entry, err := m.sessions.Get(sessionID)
	if err != nil || entry.Delivery != session.DeliveryConsumer {
		return false
	}
	return entry.MachineID == m.machineID
}

// listConsumers reads every session consumer at once (bus.SessionConsumers.ListSessionConsumers)
// for the relays that gaining the durable binding starts together, so a relay whose consumer a
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
// started by the burst of gaining the durable binding takes the session's listed consumer.
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
// session's next registration. Both only while the session is still this listener's.
//
// The pause is logged once an episode: the line is written for the first pause since the session
// last answered a frame, not for each probe that found it silent again.
func (m *sessionRelays) relay(ctx context.Context, run *relayRun) {
	retry, pause, paused := relayRetryFirst, m.pauseFirst, false
	for {
		outcome := m.relayNATS(ctx, run)
		switch outcome {
		case relayEnded:
			return
		case relayPaused:
			if run.answered.Swap(false) {
				// The session answered a frame since the last pause: this one is a new episode.
				pause, paused = m.pauseFirst, false
			}
			if !paused {
				paused = true
				m.logger.Info("session relay paused: the session answered no frame within the receipt wait; it sends again after the pause backoff, or at the session's next registration",
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
		if !m.wanted(run.sessionID) {
			return
		}
	}
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
// The restart reads the session's stored topics under the session's lock, as a registration and
// an unsubscribe read them before they write the filter, so it never writes a filter read before
// one of them wrote a newer one. It takes its mark (sessionDelivery.mark) first, under the lock,
// and records it once it succeeds (relayRun.restartedAt).
//
// The first restart of a run the burst of gaining the durable binding started has the session's
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
		filter, ok := m.storedFilter(callCtx, sessionID)
		if !ok {
			unlock()
			return bus.SessionConsumerStart{}, nil, relayRetry
		}
		start, kept, err := m.delivery.consumers.KeepListed(callCtx, sessionID, filter, listed)
		unlock()
		if err == nil && kept {
			run.restartedAt.Store(run.listedAt)
			var pull *bus.SessionPull
			if pull, err = m.delivery.consumers.Pull(callCtx, sessionID); err == nil {
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
	filter, ok := m.storedFilter(callCtx, sessionID)
	if !ok {
		unlock()
		return bus.SessionConsumerStart{}, nil, relayRetry
	}
	start, err := m.delivery.consumers.RestartAtFloor(callCtx, sessionID, filter)
	unlock()
	if err == nil {
		run.restartedAt.Store(at)
		var pull *bus.SessionPull
		if pull, err = m.delivery.consumers.Pull(callCtx, sessionID); err == nil {
			return start, pull, relayRetry
		}
	}
	return m.restartFailed(ctx, sessionID, err)
}

// storedFilter is the filter of sessionID's consumer for its stored interest topics, read within
// ctx under the session's lock, which the caller holds; false, logged, when they cannot be read.
func (m *sessionRelays) storedFilter(ctx context.Context, sessionID string) ([]string, bool) {
	topics, err := storedInterestTopics(ctx, m.registry, sessionID)
	if err != nil {
		m.logger.Warn("session relay could not read the session's interests; retrying", slog.String("session_id", sessionID), slog.String("error", err.Error()))
		return nil, false
	}
	return sessionConsumerFilter(sessionID, topics), true
}

// restartFailed is the outcome of a restart (sessionRelays.restart) whose call or pull failed
// with err.
func (m *sessionRelays) restartFailed(ctx context.Context, sessionID string, err error) (bus.SessionConsumerStart, *bus.SessionPull, relayOutcome) {
	switch {
	case ctx.Err() != nil:
		return bus.SessionConsumerStart{}, nil, relayEnded
	case errors.Is(err, bus.ErrSessionConsumerNotFound) || errors.Is(err, bus.ErrNoSessionConsumerName):
		m.logger.Info("session relay paused: the session has no consumer; its next registration creates one", slog.String("session_id", sessionID))
		return bus.SessionConsumerStart{}, nil, relayEnded
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
func (m *sessionRelays) relayNATS(ctx context.Context, run *relayRun) relayOutcome {
	sessionID := run.sessionID
	start, pull, outcome := m.restart(ctx, run)
	if pull == nil {
		return outcome
	}
	// A run whose context was cancelled lost its session: the session moved to another listener,
	// the machine's durable binding was lost, the listener is stopping, or a new epoch replaced
	// the run. Such a run returns everything it holds; one that ended for a reason of its own
	// leaves it where it is.
	defer func() {
		if ctx.Err() == nil {
			m.forget(run)
			pull.Stop()
			return
		}
		// The pull is detached before anything is naked. A cancelled pull still has a pull
		// request standing at the server, so a nak sent while that pull is subscribed can be
		// delivered straight back into the pull this run is giving up, where nothing reads it
		// and it is lost again until its ack wait passes; detached, the pull has no interest
		// left and the frames reach the session's next owner. The reviewer measured the other
		// order at 62cc8055, over ten runs holding 200 frames with the next owner's pull already
		// open: six delivered messages out of stream order and four delivered one of them a
		// third time (TestSessionConsumerRelayLosingItsSessionReturnsEveryMessageItHoldsInStreamOrder
		// holds the fix). This call discards the detach's own error rather than dropping it:
		// Detach answers every caller the one result, and the Release below joins that same
		// result into what is reported.
		_ = pull.Detach()
		// Every frame the run published was read from the pull's queue before anything still in
		// it, so the frames are naked first and the queue after, each in the order it was sent or
		// queued.
		returned, err := m.release(run)
		fromPull, pullErr := pull.Release()
		returned += fromPull
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
			m.logger.Warn("session relay stopped; it could not give the server back every message it held, and each one it did not is delivered again only after the consumer's ack wait",
				slog.String("session_id", sessionID),
				slog.String("relay_machine_id", m.machineID),
				slog.String("reason", relayCancelledFor(ctx).String()),
				slog.Int("returned", returned),
				slog.String("error", err.Error()))
			return
		}
		m.logger.Info("session relay stopped; it gave the server back the messages it held, so they are delivered again at once rather than after the consumer's ack wait",
			slog.String("session_id", sessionID),
			slog.String("relay_machine_id", m.machineID),
			slog.String("reason", relayCancelledFor(ctx).String()),
			slog.Int("returned", returned))
	}()
	if start.StreamID == "" {
		m.logger.Warn("session relay could not read the stream's identity; retrying", slog.String("session_id", sessionID), slog.String("error", bus.ErrNoStreamID.Error()))
		return relayRetry
	}
	streamID, gap := start.StreamID, start.Gap
	for {
		next := ctx
		cancelNext := context.CancelFunc(func() {})
		if deadline, waiting := m.receiptDeadline(run); waiting {
			if !time.Now().Before(deadline) {
				return relayPaused
			}
			next, cancelNext = context.WithDeadline(ctx, deadline)
		}
		msg, err := pull.Next(next)
		cancelNext()
		if err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			m.logger.Warn("session relay pull ended; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		if err := m.publishFrame(ctx, run, msg, streamID, gap); err != nil {
			if ctx.Err() != nil {
				return relayEnded
			}
			m.logger.Warn("session relay could not publish a frame; restarting at the ack floor", slog.String("session_id", sessionID), slog.String("error", err.Error()))
			return relayRetry
		}
		gap = false
	}
}

// publishFrame publishes msg as a frame on the session's relay subject, its reply subject a token
// of its own under the listener's reply inbox.
func (m *sessionRelays) publishFrame(ctx context.Context, run *relayRun, msg bus.SessionMessage, streamID string, gap bool) error {
	token := strconv.FormatUint(m.nextToken.Add(1), 36)
	frame := &relayFrame{run: run, acker: msg.SessionAcker, sentAt: time.Now()}
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
	}
	return m.delivery.consumers.Publish(ctx, &nats.Msg{
		Subject: bus.DeliverSubject(run.sessionID),
		Reply:   m.replyPrefix + token,
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

// forget drops every frame of run still awaiting an answer and returns them by their tokens: a
// reply to one is ignored from here on. It answers nothing to the server, so each dropped frame's
// message is delivered again only once its ack wait has passed, or at the next restart at the ack
// floor. That is right for a run that ended for a reason of its own, and wrong for one that was
// cancelled, which calls release instead.
//
// It hands back the run's own map rather than a copy of it, since the map is the run's no longer:
// the common caller throws the result away, and framesMu, the one lock every session's replies
// wait on, is held over no allocation.
func (m *sessionRelays) forget(run *relayRun) map[string]*relayFrame {
	m.framesMu.Lock()
	defer m.framesMu.Unlock()
	dropped := run.outstanding
	for token := range dropped {
		delete(m.frames, token)
	}
	run.outstanding = nil
	run.unanswered = nil
	return dropped
}

// release forgets run and returns the message of every frame it dropped to the server, so the
// session's next owner is delivered them at once rather than after their ack wait
// (bus.SessionConsumerAckWait). A nak is safe to repeat as far as the server goes, since the
// session consumer sets MaxDeliver = -1 (internal/bus/session_consumers.go:471): a message
// returned here spends no server redelivery budget. Each delivery still raises the message's
// delivery count, which the session's client counts toward the attempts it allows a frame its
// host fails (RELAY_ATTEMPTS_BEFORE_FAILED, packages/envoy-client/src/consumer-delivery.ts). It
// returns how many messages it returned, and the naks that failed as one error, so that the
// run's one hand-over line can say whether the hand-over was whole rather than claiming it was.
func (m *sessionRelays) release(run *relayRun) (int, error) {
	dropped := slices.Collect(maps.Values(m.forget(run)))
	// The frames are naked in the order they were sent, which is stream order except for a
	// message the session naked and the relay sent again. No nak is sent under framesMu: it is a
	// publish, and the lock answers every session's replies.
	slices.SortFunc(dropped, func(a, b *relayFrame) int { return a.sentAt.Compare(b.sentAt) })
	returned, failed := 0, 0
	var firstErr error
	for _, frame := range dropped {
		if err := frame.acker.NakWithDelay(0); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		returned++
	}
	if failed > 0 {
		// A nak fails because the connection is closed or its reconnect buffer is full, which is
		// the shutdown burst where every relay releases at once, so the failures of one run are
		// one error naming the first of them rather than up to MaxAckPending copies of it.
		return returned, fmt.Errorf("could not return %d of the session's frames to the server; the first failed with: %w", failed, firstErr)
	}
	return returned, nil
}

// onReply hears a session's reply to a frame: received marks the frame answered, progress restarts
// its ack wait, done acks it and nak has the server deliver it again after relayNakDelay; done and
// nak end the frame. Any of them is the session answering, which puts the run's pause backoff back
// to its first wait. A reply to a token no frame awaits, or with any other body, is ignored.
func (m *sessionRelays) onReply(msg *nats.Msg) {
	token, ok := strings.CutPrefix(msg.Subject, m.replyPrefix)
	if !ok {
		return
	}
	reply := string(bytes.TrimSpace(msg.Data))
	m.framesMu.Lock()
	frame := m.frames[token]
	if frame == nil {
		m.framesMu.Unlock()
		return
	}
	switch reply {
	case relayReplyReceived, relayReplyProgress:
		frame.answered = true
	case relayReplyDone, relayReplyNak:
		frame.answered = true
		delete(m.frames, token)
		delete(frame.run.outstanding, token)
	default:
		m.framesMu.Unlock()
		return
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
		err = frame.acker.NakWithDelay(relayNakDelay)
	}
	if err != nil {
		m.logger.Warn("session relay could not answer the consumer for a frame", slog.String("session_id", frame.run.sessionID), slog.String("reply", reply), slog.String("error", err.Error()))
	}
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
