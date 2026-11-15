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

// keepsForSession reports whether an attempt's last try ended in the one failure a targeted
// message is kept through: the listener holds no record of the session the attempt names,
// whether its registry read answered so ("no live session <id>", resolveDeliveryTarget) or its
// send's own 404 did. Both are one fact, so a person sees one outcome for it. An attempt that
// names no session is not kept: a role whose holder is gone resolves to none.
func keepsForSession(failure, sessionID string) bool {
	return sessionID != "" && strings.HasPrefix(failure, "no live session")
}

// keepForSession keeps a targeted message for a session the listener holds no record of: it
// publishes onto the session's own subject (contracts.AgentSubject) the envelope the listener's
// send route would have stored for this attempt, under the same dedupe key, and answers as a send
// does - the envelope it stored, or none and duplicate when the stream already held it, and ""
// for a message kept.
//
// The session reads the copy from its consumer when it registers again under its id, since that
// consumer starts at the first message stored on the subject that the session has not
// acknowledged (bus.SessionConsumers.create). The listener's own delivery of the copy, finding no
// record, gives it up with the agent wake that brings a released pod back. A repeat under the
// same key - the delivery sweep resuming an attempt whose sender died between this publish and
// its settle, or a copy of a send that did land - is the duplicate the stream reports rather than
// a second copy.
//
// A publish that fails keeps nothing: the attempt then settles failed with failure and the
// publish's own error, so the person can retry it.
func (s *server) keepForSession(
	ctx context.Context, message model.Message, pending pendingMessageDelivery, key string,
	wanted contracts.ExpectsReply, failure string,
) (*string, bool, string) {
	envelope := keptEnvelope(listenerSendInput(
		pending.resolved.attemptSessionID, pending.resolved.Delivery, message.Body, key,
		pending.urgency, wanted, pending.frame,
	))
	// The listener refuses to store an envelope that does not validate, and so does this.
	if err := envelope.Validate(); err != nil {
		return nil, false, keepFailure(failure, err)
	}
	duplicate, err := s.deps.AgentPublisher.PublishReportingDuplicateWithin(ctx, envelope)
	if err != nil {
		return nil, false, keepFailure(failure, err)
	}
	// A duplicate names an envelope JetStream discarded, so the attempt records none, as a send's
	// does.
	if duplicate {
		return nil, true, ""
	}
	return &envelope.EventID, false, ""
}

// keepFailure is what an attempt records when its message could not be kept: the listener's "no
// live session <id>" first, then why the keep failed.
func keepFailure(failure string, err error) string {
	return failure + "; it could not be kept for the session: " + err.Error()
}

// keptEnvelope is the envelope the listener's send route stores for input: built with the
// builder that route uses (contracts.AgentSendEnvelope), on the session's own subject under
// AgentSendDedupeKey(session, idempotency key), from source "dispatch" as every send Dispatch
// makes names itself (dispatchenvoy.Client.Send), with the frame as its payload. A send that did
// land and its kept copy are then one message under one stream id (bus.ForwardMsgID), which the
// stream stores once.
func keptEnvelope(input dispatchenvoy.SendInput) contracts.Envelope {
	payload := string(input.Payload)
	return contracts.AgentSendEnvelope(input.TargetSession, input.IdempotencyKey, contracts.Message{
		Source:       "dispatch",
		Text:         input.Message,
		Payload:      &payload,
		Urgency:      input.Urgency,
		ExpectsReply: input.ExpectsReply,
	})
}

// AgentPublishRecorder is the AgentPublisher of a test process that has no NATS: the browser
// suite runs Dispatch with DISPATCH_NATS_DISABLED=1 and its test hooks on, and reads what this
// process kept through GET /api/v1/messages/_test/kept. It stores an envelope once per subject and
// dedupe key and reports a repeat as a duplicate, as the stream does for an envelope it already
// holds. It records in this process's memory alone, so a test reads it from the process that
// kept the message.
type AgentPublishRecorder struct {
	mu        sync.Mutex
	envelopes []contracts.Envelope
}

// NewAgentPublishRecorder returns a recorder that has kept nothing yet.
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

// Kept answers every envelope recorded so far, oldest first.
func (r *AgentPublishRecorder) Kept() []contracts.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.envelopes)
}

// keptMessages is the body GET /api/v1/messages/_test/kept answers.
type keptMessages struct {
	Kept []keptMessage `json:"kept"`
}

// keptMessage is one envelope this process kept, as the stream would hold it: the subject it is
// stored on and the fields a test compares with the message it sent.
type keptMessage struct {
	Subject        string `json:"subject"`
	DedupeKey      string `json:"dedupe_key"`
	EventID        string `json:"event_id"`
	Source         string `json:"source"`
	PayloadSummary string `json:"payload_summary"`
	// Payload is the delivery frame the listener's send would have carried, as its JSON text.
	Payload string `json:"payload"`
	Urgency string `json:"urgency,omitempty"`
}

// listKeptMessages answers {"kept": [...]}: every targeted message this process kept for a
// session the listener held no record of, oldest first. It answers 404 TEST_HOOKS_OFF on a
// process that keeps them on the bus, where the stream is what holds them.
func (s *server) listKeptMessages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	recorder, ok := s.deps.AgentPublisher.(*AgentPublishRecorder)
	if !ok {
		writeError(w, "TEST_HOOKS_OFF", http.StatusNotFound,
			"this process keeps targeted messages on the bus, not in a test hook's recorder")
		return
	}
	stored := recorder.Kept()
	kept := make([]keptMessage, len(stored))
	for index, envelope := range stored {
		kept[index] = keptMessage{
			Subject:        envelope.Topic,
			DedupeKey:      envelope.DedupeKey,
			EventID:        envelope.EventID,
			Source:         envelope.Source,
			PayloadSummary: envelope.PayloadSummary,
			Payload:        envelope.Payload,
			Urgency:        envelope.Urgency,
		}
	}
	WriteJSON(w, http.StatusOK, keptMessages{Kept: kept})
}
