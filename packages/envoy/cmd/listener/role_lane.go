package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/config"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
)

// roleForwardWaitLimit bounds the role forwards the role lane holds at once, queued behind their
// holder's earlier forwards or waiting for their outcome. When it is reached the role lane's
// handler waits for one to end, which only holds role messages in the lane's subscription, whose
// pending limits are far above any backlog that wait can build (bus.ConnectRoleLane).
const roleForwardWaitLimit = 4096

// roleLaneSlowConsumerInterval is how often the role lane's slow-consumer count is read for a change
// to log (watchRoleLaneSlowConsumers).
const roleLaneSlowConsumerInterval = 5 * time.Second

// roleForward is one role message the role lane resolved to its holder, every guard passed: item is
// the message as it arrived and sessionID its holder. resolvedAt is when the resolution that chose
// that holder started, on the listener's clock, which the forward is judged by when its publish
// starts (roleForwarder.publish, roles.ForwardFenceAge); the kept lane's walk, which reads the
// role's claim before each copy, leaves it zero.
type roleForward struct {
	item       contracts.Envelope
	sessionID  string
	resolvedAt time.Time
}

// roleForwarder runs the role lane's forwards off its NATS handler, so a forward the stream is slow
// to acknowledge holds up no other role message. Forwards to one holder are published in the order
// the handler handed them over, by a worker per holder with forwards queued; each forward's wait
// for its outcome runs on a goroutine of its own, and the outcome becomes the forward's delivery
// status and exception as it always has (finishRoleForward). Forwards queued and waiting together
// are bounded by roleForwardWaitLimit.
type roleForwarder struct {
	// cfg is the delivery configuration the forwards are published (forwardRole) and reported
	// through.
	cfg   listenerDeliveryHandlerConfig
	slots chan struct{}
	// pending counts the forwards handed over and not yet reported, for wait.
	pending sync.WaitGroup

	mu sync.Mutex
	// queues holds each holder's forwards not yet published, in arrival order. A holder has a key
	// exactly while its worker runs.
	queues map[string][]roleForward
}

func newRoleForwarder(cfg listenerDeliveryHandlerConfig) *roleForwarder {
	return &roleForwarder{cfg: cfg, slots: make(chan struct{}, roleForwardWaitLimit), queues: map[string][]roleForward{}}
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
// has none, once a slot is free.
func (f *roleForwarder) enqueue(forward roleForward) {
	f.slots <- struct{}{}
	f.pending.Add(1)
	f.mu.Lock()
	queue, running := f.queues[forward.sessionID]
	f.queues[forward.sessionID] = append(queue, forward)
	f.mu.Unlock()
	if !running {
		go f.publish(forward.sessionID)
	}
}

// publish is sessionID's worker: it publishes the holder's queued forwards in order, hands each
// one's wait to a goroutine of its own, and ends once the queue is empty. A forward whose
// resolution is roles.ForwardFenceAge old or older when its publish starts - one that waited that
// long behind its holder's earlier forwards - resolves the role's holder again first
// (refreshStaleRoleForward) and goes to the holder that resolution finds, or is kept or refused as
// the lane keeps or refuses a message on that resolution: a takeover's copy reads the previous
// holder's subject for its record's life, and a forward started on a fresher resolution is stored
// where that copy reads it.
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

		if roles.ResolutionTooOld(forward.resolvedAt, f.cfg.now()) {
			refreshed, ok := refreshStaleRoleForward(f.cfg, forward)
			if !ok {
				f.release()
				continue
			}
			forward = refreshed
		}
		subject := f.cfg.roleForwardSubject(forward)
		wait, err := f.cfg.forwardRole(subject, forward.forwarded(), forward.msgID(subject), roleForwardTimeout)
		if err != nil {
			f.finish(forward, err)
			continue
		}
		go func() { f.finish(forward, wait()) }()
	}
}

// refreshStaleRoleForward resolves the holder of forward's role again, for a forward whose
// resolution was roles.ForwardFenceAge old when its publish started, by the rules the lane's own
// delivery resolves and judges by (resolveRoleForward), and reports the forward to make and
// whether to make one. A role with no live holder now has the message kept as the lane keeps it,
// and a holder beforeForward refuses has it refused as the lane refuses it, each with the lane's
// own outcome, and no forward is made.
func refreshStaleRoleForward(cfg listenerDeliveryHandlerConfig, forward roleForward) (roleForward, bool) {
	item := forward.item
	cfg.attemptCache.Clear(item.DedupeKey, forward.sessionID)
	cfg.logger.LogEvent(slog.LevelInfo, "listener role forward resolves its role's holder again: its resolution is too old to forward on",
		slog.String("event_id", item.EventID),
		slog.String("session_id", forward.sessionID),
		slog.String("topic", item.Topic),
		slog.Duration("age", cfg.now().Sub(forward.resolvedAt)))
	return resolveRoleForward(cfg, item)
}

// release frees the slot of a forward that ends with no publish and no outcome of its own to
// report: one whose fresh resolution kept or refused the message and reported that
// (refreshStaleRoleForward).
func (f *roleForwarder) release() {
	<-f.slots
	f.pending.Done()
}

// finish reports forward's outcome and frees its slot.
func (f *roleForwarder) finish(forward roleForward, err error) {
	finishRoleForward(f.cfg, forward, err)
	f.release()
}

// outstanding is how many forwards are queued or waiting for their outcome.
func (f *roleForwarder) outstanding() int {
	return len(f.slots)
}

// wait waits, within ctx, for every forward handed over to be reported. The shutdown runs it once
// the role lane has drained, so no forward is handed over meanwhile, and before the main client
// drains, since the forwards and their exceptions are published through it. It reports whether all
// were.
func (f *roleForwarder) wait(ctx context.Context) bool {
	return waitWithin(ctx, &f.pending)
}

// roleForwardSubject is the holder's agent subject a forward is published on: in the required
// privacy mode the one under the role message's project (contracts.AgentSendTopic), so the relay
// decides the forward as that project's frame; in the all-public mode the form it always had.
func (cfg listenerDeliveryHandlerConfig) roleForwardSubject(forward roleForward) string {
	if !cfg.privacy.isRequired() {
		return contracts.LegacyAgentSubject(forward.sessionID)
	}
	project, _ := contracts.SubjectProject(forward.item.Topic)
	return contracts.AgentSendTopic(project, forward.sessionID)
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
// forwards its handler hands over, and stopWatch, which ends watchRoleLaneSlowConsumers. An edge
// listener subscribes no role lane, so its roleLane holds the forwards alone.
type roleLane struct {
	client    *bus.Client
	forwards  *roleForwarder
	stopWatch func()
}

// startRoleLane opens the listener's role lane: a core-NATS queue subscription on a connection of
// its own with finite pending limits (bus.RoleLanePendingMsgs, bus.RoleLanePendingBytes;
// bus.ConnectRoleLane), in the queue group every listener shares, or this machine's group on a hub
// whose grant predates it, whose messages handler takes. During a rolling deploy the task being
// replaced is a member too, and NATS hands each role message to one member of a group, so opening
// the lane before the durable binds forwards no message twice. It counts the lane's slow-consumer
// errors on met and watches them (watchRoleLaneSlowConsumers) until the lane's drain.
//
// An edge listener (edge) opens none: its grant subscribes no role lane (natsgrant.Edge), so it
// takes no share of any project's role messages. A shared listener resolves a role's holder, an
// edge session's among them, and forwards the message to the holder's agent subject, which the
// holder's session consumer delivers. Its lane holds the forwards alone. extra are the options of
// the listener's privacy mode (listenerPrivacy.busOptions), which the lane's connection takes.
func startRoleLane(cfg config.Service, edge *edgeGrant, handler nats.MsgHandler, forwards *roleForwarder, met *metrics.Registry, logger *logging.Logger, extra ...bus.ConnectOption) (roleLane, error) {
	if edge != nil {
		logger.Info("role lane not subscribed: an edge listener takes no role message; a shared listener forwards each to its holder's agent subject")
		return roleLane{forwards: forwards}, nil
	}
	client, queue, err := bus.ConnectRoleLane(cfg.NATSURLs, cfg.MachineID, handler, extra...)
	if err != nil {
		return roleLane{}, err
	}
	logger.Info("role lane subscribed", slog.String("subject", contracts.RoleTopicPrefix+">"), slog.String("queue", queue))
	met.NewGaugeFunc("envoy_role_lane_slow_consumers", "Slow-consumer errors the role lane's connection has reported", func() int64 {
		return int64(client.SlowConsumers())
	})
	watchCtx, stopWatch := context.WithCancel(context.Background())
	go watchRoleLaneSlowConsumers(watchCtx, client, logger, roleLaneSlowConsumerInterval)
	return roleLane{client: client, forwards: forwards, stopWatch: stopWatch}, nil
}

// drain ends the role lane at shutdown: it drains the lane's client within timeout, so the handler
// takes every role message the server already delivered and no more, then waits for the forwards it
// handed over, the forward window after that at most, since a forward published by then has its
// outcome within it. A forward still waiting then is logged and left to the main client's drain. A
// lane with no client, an edge listener's, waits for the forwards alone.
func (l roleLane) drain(logger *logging.Logger, timeout time.Duration) {
	if l.stopWatch != nil {
		l.stopWatch()
	}
	if l.client != nil {
		if err := l.client.Drain(timeout); err != nil {
			logger.Warn("role lane drain error", slog.String("error", err.Error()))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), roleForwardTimeout)
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
