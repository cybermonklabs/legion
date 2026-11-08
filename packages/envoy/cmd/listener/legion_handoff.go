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
)

// legionHandoffPrefix and legionHandoffAgents frame the subject a hand-off is published on,
// notifications.legion.<legion_project>.agents.<session> (legionAgentHandoffSubject in
// packages/contracts, intake.HandoffSubject in packages/daemon).
const (
	legionHandoffPrefix = bus.NotificationTopicPrefix + "legion."
	legionHandoffAgents = ".agents."
)

func legionHandoffSubject(project, sessionID string) string {
	return legionHandoffPrefix + project + legionHandoffAgents + sessionID
}

// isLegionHandoffSubject reports whether subject is a hand-off's: notifications.legion.<project>.
// agents.<session>. The notification stream retains it for the daemon's consumer, so it reaches
// every listener's durable too, which holds no envelope to deliver there.
func isLegionHandoffSubject(subject string) bool {
	rest, ok := strings.CutPrefix(subject, legionHandoffPrefix)
	project, sessionID, agents := strings.Cut(rest, legionHandoffAgents)
	return ok && agents && project != "" && sessionID != "" && !strings.Contains(project, ".") && !strings.Contains(sessionID, ".")
}

// legionHandoffMessage is what a hand-off publishes: the topic the message was for, the agent
// session it is for, that session's role token and Legion project token, and the envelope exactly as
// it would have been delivered to the agent. It is LegionAgentHandoffSchema
// (packages/contracts/src/legion-handoff.ts), which the daemon decodes as intake.Handoff.
type legionHandoffMessage struct {
	OriginalTopic string             `json:"original_topic"`
	TargetSession string             `json:"target_session"`
	RoleToken     string             `json:"role_token"`
	Project       string             `json:"project"`
	Envelope      contracts.Envelope `json:"envelope"`
}

// legionHandoffTarget is one message no session took, which handOffToLegion may hand off: its
// envelope, and whom it is for, a session (a send, or an agent subject on the bus) or a role token
// (a role publish). stampSender is a send, whose envelope gets its sender stamp (senderStamp) only
// when it is handed off, as it would have before its delivery; a role publish stamps the sender
// before it resolves the holder (sendToRole) and a bus message already carries the stamp its
// publishing listener gave it. optedOut is a publish or send body that carried "handoff": false,
// as every one the daemon makes does.
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

// handOffToLegion publishes target to its agent's daemon in place of the answer a message no
// session took gets, when all of these hold:
//
//   - the source is not envoy, whose messages (the broker's wake sends, the listener's own
//     exceptions) are not questions, and the body did not opt out with "handoff": false;
//   - the Legion agent registry names the session: the session addressed, or the role token's
//     newest session;
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
// It answers the hand-off it published, or nil when one of those does not hold, in which case the
// caller answers as it did before hand-off. A registry it cannot read is logged and answered nil
// the same way. The error is a publish that failed. The publish carries a MsgId built from the
// envelope's dedupe key, so the copies every listener's durable hands off for one bus message, and
// a send repeated under one idempotency key, are stored once.
func (h *legionHandoff) handOffToLegion(ctx context.Context, target legionHandoffTarget) (*legionHandoffMessage, error) {
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
	handoff := legionHandoffMessage{
		OriginalTopic: target.envelope.Topic,
		TargetSession: agent.SessionID,
		RoleToken:     agent.RoleToken,
		Project:       agent.LegionProject,
		Envelope:      target.envelope,
	}
	if target.stampSender {
		handoff.Envelope.Sender = senderStamp(ctx, h.registry, h.sessions, handoff.Envelope.SourceSession)
	}
	data, err := json.Marshal(handoff)
	if err != nil {
		return nil, fmt.Errorf("encode legion hand-off: %w", err)
	}
	subject := legionHandoffSubject(agent.LegionProject, agent.SessionID)
	if _, err := h.client.PublishRetained(ctx, subject, data, "legion.handoff."+handoff.Envelope.DedupeKey+":"+subject); err != nil {
		return nil, fmt.Errorf("publish legion hand-off to %s: %w", subject, err)
	}
	h.logger.Info("listener handed a message to its legion daemon",
		slog.String("session_id", agent.SessionID), slog.String("topic", handoff.OriginalTopic), slog.String("event_id", handoff.Envelope.EventID), slog.String("subject", subject))
	return &handoff, nil
}

// answeringAgent is the registry's entry for target whose project last said it answers, nil when
// the registry names no session for it (a role token that is not a Legion workflow role's, or a
// session id the registry would not hold, included), when its project said otherwise, and while
// the registry's bucket is not open, which the routes answer 503 and this answers as it answers a
// registry naming no session.
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
		if len(held) == 0 {
			return nil, nil
		}
		entry = held[0].Entry
	} else {
		agent, err := agents.Get(ctx, target.session)
		if errors.Is(err, legionagents.ErrNotFound) || errors.Is(err, legionagents.ErrInvalid) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		entry = agent.Entry
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
