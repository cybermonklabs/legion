package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// errStoredCopyUnread is wrapped by the error of a send, a topic publish or a role forward whose
// message id the stream already knows when the listener read no stored copy to answer: the copy a
// duplicate verdict named could not be read (storedCopy), or a send or a topic publish was refused
// while an earlier publish under the same message id was still in process
// (bus.ErrDuplicateInProcess), so that no copy is stored yet. A role forward refused that way is
// tried again as any forward that failed is, and its next answer names the copy (sendToRole). A
// read's cause is named in the text and not wrapped, so no comparison of a read's error (a denial,
// a refused subject) can turn the answer into another one: writeBusUnavailable answers it 503 with
// Retry-After: 1 under storedCopyUnreadablePrefix, whatever the read met. The in-process refusal
// is wrapped whole, since it is the publish's own answer and may follow a first send nothing
// answered (bus.ErrUnacknowledged), which the 503 then says with listenerwire.CodeMaybeStored
// (mayHaveStored). Every error wrapping errStoredCopyUnread is written "<its text>: <reason>", so
// writeBusUnavailable writes the reason alone after the prefix.
var errStoredCopyUnread = errors.New("no stored copy read")

// storedPublish is the publish of an envelope on its own topic that returns the stream's
// acknowledgement: bus.Client.PublishAckWithin for both routes in the listener, and a function of
// the test's own where a test stands in for an answer of the stream's, as keptRolePublish is for
// a keep.
type storedPublish func(ctx context.Context, item contracts.Envelope) (bus.StoredAck, error)

// storedReader reads the message the stream holds on a subject at a sequence or after it:
// bus.Client in the listener, and a reader of the test's own where a test stands in for a read
// that does not answer.
type storedReader interface {
	NextStoredMessage(ctx context.Context, subject string, seq uint64) (bus.StoredMessage, error)
}

// requestSender is who a send or a publish speaks for, which decides how much of a stored copy
// its answer may carry (answer): the event_id, source and source_session of the envelope the
// route built from the request, and, where the request presented a secrets-broker token, the
// session that token proves (brokerCaller).
type requestSender struct {
	eventID, source, session string
	proven                   bool
	tokenSession             string
}

// senderOf is the sender of r, whose route built item from it.
func senderOf(r *http.Request, item contracts.Envelope) requestSender {
	sender := requestSender{eventID: item.EventID, source: item.Source, session: item.SourceSession}
	if caller, ok := brokerCaller(r); ok {
		sender.proven, sender.tokenSession = true, caller.Session
	}
	return sender
}

// answer is held, a copy the stream holds under a request's message id, as the request's answer
// may carry it. The request's own copy (its own event_id, which the bus's resend or a role
// send's own earlier forward or keep stored) is answered whole, and so is a copy whose source
// and source_session are the request's, where, when the copy names a session and the request
// proved one with a broker token, that is the copy's. A copy that names no session names no
// owner a token could prove, so a same-source caller with no session reads it whole with or
// without a token, as a caller on the shared bearer always could. Any other copy is another
// sender's message under a key this caller repeated, which may be known or guessed (a Dispatch
// mention key, a role key taken verbatim), so the answer says only that the key is taken: the
// copy's event_id and its times, with the topic and dedupe key it was stored under and the reply it
// asks for (expects_reply, which every answer of a stored copy names, expectsReplyAnswer), and none
// of what the other sender wrote or who it is (payload, payload_summary, payload_ref, source,
// source_event_id, source_session, sender, trace_id and the rest).
//
// A caller that names no session and the source dispatch reads Dispatch's own copies whole: the
// route trusts a caller's source and an empty session as it always has (sessionNotBound).
func (s requestSender) answer(held contracts.Envelope) contracts.Envelope {
	if held.EventID == s.eventID ||
		held.Source == s.source && held.SourceSession == s.session &&
			(!s.proven || held.SourceSession == "" || held.SourceSession == s.tokenSession) {
		return held
	}
	return contracts.Envelope{
		EventID:      held.EventID,
		Topic:        held.Topic,
		DedupeKey:    held.DedupeKey,
		IssuedAt:     held.IssuedAt,
		ExpiresAt:    held.ExpiresAt,
		KeptAt:       held.KeptAt,
		ExpectsReply: held.ExpectsReply,
	}
}

// publishHeld publishes *item within ctx through publish, marks charge by the stream's answer to
// that publish (senderCharge.published), and leaves in *item the envelope the stream holds for
// it, which it reads through reader, as sender may read it (requestSender.answer). It is the one
// publish of the send route and of the topic publish, so neither can answer an envelope the
// stream does not hold: a route answers *item, and after a duplicate verdict *item is the stored
// copy, not the envelope the route built.
//
// A publish the stream stored as a new copy leaves *item as it was, and duplicate false. A
// duplicate verdict names the copy the stream holds under the same message id, at its sequence
// (bus.StoredAck), and *item becomes that copy as storedCopy reads it, whole for its own sender
// and its ids, times and expects_reply alone for anyone else. duplicate is whether that copy is
// another request's, which is when its event_id is not *item's: a caller's repeat under the same
// idempotency_key built a new envelope, with a new event_id, that no stored message carries. A
// verdict that answers the bus's own resend after a closed connection (bus.StoredAck.Resent) can
// name the copy this request's first send stored, which carries its own event_id: that copy is
// this request's, answered with no duplicate as any new copy is.
//
// A copy that cannot be read, and a refusal of a publish whose message id is still in process
// under an earlier one (bus.ErrDuplicateInProcess), are an error wrapping errStoredCopyUnread,
// with *item untouched, and the charge stays marked by the publish's own answer. A copy that
// cannot be read after the bus sent the publish again (bus.StoredAck.Resent) may be the one this
// request's first send stored, so that error is bus.ErrUnacknowledged too (resentUnread), and its
// 503 says maybe_stored as every answer that may have stored the message does (mayHaveStored).
func publishHeld(ctx context.Context, reader storedReader, publish storedPublish, charge *senderCharge, sender requestSender, item *contracts.Envelope) (duplicate bool, err error) {
	ack, err := publish(ctx, *item)
	charge.published(ack, err)
	if errors.Is(err, bus.ErrDuplicateInProcess) {
		return false, fmt.Errorf("%w: %w", errStoredCopyUnread, err)
	}
	if err != nil || !ack.Duplicate {
		return false, err
	}
	held, err := storedCopy(ctx, reader, item.Topic, *item, ack.Sequence)
	if err != nil {
		if ack.Resent {
			return false, resentUnread{err}
		}
		return false, err
	}
	duplicate = held.EventID != item.EventID
	*item = sender.answer(held)
	return duplicate, nil
}

// resentUnread is a stored copy that could not be read after a resend's duplicate verdict: the
// read's own error, whose text it keeps, and bus.ErrUnacknowledged, since the first send of the
// publish went unanswered and may have stored the copy the verdict names (publishHeld).
type resentUnread struct{ error }

func (e resentUnread) Unwrap() []error { return []error{e.error, bus.ErrUnacknowledged} }

// storedCopy reads, through reader, the copy a duplicate verdict on item's publish to subject
// named at seq: the message the stream holds at seq on subject, item's own topic for a send or a
// topic publish and the holder's agent subject for a role forward (roleSend.deliveredHeld). Its
// message id is made of its dedupe key and the subject it was stored on (bus.ForwardMsgID), and
// the read is of that subject alone, so the message is that copy only when it is at seq itself and
// carries item's dedupe key. Its envelope's own topic is not compared: a role forward stored on a
// holder's agent subject under that holder's own send key (contracts.RoleForwardDedupeKey) keeps
// its role topic, and is the copy a direct send under the same key repeats. Every other answer is
// an error wrapping errStoredCopyUnread:
//
//   - sequence 0, which names no copy, refused before any read as a defence: the read would hand
//     back the subject's first message, another one. nats-server v2.10.29 acknowledged a
//     duplicate at 0 for an id it had staged there (server/jetstream_cluster.go:7984-7996); the
//     2.11 line answers a staged id with the in-process error 10158 instead (:8571-8579 in
//     v2.11.12-RC.1), and v2.12.0 and later stage nothing, so no server the listener admits
//     sends one;
//   - a read that failed or did not answer within ctx;
//   - a stream that holds nothing at seq on subject, as after an operator's delete, or at the
//     edge of the 72-hour age, which equals the duplicate window, where the stream can still
//     remember an id whose copy has aged out: the next message on subject, or none, is no copy
//     of this message, whatever dedupe key it carries;
//   - a message there that does not decode, or carries another dedupe key.
func storedCopy(ctx context.Context, reader storedReader, subject string, item contracts.Envelope, seq uint64) (contracts.Envelope, error) {
	if seq == 0 {
		return contracts.Envelope{}, fmt.Errorf("%w: the stream's duplicate verdict on %s named no sequence", errStoredCopyUnread, subject)
	}
	stored, err := reader.NextStoredMessage(ctx, subject, seq)
	if err != nil {
		return contracts.Envelope{}, fmt.Errorf("%w: read sequence %d of %s: %v", errStoredCopyUnread, seq, subject, err)
	}
	if stored.Sequence != seq {
		return contracts.Envelope{}, fmt.Errorf("%w: the stream holds nothing at sequence %d of %s, and its next message there is at %d",
			errStoredCopyUnread, seq, subject, stored.Sequence)
	}
	var held contracts.Envelope
	if err := json.Unmarshal(stored.Data, &held); err != nil {
		return contracts.Envelope{}, fmt.Errorf("%w: decode sequence %d of %s: %v", errStoredCopyUnread, seq, subject, err)
	}
	if held.DedupeKey != item.DedupeKey {
		return contracts.Envelope{}, fmt.Errorf("%w: sequence %d of %s holds dedupe key %q, not %q",
			errStoredCopyUnread, seq, subject, held.DedupeKey, item.DedupeKey)
	}
	return held, nil
}
