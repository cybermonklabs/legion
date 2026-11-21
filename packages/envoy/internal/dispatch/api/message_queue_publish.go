package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/sjawhar/envoy/internal/contracts"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// mayQueueForUnknownSession reports whether an attempt whose last try ended in failure is one
// Dispatch queues itself (queueForUnknownSession): a message to a session, whose attempt names
// that session, whose last try found no live session for it, which this process has a publisher
// for, and which does not ask for a session that is running now.
//
// "no live session" is the one answer that says neither the listener's registry nor its
// known-session store holds the session: the registry read's 404 (resolveDeliveryTarget) and the
// send's own 404 are that fact, so a person sees one outcome for it. A session either store holds
// is the listener's to queue, and it answers the send stopped (deliveryOutcome).
//
// queueForUnknownSession builds the subject from the attempt's session (attemptSessionID), and
// for a record the registry calls gone that is the id the listener answered rather than the one
// the message's target names (resolveDeliveryTarget). So the attempt's session must be the very
// id of the target's parsed session route, and an attempt naming any other session, or none, is
// not queued: a session route's id is never empty (model.ParseRoute), and the subject is always
// that of the session the validated route names, never one an answer put in its place.
//
// A role target keeps its failure: a role whose holder is gone names no session to queue for,
// and a role's message does not belong on the subject of a session that held the role once. A
// send that asks for a running session (connectedOnly, a model or thinking switch) keeps it too,
// since a switch stored for a session that is not running would apply at an unknown later time.
func (s *server) mayQueueForUnknownSession(
	message model.Message, pending pendingMessageDelivery, connectedOnly bool, failure string,
) bool {
	if s.deps.AgentPublisher == nil || connectedOnly || !strings.HasPrefix(failure, noLiveSessionPrefix) {
		return false
	}
	route, err := model.ParseRoute(messageTarget(message.Target))
	return err == nil && route.Kind == "session" && route.ID == pending.resolved.attemptSessionID
}

// queueForUnknownSession queues a targeted message for a session no store of the listener's holds:
// it publishes onto the session's own subject, in the form every reader takes
// (contracts.LegacyAgentSubject, through contracts.AgentSendEnvelope), the envelope the
// listener's send route would have stored for this attempt, under the same dedupe key, and
// answers as a send to a stopped session does - queued, with the envelope it stored, or with
// none and duplicate when the stream already held it.
//
// The listener's delivery of that copy finds no record of the session and gives it up after its
// retry window with the agent wake, which brings a released Legion pod back. The session reads the
// copy from its consumer when it registers again under its id, since that consumer starts at the
// first message stored on the subject that the session has not acknowledged. A repeat under the
// same key - the delivery sweep resuming an attempt whose sender died between this publish and its
// settle, or a Retry in the same mode - is the duplicate the stream reports, not a second copy.
//
// A publish that fails queues nothing: the attempt then settles failed with failure and the
// publish's own error, so the person can retry it.
func (s *server) queueForUnknownSession(
	ctx context.Context, message model.Message, pending pendingMessageDelivery, key string,
	wanted contracts.ExpectsReply, failure string,
) sendOutcome {
	envelope := queuedEnvelope(listenerSendInput(
		pending.resolved.attemptSessionID, pending.resolved.Delivery, message.Body, key,
		pending.urgency, wanted, pending.frame, false,
	))
	// The listener refuses to store an envelope that does not validate, and so does this.
	if err := envelope.Validate(); err != nil {
		return sendOutcome{failure: queueFailure(failure, err)}
	}
	duplicate, err := s.deps.AgentPublisher.PublishReportingDuplicateWithin(ctx, envelope)
	if err != nil {
		return sendOutcome{failure: queueFailure(failure, err)}
	}
	// A duplicate names an envelope JetStream discarded, so the attempt records none, as a send's
	// does.
	if duplicate {
		return sendOutcome{duplicate: true, queued: true}
	}
	return sendOutcome{envelopeID: &envelope.EventID, queued: true}
}

// queueFailure is what an attempt records when Dispatch could not queue its message: the
// listener's "no live session <id>" first, then why the publish failed.
func queueFailure(failure string, err error) string {
	return failure + "; Dispatch could not queue it for the session: " + err.Error()
}

// queuedEnvelope is the envelope the listener's send route stores for input: built with the
// builder that route uses (contracts.AgentSendEnvelope), on the session's own subject under
// AgentSendDedupeKey(session, idempotency key), from the message Client.Send writes its body from
// (dispatchenvoy.SendInput.EnvelopeMessage). A send that did reach the listener and the copy
// Dispatch queued are then one message under one stream id (bus.ForwardMsgID), which the stream
// stores once.
func queuedEnvelope(input dispatchenvoy.SendInput) contracts.Envelope {
	return contracts.AgentSendEnvelope(contracts.NoProject, input.TargetSession, input.IdempotencyKey, input.EnvelopeMessage())
}

// AgentPublishRecorder is the AgentPublisher of a test process that has no NATS: the browser
// suite runs Dispatch with DISPATCH_NATS_DISABLED=1 and its test hooks on, and reads what this
// process queued through GET /api/v1/messages/_test/queued. It stores an envelope once per subject
// and dedupe key and reports a repeat as a duplicate, as the stream does for an envelope it
// already holds. It records in this process's memory alone, so a test reads it from the process
// that queued the message.
type AgentPublishRecorder struct {
	mu        sync.Mutex
	envelopes []contracts.Envelope
}

// NewAgentPublishRecorder returns a recorder that holds nothing yet.
func NewAgentPublishRecorder() *AgentPublishRecorder {
	return &AgentPublishRecorder{}
}

// PublishReportingDuplicateWithin records envelope, or reports it a duplicate of one recorded on
// the same subject under the same dedupe key. A ctx that has ended records nothing, as a publish
// whose ctx has ended sends nothing.
func (r *AgentPublishRecorder) PublishReportingDuplicateWithin(ctx context.Context, envelope contracts.Envelope) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, stored := range r.envelopes {
		if stored.Topic == envelope.Topic && stored.DedupeKey == envelope.DedupeKey {
			return true, nil
		}
	}
	r.envelopes = append(r.envelopes, envelope)
	return false, nil
}

// Published answers every envelope recorded so far, oldest first.
func (r *AgentPublishRecorder) Published() []contracts.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.envelopes)
}

// queuedMessages is the body GET /api/v1/messages/_test/queued answers.
type queuedMessages struct {
	Queued []queuedMessage `json:"queued"`
}

// queuedMessage is one envelope this process queued, as the stream would hold it: the subject it
// is stored on and the fields a test compares with the message it sent.
type queuedMessage struct {
	Subject        string `json:"subject"`
	DedupeKey      string `json:"dedupe_key"`
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	PayloadSummary string `json:"payload_summary"`
	// Payload is the delivery frame the listener's send would have carried, as its JSON text.
	Payload string `json:"payload"`
	Urgency string `json:"urgency,omitempty"`
}

// listQueuedMessages answers {"queued": [...]}: every targeted message this process queued
// itself for a session no store of the listener's held, oldest first. It answers 404
// QUEUED_ON_BUS on a process that publishes them to the bus, where the stream is what holds them.
func (s *server) listQueuedMessages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	recorder, ok := s.deps.AgentPublisher.(*AgentPublishRecorder)
	if !ok {
		writeError(w, "QUEUED_ON_BUS", http.StatusNotFound,
			"this process queues targeted messages on the bus, not in a test hook's recorder")
		return
	}
	stored := recorder.Published()
	queued := make([]queuedMessage, len(stored))
	for index, envelope := range stored {
		queued[index] = queuedMessage{
			Subject:        envelope.Topic,
			DedupeKey:      envelope.DedupeKey,
			EventID:        envelope.EventID,
			Source:         envelope.Source,
			PayloadSummary: envelope.PayloadSummary,
			Payload:        envelope.Payload,
			Urgency:        envelope.Urgency,
		}
	}
	WriteJSON(w, http.StatusOK, queuedMessages{Queued: queued})
}
