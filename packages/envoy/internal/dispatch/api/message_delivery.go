package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// createDeliveryInput is the body of POST /api/v1/messages/{id}/deliveries.
type createDeliveryInput struct {
	Delivery string       `json:"delivery" required:"true"`
	Actor    *model.Actor `json:"actor"`
}

func (s *server) createDelivery(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input createDeliveryInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !human {
		if actor, err = bearerSessionActor(r, actor, input.Actor); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if !validDelivery(input.Delivery) {
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "delivery must be one of btw, aside, steer")
		return
	}
	message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, "", r.PathValue("id"))
	if err != nil || message.Target == nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	// A send of a deleted message would carry the text the delete withdrew, so there is nothing
	// left to send; every attempt still pending when the delete ran was failed by it.
	if message.DeletedAt != nil {
		s.writeHandlerError(w, deletedPostError(refs.ItemMessage))
		return
	}
	// The send outlives the request's connection, keeping its values and dropping its
	// cancellation, as createStoredMessage's does: the attempt is committed before the listener
	// call, a cut connection must not strand it, and one a process death strands the delivery
	// sweep resumes. It carries the urgency of the attempt it resumes, and none on an attempt it
	// opens: a Retry asks for none.
	attempt, err := s.deliverMessage(context.WithoutCancel(r.Context()), message, input.Delivery, actor, nil)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, attempt)
}

// acceptedAsUserTurn is what an accepted attempt records it became: the session's own user turn.
const acceptedAsUserTurn = "user_turn"

// acceptFresh is one of the accept's checks, judged by Postgres against the created_at Postgres
// wrote, as model.DeliveryClaimFreeSQL is: the attempt was opened within the last minute.
// claimed_at, because a resume moves claimed_at. So only an attempt a person has just asked for
// can be taken, and one that reached its session as a card - an older plugin, a card fallback of
// the last minute's accept - can never be taken as a turn later.
// Inside that minute the session itself keeps a carded attempt a card, since only it knows that
// frame already reached it (the handled attempts of `@legion/pi-shared/dispatch-user-turn`, kept
// in an Oh My Pi session's transcript and in claude-envoy's `turns/<session id>.json`).
const acceptFresh = `created_at >= now() - interval '1 minute'`

// acceptByAuthor is the check that the person who asked for the attempt (its requested_by) is
// the person who wrote message $1, so a message becomes a session's turn only on its author's own
// Send or Aside, never on another person's retry of it. People are compared case-insensitively,
// as model.CanonicalLogin compares them: an email names one person however it is cased.
const acceptByAuthor = `coalesce(
	lower(requested_by ->> 'id') = (select lower(author ->> 'id') from messages where id = $1),
	false)`

// acceptDirect is the check that the message is the one kind a session takes as a turn, a direct
// message from the Agents page: on no issue, neither a broadcast's copy nor a reply in a
// broadcast's thread, in a thread whose root targets the accepting session ($3). Who wrote it and
// how it was sent are the accept's own checks beside it. "Accepted as a user turn" means nothing
// for any other message, so it is refused rather than let a bearer have the page say one reached
// the conversation. It reads messageThreadCTE's thread of the message ($1); a message's author,
// issue, broadcast, target and parent are written once, at insert.
const acceptDirect = `coalesce((
	select m.issue_key is null and m.broadcast_id is null
	       and root.target = 'session:' || $3::text and root.broadcast_id is null
	from messages m, thread t join messages root on root.id = t.id
	where m.id = $1 and t.in_reply_to is null
), false)`

// acceptedDelivery is the accept route's answer: the attempt it accepted and the body of the
// message as Dispatch stored it.
type acceptedDelivery struct {
	model.MessageDelivery
	Body string `json:"body"`
}

// acceptDelivery is POST /api/v1/messages/{id}/deliveries/{attempt}/accept: the session an
// attempt went to records that it took the message as its user's own turn, and is answered the
// message's stored body in that attempt's mode. It is one compare-and-set under the message's
// row lock, and it succeeds only for a person's direct message to that session (acceptDirect)
// which that person wrote themselves, sent as a Send or an Aside, and only for the latest attempt
// of one none of whose attempts was accepted before, which the message's own author asked for
// within the last minute and which did not fail; a refusal names the check.
//
// "That person wrote themselves" is two checks, because a person's own credential now reaches the
// send route through an agent: the author is a person (byPerson), and that author carries no act
// (byAgent), the agent an answer, message or review records when one was made for its person. A
// message an agent wrote for the person must never become words the person typed into the
// session, so it is refused here and reaches the session as a card naming the agent instead.
//
// It may land while the attempt is still pending, since a session answers the frame before
// Dispatch settles the send, so it writes neither state nor claimed_at, which the settle is
// scoped to, and appends its own message.accepted rather than a second message.delivery receipt.
// It takes no owner lock: the message it accepts is a direct message (acceptDirect), whose event
// names no owner, and a message on an issue is refused 409 ACCEPT_NOT_DIRECT before any write, so
// no append of this route takes an owner's lock either.
//
// The attempt in the path is a positive number that fits message_deliveries.attempt, an integer
// column (fitsIntegerColumn): any other is an attempt no message has, so it is the 404
// MESSAGE_NOT_FOUND a missing attempt is, answered before any query is made with it.
//
// The actor is a bearer's own claim of its session, as on every session write, so a bearer
// naming another session can spend that session's one acceptance of a fresh attempt: the real
// session is then refused and shows a card, and nothing is injected.
func (s *server) acceptDelivery(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "ACCEPT_FORBIDDEN", http.StatusForbidden, "accept requires an agent bearer token")
		return
	}
	var input actorInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	number, err := strconv.Atoi(r.PathValue("attempt"))
	if err != nil || number < 1 || !fitsIntegerColumn(number) {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	ctx := r.Context()
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, "", r.PathValue("id"))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	event := messageEvent(message, "message.accepted", actor, model.MessageAcceptedEventPayload{
		MessageID: message.ID, Attempt: number, SessionID: actor.ID,
		AcceptedAs: acceptedAsUserTurn, Target: messageTarget(message.Target),
	})
	tx, err := s.begin(ctx)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(ctx)
	// Every attempt is opened, resumed and accepted under this lock, so what the checks below
	// read cannot move before the update. It is FOR NO KEY UPDATE, which serialises it against
	// the claim's, and not FOR UPDATE: the session's reply takes the attempt row first and then,
	// through its insert's foreign key, this row FOR KEY SHARE, which FOR UPDATE would wait for
	// while this transaction waits for the attempt row the reply holds.
	if _, err := tx.Exec(ctx, `select 1 from messages where id = $1 for no key update`, message.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var sessionID, delivery, state string
	var direct, byPerson, byAgent, latest, byAuthor, fresh bool
	var taken *int
	var requester *string
	err = tx.QueryRow(ctx, messageThreadCTE+`
		select session_id, delivery, state,
		       `+acceptDirect+`,
		       (select author ->> 'kind' = 'user' from messages where id = $1),
		       (select author ? 'act' from messages where id = $1),
		       attempt = (select max(attempt) from message_deliveries where message_id = $1),
		       (select attempt from message_deliveries where message_id = $1 and accepted_at is not null),
		       requested_by ->> 'kind',
		       `+acceptByAuthor+`,
		       `+acceptFresh+`
		from message_deliveries
		where message_id = $1 and attempt = $2
	`, message.ID, number, actor.ID).Scan(
		&sessionID, &delivery, &state, &direct, &byPerson, &byAgent, &latest, &taken, &requester, &byAuthor, &fresh,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	switch {
	case sessionID != actor.ID:
		writeError(w, "ACCEPT_FORBIDDEN", http.StatusForbidden, "session may accept only its own delivery")
		return
	case !direct:
		writeError(w, "ACCEPT_NOT_DIRECT", http.StatusConflict,
			"only a direct message to this session, on no issue and from no broadcast, is taken as its turn")
		return
	case !byPerson:
		writeError(w, "ACCEPT_NOT_WRITTEN_BY_PERSON", http.StatusConflict,
			"only a message a person wrote is taken as the session's turn")
		return
	case byAgent:
		writeError(w, "ACCEPT_WRITTEN_BY_AGENT", http.StatusConflict,
			"this message was written by an agent acting for the person, so it is shown as a card rather than taken as the session's turn")
		return
	case delivery != "aside" && delivery != "steer":
		writeError(w, "ACCEPT_NOT_ASIDE_OR_STEER", http.StatusConflict,
			fmt.Sprintf("attempt %d is a %s, and only a Send or an Aside is taken as the session's turn", number, delivery))
		return
	case taken != nil:
		writeError(w, "ACCEPT_ALREADY_ACCEPTED", http.StatusConflict,
			fmt.Sprintf("attempt %d of this message was already accepted", *taken))
		return
	case !latest:
		writeError(w, "ACCEPT_SUPERSEDED", http.StatusConflict,
			fmt.Sprintf("attempt %d is not this message's latest attempt", number))
		return
	case requester == nil || *requester != "user":
		writeError(w, "ACCEPT_NOT_REQUESTED_BY_PERSON", http.StatusConflict,
			fmt.Sprintf("no person requested attempt %d", number))
		return
	case !byAuthor:
		writeError(w, "ACCEPT_NOT_REQUESTED_BY_AUTHOR", http.StatusConflict,
			fmt.Sprintf("attempt %d was requested by someone other than the person who wrote the message", number))
		return
	case state != model.DeliveryStatePending && !model.DeliveryTaken(state):
		// Dispatch told the person this attempt failed, and they may already have sent the message
		// another way; a frame naming it is not the delivery they were shown. A queued attempt is
		// not one of those: the listener took it, and the session accepting it now is the session
		// it was stored for, reading it as it runs again.
		writeError(w, "ACCEPT_FAILED", http.StatusConflict,
			fmt.Sprintf("attempt %d failed, and only a pending, sent or queued attempt is taken as the session's turn", number))
		return
	case !fresh:
		writeError(w, "ACCEPT_STALE", http.StatusConflict,
			fmt.Sprintf("attempt %d was requested more than a minute ago", number))
		return
	}
	accepted, err := scanMessageDelivery(tx.QueryRow(ctx, `
		update message_deliveries set accepted_at = now(), accepted_as = $3
		where message_id = $1 and attempt = $2
		returning `+messageDeliveryColumns,
		message.ID, number, acceptedAsUserTurn,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	appended, err := s.appendEvent(ctx, tx, event)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(ctx, appended)
	WriteJSON(w, http.StatusOK, acceptedDelivery{MessageDelivery: accepted, Body: message.Body})
}

// pendingMessageDelivery is the attempt one targeted send owns: committed as pending under
// the message's lock, carrying the frame, the listener resolution and the urgency it is sent
// with. The urgency is the one the attempt's row recorded (message_deliveries.urgency), so every
// send of one attempt - its first, the delivery sweep's, a person's same-mode Retry - carries the
// same one, and an attempt a Retry opens, which records none, carries none.
type pendingMessageDelivery struct {
	attemptClaim
	resolved ResolvedMention
	urgency  *string
	// connectedOnly is the attempt row's own connected_only, the request's flag: attempt 1's,
	// which createStoredMessage writes from the request that stored the message, and attempt 1's
	// again for every later one (insertMessageDeliveryAttempt). Every send of the attempt asks the
	// listener for a session that is running now when it is set or the send is a model or thinking
	// switch the session runs as its own turn (deliverMessageResuming, isSessionSwitch): a person's
	// own issue-less Send or Aside (not one an agent wrote for them), no broadcast's copy, whose
	// body is one switch line. The listener then refuses a stopped target rather than storing the
	// message for it: attempt 1's own send, the delivery sweep's resume, a Retry and a mode change
	// alike. No row records the switch rule, which each send judges from the message and the mode
	// it is sent in: a switch an earlier release stored has rows with no flag, which a resume
	// claims as they stand, and the rule's verdict for a Send must not pass to a Use BTW instead,
	// which changes no model. Every other message - an issue message, a broadcast copy, a BTW, a
	// session's message - is queued for a stopped session.
	connectedOnly bool
	frame         []byte
	// message is the message this attempt is sent with, carrying the text its row held under the
	// claim's lock (recordPendingMessageDelivery): the body and edited_at an edit moves. The
	// frame is built from it and the send carries its body, so neither carries text an edit or a
	// delete that committed while the claim waited on its lock took away.
	message model.Message
}

// messageDeliveryRoute is what a targeted send resolves against the listener: the session a
// resumable pending attempt is already pointed at, or the message's own target when there is no
// such attempt or it was stranded before anything was resolved. A resumed attempt keeps its
// recipient - it is the attempt the listener has already deduplicated under its idempotency key,
// so re-pointing it at whoever holds the message's role now would deliver one attempt number to
// two sessions and leave the first unable to answer the frame it was sent. The attempt read is
// the one a bound sender (resumeAttempt > 0) owns, and otherwise the lowest pending one, which is
// the one an unbound send resumes. This runs with nothing held, before the resolution;
// recordPendingMessageDelivery re-reads the attempt under the message's lock and redoes the pair
// when the row it locked names a different recipient.
//
// A send bound to resumeAttempt routes by that attempt alone, whoever holds it, as its claim
// judges its recipient: routed by the oldest pending attempt instead - one a later retry overtook,
// pinned to the session that held the message's role then - it would never match its own attempt,
// and an owed attempt would be refused its send on every pass of the delivery sweep.
func (s *server) messageDeliveryRoute(ctx context.Context, message model.Message, resumeAttempt int) (string, error) {
	var pinned string
	var lapsed bool
	var err error
	if resumeAttempt > 0 {
		lapsed = true
		err = s.deps.Store.Pool.QueryRow(ctx, `
			select session_id from message_deliveries
			where message_id = $1 and attempt = $2 and state = 'pending'
		`, message.ID, resumeAttempt).Scan(&pinned)
	} else {
		err = s.deps.Store.Pool.QueryRow(ctx, `
			select session_id, `+model.DeliveryClaimFreeSQL+`
			from message_deliveries
			where message_id = $1 and state = 'pending'
			order by attempt
			limit 1
		`, message.ID).Scan(&pinned, &lapsed)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err != nil || !lapsed || pinned == "" {
		return messageTarget(message.Target), nil
	}
	return "session:" + pinned, nil
}

// recordPendingMessageDelivery is the claim transaction of a targeted delivery: it locks the
// message, settles which attempt this send owns and commits it as pending, all before the
// listener send, so the send holds no pooled connection. The locked rows re-verify the
// recipient the resolution was taken for: the attempt's own for a resumed attempt, the
// message's target for a new one.
//
// The idempotency key is scoped to the message and the mode, so resuming a stranded attempt
// under a DIFFERENT mode would publish a second frame under one attempt number: the row would
// name one mode while the agent had been sent two. A mode change therefore settles the stranded
// attempt rather than resuming it, and opens an attempt of its own pinned to the session that
// attempt named - the recipient the resolution was taken for, whatever the message's target
// says now.
func (s *server) recordPendingMessageDelivery(
	ctx context.Context,
	message model.Message,
	actor model.Actor,
	resolved ResolvedMention,
	replyThread *messageReplyThread,
	resumeAttempt int,
) (pendingMessageDelivery, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return pendingMessageDelivery{}, err
	}
	defer tx.Rollback(ctx)
	// The issue is locked before the message row and the attempt rows, the order every other
	// message transaction takes them in, so the two halves of a delivery cannot deadlock
	// against each other. It is also the receipt owner a supersede below appends against. A
	// closed issue still takes a delivery, so the lock refuses nothing.
	if message.IssueKey != nil {
		if _, err := s.lockIssue(ctx, tx, *message.IssueKey); err != nil {
			return pendingMessageDelivery{}, err
		}
	}
	// FOR NO KEY UPDATE, as the accept's is: the session's reply takes an attempt row and then
	// this row FOR KEY SHARE through its insert's foreign key, and FOR UPDATE would wait for that
	// while this transaction waits below for the attempt row the reply holds. A message of an
	// issue has its text read here too, under the issue's owner lock every writer of it holds
	// (lockedMessageForCorrection), so the frame and the send carry the body as the lock's
	// previous holder left it, not as the caller read it before the lock. A message with no issue,
	// a direct message or a broadcast's copy, takes no edit and no delete (NOT_AN_ISSUE_MESSAGE),
	// so the body the caller read is the body it has, and its row gives the target alone.
	var target string
	var deleted bool
	if message.IssueKey != nil {
		if err := tx.QueryRow(ctx, `
			select target, body, edited_at, deleted_at is not null from messages where id = $1 for no key update
		`, message.ID).Scan(&target, &message.Body, &message.EditedAt, &deleted); err != nil {
			return pendingMessageDelivery{}, err
		}
	} else if err := tx.QueryRow(ctx, `select target from messages where id = $1 for no key update`, message.ID).Scan(&target); err != nil {
		return pendingMessageDelivery{}, err
	}
	// An unbound send is a Retry (createDelivery), which resumes or opens an attempt of the
	// message, so it is this row that decides whether any text is left to send: a delete that
	// committed while the Retry waited on the lock refuses it here, where the route's own check
	// read the message before the lock. A bound sender takes its own attempt and no other, which
	// that delete has already settled (queueDeletedPostSettlement), so it finds the attempt no
	// longer pending below and sends nothing; one bound to the first attempt of a message that has
	// none opens nothing for a deleted message either (below).
	if resumeAttempt == 0 && deleted {
		return pendingMessageDelivery{}, deletedPostError(refs.ItemMessage)
	}
	pending := pendingMessageDelivery{resolved: resolved}
	if resumeAttempt > 0 {
		// A bound sender takes its own attempt or nothing. The row must still be pending, still
		// in the mode this send resolved for, and free (nobody else inside the claim lease) -
		// anything else means this send is not the one that owes the frame. Its recipient is the
		// one the resolution must have been taken for: the session a pinned row names, which a
		// different resolution means another sender's; or, for a row written before anything was
		// resolved (a direct message's attempt 1, which createStoredMessage opens in the
		// transaction that stores the message), the message's own target, which a different
		// resolution means a stale one.
		var bound model.MessageDelivery
		var free bool
		err = tx.QueryRow(ctx, `
			select attempt, delivery, session_id, urgency, `+model.ConnectedOnlySQL+`, `+model.DeliveryClaimFreeSQL+`
			from message_deliveries
			where message_id = $1 and attempt = $2 and state = 'pending'
			for update
		`, message.ID, resumeAttempt).Scan(
			&bound.Attempt, &bound.Delivery, &bound.SessionID, &pending.urgency, &pending.connectedOnly, &free,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			// A deleted message opens no first attempt. Its delete settled each attempt it had
			// pending and wrote none for a message that had none, so there is no attempt of it to
			// open, settled or not: an attempt opened failed here would be one the delete never
			// ended, with a receipt the delete never appended. The sender is told, as a bound
			// sender whose attempt was settled is, that it has nothing to send.
			if deleted {
				return pendingMessageDelivery{}, errAttemptNotPending
			}
			return s.openFirstMessageDeliveryAttempt(
				ctx, tx, message, target, actor, resolved, replyThread, resumeAttempt,
			)
		}
		if err != nil {
			return pendingMessageDelivery{}, err
		}
		if !free || bound.Delivery != resolved.Delivery ||
			(bound.SessionID != "" && "session:"+bound.SessionID != resolved.Target) {
			return pendingMessageDelivery{}, errAttemptNotPending
		}
		if bound.SessionID == "" && target != resolved.Target {
			return pendingMessageDelivery{}, errStaleResolution
		}
		// An unpinned row takes the session this resolution found, as the unbound resume below
		// pins one, so the row names the session its frame is going to and that session can
		// answer it.
		pending.attempt = bound.Attempt
		if err := tx.QueryRow(ctx, `
			update message_deliveries
			set session_id = coalesce(nullif(session_id, ''), $3), claimed_at = now()
			where message_id = $1 and attempt = $2 and state = 'pending'
			returning claimed_at, session_id
		`, message.ID, pending.attempt, resolved.attemptSessionID,
		).Scan(&pending.claimedAt, &pending.resolved.attemptSessionID); err != nil {
			return pendingMessageDelivery{}, err
		}
		if err := finishPendingMessageDelivery(ctx, tx, s, message, replyThread, &pending); err != nil {
			return pendingMessageDelivery{}, err
		}
		return pending, nil
	}
	var stranded model.MessageDelivery
	var strandedUrgency *string
	var lapsed bool
	err = tx.QueryRow(ctx, `
		select attempt, delivery, session_id, urgency, `+model.ConnectedOnlySQL+`, `+model.DeliveryClaimFreeSQL+`
		from message_deliveries
		where message_id = $1 and state = 'pending'
		order by attempt
		limit 1
		for update
	`, message.ID).Scan(
		&stranded.Attempt, &stranded.Delivery, &stranded.SessionID, &strandedUrgency, &pending.connectedOnly, &lapsed,
	)
	var superseded *model.Event
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return pendingMessageDelivery{}, err
	case err == nil && lapsed:
		// Nobody holds this attempt, or whoever claimed it never came back. It keeps the
		// recipient its row names; only an attempt stranded before anything was resolved,
		// whose row names none, goes to the message's target. Either way the resolution has
		// to have been taken for that recipient.
		recipient := target
		if stranded.SessionID != "" {
			recipient = "session:" + stranded.SessionID
		}
		if recipient != resolved.Target {
			return pendingMessageDelivery{}, errStaleResolution
		}
		if stranded.SessionID != "" && stranded.Delivery != resolved.Delivery {
			// A different mode: this send may not ride on that attempt's key. Settle it,
			// append the receipt its row now owes - the dashboard's attempt history is built
			// from receipts alone, so a row settled without one never reads failed there -
			// and open an attempt of this send's own, pinned to the same session.
			stranded.MessageID, stranded.State = message.ID, "failed"
			failure := supersededByModeChangeText
			stranded.Error = &failure
			// claimed_at moves with the settle, exactly as the resume branch below moves it:
			// it is how a sender still holding the old claim learns the row is no longer its
			// own. Without it, a sender that returns after its lease lapsed still reads
			// `mine`, and if the agent answered the frame it already held it would append a
			// SECOND message.delivery receipt for this attempt.
			if err := tx.QueryRow(ctx, `
				update message_deliveries set state = 'failed', error = $3, claimed_at = now()
				where message_id = $1 and attempt = $2 and state = 'pending'
				returning attempt
			`, message.ID, stranded.Attempt, failure).Scan(&stranded.Attempt); err != nil {
				return pendingMessageDelivery{}, err
			}
			receipt, err := s.appendEvent(ctx, tx, messageDeliveryReceipt(message, actor, stranded, resolved.title))
			if err != nil {
				return pendingMessageDelivery{}, err
			}
			superseded = &receipt
			if err := s.insertMessageDeliveryAttempt(ctx, tx, message, actor, resolved, &pending); err != nil {
				return pendingMessageDelivery{}, err
			}
			break
		}
		// The same mode: resume it under its original number, and so its original
		// idempotency key, whose repeat of a send that did land is recognised and dropped
		// (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts), and with the urgency it
		// recorded, as the delivery sweep would send it. An attempt that already names
		// a session keeps it; one stranded before anything was resolved takes the recipient
		// this resolution found, so the row names the session its frame is going to either
		// way and that session can answer it.
		pending.attempt, pending.urgency = stranded.Attempt, strandedUrgency
		if err := tx.QueryRow(ctx, `
			update message_deliveries
			set delivery = $3, session_id = coalesce(nullif(session_id, ''), $4), claimed_at = now()
			where message_id = $1 and attempt = $2
			returning claimed_at, session_id
		`, message.ID, pending.attempt, resolved.Delivery, resolved.attemptSessionID,
		).Scan(&pending.claimedAt, &pending.resolved.attemptSessionID); err != nil {
			return pendingMessageDelivery{}, err
		}
	default:
		// No attempt is pending, or a live sender holds the one that is, so this send opens an
		// attempt of its own rather than two senders driving the same one. The message row is
		// locked, so the highest attempt cannot move between reading it and inserting beside it.
		if target != resolved.Target {
			return pendingMessageDelivery{}, errStaleResolution
		}
		if err := s.insertMessageDeliveryAttempt(ctx, tx, message, actor, resolved, &pending); err != nil {
			return pendingMessageDelivery{}, err
		}
	}
	if err := finishPendingMessageDelivery(ctx, tx, s, message, replyThread, &pending); err != nil {
		return pendingMessageDelivery{}, err
	}
	if superseded != nil {
		s.publish(ctx, *superseded)
	}
	return pending, nil
}

// openFirstMessageDeliveryAttempt is a bound sender's claim when its attempt is not a pending row:
// bound to the first attempt of a message that has none, it opens that attempt, and otherwise the
// attempt was settled or superseded and it is errAttemptNotPending. Two senders bind to a message's
// first attempt before it exists - a repeat of a keyed send that found the message committed with
// no attempt (sendHeldMessageOn) and the delivery sweep's resume of that keyed send, both of them
// recoveries for a message an earlier release stored - and the message's
// lock, which tx holds, makes whichever claims first open the attempt; the other then finds it
// taken and sends nothing. No request's connected_only was recorded for a message that reaches
// here, so the attempt it opens records none, and its send asks for a session running now only
// when it is a switch the session runs as its own turn (deliverMessageResuming, isSessionSwitch).
func (s *server) openFirstMessageDeliveryAttempt(
	ctx context.Context,
	tx pgx.Tx,
	message model.Message,
	target string,
	actor model.Actor,
	resolved ResolvedMention,
	replyThread *messageReplyThread,
	resumeAttempt int,
) (pendingMessageDelivery, error) {
	if resumeAttempt != 1 {
		return pendingMessageDelivery{}, errAttemptNotPending
	}
	var attempted bool
	if err := tx.QueryRow(ctx,
		`select exists (select 1 from message_deliveries where message_id = $1)`, message.ID,
	).Scan(&attempted); err != nil {
		return pendingMessageDelivery{}, err
	}
	if attempted {
		return pendingMessageDelivery{}, errAttemptNotPending
	}
	if target != resolved.Target {
		return pendingMessageDelivery{}, errStaleResolution
	}
	pending := pendingMessageDelivery{resolved: resolved}
	if err := s.insertMessageDeliveryAttempt(ctx, tx, message, actor, resolved, &pending); err != nil {
		return pendingMessageDelivery{}, err
	}
	if err := finishPendingMessageDelivery(ctx, tx, s, message, replyThread, &pending); err != nil {
		return pendingMessageDelivery{}, err
	}
	return pending, nil
}

// finishPendingMessageDelivery builds the frame this claimed attempt will be sent with and
// commits the claim transaction. Both claim paths end here, so a bound sender's frame says
// exactly what an ordinary one's does. message is the message as the claim read it under its
// lock, which the attempt's send carries too (pendingMessageDelivery.message).
func finishPendingMessageDelivery(
	ctx context.Context,
	tx pgx.Tx,
	s *server,
	message model.Message,
	replyThread *messageReplyThread,
	pending *pendingMessageDelivery,
) error {
	thread := replyThread
	if thread == nil {
		// A retry sends the stored message, so the block it answers is read as its write named
		// it, for the frame's message and the preview its thread carries. The frame is sent where
		// every reader of the conversation reads it, so the block is read as everyReader.
		if err := hydrateMessageReplyBlocks(ctx, tx, everyReader, &message); err != nil {
			return err
		}
		derived, err := loadMessageReplyThread(ctx, tx, message)
		if err != nil {
			return err
		}
		thread = &derived
	}
	frame, err := json.Marshal(struct {
		Event    model.Event `json:"event"`
		Delivery any         `json:"delivery"`
	}{
		// A delivery frame replays the message to one session; the reference changes belong to
		// the appended event, which the stream already carried.
		Event: messageEvent(
			message, "message.created", message.Author, thread.payload(message, model.ReferenceChanges{}),
		),
		Delivery: map[string]any{"attempt": pending.attempt, "mode": pending.resolved.Delivery},
	})
	if err != nil {
		return fmt.Errorf("encode target delivery frame: %w", err)
	}
	pending.frame, pending.message = frame, message
	return tx.Commit(ctx)
}

// supersededByModeChangeText is what a stranded attempt records when a retry in another mode
// takes over: that mode is a different idempotency key, so the new send is genuinely a second
// delivery, and this attempt's own fate was never learned.
const supersededByModeChangeText = "superseded by a retry in another mode; it may already have been delivered"

// insertMessageDeliveryAttempt opens an attempt of this send's own, under the recipient the
// resolution was taken for, recording requestedBy - the actor this send carries - as who asked
// for it. The message row is locked by the caller, so the highest attempt cannot move between
// reading it and inserting beside it.
//
// connected_only is attempt 1's, never this send's request's: the request that stored the message
// is the only one that says whether it asks for a session running now (createStoredMessage), so a
// Retry or a mode change of a message that asked it asks it again, and the listener stores nothing
// for a stopped session. The switch rule is no row's (pendingMessageDelivery.connectedOnly): each
// send judges it in its own mode (deliverMessageResuming, isSessionSwitch), so a Retry or a Use
// Aside instead of a switch whose request asked nothing is refused for a stopped session and a
// Use BTW instead of it is queued, whether this release wrote its attempt 1, an earlier release
// wrote it with no column to write, or it has none and this opens it
// (openFirstMessageDeliveryAttempt).
func (s *server) insertMessageDeliveryAttempt(
	ctx context.Context, tx pgx.Tx, message model.Message, requestedBy model.Actor, resolved ResolvedMention,
	pending *pendingMessageDelivery,
) error {
	requester, err := json.Marshal(requestedBy)
	if err != nil {
		return err
	}
	return tx.QueryRow(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, claimed_at,
		                                requested_by, connected_only)
		values (
			$1,
			(select coalesce(max(attempt), 0) + 1 from message_deliveries where message_id = $1),
			$2, $3, 'pending', now(), $4,
			(select connected_only from message_deliveries where message_id = $1 order by attempt limit 1)
		)
		returning attempt, claimed_at, `+model.ConnectedOnlySQL+`
	`, message.ID, resolved.Delivery, resolved.attemptSessionID, requester,
	).Scan(&pending.attempt, &pending.claimedAt, &pending.connectedOnly)
}

// messageDeliveryReceipt is the message.delivery receipt an attempt row owes, read off that
// row: which attempt of which message went where, how it ended, and under which mode. Two
// fields the row does not carry come from either side of it - the target is the message's own,
// the conversation every attempt of it belongs to, and title is what the listener resolution
// called the session, empty where the session itself appends the receipt. Every producer of a
// message.delivery event builds it here.
func messageDeliveryReceipt(
	message model.Message, actor model.Actor, attempt model.MessageDelivery, title string,
) model.Event {
	return messageEvent(message, "message.delivery", actor,
		model.MessageDeliveryEventPayload{
			MessageID: attempt.MessageID, Attempt: attempt.Attempt, Delivery: attempt.Delivery,
			SessionID: attempt.SessionID, Target: messageTarget(message.Target), Title: title,
			State: attempt.State, Duplicate: attempt.Duplicate, Error: receiptError(attempt.Error),
		})
}

// completeMessageDelivery is the settle transaction of a targeted delivery: it records what the
// send did and appends the message.delivery receipt, holding no listener call.
func (s *server) completeMessageDelivery(
	ctx context.Context,
	message model.Message,
	actor model.Actor,
	pending pendingMessageDelivery,
	outcome sendOutcome,
) (model.MessageDelivery, error) {
	state, failure := deliveryOutcome(outcome.failure, outcome.queued)
	// The receipt this send owes, read off the row it is about to write.
	receipt := messageDeliveryReceipt(message, actor, model.MessageDelivery{
		MessageID: message.ID, Attempt: pending.attempt, Delivery: pending.resolved.Delivery,
		SessionID: pending.resolved.attemptSessionID, State: state, Duplicate: outcome.duplicate, Error: failure,
	}, pending.resolved.title)
	return settleDeliveryAttempt(ctx, s, receipt,
		func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, error) {
			settled, err := scanMessageDelivery(tx.QueryRow(ctx, `
				update message_deliveries set envelope_id = $4, state = $5, error = $6, duplicate = $7
				where message_id = $1 and attempt = $2 and state = 'pending' and claimed_at = $3
				returning `+messageDeliveryColumns,
				message.ID, pending.attempt, pending.claimedAt, outcome.envelopeID, state, failure, outcome.duplicate,
			))
			return settled, err
		},
		func(ctx context.Context, tx pgx.Tx) (model.MessageDelivery, bool, *string, error) {
			var mine bool
			answered, err := scanMessageDelivery(tx.QueryRow(ctx, `
				select `+messageDeliveryColumns+`, claimed_at is not distinct from $3
				from message_deliveries where message_id = $1 and attempt = $2
			`, message.ID, pending.attempt, pending.claimedAt), &mine)
			return answered, mine, answered.ReplyID, err
		},
		func(answered model.MessageDelivery) model.Event {
			return messageDeliveryReceipt(message, actor, answered, pending.resolved.title)
		},
		// The session this attempt reached, in the transaction that records that it did and
		// under the receipt that records it (graphstore.QueueReached): an attempt the listener
		// took, sent or queued (model.DeliveryTaken), is a delivery and its session is a node
		// from this moment, a pending or failed one reached nobody and writes nothing.
		func(b *store.WriteBatch, settled model.MessageDelivery) error {
			return graphstore.QueueReached(b, settled.State, settled.SessionID, actor, settled.CreatedAt, graphstore.LatestEvent)
		},
	)
}

// deliverMessage records one delivery attempt of a targeted message and sends it to the
// resolved session: the listener resolution first with nothing held, the attempt committed as
// pending, the send, then the outcome. replyThread is what the delivery frame says about the
// thread the message joins; a caller that has not derived it passes nil and it is read from
// the stored parent.
func (s *server) deliverMessage(
	ctx context.Context,
	message model.Message,
	delivery string,
	actor model.Actor,
	replyThread *messageReplyThread,
) (model.MessageDelivery, error) {
	return s.deliverMessageResuming(ctx, message, delivery, actor, replyThread, 0)
}

// errAttemptNotPending reports that the attempt a bound sender owns is no longer its to send:
// something else settled it, superseded it, or is sending it right now, or it is the first
// attempt of a message deleted before it had one (recordPendingMessageDelivery). Only a bound
// sender (resumeAttempt > 0) can see it, and its caller drops the send rather than opening
// another attempt - a second attempt here is a second frame on the wire.
var errAttemptNotPending = errors.New("delivery attempt is no longer pending")

// deliverMessageResuming is deliverMessage bound to one attempt. resumeAttempt > 0 sends that
// attempt and nothing else: it never opens a second attempt beside it and never supersedes it,
// answering errAttemptNotPending instead. Every sender whose attempt exists before it starts sends
// that way: a direct message's own request (createStoredMessage opens attempt 1 in the
// transaction that stores the message), a broadcast's workers, and the delivery sweep
// (delivery_sweep.go). A human who changes the mode from the agent card in the meantime settles
// the attempt and sends their own, and a bound sender that then opened an attempt of its own
// would put a second frame on the session's subject. Bound to attempt 1 of a message that has no
// attempt yet - only a message an earlier release stored before it opened one, since
// createStoredMessage writes attempt 1 in the transaction that stores the message - it opens that
// attempt (openFirstMessageDeliveryAttempt): a repeat of a keyed send that finds its message
// committed with no attempt does, and only the first of the two to claim sends.
//
// Bound or not, a send that meets a listener take-over tries again under the attempt it claimed
// (takeoverRetry.retry, renewMessageDeliveryClaim) and settles only with the last try's outcome.
// A last try that found no live session for the session the attempt names - the registry read's
// answer or the send's own 404, so neither the listener's registry nor its known-session store
// holds that session - is queued by Dispatch itself on that session's own subject
// (queueForUnknownSession) while this sender still holds the attempt, so every sender of a
// targeted message queues it alike: the request, a keyed repeat, a broadcast's copy, a Retry and
// the delivery sweep. The queued copy is a send too, so the sender re-checks its claim
// immediately before it (holdMessageDeliveryClaim), as it does before every try.
func (s *server) deliverMessageResuming(
	ctx context.Context,
	message model.Message,
	delivery string,
	actor model.Actor,
	replyThread *messageReplyThread,
	resumeAttempt int,
) (model.MessageDelivery, error) {
	pending, err := resolveThenLock(ctx,
		func(ctx context.Context) (ResolvedMention, error) {
			route, err := s.messageDeliveryRoute(ctx, message, resumeAttempt)
			if err != nil {
				return ResolvedMention{}, err
			}
			return s.resolveMentionTarget(ctx, route, delivery, 0), nil
		},
		func(ctx context.Context, resolved ResolvedMention) (pendingMessageDelivery, error) {
			return s.recordPendingMessageDelivery(ctx, message, actor, resolved, replyThread, resumeAttempt)
		},
		errorf(http.StatusConflict, "MESSAGE_TARGET_CHANGED",
			"the message's target changed while this delivery was being recorded"),
	)
	if err != nil {
		return model.MessageDelivery{}, err
	}
	// The message as the claim read it under its lock, so the send, any queued copy and the
	// settle carry the text the frame does.
	message = pending.message
	// The key is scoped to the message and the mode, stable across every attempt of that pair,
	// so a retry of a send that already landed repeats its dedupe key and is recognised as the
	// repeat it is (DELIVERY_DUPLICATE_WINDOW_MS in packages/contracts says by what). The
	// listener scopes it further by recipient, which is what lets a role's new holder still be
	// reached.
	key := message.ID + ":" + pending.resolved.Delivery
	// A model or thinking switch the session runs as its own turn asks for a session running now
	// on every send of it, whatever its row holds, judged in the mode this attempt is sent in
	// (isSessionSwitch, pendingMessageDelivery.connectedOnly).
	connectedOnly := pending.connectedOnly || isSessionSwitch(message, pending.resolved.Delivery)
	// A direct message is written to the session it names and invites its answer; nothing about
	// one says a reply is unwanted. A copy Dispatch queues for its session says the same.
	wanted := contracts.ExpectsReplyOptional
	takeover := s.newTakeoverRetry()
	var outcome sendOutcome
	// held is whether this sender still holds the attempt's claim when its tries end. A renew that
	// found the attempt answered or taken meanwhile ends them holding nothing, and then nothing is
	// queued: the settle below reads what the row holds now.
	held := true
	err = takeover.retry(ctx,
		func() string {
			outcome = s.sendResolvedDelivery(
				ctx, pending.resolved, messageTarget(message.Target), message.Body, key, pending.urgency, wanted, pending.frame, connectedOnly,
			)
			return outcome.failure
		},
		func() (bool, error) {
			renewed, stillHeld, err := s.renewMessageDeliveryClaim(ctx, message, pending)
			if stillHeld {
				pending = renewed
			}
			held = stillHeld
			return stillHeld, err
		},
	)
	if err != nil {
		return model.MessageDelivery{}, err
	}
	if held && s.mayQueueForUnknownSession(message, pending, connectedOnly, outcome.failure) {
		// The last try was a listener call with nothing held, and a delete that committed during it
		// settled the attempt and moved its claim (queueDeletedPostSettlement); a copy published
		// now would store the deleted body for the session to read when it registers again. So the
		// claim is re-checked first, in a statement scoped to it, as every try's renew is, and a
		// claim no longer this sender's publishes nothing: the settle below reads the row as it
		// stands and appends whatever receipt that row still owes.
		pending, held, err = s.holdMessageDeliveryClaim(ctx, message, pending, pending)
		if err != nil {
			return model.MessageDelivery{}, err
		}
		if held {
			outcome = s.queueForUnknownSession(ctx, message, pending, key, wanted, outcome.failure)
		}
	}
	return s.completeMessageDelivery(ctx, message, actor, pending, outcome)
}

// renewMessageDeliveryClaim prepares one more try of an attempt this sender holds, after a try a
// listener take-over failed: it resolves again only the recipient the attempt names - the session
// its row is pinned to, or, for a row still unpinned, the message's own target, which is written
// once - and renews the claim on that resolution (holdMessageDeliveryClaim). It answers the
// attempt as the next try sends and settles it, and held = false, with pending unchanged, when
// the claim is no longer this sender's.
func (s *server) renewMessageDeliveryClaim(
	ctx context.Context, message model.Message, pending pendingMessageDelivery,
) (pendingMessageDelivery, bool, error) {
	route := messageTarget(message.Target)
	if pending.resolved.attemptSessionID != "" {
		route = "session:" + pending.resolved.attemptSessionID
	}
	renewed := pending
	// Unbounded, as the message's own resolution above is: only a comment's creation bounds its
	// lookups at mentionLookupTimeout, because it answers without them. A read cut short here
	// would leave the attempt unanswered with no resolve error, and this sender settles it.
	renewed.resolved = s.resolveMentionTarget(ctx, route, pending.resolved.Delivery, 0)
	return s.holdMessageDeliveryClaim(ctx, message, pending, renewed)
}

// holdMessageDeliveryClaim renews the claim pending holds on its attempt, pinning a row still
// unpinned to the session next resolved to, in one statement scoped to the claim held, as every
// settle statement is, in a transaction of its own, as every write a keyed request makes is
// (markRequestKeyCommitted). It answers next with the claim it renewed and the session the row is
// pinned to, and held = false, with pending unchanged, when the claim is no longer this sender's:
// the attempt was answered, taken or settled by its post's delete meanwhile, and the settle that
// follows reads which. A sender about to queue its message passes pending as next: the row is
// pinned already, and the statement only re-checks and renews the claim.
func (s *server) holdMessageDeliveryClaim(
	ctx context.Context, message model.Message, pending, next pendingMessageDelivery,
) (pendingMessageDelivery, bool, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return pending, false, err
	}
	defer tx.Rollback(ctx)
	err = tx.QueryRow(ctx, `
		update message_deliveries
		set claimed_at = now(), session_id = coalesce(nullif(session_id, ''), $4)
		where message_id = $1 and attempt = $2 and state = 'pending' and claimed_at = $3
		returning claimed_at, session_id
	`, message.ID, pending.attempt, pending.claimedAt, next.resolved.attemptSessionID,
	).Scan(&next.claimedAt, &next.resolved.attemptSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending, false, nil
	}
	if err != nil {
		return pending, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return pending, false, err
	}
	return next, true, nil
}

// messageReplyInput is the body of POST /api/v1/messages/{id}/reply.
type messageReplyInput struct {
	Actor   *model.Actor `json:"actor"`
	Attempt int          `json:"attempt" required:"true"`
	Body    *string      `json:"body"`
	Error   *string      `json:"error"`
	// InReplyToBlock is the id of the block of message {id} the reply answers ("b2").
	InReplyToBlock *string `json:"in_reply_to_block"`
}

// messageReplyPlan is everything a session's reply to a message delivery is decided on, read
// and planned before the route takes its first row lock (replyRowPlan), with the body that
// reply carries and the block of its parent it names.
//
// One plan serves both rows the route can insert - the reply to the attempt, and a follow-up
// posted under a reply the session already made (replyAfterAnswer) - because at most one of
// them is written and both carry this body, in this conversation's issue.
type messageReplyPlan struct {
	replyRowPlan
	// block is the block of the answered message the reply names, nil when it names none; a
	// follow-up names none, so one plan serves both rows.
	block *namedBlock
	// body is the reply row's, which its index and its references were planned for.
	body string
	// node is the write of the reply's node, planned for the reserved id (planMessageNode).
	node graphstore.Uncaused
}

// planMessageReply plans one session reply above every row lock the route takes: the block the
// reply names with the index its message owes, and the reply's own index and references, for an
// id and a created_at reserved here. Nothing it plans is written here.
//
// The reply row is not inserted yet, so the document its `#id`s resolve against is handed over,
// named by the fields that row will hold (planPendingItem, refs.PendingSource), as
// createStoredMessage hands it over for a row it has only queued. The named block is addressed
// here for the same reason: the read that gives a stored reply its block would find no row yet.
// A direct message writes no mention, so its blocks hold none and it moves nothing. who is the
// session replying, which a block it may not see is refused to.
func (s *server) planMessageReply(
	ctx context.Context, tx pgx.Tx, who model.Actor, message model.Message, body string, inReplyToBlock *string,
) (messageReplyPlan, error) {
	plan := messageReplyPlan{body: body}
	if inReplyToBlock != nil {
		repair, block, err := s.planReplyBlock(ctx, tx, who, refs.ItemMessage, message.ID, *inReplyToBlock, "in_reply_to_block", "INVALID_MESSAGE")
		if err != nil {
			plan.refusal, err = deferredRefusal(err)
			return plan, err
		}
		plan.parentIndex, plan.block = repair, block
		plan.replyBlock = block.replyBlock(message.IssueKey, "", "")
	}
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&plan.id, &plan.createdAt); err != nil {
		return plan, err
	}
	// The reply's node, in its message's issue's project or between its thread's parties
	// (planMessageNode); a follow-up answers the first reply, in the same thread and issue.
	var err error
	if plan.node, err = planMessageNode(ctx, tx, plan.id, message.IssueKey, &message.ID, who, message.Target, plan.createdAt); err != nil {
		return plan, err
	}
	plan.changes, plan.write, err = s.planPendingItem(
		ctx, tx, who, refs.ItemMessage, plan.id, plan.body, refs.PendingSource{IssueKey: message.IssueKey},
		plan.createdAt, message.IssueKey != nil,
	)
	return plan, err
}

func (s *server) replyMessage(w http.ResponseWriter, r *http.Request) {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires an agent bearer token")
		return
	}
	var input messageReplyInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if actor.Kind != "session" || input.Attempt < 1 || (input.Body == nil) == (input.Error == nil) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, "reply requires one body or error for a delivery attempt")
		return
	}
	if input.Body != nil {
		if strings.TrimSpace(*input.Body) == "" {
			writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "message body is required")
			return
		}
		// A reply is stored as a message of its own, so it takes the message route's bound.
		if err := blockBoundError(itemblocks.Check("body", *input.Body)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.InReplyToBlock != nil && input.Body == nil {
		writeError(w, "INVALID_MESSAGE", http.StatusBadRequest, "in_reply_to_block names the block a reply's body answers; an error report has none")
		return
	}

	followUp, err := followUpRequested(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The message's target and the session its attempt names are read before the transaction,
	// so a reply from a session the attempt does not name asks the listener, with nothing held,
	// whether that session holds the role now, for a message sent for a route to a role, or
	// replaced the attempt's session, for one sent to a session (lookUpRoleHolderReply). A read
	// that finds nothing asks nothing: the transaction below answers that message or attempt as it
	// always has. A read that fails is the route's 500. An attempt past
	// message_deliveries.attempt's range names no delivery, so no read is made with it: the bound
	// below (fitsIntegerColumn) answers it, where it always has.
	var roleReply roleHolderReply
	if fitsIntegerColumn(input.Attempt) {
		roleReply, err = s.roleHolderReplyOf(r, actor, r.PathValue("id"), `
		select coalesce(m.target, ''), d.session_id
		from messages m left join message_deliveries d on d.message_id = m.id and d.attempt = $2
		where m.id = $1
	`, input.Attempt)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	// The first read is for the issue key alone. A delete committing behind it leaves the row
	// it holds stale, so the issue is locked first - the order createStoredMessage takes
	// (messages.go) - and the row this route judges and answers from is the one read again
	// under that lock. A message with no issue takes no lock and needs none: a delete refuses
	// one (NOT_AN_ISSUE_MESSAGE), so its deleted_at is never set.
	message, err := s.loadMessage(r.Context(), tx, "", r.PathValue("id"))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message not found")
		return
	}
	if message.IssueKey != nil {
		if _, err := s.lockIssue(r.Context(), tx, *message.IssueKey); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if message, err = s.loadMessage(r.Context(), tx, "", message.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// A reply adds text under the post, which a deleted one no longer holds. The session keeps
	// its attempt: a reply it cannot post is this refusal, not a lost delivery.
	if message.DeletedAt != nil {
		s.writeHandlerError(w, deletedPostError(refs.ItemMessage))
		return
	}
	// This reply - its own body, and the block it names - is read and planned here, before the
	// first statement that takes a row lock, so no document work runs while the transaction
	// holds a transaction id (INFRA-636). The refusal the planning earns is raised in the
	// branch that answers with it, where it was raised; the index it finds the parent owes is
	// written there too, under the parent's lock, which is taken below: the order
	// refs.IndexItemIfOwed states, and the one queueReplyParentMessageLock queues for a route
	// with a write phase.
	var plan messageReplyPlan
	if input.Body != nil {
		if plan, err = s.planMessageReply(r.Context(), tx, actor, message, *input.Body, input.InReplyToBlock); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.InReplyToBlock != nil {
		// Before the attempt row, as the accept route takes them: issue, message, attempt.
		if err := lockReplyParentMessage(r.Context(), tx, message.ID); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The attempt is a number message_deliveries keeps in an integer column, so one past that
	// column (fitsIntegerColumn) is a delivery no message has, answered as the read below answers
	// one, before any query is made with it.
	if !fitsIntegerColumn(input.Attempt) {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	attempt, err := scanMessageDelivery(tx.QueryRow(r.Context(), `
		select `+messageDeliveryColumns+`
		from message_deliveries where message_id = $1 and attempt = $2 for update
	`, message.ID, input.Attempt))
	if err != nil {
		writeError(w, "MESSAGE_NOT_FOUND", http.StatusNotFound, "message delivery not found")
		return
	}
	// The session the attempt names answers it, and so does the session that holds the role the
	// message went to now, while it is live, and a session that replaced the one the attempt
	// names: a different session that took the role, or that replaced the session, is handed what
	// that session had not read (lookUpRoleHolderReply).
	if attempt.SessionID != actor.ID && !roleReply.allows(messageTarget(message.Target)) {
		writeError(w, "REPLY_FORBIDDEN", http.StatusForbidden, replyForbiddenSession)
		return
	}
	// Each outcome is a response, its status, and the event it appended, if any; they share one
	// commit, publish and write.
	var response any
	status := http.StatusOK
	var events []model.Event
	switch {
	case attempt.ReplyID != nil:
		if followUp && input.InReplyToBlock != nil {
			writeError(w, "INVALID_MESSAGE", http.StatusBadRequest,
				"a follow-up is filed under the session's first reply, so it names no block of message "+message.ID)
			return
		}
		answer, err := s.loadMessage(r.Context(), tx, messageIssueKey(message), *attempt.ReplyID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		read, posted, err := s.replyAfterAnswer(r.Context(), tx, answer, actor, input.Body != nil && followUp, plan, wantsDisplayTrees(r))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// A message handed back as it is stored reads the block it answers as every read does.
		// It is a database read, and loadMessage beside it is in this window whatever this read
		// does, so it stays on the transaction, where replyComment's own replay hydrates too.
		if posted == nil {
			if err := hydrateMessageReplyBlocks(r.Context(), tx, actor, &read.Message); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			// The hydration wrote the block into the row the shape stands in front of, so the
			// shape is taken again over it (display_trees.go).
			shapeBuilt(wantsDisplayTrees(r), read.shape)
		}
		response = read
		if posted != nil {
			events = append(events, *posted)
			status = http.StatusCreated
		}
	case input.Error != nil && attempt.State == "failed":
		response = attempt
	case input.Error != nil:
		updated, err := scanMessageDelivery(tx.QueryRow(r.Context(), `
			update message_deliveries set state = 'failed', error = $3 where message_id = $1 and attempt = $2
			returning `+messageDeliveryColumns,
			message.ID, input.Attempt, *input.Error,
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		receipt, err := s.appendEvent(r.Context(), tx, messageDeliveryReceipt(message, actor, updated, ""))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response, events = updated, append(events, receipt)
	default:
		if plan.refusal != nil {
			s.writeHandlerError(w, plan.refusal)
			return
		}
		if err := plan.parentIndex.Write(r.Context(), tx); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		reply, answered, err := s.insertSessionReply(r.Context(), tx, message, actor, plan)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		// The session answered, so the message reached it whatever the attempt recorded: a failed
		// attempt (a stale receipt, an error the session itself reported) reads as sent and answered.
		// So the session that attempt names is a node of the store from this write on, as a
		// settle's is, written in the same batch as the attempt's update. The store records no
		// delivery of a message yet, though a message is a node, so the answer states the
		// session's node alone and no delivered_to (queueAnsweredDelivery).
		if err := store.RunWrite(r.Context(), tx, func(b *store.WriteBatch) error {
			b.Queue("", `
				update message_deliveries set reply_id = $3, state = 'sent', error = null
				where message_id = $1 and attempt = $2
			`, message.ID, input.Attempt, reply.ID)
			return queueAnsweredDelivery(b, answeredDelivery{
				session: attempt.SessionID, was: attempt.State,
			}, actor, answered.CreatedAt, answered.ID)
		}); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response, events, status = replyRead{shapedMessage: newShapedMessage(reply, wantsDisplayTrees(r))}, append(events, answered), http.StatusCreated
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), events...)
	WriteJSON(w, status, response)
}

// replyAfterAnswer is the reply route on an attempt the session already answered with answer.
// post is this call asking to post the plan's body under that answer: the request carried a
// body and named ?follow_up=true. Without it every call is a retry of that answer and posts
// nothing: the host's automatic BTW answer is sent that way, so a frame the session is handed
// twice can never post a second answer, and an error report after an answer is a retry too. A
// follow-up whose text the session already posted in this conversation (its first reply, or an
// earlier follow-up) is a resend and posts nothing; any other is posted under the first reply,
// and its event is returned. A follow-up under an answer the session has deleted is refused, as
// a reply to any other deleted post is. A posted follow-up is the plan's row: the route planned
// one reply for this body above its locks, and at most one of the two branches writes it
// (messageReplyPlan).
func (s *server) replyAfterAnswer(
	ctx context.Context, tx pgx.Tx, answer model.Message, actor model.Actor, post bool,
	plan messageReplyPlan, trees bool,
) (replyRead, *model.Event, error) {
	if !post {
		return replyRead{shapedMessage: newShapedMessage(answer, trees), Duplicate: true}, nil, nil
	}
	// The answer is the session's own post, which it may have deleted since it wrote it: a
	// follow-up adds text under it, and a deleted post takes none, which is the refusal the
	// message replied to takes above. A retry above this line posts nothing, so it stands and
	// hands back the answer as every other read of a deleted post gives it, with no body.
	//
	// The answer was read under its issue's lock (replyMessage), so no delete commits behind
	// it; an answer in a conversation with no issue is never deleted at all.
	if answer.DeletedAt != nil {
		return replyRead{}, nil, deletedPostError(refs.ItemMessage)
	}
	posted, err := scanMessage(tx.QueryRow(ctx, `
		select `+messageColumns+`
		from messages
		where (id = $1 or in_reply_to = $1) and body = $2
		  and author->>'kind' = 'session' and author->>'id' = $3
		order by created_at desc limit 1
	`, answer.ID, plan.body, actor.ID))
	if err == nil {
		return replyRead{shapedMessage: newShapedMessage(posted, trees), Duplicate: true}, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return replyRead{}, nil, err
	}
	followUpReply, event, err := s.insertSessionReply(ctx, tx, answer, actor, plan)
	if err != nil {
		return replyRead{}, nil, err
	}
	return replyRead{shapedMessage: newShapedMessage(followUpReply, trees)}, &event, nil
}

// replyRead is what the reply route answers with a message: the message, and `duplicate` when
// the route posted nothing and handed back one the conversation already holds (an answered
// attempt's stored reply, or a follow-up whose text the session already posted). A caller cannot
// otherwise tell a resend from a send, since both carry the message it asked for.
type replyRead struct {
	shapedMessage
	Duplicate bool `json:"duplicate,omitempty"`
}

// followUpRequested reads the reply route's `?follow_up=`: `true` is the session asking to post
// more in a conversation whose attempt it already answered, `false` or absent is not, and any
// other value is refused rather than read as a retry. It is a query parameter rather than a body
// field because the route decodes its body strictly, so a Dispatch that predates follow-ups
// would refuse a body naming it, where it ignores the parameter and answers with its stored
// reply, which the tool already reports as "already answered".
func followUpRequested(r *http.Request) (bool, error) {
	values, present := r.URL.Query()["follow_up"]
	if !present {
		return false, nil
	}
	if len(values) == 1 {
		switch values[0] {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
	}
	return false, errorf(http.StatusBadRequest, "MESSAGE_INPUT", "follow_up must be true or false")
}

// insertSessionReply writes a session's reply to parent in parent's conversation - its issue,
// or none, and its target - with the reply's references and its message.answered event. A reply
// naming a block of parent stores it and previews that block's text in place of the parent's.
//
// plan carries the row: its id and created_at, written explicitly because its index and its
// references were planned for them before this insert ran, the body, the block it names, the
// step that writes its index and the write of its node (planMessageReply). A follow-up names no
// block, and its thread and issue are the first reply's, so the plan's serves both rows the route
// can insert. Nothing of the body and nothing of the reply's own row is read here, so this write
// holds the transaction id for its own statements alone (INFRA-636).
func (s *server) insertSessionReply(
	ctx context.Context, tx pgx.Tx, parent model.Message, actor model.Actor, plan messageReplyPlan,
) (model.Message, model.Event, error) {
	author, err := json.Marshal(actor)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	// The reply's node goes in before its row. The node write and the row's insert go to the
	// server as one batch (store.WriteBatch), the node write first, as createStoredMessage writes
	// a message's node and row: the node write adds no round trip while the reply holds its
	// transaction id. What that write states is appended in the batch that appends the
	// message.answered event below (appendCausingEvent).
	var reply model.Message
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		graphstore.QueueNodes(b, plan.node)
		b.Queue("", `
			insert into messages (id, issue_key, author, body, target, in_reply_to, replies_to_block, created_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
			returning `+messageColumns+`
		`, plan.id, parent.IssueKey, author, plan.body, parent.Target, parent.ID, plan.block.nodeID(), plan.createdAt).QueryRow(func(row pgx.Row) error {
			var err error
			reply, err = scanMessage(row)
			return err
		})
		return nil
	}); err != nil {
		return model.Message{}, model.Event{}, err
	}
	// The block was read above the route's locks, where this row did not exist yet, so the
	// reply carries it from the plan rather than reading it back (createStoredMessage).
	reply.ReplyBlock = plan.replyBlock
	replyParent := messageReplyParentOf(parent)
	if plan.block != nil {
		replyParent.ReplyBody = messageReplyPreview(plan.block.excerpt)
	}
	// The reply's blocks, the mentions each holds and its own references were planned above the
	// route's locks; their statements run here as one batch of their own, where the index of a
	// reply this route wrote has always run.
	if err := store.RunWrite(ctx, tx, plan.write); err != nil {
		return model.Message{}, model.Event{}, err
	}
	thread, err := replyParent.thread(ctx, tx)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	payload := thread.payload(reply, plan.changes)
	payload.ThreadLogins, err = messageThreadLogins(ctx, tx, reply)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	event, err := s.appendCausingEvent(ctx, tx, messageEvent(reply, "message.answered", actor, payload), plan.node)
	if err != nil {
		return model.Message{}, model.Event{}, err
	}
	if reply.IssueKey != nil {
		if err := refs.Stamp(ctx, tx, "message", reply.ID, event.ID); err != nil {
			return model.Message{}, model.Event{}, err
		}
	}
	return reply, event, nil
}

// messageDeliveryColumns is the message_deliveries select list scanMessageDelivery reads, in
// scan order.
const messageDeliveryColumns = `message_id::text, attempt, delivery, session_id, envelope_id, duplicate, state, error, reply_id::text, created_at, requested_by, accepted_at, accepted_as`

// scanMessageDelivery decodes one messageDeliveryColumns row; extra receives any columns
// selected after them.
func scanMessageDelivery(row pgx.Row, extra ...any) (model.MessageDelivery, error) {
	var delivery model.MessageDelivery
	var requestedBy []byte
	fields := []any{
		&delivery.MessageID, &delivery.Attempt, &delivery.Delivery, &delivery.SessionID, &delivery.EnvelopeID,
		&delivery.Duplicate, &delivery.State, &delivery.Error, &delivery.ReplyID, &delivery.CreatedAt,
		&requestedBy, &delivery.AcceptedAt, &delivery.AcceptedAs,
	}
	if err := row.Scan(append(fields, extra...)...); err != nil {
		return model.MessageDelivery{}, err
	}
	if requestedBy != nil {
		delivery.RequestedBy = &model.Actor{}
		if err := json.Unmarshal(requestedBy, delivery.RequestedBy); err != nil {
			return model.MessageDelivery{}, fmt.Errorf("decode delivery requester: %w", err)
		}
	}
	return delivery, nil
}

// loadMessageDeliveries reads every delivery attempt of the given messages in one query, keyed
// by message id and ordered by attempt within each; a message with no attempts maps to an empty
// (never nil) slice.
func (s *server) loadMessageDeliveries(ctx context.Context, q queryer, messageIDs []string) (map[string][]model.MessageDelivery, error) {
	deliveries := make(map[string][]model.MessageDelivery, len(messageIDs))
	for _, id := range messageIDs {
		deliveries[id] = []model.MessageDelivery{}
	}
	if len(messageIDs) == 0 {
		return deliveries, nil
	}
	rows, err := q.Query(ctx, `
		select `+messageDeliveryColumns+`
		from message_deliveries where message_id = any($1::uuid[]) order by message_id, attempt
	`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		delivery, err := scanMessageDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries[delivery.MessageID] = append(deliveries[delivery.MessageID], delivery)
	}
	return deliveries, rows.Err()
}

// hydrateWrittenMessage sets message.Deliveries to every attempt of the message, as GET
// /api/v1/messages/{id} lists them (hydrateMessageThreads), for the answer of a write of it. A
// correction (editMessage, deleteMessage) passes its own transaction as q, in its read phase,
// before its write phase runs, so a read that fails ends the request with nothing written; the
// delete sets the attempts its settle leaves in the answer itself. The other callers pass the
// pool after the write committed, so a read that fails there fails the answer of a write that
// stands: a keyed repeat's answer (recoveredMessage, freshMessage), and the answer of a targeted
// message whose attempt 1 another sender took (createStoredMessage's errAttemptNotPending).
func (s *server) hydrateWrittenMessage(ctx context.Context, q queryer, message *model.Message) error {
	deliveries, err := s.loadMessageDeliveries(ctx, q, []string{message.ID})
	if err != nil {
		return err
	}
	message.Deliveries = deliveries[message.ID]
	return nil
}
