package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/listenerwire"
)

// errStoredCopyUnread is wrapped by the error of a send, a topic publish or a role forward whose
// message id the stream already knows when the listener read no stored copy to answer: the copy a
// duplicate verdict named could not be read (storedCopy). A publish or a role forward the stream
// refuses while an earlier one under the same message id is still in process
// (bus.ErrDuplicateInProcess) is no such error: it is made again until the stream names the
// earlier one's copy (publishPastInProcess, which publishHeld and startRoleForward take), and a
// request that ends first is the 503 under earlierAttemptInProcessPrefix. A read's cause is named
// in the text and not wrapped, so no comparison of a read's error (a denial, a refused subject) can
// turn the answer into another one: writeBusUnavailable answers it 503 with Retry-After: 1 under
// storedCopyUnreadablePrefix, whatever the read met. A copy that may be the request's own also
// wraps bus.ErrUnacknowledged (mayBeOwnUnread), which the 503 then says with
// listenerwire.CodeMaybeStored (mayHaveStored). Every error wrapping errStoredCopyUnread is written
// "<its text>: <reason>", so writeBusUnavailable writes the reason alone after the prefix.
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
// route built from the request, where the request presented a secrets-broker token the session
// that token proves (brokerCaller), and whether the request names the key-taken answer as one it
// reads (listenerwire.ReadsKeyTaken).
type requestSender struct {
	eventID, source, session string
	proven                   bool
	tokenSession             string
	readsKeyTaken            bool
}

// senderOf is the sender of r, whose route built item from it.
func senderOf(r *http.Request, item contracts.Envelope) requestSender {
	sender := requestSender{eventID: item.EventID, source: item.Source, session: item.SourceSession, readsKeyTaken: readsKeyTaken(r)}
	if caller, ok := brokerCaller(r); ok {
		sender.proven, sender.tokenSession = true, caller.Session
	}
	return sender
}

// readsKeyTaken is whether r names the key-taken answer among those it reads, in any of its
// listenerwire.ReadsHeader values, with any case and spacing.
func readsKeyTaken(r *http.Request) bool {
	for _, value := range r.Header.Values(listenerwire.ReadsHeader) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), listenerwire.ReadsKeyTaken) {
				return true
			}
		}
	}
	return false
}

// answer is held, a copy the stream holds under a request's message id, as the request's answer
// may carry it; asked is the envelope the route built for the request. A copy the sender may read
// (mayRead) is answered whole. Any other copy is another sender's message under a key this caller
// repeated, which may be known or guessed (a Dispatch mention key, a role key taken verbatim, the
// Legion daemon's sequential legion-outbox keys), so the answer gives none of what the other
// sender wrote or who it is (payload, payload_summary, payload_ref, source, source_event_id,
// source_session, sender, trace_id and the rest). To a request that reads the key-taken answer
// (listenerwire.ReadsKeyTaken) it says only that the key is taken: the copy's event_id and its
// times, with the topic and dedupe key it was stored under and the reply it asks for
// (expects_reply, which every answer of a stored copy names, expectsReplyAnswer). To any other
// request it is asked, the answer a listener before the key-taken answer gave every repeat, which
// a client that parses every answer as a whole envelope reads, and which the route marks as a
// duplicate (answerStored).
func (s requestSender) answer(held, asked contracts.Envelope) contracts.Envelope {
	switch {
	case s.mayRead(held):
		return held
	case s.readsKeyTaken:
		return contracts.Envelope{
			EventID:      held.EventID,
			Topic:        held.Topic,
			DedupeKey:    held.DedupeKey,
			IssuedAt:     held.IssuedAt,
			ExpiresAt:    held.ExpiresAt,
			KeptAt:       held.KeptAt,
			ExpectsReply: held.ExpectsReply,
		}
	default:
		return asked
	}
}

// mayRead is whether the sender may read held whole: held is the request's own copy (its own
// event_id, which the bus's resend or a role send's own earlier forward or keep stored), or held
// names a session and its source and source_session are the request's, where, when the request
// proved a session with a broker token, that is held's. A copy that names no session has no owner
// a caller can prove, so it is whole only for its own request: a caller that names no session,
// with or without a token, may be anyone that names none, the Legion daemon's role messages
// (no source and no session) and Dispatch's sends among them.
func (s requestSender) mayRead(held contracts.Envelope) bool {
	if held.EventID == s.eventID {
		return true
	}
	return held.SourceSession != "" && held.Source == s.source && held.SourceSession == s.session &&
		(!s.proven || held.SourceSession == s.tokenSession)
}

// publishHeld publishes *item within ctx through publish, marks record's charge by the stream's
// answer to that publish (senderCharge.published), and leaves in *item the answer of the envelope
// the stream holds for it, which it reads through reader, as sender may read it
// (requestRecord.answerStored). It is the one publish of the send route and of the topic publish,
// so neither answers its own envelope for a copy its sender may read, or names another sender's
// words: a route answers *item. It publishes through publishPastInProcess, so a refusal of the
// publish as still in process under an earlier one is made again until the stream names that
// one's copy.
//
// A publish the stream stored as a new copy leaves *item as it was, and duplicate false. A
// duplicate verdict names the copy the stream holds under the same message id, at its sequence
// (bus.StoredAck), and *item becomes the answer of that copy as storedCopy reads it: whole for its
// own sender, and for anyone else the key-taken answer or the envelope the route built
// (requestSender.answer). duplicate is whether that copy is another request's, which is when its
// event_id is not *item's: a caller's repeat under the same idempotency_key built a new envelope,
// with a new event_id, that no stored message carries. A verdict that answers the bus's own resend
// after a closed connection (bus.StoredAck.Resent) can name the copy this request's first send
// stored, which carries its own event_id: that copy is this request's, answered with no duplicate
// as any new copy is.
//
// A copy that cannot be read is an error wrapping errStoredCopyUnread, with *item untouched, and
// the charge stays marked by the publish's own answer. A copy that cannot be read after the
// publish was sent again (bus.StoredAck.Resent: the bus's resend, or publishPastInProcess's after
// an in-process refusal of a send nothing answered) may be the one this request's first send
// stored, so that error is bus.ErrUnacknowledged too (mayBeOwnUnread), and its 503 says
// maybe_stored as every answer that may have stored the message does (mayHaveStored).
func publishHeld(ctx context.Context, reader storedReader, publish storedPublish, record *requestRecord, sender requestSender, item *contracts.Envelope) (duplicate bool, err error) {
	ack, err := publishPastInProcess(ctx, func() (bus.StoredAck, error) { return publish(ctx, *item) })
	record.charge.published(ack, err)
	if err != nil || !ack.Duplicate {
		return false, err
	}
	held, err := storedCopy(ctx, reader, item.Topic, *item, ack.Sequence)
	if err != nil {
		if ack.Resent {
			return false, mayBeOwnUnread{err}
		}
		return false, err
	}
	return record.answerStored(item, held, sender), nil
}

// answerStored makes *item, the envelope a route built for its request, the answer of held, a
// copy the stream or the role's kept lane holds under the request's message id, as sender may
// read it (requestSender.answer). The record's line then names that copy's event_id, whatever the
// answer carries, and, from the envelope built for the request (asked), who asked and with what
// words. duplicate is whether held is another request's copy, which is when its event_id is not
// the built envelope's.
func (rec *requestRecord) answerStored(item *contracts.Envelope, held contracts.Envelope, sender requestSender) (duplicate bool) {
	asked := *item
	rec.asked = &asked
	rec.held = held.EventID
	*item = sender.answer(held, asked)
	return held.EventID != asked.EventID
}

// mayBeOwnUnread is a stored copy that could not be read when it may be the request's own: after a
// resend's duplicate verdict (publishHeld), and after a role forward of the same send that failed
// with no refusal (roleSend.deliveredHeld). It is the read's own error, whose text it keeps, and
// bus.ErrUnacknowledged, since an earlier send went unanswered and may have stored the copy the
// verdict names.
type mayBeOwnUnread struct{ error }

func (e mayBeOwnUnread) Unwrap() []error { return []error{e.error, bus.ErrUnacknowledged} }

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
