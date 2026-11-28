package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/legionagents"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// legionHandoffPrefix and legionHandoffAgents frame the subject isLegionHandoffSubject reads, the
// one listenerwire.LegionHandoffSubject builds: notifications.legion.<legion_project>.agents.
// <session>.
const (
	legionHandoffPrefix = bus.NotificationTopicPrefix + "legion."
	legionHandoffAgents = ".agents."
)

// isLegionHandoffSubject reports whether subject is a hand-off's: notifications.legion.<project>.
// agents.<session>. The notification stream retains it for the daemon's consumer, so it reaches
// every listener's durable too, which holds no envelope to deliver there.
func isLegionHandoffSubject(subject string) bool {
	rest, ok := strings.CutPrefix(subject, legionHandoffPrefix)
	project, sessionID, agents := strings.Cut(rest, legionHandoffAgents)
	return ok && agents && project != "" && sessionID != "" && !strings.Contains(project, ".") && !strings.Contains(sessionID, ".")
}

// legionHandoffTarget is one message no session took, which handOffToLegion may hand off: its
// envelope, and whom it is for, a session (a send, or an agent subject on the bus) or a role token
// (a role publish, or a send marked with a role whose claim does not name its target). stampSender
// is a send, whose envelope gets its sender stamp (senderStamp) only when it is handed off, as it
// would have before its delivery; a role publish stamps the sender before it resolves the holder
// (sendToRole) and a bus message already carries the stamp its publishing listener gave it.
// optedOut is a publish or send body that carried "handoff": false, as every one the daemon makes
// does. confirmMiss, which only a role token's target carries, is its caller's last check before
// the registry is scanned for the role's newest session (answeringAgent): it is asked once the
// role's project is found to answer, and a false answer hands nothing off. The publish route's
// role send sets it to its confirmation, from the buckets' stream leaders, that the role still has
// no live holder (roleSend.missConfirmed), so a role whose project does not answer costs no leader
// read.
type legionHandoffTarget struct {
	envelope    contracts.Envelope
	session     string
	roleToken   string
	stampSender bool
	optedOut    bool
	confirmMiss func() bool
}

// legionHandoff hands a stopped Legion agent's message to its daemon. A nil one hands nothing off.
type legionHandoff struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	agents   *legionAgentRegistry
	logger   *logging.Logger
}

// legionHandedOff is a hand-off handOffToLegion published: the body, the subject it was published
// on, and the stream's verdict on it (bus.Client.PublishRetained): whether the stream already held
// a copy under its MsgId, so it stored nothing new, the sequence of the copy it holds, and whether
// that verdict answers a resend, which may name this very hand-off's own copy
// (bus.StoredAck.Resent). A send repeated under one idempotency key, a role publish repeated under
// one key, and every listener's copy of one bus message after the first all find one. A request
// a hand-off answers is answered the copy the stream holds (answerHandedOff), and the route that
// made it marks its sender's charge by the verdict, as a publish's own verdict marks it
// (senderCharge.published).
type legionHandedOff struct {
	listenerwire.LegionHandoff
	subject string
	ack     bus.StoredAck
}

// handOffToLegion publishes target to its agent's daemon in place of the answer a message no
// session took gets, when all of these hold:
//
//   - the source is not envoy, whose messages (the broker's wake sends, the listener's own
//     exceptions) are not questions, and the body did not opt out with "handoff": false;
//   - the Legion agent registry names the session: the session addressed, or the role token's
//     newest session, whether that entry's issue has left the workflow or not (answeringAgent). A
//     role the registry holds no entry for keeps its message for the role's first holder instead;
//   - that session's project last said it answers (project.<legion_project>, legionagents.Project,
//     legionProjectAnswers). Only the flag counts, never its age: a daemon that stated answering:
//     true and then stopped still gets its agents' messages, which JetStream holds until it reads
//     them (spec v21, section 2.12 of dispatch://INFRA-204's plan). A project that stated
//     answering: false, or never stated anything, keeps the refusal it had before hand-off. A role
//     token names its project, whose flag is read before anything else (answeringAgent);
//   - for a role token whose caller confirms the role's miss (confirmMiss), that confirmation
//     holds;
//   - envoy_sessions holds no record of the session at all, or one past the gone horizon
//     (notGone, read through this listener's cache to the bucket). A session that is live, and one
//     past the live window but still inside the gone horizon, is not handed off: the first is
//     answering now, perhaps on another listener, and the second keeps its claims and its stored
//     messages and may answer in a moment, so the message stays the agent's rather than being
//     answered in its name from a conversation it has moved past.
//
// An agent whose issue pod the daemon released because it was idle is handed off as any other
// stopped agent is: its daemon decides what its message does, and whether it wakes the agent. So
// every route raises its wake report only when nothing was handed off (sendHandler,
// roleSend.noHolder, handOffOrKeepRoleSend, decideNoRecordAgentMessage): a wake raised beside a
// hand-off would resume a released agent before its daemon decided. An agent that is not released
// needs no wake report for a handed-off message either, since the daemon acts on one only for a
// claim of its own that is released (listenerWake in packages/daemon/internal/workflow).
//
// It answers the hand-off it published (listenerwire.LegionHandoff, the body the daemon's hand-off
// reader decodes) with the stream's verdict on it, or nil when one of those does not hold, in
// which case the caller answers as it did before hand-off. A registry it cannot read is logged and
// answered nil the same way. The error is a publish that failed. The publish carries a MsgId built
// from the envelope's dedupe key, so the copies every listener's durable hands off for one bus
// message, and a send repeated under one idempotency key, are stored once, and every copy after
// the first is answered duplicate. A copy that meets an earlier one still in process under that
// MsgId is published again until the stream names that copy, answered duplicate, or ctx ends
// (publishPastInProcess).
func (h *legionHandoff) handOffToLegion(ctx context.Context, target legionHandoffTarget) (*legionHandedOff, error) {
	if !h.mayHandOff(target) {
		return nil, nil
	}
	agent, err := h.answeringAgent(ctx, target)
	if err != nil {
		h.logger.Warn("listener legion hand-off could not read the registry; answering as without one",
			slog.String("topic", target.envelope.Topic), slog.String("event_id", target.envelope.EventID), slog.String("error", err.Error()))
		return nil, nil
	}
	if agent == nil {
		return nil, nil
	}
	// The hand-off takes the message away from the session on a miss, so its session read is the
	// bucket leader's, as every read that takes something from a session on a miss is
	// (liveSessionEntry): a copy that has not applied a registration made a moment ago would hand a
	// live session's message to its daemon.
	_, _, err = liveSessionEntry(ctx, h.sessions, agent.SessionID, roleHolderReadTimeout, notGone, h.sessions.RefreshFromLeader)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
	case err != nil:
		h.logger.Warn("listener legion hand-off could not read the session registry; answering as without one",
			slog.String("session_id", agent.SessionID), slog.String("topic", target.envelope.Topic), slog.String("error", err.Error()))
		return nil, nil
	default:
		return nil, nil
	}
	handoff := legionHandedOff{LegionHandoff: listenerwire.LegionHandoff{
		OriginalTopic: target.envelope.Topic,
		TargetSession: agent.SessionID,
		RoleToken:     agent.RoleToken,
		Project:       agent.LegionProject,
		Envelope:      target.envelope,
	}}
	if target.stampSender {
		handoff.Envelope.Sender = senderStamp(ctx, h.registry, h.sessions, handoff.Envelope.SourceSession)
	}
	data, err := json.Marshal(handoff.LegionHandoff)
	if err != nil {
		return nil, fmt.Errorf("encode legion hand-off: %w", err)
	}
	subject := listenerwire.LegionHandoffSubject(agent.LegionProject, agent.SessionID)
	msgID := "legion.handoff." + handoff.Envelope.DedupeKey + ":" + subject
	ack, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) {
		return h.client.PublishRetained(ctx, subject, data, msgID)
	})
	if err != nil {
		return nil, fmt.Errorf("publish legion hand-off to %s: %w", subject, err)
	}
	handoff.subject, handoff.ack = subject, ack
	h.logger.Info("listener handed a message to its legion daemon",
		slog.String("session_id", agent.SessionID), slog.String("topic", handoff.OriginalTopic), slog.String("event_id", handoff.Envelope.EventID), slog.String("subject", subject),
		slog.Bool("duplicate", handoff.ack.Duplicate))
	return &handoff, nil
}

// mayHandOff reports whether handOffToLegion can hand target off at all, before any read: this
// listener has a hand-off, the body did not opt out with "handoff": false, the source is not
// envoy, and a role token is a Legion workflow role's (legionagents.ValidateRoleToken), since the
// registry names no session for any other. handOffToLegion asks it first, and a read a caller pays
// for a hand-off alone is the target's confirmMiss, asked only once this holds and the role's
// project answers, so a message that can never be handed off, a plain Envoy role's among them,
// and one for a role whose project does not answer pay nothing for it (roleSend.noHolder).
func (h *legionHandoff) mayHandOff(target legionHandoffTarget) bool {
	return h != nil && !target.optedOut && target.envelope.Source != "envoy" &&
		(target.roleToken == "" || legionagents.ValidateRoleToken(target.roleToken) == nil)
}

// handOffForRequest is the hand-off of a message a request carries, as each route that answers a
// request makes it: the send route's for a session (sendHandler), the send route's for a send
// marked with a role (handOffOrKeepRoleSend) and the publish route's for a role
// (roleSend.noHolder). It hands target off (handOffToLegion), marks the request's charge
// (record.charge) by the stream's verdict on the hand-off's publish, a publish that failed
// included (senderCharge.published), and makes *item, the envelope target carries, the answer of
// the hand-off it published (answerHandedOff), reading a copy the stream already held through
// this listener's bus client. It answers the hand-off, nil with *item untouched when nothing was
// handed off, and duplicate as answerHandedOff answers it. The error is a hand-off whose publish
// failed, or whose copy could not be read back: each caller refuses on it as it refuses a publish
// of its own that failed, and otherwise builds its own answer.
func (h *legionHandoff) handOffForRequest(ctx context.Context, target legionHandoffTarget, record *requestRecord, sender requestSender, item *contracts.Envelope) (handed *legionHandedOff, duplicate bool, err error) {
	handed, err = h.handOffToLegion(ctx, target)
	if err != nil {
		record.charge.published(bus.StoredAck{}, err)
		return nil, false, err
	}
	if handed == nil {
		return nil, false, nil
	}
	record.charge.published(handed.ack, nil)
	if duplicate, err = answerHandedOff(ctx, h.client, record, sender, handed, item); err != nil {
		return nil, false, err
	}
	return handed, duplicate, nil
}

// handedOffSendResponse is the send route's 200 for a send handed to a stopped Legion agent's
// daemon (handOffForRequest), whether the send named the agent's session (sendHandler) or carried
// the mark of a role whose newest session the agent is (handOffOrKeepRoleSend), so both answer
// alike: item, the envelope the hand-off answered, its expects_reply in the dialect the request
// spoke (expectsReplyAnswer), the agent's session as recipient and recipient_state gone. A
// hand-off takes only a session with no entry within the gone horizon (handOffToLegion), which is
// the session listenerwire.SendResponse.RecipientState names gone. It carries no state and no
// last_seen: the message was stored for no session, and no session was probed.
func handedOffSendResponse(item contracts.Envelope, expectsReply json.RawMessage, handed *legionHandedOff, duplicate bool) listenerwire.SendResponse {
	return listenerwire.SendResponse{
		Envelope:       item,
		ExpectsReply:   expectsReplyAnswer(expectsReply, item.ExpectsReply),
		Recipient:      handed.TargetSession,
		RecipientState: session.Gone,
		Duplicate:      duplicate,
	}
}

// answerHandedOff makes *item, the envelope a route built for its request, the answer of handed,
// the hand-off handOffToLegion published for it, as publishHeld answers a send's own publish. A
// hand-off the stream stored as a new copy answers the envelope it handed off, with the sender's
// stamp a send gets at its hand-off, and duplicate false. A duplicate verdict names the hand-off
// the stream already holds under the same MsgId, at its sequence, and *item becomes the answer of
// the envelope that hand-off carries, read back (storedCopy through handoffCopies), as sender may
// read it (requestRecord.answerStored): whole for its own sender, and for anyone else the
// key-taken answer or the envelope handed off for this request. duplicate is whether that copy is
// another request's, which is when its event_id is not this request's, so a verdict that answers
// a resend with this request's own hand-off answers no duplicate. The copy is read on the
// hand-off's own subject, notifications.legion.<project>.agents.<session>, where the hand-off's
// MsgId stored it, never on the agent subject a send's own copy is read on: a message handed off
// is stored nowhere else.
//
// A copy that cannot be read is an error wrapping errStoredCopyUnread, with *item untouched, which
// each route answers as it answers a hand-off whose publish failed, here the 503 with Retry-After:
// 1 under storedCopyUnreadablePrefix. After a resend (bus.StoredAck.Resent) the copy may be this
// request's own, so that error is bus.ErrUnacknowledged too (mayBeOwnUnread), and the 503 says
// maybe_stored. Each route marks the sender's charge for the hand-off by the stream's verdict on
// its publish, as publishHeld marks a send's own (senderCharge.published).
func answerHandedOff(ctx context.Context, reader storedReader, record *requestRecord, sender requestSender, handed *legionHandedOff, item *contracts.Envelope) (duplicate bool, err error) {
	if !handed.ack.Duplicate {
		*item = handed.Envelope
		return false, nil
	}
	held, err := storedCopy(ctx, handoffCopies{reader}, handed.subject, handed.Envelope, handed.ack.Sequence)
	if err != nil {
		if handed.ack.Resent {
			return false, mayBeOwnUnread{err}
		}
		return false, err
	}
	*item = handed.Envelope
	return record.answerStored(item, held, sender), nil
}

// handoffCopies reads a message the stream holds as the envelope a hand-off carries
// (listenerwire.LegionHandoff's envelope field), so storedCopy reads the copy a duplicate verdict on
// a hand-off names under every check it makes of a send's copy. A message that does not decode as a
// hand-off reads as no envelope at all, which storedCopy refuses as a message that does not decode.
type handoffCopies struct{ storedReader }

func (r handoffCopies) NextStoredMessage(ctx context.Context, subject string, seq uint64) (bus.StoredMessage, error) {
	stored, err := r.storedReader.NextStoredMessage(ctx, subject, seq)
	if err != nil {
		return stored, err
	}
	var handoff struct {
		Envelope json.RawMessage `json:"envelope"`
	}
	_ = json.Unmarshal(stored.Data, &handoff)
	stored.Data = handoff.Envelope
	return stored, nil
}

// answeringAgent is the registry's entry for target whose project last said it answers, nil when
// the registry names no session for it (a session id the registry would not hold included), when
// its project said otherwise, and while the registry's bucket is not open, which the routes
// answer 503 and this answers as it answers a registry naming no session. A role token that is
// not a Legion workflow role's never reaches it (mayHandOff). Whether a project answers is
// legionProjectAnswers' rule, the one the registry's reads answer each entry's answering by.
//
// A role token names its project (legionagents.RoleTokenParts), the one every entry under it
// carries (legionagents.Validate), so that project's flag is read first: a role whose project does
// not answer costs neither a scan of the registry nor the reads its caller confirms the role's
// miss with (confirmMiss), which are asked next, before the scan. The flag's read and the scan
// are each bounded by legionagents.ReadTimeout, so the confirmation's reads take nothing from the
// scan's bound. A session addressed by id is read first, and its entry names the project whose
// flag is read after it, the two under one bound.
//
// A role token is answered for by its newest entry, whether that entry's issue has left the
// workflow or not: a worker that failed, or that the workflow, an operator or an idle release
// suspended, has stopped with the role unclaimed, and its daemon answers the role's message from
// that worker's conversation, as it does for a finished role's.
// A role the registry holds no entry for is nobody's to answer
// for, so its message is kept on the role's kept lane for the role's first holder (INFRA-514's
// 202, roleSend.noHolder), which is what it was before the hand-off existed. A session addressed
// by id is answered for whether its issue is finished or not: that session has stopped, and the
// message is for it. A send that carries a role's mark (Dispatch's send to a `role:` route) is
// held to that role's claim before it reaches this, and one whose claim does not name its target
// is offered here on its role token only while no session holds the role live, so it is answered
// for by this rule rather than the session's (handOffOrKeepRoleSend). A role a session holds live
// is that holder's - a session that took the role over among them - so its message is kept for
// that holder and never offered here.
func (h *legionHandoff) answeringAgent(ctx context.Context, target legionHandoffTarget) (*legionagents.Entry, error) {
	agents := h.agents.store()
	if agents == nil {
		return nil, nil
	}
	if target.roleToken != "" {
		project, _ := legionagents.RoleTokenParts(target.roleToken)
		readCtx, cancel := context.WithTimeout(ctx, legionagents.ReadTimeout)
		answers, err := legionProjectAnswers(readCtx, agents, project)
		cancel()
		if err != nil || !answers {
			return nil, err
		}
		if target.confirmMiss != nil && !target.confirmMiss() {
			return nil, nil
		}
		readCtx, cancel = context.WithTimeout(ctx, legionagents.ReadTimeout)
		defer cancel()
		held, err := agents.ByRoleToken(readCtx, target.roleToken)
		if err != nil || len(held) == 0 {
			return nil, err
		}
		return &held[0].LegionAgentEntry, nil
	}
	ctx, cancel := context.WithTimeout(ctx, legionagents.ReadTimeout)
	defer cancel()
	agent, err := agents.Get(ctx, target.session)
	if errors.Is(err, legionagents.ErrNotFound) || errors.Is(err, legionagents.ErrInvalid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if answers, err := legionProjectAnswers(ctx, agents, agent.LegionProject); err != nil || !answers {
		return nil, err
	}
	return &agent.LegionAgentEntry, nil
}

// legionHandoffGiveUpWindow is the bound main.go gives one hand-off the agent-subject delivery's
// decision makes (listenerDeliveryHandlerConfig.handoffGiveUpWindow): its registry reads and its
// publish, retries of an id still in process included. A publish takes milliseconds and each send
// is bounded by the bus's own publish bound, so a window well inside the consumer's ack wait
// (consumerAckWait) leaves the NAK room to ask for the redelivery before the server would send the
// message again by itself.
const legionHandoffGiveUpWindow = 10 * time.Second

// offerAgentDeliveryToLegion offers one agent-subject delivery this listener found no session
// record for to the stopped Legion agent's daemon, and reports whether that offer was the
// message's whole outcome. The decision on a message with no record reaches it
// (decideNoRecordAgentMessage): once the message's one wait is spent, at once when it has no wait
// to take, and at the stream's last delivery after a read of the record that did not answer. A
// session whose interest row names this listener and outlives the session entry it was written
// beside, which is the shape every Legion agent that ran here leaves behind, reaches it the same
// way: session.HandleAgentMessage answers its gone record AgentNoRecord, as it answers a session
// no interest names. It records the outcome and finalizes the message itself for the two answers
// it owns, so one line, one counter and one disposal serve the decision:
//
//   - a hand-off that was published is the whole answer: handed_off, the dedupe key recorded and
//     the message ACKed, with no exception - the daemon answers it from the agent's own
//     conversation. Its line says whether the stream already held that hand-off (duplicate),
//     as another listener's durable that handed off the same message first leaves it;
//   - a hand-off whose publish failed is logged at ERROR, counted failed and NAKed, with its
//     attempt-cache entry cleared, and reports no wake: the decision's own answer would report the
//     very wake the hand-off replaces. Below the stream's last delivery (consumerMaxDeliver) the
//     NAK brings the message back to be decided again, and the cleared entry keeps that redelivery
//     from being swallowed as an attempt-dedupe skip before the handler runs again. At the stream's
//     last delivery the NAK gets no redelivery, and a core delivery is never sent again
//     (deliveryMessage.finalize does nothing to one), so there the message ends with the ERROR
//     line alone.
//
// It answers false when nothing was handed off - no registry entry for the session, a project
// that does not answer, a session that is not gone, a message that opted out, a registry that
// could not be read (handOffToLegion) - having recorded nothing and finalized nothing, and the
// caller reports the session's wake (reportNoRecordWake).
//
// The decision answers no request, so the hand-off takes a context of its own,
// cfg.handoffWindow(), rather than none: its publish is made again for as long as a clustered
// stream refuses it as still in process under an earlier attempt (publishPastInProcess), and a
// refusal that outlives the window is a hand-off that failed, answered as any other is.
func offerAgentDeliveryToLegion(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.handoffWindow())
	defer cancel()
	handed, err := cfg.handoff.handOffToLegion(ctx, legionHandoffTarget{envelope: item, session: sessionID})
	switch {
	case err != nil:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener legion hand-off failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
			},
			clearAttempt: true,
		})
		cfg.messagesNAKed.Inc()
		message.finalize(true)
	case handed != nil:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "handed_off",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener agent handed off to its legion daemon", sessionID, item.Topic, item.EventID, "handed_off",
					slog.String("project", handed.Project), slog.Bool("duplicate", handed.ack.Duplicate))
			},
			recordDedupe: true,
		})
		message.finalize(false)
	default:
		return false
	}
	return true
}

// handoffWindow is the bound of one hand-off the agent-subject delivery's decision makes
// (offerAgentDeliveryToLegion): cfg.handoffGiveUpWindow, or legionHandoffGiveUpWindow for a
// configuration that sets none, so a zero window never ends a hand-off before its first read.
func (cfg listenerDeliveryHandlerConfig) handoffWindow() time.Duration {
	if cfg.handoffGiveUpWindow <= 0 {
		return legionHandoffGiveUpWindow
	}
	return cfg.handoffGiveUpWindow
}
