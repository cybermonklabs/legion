package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
// does.
type legionHandoffTarget struct {
	envelope    contracts.Envelope
	session     string
	roleToken   string
	stampSender bool
	optedOut    bool
}

// legionHandoff hands a stopped Legion agent's message to its daemon. A nil one hands nothing off.
type legionHandoff struct {
	client   *bus.Client
	registry *store.Registry
	sessions *session.SessionRegistry
	agents   *legionAgentRegistry
	logger   *logging.Logger
}

// legionHandedOff is a hand-off handOffToLegion published: the body, and whether the stream
// already held a copy under its MsgId (bus.Client.PublishRetained), so it stored nothing new. A
// send repeated under one idempotency key, a role publish repeated under one key, and every
// listener's copy of one bus message after the first all find one.
type legionHandedOff struct {
	listenerwire.LegionHandoff
	duplicate bool
}

// handOffToLegion publishes target to its agent's daemon in place of the answer a message no
// session took gets, when all of these hold:
//
//   - the source is not envoy, whose messages (the broker's wake sends, the listener's own
//     exceptions) are not questions, and the body did not opt out with "handoff": false;
//   - the Legion agent registry names the session: the session addressed, or the role token's
//     newest session, and for a role token that session's entry is finished - its issue has left
//     the workflow, so no next holder of the role is coming (answeringAgent). A role whose issue
//     still runs keeps its message for that next holder instead;
//   - that session's entry is not released: a claim suspended by idle release is resumed by the
//     wake its message raises, so the message stays the agent's (legionagents.Entry.Released);
//   - that session's project last said it answers (project.<legion_project>, legionagents.Project).
//     Only the flag counts, never its age: a daemon that stated answering: true and then stopped
//     still gets its agents' messages, which JetStream holds until it reads them (spec v21,
//     section 2.12 of dispatch://INFRA-204's plan). A project that stated answering: false, or
//     never stated anything, keeps the refusal it had before hand-off;
//   - envoy_sessions holds no record of the session at all, or one past the gone horizon
//     (notGone, read through this listener's cache to the bucket). A session that is live, and one
//     past the live window but still inside the gone horizon, is not handed off: the first is
//     answering now, perhaps on another listener, and the second keeps its claims and its stored
//     messages and may answer in a moment, so the message stays the agent's rather than being
//     answered in its name from a conversation it has moved past.
//
// It answers the hand-off it published (listenerwire.LegionHandoff, the body the daemon's hand-off
// reader decodes) with the stream's verdict on it, or nil when one of those does not hold, in
// which case the caller answers as it did before hand-off. A registry it cannot read is logged and
// answered nil the same way. The error is a publish that failed. The publish carries a MsgId built
// from the envelope's dedupe key, so the copies every listener's durable hands off for one bus
// message, and a send repeated under one idempotency key, are stored once, and every copy after
// the first is answered duplicate.
func (h *legionHandoff) handOffToLegion(ctx context.Context, target legionHandoffTarget) (*legionHandedOff, error) {
	if h == nil || target.optedOut || target.envelope.Source == "envoy" {
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
	_, _, err = liveSessionEntry(ctx, h.sessions, agent.SessionID, roleHolderReadTimeout, notGone)
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
	handoff.duplicate, err = h.client.PublishRetained(ctx, subject, data, "legion.handoff."+handoff.Envelope.DedupeKey+":"+subject)
	if err != nil {
		return nil, fmt.Errorf("publish legion hand-off to %s: %w", subject, err)
	}
	h.logger.Info("listener handed a message to its legion daemon",
		slog.String("session_id", agent.SessionID), slog.String("topic", handoff.OriginalTopic), slog.String("event_id", handoff.Envelope.EventID), slog.String("subject", subject),
		slog.Bool("duplicate", handoff.duplicate))
	return &handoff, nil
}

// answeringAgent is the registry's entry for target whose project last said it answers, nil when
// the registry names no session for it (a role token that is not a Legion workflow role's, or a
// session id the registry would not hold, included), when its project said otherwise, and while
// the registry's bucket is not open, which the routes answer 503 and this answers as it answers a
// registry naming no session.
//
// A role token is answered for only while its newest entry is finished: the role's issue has left
// the workflow, so no later holder of that role is coming (legionagents.Entry.Finished). A role
// whose issue still runs is nobody's to answer for - the gap between two phase workers of one
// issue, and a pod being replaced, both leave the role unclaimed while the registry still names
// the worker that stopped - so its message is kept on the role's kept lane for the next holder
// (INFRA-514's 202, roleSend.noHolder), which is what it was before the hand-off existed. A
// session addressed by id is answered for whether its issue is finished or not: that session has
// stopped, and the message is for it. A send that carries a role's mark (Dispatch's send to a
// `role:` route) is held to that role's claim before it reaches this, and one whose claim does not
// name its target is offered here on its role token only while no session holds the role live, so
// it is answered for by this rule rather than the session's (handOffOrKeepRoleSend): a finished
// role's message reaches its daemon, and a running role's is kept for its next holder (#4387's
// keep). A role a session holds live is that holder's, finished or not - a session that took the
// role over among them - so its message is kept for that holder and never offered here.
//
// Neither path answers for an entry that is released (legionagents.Entry.Released): the daemon
// released that session's idle issue pod and keeps its claim to resume, so the message is a wake
// for that very session, not a question its daemon answers in its name. Naming no agent leaves
// each caller where it was before the hand-off existed: the send route's 404 beside the wake it
// reported first, the give-up's no_holder notice and its wake, and a role's message kept on its
// kept lane for the resumed worker.
func (h *legionHandoff) answeringAgent(ctx context.Context, target legionHandoffTarget) (*legionagents.Entry, error) {
	agents := h.agents.store()
	if agents == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, legionagents.ReadTimeout)
	defer cancel()
	var entry legionagents.Entry
	if target.roleToken != "" {
		if legionagents.ValidateRoleToken(target.roleToken) != nil {
			return nil, nil
		}
		held, err := agents.ByRoleToken(ctx, target.roleToken)
		if err != nil {
			return nil, err
		}
		if len(held) == 0 || !held[0].Finished {
			return nil, nil
		}
		entry = held[0].LegionAgentEntry
	} else {
		agent, err := agents.Get(ctx, target.session)
		if errors.Is(err, legionagents.ErrNotFound) || errors.Is(err, legionagents.ErrInvalid) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		entry = agent.LegionAgentEntry
	}
	if entry.Released {
		return nil, nil
	}
	record, found, err := agents.Project(ctx, entry.LegionProject)
	if err != nil {
		return nil, err
	}
	if !found || !record.Answering {
		return nil, nil
	}
	return &entry, nil
}

// offerAgentDeliveryToLegion offers one agent-subject delivery this listener found no session
// record for to the stopped Legion agent's daemon, and reports whether that offer was the
// message's whole outcome. The unplaceable path's give-up reaches it once its deferrals are spent
// (unplaceableAgentDelivery), and so does a session whose interest row names this listener and
// outlives the session entry it was written beside, which is the shape every Legion agent that
// ran here leaves behind: session.HandleAgentMessage answers its gone record as it answers a
// session no interest names. It records the outcome and finalizes the message itself for the two
// answers it owns, so one line, one counter and one disposal serve the give-up:
//
//   - a hand-off that was published is the whole answer: handed_off, the dedupe key recorded and
//     the message ACKed, with no exception - the daemon answers it from the agent's own
//     conversation. Its line says whether the stream already held that hand-off (duplicate),
//     as another listener's durable that handed off the same message first leaves it;
//   - a hand-off whose publish failed is NAKed and tried again rather than answered, since the
//     give-up's own answer would publish the very notice the hand-off replaces. Its attempt-cache
//     entry is cleared with it, or the redelivery this NAK asks for is swallowed as an
//     attempt-dedupe skip before the handler runs again.
//
// It answers false when nothing was handed off - no registry entry for the session, a project
// that does not answer, a session that is not gone, a message that opted out, a registry that
// could not be read (handOffToLegion) - having recorded nothing and finalized nothing, and the
// caller answers exactly as it did before the hand-off existed.
func offerAgentDeliveryToLegion(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) bool {
	handed, err := cfg.handoff.handOffToLegion(context.Background(), legionHandoffTarget{envelope: item, session: sessionID})
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
					slog.String("project", handed.Project), slog.Bool("duplicate", handed.duplicate))
			},
			recordDedupe: true,
		})
		message.finalize(false)
	default:
		return false
	}
	return true
}
