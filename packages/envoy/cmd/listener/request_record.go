// The listener's record of one inbound send or publish: the fixed outcome vocabulary and the
// requestRecord through which both handlers write each answer and its one log line together.

package main

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
)

// requestOutcome is what became of one inbound send or publish, in the fixed vocabulary those two
// records are read with: an operator's log filter matches these strings, so a route gets one word
// per answer and never a second spelling of the same one.
type requestOutcome string

const (
	outcomeAccepted       requestOutcome = "accepted"
	outcomeInvalidRequest requestOutcome = "invalid_request"
	outcomeTooLarge       requestOutcome = "too_large"
	outcomeBusUnavailable requestOutcome = "bus_unavailable"
	// outcomeNoLiveSession belongs to the send route alone: the session registry holds no entry
	// of the target within the gone horizon and the known-session store no record of it, so
	// nothing of it is live and nothing of it is known. outcomeSessionStopped is that route's 409
	// for a connected_only send whose target is stopped: the sender asked to be told rather than
	// have its message wait, and nothing was stored. outcomeCapabilityRefused is the send route's
	// target, or the publish route's role holder (sendToRole), that will not take this delivery
	// mode.
	outcomeNoLiveSession     requestOutcome = "no_live_session"
	outcomeSessionStopped    requestOutcome = "session_stopped"
	outcomeCapabilityRefused requestOutcome = "capability_refused"
	// outcomeRoleUnclaimed and outcomeRoleLapsed belong to the publish route alone. Both answer
	// 404 and neither is outcomeNoLiveSession: the request named a role, not a session, and the
	// two say different things about the claim behind it.
	outcomeRoleUnclaimed requestOutcome = "role_unclaimed"
	outcomeRoleLapsed    requestOutcome = "role_lapsed"
	// outcomeRoleClaimUnreadable is the publish route's role claim record that the bus answered
	// and no read can decode (store.ErrMalformedRoleClaim): a 500, not outcomeBusUnavailable,
	// since the bus did answer and retrying reads the same record.
	outcomeRoleClaimUnreadable requestOutcome = "role_claim_unreadable"
	// outcomePublishDenied is a publish the server refused under this listener's own publish
	// permissions (bus.ErrPublishDenied). Only refuseBusUnavailable records it, from the 403 that
	// writeBusUnavailable writes. It is not outcomeBusUnavailable: the bus answered, with a denial
	// no retry can get past.
	outcomePublishDenied requestOutcome = "publish_denied"
	// outcomeDeliveryFailed is the publish route's role send answered 504 (confirmed.go): its
	// window ended with no forward the stream stored for the holder.
	outcomeDeliveryFailed requestOutcome = "delivery_failed"
	// outcomeStreamFull is a send or publish the stream refused because it holds its byte limit
	// (bus.ErrStreamFull). It is not outcomeBusUnavailable: the bus answered, that the stream is
	// full, so an operator reading the record looks at the stream's size, not for an outage.
	outcomeStreamFull requestOutcome = "stream_full"
	// outcomeSenderRateLimited is a send or publish of an agent session whose count under the
	// operator's sender limit was empty (sender_limit.go), answered 429 before the route read a
	// session, resolved a role, reported a wake or stored anything, so no other record holds the
	// message. It is not the 429 apiauth.go answers a broker token's check with, whose reason is
	// rate_limited: this one is about how fast a session sends, not about its credential.
	outcomeSenderRateLimited requestOutcome = "sender_rate_limited"
)

// requestRecord is this listener's record of one inbound send or publish, and the only way either
// handler answers one. Until it existed the log held what arrived from the bus and nothing of what
// was asked of this listener, so a send that timed out left no server-side trace at all and "never
// attempted", "attempted and lost" and "attempted and refused" could not be told apart afterwards.
// A request refused before it reaches the bus is the one message no other system holds -- nothing
// was published, so no "listener received" line (delivery.go) and no stream entry will ever name
// it -- which is why the record carries the same short payload summary that arrival line carries:
// the message stays identifiable from this one line.
//
// Every return path of sendHandler and publishHandler answers through one of the methods below,
// which write the response and the record together. A status answered with no record, or a record
// whose outcome has drifted from the status actually written, is therefore not expressible.
//
// Two refusals are deliberately outside this record, because neither reaches a handler: a request
// the API auth wrapper refuses with 401 before the route is entered (apiauth.go), and a caller the
// starting gate turns away while the routes are still opening, which writes its own "request
// refused while starting" line (main.go).
type requestRecord struct {
	logger *logging.Logger
	// line is the record's message, "listener send" or "listener publish", and laneKey the name
	// of the destination field that route is addressed by, "target_session" or "topic". lane is
	// that destination, empty until the body has been decoded far enough to know it.
	line    string
	laneKey string
	lane    string
	// item is the envelope the handler built, taken by pointer rather than copied, and nil on a
	// request refused before the envelope exists (an undecodable body): that request still gets
	// its line, with the envelope's four fields empty rather than no line at all.
	item   *contracts.Envelope
	holder string
	// charge is the message's take from its sender's count (senderLimit.admit), nil for a message
	// the limit does not count, and refused the refusal of a message whose count was empty. The
	// line names the sender of a refusal, and of a charge only once it costs one
	// (senderCharge.counted), so the field sender marks exactly the messages that cost one or were
	// refused for the rate, and no other line of the listener carries it.
	charge  *senderCharge
	refused *senderRefusal
}

// accept writes the handler's 200 and records the request as accepted.
func (rec *requestRecord) accept(w http.ResponseWriter, value interface{}) {
	rec.acceptWith(w, http.StatusOK, value)
}

// acceptWith writes an accepted answer under a status of its own and records it as accepted: the
// publish route's 202 for a role message it kept rather than delivered (roleSend.kept), which is
// an answer and not a refusal, and is a different fact from the 200 of a message a holder took.
func (rec *requestRecord) acceptWith(w http.ResponseWriter, status int, value interface{}) {
	writeJSON(w, status, value)
	rec.write(status, outcomeAccepted)
}

// refuse writes exactly the named refusal writeJSONError writes, and records it under outcome.
func (rec *requestRecord) refuse(w http.ResponseWriter, status int, outcome requestOutcome, message string, expected ...string) {
	writeJSONError(w, status, message, expected...)
	rec.write(status, outcome)
}

// refuseWith writes a refusal whose body says more than writeJSONError's - a confirmed send's 403
// naming the holder or its 504 naming who did not confirm (confirmed.go) - and records it under
// outcome.
func (rec *requestRecord) refuseWith(w http.ResponseWriter, status int, outcome requestOutcome, body interface{}) {
	writeJSON(w, status, body)
	rec.write(status, outcome)
}

// refuseBusUnavailable answers a bus that did not serve the request and records the outcome the
// status it wrote implies: 413 is the caller's own oversized message, 400 the subject or key NATS
// will not take however often it is sent, 403 the publish this listener's own grant denies, and
// anything else the bus not answering. The status is read back from the write rather than
// classified a second time here, so the pair cannot drift, with one exception: a full stream
// (bus.ErrStreamFull) shares its 503 with the bus not answering, so the error tells it apart.
func (rec *requestRecord) refuseBusUnavailable(w http.ResponseWriter, err error) {
	status := writeBusUnavailable(w, err)
	outcome := outcomeBusUnavailable
	switch {
	case errors.Is(err, bus.ErrStreamFull):
		outcome = outcomeStreamFull
	case status == http.StatusRequestEntityTooLarge:
		outcome = outcomeTooLarge
	case status == http.StatusBadRequest:
		outcome = outcomeInvalidRequest
	case status == http.StatusForbidden:
		outcome = outcomePublishDenied
	}
	rec.write(status, outcome)
}

// refuseRoleClaimUnreadable answers a publish whose role claim record cannot be decoded
// (store.ErrMalformedRoleClaim) with the 500 writeRoleClaimUnreadable writes for both routes that
// resolve a role, and records it under its own outcome rather than bus_unavailable: the bus
// answered, so naming it bus_unavailable would send an operator after an outage that never
// happened instead of after the record.
func (rec *requestRecord) refuseRoleClaimUnreadable(w http.ResponseWriter, role string, err error) {
	rec.write(writeRoleClaimUnreadable(w, role, err), outcomeRoleClaimUnreadable)
}

// refuseRoleHolder answers the publish route's two role-holder refusals. A lapsed holder is the
// session the claim named, which the answer's own body names as well, so the record carries it;
// an unclaimed role has no holder to carry.
func (rec *requestRecord) refuseRoleHolder(w http.ResponseWriter, role string, result roles.Resolution) {
	writeRoleHolderError(w, role, result)
	outcome := outcomeRoleUnclaimed
	if result.State == roles.Lapsed {
		outcome = outcomeRoleLapsed
	}
	rec.holder = result.Holder
	rec.write(http.StatusNotFound, outcome)
}

// refuseSenderRateLimited answers a message whose sender's count was empty (senderLimit.take)
// with the 429: Retry-After and the body's retry_after_seconds name the same whole seconds,
// until the count holds one message again. It records the request under outcomeSenderRateLimited
// with the sender the limit refused.
func (rec *requestRecord) refuseSenderRateLimited(w http.ResponseWriter, refusal *senderRefusal) {
	rec.refused = refusal
	w.Header().Set("Retry-After", strconv.Itoa(refusal.retryAfterSeconds))
	rec.refuseWith(w, http.StatusTooManyRequests, outcomeSenderRateLimited, refusal.answer())
}

// write emits the one line, INFO for a request this listener took and WARN for one it refused.
func (rec *requestRecord) write(status int, outcome requestOutcome) {
	var eventID, source, sourceSession, payloadSummary string
	if rec.item != nil {
		eventID, source = rec.item.EventID, rec.item.Source
		sourceSession, payloadSummary = rec.item.SourceSession, rec.item.PayloadSummary
	}
	attrs := make([]slog.Attr, 0, 9)
	attrs = append(attrs,
		slog.String(rec.laneKey, rec.lane),
		slog.String("event_id", eventID),
		slog.String("source", source),
		slog.String("source_session", sourceSession),
		slog.String("payload_summary", payloadSummary),
		slog.String("outcome", string(outcome)),
		slog.Int("status", status))
	if rec.holder != "" {
		attrs = append(attrs, slog.String("holder", rec.holder))
	}
	// The charge is read as the line is written, which is why every route marks it before it
	// answers (senderCharge).
	sender := rec.charge.counted()
	if rec.refused != nil {
		sender = rec.refused.sender
	}
	if sender != "" {
		attrs = append(attrs, slog.String("sender", sender))
	}
	level := slog.LevelWarn
	if outcome == outcomeAccepted {
		level = slog.LevelInfo
	}
	rec.logger.LogEvent(level, rec.line, attrs...)
}
