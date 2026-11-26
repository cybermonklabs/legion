package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

type streamInfoLookup interface {
	StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error)
}

// optedOutOfHandoff reports whether body carried "handoff": false
// (listenerwire.MessageBody.Handoff), which keeps its message from being handed to a stopped
// Legion agent's daemon (handOffToLegion).
func optedOutOfHandoff(body listenerwire.MessageBody) bool {
	return body.Handoff != nil && !*body.Handoff
}

// noLiveSession is the refusal body for sessionID in state, with the last registration the
// registry can still name for it (lastSeenOf). When it can name none the text says so after the
// kept prefix, because the two cases call for different acts and a missing field names neither:
// a session this listener watched reach the gone horizon is dead, while one it has no record and
// no remembered registration for — the state a restart leaves, and the state of an id that never
// registered here — may be alive on another listener (dispatch://INFRA-392, A6).
func noLiveSession(sessions *session.SessionRegistry, sessionID string, entry session.SessionEntry, state session.State) listenerwire.NoLiveSessionRefusal {
	refusal := listenerwire.NoLiveSessionRefusal{
		Error:    contracts.NoLiveSession + " " + sessionID,
		State:    state,
		LastSeen: lastSeenOf(sessions, sessionID, entry),
	}
	if refusal.LastSeen == 0 {
		refusal.Error += "; this listener does not know when it last registered"
	}
	return refusal
}

// lastSeenOf is the last registration this listener can still name for sessionID: the record's
// own when one stands, else the final heartbeat the registry remembers for a session whose
// record has gone (session.SessionRegistry.LastSeen), else zero, which every caller reads as
// "not known". Both a refused send and a role whose holder lapsed report it, so a caller told a
// session is not answering can say how long it has been so.
func lastSeenOf(sessions *session.SessionRegistry, sessionID string, entry session.SessionEntry) int64 {
	if entry.UpdatedAt != 0 {
		return entry.UpdatedAt
	}
	return sessions.LastSeen(sessionID)
}

// roleSetResponse is POST /v1/roles/set's answer to a claim that landed: the claimant's interest
// record, and a warning when the claim named its role in the old form, its name alone, which names
// the new form, the project first (store.Registry.ClaimedRole).
type roleSetResponse struct {
	store.Interest
	Warnings []string `json:"warnings,omitempty"`
}

const unwiredRepositoryWarningTimeout = 750 * time.Millisecond

// hasCapability reports whether capabilities includes value. The Dispatch
// server (packages/envoy/internal/dispatch/api) has its own copy of this
// same check; the two packages communicate over HTTP, not a shared Go
// import, so there is one definition per side of that boundary rather than
// one shared helper.
func hasCapability(capabilities []string, value string) bool {
	for _, capability := range capabilities {
		if capability == value {
			return true
		}
	}
	return false
}

// frameDeliveryMode reads the canonical delivery mode from a targeted Dispatch frame's
// payload the same way the receiving client reads it: an exact, case-sensitive "delivery" key
// at the top level and an exact, case-sensitive "mode" key inside it. The receiving client
// (packages/envoy-client/src/delivery.ts's parseDispatchFrame) gets there via JSON.parse plus
// plain object property access, which is exact and case-sensitive in JavaScript; the wire key
// is fixed lower-case ("delivery") in packages/contracts/src/dispatch-api.ts. Decoding straight
// into a Go struct would be a *second, more lenient* reader of the same bytes:
// encoding/json matches JSON object keys case-insensitively when no exact match exists, so a
// payload carrying both the receiver's exact "delivery" key and a same-key-different-case
// sibling like "Delivery" could make that lenient decode read the sibling's mode while the
// receiver executes the exact key's -- guard and receiver disagreeing about what is being
// sent. This function never resolves that disagreement; it refuses to guess.
//
// The return is a genuine tri-state, distinguished by type, not by string emptiness (an empty
// mode string can never again mean two different things):
//
//   - No "delivery" key at all (nil payload, unparseable JSON, or a parsed object missing the
//     exact key): mode="", err=nil. This is an ordinary, untagged send -- exactly like an
//     interest-topic publish or role forward -- and stays unguarded, because there is no
//     "delivery" key for the receiver to see either.
//   - "delivery" is present (the exact key exists) but its mode cannot be determined
//     unambiguously as a single non-empty string -- delivery is not an object, has no "mode"
//     key, "mode" is not a JSON string, "mode" is an empty string, or a same-key-different-
//     case sibling exists for "delivery" or "mode": mode="", err!=nil. The caller must refuse
//     the send outright. A malformed or unreadable targeted frame is never treated as if it
//     were untagged: presence of the exact "delivery" key is itself a claim that this send is
//     targeted, and an unreadable claim is refused, not waved through.
//   - "delivery" is present and its mode reads unambiguously as one of the three delivery
//     modes: mode=<that string>, err=nil.
//   - "delivery" is present and its mode reads unambiguously as something else: mode="",
//     err!=nil, refused like any other unreadable claim. Waving it through on the grounds that
//     the capability check behind it would refuse a mode no session advertises does not hold:
//     `capabilities` is an open list from the registry, so a session that advertises a
//     bogus string is exactly what makes that argument fail. The Dispatch server's
//     `validDelivery` already refuses the same set at its own boundary; this is the listener's.
func frameDeliveryMode(payload *string) (mode string, err error) {
	if payload == nil {
		return "", nil
	}
	var top map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal([]byte(*payload), &top); unmarshalErr != nil {
		return "", nil
	}
	deliveryRaw, hasDelivery := top["delivery"]
	if !hasDelivery {
		return "", nil
	}
	if hasCaseVariantSibling(top, "delivery") {
		return "", fmt.Errorf("delivery key has an ambiguous case-variant sibling")
	}
	var delivery map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(deliveryRaw, &delivery); unmarshalErr != nil {
		return "", fmt.Errorf("delivery is present but not a JSON object: %w", unmarshalErr)
	}
	if hasCaseVariantSibling(delivery, "mode") {
		return "", fmt.Errorf("delivery.mode key has an ambiguous case-variant sibling")
	}
	modeRaw, hasMode := delivery["mode"]
	if !hasMode {
		return "", fmt.Errorf("delivery is present but has no mode")
	}
	if unmarshalErr := json.Unmarshal(modeRaw, &mode); unmarshalErr != nil {
		return "", fmt.Errorf("delivery.mode is present but not a JSON string: %w", unmarshalErr)
	}
	if mode == "" {
		return "", fmt.Errorf("delivery.mode is present but empty")
	}
	if !isDeliveryMode(mode) {
		return "", fmt.Errorf("delivery.mode %q is not a delivery mode", mode)
	}
	return mode, nil
}

// payloadDeliveryMode is frameDeliveryMode of payload, none when it is empty.
func payloadDeliveryMode(payload string) (mode string, err error) {
	var framed *string
	if payload != "" {
		framed = &payload
	}
	return frameDeliveryMode(framed)
}

// isDeliveryMode reports whether value is one of the three targeted-delivery modes. The list is
// DELIVERY_CAPABILITIES in packages/contracts/src/dispatch-api.ts, spelled here as the Dispatch
// server spells it in its own `validDelivery`: the two sides talk over HTTP, not a shared Go
// import.
func isDeliveryMode(value string) bool {
	return value == "aside" || value == "btw" || value == "steer"
}

// hasCaseVariantSibling reports whether keys contains some key that case-insensitively, but
// not exactly, matches exact -- a same-field claim under a different spelling that an
// exact-match reader (this guard, or the JavaScript receiver's plain property access) would
// never see, but a case-insensitive one (Go's struct unmarshal, if used here) could.
func hasCaseVariantSibling(keys map[string]json.RawMessage, exact string) bool {
	for key := range keys {
		if key != exact && strings.EqualFold(key, exact) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string, expected ...string) {
	writeJSON(w, status, listenerwire.APIError{Error: message, Expected: expected})
}

// listRefillingCode, listRefillingMessage and listRefillingRetryAfter are the refilling answer of
// the two list routes (writeListRefilling).
const (
	listRefillingCode       = "refilling"
	listRefillingMessage    = "envoy: the agent list is refilling; try again in a moment"
	listRefillingRetryAfter = "5"
)

// writeListRefilling answers a list read (GET /v1/sessions, GET /v1/interests/) whose cache copy
// cannot be served (kvwatch.ErrRefilling): it has not completed a read of its bucket yet, or has
// not followed the bucket for longer than the stale bound. A 503 with Retry-After and the
// refilling code, never a list: a list from that copy could be missing a live agent, and a caller
// would take the missing row for an absent agent. The /v1 start gate answers the same while no
// copy has been read at all.
func writeListRefilling(w http.ResponseWriter) {
	w.Header().Set("Retry-After", listRefillingRetryAfter)
	writeJSON(w, http.StatusServiceUnavailable, listenerwire.APIError{Error: listRefillingMessage, Code: listRefillingCode})
}

// writeNATSError answers a request NATS did not serve. What NATS refuses however often it is sent
// (bus.ErrRefused) is the caller's own input, so it is answered naming why: a message too large to
// take whole, or a session id or role whose KV key would make a subject too long (bus.ErrTooLarge),
// is a 413, and any other refusal, a topic or key NATS does not accept in a subject
// (bus.ErrInvalidSubject) or a key outside nats.go's key alphabet (bus.ErrInvalidKey: `ses:bad`), a
// 400. Any other failure is status with message. It returns the status it wrote: a caller that
// also records the answer (requestRecord, request_record.go) reads this one classification back
// instead of repeating it, so no record can disagree with the response that was sent.
func writeNATSError(w http.ResponseWriter, err error, status int, message string) int {
	switch {
	case errors.Is(err, bus.ErrTooLarge):
		status, message = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, bus.ErrRefused):
		status, message = http.StatusBadRequest, err.Error()
	}
	writeJSONError(w, status, message)
	return status
}

// busUnansweredPrefix opens the body of every answer this listener gives a request the message bus
// did not answer, so one string matches in a test and in a deployed log filter. It is deliberately
// distinct from the starting gate's "service starting" 503 (main.go): that one says this listener
// is not ready yet and the same request will be served here in a moment, this one says the bus
// itself did not answer, and the two ask the caller for different actions.
const busUnansweredPrefix = "envoy: the message bus did not answer"

// publishDeniedPrefix opens the body of every answer this listener gives a publish the NATS server
// refused under its connection's own publish permissions, so one string matches in a test, in a
// deployed log filter and in the Legion daemon, whose notify.HTTPPublisher finishes a notice on a
// 403 whose error opens with its copy of this string (publishDeniedPrefix,
// packages/daemon/internal/notify/notify.go). Rewording it turns that denial back into a notice the
// daemon retries without a cap, holding the architect's later notices behind it. It is
// deliberately not busUnansweredPrefix: the bus answered this publish, and answered it with a
// denial, so "the message bus did not answer" would be false of it, and the operator reading it
// would go looking for an outage instead of for the grant that forbids the subject.
const publishDeniedPrefix = "envoy: the message bus denied the publish under this listener's own permissions"

// streamFullPrefix opens the body of every answer this listener gives a send or publish the stream
// refused because it holds its byte limit (bus.ErrStreamFull: ENVOY_STREAM_MAX_BYTES with
// DiscardNew), so one string matches in a test and in a deployed log filter. It is deliberately not
// busUnansweredPrefix: the bus answered, that the stream is full, so "the message bus did not
// answer" would send an operator looking for an outage instead of at the stream's size. Unlike a
// denial it is worth retrying: the stream takes the same message once age or an operator has made
// room.
const streamFullPrefix = "envoy: the message bus refused the message because its stream is full"

// storedCopyUnreadablePrefix opens the body of the 503 a send, a topic publish or a role send is
// answered when the stream already knows the message's id and the listener read no stored copy to
// answer (errStoredCopyUnread names the cases). One string, so it matches in a test and in a
// deployed log filter. It says only what was seen: the copy may be stored yet, or gone, deleted or
// aged out while the stream still remembers its id. It is deliberately not busUnansweredPrefix:
// the bus answered the publish, and the read may have been answered too, with a denial or a
// message that is not the copy. The route answers no envelope and asks for the same request
// again, which gets the copy once it can be read.
const storedCopyUnreadablePrefix = "envoy: the stream already knows this message's id, and no stored copy of it was read"

// earlierAttemptInProcessPrefix opens the body of the 503 a send or publish gets when its request
// ended while the stream was still storing an earlier publish under the same message id
// (errEarlierAttemptInProcess): an earlier attempt of the same message, this sender's or another
// listener's. It is deliberately not busUnansweredPrefix: the bus answered, that the earlier
// attempt is in process, and the stream may store it yet, so the answer carries
// listenerwire.CodeMaybeStored and the sender reads it as a send that may have landed.
const earlierAttemptInProcessPrefix = "envoy: an earlier attempt of this message is still being stored"

// notSentPrefix opens the body of the 503 a send or publish gets when the bus refused its message
// before it sent anything, because the deployed stream, as this listener's client last read its
// subjects, does not capture a topic this build stores (bus.ErrSubjectNotDeployed): a hub move's
// quiesce, or an operator's edit of the subjects. It is deliberately not busUnansweredPrefix: the
// bus was never asked, so "the message bus did not answer" would send an operator looking for an
// outage, and a sender could not tell it from a 5xx after which the message may have arrived. It
// is contracts.MessageNotSentPrefix, which envoy-client's transport reads as a message nothing
// stored, so its sender may send it again once the stream captures the topic.
const notSentPrefix = contracts.MessageNotSentPrefix

// grantGuardPrefix opens the error of the answer POST /v1/roles/set gives a soft claim the grant
// guard refused (roleGrantGuarded): a role held under the claimant's own session id whose renewal
// is older than the live window, while the role claim copy's last whole read is past its repair
// bound or none has completed, or while this listener's own renewal of that claim failed inside
// the window (roleOwnSessionVerdict). The error after it names which of the two refused. One
// string, so it matches in a test and in a deployed log filter. It is deliberately not
// busUnansweredPrefix: every read the guard rests on answered, so "the message bus did not
// answer" would send an operator looking for an outage instead of at the repair's age or at the
// renewal that failed. It rides the 409 that names the claimant's own session id as the holder
// (writeGrantGuardRefusal), which the claimant waits out as it waits out a standing claim of
// another process: the guard stops refusing once a whole read completes inside the bound and the
// record of the failure is cleared or older than the live window.
const grantGuardPrefix = "envoy: the role grant guard refused the claim because this listener cannot vouch that its holder stopped renewing it"

// writeBusUnavailable answers a send, a publish, a role lookup or a session or interest read the
// bus did not serve, in the one place that writes that 503, so the body's prefix and the hint never
// drift apart. Retry-After is set only where retrying can help: every member of bus.ErrRefused -
// a refusal NATS repeats however often the same bytes are sent - is answered by writeNATSError as
// what it is, bus.ErrTooLarge as the 413 and every other member as the 400, so this one guard
// keeps the header off every bus.ErrRefused and off nothing else. The status writeNATSError
// settled on is passed back for the record's sake, as that function's own comment explains.
//
// A publish the server denied (bus.ErrPublishDenied) is answered ahead of both, because it is
// neither. bus.ErrRefused's members (internal/bus/nats.go) are each a refusal of the caller's bytes
// that the same bytes meet at every attempt: this client's own, decided before anything is sent -
// bus.ErrInvalidSubject, bus.ErrInvalidKey, bus.ErrNoForwardMsgID, kvchunk.ReservedKey, and
// bus.ErrTooLarge for a message past the max payload - or the stream's refusal of a value past its
// bucket's largest value (JetStream's 10054, sent after the write), which bus.ErrTooLarge names
// too. A denial is the opposite: the bytes are fine, and the publish reached a
// server that answered `-ERR 'Permissions Violation for Publish to "<subject>"'` and dropped it,
// which internal/bus names bus.ErrPublishDenied around nats.ErrPermissionViolation and the
// server's own text (confirmPublished for a core publish, deniedPublish for a JetStream one). So
// it is not an outage and not a malformed request, and retrying it can never succeed: the grant
// this listener connects with forbids the subject until an operator changes it. 403 says exactly
// that - the request was understood and the server refuses to authorise it - where 400 would
// blame the caller's bytes, which are fine, and where any 5xx would be retried:
// packages/envoy-client/src/transport.ts retries every status at or past 500 and no status below
// it. No Retry-After rides it for the same reason.
//
// A message the stream refused because it is full (bus.ErrStreamFull) is answered ahead of the
// bus-unavailable 503 too, with a 503 and Retry-After of its own under streamFullPrefix: the bus
// answered, so the body must not say it did not, but the same message is taken once the stream
// has room, so the caller's retry is right. So is a message the bus refused before it sent
// anything (notSent), under notSentPrefix: the bus was never asked, and the same message is
// stored once the stream captures its topic again.
//
// A message the stream already held whose stored copy could not be read (errStoredCopyUnread) is
// answered first of all, a 503 with Retry-After under storedCopyUnreadablePrefix, whatever the
// read met: its error names a read's cause in its text and wraps none of it, so a read the server
// denied is not the publish's 403, and the stored message is not lost to a refusal no retry gets
// past. The body carries the reason once, after the prefix, without the sentinel's own words.
//
// A publish that may have stored its message (mayHaveStored) is the 503 with
// listenerwire.CodeMaybeStored, whichever answer ended it - a full stream's, or a refusal of the
// resend after a send nothing answered - so a sender treats it as a send that may have landed
// rather than one that stored nothing. So is a publish whose request ended while an earlier
// publish under its message id was still in process (earlierAttemptInProcess), under
// earlierAttemptInProcessPrefix unless the stream also said it is full: the stream may still store
// that earlier attempt.
func writeBusUnavailable(w http.ResponseWriter, err error) int {
	if errors.Is(err, errStoredCopyUnread) {
		w.Header().Set("Retry-After", "1")
		reason := strings.TrimPrefix(err.Error(), errStoredCopyUnread.Error()+": ")
		answer := listenerwire.APIError{Error: fmt.Sprintf("%s: %s", storedCopyUnreadablePrefix, reason)}
		if mayHaveStored(err) {
			answer.Code = listenerwire.CodeMaybeStored
		}
		writeJSON(w, http.StatusServiceUnavailable, answer)
		return http.StatusServiceUnavailable
	}
	if errors.Is(err, bus.ErrPublishDenied) {
		writeJSONError(w, http.StatusForbidden, fmt.Sprintf("%s: %s", publishDeniedPrefix, err))
		return http.StatusForbidden
	}
	stored, full, inProcess := mayHaveStored(err), errors.Is(err, bus.ErrStreamFull), earlierAttemptInProcess(err)
	prefix := busUnansweredPrefix
	switch {
	case full:
		prefix = streamFullPrefix
	case inProcess:
		prefix = earlierAttemptInProcessPrefix
	case notSent(err):
		prefix = notSentPrefix
	}
	if !stored && !full && !inProcess {
		if !errors.Is(err, bus.ErrRefused) {
			w.Header().Set("Retry-After", "1")
		}
		return writeNATSError(w, err, http.StatusServiceUnavailable, fmt.Sprintf("%s: %s", prefix, err))
	}
	w.Header().Set("Retry-After", "1")
	answer := listenerwire.APIError{Error: fmt.Sprintf("%s: %s", prefix, err)}
	if stored || inProcess {
		answer.Code = listenerwire.CodeMaybeStored
	}
	writeJSON(w, http.StatusServiceUnavailable, answer)
	return http.StatusServiceUnavailable
}

// mayHaveStored reports whether a publish that failed with err may still have stored its message:
// a send of it went out and nothing answered (bus.ErrUnacknowledged), whatever answer ended the
// publish after it - a full stream, or a refusal of a later send, since the first may have landed.
// It is the one rule the bus-unavailable answer (writeBusUnavailable), the sender's charge
// (senderCharge.published), a keep's failure (storeKeptRoleCopy) and a publish past an in-process
// refusal (publishPastInProcess) read.
func mayHaveStored(err error) bool {
	return errors.Is(err, bus.ErrUnacknowledged)
}

// notSent reports whether a send or publish that failed with err sent nothing: the bus refused its
// message before anything went out (bus.ErrSubjectNotDeployed) and no earlier send of it went out
// unanswered (mayHaveStored). It is the one rule the answer's prefix (writeBusUnavailable) and the
// request record's outcome (requestRecord.refuseBusUnavailable) read.
func notSent(err error) bool {
	return errors.Is(err, bus.ErrSubjectNotDeployed) && !mayHaveStored(err)
}

// writeGrantGuardRefusal answers a soft claim the grant guard refused (roleGrantGuarded) as the
// route answers the refusal of a role held under the claimant's own session id by another process
// (roleHeldByAnotherProcess, through store.ErrRoleHeld): a 409 whose body names role and, as
// holder, the session the standing claim is held under, which is the claimant's own. Its error
// opens with grantGuardPrefix and goes on to err, which names the arm that refused. No
// Retry-After rides it.
//
// The status and the body are that refusal's, and not the bus-unavailable 503's, for what the
// clients do with each. A relaunched Legion pane claims under the session id of the process it
// replaced, with a client instance of its own, while that process's claim still stands, and none
// of its registrations clears a record the guard reads against that claim. On a 503
// envoy-client's setRole makes its two attempts, 250 ms apart, and rejects
// (packages/envoy-client/src/transport.ts), and a rejected claim stops the pane
// (packages/pi-shared/src/legion-claim-core.ts, stopAfterRegistration). setRole reads a 409
// carrying error, role and holder as a claim not granted that names its holder, and both
// harnesses ask again on their own cadence for as long as that holder is their own session
// (packages/claude-envoy/src/legion-claim.ts, packages/pi-envoy/src/registration-state.ts): the
// wait a standing claim of another process gets. claude-envoy asks for no longer than
// ROLE_CLAIM_WAIT_MS, the live window and 10 s, so a refusal that outlasts that still stops its
// pane.
func writeGrantGuardRefusal(w http.ResponseWriter, role, holder string, err error) {
	writeJSON(w, http.StatusConflict, map[string]string{
		"error":  fmt.Sprintf("%s: %s", grantGuardPrefix, err),
		"role":   role,
		"holder": holder,
	})
}

// storeAnswered reports whether err, from the store work of a /v1 route that writes, done within
// ctx, is the store's own answer: a key NATS refuses (bus.ErrRefused), no key (nats.ErrKeyNotFound),
// a record this build cannot decode (store.ErrMalformedInterest, store.ErrMalformedRoleClaim), or a
// conditional write or delete another writer's change beat (bus.IsCASConflict). Every other error
// did not answer, whatever it names, and so does any error once ctx has ended. An errors.Join of
// failures (store.Registry.SetRoleWithPrevious joins a lost claim with its holder read and its
// rollback) answered only when every part of it did.
func storeAnswered(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return storeAnswer(err)
}

// joinedType is the type errors.Join returns. fmt.Errorf with several %w also unwraps to a list,
// but its parts are one failure and the detail it carries (store.ErrMalformedInterest and the
// decode error), so only a join is split; a join under a single-%w wrapper is found by walking the
// wrapper's chain first.
var joinedType = reflect.TypeOf(errors.Join(errors.New("")))

func storeAnswer(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if joined, ok := e.(interface{ Unwrap() []error }); ok && reflect.TypeOf(e) == joinedType {
			for _, part := range joined.Unwrap() {
				if !storeAnswer(part) {
					return false
				}
			}
			return true
		}
	}
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, nats.ErrKeyNotFound) || bus.IsCASConflict(err) ||
		errors.Is(err, store.ErrMalformedInterest) || errors.Is(err, store.ErrMalformedRoleClaim)
}

// writeStoreError answers err from a write route's store work done within ctx: a store answer
// (storeAnswered) as status naming err, as the route always answered it (a refused key still the
// 413 or 400 writeNATSError gives it), and anything else as the bus-unavailable 503 with
// Retry-After: 1 (writeBusUnavailable).
func writeStoreError(ctx context.Context, w http.ResponseWriter, err error, status int) {
	if storeAnswered(ctx, err) {
		writeNATSError(w, err, status, err.Error())
		return
	}
	writeBusUnavailable(w, err)
}

// validateMessageBody refuses a body no envelope may be built from, naming the refusal and the
// field it is about, and returns the body's parsed expects_reply so the envelope's message
// (listenerMessage) takes the value this parse already read instead of parsing the same bytes
// again. A refused body returns the unset value, which no caller reads.
func validateMessageBody(body listenerwire.MessageBody) (string, string, listenerwire.ExpectsReply) {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"in_reply_to", body.InReplyTo},
		{"supersedes", body.Supersedes},
	} {
		if field.value != nil && strings.TrimSpace(*field.value) == "" {
			return field.name + " must not be empty", field.name, listenerwire.ExpectsReplyUnset
		}
	}
	if body.Urgency != nil {
		if *body.Urgency == "" || (*body.Urgency != "low" && *body.Urgency != "med" && *body.Urgency != "high" && *body.Urgency != "blocking") {
			return "urgency must be one of low, med, high, blocking", "urgency", listenerwire.ExpectsReplyUnset
		}
	}
	expectsReply, err := listenerwire.ParseExpectsReply(body.ExpectsReply)
	if err != nil {
		return "expects_reply must be one of " + listenerwire.ExpectsReplyVocabulary, "expects_reply", listenerwire.ExpectsReplyUnset
	}
	return "", "", expectsReply
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// listenerMessage is what the request's sender wrote, as the envelope builders take it
// (contracts.MessageEnvelope, contracts.AgentSendEnvelope), its expects_reply the value
// validateMessageBody parsed from the same bytes.
func listenerMessage(body listenerwire.MessageBody, expectsReply contracts.ExpectsReply) contracts.Message {
	return contracts.Message{
		Source:        body.Source,
		SourceSession: body.SourceSession,
		Text:          body.Message,
		Payload:       body.Payload,
		InReplyTo:     optionalString(body.InReplyTo),
		Supersedes:    optionalString(body.Supersedes),
		Urgency:       optionalString(body.Urgency),
		ExpectsReply:  expectsReply,
		ExpiresAt:     body.ExpiresAt,
	}
}

// expectsReplyAnswer is the expects_reply an answer to one request writes for the envelope it
// answers, whose own value is answered: the request's own bytes where they read as that value, so
// each sender reads back the dialect it spoke - the literal false, "optional", "required", or the
// old string "none" - and nil where both say nothing about a reply, a request that wrote null
// among them, whose answer omits the field as it always has. The route refuses anything else
// (validateMessageBody) before an answer is built, and the envelope those same bytes were parsed
// into carries the one new vocabulary whichever spelling they used, so for the envelope a route
// built from the request the answer is always the request's own bytes.
//
// The answered envelope is another one only where the stream or a role's kept lane already held
// the message, and answers its stored copy (publishHeld, roleSend.kept, keepRoleSend): a repeat
// under the same key whose request asked for another reply than the copy carries is answered the
// copy's value, in the one new vocabulary, since the answer names the stored message and not the
// request.
//
// Answering the request's own spelling is what keeps the ingress leniency's promise whole. A
// plugin build old enough to write "none" holds an EnvelopeSchema whose expects_reply is the
// three-word enum (packages/contracts/src/envelope.ts), and it parses this very answer
// (packages/envoy-client/src/transport.ts): a literal false there fails that parse, so its send
// is reported unconfirmed and re-sent, and its publish throws. Such a plugin re-sends the body it
// sent, so a repeat of its own reads back its own word. The echo is the answer's alone - every
// envelope this listener puts on the bus carries the one new vocabulary - and it goes with the
// leniency itself, on the same count listenerwire.ParseExpectsReply's comment names: once no
// deployment pins a plugin build old enough to write "none".
func expectsReplyAnswer(requested json.RawMessage, answered contracts.ExpectsReply) json.RawMessage {
	if answered == contracts.ExpectsReplyUnset {
		return nil
	}
	if spoken, err := listenerwire.ParseExpectsReply(requested); err == nil && spoken == answered {
		return requested
	}
	written, err := json.Marshal(answered)
	if err != nil {
		// A value no decoded envelope carries.
		return nil
	}
	return written
}

// senderStamp is the sender a send or a publish carries: the session, the machine and directory it
// runs in (its interest record, or its session entry where that record cannot be read), the roles
// whose claims name it (store.Registry.RolesHeldBy), and its title (its session entry). It only
// enriches the envelope, so no read it makes may cost the message. The interest record is read
// from the cache, and a miss - a sender that subscribed to nothing, or any sender while the cache
// refills after a reconnect - is read through to the bucket within senderStampReadTimeout of ctx,
// the route's own (store.Registry.Get bounds that read alone). A read that does not answer leaves
// the stamp with what is already known, the session id and the title, machine and directory of
// the entry the session cache holds, and leaves the rest of the budget to the publish that
// follows, which meets the same silent bus itself and is answered as the named 503. The roles come
// from the registry's watched view of the claims, with no bucket call, and a view that is not
// complete (store.ErrRoleViewNotReady) leaves them out rather than refusing the send.
//
// The machine and directory are the interest record's wherever its read answers it, and the
// session entry's where that read finds no record, does not answer, or answers a record this build
// cannot decode (store.ErrMalformedInterest). Each registration writes both, moving the record to
// the listener and directory it came through (internal/store's mergeForUpsert) and building the
// entry afresh (sessionEntryFromSubscribe), so either names the registration the session has now.
// The record is read first and decides wherever it answers; the entry is the one the title is read
// from, so the fallback adds no read and spends no more of the window.
//
// The title is read the same way and within that same one window, so the two read-throughs of one
// stamp cannot cost the message more than the window the budget allots it (senderStampReadTimeout)
// and a silent bus still leaves the publish its 1.5 s. session.SessionRegistry.Get answers this
// listener's in-process cache alone, with one error for every row it does not hold
// (nats.ErrKeyNotFound), and exactly that error is read through to the bucket with
// session.SessionRegistry.Refresh: so a sender that registered a moment ago through another
// listener, and any sender while the session cache refills after a reconnect, carries the title
// the bucket holds. The read is made for a sender this cache misses, once per request, and nothing
// remembers the miss - neither a negative cache nor a rate limit
// (dispatch://INFRA-1368/artifact/decisions-md).
//
// Two senders still give a message with no title, and no read answers either. A row the bucket
// dropped at its maximum age is gone from the bucket too, because a cached row's deadline is that
// same drop point (session.SessionRegistry.Get), so a row this cache calls expired is one the
// bucket no longer holds. A registration that carries no title is a hit with an empty value rather
// than a miss, so it reaches the bucket not at all.
//
// A read-through that answers neither the row nor the bucket's word that it holds none - one the
// window ended, or one the bus failed - is logged at WARN through the registry's logger, with its
// bound (senderStampReadTimeout) and its error: the stamp it left short says nothing of why. A
// cache hit makes no read, a sender with no row is answered nats.ErrKeyNotFound, and an interest
// read's answer about the record (interestReadAnswered) is no failed read either, so the line is
// never one a healthy bus writes per message; a record that does not decode is left to the
// registry's own line.
func senderStamp(ctx context.Context, registry *store.Registry, sessions *session.SessionRegistry, sourceSession string) *contracts.EnvelopeSender {
	if sourceSession == "" {
		return nil
	}
	sender := &contracts.EnvelopeSender{SessionID: sourceSession}
	readCtx, cancel := context.WithTimeout(ctx, senderStampReadTimeout)
	defer cancel()
	var unansweredInterest, titleErr error
	interest, interestErr := registry.Get(readCtx, sourceSession)
	if interestErr != nil && !errors.Is(interestErr, nats.ErrKeyNotFound) && !interestReadAnswered(interestErr) {
		unansweredInterest = interestErr
	}
	if held, err := registry.RolesHeldBy(sourceSession); err == nil {
		sender.Roles = held
	}
	entry, entryErr := sessions.Get(sourceSession)
	if errors.Is(entryErr, nats.ErrKeyNotFound) {
		entry, entryErr = sessions.Refresh(readCtx, sourceSession)
		if entryErr != nil && !errors.Is(entryErr, nats.ErrKeyNotFound) {
			titleErr = entryErr
		}
	}
	if entryErr == nil {
		sender.Title = entry.Title
	}
	switch {
	case interestErr == nil:
		sender.Machine, sender.Cwd = interest.MachineID, interest.Dir
	case entryErr == nil:
		sender.Machine, sender.Cwd = entry.MachineID, entry.Dir
	}
	if readErr := errors.Join(unansweredInterest, titleErr); readErr != nil {
		registry.Logger().Warn("listener sender stamp left out what a read-through did not answer within its bound",
			slog.String("session_id", sourceSession),
			slog.Duration("bound", senderStampReadTimeout),
			slog.String("error", readErr.Error()),
		)
	}
	return sender
}

// forwardVerdictKind is how beforeForward judged a message to a holder.
type forwardVerdictKind uint8

const (
	// forwardAllowed: nothing stops the forward.
	forwardAllowed forwardVerdictKind = iota
	// forwardEcho: the message's source session is the holder itself.
	forwardEcho
	// forwardRepeat: the holder already took a message under this dedupe key.
	forwardRepeat
	// forwardModeUnreadable: the payload's delivery mode cannot be read unambiguously.
	forwardModeUnreadable
	// forwardModeUnadvertised: the holder does not advertise the payload's delivery mode.
	forwardModeUnadvertised
)

// forwardVerdict is beforeForward's judgement. mode is the payload's delivery mode, set for
// forwardModeUnadvertised; refusal is the refusal's text, set for both mode verdicts, and is what
// sendHandler and the publish route answer in their 403.
type forwardVerdict struct {
	kind    forwardVerdictKind
	mode    string
	refusal string
}

// beforeForward applies, to a message about to be forwarded to holder, whose session entry is
// entry, the rules a role delivery applies before it forwards, in this order: the source session
// holds the role (an echo, never forwarded back), seen reports the holder already took this dedupe
// key (a repeat), and the holder cannot take the payload's delivery mode (deliveryModeVerdict). The
// role lane (roleTopicDelivery) and the publish route (roleSend.settledBeforeForward) both judge by
// it, each with the record it trusts as seen, so both refuse and skip the same messages. Guarding
// after the dedupe check keeps a delivered repeat from being judged by the holder's current
// capabilities, which can have changed since the original forward and would misreport a delivered
// message as failed — a false signal the Legion daemon treats as cause to probe and potentially
// resume the worker.
func beforeForward(item contracts.Envelope, holder string, entry session.SessionEntry, seen func(key, sessionID string) bool) forwardVerdict {
	if item.SourceSession != "" && item.SourceSession == holder {
		return forwardVerdict{kind: forwardEcho}
	}
	if seen(item.DedupeKey, holder) {
		return forwardVerdict{kind: forwardRepeat}
	}
	return deliveryModeVerdict(holder, entry, item.Payload)
}

// deliveryModeVerdict judges a message to sessionID, whose session entry is entry, by its
// payload's delivery mode, read as the receiving client reads it (frameDeliveryMode): the mode
// cannot be read unambiguously, or entry does not advertise it (hasCapability); otherwise the
// message may be forwarded. sendHandler answers a refusal 403 with its text, recorded as
// outcomeCapabilityRefused, as the publish route does through beforeForward.
func deliveryModeVerdict(sessionID string, entry session.SessionEntry, payload string) forwardVerdict {
	return deliveryModeVerdictFor(sessionID, entry.Title, entry.Capabilities, true, payload)
}

// stoppedDeliveryModeVerdict judges a send to sessionID, which the session registry holds no entry
// for within the gone horizon, by the known-session record it does have. The record's
// capabilities are the ones the session last registered with, so a stopped session is judged by
// its own word exactly as a live one is judged by its entry.
//
// A record carrying no capabilities at all is one whose plugin declared none: KnownSession has
// stored Capabilities since its first write, so there is no record that predates the field. Such
// a send is accepted here, where the live path refuses the same mode-carrying send to the same
// plugin (deliveryModeVerdict passes advertised true), because the message is stored and judged
// where it can be judged by the session that reads it: the resumed session declares what it
// takes when it registers again, and that registration can declare the mode this send carries.
// Refusing it here would turn a send to a resumable session into a 403 on the strength of a
// field nobody filled in.
func stoppedDeliveryModeVerdict(sessionID string, record session.KnownSession, payload string) forwardVerdict {
	return deliveryModeVerdictFor(sessionID, record.Title, record.Capabilities, len(record.Capabilities) > 0, payload)
}

// deliveryModeVerdictFor is the judgement both of the above make: the payload's delivery mode is
// unreadable, or advertised is set and capabilities do not carry that mode. An unreadable mode is
// refused whatever the session is, since that refusal is about the payload and not about who it
// is for; advertised is false only for a known-session record that carries no capability at all,
// whose send is left to the session that reads it to judge (stoppedDeliveryModeVerdict).
func deliveryModeVerdictFor(sessionID, title string, capabilities []string, advertised bool, payload string) forwardVerdict {
	mode, err := payloadDeliveryMode(payload)
	switch {
	case err != nil:
		return forwardVerdict{
			kind:    forwardModeUnreadable,
			refusal: fmt.Sprintf("session %s (%s) sent a delivery mode that cannot be read unambiguously", sessionID, title),
		}
	case advertised && mode != "" && !hasCapability(capabilities, mode):
		return forwardVerdict{
			kind:    forwardModeUnadvertised,
			mode:    mode,
			refusal: fmt.Sprintf("session %s (%s) does not advertise %s", sessionID, title, mode),
		}
	}
	return forwardVerdict{kind: forwardAllowed}
}

// sendRequestBudget is the bus work a /v1 route does under the request's own context: the send
// route's read of the target session, its reads of the sender's interest record and session entry
// (senderStamp), its connection probe of the target (connectionProbeTimeout) and its publish share
// it, and GET /v1/roles/<role> resolves its holder within it too. The publish route spends it on
// the publish of a topic the stream captures, and a role send's first resolution of the role's
// holder (sendToRole, every attempt of that resolution) is answered within it too; a role send
// then runs on within publishDeliveryWindow (confirmed.go). GET
// /v1/sessions, GET /v1/registry/<session_id> and GET /v1/interests/<session_id> read within it as
// well, and every route that writes does its store work within it: subscribe, unsubscribe, POST
// /v1/roles/set, and DELETE of /v1/interests/<session_id> and /v1/sessions/<session_id>. The HTTP
// client gives each attempt 5 s (packages/envoy-client/src/transport.ts) and this listener's HTTP
// write timeout is 10 s (main.go), so answering inside 3.5 s leaves the rest for TLS, scheduling
// and the body: a sender whose bus cannot be reached reads the 503 that names the reason, in time
// to act on it, instead of the timeout it would otherwise retry blind.
const sendRequestBudget = 3500 * time.Millisecond

// sendSessionReadTimeout bounds the send route's read of the target session from the bucket, so a
// bucket that does not answer still leaves most of sendRequestBudget (~2.5 s) to the sender stamp,
// the connection probe and the publish (connectionProbeTimeout says how they share it). The role
// paths keep roleHolderReadTimeout as their per-read cap instead.
const sendSessionReadTimeout = 1 * time.Second

// senderStampReadTimeout bounds senderStamp's reads of the sender's interest record and session
// entry from their buckets, the two together.
// nats.go's KV read takes no context and waits out the JetStream MaxWait (10 s), and the stamp is
// only enrichment, so on a bus that does not answer it gets no more of sendRequestBudget than the
// send route's own read of its target (sendSessionReadTimeout): a send whose two reads both go
// unanswered still leaves its publish 1.5 s, and the publish is what answers the named 503. The
// connection probe shares this second with the stamp (connectionProbeTimeout).
const senderStampReadTimeout = 1 * time.Second

// sendHandler publishes a direct message to a session Envoy knows, and tells its sender what that
// session was when the message was stored. Which answer a target gets turns on two stores, the
// session registry (the entries of the sessions running now, each judged by its age:
// session.StateOf) and the known-session store (every session that registered in the last 30
// days, session.KnownSessions), and on the connection probe of the session's relay subject
// (probeSessionConnection):
//
//   - An entry within the gone horizon whose probe was answered, met silence or failed any other
//     way (probeSessionConnection counts that as connected), or whose entry records no consumer
//     delivery and so is not probed: 200, state connected, recipient_state live or unresponsive
//     by the entry's age.
//   - An entry within the gone horizon whose probe met no responders: 200, state stopped,
//     recipient_state by the entry's age and last_seen the entry's. The probe wins over age.
//   - No entry, or one past the gone horizon, for a stopped Legion agent whose daemon answers for
//     it (handOffToLegion: the agent registry names the session and its project last said it
//     answers, an agent whose idle issue pod the daemon released among them), whether or not a
//     known-session record stands: 200, recipient the session and state empty, the message handed
//     to that daemon, stored for no session and raising no wake report. A repeat under one
//     idempotency key whose hand-off the stream already held is answered that hand-off's
//     envelope, read back as a stored copy is and with duplicate where it is another request's
//     (answerHandedOff), or the stored-copy 503 when it cannot be read back. A connected_only send
//     and one carrying "handoff": false are never handed off.
//   - No entry, or one past the gone horizon, and a known-session record: 200, state stopped,
//     recipient_state gone and last_seen the record's. The message is stored either way, and
//     Envoy keeps it for 72 hours, so a session that runs again under the same id gets it from
//     its own consumer.
//   - Neither: 404 "no live session <id>" (noLiveSession). The wording is load-bearing: Dispatch
//     classifies a delivery failure on that prefix.
//   - A stopped target whose request set connected_only: 409 "the session is stopped", with
//     nothing stored.
//
// The entry is read from the session store itself and not merely from this listener's cache of
// it: only the store's word that the key is gone can make a record-based stopped answer or a 404,
// and a store or a bus that did not answer earns the 503 that asks the sender to try the same
// route again. A target with no entry within the gone horizon that is not handed off raises the
// wake report whatever the record says, exactly where a 404 always raised it; one whose probe met
// no responders raises none, as a lingering entry never did. A target that cannot take the
// payload's delivery mode (deliveryModeVerdict for a session with an entry,
// stoppedDeliveryModeVerdict for one with only a record) is the 403 naming it. Every one of those
// answers is written through record, so this route's one line per request is written with it.
//
// Before any of them, a message of an agent session whose count under the operator's sender limit
// is empty is the 429 refuseSenderRateLimited writes, with nothing read, reported or stored
// (senderLimit.admit). A counted message costs its sender one where the wake is reported, and the
// stream's answer to its publish, or to its hand-off's, marks the rest by the rule the topic
// publish shares (senderCharge.published): a copy the stream stored or may have stored costs one,
// a copy it already held costs nothing, also after the wake, and a publish that sent nothing costs
// only the wake, if one was reported.
//
// A send carrying role, the role a Dispatch send for a route to a role is sent for, is taken only
// from Dispatch's own verified principal (refuseRoleMark): any other caller carrying it is the
// 403 naming the field, before anything is read. Taken, the mark rides the envelope, and the
// role's claim is read before the target (answerRoleSend): a claim naming the target sends as
// above, and a claim naming another session, or none, keeps the message for a session that holds
// the role live and answers 202 kept, and with no live holder hands it to the daemon of the role's
// newest Legion session, finished or not, and answers 200 with that session as recipient
// (handOffToLegion on the role token), or else keeps it for the role's next live holder and
// answers 202 kept (handOffOrKeepRoleSend), so the role's message never lands on the subject of a
// session that no longer holds the role, unless the send asked connected_only, which is the 409 a
// stopped target gives, with nothing kept. A claim read that did not answer is the bus-unavailable
// 503, and so is a resolution of the holder it names that did not answer.
func sendHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record := &requestRecord{logger: logger, line: "listener send", laneKey: "target_session"}
		if r.Method != http.MethodPost {
			record.refuse(w, http.StatusMethodNotAllowed, outcomeInvalidRequest, "method not allowed")
			return
		}
		// Every bus read and the publish below share one deadline derived from the caller's own, so
		// a bus this listener cannot reach is answered as a named 503 inside the caller's attempt
		// rather than left to that caller's timeout.
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		var body listenerwire.MessageBody
		var targetSession string
		var request listenerwire.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "invalid json")
			return
		}
		body = request.MessageBody
		targetSession = strings.TrimSpace(request.TargetSession)
		record.lane = targetSession
		if refusal := sessionNotBound(r, logger, strings.TrimSpace(body.SourceSession)); refusal != "" {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, refusal, "source_session")
			return
		}
		if refuseRoleMark(w, r, d, record, request.Role) {
			return
		}
		// At the edge a send is the session's own: source agent and the token's session
		// (edgeSenderRefusal).
		if refusal := edgeSenderRefusal(r, strings.TrimSpace(body.Source), strings.TrimSpace(body.SourceSession)); refusal != nil {
			record.refuseWith(w, refusal.status, outcomeCapabilityRefused, refusal.body)
			return
		}
		if targetSession == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "target_session is required", "target_session")
			return
		}
		if strings.TrimSpace(body.Message) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "message is required", "message")
			return
		}
		message, field, bodyExpectsReply := validateMessageBody(body)
		if message != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, message, field)
			return
		}
		item := contracts.AgentSendEnvelope(targetSession, body.IdempotencyKey, listenerMessage(body, bodyExpectsReply))
		item.Role = request.Role
		record.item = &item
		if err := item.Validate(); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, err.Error())
			return
		}
		charge, admitted := d.senderLimit.admit(w, r, record, item)
		if !admitted {
			return
		}
		defer charge.settle()
		if answerRoleSend(ctx, w, d, record, charge, &item, senderOf(r, item), request, targetSession) {
			return
		}
		// The cache alone cannot say a session is gone: it trails the bucket it follows, and is
		// still being refilled after a reconnect, so a miss is read through to the bucket itself.
		// Only the bucket's own word for it (nats.ErrKeyNotFound, which Refresh also returns for a
		// key no session can register under) makes a target with no entry; a read that did not
		// answer is the 503 below, which asks the sender to try again instead. A target with no
		// entry within the gone horizon is not gone to this route while the known-session store
		// holds its record, which it answers from memory: only a session neither store knows is
		// the 404 a sender routes around the pane on.
		target, recipientState, err := liveSessionEntry(ctx, d.sessions, targetSession, sendSessionReadTimeout, notGone, d.sessions.Refresh)
		// At the edge a target is one only while the token that registered it lists one of the
		// sender's projects and the stream's leader confirms that record is a writer's
		// (edgeScope.showsConfirmed). Any other target, and one no entry stands for, whose record
		// names no project, is answered as a session no listener has heard of, before a wake is
		// reported or anything is stored: the sender learns nothing of another project's session. A
		// confirmation read that did not answer is this route's bus-unavailable 503, as every other
		// read that did not answer is, never that 404.
		if hidden, confErr := edgeScopeOf(r, nil).hidesConfirmed(ctx, d.sessions, targetSession, target, err); confErr != nil {
			record.refuseBusUnavailable(w, confErr)
			return
		} else if hidden {
			record.refuseWith(w, http.StatusNotFound, outcomeNoLiveSession, unknownSession(targetSession))
			return
		}
		hasEntry := err == nil
		var known session.KnownSession
		if errors.Is(err, nats.ErrKeyNotFound) {
			// A stopped Legion agent whose daemon answers for it takes the message through that
			// daemon (handOffToLegion), ahead of the known-session record below: that record
			// stores the message for a session that runs again, and a stopped Legion agent's
			// daemon answers it now from the agent's own conversation. The sender reads the 200 a
			// delivered message gets. An agent whose idle issue pod the daemon released is handed
			// off too, and its daemon decides whether the message wakes it. A connected-only send
			// is not handed off: it asks for a stopped target to be refused, and a daemon answering
			// in the agent's place is not the agent connected.
			handed, handoffErr := d.handoff.handOffToLegion(ctx, legionHandoffTarget{
				envelope: item, session: targetSession, stampSender: true,
				optedOut: optedOutOfHandoff(body) || request.ConnectedOnly,
			})
			if handoffErr != nil {
				charge.published(bus.StoredAck{}, handoffErr)
				record.refuseBusUnavailable(w, handoffErr)
				return
			}
			if handed != nil {
				charge.published(handed.ack, nil)
				// A repeat whose first send was handed off is answered the hand-off the stream
				// holds, as a repeat this route publishes is answered its stored copy
				// (answerHandedOff, publishHeld).
				duplicate, answerErr := answerHandedOff(ctx, d.client, record, senderOf(r, item), handed, &item)
				if answerErr != nil {
					record.refuseBusUnavailable(w, answerErr)
					return
				}
				record.accept(w, listenerwire.SendResponse{
					Envelope:       item,
					ExpectsReply:   expectsReplyAnswer(body.ExpectsReply, item.ExpectsReply),
					Recipient:      handed.TargetSession,
					RecipientState: recipientState,
					Duplicate:      duplicate,
				})
				return
			}
			var recorded bool
			known, recorded = d.known.Get(targetSession)
			// A send to a stopped session the bus would refuse before sending anything, because
			// the deployed stream does not capture the target's agent subject, is answered that
			// refusal here, before the wake: a sender sends it again at Retry-After: 1 for as long
			// as a narrowing lasts, and a wake for each of those sends would cost it nothing
			// (senderCharge.published) and wake a pod for a message nothing took. A target neither
			// store knows is answered 404 below whatever the stream captures, its wake charged.
			if recorded {
				if refusal := d.client.UndeployedRefusal(item.Topic); refusal != nil {
					record.refuseBusUnavailable(w, refusal)
					return
				}
			}
			// Nothing took the message. A session of a released Legion issue pod is gone from the
			// bucket with its pod, and this answer is the only sight of the message meant for it
			// that no daemon answers: the wake is reported so the pod comes back, whether its
			// record lets the message be stored for it below or the 404 here leaves the sender's
			// next attempt to reach it.
			reportAgentWake(d.wakes, logger, targetSession)
			// The wake is what this message costs its sender, whatever is answered below, but for
			// a publish the bus refused because a narrowing began after the check above, which
			// sent nothing and costs nothing (senderCharge.published).
			charge.woke()
			if !recorded {
				record.refuseWith(w, http.StatusNotFound, outcomeNoLiveSession, noLiveSession(d.sessions, targetSession, target, recipientState))
				return
			}
		} else if err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		// A target with an entry is judged by the entry, one with only a record by the record:
		// the two carry different words for the same capabilities, and only one of them is the
		// target's.
		var verdict forwardVerdict
		if hasEntry {
			verdict = deliveryModeVerdict(targetSession, target, item.Payload)
		} else {
			verdict = stoppedDeliveryModeVerdict(targetSession, known, item.Payload)
		}
		if verdict.refusal != "" {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, verdict.refusal)
			return
		}
		// The connection probe runs on a goroutine of its own while this one stamps, in the second
		// the two share (connectionProbeTimeout). It is sent only for a target whose entry records
		// consumer delivery, whose plugin subscribes its relay subject before it registers: a
		// target with no entry is stopped with nobody asked, and an entry an older listener wrote
		// without the field counts as connected while it holds that entry.
		connected := make(chan bool, 1)
		if hasEntry && target.Delivery == session.DeliveryConsumer {
			go func() { connected <- probeSessionConnection(ctx, d.client, targetSession, logger) }()
		} else {
			connected <- hasEntry
		}
		item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
		state, lastSeen := sessionStateConnected, target.UpdatedAt
		if !hasEntry {
			lastSeen = known.LastSeen
		}
		if !<-connected {
			state = sessionStateStopped
		}
		if request.ConnectedOnly && state == sessionStateStopped {
			record.refuse(w, http.StatusConflict, outcomeSessionStopped, "the session is stopped")
			return
		}
		duplicate, err := publishHeld(ctx, d.client, d.client.PublishAckWithin, record, senderOf(r, item), &item)
		if err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		// Counted here, and only here: the send is accepted and the stream has stored it. A
		// publish the bus refused stored nothing, and a duplicate the stream recognised stored
		// nothing new, so neither is a message now waiting for a session that stopped answering,
		// which is what the counter's name and help say it counts.
		if recipientState == session.Unresponsive && !duplicate {
			d.sendsToUnresponsive.Inc()
		}
		record.accept(w, listenerwire.SendResponse{
			Envelope:       item,
			ExpectsReply:   expectsReplyAnswer(body.ExpectsReply, item.ExpectsReply),
			Recipient:      targetSession,
			RecipientState: recipientState,
			State:          state,
			LastSeen:       lastSeen,
			Duplicate:      duplicate,
		})
	}
}

// deleteSessionHandler removes a live session registration at shutdown, and answers 200 whether or
// not there was one to remove. A caller that cannot afford to remove a registration another
// process wrote names the condition its delete holds under, and then the registration is removed
// only while the stored entry meets it (session.SessionRegistry.DeleteIf), under one
// compare-and-swap at the revision the condition was read at. There are two:
//
//   - client_instance_id names the client instance that is leaving, and the registration is
//     removed only while it carries that id or carries none. A Claude Code /clear handoff
//     deregisters the session it hands off from before it claims that session's role, and the
//     entry under that id can by then be another process's live registration, which the handoff
//     must leave alone instead of clearing the predecessor its claim is then weighed against.
//   - if_not_registered_after names the moment the delete belongs to, in unix milliseconds, and
//     the registration is removed only while it was last written at or before it. It is the
//     Legion daemon's clean stop of a role whose agent process has ended (dispatch://INFRA-392):
//     a death a probe judged, whose moment is the probe's, or a suspension whose stop ended a live
//     process, whose moment is when that stop returned. A relaunch of the claim registers the
//     same session id again, in the very decision that asks for a death's stop or at a later
//     wake or resume of a suspended claim, so a stop that lands after the relaunch has registered
//     must leave that live registration alone. A value that is not a positive whole number of
//     milliseconds is a 400, since a caller that names a condition and has it silently dropped
//     would delete exactly what it asked to keep.
//
// A request naming either condition answers {"deleted":true} for a registration that is gone and
// {"deleted":false} with a reason for one that stayed, both 200: the caller has nothing to retry
// either way. Each outcome that keeps the entry gets its own reason, because they are separate
// facts: one knows another client instance owns the registration, one that the registration is
// newer than the delete, and the last — a delete that lost both compare-and-swaps — read an entry
// this very caller may own and says only that the registration changed under it. A request naming
// no condition at all removes the registration whoever wrote it, and answers 200 with no body, as
// this route always has. The delete runs within sendRequestBudget of the request's own context, so
// one the bus does not answer is the bus-unavailable 503 with Retry-After: 1 inside the caller's
// attempt (writeStoreError).
//
// A registration this route removed takes its session consumer with it
// (sessionDelivery.deleteAfterAnswer), so the consumer holds a slot of the stream's consumer
// cap no longer: after the unconditional delete, and after a conditional one answered
// session.EntryDeleted. The answer is written first and that delete runs after it, off this
// handler's goroutine, because a handler holds its written response until it returns: the
// delete waits on the session's lock with no bound of its own and then makes the bucket read
// that confirms the session is gone, the delete, which records the consumer's resume floor
// first, and the repair's own calls under a budget of its own, after a KV delete that may already
// have spent sendRequestBudget, so a deregistration that succeeded would read as failed to a
// caller whose attempt ends at 5 s (LISTENER_CALL_TIMEOUT_MS). The shutdown waits for a delete
// still running before it drains the connection it runs on, and one the listener was too late to
// start is logged and left to the sweep (sessionDelivery.awaitDeletes).
//
// Which consumer that delete removes is settled under the session's lock rather than by the
// outcome above: it reads the session bucket again there, from the leader of that bucket's
// stream, and deletes only where the bucket holds no record at all
// (sessionDelivery.deleteConsumer). So an outcome that keeps the entry keeps the consumer, a
// live registration standing behind it — another client instance's, a later one's, or the one a
// lost race read — and so does a registration that landed between this route's own delete and
// that lock: the relaunch behind an if_not_registered_after stop, whose entry this route read
// before the relaunch wrote its own. That registration's own write is what the read must see,
// which is why it is the leader's: a direct get of a replicated bucket is answered by any copy
// near enough to current, and one that has not applied the write yet would answer not found for
// a session that is live. A registration another listener landed between that read and the
// delete itself is given its consumer back there and then: the delete reads the record again
// once it has removed the consumer and creates it for a session whose record is there, at the
// floor it read before removing it (sessionDelivery.repairRegistered). The request answers
// exactly as it did before whether that delete ran, stood down or failed: the entry is gone,
// which is what the caller asked for, and the listener's sweep and the server's inactivity
// threshold each reach a consumer left behind.
//
// A request presenting a broker access token may remove only the registration of the session
// that token names, and is refused 403 for any other (sessionNotBound): this route acts as the
// session it names. The Claude Code /clear handoff still deregisters its predecessor, since it
// does so while it is still that session.
//
// A delete that removed an entry this listener's cache held moves the session's known-session
// record on, keeping its topics (session.KnownSessions.TouchWithin, which writes whatever the
// refresh-age throttle would say): the session stopped cleanly, and a publish on a topic it
// followed still counts it as a recipient, since Envoy keeps its messages for 72 hours and it can
// run again under the same id. A delete that removed nothing leaves the record alone, and a known
// store whose bucket this listener has not opened does nothing at all; neither outcome changes
// this answer.
func deleteSessionHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if refusal := sessionNotBound(r, logger, sessionID); refusal != "" {
			writeJSONError(w, http.StatusForbidden, refusal, "session_id")
			return
		}
		conditions := session.DeleteConditions{ClientInstanceID: strings.TrimSpace(r.URL.Query().Get("client_instance_id"))}
		if asked := strings.TrimSpace(r.URL.Query().Get("if_not_registered_after")); asked != "" {
			millis, err := strconv.ParseInt(asked, 10, 64)
			if err != nil || millis <= 0 {
				writeJSONError(w, http.StatusBadRequest,
					"if_not_registered_after must be a unix timestamp in milliseconds", "if_not_registered_after")
				return
			}
			conditions.NotRegisteredAfter = millis
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		// Whether the delete removes an entry is read before it runs: Delete is idempotent and
		// EntryDeleted counts a key that was already absent, so neither answer tells a stop from
		// a repeat of one. A session this listener never held is left alone, with no write. The
		// read is of this listener's session cache, whose only error is a key it does not hold
		// (session.SessionRegistry.Get), and that cache trails the bucket: a session registered
		// through another listener, or one this cache has not caught up with since its
		// connection came back, reads here as one this listener never held and is left alone
		// too. That is deliberate - the record of such a session is the owning listener's to
		// move on, and one nobody moves on ages out of the 30 days on its own, which no delivery
		// depends on.
		_, entryErr := d.sessions.Get(sessionID)
		held := entryErr == nil
		recordStop := func() {
			if !held {
				return
			}
			if err := d.known.TouchWithin(ctx, sessionID); err != nil {
				logger.Warn("known-session record not moved on at a clean stop",
					slog.String("session_id", sessionID), slog.String("error", err.Error()))
			}
		}
		// The consumer's delete is made outside the request's own context, and after the
		// answer: the entry is gone whatever the caller does next, a plugin that goes away as
		// its deregistration answers - the shutdown this route mostly serves - must not leave
		// the consumer behind for the sweep, and no caller waits on it.
		deleting := context.WithoutCancel(r.Context())
		if conditions == (session.DeleteConditions{}) {
			// A session the registry no longer holds (nats.ErrKeyNotFound) is the success this
			// route has always answered, and it leaves no registration standing either, so its
			// consumer goes too.
			if err := d.sessions.Delete(ctx, sessionID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeStoreError(ctx, w, err, http.StatusInternalServerError)
				return
			}
			recordStop()
			w.WriteHeader(http.StatusOK)
			d.consumers.deleteAfterAnswer(deleting, d, sessionID)
			return
		}
		outcome, err := d.sessions.DeleteIf(ctx, sessionID, conditions)
		if err != nil {
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
			return
		}
		switch outcome {
		case session.EntryDeleted:
			recordStop()
			writeJSON(w, http.StatusOK, listenerwire.DeleteSessionResponse{Deleted: new(true)})
			d.consumers.deleteAfterAnswer(deleting, d, sessionID)
		case session.EntryKeptForAnotherInstance:
			writeJSON(w, http.StatusOK, listenerwire.DeleteSessionResponse{
				Deleted: new(false),
				Reason:  "the registration belongs to another client instance",
			})
		case session.EntryKeptForALaterRegistration:
			writeJSON(w, http.StatusOK, listenerwire.DeleteSessionResponse{
				Deleted: new(false),
				Reason:  "the registration was written after the moment the delete belongs to",
			})
		default:
			// session.EntryKeptAfterLostRace: the entry met every condition and both
			// conditional deletes lost to a write that landed under them, this caller's own
			// heartbeat among the likeliest, so the only true thing to say is that it stayed
			// and changed. session.EntryUnanswered never reaches here: it comes with the
			// error answered above.
			writeJSON(w, http.StatusOK, listenerwire.DeleteSessionResponse{
				Deleted: new(false),
				Reason:  "the registration changed while it was being removed",
			})
		}
	}
}

// publishDedupePrefix opens every dedupe key the publish route makes for a request that chose
// none: publish.<a new id>, or publish.<the request's idempotency_key>. Three places use it: the
// route, which writes it (publishHandler) and refuses a dedupe key chosen for an envelope with
// source dispatch, so every copy it keeps of a dispatch envelope carries it; a takeover's copy of
// such a message, whose own key keeps it (takenFrameDedupeKey); and isOutboxCopy, which reads it
// to tell such a copy from one Dispatch's outbox kept under a key of its own.
const publishDedupePrefix = "publish."

// publishHandler rejects agent-targeted topics (must use /v1/messages/send
// instead) and the listener's own lanes, the kept one and the session resume one, and sends the
// envelope. A role
// topic is forwarded to its holder, answered once confirmed or when
// publishDeliveryWindow from the request's arrival has passed (sendToRole in
// confirmed.go); a topic the stream captures is answered once the stream
// stored it, with the counts of the sessions it is for (recipientCounts), and any other
// notifications. topic is refused by name, since nothing would store it for a session to read. An
// explicit dedupe_key is used verbatim (a re-send a receiver's own dedupe
// recognises); it is mutually exclusive with idempotency_key, may not begin
// with roleForwardDedupePrefix, the mark the role arbiter drops on sight, and
// may not ride a dispatch envelope. Every host drops a repeat of any dispatch key
// (contracts.DedupeKeyNamesTheUpstreamEvent), and Dispatch's outbox keys are
// sequential (dispatch-<event id>), so a caller that chose one could make a
// host drop the real event it names. Dispatch itself never needs this route's
// key: its outbox publishes to the bus directly and its sends go through
// /v1/messages/send, whose key is the listener's. Like the send route, it answers through record,
// which writes this route's one line per request with the answer; a confirmed send answers through
// it too.
//
// A publish to a topic the stream captures that the stream answers as a duplicate - a repeat whose
// dedupe key names its event (contracts.DedupeKeyNamesTheUpstreamEvent), such as one under the
// same idempotency_key the shared transport mints, which the route makes the minted dedupe key
// publish.<key>, and so the same message id - stored nothing new, and is answered 200 with
// duplicate and the copy the stream holds, read back at the sequence the verdict named
// (publishHeld), as the send route answers its own repeat: never the envelope built for the
// repeat, whose event_id no stored message carries. A copy that cannot be read is the 503 with
// Retry-After: 1 under storedCopyUnreadablePrefix.
//
// After the refusal of a topic no stream subject captures, a message of an agent session whose
// count under the operator's sender limit is empty is the 429 refuseSenderRateLimited writes, with
// no holder resolved, nothing kept, no wake reported and nothing stored (senderLimit.admit). The
// stream's answer to the publish of a counted message to a topic the stream captures marks what it
// costs its sender, by the rule the send route shares (senderCharge.published): a copy the stream
// stored or may have stored costs one, and a copy it already held, or a publish that sent nothing,
// costs nothing. A role send marks its own (roleSend).
//
// keep is this route's own request field, meaningful only for a role topic, and unset means true:
// a role message the listener cannot deliver because no live session holds the role - no claim,
// or a claim whose holder is not live - is kept on the role's kept lane for an hour and handed to
// the role's next live holder, and the send is answered 202 kept, with the copy's kept_until,
// rather than 404 (roleSend.kept). A sender that holds a queue of its own and must get the 404
// passes keep: false, which keeps nothing and answers 404 unclaimed or holder_lapsed. It is read by
// this route's give-up alone (roleSend.noHolder) and rides no envelope, which travels on to the
// holder and back in the answer. A role token no claim can name keeps nothing either, which the
// keep itself refuses, for this route and the role lane alike (keepForNextHolder).
func publishHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		record := &requestRecord{logger: logger, line: "listener publish", laneKey: "topic"}
		if r.Method != http.MethodPost {
			record.refuse(w, http.StatusMethodNotAllowed, outcomeInvalidRequest, "method not allowed")
			return
		}
		var request listenerwire.PublishRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "invalid json")
			return
		}
		// The topic is recorded as the envelope will carry it, untrimmed, so a request refused for
		// the spelling of its topic records the spelling that was refused.
		record.lane = request.Topic
		if refusal := sessionNotBound(r, logger, strings.TrimSpace(request.SourceSession)); refusal != "" {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, refusal, "source_session")
			return
		}
		// At the edge a publish is the session's own, as a send is (edgeSenderRefusal).
		if refusal := edgeSenderRefusal(r, strings.TrimSpace(request.Source), strings.TrimSpace(request.SourceSession)); refusal != nil {
			record.refuseWith(w, refusal.status, outcomeCapabilityRefused, refusal.body)
			return
		}
		if strings.TrimSpace(request.Topic) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "topic is required", "topic")
			return
		}
		if strings.TrimSpace(request.Message) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "message is required", "message")
			return
		}
		// At the edge a session publishes a role subject of its project and nothing else
		// (edgePublishRefusal), decided before any other rule of this route so that what it may
		// not publish is refused with the edge's reason whatever else is wrong with it.
		if refusal := edgePublishRefusal(r, request.Topic); refusal != nil {
			record.refuseWith(w, refusal.status, outcomeNotInProject, refusal.body)
			return
		}
		if !strings.HasPrefix(request.Topic, bus.NotificationTopicPrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "topic must begin with "+bus.NotificationTopicPrefix, "topic")
			return
		}
		if strings.HasPrefix(request.Topic, contracts.AgentTopicPrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "cannot publish to agent topics; use /v1/messages/send for direct agent messages")
			return
		}
		// The wake subject is this listener's own report of a released Legion pod, and a Legion
		// daemon acts on what it reads there: it wakes the pod a role token or a session names,
		// and it holds its whole release gate open on a `listener_ready` report's clock. A caller
		// of this route chooses every field of the envelope and the payload, source "envoy"
		// included, so a session that could publish here — a prompt-injected one among them —
		// could wake pods at will and keep that gate open with a report dated in the future. Only
		// the listener publishes it, and it does so directly (wake.go).
		if strings.TrimSpace(request.Topic) == bus.LegionWakeTopic {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest,
				"cannot publish to "+bus.LegionWakeTopic+"; the listener alone reports a Legion pod wake", "topic")
			return
		}
		// A lane of the listener's own is refused, and named in the refusal as the stream itself
		// spells it, by the one call that covers every one of them (bus.ListenerOwnLaneOf), so a
		// lane the stream gains is refused and named here with
		// nothing written out a second time. What each holds says why: the kept lane holds the
		// copies the listener keeps for a role's next holder and the cursors of its drains, so
		// a bearer publishing there could rewind a role's drain or put a copy of its own on the
		// lane; the resume lane holds its record of where a deleted session consumer stopped,
		// which is where that session's consumer is created again, so one publishing there
		// would choose where another session's delivery resumes and make it miss every message
		// stored before that floor or read again what it has read.
		if lane := bus.ListenerOwnLaneOf(request.Topic); lane != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest,
				"cannot publish to "+lane+" topics; that lane is the listener's own, holding the copies it keeps for a role's next holder with the cursors of its drains, or its record of where a deleted session consumer stopped, and the listener alone writes it", "topic")
			return
		}
		message, field, bodyExpectsReply := validateMessageBody(request.MessageBody)
		if message != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, message, field)
			return
		}
		if request.DedupeKey != "" && request.IdempotencyKey != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key and idempotency_key are mutually exclusive", "dedupe_key", "idempotency_key")
			return
		}
		if strings.HasPrefix(request.DedupeKey, roleForwardDedupePrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key must not begin with the reserved prefix "+roleForwardDedupePrefix, "dedupe_key")
			return
		}
		if request.DedupeKey != "" && request.Source == "dispatch" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key cannot be chosen for a dispatch envelope", "dedupe_key")
			return
		}
		dedupeKey := publishDedupePrefix + id.New()
		switch {
		case request.DedupeKey != "":
			dedupeKey = request.DedupeKey
		case request.IdempotencyKey != "":
			dedupeKey = publishDedupePrefix + request.IdempotencyKey
		}
		item := contracts.MessageEnvelope(listenerMessage(request.MessageBody, bodyExpectsReply), request.Topic, dedupeKey)
		record.item = &item
		if err := item.Validate(); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, err.Error())
			return
		}
		// Every topic that would deliver to no session is refused by name: a session reads
		// only what its own consumer holds, so such a publish would reach nobody, now or later;
		// nothing retries into success, so it is the 400 the refusals above answer rather than a
		// 504. Team topics are the namespace an agent names itself and the stream stores them
		// (bus.StreamSubjects). The role lanes bus.ReachesNoSession excepts are forwarded below.
		// It comes before the sender limit's take, so such a publish keeps its 400 at any count.
		if d.client.ReachesNoSession(request.Topic) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest,
				"no stream subject captures "+request.Topic+", so no session would receive it; publish to a notifications.team.<name> topic instead", "topic")
			return
		}
		// A Legion role named under a project it does not work for is refused as POST
		// /v1/roles/set refuses its claim (roleLaneProjectError): its claim is keyed on the bare
		// name, so the send would reach the holder of the role's own project. It comes before the
		// sender limit's take, as every other refusal of the request's own fields does.
		if lane, isRole := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); isRole {
			if err := roleLaneProjectError(lane); err != nil {
				record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, err.Error(), "topic")
				return
			}
		}
		charge, admitted := d.senderLimit.admit(w, r, record, item)
		if !admitted {
			return
		}
		defer charge.settle()
		if lane, ok := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); ok {
			// The lane names its role in either form while every reader takes both: its project
			// and then the role, or the role alone. The claim this send resolves is the one that
			// name is keyed by (store.RoleClaimKeyOf, which store.Registry.ReadRole answers): the
			// bare name, or <project>.<role> for a free-form role under a project, never the
			// lane's tokens as they stand. A lane naming a valid role in the old form is counted
			// and named back in the answer's warnings with its new form (store.Registry.ReadRole):
			// this publisher is one the old form's refusal would stop.
			role, roleWarning := d.registry.ReadRole(lane, roles.Valid)
			var warnings []string
			if roleWarning != "" {
				warnings = []string{roleWarning}
			}
			ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(publishDeliveryWindow))
			defer cancel()
			// The request's keep flag is read here and nowhere else: it belongs to this route's
			// give-up (roleSend.noHolder) rather than to the envelope, which travels on to the
			// holder and back in this route's own answer. No flag at all means keep, so every
			// publisher that sets none is unchanged.
			sendToRole(ctx, w, edgeScopeOf(r, nil), record, d, item, senderOf(r, item), request.ExpectsReply, role, request.Keep == nil || *request.Keep, optedOutOfHandoff(request.MessageBody), arrived, warnings)
			return
		}
		// The counts are of the moment the message is published, and are read from memory alone
		// for every topic but a controller notice lane, whose role claim they read once
		// (recipientCounts), so taking them here costs any other publish below no JetStream
		// request. Only this branch counts: a role send names its holder, and a refused topic
		// reaches nobody.
		recipients, connected := recipientCounts(r.Context(), d, item.Topic, item.SourceSession)
		// The sender stamp's read and the publish take this one deadline, derived from the
		// caller's own context, so this route too answers a bus it cannot reach rather than
		// spending the caller's attempt.
		ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(sendRequestBudget))
		defer cancel()
		item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
		duplicate, err := publishHeld(ctx, d.client, d.client.PublishAckWithin, record, senderOf(r, item), &item)
		if err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		record.accept(w, listenerwire.PublishResponse{
			Envelope:            item,
			ExpectsReply:        expectsReplyAnswer(request.ExpectsReply, item.ExpectsReply),
			Recipients:          recipients,
			RecipientsConnected: connected,
			Duplicate:           duplicate,
		})
	}
}

// sessionInterestReadTimeout bounds sessionInfoFor's read of one session's interest record from
// the bucket, which a cache miss makes (store.Registry.Get bounds that read alone): a bucket that
// does not answer is reported a second in, rather than when the route's sendRequestBudget ends,
// and GET /v1/registry/<session_id>, whose session read may already have spent
// roleHolderReadTimeout (2 s), still answers inside that budget.
const sessionInterestReadTimeout = 1 * time.Second

// sessionInfoFor joins one session entry with its interest record's topics and the roles whose
// claims name it (store.Registry.RolesHeldBy), so a row of GET /v1/sessions and the single answer
// of GET /v1/registry/<session_id> are one shape built one way. A session holds a role exactly
// when the role's claim names it: a role topic its record keeps from a claim it lost gives it no
// role, and a claim it holds is reported whatever its record's topics are, or with no record at
// all. A claims view that is not complete (store.ErrRoleViewNotReady: still scanning, refilling a
// recreated bucket, or its watcher ended) is returned alone, as a read that did not answer, never
// as a session holding no roles. A session the interest registry holds no key for keeps the
// empty topics list sessionInfoWith gives it, never null: a session that registered and
// subscribed to nothing is not an error here, and a record whose topics are null leaves that list
// in place. The other answers about the record (interestReadAnswered) are returned with the
// session's info, its topics the empty list, so the caller can still show the session and name
// what the read answered. Every other error is a read that did not answer and is returned alone:
// a caller answers it as the bus-unavailable 503 and never as a row.
func sessionInfoFor(ctx context.Context, registry *store.Registry, entry session.ListEntry) (listenerwire.SessionInfo, error) {
	held, err := registry.RolesHeldBy(entry.SessionID)
	if err != nil {
		return listenerwire.SessionInfo{}, err
	}
	info := sessionInfoWith(entry, held)
	readCtx, cancel := context.WithTimeout(ctx, sessionInterestReadTimeout)
	defer cancel()
	interest, err := registry.Get(readCtx, entry.SessionID)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return info, nil
	case interestReadAnswered(err):
		return info, err
	case err != nil:
		return listenerwire.SessionInfo{}, err
	}
	if interest.Topics != nil {
		info.Topics = interest.Topics
	}
	return info, nil
}

// sessionInfoWith is the row entry makes, holding the roles it is given, and topics as the empty
// list, never null: the one shape both sessionInfoFor and listedSessionInfo fill in, each with
// the topics of the interest record its route may read. The entry's state is what its last
// registration says about the session, which every row carries so that a caller reading a list,
// or one session's answer, can tell a session that is answering from one that has stopped without
// comparing timestamps itself.
func sessionInfoWith(entry session.ListEntry, held []string) listenerwire.SessionInfo {
	return listenerwire.SessionInfo{
		SessionID:        entry.SessionID,
		State:            entry.State,
		MachineID:        entry.MachineID,
		Host:             entry.HostOrMachine(),
		TmuxPlace:        entry.TmuxPlace,
		Dir:              entry.Dir,
		Port:             entry.Port,
		Title:            entry.Title,
		Capabilities:     append([]string{}, entry.Capabilities...),
		SelfSubscribed:   entry.SelfSubscribed,
		Topics:           []string{},
		Roles:            held,
		UpdatedAt:        entry.UpdatedAt,
		LastSeen:         entry.UpdatedAt,
		Plugin:           entry.Plugin,
		PluginVersion:    entry.PluginVersion,
		Delivery:         entry.Delivery,
		ClientInstanceID: entry.ClientInstanceID,
	}
}

// interestReadAnswered reports whether err, from a read of an interest record other than the
// bucket's word that it holds no key (nats.ErrKeyNotFound), is an answer about that record, which
// a retry reads again: a record this build cannot decode (store.ErrMalformedInterest, the 500
// writeInterestRecordUnreadable writes), or a key the interest bucket refuses (bus.ErrRefused: a
// legacy id past envoy_interests' get bound that envoy_sessions still lists, or one outside
// nats.go's key alphabet, which GET /v1/interests/<session_id> answers as the 413 or 400
// writeBusUnavailable writes and the two session routes as a row whose topics are []). Any other
// error is a read that did not answer, whatever it names. In nats.go v1.55.0 that includes ctx's
// end (store.Registry.Get's bound, or the JetStream MaxWait context nats.go's direct get always
// runs under, so it always takes requestWithContext), nats.ErrConnectionClosed,
// nats.ErrDisconnected, nats.ErrNoResponders, and whatever nats.go's publish returns, the raw *net.OpError of a write
// buffer flushed onto a broken connection among them; the rule does not rest on that list being
// complete.
func interestReadAnswered(err error) bool {
	return errors.Is(err, store.ErrMalformedInterest) || errors.Is(err, bus.ErrRefused)
}

// writeInterestRecordUnreadable answers a read of one session's interest record that this build
// cannot decode (store.ErrMalformedInterest), for GET /v1/registry/<session_id> and GET
// /v1/interests/<session_id>. The bus answered, and will answer the same record again, so this is
// not the bus-unavailable 503: it is a 500 with no Retry-After, naming the session and the record,
// as writeRoleClaimUnreadable answers an unreadable role claim.
func writeInterestRecordUnreadable(w http.ResponseWriter, sessionID string, err error) {
	writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
		"envoy: the interest record of session %s is unreadable: %s", sessionID, err,
	))
}

// listedSessionInfo is sessionInfoFor for one row of GET /v1/sessions: its topics come from the
// interest copy alone (store.Registry.Cached), never the read-through Get, so a list makes no
// per-key bucket read, which over a slow KV leader would hold the answer for seconds per row.
// listedSessions has already checked that the copy can be served, so a session it holds no
// interest for subscribed to nothing, and its row carries topics as the empty list
// sessionInfoWith gives it, as does one whose record's topics are null. The roles are the ones
// whose claims name the session, as every other answer's are (sessionInfoFor); the list reads
// them for every row in one pass (store.Registry.RolesBySession), and a claims view that cannot
// answer refuses that pass, as a read that did not answer, before any row is built.
func listedSessionInfo(registry *store.Registry, entry session.ListEntry, held []string) listenerwire.SessionInfo {
	if held == nil {
		// A session no claim names is absent from that pass's map, and its row says "no roles"
		// with an empty list, as a per-session read does; a null there would be a changed answer
		// for every session that holds no role.
		held = []string{}
	}
	info := sessionInfoWith(entry, held)
	if interest, found := registry.Cached(entry.SessionID); found && interest.Topics != nil {
		info.Topics = interest.Topics
	}
	return info
}

// listedSessions is the one listing of sessions, which GET /v1/sessions answers and the
// envoy_active_sessions gauge counts: the session copy's rows, once both copies a row is built from
// can be served. A row joins the interest copy for its topics, so a session copy that can be
// served beside an interest copy that cannot would answer rows stripped of their subscriptions.
// Servable reads each watcher's state only, never a cache, so the check takes no cache lock. Its
// error is kvwatch.ErrRefilling while either copy cannot be served (writeListRefilling).
//
// It lists the live sessions, and with include the unresponsive ones too
// (session.IncludeUnresponsive). The default stays live-only because almost every caller of this
// route is choosing a session to address now; a caller that must see, or route to, a session that
// has stopped answering asks for it by name.
func listedSessions(registry *store.Registry, sessions *session.SessionRegistry, include ...session.ListOption) ([]session.ListEntry, error) {
	if err := registry.Servable(); err != nil {
		return nil, err
	}
	if err := sessions.Servable(); err != nil {
		return nil, err
	}
	return sessions.List(include...)
}

// listedSessionCount is how many live rows listedSessions would answer, counted under the session
// copy's shared lock with no list built: the gauge wants the number alone (registerListGauges),
// and it counts the sessions that are answering, never the stalled ones a caller may still
// address. It refuses for the same reasons a list does, so a copy that cannot be served records
// no sample rather than a count of part of a bucket.
func listedSessionCount(registry *store.Registry, sessions *session.SessionRegistry) (int, error) {
	if err := registry.Servable(); err != nil {
		return 0, err
	}
	if err := sessions.Servable(); err != nil {
		return 0, err
	}
	return sessions.LiveCount(), nil
}

// includes reports whether the include selector both session routes take names value. The
// selector is a comma-separated list of states asked for beside the live sessions, and an unknown
// value is ignored rather than refused: a caller asking for something this build does not know
// still gets the live sessions, which is what it would have got without the selector.
//
// include=unresponsive asks for the sessions that have stopped answering beside the live ones.
// stopped, which only GET /v1/registry/<session_id> takes, asks for a session the registry holds
// no entry for within the accepted states, and that the known-session store still knows, to be
// answered 200 with that record (listenerwire.StoppedSessionInfo) rather than 404. Dispatch's
// delivery asks for it and nothing else does, so an issue claim's holder, a role resolution and
// the session list never see a stopped session; GET /v1/sessions ignores it, as it ignores every
// value it does not know.
func includes(r *http.Request, value string) bool {
	return slices.Contains(strings.Split(r.URL.Query().Get("include"), ","), value)
}

// sessionsHandler answers GET /v1/sessions with listedSessions, enriching each row from the
// interest copy and the role claims alone (listedSessionInfo). Each row carries the state its
// last registration puts it in and that registration as last_seen, and with include=unresponsive
// the sessions that have stopped answering are listed too. Each row's roles are those whose
// claims name the session; a claims view that cannot answer is a read that did not answer and
// ends the request with the bus-unavailable 503. While either copy cannot be served it answers
// the refilling 503 (writeListRefilling), never a list that could be missing a live agent.
// repositories is the edge listener's repository reader, nil on every other listener: at the edge
// a row shows only the topics of the token's projects (edgeShownTopics).
func sessionsHandler(registry *store.Registry, sessions *session.SessionRegistry, repositories *edgeRepositories) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var include []session.ListOption
		if includes(r, string(session.Unresponsive)) {
			include = append(include, session.IncludeUnresponsive())
		}
		entries, err := listedSessions(registry, sessions, include...)
		if errors.Is(err, kvwatch.ErrRefilling) {
			writeListRefilling(w)
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		held, err := registry.RolesBySession()
		if err != nil {
			writeBusUnavailable(w, err)
			return
		}
		dirFilter := r.URL.Query().Get("dir")
		titleFilter := r.URL.Query().Get("title")
		result := make([]listenerwire.SessionInfo, 0, len(entries))
		// At the edge the list holds the sessions of the token's projects alone, each with the
		// roles and the topics of those projects alone (edgeScope), each session shown confirmed by
		// its stored record (edgeScope.showsConfirmed), every read inside one bound, so a list the
		// leader did not answer is this route's bus-unavailable 503 inside that bound, never a
		// session silently dropped from the list.
		scope := edgeScopeOf(r, repositories)
		ctx, cancel := context.WithTimeout(r.Context(), roleHolderReadTimeout)
		defer cancel()
		for _, entry := range entries {
			if !strings.Contains(entry.Dir, dirFilter) || !strings.Contains(entry.Title, titleFilter) {
				continue
			}
			shown, err := scope.showsConfirmed(ctx, sessions, entry.SessionID, entry.SessionEntry)
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			if !shown {
				continue
			}
			info := listedSessionInfo(registry, entry, scope.roles(held[entry.SessionID]))
			info.Topics = scope.topics(entry.SessionID, info.Topics)
			result = append(result, info)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// sessionRegistrationView is the one value the view selector of GET /v1/registry/<session_id>
// takes: the registration-only answer writeSessionRegistration writes. Any other value is
// refused 400 naming the selector, as if_not_registered_after refuses a value it cannot read
// (deleteSessionHandler), because a caller whose selector was silently dropped would be handed
// the heavy answer it asked not to have and would read that answer's 503 as its own session's.
const sessionRegistrationView = "registration"

// writeSessionRegistration answers GET /v1/registry/<session_id>?view=registration: the stored
// registration alone, for the caller that names its client instance in a delete that follows
// (the Legion daemon's clean stop of a dead role). It answers whenever the session bucket
// answers, because it makes no other read: no claims view is read and no interest record, where
// the full answer reads both (sessionInfoFor) and so turns a claims view that is still refilling
// into a 503 and a record this build cannot decode into a 500.
//
// The entry is read from the session bucket stream's leader and never from this listener's cache
// or a copy of the bucket (bucketSessionEntry), for the reason that function's own comment gives:
// a cached entry can carry a client instance id the session has since replaced, or none where the
// bucket has one, and the one caller of this view decides a delete on that id. A replaced id would
// have the listener keep the stop as another client instance's, which is the symptom this view is
// here to end.
//
// No state filter applies: every entry the bucket holds is answered 200 with the state its own
// record puts it in, gone included, so include=unresponsive decides nothing under this view. The
// caller is deciding whether to remove a registration, and a registration that exists is one it
// may remove whatever state it reads; a filter here would withhold exactly the records a stop is
// owed for.
//
// A key NATS refuses (bus.ErrRefused) is tested first, as the full view tests it first and for
// the same reason: the read reports a refused key as a missing one too, and an id NATS will never
// hold is the caller's own input, answered as the 413 or 400 writeBusUnavailable gives it. The
// bucket's word that it holds no record is the 404 the full view writes (noLiveSession), with
// session.Gone as the state a session with no record is in. Every other error is a read that did
// not answer, the bus-unavailable 503 with Retry-After: 1.
func writeSessionRegistration(ctx context.Context, w http.ResponseWriter, r *http.Request, d *listenerDeps, sessionID string) {
	entry, err := bucketSessionEntry(ctx, d.sessions, sessionID, roleHolderReadTimeout)
	hidden, hideErr := edgeScopeOf(r, nil).hidesConfirmed(ctx, d.sessions, sessionID, entry, err)
	switch {
	case errors.Is(err, bus.ErrRefused):
		writeBusUnavailable(w, err)
	case hideErr != nil:
		// The edge confirms a session it would show by its stored record (edgeScope.showsConfirmed);
		// a read that did not answer is the bus-unavailable 503, never the 404 of a session it does
		// not show.
		writeBusUnavailable(w, hideErr)
	case hidden:
		// At the edge a session it does not show, and one with no record, which names no
		// project, is answered as one no listener has heard of.
		writeJSON(w, http.StatusNotFound, unknownSession(sessionID))
	case errors.Is(err, nats.ErrKeyNotFound):
		writeJSON(w, http.StatusNotFound, noLiveSession(d.sessions, sessionID, entry, session.Gone))
	case err != nil:
		writeBusUnavailable(w, err)
	default:
		writeJSON(w, http.StatusOK, listenerwire.SessionRegistration{
			SessionID:        sessionID,
			State:            d.sessions.EntryState(entry),
			LastSeen:         entry.UpdatedAt,
			UpdatedAt:        entry.UpdatedAt,
			ClientInstanceID: entry.ClientInstanceID,
		})
	}
}

// writeUnshownSession answers sessionID as a session no listener has heard of when the scope does
// not show it, confirmed by the record the stream's leader holds for its key
// (edgeScope.showsConfirmed): a record the server wrote, or one a cached entry still shows under
// projects a newer write dropped, is a session the edge never heard of. A confirmation read that
// did not answer is the bus-unavailable 503, never that 404. It reports whether it wrote the
// answer, so a read route calls it unconditionally and returns when it did; a nil scope shows every
// session, writes nothing and costs no read. entry is the cached entry sessionID names.
func writeUnshownSession(ctx context.Context, w http.ResponseWriter, scope *edgeScope, sessions *session.SessionRegistry, sessionID string, entry session.SessionEntry) (written bool) {
	shown, err := scope.showsConfirmed(ctx, sessions, sessionID, entry)
	if err != nil {
		writeBusUnavailable(w, err)
		return true
	}
	if !shown {
		writeJSON(w, http.StatusNotFound, unknownSession(sessionID))
		return true
	}
	return false
}

// claimRefreshProjects is the projects a role claim's refresh writes on the claimant's session
// entry (session.SessionEntry.Projects, through PutWithin), keeping the stored record's projects
// only where that record is the session's own registration, and taking the claim token's projects
// otherwise, so a forged record's projects are never written back as this session's own:
//   - the stored record names sessionID (entry.SessionID, a cheap pre-check on the cached entry),
//     and the stream's leader confirms its newest message is a writer's that names sessionID
//     (session.SessionRegistry.StoredByWriter), not a RePublish of another stream the server stored
//     at the key, which session.DecodeEntry can read as a record of this very key naming any
//     projects. Both "names its key" and "is a writer's" are judged on the one record the leader
//     returns, never on the direct get the pre-check reads, so a writer's write landing between the
//     two reads cannot leave a RePublished record's projects standing. Every listener confirms, not
//     the edge alone: once an in-house listener writes a RePublished record's projects back as a
//     registration, the edge can no longer tell it from one (edgeScope.showsConfirmed).
//   - otherwise - a record that names another key, one an earlier build wrote naming no key, one the
//     server wrote, and a claim with no broker token - the projects of the token this claim was made
//     with (registeringProjects), [] for a claim that presented none, which the edge shows no
//     project on.
//
// The error is the confirmation read that did not answer, which the caller answers as the
// bus-unavailable 503, so a hiccup never wipes a session's projects. clearForged is true when the
// cached entry names sessionID but the stream leader holds no writer's record for it (the RePublish
// case above): its non-project fields may be a RePublish's too, so the caller carries back none of
// them.
func claimRefreshProjects(ctx context.Context, sessions *session.SessionRegistry, r *http.Request, sessionID string, entry session.SessionEntry) (projects []string, clearForged bool, err error) {
	if entry.SessionID == sessionID {
		readCtx, cancel := context.WithTimeout(ctx, roleHolderReadTimeout)
		confirmed, ok, readErr := sessions.StoredByWriter(readCtx, sessionID)
		cancel()
		if readErr != nil {
			return nil, false, fmt.Errorf("confirm role claimant registration: %w", readErr)
		}
		if ok && confirmed.SessionID == sessionID {
			return confirmed.Projects, false, nil
		}
		return registeringProjects(r), true, nil
	}
	return registeringProjects(r), false, nil
}

// sessionEntryHandler answers GET /v1/registry/<session_id>, the one question a caller holding a
// session id asks before it addresses that session: is this session live, and how is it named? The
// read goes through liveSessionEntry, so a session this listener's cache does not hold yet — the
// session bucket was recreated and the watcher is still refilling it, or another listener
// registered the session a moment ago — is read from the bucket itself instead of being reported
// gone.
//
// A session past the live window is answered 404 as it always was, with the state its record puts
// it in and that record's last registration added (listenerwire.NoLiveSessionRefusal), unless the
// caller asks for it with include=unresponsive, which answers it 200 with state "unresponsive".
// The 404's wording is load-bearing: Dispatch classifies a delivery failure on the "no live
// session " prefix, as the send route's own 404 writes it, so the two new fields go beside that
// error and an older Dispatch reading only the prefix is unaffected.
//
// An id the session bucket refuses as a key is the caller's to fix, the 413 or 400
// writeBusUnavailable writes. A live session whose id only the interest bucket refuses (a legacy id
// past envoy_interests' get bound that envoy_sessions still serves, or one outside nats.go's key
// alphabet) and that this listener's interest copy holds no record for is answered 200 with
// topics as the empty list, as GET /v1/sessions lists it, and logged at WARN: Dispatch reads a
// 413 or 400 here as a session that cannot exist. A read that did not answer — the session's,
// its interest record's, or the claims view's (sessionInfoFor),
// all within sendRequestBudget — is a 503 to retry and never a row: neither a session a caller
// may take for ended, nor one whose empty topics and roles it may take for a session that holds
// none. An interest record this build cannot decode is the 500 writeInterestRecordUnreadable
// writes: the bus answered it.
//
// view=registration is answered by writeSessionRegistration, whose comment says what it reads
// and answers; any other value of view is refused 400 naming the selector, and no view at all is
// this route's own answer, unchanged in every respect.
//
// With stopped in the include list (includes), and only then, the route answers one more
// kind of session: one it would otherwise answer 404 that the known-session store holds a record
// of is answered 200 with listenerwire.StoppedSessionInfo, state "stopped", while a session whose
// entry the list accepts answers its ordinary listenerwire.SessionInfo. The record is read from
// memory alone, and only when stopped is asked for. Without stopped in the list the route answers
// that session 404, which is what the Agents page, issue_claim.go's holder liveness and every
// other reader of it read; GET /v1/sessions never lists a stopped session whatever it is asked.
func sessionEntryHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		switch r.URL.Query().Get("view") {
		case sessionRegistrationView:
			ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
			defer cancel()
			writeSessionRegistration(ctx, w, r, d, sessionID)
			return
		case "":
			// No selector is the full answer this route has always given, built below.
		default:
			writeJSONError(w, http.StatusBadRequest, "view must be "+sessionRegistrationView, "view")
			return
		}
		accept := sessionIsLive
		if includes(r, string(session.Unresponsive)) {
			accept = notGone
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		entry, state, err := liveSessionEntry(ctx, d.sessions, sessionID, roleHolderReadTimeout, accept, d.sessions.Refresh)
		// At the edge a session of none of the token's projects is answered as one no listener
		// has heard of, and a shown session's roles and topics are those projects' alone.
		scope := edgeScopeOf(r, d.repositories)
		switch {
		case errors.Is(err, bus.ErrRefused):
			// Refresh reports a refused key as a missing one too, so this test comes first:
			// an id NATS will never hold is answered as the bad input it is, not as a 404.
			// writeBusUnavailable answers bus.ErrTooLarge as the 413 it is and every other
			// refusal as a 400, and withholds Retry-After from both, since NATS repeats a
			// refusal however often the same bytes are sent.
			writeBusUnavailable(w, err)
		case errors.Is(err, nats.ErrKeyNotFound):
			// A stopped session's record names no project, so the edge answers none.
			if scope == nil && includes(r, sessionStateStopped) {
				if known, recorded := d.known.Get(sessionID); recorded {
					writeJSON(w, http.StatusOK, listenerwire.StoppedSessionInfo{
						SessionID:    sessionID,
						State:        sessionStateStopped,
						LastSeen:     known.LastSeen,
						Title:        known.Title,
						Capabilities: append([]string{}, known.Capabilities...),
						Topics:       append([]string{}, known.Topics...),
					})
					return
				}
			}
			if writeUnshownSession(ctx, w, scope, d.sessions, sessionID, entry) {
				return
			}
			writeJSON(w, http.StatusNotFound, noLiveSession(d.sessions, sessionID, entry, state))
		case err != nil:
			// The 503, its Retry-After: 1 and the busUnansweredPrefix body are written in one
			// place for every route that answers a bus that did not answer, so the string a
			// deployed log filter matches has a single copy (INFRA-141). This route's own
			// literal was that copy's twin until the two changes met here.
			writeBusUnavailable(w, err)
		default:
			// At the edge a session of none of the token's projects is answered as one no listener
			// has heard of, and a shown session's roles and topics are those projects' alone. A
			// confirmation read that did not answer is the bus-unavailable 503, never that 404.
			if writeUnshownSession(ctx, w, scope, d.sessions, sessionID, entry) {
				return
			}
			info, err := sessionInfoFor(ctx, d.registry, session.ListEntry{SessionID: sessionID, SessionEntry: entry, State: state})
			info.Roles = scope.roles(info.Roles)
			info.Topics = scope.topics(sessionID, info.Topics)
			switch {
			case errors.Is(err, store.ErrMalformedInterest):
				writeInterestRecordUnreadable(w, sessionID, err)
			case errors.Is(err, bus.ErrRefused):
				d.registry.Logger().Warn("session answered with no topics: its interest record could not be read",
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()),
				)
				writeJSON(w, http.StatusOK, info)
			case err != nil:
				writeBusUnavailable(w, err)
			default:
				writeJSON(w, http.StatusOK, info)
			}
		}
	}
}

// interestRowFor answers item with its session's last heartbeat: the live entry's UpdatedAt while
// the session is live, else the final heartbeat the cache still retains for it
// (session.SessionRegistry.LastSeen), else none. A listener restart empties those records, so a
// session that died before it reads as unknown.
func interestRowFor(sessions *session.SessionRegistry, item store.Interest) listenerwire.InterestRow {
	row := listenerwire.InterestRow{Interest: item}
	if entry, err := sessions.Get(item.SessionID); err == nil {
		row.LastSeen = entry.UpdatedAt
	} else {
		row.LastSeen = sessions.LastSeen(item.SessionID)
	}
	return row
}

// adminInterestsHandler lists, reads and removes persisted interests. Each row a GET answers
// carries its session's last heartbeat (interestRowFor). GET /v1/interests/<session_id>
// reads within sendRequestBudget of the request's own context: a cache miss is read through to the
// bucket, the bucket's word that the key is absent is a 404, a record this build cannot decode is
// the 500 writeInterestRecordUnreadable writes, and a read that did not answer is the
// bus-unavailable 503 with Retry-After: 1 (a key NATS refuses is still the 413 or 400
// writeBusUnavailable gives it). DELETE removes within the same budget, and answers store work that
// did not answer with that 503 too (writeStoreError). removed, when not nil, is called after a
// DELETE removed a session's interests.
//
// A DELETE presenting a broker access token may remove only the interests of the session that
// token names, and is refused 403 for any other (sessionNotBound). The GET and the list are not
// narrowed: reading another session's record is what those are for. The edge listener is the
// exception (edgeInterestReadRefusal): a session there reads its own record alone, with the topics
// of its token's projects alone (edgeShownTopics, through repositories, the edge's repository
// reader, nil on every other listener).
func adminInterestsHandler(registry *store.Registry, sessions *session.SessionRegistry, repositories *edgeRepositories, removed func(ctx context.Context, sessionID string), logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if refusal := edgeInterestReadRefusal(r, sessionID); refusal != nil && r.Method == http.MethodGet {
			refusal.write(w)
			return
		}
		if sessionID == "" {
			if r.Method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			items, err := registry.List()
			if errors.Is(err, kvwatch.ErrRefilling) {
				writeListRefilling(w)
				return
			}
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			rows := make([]listenerwire.InterestRow, 0, len(items))
			for _, item := range items {
				rows = append(rows, interestRowFor(sessions, item))
			}
			writeJSON(w, http.StatusOK, rows)
			return
		}

		switch r.Method {
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
			defer cancel()
			item, err := registry.Get(ctx, sessionID)
			// Only a session the registry holds no key for is unknown; a read NATS did not
			// serve is the listener's failure, which a caller must not take for "no topics".
			if errors.Is(err, nats.ErrKeyNotFound) {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			if errors.Is(err, store.ErrMalformedInterest) {
				writeInterestRecordUnreadable(w, sessionID, err)
				return
			}
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			item.Topics = edgeScopeOf(r, repositories).topics(sessionID, item.Topics)
			writeJSON(w, http.StatusOK, interestRowFor(sessions, item))
		case http.MethodDelete:
			// The administrator's removal: it frees every role claim the session holds,
			// whoever holds it (store.RoleReleaseAuthority.Force), and it is the only
			// caller that forces one. An operator clearing a session's interests states no
			// process, and a release naming none frees no claim at all, so without
			// the force this route could no longer free the roles of a client that has
			// gone away, which is what it is for.
			//
			// The limit this leaves open, decided and left as it stands: a second process
			// that resumed the session's id reaches this route and frees the live holder's
			// claim, because sessionNotBound refuses only a token acting as another
			// session, and such a process acts as this very one. POST
			// /v1/interests/unsubscribe, the route every client hands a role back through,
			// is scoped to the process that asks and cannot; closing this one needs a
			// credential naming the process rather than the session.
			if refusal := sessionNotBound(r, logger, sessionID); refusal != "" {
				writeJSONError(w, http.StatusForbidden, refusal, "session_id")
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
			defer cancel()
			// A deregistration answers no topic list, and this route's 204 carries no body
			// to write one in, so the store's first answer is dropped here.
			if _, err := registry.Remove(ctx, sessionID, nil, store.RoleReleaseAuthority{Force: true}); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeStoreError(ctx, w, err, http.StatusInternalServerError)
				return
			}
			if removed != nil {
				removed(r.Context(), sessionID)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// roleSetHandler answers POST /v1/roles/set. Its store work - the claimant's registration, a soft
// claim's read of the role's holder and of that holder's session, and the claim itself
// (store.Registry.SetRoleWithPrevious) - runs within sendRequestBudget of the request's own
// context, so a bus that does not answer is the bus-unavailable 503 with Retry-After: 1 inside the
// caller's attempt (writeStoreError). The store's own answers keep the answers they had: a held
// role is a 409, a refused key a 413 or 400, and an unreadable claim the 503 or 500 it was.
//
// A claim that carries if_claim_revision is conditional: it lands only while the role's claim
// record is still at that revision, the one GET /v1/roles/<role> reported as claim_revision, and
// 0 names a role with no record at all. A revision the bucket no longer holds - when the claim is
// read, or by the time its conditional write reaches the bucket - is a 409 of its own
// (store.ErrRoleClaimRevisionMismatch), naming the role, the revision the bucket does hold as
// claim_revision and its holder. No claim record is written, so the role keeps the holder the
// standing claim gave it; the claimant's own session registration is refreshed before any of
// this, as it is on every claim this route takes, conditional or not. It is how a scheduler
// turns a read of a role and the claim that
// follows into one decision, which neither last-claim-wins nor a soft claim can do.
//
// A soft claim that carries no if_claim_revision is conditional all the same, on a revision the
// route supplies rather than the caller: the revision of the claim record it made its decision
// on. The route decides on one read of that record and store.Registry.SetRoleWithPrevious writes
// against a second read of its own, so without the precondition two processes of one session id
// claiming one unheld role at the same moment are both granted it. A record replaced in between
// is no answer to such a caller, which asked nothing about revisions, so the route reads the
// claim again, decides the whole thing again on what it then finds, and tries once; a second
// mismatch is the same 409 a stated revision gets. A claim that states its own if_claim_revision
// is never retried - that revision is the caller's own precondition - and a hard claim that
// states none is written with the revision of the record it read, read again and written again on
// a conflict until its request's budget ends (store.Registry.SetRoleWithPrevious), then answered
// 503 with nothing written: the last write still wins.
//
// A soft claim names the process making it (client_instance_id), and the claim it writes records
// that process as the role's holder. Where the role is already held under the claimant's own
// session id that is the whole decision, made on the standing claim and never on the session
// record (roleOwnSessionVerdict): the claim is granted at once where it names this very process,
// and otherwise only where nobody has answered for it inside the live window the registry runs
// with (store.Registry.ClaimRenewedWithin, over the window this listener's session registry
// judges a record's age by, session.SessionRegistry.Windows). A second process that resumed a
// live session's id is refused the 409 naming the holder, and reads the lookup to see which
// process holds the role; a request that states no instance, and a claim record that names none,
// both reach the renewal test rather than a case of their own, since an absent instance matches
// nothing. A renewal older than the window is read as the holder having stopped only while this
// listener can vouch for that: nothing but a registration renews a claim, and the claim record
// reads the same whether its holder stopped registering or this listener failed to renew it. So
// the claim is refused, naming the state and with nothing claimed, in three states. While the
// role claim view cannot be served (store.Registry.RoleViewErr) it is the bus-unavailable 503
// with Retry-After: 1 (writeBusUnavailable). While the copy's last whole read of the role bucket
// is past its repair bound or none has completed (store.RoleClaimRepairReport.Err), and while
// this listener's own renewal of that role's claim for its holder failed inside the live window
// (store.Registry.RenewalFailedWithin, which says how long that record stands), it is the grant
// guard's own answer (writeGrantGuardRefusal), since the bus answered: the 409 a second process
// gets while the holder still answers for its claim, naming the claimant's own session id as the
// holder, with an error that opens with grantGuardPrefix and names the state. The claimant waits
// that 409 out as it waits out a standing claim of another process, though a refusal that
// outlasts claude-envoy's wait (ROLE_CLAIM_WAIT_MS, the live window and 10 s) still stops a
// Legion pane. Without them a listener whose renewals stopped landing would hand a live holder's
// role to the second process once the window passed. A claim from the process the standing claim
// names rests on no renewal and is granted through all three, since it is that holder re-claiming
// its own role after a listener restart. A hard claim reads no holder: it writes with the revision
// it read and lands as the last write, or answers 503 having written nothing.
//
// A claim that landed also hands the role over to its new holder (runTakenRoleDrain): it runs the
// passes of the role's takeover records, which copy what a previous holder did not read to the
// role's kept lane, and then forwards the kept lane to the claimant (handOverTakenRole, which reads
// the lane first and resolves the role's holder itself, so a claim of a role with nothing kept for
// it and no record costs its stream reads and no resolution at all, and a lane with copies on it
// goes to the claim that superseded this one when one has). That runs in a goroutine under a
// context of its own, so this answer's status and body are exactly what they were and wait for
// none of it. A try whose takeover record did not store writes no claim and answers 503
// (errTakeoverUnrecorded).
//
// A claim that landed over another session's claim of a Legion controller role narrows that
// session before it is answered (narrowFormerHolder), as an unsubscribe does: its known-session
// record is written from its stored row, which the claim took the role's topic off, and its
// consumer is filtered on that row again, which drops the role's controller notice lane now that
// the claim, read from the role bucket stream's leader, names the claimant (storedRowFilter). A
// notice published once the claim is answered reaches the new holder alone. What stays open: a
// notice published between the claim's write and that filter write still reaches the former
// holder. Everything after the claim's write - the claimant's topic merged again, the old holder's
// topic taken off its row, the record write, the wait for the former holder's session lock (which
// a registration holds across its consumer call and its writes) and the fenced filter write - runs
// inside this route's budget (sendRequestBudget), so that window ends, at the latest, 3.5 s after
// the request arrived. A narrowing that did not land by then is logged at WARN naming the role,
// the lane and both sessions, and the lane stays on the former holder's consumer until its next
// registration (a heartbeat, at most 90 s) or its relay's restart. A stopped former holder has no
// entry, so its record keeps the lane, which the publish count judges by the claim
// (recipientCounts).
//
// The refresh of the claimant's registration can make its session live again, as a registration
// that returns a session does (subscribeHandler): a process that resumed a session claims its role
// before it registers, and once the refresh has written the entry, its registration reads the
// session as live already and starts no handover. So a claimant this listener's cache read as not
// live just before the refresh (session.SessionRegistry.Lookup, read as registerPass reads it) is
// handed the kept lane of each role the role claim copy names for it (store.Registry.RolesHeldBy),
// whatever this claim's answer: a claim refused the role held under its own session id by another
// process, by the grant guard, or for a read that did not answer starts no handover of its own,
// and the copies of the session's roles would otherwise wait for the sweep. The claimed role is
// handed over once: by the handover of a claim that landed, and otherwise only where the copy
// names it for the session. A copy that cannot be served names no role, which leaves the claim's
// own handover and the sweep. These handovers run as the claim's own does, each in a goroutine
// started once the answer is written.
//
// logger is registerV1Routes's own. Two decisions of this route are logged with it. A soft claim
// that takes a role from the predecessor it names because the claimant's entry or the
// predecessor's carries no client instance id, which is the one takeover the route cannot tell
// from a different client instance's, and so names both session ids. And a soft claim refused
// the role held under its own session id, whether its holder still answers for the claim or the
// grant guard refused, logged once with the role, that session id, the holder's client instance
// and the claimant's - and, for the guard, the arm that refused - so an operator sees a second
// process being held off rather than a bare 409.
func roleSetHandler(d *listenerDeps, machineID string, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
			// A soft claim takes the role only when it is unheld, when its
			// holder is no longer live, or when its holder is the claimant's
			// declared predecessor and that predecessor's own client instance
			// allows it (PreviousSessionID); any other live holder answers 409
			// with its id. This is how a resumed or forked session recovers its
			// own role without ever taking one from a peer. Default false:
			// last-claim-wins.
			Soft bool `json:"soft"`
			// The session id this claimant is continuing (a /fork, /branch, or
			// /handoff mints a new id in the same process). Naming it is not
			// enough to take the role from it: a soft claim naming a
			// predecessor takes the role only when that predecessor has no live
			// registry entry, when its live entry carries this claimant's own
			// client instance id (the same conversation continuing inside its
			// own Oh My Pi Envoy extension instance or Claude Code
			// channel-server process), or when the claimant's entry or the
			// predecessor's carries no client instance id at all, which is a
			// client predating the field and whose takeover is logged. A
			// predecessor still live in another client instance keeps the role,
			// and the claim answers 409 naming it. Only honoured with soft;
			// ignored otherwise.
			PreviousSessionID string `json:"previous_session_id"`
			// The revision GET /v1/roles/<role> reported as claim_revision when this
			// claimant read the role. The claim lands only while the role bucket still
			// holds that very revision, and 0 names a role with no claim record at all,
			// so a claim with if_claim_revision: 0 lands only while none exists. Any
			// other revision in the bucket, a record where this names none, or none
			// where this names one is 409, and so is a claim that lost the race to a
			// claim landing between this claimant's read and its write; no claim record
			// is written. Omitted (null), the claim is weighed by the soft and
			// last-claim-wins rules alone, and a soft claim is then written against
			// the revision of the record the route decided on, which it reads again
			// and decides again once where that record moved in between.
			IfClaimRevision *uint64 `json:"if_claim_revision"`
			// The client instance of the process making this claim: the id its plugin
			// minted at its own start, the same one its registration carries
			// (subscribeHandler's client_instance_id). The claim records it as the
			// process that holds the role, and it is what tells a second process that
			// resumed this session id from the one that holds the claim: a claim under
			// a session id another process holds the role under is granted only where
			// this names that very process, and otherwise only once nobody has renewed
			// that claim inside the live window. A client that states none is weighed
			// by the renewal alone, since an absent instance matches no claim.
			ClientInstanceID string `json:"client_instance_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Role = strings.TrimSpace(body.Role)
		body.ClientInstanceID = strings.TrimSpace(body.ClientInstanceID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if refusal := sessionNotBound(r, logger, body.SessionID); refusal != "" {
			writeJSONError(w, http.StatusForbidden, refusal, "session_id")
			return
		}
		if body.Role == "" {
			writeJSONError(w, http.StatusBadRequest, "role is required", "role")
			return
		}
		// The role is read in either form, its project first (INFRA.legion-infra-infra-12-architect,
		// PRIV.lead-reviewer, _.lead-reviewer) or its name alone, and everything below works on the
		// key its claim record is keyed by (store.Registry.ClaimedRole, contracts.RoleClaimOf): the
		// bare name for the old form, for a free-form role under no project and for a Legion role,
		// and <project>.<role> for a free-form role under a project, which is that project's own
		// claim beside the claim of the same name under no project. The old form is counted and
		// named back in the answer's warnings with its new form; nothing is refused for it. A new
		// form under a project its role cannot work for is refused (RoleClaimOf's error): a
		// Legion role works for the project its own name gives, and a claim of it under another
		// project's key would believe it holds a claim it does not.
		claim, claimErr := contracts.RoleClaimOf(body.Role)
		if !roles.Valid(claim.Role) {
			writeJSONError(w, http.StatusBadRequest, "role must match "+roles.PatternString+", after its project and a dot when it names one", "role")
			return
		}
		// At the edge a claim works for one of the token's projects (edgeRoleRefusal), which is
		// decided before the rule below, so a claim of another project's role is refused for its
		// project whatever form it names that role in.
		if refusal := edgeRoleRefusal(r, body.Role); refusal != nil {
			refusal.write(w)
			return
		}
		if claimErr != nil {
			writeJSONError(w, http.StatusBadRequest, claimErr.Error(), "role")
			return
		}
		var roleWarning string
		body.Role, roleWarning = d.registry.ClaimedRole(body.Role)
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		entry, err := d.sessions.Get(body.SessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			writeJSONError(w, http.StatusNotFound, "session is not registered")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "read session registration: "+err.Error())
			return
		}
		// Get answers from the cache alone, and the cache follows the session bucket through a
		// watcher and so trails it. A trailing cache would decide this claim twice over: it would
		// read an empty client instance id from an entry this process has not caught up with,
		// demoting a claim weighed against a predecessor to the "no instance id" case below, and
		// PutWithin would write that stale entry back over a newer one. So the claimant's own
		// entry is read from the bucket itself, and when that read answers, its entry is the one
		// refreshed here and the one the predecessor rule falls back to. A read that does not
		// answer keeps the cache's entry and goes on, so this claim is never worse off than it
		// was without the bucket read.
		if fresh, freshErr := bucketSessionEntry(ctx, d.sessions, body.SessionID, roleHolderReadTimeout); freshErr == nil {
			entry = fresh
		}
		// The process this claim is made by is the one the request states, and the claim records
		// it: a session id names no process, since a second process can resume a live session's
		// id, and the session record then names whichever of them registered the id last. That
		// record is the thing this route stops trusting, so it is read for one decision alone -
		// the predecessor rule's fallback below - and registeredInstanceID is read before the
		// refresh overwrites nothing of it.
		claimantInstanceID := body.ClientInstanceID
		registeredInstanceID := entry.ClientInstanceID
		entry.MachineID = machineID
		entry.SelfSubscribed = true
		// The claim renews the claimant's registration by writing back the entry it read, and
		// it never writes the instance the request states into that entry: one registration per
		// session id is a non-goal, and a process that wants a registration of its own
		// registers (POST /v1/sessions/subscribe).
		//
		// The refresh writes the port out of the entry: no registration this build stores
		// carries one (subscribeHandler refuses a registration that names a serving port) and
		// no delivery opens a connection to a session, so a renewed entry still carrying the
		// port an earlier build's listener stored would keep answering GET /v1/sessions with it
		// (listenerwire.SessionInfo.Port) for as long as the claims keep refreshing the entry,
		// where writing it out lets it lapse with the entry it came on.
		entry.Port = 0
		// claimRefreshProjects keeps the stored record's projects only where the stream's leader
		// confirms that record is this session's own registration, and takes the claim token's
		// projects (registeringProjects) otherwise, so a forged record's projects are never written
		// back as this session's own. A confirmation read that did not answer is the bus-unavailable
		// 503, so a hiccup never wipes a session's projects.
		projects, clearForged, projectsErr := claimRefreshProjects(ctx, d.sessions, r, body.SessionID, entry)
		if projectsErr != nil {
			writeStoreError(ctx, w, projectsErr, http.StatusServiceUnavailable)
			return
		}
		entry.Projects = projects
		// The refresh names this machine as the session's relay, so a claim made through
		// another edge replica than the one the session registered through moves the relay
		// here. An edge relay holds the session's frames to the projects of the token this
		// replica admitted it on (edgeAdmissions), and relays nothing to a session it admitted
		// none of, so the claim records the admission of its own token first, as a registration
		// does (subscribeHandler): the relay the refresh starts here has it, and the session's
		// messages keep reaching it before its next registration.
		d.consumers.admitHeld(r, body.SessionID)
		// Whether the claimant was live before this refresh is read before it, from this
		// listener's cache with no bucket call, as a registration reads it (registerPass): a
		// claimant the refresh makes live again is handed its roles' kept lanes, by the rule above.
		_, before := d.sessions.Lookup(body.SessionID)
		var writeErr error
		if clearForged {
			// The stream leader holds no writer's record for the key the cached entry names, so none
			// of its fields but the machine, self-subscription and the claim token's projects may be
			// the session's own: a RePublish could set its own session's title, a non-consumer
			// delivery that pauses its relay until the next registration, a display field or a gap
			// mark. PutClearingGap writes only those three over a record no writer's stands for, as the
			// gap and renewal writes carry back nothing of a server-written record (the F3 rule); where
			// the session's own registration landed in the race between this confirmation and the
			// write, it writes that record back with only those three changed, keeping its delivery set
			// and the rest, rather than clearing a live session's own.
			writeErr = d.sessions.PutClearingGap(ctx, body.SessionID, entry.MachineID, entry.SelfSubscribed, entry.Projects)
		} else {
			writeErr = d.sessions.PutWithin(ctx, body.SessionID, entry)
		}
		if writeErr != nil {
			writeStoreError(ctx, w, fmt.Errorf("refresh role claimant registration: %w", writeErr), http.StatusServiceUnavailable)
			return
		}
		// At the edge the claim just recorded the session's admission, which resumes its relay there (resumeHeld).
		d.consumers.resumeHeld(r, body.SessionID)
		// landed is set by a claim that landed, whose own handover hands the claimed role's lane
		// over, so the handovers of a claimant this refresh made live again leave that role out.
		// At the edge a claimant is handed the kept lanes of its token's projects' roles alone
		// (edgeScope.roles): a claim of another project's role, which the session's next
		// registration releases, hands it nothing.
		landed := false
		if before != session.Live {
			defer func() {
				held, _ := d.registry.RolesHeldBy(body.SessionID)
				held = edgeScopeOf(r, nil).roles(held)
				for _, role := range held {
					if !landed || role != body.Role {
						startHandover(*d.delivery, role, runKeptRoleDrain)
					}
				}
			}()
		}
		previous := ""
		if body.Soft {
			previous = strings.TrimSpace(body.PreviousSessionID)
		}
		// liveWindow is the window this listener's session registry judges a record's age by,
		// read from that registry itself (session.SessionRegistry.Windows) so that the claim a
		// claimant's own session id holds is weighed over exactly the window its registration
		// keeps it live for. The claim's renewal is weighed against it on the registry's own
		// clock (store.Registry.ClaimRenewedWithin), which is the clock the role claim reaper
		// weighs the same claim on, so the route and the sweep can never disagree.
		liveWindow, _ := d.sessions.Windows()
		// decideSoftClaim weighs a soft claim against the role's standing claim, which it reads
		// itself, and answers what that claim may do to the holder it found (roleSupersession),
		// the holders the claim may supersede, and the claim record the verdict was made on -
		// its revision is the precondition the write then carries, and the process it names is
		// what the route's refusal logs. It writes no answer and logs nothing: the handler
		// below does both, once, for the decision whose answer it writes. A hard claim never
		// asks: it reads no holder and claims last-claim-wins.
		//
		// The error is non-nil for the claim read that did not answer, which carries no
		// verdict; with roleHolderUnread, which is a read of the holder's session record that
		// did not answer or, for a role held under the claimant's own session id, a role claim
		// view that cannot be served; and with roleGrantGuarded, which is the grant guard
		// refusing a role held under the claimant's own session id while every read it rests
		// on answered (the repair past its bound, or this listener's own renewal of that claim
		// that failed), and which is answered as the 409 naming the claimant's own session id,
		// which the claimant waits out as it waits out a standing claim of another process
		// (writeGrantGuardRefusal). The three are answered differently and the verdict is what
		// tells them apart.
		//
		// The registry sees only interest rows; a holder that is another session is the session
		// bucket's call (roleSupersessionVerdict, through liveSessionEntry and
		// bucketSessionEntry). A holder that is not live - stalled past the live window, or
		// with no record at all - may be superseded by a soft claim, and the claimant's own
		// declared predecessor may be superseded while it is still live as well, but then only
		// from the client instance it belongs to; any other live holder is protected. The live
		// window, and not the bucket's maximum age, is the test, because a soft claim is how a
		// relaunched Legion role takes its own role back: its predecessor keeps that claim while
		// it is merely unresponsive, and nothing but the successor saying "this is me again" may
		// move it before the gone horizon. A holder that is the claimant's own session id is the
		// standing claim's call instead (roleOwnSessionVerdict, which reads no session record,
		// since a session id names no process).
		decideSoftClaim := func() (supersedable []string, claim store.RoleClaim, verdict roleSupersession, err error) {
			claim, err = d.registry.RoleClaim(ctx, body.Role)
			if err != nil {
				return nil, store.RoleClaim{}, roleUnclaimed, fmt.Errorf("read role holder: %w", err)
			}
			holder := claim.HolderSessionID
			switch {
			case holder == "":
				// An unheld role is the claimant's: there is nobody to supersede.
				return nil, claim, roleUnclaimed, nil
			case holder == body.SessionID:
				verdict, err = roleOwnSessionVerdict(d.registry, body.Role, claim, claimantInstanceID, liveWindow)
			default:
				// The predecessor rule reads the claimant's own registration where the
				// request states no instance. Without that fallback a client that sends none
				// but has one stored would move from "the predecessor keeps the role" to
				// "the role is taken, logged as unidentified", which is not what this route
				// decides. It is the one fallback: the rule above, and a release, read an
				// absent instance as naming no process, and nothing stands in for it.
				claimantInstance := claimantInstanceID
				if claimantInstance == "" {
					claimantInstance = registeredInstanceID
				}
				verdict, err = roleSupersessionVerdict(ctx, d.sessions, holder, previous, claimantInstance)
			}
			if verdict == roleTakenFromHolder || verdict == roleTakenFromUnidentifiedPredecessor {
				supersedable = append(supersedable, holder)
			}
			return supersedable, claim, verdict, err
		}
		// The decision above is made on one read of the role's claim, and
		// store.Registry.SetRoleWithPrevious reads the claim again and writes against that second
		// read. The record decided on and the record written against need not be the same one: a
		// claim can land between the two reads. Without a precondition that is how two processes
		// of one session id claiming one unheld role at the same moment both come away believing
		// they hold it - each reads the role as unheld, each decides the role is its own, and the
		// loser of the write's compare-and-swap reads the winning claim as its own session's and
		// answers 200. So a soft claim that states no if_claim_revision of its own is written
		// against the revision of the claim record this route decided on
		// (store.RoleClaim.Revision, which a role with no record at all reports as 0, so the
		// precondition is "there is still no record"), and the write happens only while that
		// record still stands.
		//
		// A claim refused because that record moved under it is not an answer its caller asked
		// for: the caller said nothing about revisions. So the route reads the claim again, makes
		// the whole decision again on what the second read finds - the same-session-id verdict
		// and the predecessor verdict alike, never a verdict kept from the first read - and
		// tries once more. That is one attempt and at most one retry: a second mismatch is
		// answered exactly as one is today, the 409 naming the role, the holder and the revision
		// the bucket holds. A body that states its own if_claim_revision is never retried,
		// because that revision is the caller's own precondition and deciding again would answer
		// a question it did not ask, and a hard claim that states none is written with the
		// revision of the record it read and read again on a conflict, the last write winning.
		//
		// The claimant's registration is refreshed above, once, whichever attempt then claims.
		// The two decisions this route logs - the refusal of a second process of one session id,
		// the takeover of a predecessor neither side identifies - are written by the attempt
		// whose answer this route writes, so a decision a retry threw away logs nothing: no
		// operator reads that a claim was held off when it was granted a moment later, or that a
		// role was taken by an attempt that took nothing.
		for attempt := 0; ; attempt++ {
			var supersedable []string
			var claim store.RoleClaim
			verdict := roleUnclaimed
			ifClaimRevision := body.IfClaimRevision
			if body.Soft {
				var decideErr error
				supersedable, claim, verdict, decideErr = decideSoftClaim()
				switch {
				case verdict == roleHolderUnread:
					// A read that did not answer, or a role claim view that cannot be
					// served, takes nothing from a holder. At the edge a read of the holder's
					// session is answered in the listener's own words (edgeScope.unresolved).
					writeBusUnavailable(w, edgeScopeOf(r, nil).unresolved(body.Role, decideErr, logger))
					return
				case verdict == roleGrantGuarded:
					// The bus answered, and this listener still cannot vouch that the
					// holder stopped renewing its claim: nothing is taken, and the claimant
					// is told its own session holds the role, as a second process is while
					// the holder still answers for its claim, so it asks again. This answer
					// is the route's, so the refusal is logged here, once.
					logger.Info("listener role claim refused: the grant guard cannot vouch that the process holding the role under this session id stopped renewing it",
						slog.String("role", body.Role),
						slog.String("session_id", body.SessionID),
						slog.String("holder_client_instance_id", claim.ClientInstanceID),
						slog.String("client_instance_id", claimantInstanceID),
						slog.String("arm", decideErr.Error()))
					writeGrantGuardRefusal(w, body.Role, claim.HolderSessionID, decideErr)
					return
				case errors.Is(decideErr, store.ErrMalformedRoleClaim):
					// A stored claim no read can decode is no outage: the 500 the role lookup
					// gives it (writeRoleClaimUnreadable), so a claimant stops rather than
					// claim again at every backoff for a record that will not change.
					writeRoleClaimUnreadable(w, body.Role, decideErr)
					return
				case decideErr != nil:
					writeStoreError(ctx, w, decideErr, http.StatusServiceUnavailable)
					return
				}
				if ifClaimRevision == nil {
					decidedRevision := claim.Revision
					ifClaimRevision = &decidedRevision
				}
			}
			tries := &takeoverTries{cfg: *d.delivery, claimant: body.SessionID, role: body.Role}
			item, err := d.registry.SetRoleWithPrevious(ctx, store.RoleClaimRequest{
				SessionID:         body.SessionID,
				MachineID:         machineID,
				ClientInstanceID:  claimantInstanceID,
				Role:              body.Role,
				PreviousSessionID: previous,
				Soft:              body.Soft,
				IfClaimRevision:   ifClaimRevision,
				Supersedable:      supersedable,
				BeforeTry:         tries.beforeTry,
			})
			// Neither wrote a claim, and both are the client's to make again: every try met a
			// conflict until the deadline, or a try's takeover record did not store.
			if errors.Is(err, store.ErrRoleClaimContended) || errors.Is(err, errTakeoverUnrecorded) {
				if errors.Is(err, errTakeoverUnrecorded) {
					warnTakeoverUnrecorded(logger, body.Role, body.SessionID, err)
				}
				w.Header().Set("Retry-After", "1")
				writeJSONError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			var stale *store.ErrRoleClaimRevisionMismatch
			mismatch := errors.As(err, &stale)
			if mismatch && attempt == 0 && body.Soft && body.IfClaimRevision == nil {
				// The record this decision rested on was replaced between the read and the
				// write. Decide again on the record that stands now, and with the decision
				// goes whatever it was about to log.
				continue
			}
			// This attempt's answer is the route's, so this decision is the one logged.
			switch verdict {
			case roleTakenFromUnidentifiedPredecessor:
				logger.Info("listener role taken from a live predecessor because one side carries no client instance id",
					slog.String("role", body.Role),
					slog.String("session_id", body.SessionID),
					slog.String("previous_session_id", claim.HolderSessionID))
			case roleHeldByAnotherProcess:
				// A second process of one session id, held off while the first still answers
				// for its claim: store.Registry.SetRoleWithPrevious answers the 409 that names
				// the holder, and the refused client reads the lookup to see which process
				// holds it (GET /v1/roles/<role>).
				logger.Info("listener role claim refused: the role is held under this session id by another process",
					slog.String("role", body.Role),
					slog.String("session_id", body.SessionID),
					slog.String("holder_client_instance_id", claim.ClientInstanceID),
					slog.String("client_instance_id", claimantInstanceID))
			}
			// At the edge a 409 names no holder the scope does not show, nor the revision of
			// that holder's claim, as its role read names neither (edgeScope.namesHolderConfirmed,
			// roleHeldElsewhere); a 409 of a holder the scope shows keeps the revision, so the
			// caller can claim against the record that stands.
			scope := edgeScopeOf(r, nil)
			if mismatch {
				answer := map[string]any{
					"error":          err.Error(),
					"role":           stale.Role,
					"holder":         stale.Holder,
					"claim_revision": stale.Actual,
				}
				if named, namedErr := scope.namesHolderConfirmed(ctx, d.sessions, stale.Holder); namedErr != nil {
					writeBusUnavailable(w, namedErr)
					return
				} else if stale.Holder != "" && !named {
					answer = roleHeldElsewhere(stale.Role)
				}
				writeJSON(w, http.StatusConflict, answer)
				return
			}
			var held *store.ErrRoleHeld
			if errors.As(err, &held) {
				answer := map[string]any{
					"error":  err.Error(),
					"role":   held.Role,
					"holder": held.Holder,
				}
				if named, namedErr := scope.namesHolderConfirmed(ctx, d.sessions, held.Holder); namedErr != nil {
					writeBusUnavailable(w, namedErr)
					return
				} else if !named {
					answer = roleHeldElsewhere(held.Role)
				}
				writeJSON(w, http.StatusConflict, answer)
				return
			}
			if err != nil {
				writeStoreError(ctx, w, err, http.StatusInternalServerError)
				return
			}
			landed = true
			startHandover(*d.delivery, body.Role, runTakenRoleDrain)
			// A claim that took a controller role from another session narrows that session
			// before it is answered, within this route's budget (narrowFormerHolder), so a notice
			// published once this claim is answered reaches the new holder alone.
			if tries.replaced != "" {
				narrowFormerHolder(ctx, d, logger, body.Role, body.SessionID, tries.replaced)
			}
			answer := roleSetResponse{Interest: item}
			if roleWarning != "" {
				answer.Warnings = []string{roleWarning}
			}
			writeJSON(w, http.StatusOK, answer)
			return
		}
	}
}

// roleHeldElsewhere is the 409 body of a claim of role at the edge whose standing claim names a
// holder the scope does not show (edgeScope.namesHolderConfirmed): that the role is held, and
// neither by whom nor at which claim revision, since the role read answers such a role as one no
// claim holds, at claim revision 0 (edgeScope.holderConfirmed).
func roleHeldElsewhere(role string) map[string]any {
	return map[string]any{"error": fmt.Sprintf("role %s is held by another session", role), "role": role}
}

// roleGetHandler answers GET /v1/roles/<role> with the role's live holder, its holder_state and
// its last_seen. It resolves the holder (roles.ResolveHolder, the one resolution)
// within sendRequestBudget of the request's own context, as
// a role send's first resolution through the publish route does, so a bus that does not answer is
// reported inside the caller's attempt. A holder past the live window is the 404
// writeRoleHolderError writes, naming its state and saying it released nothing: the role cannot
// be reached now, and the caller must be told so, but the claim behind it stands until the role
// reaper finds the holder gone. A claim record that cannot be decoded
// (store.ErrMalformedRoleClaim) is the publish route's 500 with no Retry-After
// (writeRoleClaimUnreadable): the bus answered it. Any other failure is a role store call that
// did not answer within its cap or that budget - the claim read or the holder's session read -
// and is answered as the publish route answers it (writeBusUnavailable): the 503 with
// Retry-After: 1 whose body begins busUnansweredPrefix and names the call, or the 413 and 400 a
// refused key gets. At the edge a holder's session read that did not answer names no holder: it
// is answered in the listener's own words, and logger gets the read's error (edgeScope.unresolved).
//
// Both answers carry claim_revision, the role bucket revision of the claim record the resolution
// read: the revision the live holder's claim was read at on the 200, and on the 404 the revision
// of a lapsed holder's claim, which stands until the role reaper takes it, or 0 for a role no
// record claims. A scheduler reads it and hands it back as POST /v1/roles/set's
// if_claim_revision, so its claim is refused if the role moved in between.
//
// Both answers carry client_instance_id too, the process the claim names as its holder
// (store.RoleClaim.ClientInstanceID): on the 200 beside the live holder, and on the 404 beside a
// lapsed holder. It is how a client sees that a role is held under its own session id by another
// process, which is what POST /v1/roles/set refuses it for; that 409 is not widened, and a
// refused claimant reads this lookup. A claim that names no process - one an earlier build
// wrote, or one whose claimant stated none - leaves the field out on both answers, as the 404
// has always left it out (listenerwire.RoleRefusal.ClientInstanceID, omitempty): a client reads
// an absent field as "no other process holds this role" and an empty one as "another process
// does", so a 200 that wrote "" would send the holder of such a claim a soft claim it does not
// need and a 409 naming itself, until its next registration adopted the claim for its process.
//
// Both answers carry handed_back where the claim carries the hand-back mark
// (store.RoleClaim.HandedBack): the holder gave this role back through a release that could not
// free the claim, so no registration renews it any more and the role claim reaper frees the role
// once that renewal has lapsed, a live window and up to one reap interval later. The key is left
// out where the claim carries no mark, as
// client_instance_id is left out where it names no process, so a client reads an absent field as
// "this claim is still its holder's" rather than having to tell false from missing.
func roleGetHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		if name == "" {
			writeJSONError(w, http.StatusBadRequest, "role is required", "role")
			return
		}
		// The path names the role in either form, its project first or its name alone, and the
		// holder is read under the key a claim of that name is keyed by (store.RoleClaimKeyOf,
		// which store.Registry.ReadRole answers). A path naming a valid role in the old form is
		// counted (store.Registry.ReadRole): a caller that looks a role up by its name alone is
		// still on the old form, though this answer, a holder's record, carries no warning of it.
		if refusal := edgeRoleRefusal(r, name); refusal != nil {
			refusal.write(w)
			return
		}
		role, _ := d.registry.ReadRole(name, roles.Valid)
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		scope := edgeScopeOf(r, nil)
		result, err := roles.ResolveHolder(ctx, d.registry, roleHolderSessions{d.sessions}, role)
		if errors.Is(err, store.ErrMalformedRoleClaim) {
			writeRoleClaimUnreadable(w, role, err)
			return
		}
		if err != nil {
			writeBusUnavailable(w, scope.unresolved(role, err, logger))
			return
		}
		// At the edge a holder the scope does not show, live or lapsed, is no holder the answer
		// names: the role reads as one no claim holds (edgeScope.holderConfirmed).
		confirmed, confErr := scope.holderConfirmed(ctx, d.sessions, result)
		if confErr != nil {
			writeBusUnavailable(w, confErr)
			return
		}
		result = confirmed
		if result.State != roles.Live {
			writeRoleHolderError(w, role, result)
			return
		}
		// holder_state is the state the holder's own record puts it in, live on this answer, and
		// is named exactly as the 404 of a lapsed holder names it (listenerwire.RoleRefusal) and
		// as the holder's state is named on a publish answer: a caller reads one field on every
		// role answer rather than inferring the state from the status code.
		writeJSON(w, http.StatusOK, listenerwire.RoleHolder{
			Capabilities:     append([]string{}, result.Entry.Capabilities...),
			ClaimRevision:    result.ClaimRevision,
			ClientInstanceID: result.HolderClientInstanceID,
			Dir:              result.Entry.Dir,
			HandedBack:       result.HolderHandedBack,
			Holder:           result.Holder,
			HolderState:      result.HolderState,
			Host:             result.Entry.HostOrMachine(),
			LastSeen:         &result.Entry.UpdatedAt,
			MachineID:        result.Entry.MachineID,
			Role:             role,
			Title:            result.Entry.Title,
		})
	}
}

func (d *listenerDeps) streamInspector() streamInfoLookup {
	if d.streamInfo != nil {
		return d.streamInfo
	}
	return d.client.JS()
}

func streamInfoWithin(ctx context.Context, inspector streamInfoLookup, streamName, subjectsFilter string) (*nats.StreamInfo, error) {
	type result struct {
		info *nats.StreamInfo
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		info, err := inspector.StreamInfo(
			streamName,
			&nats.StreamInfoRequest{SubjectsFilter: subjectsFilter},
			nats.Context(ctx),
		)
		resultCh <- result{info: info, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.info, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// githubRepositoryTopic is a GitHub topic read as Envoy publishes it. Its repository name runs from
// the token after the owner to the first token that is one of contracts.GithubTopicKinds or a
// wildcard, and Envoy writes each dot in a name as `_`, so a name the topic spreads over several
// tokens receives nothing.
type githubRepositoryTopic struct {
	owner string
	// name holds the name's tokens as the topic wrote them; repo is them joined with `_`.
	name []string
	repo string
	// spelled is the topic with repo in place of name.
	spelled string
	// kindFollows is whether a kind, rather than a wildcard or the topic's end, ended the name.
	kindFollows bool
	// rest holds the tokens after the name: `pr`, `7`, `>` in `...example-repo.pr.7.>`.
	rest []string
}

func parseGithubRepositoryTopic(topic string) (githubRepositoryTopic, bool) {
	const githubTopicPrefix = "notifications.github."
	remainder, ok := strings.CutPrefix(topic, githubTopicPrefix)
	if !ok {
		return githubRepositoryTopic{}, false
	}
	parts := strings.Split(remainder, ".")
	if len(parts) < 3 || parts[0] == "" {
		return githubRepositoryTopic{}, false
	}
	end := 2
	for end < len(parts) && parts[end] != "*" && parts[end] != ">" && !slices.Contains(contracts.GithubTopicKinds, parts[end]) {
		end++
	}
	repo := strings.Join(parts[1:end], "_")
	if repo == "" {
		return githubRepositoryTopic{}, false
	}
	return githubRepositoryTopic{
		owner:       parts[0],
		name:        parts[1:end],
		repo:        repo,
		spelled:     githubTopicPrefix + strings.Join(append([]string{parts[0], repo}, parts[end:]...), "."),
		kindFollows: end < len(parts) && slices.Contains(contracts.GithubTopicKinds, parts[end]),
		rest:        parts[end:],
	}, true
}

// lifecycleOnly is whether the topic is a pull request's bare subject, `pr.<n>` with nothing after
// it. That subject carries the pull request's own events, every action of its `pull_request`
// webhook (opened, synchronize, closed and the rest), and nothing else: its reviews, comments,
// mentions and checks are published as `pr.<n>.<kind>`, which only a trailing wildcard such as
// `pr.<n>.>` reaches.
func (g githubRepositoryTopic) lifecycleOnly() bool {
	return len(g.name) == 1 && len(g.rest) == 2 && g.rest[0] == "pr" && g.rest[1] != "" &&
		strings.Trim(g.rest[1], "0123456789") == ""
}

// subjectMatches reports whether the NATS subject pattern matches the concrete subject: `*` takes
// one token, and a last `>` one or more.
func subjectMatches(pattern, subject string) bool {
	want, got := strings.Split(pattern, "."), strings.Split(subject, ".")
	for i, token := range want {
		if token == ">" {
			return i == len(want)-1 && len(got) > i
		}
		if i >= len(got) || (token != "*" && token != got[i]) {
			return false
		}
	}
	return len(want) == len(got)
}

// lifecycleOnlyWarning is what a session is told when it follows a pull request's bare subject and
// none of its topics reaches that pull request's reviews: it would hear the pull request open,
// change and close, and no review, comment or check of it. The plugins register the bare subject
// beside every `pr.<n>.>` they are asked for (a NATS `>` does not match its base), so a session
// that asked for the family holds both and is told nothing.
func lifecycleOnlyWarning(parsed githubRepositoryTopic, topic string, topics []string) string {
	if !parsed.lifecycleOnly() {
		return ""
	}
	review := topic + ".review"
	if slices.ContainsFunc(topics, func(other string) bool { return subjectMatches(other, review) }) {
		return ""
	}
	return fmt.Sprintf("%s carries only the pull request's own events (opened, synchronize, closed and its other actions): subscribe to %s.> for its reviews, comments, mentions and checks as well", topic, topic)
}

// warning is what a subscription to the topic is told when its name spans several tokens. A kind
// after them, or an empty token among them (a leading, trailing or doubled dot), means the name
// was spelled with its dot, and the warning names Envoy's spelling. Otherwise the token after the
// name's first is a mistyped kind, and the warning names the kinds instead of a spelling that
// would receive nothing either. A repository whose name is itself a kind (`acme.pr.pr.7.>`) is
// read as that name, and `acme.site.pr.pr.7.>` reads as `acme/site`: the two can't be told apart.
func (g githubRepositoryTopic) warning(topic string) string {
	switch {
	case len(g.name) == 1:
		return ""
	case g.kindFollows || slices.Contains(g.name, ""):
		return fmt.Sprintf(`%s spells a repository name with a dot, and GitHub topics write each dot in a name as "_": subscribe to %s`, topic, g.spelled)
	default:
		return fmt.Sprintf(`%s: %q after %s/%s is not a GitHub topic kind (%s), and a dot in a repository name is written "_"`,
			topic, g.name[1], g.owner, g.name[0], strings.Join(contracts.GithubTopicKinds, ", "))
	}
}

func unwiredRepositoryWarning(ctx context.Context, d *listenerDeps, topic string, logger *logging.Logger) string {
	parsed, ok := parseGithubRepositoryTopic(topic)
	if !ok {
		return ""
	}
	owner, repo := parsed.owner, parsed.repo
	if strings.ContainsAny(owner, "*>") || strings.ContainsAny(repo, "*>") {
		logger.Warn("listener ignored GitHub repository topic with wildcard segment", slog.String("topic", topic))
		return ""
	}
	inspector := d.streamInspector()
	streamName := d.streamName
	if streamName == "" {
		streamName = bus.Stream
	}
	subjectsFilter := "notifications.github." + owner + "." + repo + ".>"
	info, err := streamInfoWithin(ctx, inspector, streamName, subjectsFilter)
	if err != nil {
		logger.Warn("listener stream info failed",
			slog.String("stream", streamName),
			slog.String("subjects_filter", subjectsFilter),
			slog.String("error", err.Error()))
		return fmt.Sprintf("could not verify GitHub wiring for %s/%s: %v", owner, repo, err)
	}
	if info == nil || len(info.State.Subjects) == 0 {
		return fmt.Sprintf("no GitHub event for %s/%s in the stream's retention window; is the App installed there?", owner, repo)
	}
	return ""
}

// splitUncapturedTopics splits a registration's requested topics into the ones it plans and the
// set it leaves out: a notification topic that neither the deployed stream nor this build
// captures and that is no role lane (bus.ReachesNoSession). The plan made of the rest keeps out
// what the session's consumer filter leaves out in turn (planWithoutLeftOut). A session receives
// what its own consumer delivers, which is what the stream stored, so an interest in such a topic
// would be stored and deliver nothing. A topic this build stores is planned also while the deployed
// stream does not capture it, as during a hub move's quiesce: the stream stores it again once the
// narrowing ends, and the session's consumer, whose filter holds every topic of the row whatever
// the deployed stream captures (sessionConsumerFilter), then delivers it.
// It is asked once per topic, here, and the answer carries to the plan as well as to the warning
// each dropped topic earns (contracts.UncapturedTopicRefusal) — the warning a topic in this set
// earns only when the private rule has nothing to say about it, since a wildcard such as
// notifications.> is both, and what it earns is the private warning (subscribeHandler).
//
// It judges the topics on m.stream, the listener's own client, whose last read of the deployed
// subjects keeps a topic only a newer build's subjects capture. The consumer's filter reads no
// such subjects. A nil m judges the subjects this build compiles; such a listener refuses every
// registration anyway.
//
// Both plugins refuse an explicit subscribe to one in those same words (envoy_subscribe in
// pi-envoy's extensions/envoy.ts and follow in claude-envoy's src/envoy-channel-server.ts, both
// through refuseUncapturedTopics in @legion/envoy-client's stream-topics.ts), so a caller that
// named one reads the same sentence wherever it is told: the sentence itself is written once, as
// uncapturedTopicRefusal in packages/contracts, which scripts/gen-go.ts emits as
// contracts.UncapturedTopicRefusal.
func (m *sessionDelivery) splitUncapturedTopics(requested []string) ([]string, map[string]struct{}) {
	var stream *bus.Client
	if m != nil {
		stream = m.stream
	}
	planned := make([]string, 0, len(requested))
	uncaptured := make(map[string]struct{})
	for _, topic := range requested {
		if stream.ReachesNoSession(topic) {
			uncaptured[topic] = struct{}{}
			continue
		}
		planned = append(planned, topic)
	}
	return planned, uncaptured
}

// planWithoutLeftOut takes out of plan's topics each one the session's consumer filter leaves out
// by its fourth or fifth kind (sessionFilter): a topic that does not begin with the literal
// notifications., and a wildcard topic that fed a merge of the filter's reduction reaching another
// session's private subjects. It lists them in plan.LeftOut, which the planned write takes out of
// the row too (store.InterestPlan.LeftOut), and returns them with what made each go, which the
// handler names in its answer and in a WARN line of its own (subscribeHandler). So the row holds
// what the filter carries, no publish counts the session as a recipient for a topic its consumer
// never delivers, and the session is told.
//
// It weighs the plan's whole merge, the row it read and the topics requested together: the fifth
// kind is a property of the set, and a topic an earlier build's row holds is left out and named
// as surely as one the body names. No subject this build compiles captures a wildcard topic that
// can feed such a merge, so splitUncapturedTopics takes a requested one off first, and what is
// left of the fifth kind here is a row an earlier build stored, or a topic only a newer build's
// deployed subjects capture.
func planWithoutLeftOut(sessionID string, plan store.InterestPlan) (store.InterestPlan, []filterLeftOut) {
	_, leftOut := sessionFilter(sessionID, plan.Topics)
	if len(leftOut) == 0 {
		return plan, nil
	}
	plan.LeftOut = make([]string, 0, len(leftOut))
	for _, left := range leftOut {
		plan.LeftOut = append(plan.LeftOut, left.topic)
	}
	plan.Topics = slices.DeleteFunc(slices.Clone(plan.Topics), func(topic string) bool { return slices.Contains(plan.LeftOut, topic) })
	return plan, leftOut
}

// registrationPlanAttempts bounds the plans one registration makes. Each conflict or no-op write
// means another writer stored the session's row between a plan and its write, which only an
// unsubscribe, a role claim or another registration of the same session does; three attempts
// cover a burst of those and still end, rather than letting one session's registration spin
// against a writer that keeps winning. Past them the registration is answered the
// bus-unavailable 503 and the plugin's next heartbeat registers again, with nothing stored.
const registrationPlanAttempts = 3

// registeredSession is what a registration that answers left behind: the interest row stored,
// the plan that decided it — whose Roles the warnings are read against — and the session's
// consumer. A registration whose session entry was not written leaves none of it
// (sessionEntryWriteError, listenerDeps.registerPass).
type registeredSession struct {
	row      store.Interest
	plan     store.InterestPlan
	delivery sessionDeliveryOutcome
	// dropped is the topics the edge's hold took out of that plan (edgeTopicHold.holdPlan):
	// topics of the stored row the session may not follow there, which the row and the filter
	// no longer hold and the answer's warnings name. It is empty on every other listener.
	dropped []string
	// epochChanged says the entry this registration replaced recorded no consumer delivery at
	// the same epoch: the plugin connected anew, so the relay restarts the session's consumer
	// before it relays again (sessionRelay.registered). It is read under the session's lock,
	// from the entry the write below replaces.
	epochChanged bool
	// returning says this registration makes the session live again: the record this listener's
	// cache held for it just before the registration wrote its entry read as not live -
	// unresponsive, gone with its claims still standing, or not in the cache at all
	// (session.SessionRegistry.Lookup). Such a holder answers again by registering and claims
	// nothing, since its claim still names its session, so the handler hands it the kept lane of
	// each role whose claim record the registration's renewal read for it, renewed or not
	// (store.RoleRenewal.Set, subscribeHandler). It is read under the session's lock, from the
	// cache with no bucket call, whatever the delivery mode (listenerDeps.registerPass), and is
	// false on every heartbeat of a session that was live already.
	returning bool
	// leftOut is each topic the answering pass's plan kept out of the row and the consumer's
	// filter (planWithoutLeftOut), with what made it go, which the handler names in a warning and
	// a WARN line each.
	leftOut []filterLeftOut
}

// sessionEntryWriteError is the session entry's write failing (listenerDeps.registerPass). That
// write is the one failure of a registration that leaves the interest row stored with nothing
// telling another reader the session is here, so it travels as the registration's error and no
// caller is handed the registration beside it: a caller that acted on one would act on a session
// the registry does not hold. The handler picks it out with errors.As to answer in the session
// registry's own words and not the interest row's (subscribeHandler).
type sessionEntryWriteError struct {
	// cause is what the session registry answered the write with.
	cause error
}

func (e *sessionEntryWriteError) Error() string {
	return fmt.Sprintf("write the session's entry: %v", e.cause)
}

func (e *sessionEntryWriteError) Unwrap() error { return e.cause }

// registerSession plans the session's interest row, filters its consumer on that plan and writes
// it, until the topics the consumer was filtered on are the topics the row holds
// (store.InterestPlan). Two things can part them, and both are another writer storing this
// session's row in the seconds the consumer call takes: the write is refused the revision its
// plan read (store.ErrInterestPlanConflict), or it finds the row already holds everything the
// plan would add and stores nothing, leaving a row wider than the filter. Either way it plans
// again from the row that is there, filters on that plan, and writes again. Answering 200 with
// the filter of an earlier plan would leave a topic the registration was answered 200 for out
// of the consumer until the session's next heartbeat, and every message published on it in that
// window undelivered.
//
// Nothing a pass answers carries into the next, save the gap: a pass that can give no consumer
// answers none, and that answer stands (sessionDelivery.register). The gap is the one thing this
// loop keeps of its own, because only the pass that creates the consumer is told its start fell
// below the stream's first sequence (bus.SessionConsumers.Ensure): a pass that is given no
// consumer answers nothing at all, and a pass that finds the consumer already made answers a gap
// of false (sessionDelivery.consumerFor). The flag starts false, is read out of each pass's
// answer and never back out of it, and the loop sets it on the answer of the pass that answers,
// which is the answer this registration returns. It holds within this registration's passes
// alone: the session's next registration finds the consumer there and is told no gap, so this
// answer is the only one telling the session that messages it would have been delivered are
// gone.
//
// A registration this listener can give no consumer is refused, which the caller answers by
// name: a session is delivered to through its own consumer alone, so such a registration must
// leave no interest row and no session entry behind - the row would make the listener deliver to
// a session nothing reaches, and the entry would say a session is here that receives nothing.
// The consumer is created, or has its filter set to the topics about to be stored, before the
// registration answers, so a message published on a newly followed stored topic after the answer
// is the consumer's to deliver. That refusal is why the consumer call comes before the row and
// not after it: a row written first would outlive a registration the consumer call then refuses.
//
// Each pass runs under the session's consumer lock, and the pass that answers writes the
// session's entry and confirms its consumer under it too (registerPass). The error returned here
// is the plan's, the interest row's or the session entry's, the last as a
// sessionEntryWriteError with no registration beside it.
//
// A registration that is not answered leaves the filter of its last pass on the session's
// consumer, and no entry of its own behind for the repair that judges that filter
// (sessionDelivery.confirmRegistered): the plan it filtered on may be one nothing stored - every
// plan outrun, the row written and the entry refused - while the registration that won the row
// was answered 200 for topics the filter leaves out. So every exit that answers no registration
// holds the filter to the row the bucket holds first (sessionDelivery.storedFilterWrite), on a
// context the client's own cannot cancel but this registration's store budget bounds, since the
// answer waits on it. One deferred settle behind an answered flag is what makes that true of
// every exit, an exit added later included. A registration refused before any consumer call was
// sent has set no filter and settles nothing (deliveryRefusal.filtered).
//
// ctx bounds the store work (sendRequestBudget) and registerCtx the consumer calls, which take
// the request's own context as they always have. projects is what the session's entry records of
// the token that made the registration (registeringProjects). hold is the edge's hold of the
// registration's topics, nil on every other listener: each plan, the first and every one made
// again after a conflict, is held to it before its filter or its row is written
// (edgeTopicHold.holdPlan), since each merges in the stored row, which another writer may have
// given a topic the session may not follow here.
func (d *listenerDeps) registerSession(
	ctx, registerCtx context.Context,
	machineID string,
	body listenerwire.SubscribeRequest, planned, projects []string,
	hold *edgeTopicHold,
	logger *logging.Logger,
) (registeredSession, *deliveryRefusal, error) {
	// The gap any pass of this registration found, which the answer below carries whichever pass
	// answers.
	gap := false
	// Whether a pass of this registration sent a consumer call, which is the call that sets the
	// filter: what the settle below holds to the bucket's newest row.
	filtered := false
	// answered is set on the one return that answers this registration, and the settle below
	// runs on every other exit.
	answered := false
	// The settle holds the filter of a registration this listener did not answer to the row the
	// bucket holds. A registration filters the session's consumer on the plan it is about to
	// store, before it stores it (sessionDelivery.consumerFor), and the repair that follows its
	// session entry is what judges that filter against the bucket's newest record and row
	// (sessionDelivery.confirmRegistered). A registration that reaches no entry - every plan it
	// made was outrun, or its entry write failed - leaves the filter of a plan nothing stored
	// over the filter the registration that won the row set: that session is answered 200 for a
	// topic its consumer does not filter on, at the cost fenceFilterWrite's doc states.
	//
	// It writes the filter alone and creates no consumer, where the repair of an answered
	// registration creates one: this registration was answered nothing, so it promises no
	// consumer, and a session whose consumer a delete took away has one again at its next
	// registration. A session the bucket holds no row for is left filtering on its own agent
	// subject alone, which is every topic such a row would name.
	//
	// The session's lock, the call budget and the listener that holds no session consumers at
	// all are sessionDelivery.storedFilterWrite's, as they are every caller of it; nothing here
	// holds that lock, since the pass that set the filter released it by then (registerPass).
	//
	// It is deferred rather than written at each exit that answers no registration because an
	// exit added later would otherwise leave the filter of a plan nothing stored on the
	// session's consumer, which is the bug the settle was added for.
	defer func() {
		if answered || !filtered {
			return
		}
		// The settle outlives the client's own context, as the confirm of an answered
		// registration does: the filter it holds to the bucket's row stands whatever the client
		// does with this answer. It does not outlive the budget this registration's store work
		// had (sendRequestBudget), because the answer waits on it: a bucket that stopped
		// answering has spent that budget already, and a fence that cannot read the bucket can
		// judge nothing anyway (fenceFilterWrite), so holding the 503 for it would only spend
		// the caller's attempt (write_routes_frozen_bus_test.go).
		settleCtx := context.WithoutCancel(ctx)
		if deadline, ok := ctx.Deadline(); ok {
			var cancel context.CancelFunc
			settleCtx, cancel = context.WithDeadline(settleCtx, deadline)
			defer cancel()
		}
		d.consumers.storedFilterWrite(settleCtx, d.registry, body.SessionID, filterAfterUnansweredRegistration)
	}()
	for attempt := 1; attempt <= registrationPlanAttempts; attempt++ {
		plan := store.InterestPlan{}
		var dropped []string
		var err error
		if attempt == 1 {
			plan, err = d.registry.PlanInterest(ctx, body.SessionID, planned)
		} else {
			plan, err = d.registry.PlanInterestAfterConflict(ctx, body.SessionID, planned)
		}
		if err == nil {
			plan, dropped, err = hold.holdPlan(ctx, plan)
		}
		if err != nil {
			return registeredSession{}, nil, err
		}
		// The topics the session's consumer filter leaves out as no topic it can follow are kept
		// out of the row this pass filters on and writes, an earlier build's row's own included.
		var leftOut []filterLeftOut
		plan, leftOut = planWithoutLeftOut(body.SessionID, plan)
		registered, answeredPass, refusal, err := d.registerPass(ctx, registerCtx, machineID, body, plan, projects)
		registered.dropped = dropped
		if refusal != nil {
			filtered = filtered || refusal.filtered
			return registeredSession{}, refusal, nil
		}
		// The pass was given its consumer, so its call was sent and the filter it carried is the
		// plan's.
		filtered = true
		// The gap is read out of each pass's answer and never back out of it: only the pass
		// that creates the consumer is told of a gap, and a later pass finds the consumer
		// already made and answers none (sessionDelivery.consumerFor).
		if registered.delivery.answer != nil && *registered.delivery.answer.Gap {
			gap = true
		}
		// A pass that answers carries no error (listenerDeps.registerPass), so the switch
		// below is left every error there is, the entry write's among them.
		if answeredPass {
			if registered.delivery.answer != nil {
				registered.delivery.answer.Gap = &gap
			}
			registered.leftOut = leftOut
			answered = true
			return registered, nil, nil
		}
		switch {
		case err == nil:
			logger.Warn("listener plans the session's interest again: the row it stored is not the one it filtered on",
				slog.String("session_id", body.SessionID), slog.Int("plans", attempt))
		case errors.Is(err, store.ErrInterestPlanConflict):
			logger.Warn("listener plans the session's interest again: another writer stored the row",
				slog.String("session_id", body.SessionID), slog.Int("plans", attempt))
		default:
			return registeredSession{}, nil, err
		}
	}
	return registeredSession{}, nil, fmt.Errorf("write %s's interest record: %w after %d plans",
		body.SessionID, store.ErrInterestPlanConflict, registrationPlanAttempts)
}

// registerPass is one pass of a registration: it filters the session's consumer on plan, the
// topics the interest row is about to hold, writes that row, and on the pass that answers - the
// one whose stored row holds the topics it filtered on - writes the session's entry and confirms
// the consumer that entry promises.
//
// The session's consumer lock is taken by the consumer call that sets the filter
// (sessionDelivery.register returns it held, as the func that releases it) and released when
// this function returns, on every path out of it and on a panic with it. It is no re-entrant
// lock, so the pass takes it once and releases it once. A refusal is decided before it is taken
// and hands back noUnlock, which releases nothing.
//
// What the lock closes is the window from that filter to the row and the entry written under it.
// A relay restart rebuilds the filter from the stored row (sessionRelays.restart), so one
// landing between the filter and the row would set the filter back to a row without the topic
// this registration is answered 200 for, and every message published on that topic until the
// session's next heartbeat would be skipped: the row write then matches its plan, so the loop
// above ends content and nothing restores the message. The one consumer delete made on this
// listener - a reaped record's, a deregistration's or the sweep's - runs under the same lock
// and re-reads the session's record under it (sessionDelivery.deleteConsumer), so one landing
// between the consumer call and the entry that says the session is registered would leave a
// session answered 200 with no consumer. Under the
// lock it runs wholly before this pass or wholly after it. A delete made on another listener
// takes no lock of this process, and the bucket orders that one: it reads the record after its
// own delete, and this pass reads the consumer after its entry write, so whichever of the two
// acts last repairs.
//
// Nothing the handler does with what comes back - the warnings it reads from the plan, the relay
// it tells, the answer it writes - is under the lock: a reader of this session's row and
// consumer waits on none of it (subscribeHandler).
//
// Whether this pass answers the registration comes back in a bool of its own: every pass leaves
// a registration behind, and that bool is what tells the loop which of them the registration is
// answered from, without the loop comparing the stored row to the plan a second time. It is
// true only beside a nil error, so a caller reading it has a registration to answer from: a
// pass whose entry write failed answers false and leaves no registration at all, only the error
// (sessionEntryWriteError). The gap is the loop's own: it reads the answer this pass gives and
// sets the gap on the answer of the pass that answers (registerSession).
func (d *listenerDeps) registerPass(
	ctx, registerCtx context.Context,
	machineID string,
	body listenerwire.SubscribeRequest, plan store.InterestPlan, projects []string,
) (registeredSession, bool, *deliveryRefusal, error) {
	outcome, refusal, unlock := d.consumers.register(registerCtx, body, plan.Topics)
	defer unlock()
	if refusal != nil {
		return registeredSession{}, false, refusal, nil
	}
	registered := registeredSession{plan: plan, delivery: outcome}
	row, err := d.registry.UpsertPlanned(ctx, store.Interest{
		SessionID: body.SessionID,
		MachineID: machineID,
		Dir:       body.Dir,
	}, plan)
	if err != nil {
		return registered, false, nil, err
	}
	registered.row = row
	if !slices.Equal(row.Topics, plan.Topics) {
		return registered, false, nil, nil
	}
	// Whether the session was live before this registration is read before its entry is
	// written, from this listener's cache with no bucket call, so a heartbeat pays nothing for
	// it: only a registration that returns the session hands its roles' kept lanes over
	// (subscribeHandler). A cache that trails the bucket can read a session another listener has
	// just registered as not live still, which costs a handover that finds nothing to do.
	_, before := d.sessions.Lookup(body.SessionID)
	registered.returning = before != session.Live
	// Every registration answered writes the session's entry: it holds the session's consumer
	// and is relayed (sessionDelivery.register), which is the only way a message reaches a
	// session. The entry is what tells every other reader that the session is registered, and
	// the repair made after it is this registration's side of the invariant that a session
	// answered 200 with consumer delivery holds a consumer filtered on the topics it was
	// answered for, whatever delete raced it, on whatever listener: a delete on another
	// listener whose record read came before this write found no record to restore from, and
	// this repair reads the bucket's newest record and interest row and makes the consumer what
	// they ask for, creating it where that delete took it away and setting its filter where the
	// live one is not what that row builds (sessionDelivery.confirmRegistered).
	if outcome.relayed() {
		previous, err := d.sessions.Get(body.SessionID)
		registered.epochChanged = err != nil || previous.Delivery != session.DeliveryConsumer || previous.Epoch != outcome.epoch
	}
	// The gap mark the session is owed (session.SessionEntry.RepositoryGap) is the write's own to
	// carry over from the entry it replaces, read at the revision it writes over
	// (session.SessionRegistry.PutWithin), so a registration never clears a mark no frame has
	// carried, also one another listener set after this listener's copy last read the entry.
	entry := sessionEntryFromSubscribe(body, machineID, outcome, projects)
	if err := d.sessions.PutWithin(ctx, body.SessionID, entry); err != nil {
		// The row is stored and nothing tells another reader the session is here, so the
		// registration travels as this error alone and no caller is handed it. The pass
		// answers nothing: the error is the registration's, and the caller's switch over it
		// returns it rather than planning again.
		return registeredSession{}, false, nil, &sessionEntryWriteError{cause: err}
	}
	if outcome.relayed() {
		// The write stands whatever the client does with the answer, so the consumer this
		// entry promises is confirmed on a context the client's own cannot cancel, as the
		// consumer delete of a deregistration is (sessionDelivery.deleteAfterAnswer).
		d.consumers.confirmRegistered(context.WithoutCancel(registerCtx), d, body.SessionID, outcome.ackFloor)
	}
	return registered, true, nil, nil
}

// subscribeHandler answers POST /v1/interests/subscribe. Its store work - the interest record's
// read and write (store.Registry.PlanInterest, then UpsertPlanned), the session entry's write,
// the renewal of the role claims this registration's own process holds
// (store.Registry.RenewRoleClaims) and the unwired-repository checks - runs within
// sendRequestBudget of the request's own context, so a bus that does not answer is the
// bus-unavailable 503 with Retry-After: 1 inside the caller's attempt (writeStoreError); a
// refused key is still its 413 or 400.
//
// The renewal is the one thing here that cannot fail the registration. It runs last, once the
// records this route owns are written, and whatever it could not do is logged and passed over: a
// bucket call that did not answer, a claim record this build cannot read, a renewal that lost its
// compare-and-swap. A heartbeat refused for any of those would take down the very sessions whose
// claims it keeps alive.
//
// Passed over is not forgotten. A claim the renewal could not read or write is recorded against
// its role and this session (store.Registry.RenewalFailedWithin), and so is every role the
// session may hold when the registration fails before its renewal runs - the interest record's
// read or write, or the session entry's write - since such a registration renews nothing
// (store.Registry.RecordRenewalFailures). That record is what keeps POST /v1/roles/set from
// reading a claim this listener failed to renew as one its holder abandoned
// (roleOwnSessionVerdict); store.Registry.RenewalFailedWithin says what clears it and how long
// it stands.
//
// A registration that makes its session live again also hands that session the kept lane of each
// role whose claim record its renewal read for the session (store.RoleRenewal.Set: the role topics
// of its stored row and the roles the role claim copy names for it), renewed or not
// (runKeptRoleDrain), as a claim that lands does (roleSetHandler). Such a holder - one back from
// unresponsive, or from gone with its claim still standing - answers again by registering and by
// nothing else: pi-envoy's and claude-envoy's reassertRole read the role, find the session holding
// it and claim nothing. So the registration is the event that makes the holder live again, and the
// handover starts off it rather than being left to the sweep, which would hold the copies for up to
// roleWakeInterval and drop each one whose hour ended in that wait while its holder was live
// (sweepKeptRoleLanes). A process that resumed the session can claim its role before it
// registers, and the claim route's refresh of the session's registration then makes it live
// again first, so that route hands over the lane of each role the role claim copy names for the
// session, whether or not its claim lands (roleSetHandler), and this registration reads the
// session as live already.
//
// The set is the roles weighed and not the roles renewed because a session resumed by a new
// process registers under a client instance its claim does not name: the claim still names the
// process that stopped, so the renewal renews nothing (store.Registry.RenewRoleClaims), and the new
// process's own soft claim is refused until that claim's last renewal is older than the live
// window (roleOwnSessionVerdict), yet the role is the session's and its copies are owed to it now.
// The handover resolves the holder itself, by session and not by process (roles.ResolveHolder):
// the copies of a role whose claim names this session, whichever process the claim names, are
// forwarded to this session, which reads live once this registration's entry is written; the
// copies of a role whose claim names another live session are that holder's to take; and a role
// whose claim names no live session has its lane walked with no holder.
//
// It costs one handover per role in the set, and only on a registration that returns its session
// (registeredSession.returning): a heartbeat of a session that was live already starts none and
// costs nothing it did not cost before. A handover reads the lane's head before it resolves the
// holder (readKeptLaneHead, two stream reads), so a role with nothing kept for it costs those two
// reads, and a cache that trails the bucket and reads a live session as not live costs at worst a
// handover that finds nothing to do. Each runs in a goroutine under a context of its own, so this
// answer waits for none of it.
func subscribeHandler(d *listenerDeps, machineID string, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body listenerwire.SubscribeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if refusal := sessionNotBound(r, logger, body.SessionID); refusal != "" {
			writeJSONError(w, http.StatusForbidden, refusal, "session_id")
			return
		}
		logger.Info("listener subscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics), slog.Int("port", body.Port), slog.Bool("self_subscribed", body.SelfSubscribed), slog.String("dir", body.Dir), slog.String("plugin", body.Plugin), slog.String("plugin_version", body.PluginVersion))
		// A registration naming a serving port is refused before anything is stored: this
		// listener opens no connection to a session, so a stored registration carrying one
		// would leave its session waiting for deliveries that never come. The refusal names the
		// field, as every other refusal of this route does, and says what to send instead.
		if body.Port > 0 {
			writeJSONError(w, http.StatusBadRequest, "port is not supported: this listener delivers nothing to a session's port. Register with no port (port 0) and receive the session's messages from its own consumer (delivery.consumer).", "port")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), d.registrationBudgetOr())
		defer cancel()
		// At the edge each topic the registration names is held to the token's projects, and a
		// GitHub topic to the repositories their issues resolve to, read fresh for it within this
		// route's budget, before anything is planned or stored; each plan's topics, the stored
		// row's among them, are held the same way below (edgeTopicHold).
		hold := newEdgeTopicHold(r, d.repositories, body.SessionID, logger)
		topicRefusal, err := hold.refusal(ctx, body.Topics)
		if err != nil {
			hold.answerRepositoryOutage(ctx, w, d.registry, d.sessions, machineID, err)
			return
		}
		if topicRefusal != nil {
			logger.Warn("edge listener refused a registration", slog.String("session_id", body.SessionID),
				slog.String("reason", topicRefusal.body.Reason), slog.Any("refused", topicRefusal.body.Refused))
			topicRefusal.write(w)
			return
		}
		// At the edge every claim the session holds of a role of none of the token's projects,
		// which another listener granted it or a token that listed more did, is released before
		// anything of the registration is stored, and the answer's warnings name it
		// (store.Registry.ReleaseRefusedRoleClaims). A release that fails refuses the registration
		// with the retryable 503 (writeEdgeReleaseRefusal): the claim stands, and nothing else
		// frees it while the session lives. The roles weighed are the stored row's role lanes and
		// the claim copy's, and the role bucket's own claims while that copy cannot be served.
		released, err := hold.releaseRefusedRoles(ctx, d.registry)
		if err != nil {
			logger.Warn("edge listener could not release a role claim of a project the session's token does not list; the registration is refused 503",
				slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
			writeEdgeReleaseRefusal(ctx, w, err)
			return
		}
		// The relay holds the session's frames to the projects of the token this registration was
		// admitted on, which it keeps in memory and never reads back from a stored record
		// (edge_frames.go). It is recorded before the registration writes the session's entry, so
		// the relay the entry's write starts already has it.
		d.consumers.admitHeld(r, body.SessionID)
		// One read and one merge decide this registration's topics, which are both the row it
		// stores and the filter of the session's own consumer (store.Registry.PlanInterest). The
		// claim of the controller role behind a project's notice lane
		// (notifications.legion.<project>.controller, the daemon's notify.ControllerTopic) is read
		// for the topics the body names and for the row it already holds together, since either
		// may name that lane, which its holder registers because a Legion controller receives
		// that lane from its own consumer now and from no core subscription of its own
		// (INFRA-771): a heartbeat whose body leaves the lane out therefore keeps it in the filter
		// and in the row alike, and the write below stores this plan rather than deciding it a
		// second time (store.Registry.UpsertPlanned).
		//
		// The session holds that role when the role's claim record names it, read from the claim
		// bucket and not from this listener's copy of the claims, which can lack a claim the
		// store gives the session or name one the store has moved to another session. The claim
		// is read only when one of the two names such a lane, so no other registration waits on
		// it, and a registration whose claim read did not answer or failed is refused the
		// bus-unavailable 503 rather than stored without the lane: the stored topics are the
		// consumer's filter, and a notice published while the lane is out of it reaches the
		// controller on no later path. The row's own read is the registry cache's on every
		// heartbeat, and one that fails refuses the registration as the write it precedes would.
		requested := append(slices.Clone(body.Topics), contracts.LegacyAgentSubject(body.SessionID))
		// The topics that would deliver to no session are split off once, here
		// (splitUncapturedTopics): they are neither planned, stored nor filtered on, and the
		// loop below names each in its warning, after the private rule has had its say. The
		// merge itself drops a private-subject topic from the plan (store.MergedTopics), and each
		// pass keeps out of its plan the topics the session's consumer filter leaves out as no
		// topic it can follow (planWithoutLeftOut), which the answer names below.
		planned, uncaptured := d.consumers.splitUncapturedTopics(requested)
		registered, refusal, err := d.registerSession(ctx, r.Context(), machineID, body, planned, registeringProjects(r), hold, logger)
		if refusal != nil {
			logger.Warn("listener refused a registration it can give no session consumer",
				slog.String("session_id", body.SessionID), slog.String("reason", refusal.Reason), slog.Int("status", refusal.status))
			writeJSON(w, refusal.status, refusal)
			return
		}
		// The registration writes the session's entry under the lock it holds, and a write the
		// session registry did not answer is the registration's failure in that registry's own
		// words and not the interest row's: the row is stored, and nothing tells another reader
		// the session is here (sessionEntryWriteError). It is answered before the ordinary store
		// error below, and nothing past here reads a registration whose entry was never written.
		//
		// Either failure ends the registration before its renewal, so no claim the session
		// holds is renewed by this heartbeat. Each is recorded as a failed renewal of every role
		// the session may hold - the roles the role claim copy names for it and the role topics
		// of its cached interest row, read with no bucket call, since the bus is what just
		// failed - before the answer, so a second process of the session is not granted a role
		// whose renewal this listener could not make (store.Registry.RecordRenewalFailures,
		// roleOwnSessionVerdict).
		if err != nil {
			// Only the edge's hold of a plan's topics reads repositories (edgeTopicHold.holdPlan):
			// a stored GitHub topic whose answer could not be read is the edge's own 503, answered
			// as the body's is.
			if errors.Is(err, errRepositoriesUnknown) {
				hold.answerRepositoryOutage(ctx, w, d.registry, d.sessions, machineID, err)
				return
			}
			d.registry.RecordRenewalFailures(body.SessionID, err)
			var entryErr *sessionEntryWriteError
			if errors.As(err, &entryErr) {
				logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", entryErr.cause.Error()))
				writeStoreError(ctx, w, fmt.Errorf("session registry unavailable: %w", entryErr.cause), http.StatusServiceUnavailable)
				return
			}
			logger.Warn("listener could not register the session's interest",
				slog.String("session_id", body.SessionID),
				slog.Any("topics", body.Topics),
				slog.String("error", err.Error()))
			writeStoreError(ctx, w, err, http.StatusServiceUnavailable)
			return
		}
		item, plan, delivery := registered.row, registered.plan, registered.delivery
		// A topic this registration did not store is named in a warning, with the rest of the
		// registration stored, rather than refusing the registration whole: a session reads only
		// what its own consumer holds and a refusal leaves it no consumer at all, while neither
		// kind of topic need have been typed by anyone. A released pi-envoy registers every
		// subject it has open, a fork parent's agent subject and any topic its envoy_subscribe
		// took included, and a session registers the topics of the row an earlier build stored
		// for it, the bare notifications.team such a build expanded notifications.team.> to
		// included.
		//
		// A topic reaching a private stream subject that is not this session's (another session's
		// inbox, a failed-delivery lane, another project's controller notice lane;
		// bus.ReachesPrivateStreamSubject) is out of the plan (store.MergedTopics): the topics
		// stored drive the listener's fan-out (`store.Registry.Match`) and
		// the filters of the session's own consumer (`sessionConsumerFilter`), and such a topic
		// would hand this session messages meant for another session or for another project's
		// controller. The plan drops such a topic from the stored row too, so a row an earlier
		// build stored with one, and the lane of a session that has since lost the controller
		// role, are cleaned by this registration.
		//
		// A topic that would deliver to no session was split off above: such an interest would be
		// stored and deliver nothing. What a caller names by hand is still refused by name, where
		// the caller is there to read it: /v1/messages/publish refuses such a publish, and either
		// plugin's explicit subscribe refuses such a topic in this warning's own words.
		//
		// The private rule is asked first, as the plugins' own refusal asks it
		// (subscribeRefusesTopic in @legion/envoy-client's stream-topics.ts): a wildcard such as
		// notifications.> or notifications.*.x reaches a private stream subject and is captured
		// by no stream subject at all, and what it earns is the private warning. The uncaptured
		// sentence tells such a caller to follow a notifications.team.<name> topic instead,
		// which is no answer to a wildcard over every session's inbox.
		//
		// A topic the session's consumer filter leaves out as no topic it can follow, one that
		// does not begin with the literal notifications. and a wildcard that fed a merge of the
		// filter reaching another session's private subjects, is out of the plan and the row
		// (planWithoutLeftOut), the row's own included, so no publish counts the session as a
		// recipient for it. Each is named once, in a warning that says why, after the loop below,
		// which leaves it to that warning, and in a WARN line of its own (filterLeftOutLine): a
		// topic that leaves the filter is never left silently.
		//
		// The roles are the last plan's, which decided the row that was stored. A topic of the
		// stored row that the edge's hold dropped from that plan (edgeTopicHold.holdPlan) is named
		// last: it is gone from the row and from the consumer's filter.
		warnings := make([]string, 0)
		for _, topic := range requested {
			if bus.ReachesPrivateStreamSubject(body.SessionID, topic, plan.Roles) {
				warnings = append(warnings, fmt.Sprintf("%s was not registered: it reaches a private stream subject (another session's inbox, a failed-delivery lane, or a Legion controller notice lane), and of those a session registers only its own agent subject, %s, and the notice lane of a project whose controller role it holds",
					topic, contracts.LegacyAgentSubject(body.SessionID)))
				continue
			}
			if slices.Contains(plan.LeftOut, topic) {
				continue
			}
			if _, dropped := uncaptured[topic]; dropped {
				warnings = append(warnings, contracts.UncapturedTopicRefusal(topic, body.SessionID))
			}
		}
		for _, topic := range registered.dropped {
			warnings = append(warnings, edgeDroppedWarning(topic, hold.projects))
		}
		if len(warnings) > 0 {
			logger.Warn("listener subscribe left topics out of the registration",
				slog.String("session_id", body.SessionID), slog.Any("dropped", warnings))
		}
		for _, left := range registered.leftOut {
			warnings = append(warnings, left.warning(body.SessionID))
		}
		// The line is logged once per session and topic an hour (registrationLeftOutLogs): a
		// followed topic comes back on every heartbeat, and the answer names it each time.
		logFilterLeftOutLimited(logger, &registrationLeftOutLogs, body.SessionID, registered.leftOut, leftOutByRegistration)
		// A topic the registration stored in the old form of its name, an issue's without its
		// project or a role's or an agent's without one, is stored as the caller wrote it and
		// every reader takes both forms; the answer names its new form, and the registry counts
		// it (store.Registry.LegacyTopicWarnings). Only the caller's own topics are weighed, so
		// the agent subject this route adds itself is never counted for a caller that has moved.
		var stored []string
		for _, topic := range body.Topics {
			if _, dropped := uncaptured[topic]; !dropped && !bus.ReachesPrivateStreamSubject(body.SessionID, topic, plan.Roles) && !slices.Contains(plan.LeftOut, topic) {
				stored = append(stored, topic)
			}
		}
		warnings = append(warnings, d.registry.LegacyTopicWarnings(body.SessionID, stored)...)
		if delivery.relayed() {
			// The mark tells it which of its runs restarted after this registration: those
			// need no restart for its epoch (sessionRelays.apply).
			d.consumers.relay.registered(body.SessionID, registered.epochChanged, delivery.heard)
		}
		// The registration is this process's heartbeat, so it is where the claims it holds
		// say they are still held: every claim naming this session and this client instance
		// is stamped renewed, and a claim that names no process at all is adopted by the
		// first registration of the session holding it that states a process, while no
		// registration that states none has renewed it (store.RoleClaim.RenewedWithoutProcess).
		// A registration stating no process renews the claims of this session that name none
		// and adopts nothing, so the claim of a client that states none ages out for nobody
		// while it runs. The write is the registration's last, so a renewal never costs the
		// records above it.
		//
		// The roles it weighs are the role topics of the stored row this registration's own
		// UpsertPlanned returned - the bucket's row, read by that write and merged with the
		// plan, so no cache that has not yet followed the bucket hides one - and the roles the
		// role claim copy names for the session, and each of them is renewed only as its own
		// claim record, read here, says (store.Registry.RenewRoleClaims). A claim the copy lost
		// is renewed from the row, and one the copy names that the store gives another session
		// or nobody is left alone. While the copy cannot answer, the row's roles are the set,
		// which is no failure. Nothing on the row decides a renewal on its own: a claim a
		// hand-back could not free carries that hand-back as a mark of its own
		// (store.RoleClaim.HandedBack, written by POST /v1/interests/unsubscribe), and no
		// registration renews a marked claim, which the role claim reaper then frees at its
		// first pass past the live window.
		//
		// A role whose claim read or write did not answer or failed is the renewal's failure:
		// it is in the error logged below, and the renewal records it against that role and
		// this session, where POST /v1/roles/set reads it (roleOwnSessionVerdict);
		// store.Registry.RenewalFailedWithin says what clears that record and how long it
		// stands. A lost compare-and-swap, a claim another session or process holds, a
		// handed-back claim and a role with no claim record are none: the record said what
		// stands.
		//
		// At the edge the renewal is held to the token's projects as a new claim is
		// (edgeTopicHold.holdsRole, which holds every role off the edge): a claim of a role of
		// none of them was released above, and one granted since is not renewed.
		renewal, err := d.registry.RenewRoleClaims(ctx, body.SessionID, strings.TrimSpace(body.ClientInstanceID), registered.row, hold.holdsRole)
		logger.LogEvent(slog.LevelDebug, "listener subscribe weighed the session's role claims",
			slog.String("session_id", body.SessionID),
			slog.Any("roles", renewal.Set),
			slog.Any("renewed", renewal.Renewed))
		if err != nil {
			logger.Warn("listener subscribe left a role claim unrenewed",
				slog.String("session_id", body.SessionID),
				slog.String("error", err.Error()))
		}
		for _, role := range released {
			warnings = append(warnings, edgeReleasedRoleWarning(role, hold.projects))
		}
		// A registration that returns its session hands that session the kept lane of each role
		// whose claim record the renewal read for it, renewed or not: such a holder claims nothing
		// when it answers again, and one resumed by a new process renews nothing either, since
		// its claim names the process before it. Each handover resolves the role's holder itself.
		// A heartbeat of a session that was live already starts no handover
		// (registeredSession.returning).
		if registered.returning {
			for _, role := range renewal.Set {
				startHandover(*d.delivery, role, runKeptRoleDrain)
			}
		}
		// The session is known for the next 30 days, with the topics just stored, which are the
		// filter of the consumer the registration gave it: a registration reaches here only once
		// that consumer call landed, since every one that could not get its consumer was refused
		// above with nothing recorded. A failed write is this WARN and nothing else: the
		// registration is answered whatever it did (session.KnownSessions).
		if err := d.known.RecordWithin(ctx, session.KnownSession{
			SessionID:    body.SessionID,
			Title:        body.Title,
			Capabilities: body.Capabilities,
			Topics:       item.Topics,
		}); err != nil {
			logger.Warn("listener known-session record not written",
				slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
		}
		warningCtx, cancelWarnings := context.WithTimeout(ctx, unwiredRepositoryWarningTimeout)
		defer cancelWarnings()
		warnedRepositories := make(map[string]struct{})
		for _, topic := range body.Topics {
			parsed, ok := parseGithubRepositoryTopic(topic)
			if !ok {
				continue
			}
			if warning := parsed.warning(topic); warning != "" {
				warnings = append(warnings, warning)
				continue
			}
			if warning := lifecycleOnlyWarning(parsed, topic, body.Topics); warning != "" {
				warnings = append(warnings, warning)
			}
			key := parsed.owner + "\x00" + parsed.repo
			if _, seen := warnedRepositories[key]; seen {
				continue
			}
			warnedRepositories[key] = struct{}{}
			if warning := unwiredRepositoryWarning(warningCtx, d, topic, logger); warning != "" {
				warnings = append(warnings, warning)
			}
		}
		writeJSON(w, http.StatusOK, listenerwire.SubscribeResponse{Interest: item, Warnings: warnings, Delivery: delivery.answer})
	}
}

// registerV1Routes builds the /v1 routes over the listener's dependencies, which main hands over
// only once every store is open, so no handler can see a store that is not. The Legion agent
// registry is the one that may be unavailable, and its routes answer that themselves
// (legionAgentRegistry).
func registerV1Routes(v1 *http.ServeMux, d *listenerDeps, machineID string, logger *logging.Logger) {
	v1.HandleFunc("/v1/interests/subscribe", subscribeHandler(d, machineID, logger))
	v1.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		// The body is trimmed below: a request that leaves client_instance_id out states no
		// process, as listenerwire.UnsubscribeRequest says.
		var body listenerwire.UnsubscribeRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.ClientInstanceID = strings.TrimSpace(body.ClientInstanceID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if refusal := sessionNotBound(r, logger, body.SessionID); refusal != "" {
			writeJSONError(w, http.StatusForbidden, refusal, "session_id")
			return
		}
		logger.Info("listener unsubscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics))
		// The removal runs within sendRequestBudget of the request's own context, as subscribe's
		// write does, and store work that did not answer is the bus-unavailable 503.
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		// The role claims released are the ones this process holds, never the whole session's:
		// a session id names no process, and a second process that resumed this one's id hands
		// back a role it does not hold. This route never forces a release; DELETE
		// /v1/interests/<session_id> is the one that does.
		removed, err := d.registry.Remove(ctx, body.SessionID, body.Topics, store.RoleReleaseAuthority{ClientInstanceID: body.ClientInstanceID})
		if err != nil {
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
			return
		}
		// The session now follows what is left of its topics: its known-session record says so,
		// so a publish stops counting it as a recipient of the ones just removed, and its
		// consumer, when the relay serves it one, stops filtering on them. The record is written
		// within this route's budget and before the consumer call, which has a budget of its own,
		// so the record a publish counts its recipients by never waits on a consumer.
		recordFollowedTopics(ctx, d, body.SessionID, logger)
		d.consumers.refollow(r.Context(), d, body.SessionID)
		// removed is what the store took out of the record, not the topics the request named.
		// The two differ over a role lane topic the record kept: a release that may not free
		// the claim leaves the lane topic standing (store.Registry.Remove), the interest half
		// of a claim that still stands, and echoing the request would tell the client a topic
		// was removed while the record still holds it. A deregistration, which names no topic
		// and deletes the whole record, and a release whose every named topic was kept both
		// answer the empty list, and the store's nil is written as that empty list rather than
		// as null.
		if removed == nil {
			removed = []string{}
		}
		writeJSON(w, http.StatusOK, listenerwire.UnsubscribeResponse{Removed: removed})
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(d, machineID, logger))
	v1.HandleFunc("/v1/roles/", roleGetHandler(d, logger))
	v1.HandleFunc("/v1/registry/", sessionEntryHandler(d))
	v1.HandleFunc("/v1/legion/agents", legionAgentsHandler(d, logger))
	v1.HandleFunc("/v1/legion/agents/sessions/", legionAgentSessionHandler(d, logger))
	v1.HandleFunc("/v1/legion/agents/roles/", legionAgentRoleHandler(d))
	v1.HandleFunc("/v1/legion/projects/", legionProjectHandler(d, logger))

	// A DELETE here removes every interest of a session, so its record is left naming none and a
	// publish counts it for nothing, and its consumer is refollowed on what is left, as the
	// unsubscribe route's are.
	v1.Handle("/v1/interests/", adminInterestsHandler(d.registry, d.sessions, d.repositories, func(ctx context.Context, sessionID string) {
		recordFollowedTopics(ctx, d, sessionID, logger)
		d.consumers.refollow(ctx, d, sessionID)
	}, logger))
	v1.Handle("/v1/sessions", sessionsHandler(d.registry, d.sessions, d.repositories))
	v1.Handle("/v1/sessions/", deleteSessionHandler(d, logger))
	v1.Handle(relaySocketPattern, relaySocketRoute(d.relaySockets))
	v1.HandleFunc("/v1/messages/send", sendHandler(d, logger))
	v1.HandleFunc("/v1/messages/publish", publishHandler(d, logger))
	v1.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	v1.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
}
