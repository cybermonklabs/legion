package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/listenerwire"
)

// publishDeliveryWindow bounds a confirmed send through POST /v1/messages/publish, counted from the
// request's arrival: every read, forward and pause of the send happens inside it. It keeps the
// answer inside envoy-client's 5 s attempt, the Legion daemon's 10 s client and the listener's 10 s
// write timeout, and leaves room for two forwards of roleForwardTimeout, so a forward the stream
// did not acknowledge storing is made again.
const publishDeliveryWindow = 4 * time.Second

// publishRetryPause is the wait between one failed attempt of a confirmed send and the next, so a
// recipient whose connection is down is not sent a burst of forwards that reach nobody.
const publishRetryPause = 100 * time.Millisecond

// sendOutcome is how a confirmed send ended, or how its recipient did. An answer that carries a
// reason carries the outcome's string (listenerwire.RoleSendFailure and CapabilityRefusal). A
// send's log line names the outcome as its reason attribute and deliveryStatus's label for it as
// its delivery_status attribute, so the reason reads as the answer does and delivery_status reads
// as the role lane's own lines and the messagesDelivered count do.
type sendOutcome string

const (
	// sendDelivered is a send the stream stored for its recipient, which reads it from its own
	// session consumer.
	sendDelivered sendOutcome = "delivered"
	// sendDedupe is a recipient this route had already stored this dedupe key for, so it was not
	// sent the message again.
	sendDedupe sendOutcome = "dedupe"
	// sendSkipped is a role send from the role's own holder, the role lane's echo skip.
	sendSkipped sendOutcome = "skipped"
	// sendUnclaimed and sendHolderLapsed are a role send with no live holder that kept nothing -
	// keep: false, a role token no claim can name, or a keep that failed - answered 404
	// (roleSend.noHolder).
	sendUnclaimed    sendOutcome = sendOutcome(roles.Unclaimed)
	sendHolderLapsed sendOutcome = sendOutcome(roles.Lapsed)
	// sendDeliveryFailed is a recipient the stream stored no forward for: none live to forward to,
	// a read that failed or ran out of time, or a forward it did not acknowledge storing.
	sendDeliveryFailed sendOutcome = "delivery_failed"
	// sendCapability is a role holder that cannot take the payload's delivery mode, answered 403.
	sendCapability sendOutcome = "capability"
	// sendKept is a role send with no live holder whose message the listener kept on the role's
	// kept lane instead of refusing it, answered 202 (roleSend.kept).
	sendKept sendOutcome = sendOutcome(roleKeptDeliveryStatus)
	// sendFailed is a send refused as requestRecord.refuseBusUnavailable maps it (a forward or
	// publish NATS refuses or the grant denies), or a role send whose first resolution of the
	// holder failed, answered as GET /v1/roles/<role> answers it.
	sendFailed sendOutcome = "failed"
)

// deliveryStatus is the label the role lane gives the same outcome, which the route counts under
// messagesDelivered and logs as delivery_status, so the route's counts and lines read as the
// lane's: an unclaimed role is skipped, a kept message reads as the lane's own kept, and every
// outcome that is neither taken, skipped nor kept is failed.
func (o sendOutcome) deliveryStatus() string {
	switch o {
	case sendDelivered, sendDedupe, sendSkipped, sendKept:
		return string(o)
	case sendUnclaimed:
		return string(sendSkipped)
	default:
		return string(sendFailed)
	}
}

// forwardWindow is one forward's wait for the stream's acknowledgement that it stored it:
// roleForwardTimeout, or what is left of the send when that is less.
func forwardWindow(ctx context.Context) time.Duration {
	deadline, _ := ctx.Deadline()
	return min(roleForwardTimeout, time.Until(deadline))
}

// pauseBeforeRetry waits publishRetryPause, or until ctx ends, and reports whether the send has time
// left for another attempt.
func pauseBeforeRetry(ctx context.Context) bool {
	return waitUnlessDone(ctx, publishRetryPause) == nil && ctx.Err() == nil
}

// forwardAndWait forwards item to subject, under msgID as its Nats-Msg-Id when that is not empty,
// through the delivery configuration's forwardRole and waits for its outcome, which is the form a
// caller that answers for the forward needs: the publish route answers its sender with what the
// forward did, and the kept lane's drain moves its cursor past the copy (walkKeptRole), where the
// role lane hands the wait over and reports it later. The outcome is the stream's acknowledgement
// that it stored the forward, nothing more: the holder answers no receipt and reads the stored
// copy from its session consumer (startRoleForward).
func (cfg listenerDeliveryHandlerConfig) forwardAndWait(subject string, item contracts.Envelope, msgID string, window time.Duration) (bus.StoredAck, error) {
	wait, err := cfg.forwardRole(subject, item, msgID, window)
	if err != nil {
		return bus.StoredAck{}, err
	}
	return wait()
}

// forwardRefused reports whether a forward failed in a way another attempt repeats: NATS refuses
// the envelope or subject however often it is sent (bus.ErrRefused), or the listener's grant does
// not allow the subject (bus.ErrPublishDenied). A role send answers it at once, as
// requestRecord.refuseBusUnavailable maps it (413 or 400 for a refusal, 403 for a denial).
func forwardRefused(err error) bool {
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrPublishDenied)
}

// confirm records that the stream stored item, the message under its dedupe key, for sessionID
// through the route, at sequence. The route's own record (listenerDeps.confirmed) is what a later
// send of the same key reads, so envoy-client's retry of a send skips a recipient the first
// attempt stored it for; the delivery handlers' cache is told too, so they do not deliver the same
// key to it again. It also records the forward under the request that made it
// (forwardRequest), which a later echo of the same request reads (settledBeforeForward).
func (d *listenerDeps) confirm(item contracts.Envelope, sessionID string, sequence uint64) {
	d.confirmed.RecordStored(item.DedupeKey, sessionID, sequence)
	d.confirmed.RecordForward(forwardRequest(item), sessionID, sequence)
	d.delivery.dedupeCache.Record(item.DedupeKey, sessionID)
}

// forwardRequest is the key the route's record keeps a role forward under for an echo to read:
// everything that makes two role sends the same request, which a repeat of one has in common with
// it - the sender (source and source_session), the role topic and the dedupe key - so a repeat
// reads the forward of its own request and never one another sender, or the same sender to
// another role, made under the same key.
func forwardRequest(item contracts.Envelope) string {
	return strings.Join([]string{item.Source, item.SourceSession, item.Topic, item.DedupeKey}, "\x00")
}

// roleSend is one role message through the publish route. The route does not publish on the role
// lane: it forwards the envelope to the holder's agent subject itself and answers 200 once the
// stream stored that forward for the holder's session consumer, or with the failure once
// publishDeliveryWindow from arrived has passed, so its sender learns whether the message was kept
// for its holder. No holder answers a receipt (startRoleForward), so the 200 is the stream's word
// rather than the holder's. It publishes no delivery exception: the sender has the answer. Every
// answer goes through record, the route's one line per request (request_record.go), and is then
// settled: counted, and logged on the send's own delivery line.
//
// Each answer marks the message's take from its sender's count (record.charge) before it is
// written: a wake reported, a forward the stream stored, a forward that failed with no refusal,
// which the stream may have stored for the holder it went to, and a window that ended cost one.
// The mark of a forward that may have been stored stands whatever the send answers next
// (senderCharge.stored): a forward refused, an echo, a repeat the route's own record or the kept
// lane answers, and a 403. A repeat the kept lane answers is forgiven, which cancels the wake's
// mark alone, and every other answer marks nothing, so where no forward may have been stored the
// route's deferred settle gives the take back.
type roleSend struct {
	w      http.ResponseWriter
	record *requestRecord
	d      *listenerDeps
	// ctx bounds the leader read this send's answers confirm the holder by (shownHolder), the
	// request's own send window (sendToRole).
	ctx context.Context
	// scope is the edge's scope of the request (edgeScopeOf), nil on every other listener: the
	// holders this send's answers may name (edgeScope.holderConfirmed).
	scope *edgeScope
	item  contracts.Envelope
	// expectsReply is the request's own expects_reply bytes, whichever of the field's spellings
	// it wrote, which each answer writes back for the envelope it answers (expectsReplyAnswer,
	// api.go): the forwarded envelope's, or the kept copy's for a repeat a keep answers (kept). The
	// envelope the holder is forwarded is untouched by it, and carries the parsed value.
	expectsReply json.RawMessage
	role         string
	// keep is the request's keep field as the route read it, true when it set none: whether
	// this send's give-up keeps the message for the role's next holder (noHolder). It is the
	// send's own rather than the envelope's, so no copy the listener forwards and no answer it
	// writes carries it.
	keep    bool
	arrived time.Time
	holder  string
	// shown is the resolution that named holder as this send's answers name it (shownHolder,
	// edgeScope.holderConfirmed): the resolution itself on every listener but the edge, and at the
	// edge the resolution of a role no claim holds for a holder the scope does not show, which the
	// send still forwards to. Its HolderState is the state the holder's own record put it in, so the
	// 200 and the 202 of a holder that is not live can say it (GET /v1/roles/<role> says it the
	// same way): a 200 is answered only for a holder a resolution read live, and a 202 kept for a
	// lapsed holder carries unresponsive or gone (kept).
	shown roles.Resolution
	// warnings is what the route's answers name of the topic's old form (store.Registry.ReadRole):
	// the 200 and the 202 carry it, as the subscribe and claim routes' answers carry theirs.
	warnings []string
	attempts int
	// duplicate is whether the forward this send answers is another request's copy the stream
	// already held, which is then the stored forward's envelope (deliveredHeld). A forward this
	// very send made on an earlier attempt, whose acknowledgement was lost, is its own copy and
	// carries no duplicate.
	duplicate bool
	// sender is who the request speaks for, which decides how much of a copy the stream or the
	// kept lane already held the 200 or the 202 may carry (requestSender.answer).
	sender requestSender
	// unanswered is set once a forward of this send failed with no refusal, which may have stored
	// the message for the holder it went to (sendToRole), so a copy a later duplicate verdict names
	// may be that forward's (deliveredHeld).
	unanswered bool
}

// shownHolder is the holder this send's answers may name: result for a holder the scope shows and the
// stream's leader confirms is a writer's record (edgeScope.holderConfirmed), and the resolution of a
// role no claim holds for any other - a server-written holder record, and a confirmation read that
// did not answer, among them. The send forwards to result.Holder whatever this names, so a read that
// did not answer fails closed to naming no holder rather than refusing a delivery that still happens.
// A nil scope names result as it is, at no read's cost.
func (s *roleSend) shownHolder(result roles.Resolution) roles.Resolution {
	readCtx, cancel := context.WithTimeout(s.ctx, roleHolderReadTimeout)
	defer cancel()
	shown, err := s.scope.holderConfirmed(readCtx, s.d.sessions, result)
	if err != nil {
		return roles.Resolution{State: roles.Unclaimed}
	}
	return shown
}

// hold makes result's holder the one this send forwards to, and result as the scope shows it the
// one its answers name (shownHolder).
func (s *roleSend) hold(result roles.Resolution) {
	s.holder, s.shown = result.Holder, s.shownHolder(result)
}

// settle counts and logs the send's outcome, the one delivery line a send writes, once its answer
// is written. It counts and logs as delivery_status the label the role lane gives the same outcome,
// and logs the outcome itself as reason. Its event_id is the copy the stream or the kept lane held
// where the send answered one (requestRecord.held), as the request's own line names it, whatever
// the answer carries.
func (s *roleSend) settle(outcome sendOutcome) {
	s.d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
	level := slog.LevelWarn
	switch outcome {
	case sendDelivered, sendSkipped, sendDedupe, sendUnclaimed, sendKept:
		level = slog.LevelInfo
	}
	eventID := s.item.EventID
	if s.record.held != "" {
		eventID = s.record.held
	}
	s.d.delivery.logger.DeliveryLog(level, "listener publish role send", s.holder, s.item.Topic, eventID, outcome.deliveryStatus(),
		slog.String("reason", string(outcome)),
		slog.String("dedupe_key", s.item.DedupeKey),
		slog.Int("attempts", s.attempts),
		slog.String("elapsed", time.Since(s.arrived).String()))
}

// delivered answers 200 with the envelope, the holder it was stored for and the state that
// holder's record put it in, which GET /v1/roles/<role> names the same way (shown).
func (s *roleSend) delivered(outcome sendOutcome) {
	s.record.holder = s.holder
	s.record.accept(s.w, listenerwire.PublishResponse{Envelope: s.item, ExpectsReply: expectsReplyAnswer(s.expectsReply, s.item.ExpectsReply), Holder: s.shown.Holder, HolderState: s.shown.HolderState, Duplicate: s.duplicate, Warnings: s.warnings})
	s.settle(outcome)
}

// deliveredHeld answers a forward the stream already held on recipient's agent subject, as
// delivered does: 200 with the answer of the forward the stream holds at seq, read back
// (storedCopy) with the forward mark taken off its dedupe key, so the request's record line and
// the delivery line name the copy the stream holds; the forward whole for its own sender, and for
// anyone else the key-taken answer or the envelope this send built (requestSender.answer).
// recipient is the holder this send forwards to, or, for an echo's repeat, the earlier holder the
// route stored this key's forward for (settledBeforeForward); the answer still names the holder
// this send resolved. duplicate is whether the copy is another request's, which is when its
// event_id is not this send's: a forward this very send made on an earlier attempt, whose
// acknowledgement was lost, carries this send's own. A copy that cannot be read, a sequence of 0
// among them, is answered as a refused forward is (refused): the 503 refuseBusUnavailable writes
// for errStoredCopyUnread, marking the charge with nothing more than the forward already did.
// After a forward of this send that failed with no refusal (unanswered), the copy may be that
// forward's, so the 503 says maybe_stored too (mayBeOwnUnread), as the send and topic routes'
// does after a resend (publishHeld). The read's error names recipient's agent subject: refused
// judges the holder this send resolved (shownForwardError), and an echo's earlier holder, which
// this send did not resolve, is named at the edge only where the stream's leader confirms its
// record is a writer's registration the scope shows (edgeScope.namesHolderConfirmed), and
// otherwise answered in the listener's own words (hideHolder); a confirmation read that does not
// answer fails closed to naming no holder, as shownHolder does.
func (s *roleSend) deliveredHeld(ctx context.Context, recipient string, seq uint64, outcome sendOutcome) {
	held, err := storedCopy(ctx, s.d.client, contracts.LegacyAgentSubject(recipient), s.forwarded(), seq)
	if err != nil {
		if s.unanswered {
			err = mayBeOwnUnread{err}
		}
		if recipient != s.holder {
			readCtx, cancel := context.WithTimeout(ctx, roleHolderReadTimeout)
			named, namedErr := s.scope.namesHolderConfirmed(readCtx, s.d.sessions, recipient)
			cancel()
			if namedErr != nil || !named {
				err = hideHolder(s.d.delivery.logger, "edge listener read no stored copy of a role forward to an earlier holder; its answer names no holder",
					s.role, recipient, "read role "+s.role+"'s forward to its earlier holder", err)
			}
		}
		s.refused(err)
		return
	}
	held.DedupeKey = strings.TrimPrefix(held.DedupeKey, roleForwardDedupePrefix)
	s.duplicate = s.record.answerStored(&s.item, held, s.sender)
	s.delivered(outcome)
}

// noHolder answers a role with no live holder. For a state the listener keeps for (roleKeepable:
// a role nobody claims, and a claim whose holder is not live), and while this send's own keep
// flag asks for it, it keeps the message for the role's next live holder and hands the lane to
// whoever holds the role now, both in the one keep both give-up paths take (keepForNextHolder),
// and answers kept's 202. A keep that kept nothing - one this send's keep flag turned off, one
// under a role token no claim can name, a keep that failed, logged by keepRoleMessage - is
// answered as a send that kept nothing is, since the sender's own fallback must run when the
// message was not kept: writeRoleHolderError's 404 (unclaimed or holder_lapsed), which the Legion
// daemon reads as notify.ErrNoHolder, recorded role_unclaimed or role_lapsed, with its refusal
// exception (publishRefusalException). The role lane counts an unclaimed role skipped and a lapsed
// holder failed when its own keep fails. A keep whose publish went out and was never acknowledged
// (bus.ErrUnacknowledged) is answered so too, as at main, although it may have stored the copy:
// what each role route answers is unchanged here, and a sender that publishes again after the
// 404 gives the role's next holder the message once more, which at-least-once delivery allows.
//
// It is also the one place a message to a Legion role of a released issue pod is seen at all, so
// it raises the wake report beside the answer, before either answer and waiting for neither
// (reportRoleWake). Both answers need it: a message kept for the role's next holder waits, for
// one hour at most, until a live session holds the role again, and a publish with keep: false
// keeps nothing and is still answered 404, so only the wake brings the released pod back to claim
// the role, drain what was kept for it (drainKeptRole) and take the sender's next attempt. The
// report names the role and nothing else; which daemon owns it, and whether any claim of that
// role is released, is for the daemons to decide.
func (s *roleSend) noHolder(result roles.Resolution) {
	reportRoleWake(s.d, s.d.delivery.logger, s.role)
	// The wake is what this message costs its sender, whether it is kept or answered 404 below.
	s.record.charge.woke()
	if s.keep && roleKeepable(result) {
		if kept, err := keepForNextHolder(*s.d.delivery, s.role, s.item); err == nil {
			s.kept(result, kept)
			return
		}
	}
	s.record.refuseRoleHolder(s.w, s.role, s.shownHolder(result))
	// The 404 is written; the refusal's own bus signal goes out behind it (publishRefusalException).
	publishRefusalException(s.d.client, s.d.delivery.logger, s.d.delivery.edge, s.item, result)
	outcome := sendHolderLapsed
	if result.State == roles.Unclaimed {
		outcome = sendUnclaimed
	}
	s.settle(outcome)
}

// kept answers 202 with the envelope the role's kept lane holds for the message (kept.item): kept
// is true, kept_reason names the state the resolution found - unclaimed, or holder_lapsed for a
// claim whose holder is not live (roleKeepable) - and kept_until names the end of the copy's hour,
// until which the walk forwards it to the next session that holds the role live. A lapsed
// holder's answer names that holder, its state and when it last registered, as the 404
// holder_lapsed names them, so its sender learns all the 404 told it and that the claim stands.
// The message reaches the role's next live holder through a handover (drainKeptRole), so this is
// an answer rather than a refusal, and it settles under its own outcome so the send still writes
// exactly one delivery line.
//
// A repeat of a dedupe key whose copy is still on the lane inside its hour stored nothing new
// (storeKeptRoleCopy), so the answer comes from that first copy, and the request's record line
// and the send's delivery line name its event id: the copy whole for its own sender, and for
// anyone else the key-taken answer or the envelope this send built (requestSender.answer). Such a
// repeat is forgiven: it costs its sender nothing, though noHolder's wake report marked its
// charge, unless an earlier forward of the same send failed with no refusal, which may have stored
// the message for the holder it went to and marked the charge so (sendToRole).
//
// 202 rather than 200, so a sender tells the three answers of a role send apart by status alone:
// 200, the message is with a live holder; 202, no live session holds the role and the message
// waits on the role's kept lane until kept_until; 404, nothing was kept and the sender's own
// fallback must run, as decided on 2026-10-31.
func (s *roleSend) kept(result roles.Resolution, kept keptRoleCopy) {
	if kept.repeat {
		s.record.charge.forgive()
	}
	// The kept copy is another request's when its event_id is not this send's, whether the keep
	// read it as a repeat or answered its own resend or in-process refusal with it, which the 202
	// says with duplicate as a 200 does (deliveredHeld), and which tells a client the key-taken
	// answer another sender gets (requestSender.answer) from a whole one.
	duplicate := s.record.answerStored(&s.item, kept.item, s.sender)
	s.hold(result)
	s.record.holder = s.holder
	// At the edge a lapsed holder the scope does not show is named as no holder at all, as GET
	// /v1/roles/<role> names it (shownHolder, which hold called).
	answer := listenerwire.PublishResponse{
		Envelope:     s.item,
		ExpectsReply: expectsReplyAnswer(s.expectsReply, s.item.ExpectsReply),
		Holder:       s.shown.Holder,
		HolderState:  s.shown.HolderState,
		Kept:         true,
		KeptReason:   s.shown.State,
		KeptUntil:    kept.until,
		Duplicate:    duplicate,
		Warnings:     s.warnings,
	}
	if s.shown.LastSeen != 0 {
		answer.LastSeen = &s.shown.LastSeen
	}
	s.record.acceptWith(s.w, http.StatusAccepted, answer)
	s.settle(sendKept)
}

// unresolved answers a first resolution of the holder that did not resolve, as GET
// /v1/roles/<role> answers it: a claim record no read can decode (store.ErrMalformedRoleClaim) is
// the 500 that names it, and any other failure - a read that did not answer within its cap or
// sendRequestBudget among them - the bus-unavailable 503, or the 413 or 400 a refused key gets,
// which at the edge names no holder for a holder's session read that did not answer
// (edgeScope.unresolved). Each is recorded under its own outcome.
func (s *roleSend) unresolved(err error) {
	switch {
	case errors.Is(err, store.ErrMalformedRoleClaim):
		s.record.refuseRoleClaimUnreadable(s.w, s.role, err)
	default:
		s.record.refuseBusUnavailable(s.w, s.scope.unresolved(s.role, err, s.d.delivery.logger))
	}
	s.settle(sendFailed)
}

// refused answers a forward NATS refuses however often it is sent, or one the listener's grant
// denies (forwardRefused), as requestRecord.refuseBusUnavailable maps it, and a stored copy that
// could not be read back (deliveredHeld) the same way. The refusal's own text, and the read's,
// names the subject the forward went to, the holder's agent subject, so at the edge, for a holder
// the scope does not show, the answer is in the listener's own words under the same status, and
// the error goes to the log (shownForwardError). The refused forward stored nothing, so it marks
// nothing: an earlier forward of the same send that may have stored the message has marked the
// charge already (sendToRole).
func (s *roleSend) refused(err error) {
	s.record.holder = s.holder
	s.record.refuseBusUnavailable(s.w, s.shownForwardError("edge listener's forward of a role message was refused, or its stored copy not read; its answer names no holder", err))
	s.settle(sendFailed)
}

// shownForwardError is err, a forward's error, as this send's answer may give it. Such an error
// can name the subject the forward went to, the holder's agent subject, so at the edge, for a
// holder the scope does not show (shown, set by hold from shownHolder's leader-confirmed
// resolution, reads unclaimed: edgeScope.holderConfirmed), it is the listener's
// own words with err beneath, which still decides the answer's status, code and Retry-After, and
// err goes to the log under line (hideHolder). Every other holder's answer carries err as it is.
func (s *roleSend) shownForwardError(line string, err error) error {
	if s.shown.State != roles.Unclaimed {
		return err
	}
	return hideHolder(s.d.delivery.logger, line, s.role, s.holder, "forward role "+s.role+"'s message to its holder", err)
}

// timedOut answers the 504 of a send whose window ended with no forward the stream stored for its
// holder, recorded delivery_failed. A 504 costs its sender one whether or not a forward went out,
// so also when the window ended before the first forward, or on resolutions that failed or found
// the holder not live: the limit holds a 504 to be the answer after which the listener cannot say
// whether it stored the message, and does not tell a 504 that forwarded nothing from the rest.
func (s *roleSend) timedOut() {
	s.record.charge.stored()
	s.record.holder = s.holder
	s.record.refuseWith(s.w, http.StatusGatewayTimeout, outcomeDeliveryFailed, listenerwire.RoleSendFailure{
		Error: fmt.Sprintf("role %s: event %s was not stored for its holder %s within %s (%s)",
			s.role, s.item.EventID, cmp.Or(s.shown.Holder, "(none)"), publishDeliveryWindow, sendDeliveryFailed),
		Reason:    string(sendDeliveryFailed),
		EventID:   s.item.EventID,
		DedupeKey: s.item.DedupeKey,
		Holder:    s.shown.Holder,
		Attempts:  s.attempts,
	})
	s.settle(sendDeliveryFailed)
}

// endedInProcess answers a send whose window ended after its last forward ended while an earlier
// forward under the same Nats-Msg-Id was still in process (earlierAttemptInProcess, err): the
// stream may yet store that earlier forward for the holder, so the send is not told nothing was
// stored. It is the 503 with Retry-After and listenerwire.CodeMaybeStored the send and publish
// routes give a request that ends so (writeBusUnavailable), under earlierAttemptInProcessPrefix
// and recorded earlier_attempt_in_process, where a send whose window ends otherwise is the 504
// (timedOut). It costs its sender one, as the 504 does: the forward that ended in process marked
// the charge (sendToRole). err is the error as sendToRole kept it, already judged by the holder
// its forward went to (shownForwardError), so a holder the send resolved since judges nothing.
func (s *roleSend) endedInProcess(err error) {
	s.record.holder = s.holder
	s.record.refuseBusUnavailable(s.w, err)
	s.settle(sendDeliveryFailed)
}

// windowEnded answers a send whose window ended with no forward the stream stored for its holder:
// endedInProcess when its last forward ended in process (inProcess, that forward's error), and the
// 504 (timedOut) otherwise.
func (s *roleSend) windowEnded(inProcess error) {
	if inProcess != nil {
		s.endedInProcess(inProcess)
		return
	}
	s.timedOut()
}

// settledBeforeForward judges the message to a holder the send has not checked yet, whose session
// entry is entry, by beforeForward, the rules the role lane applies before it forwards, and reports
// whether it answered: an echo (the source session holds the role), answered 200 with nothing
// forwarded; a holder this route already stored this dedupe key for, answered as that stored
// forward with nothing forwarded (deliveredHeld: 200 with it, or the stored-copy 503 when it
// cannot be read back); a holder that cannot take the payload's delivery mode, answered 403 with
// the refusal sendHandler gives, recorded capability_refused as sendHandler records it.
//
// An echo is answered with its own envelope, which no forward stored, unless the route's record
// names a forward the stream stored for another holder for the same request: the same sender,
// role and dedupe key (forwardRequest, dedupe.Cache.Forward) - a session that sent to the role,
// took it, and repeats. The record is written for each holder a send of the request was answered
// for past the echo check: a forward the stream confirmed (confirm), and a repeat the route's
// record answered (the forwardRepeat case below), such as another sender's under the same key,
// or the same sender's to another role the same holder holds. So it never names the echo's
// holder, which is the request's own source session (beforeForward).
// The repeat is answered as that stored forward (deliveredHeld on the earlier holder's subject),
// never with the new envelope built for the repeat, whose event_id no stored message carries.
// The record is the listener's own and lasts 10 minutes; a repeat after that or on another
// listener answers its own new envelope, as at main. No stream lookup finds the earlier forward
// by the request alone, since its message id names the earlier holder's subject.
//
// None of those answers marks the charge: each stores nothing, and an earlier forward of the same
// send that failed with no refusal, which may have stored the message for the holder it went to,
// has marked the charge already (sendToRole).
//
// The dedupe check reads the route's own record (listenerDeps.confirmed), not the delivery
// handlers' cache: agentSubjectDelivery and fanoutDelivery record a key there for every delivery
// they make or hand over, so a role send under a key one of them saw would be answered 200 for a
// holder the route never forwarded it to.
func (s *roleSend) settledBeforeForward(ctx context.Context, entry session.SessionEntry) bool {
	verdict := beforeForward(s.item, s.holder, entry, s.d.confirmed.Seen)
	switch verdict.kind {
	case forwardAllowed:
		return false
	case forwardEcho:
		if earlier, seq, stored := s.d.confirmed.Forward(forwardRequest(s.item)); stored {
			s.deliveredHeld(ctx, earlier, seq, sendSkipped)
		} else {
			s.delivered(sendSkipped)
		}
	case forwardRepeat:
		// The route stored this key's forward for this holder, and its record keeps the
		// sequence the stream holds that forward at, which is the copy this repeat answers. A
		// record that lapsed since the verdict read it is forwarded again, which the stream
		// answers as a duplicate naming the same copy.
		seq, recorded := s.d.confirmed.Stored(s.item.DedupeKey, s.holder)
		if !recorded {
			return false
		}
		// Recorded under this request too, as a forward the stream confirmed is (confirm), so
		// its sender's repeat after it takes the role, an echo, answers the same copy.
		s.d.confirmed.RecordForward(forwardRequest(s.item), s.holder, seq)
		s.deliveredHeld(ctx, s.holder, seq, sendDedupe)
	default:
		s.record.holder = s.holder
		// The refusal names the holder and its title, which an edge answer names only for a
		// holder the scope shows (shown).
		refusal := verdict.refusal
		if s.shown.State == roles.Unclaimed {
			refusal = fmt.Sprintf("the holder of role %s cannot take this message's delivery mode", s.role)
		}
		s.record.refuseWith(s.w, http.StatusForbidden, outcomeCapabilityRefused, listenerwire.CapabilityRefusal{
			Error:  refusal,
			Reason: string(sendCapability),
			Holder: s.shown.Holder,
		})
		s.settle(sendCapability)
	}
	return true
}

// sendToRole delivers a role message through the publish route, answering through record. The
// first resolution reads the holder as an HTTP lookup does (roles.ResolveHolder), within
// sendRequestBudget of arrived as GET /v1/roles/<role> reads it, and gives that route's answers:
// a role with no live holder - no claim, or a claim whose holder is not live - is kept for the
// role's next live holder and answered 202 while keep asks for it, the request's own keep field,
// true when it set none (roleKeepable), else 404 unclaimed or holder_lapsed (noHolder); and for a
// resolution that failed the lookup's 503 or 500 (unresolved). The first attempt forwards to that
// holder without reading the claim again; every other attempt resolves the holder again, so a
// holder that relaunched under a new session between attempts gets the next one. On those
// attempts an unclaimed role is answered as the first resolution answers it, since the claim
// itself is gone; a holder that stopped answering keeps its claim and fails that attempt only, as
// a read that failed does, so a holder that comes back inside the window still gets the message,
// and a send whose window ends on such attempts is a 504 that kept nothing: the sender is told the
// send failed for the holder its first resolution read live. Each attempt forwards the same
// envelope, under the role forward's dedupe key and under one Nats-Msg-Id, the key and the
// holder's subject (bus.ForwardMsgID), and waits what is left of the send, at most
// roleForwardTimeout, for the stream's acknowledgement that it stored it; that acknowledgement is
// the send's 200, since the holder answers nothing and reads the stored copy from its session
// consumer (startRoleForward). The stream, which captures agent subjects, therefore keeps one copy
// of the forward however many attempts there are. Every read takes ctx, the send's window, so
// none outlasts it: a send whose window ends is answered 504, unless its last forward ended while
// an earlier forward under its Nats-Msg-Id was still in process, which is the 503 maybe_stored
// (windowEnded). expectsReply is the request's own expects_reply bytes, which each answer writes
// back for the envelope it answers (expectsReplyAnswer, api.go); the envelope this send forwards
// and keeps carries the envelope's own value. scope is the edge's scope of the request, nil on
// every other listener: an answer names only a holder it shows (edgeScope.holderConfirmed), and
// the send forwards to the holder whatever it names. sender is who the request speaks for
// (senderOf), which decides how much of a copy the stream or the kept lane already held an answer
// carries. warnings is what its 200 and 202 name of the topic's old form.
func sendToRole(ctx context.Context, w http.ResponseWriter, scope *edgeScope, record *requestRecord, d *listenerDeps, item contracts.Envelope, sender requestSender, expectsReply json.RawMessage, role string, keep bool, arrived time.Time, warnings []string) {
	send := &roleSend{w: w, record: record, d: d, ctx: ctx, scope: scope, item: item, sender: sender, expectsReply: expectsReply, role: role, keep: keep, arrived: arrived, warnings: warnings}
	// The sender is stamped before the holder is resolved, so the copy a keep stores carries the
	// sender's title, roles and directory exactly as the forward of a live delivery does: the
	// envelope is kept as it is here (keepRoleMessage), and the holder that is handed it later
	// reads the same stamp this send's own 202 names.
	send.item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
	firstCtx, cancelFirst := context.WithDeadline(ctx, arrived.Add(sendRequestBudget))
	resolvedAt := d.delivery.now()
	first, err := roles.ResolveHolder(firstCtx, d.registry, roleHolderSessions{d.sessions}, role)
	cancelFirst()
	if err != nil {
		send.unresolved(err)
		return
	}
	if first.State != roles.Live {
		send.noHolder(first)
		return
	}
	send.hold(first)
	if send.settledBeforeForward(ctx, first.Entry) {
		return
	}
	checked := send.holder
	forwarded := send.forwarded()
	resolved := first
	// inProcess is the error of the send's last forward when that forward ended while an earlier
	// forward under its Nats-Msg-Id was still in process, and nil otherwise (windowEnded).
	var inProcess error
	for pass := 0; ; pass++ {
		if pass > 0 || !sessionIsLive(first.HolderState) || roles.ResolutionTooOld(resolvedAt, d.delivery.now()) {
			resolvedAt = d.delivery.now()
			resolved, err = roles.ResolveHolder(ctx, d.registry, roleHolderSessions{d.sessions}, role)
			if err != nil && ctx.Err() != nil {
				// The window ended under the resolution: what it read says nothing more.
				break
			}
		}
		if resolved.Holder != "" {
			send.hold(resolved)
		}
		switch {
		case err != nil:
			// Retried below, as every resolution not answered here is.
		case resolved.State == roles.Unclaimed:
			send.noHolder(resolved)
			return
		case resolved.State == roles.Live:
			if send.holder != checked {
				checked = send.holder
				if send.settledBeforeForward(ctx, resolved.Entry) {
					return
				}
			}
			window := forwardWindow(ctx)
			if window <= 0 {
				send.windowEnded(inProcess)
				return
			}
			subject := contracts.LegacyAgentSubject(send.holder)
			// The sender is still waiting, so a forward whose relay frame would be past the max
			// payload is refused here, the 413 the routes answer for the same rule
			// (bus.Client.RelayFrameTooLarge), rather than stored for a holder that could be handed
			// it only as the relay's notice. A forward of a copy the stream already holds - the
			// kept lane's walk, the role lane's delivery - makes no such check: that copy is past
			// refusing, and its holder is handed it whole or the notice in its place.
			if err := d.client.RelayFrameTooLarge(subject, forwarded); err != nil {
				send.refused(err)
				return
			}
			send.attempts++
			// The forward carries the same Nats-Msg-Id on every attempt, so the stream, which
			// captures agent subjects, keeps one copy of it per message and recipient however
			// many attempts this send makes.
			ack, err := d.delivery.forwardAndWait(subject, forwarded, bus.ForwardMsgID(forwarded.DedupeKey, subject), window)
			if err == nil {
				d.confirm(item, send.holder, ack.Sequence)
				send.record.charge.stored()
				if ack.Duplicate {
					send.deliveredHeld(ctx, send.holder, ack.Sequence, sendDelivered)
					return
				}
				send.delivered(sendDelivered)
				return
			}
			if errors.Is(err, bus.ErrDuplicateWithoutSequence) {
				// The stream answered the forward a duplicate of a copy it names no sequence of,
				// and no later reply comes: the 503 that asks again, at once, costing what a
				// forward that may have been stored costs.
				send.record.charge.stored()
				send.deliveredHeld(ctx, send.holder, 0, sendDelivered)
				return
			}
			if forwardRefused(err) {
				send.refused(err)
				return
			}
			// A forward that failed with no refusal may have been stored for its holder, so it
			// costs its sender one whatever this send answers next, and a copy a later verdict
			// names may be its own (deliveredHeld).
			send.record.charge.stored()
			send.unanswered = true
			// The error is kept as the answer may give it, judged now by the holder this forward
			// went to (shownForwardError): a later pass may hold another holder, one the edge
			// shows, while the error names this one's agent subject.
			inProcess = nil
			if earlierAttemptInProcess(err) {
				inProcess = send.shownForwardError("edge listener's role send ended while an earlier forward was in process; its answer names no holder", err)
			}
		}
		// Any resolution not answered above (a holder past the live window, which keeps its
		// claim, or a read that failed) fails this attempt only.
		if !pauseBeforeRetry(ctx) {
			break
		}
	}
	send.windowEnded(inProcess)
}

// forwarded is the envelope this send forwards to a holder: its own, under the role forward's
// dedupe key (roleForwardDedupePrefix and the envelope's key), on the holder's agent subject.
func (s *roleSend) forwarded() contracts.Envelope {
	forwarded := s.item
	forwarded.DedupeKey = roleForwardDedupePrefix + s.item.DedupeKey
	return forwarded
}
