package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/access"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const maxMessageReplyPreview16 = 160

type createMessageInput struct {
	Body      string  `json:"body" required:"true"`
	InReplyTo *string `json:"in_reply_to"`
	// InReplyToBlock is the id of the block of message InReplyTo the reply answers ("b2").
	InReplyToBlock *string      `json:"in_reply_to_block"`
	Target         *string      `json:"target"`
	Delivery       *string      `json:"delivery"`
	Urgency        *string      `json:"urgency"`
	Actor          *model.Actor `json:"actor"`
}

// messageRead is one conversation as a route answers it: its root message and every message
// replying to it, each shaped for the dashboard (shapedMessage) so a caller that asked for the
// display trees is given them beside every body.
type messageRead struct {
	Message shapedMessage   `json:"message"`
	Replies []shapedMessage `json:"replies"`
	// Unread is this conversation's own unread verdict for the caller, from the shared
	// unreadDirectRepliesCTE: the one definition the count reads too, so a client never
	// re-derives it from timestamps. Only the agent-conversation list sets it.
	Unread bool `json:"unread,omitempty"`
}

// createMessage is POST /api/v1/issues/{key}/messages. A reply names its parent in in_reply_to, a
// message of that issue its writer may read (access.MessageClause, in createStoredMessage's parent
// read); any other id answers 400 MESSAGE_INPUT, as an id no message holds does.
func (s *server) createMessage(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	var input createMessageInput
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
	delivery, err := validateCreateMessage(&input)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader, ok := s.readerAs(w, r, actor)
	if !ok {
		return
	}
	issueKey := r.PathValue("key")
	message, advice, err := s.createStoredMessage(r.Context(), &issueKey, input, delivery, false, actor, reader)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, withAdvice(newShapedMessage(message, wantsDisplayTrees(r)), advice))
}

// createAgentMessageInput is the body of POST /api/v1/agents/{session_id}/messages.
type createAgentMessageInput struct {
	Body           string  `json:"body" required:"true"`
	Delivery       string  `json:"delivery" required:"true"`
	InReplyTo      *string `json:"in_reply_to"`
	InReplyToBlock *string `json:"in_reply_to_block"`
	// ConnectedOnly asks that this message reach a session that is running now or not at all:
	// the send is made with the listener's connected_only, which refuses a session the listener
	// knows and that is not running rather than storing the message for it, and the attempt
	// records that refusal as its failure, as it does on every later attempt of the message. The
	// live view's model and thinking switch sets it; a switch sent without it, from a tab loaded
	// before the field existed, is held to a running session all the same (isSessionSwitch), as a
	// person's own Send or Aside (not one an agent wrote for them) whose body is one switch line.
	// Every other send that leaves it out is queued for a stopped session, and the listener hands
	// it the message when it runs again. An issue message takes no such field: a stopped
	// session's is queued whatever its body says.
	ConnectedOnly bool `json:"connected_only"`
}

// createAgentMessage is POST /api/v1/agents/{session_id}/messages: a person's send to one session.
// A reply names its parent in in_reply_to: an issue-less message whose thread root targets that
// same session and that its writer may read (access.MessageClause), so a direct message of a
// conversation the writer may not read answers as an id no message holds (400 MESSAGE_INPUT
// otherwise). A send under an Idempotency-Key commits its message as the key's recovery point, and
// a repeat is answered from it (replayAgentMessage).
func (s *server) createAgentMessage(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input createAgentMessageInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	target := "session:" + r.PathValue("session_id")
	messageInput := createMessageInput{
		Body: input.Body, InReplyTo: input.InReplyTo, InReplyToBlock: input.InReplyToBlock,
		Target: &target, Delivery: &input.Delivery,
	}
	delivery, err := validateCreateMessage(&messageInput)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	reader, ok := s.readerAs(w, r, actor)
	if !ok {
		return
	}
	message, _, err := s.createStoredMessage(r.Context(), nil, messageInput, delivery, input.ConnectedOnly, actor, reader)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, newShapedMessage(message, wantsDisplayTrees(r)))
}

// messageRecovery is the recovery point a keyed message write commits with its message
// (bindRequestKeyRecovery): the message, and the mode it was sent in, empty for a message that
// targets nobody.
type messageRecovery struct {
	MessageID string `json:"message_id"`
	Delivery  string `json:"delivery"`
}

// recoveredMessage is the message a keyed write committed, read once sendHeldMessageOn has put a
// targeted one on its way, with its deliveries as they stand and the block it answers, read for
// who, the writer the repeat answers. It is read from the database before the listener is, so a
// repeat never fails because the listener is down.
func (s *server) recoveredMessage(ctx context.Context, who model.Actor, point messageRecovery) (model.Message, error) {
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, who, "", point.MessageID)
	if err != nil {
		return model.Message{}, err
	}
	// loadMessage scans the row alone, so a reply that names a block carries only its node id:
	// without this the repeat of a keyed reply answers reply_block null where the first answered
	// the block.
	if err := hydrateMessageReplyBlocks(ctx, s.deps.Store.Pool, who, &message); err != nil {
		return model.Message{}, err
	}
	if message.Target != nil {
		if err := s.sendHeldMessageOn(ctx, message, point.Delivery); err != nil {
			return model.Message{}, err
		}
	}
	deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, who, []string{message.ID})
	if err != nil {
		return model.Message{}, err
	}
	message.Deliveries = deliveries[message.ID]
	return message, nil
}

// replayAgentMessage answers a repeat of a keyed single-session send whose message committed: the
// message as GET /api/v1/agents/{session_id}/messages reads it (recoveredMessage), under the send's
// own 201.
func (s *server) replayAgentMessage(w http.ResponseWriter, r *http.Request, point messageRecovery, _ []byte) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	message, err := s.recoveredMessage(r.Context(), who, point)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, newShapedMessage(message, wantsDisplayTrees(r)))
}

// replayIssueMessage answers a repeat of a keyed issue message whose message committed: the
// message as it stands (recoveredMessage), a targeted one's owed delivery made first, in the first
// answer's shape - its advice kept - under the post's own 201.
func (s *server) replayIssueMessage(w http.ResponseWriter, r *http.Request, point messageRecovery, stored []byte) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	message, err := s.recoveredMessage(r.Context(), who, point)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeRefreshed(w, stored, message)
}

// sendHeldMessageOn makes sure a keyed send's message is on its way before its repeat is answered,
// in the mode it was sent in, as its author's send: the attempt it owes (owedMessageAttempts) is
// sent bound to its number (deliverMessageResuming), so of two senders that both find it owed, one
// sends and the other finds the attempt taken and sends nothing. A pending attempt whose claim is
// live is in flight and left to its sender, and an attempt that settled owes nothing. The send
// runs on past the request, as the original's does, and a listener it cannot reach is recorded on
// the attempt, never returned.
func (s *server) sendHeldMessageOn(ctx context.Context, message model.Message, delivery string) error {
	owed, err := s.owedMessageAttempts(ctx, []string{message.ID})
	attempt := owed[message.ID]
	if err != nil || attempt == 0 {
		return err
	}
	sending := context.WithoutCancel(ctx)
	if _, err := s.deliverMessageResuming(sending, message, delivery, message.Author, nil, attempt); err != nil &&
		!errors.Is(err, errAttemptNotPending) {
		return err
	}
	return nil
}

// owedMessageAttempts is the attempt each of the targeted messages owes, keyed by message id, with
// a message that owes nothing absent: the first attempt of a message with no attempt at all,
// which only a message an earlier release stored can be, since createStoredMessage writes attempt
// 1 in the transaction that stores the message - or
// the message's latest attempt when it is pending and its claim has lapsed, which Postgres judges
// against the claimed_at it wrote (model.DeliveryClaimFreeSQL). A pending attempt whose claim is
// live is in flight and left to its sender, and a latest attempt that settled owes nothing: an
// earlier pending one a later retry from the agent card overtook is that retry's to answer. A
// keyed send's repeat (sendHeldMessageOn) and a broadcast's (sendOwedBroadcastRecipients) both
// ask it.
func (s *server) owedMessageAttempts(ctx context.Context, messageIDs []string) (map[string]int, error) {
	owed := make(map[string]int, len(messageIDs))
	if len(messageIDs) == 0 {
		return owed, nil
	}
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select m.id::text, coalesce(latest.attempt, 1), coalesce(latest.owed, true)
		from unnest($1::uuid[]) as m(id)
		left join lateral (
			select attempt, state = 'pending' and `+model.DeliveryClaimFreeSQL+` as owed
			from message_deliveries where message_id = m.id
			order by attempt desc
			limit 1
		) latest on true
	`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID string
		var attempt int
		var stranded bool
		if err := rows.Scan(&messageID, &attempt, &stranded); err != nil {
			return nil, err
		}
		if stranded {
			owed[messageID] = attempt
		}
	}
	return owed, rows.Err()
}

// planMessageNode reads, in a request's read phase, the scope of message id, which the request
// is about to store, and answers the write of its node (graphstore.MessageNode), held for the
// event that records the message: the request queues the node write before the row's insert
// (graphstore.QueueNodes) and its cause after the event (graphstore.QueueCaused), both in its
// write phase. A message on an issue is in that issue's project. A message on none is between
// its thread's parties (graphstore.ThreadParties): those of the root it answers, read up
// in_reply_to as messageThreadCTE walks it, or, for a message that answers nothing, its own
// author and target. at is the row's created_at, which the node is dated with.
func planMessageNode(
	ctx context.Context, tx pgx.Tx, id string, issueKey, inReplyTo *string, author model.Actor, target *string, at time.Time,
) (graphstore.Uncaused, error) {
	var project string
	var parties []string
	switch {
	case issueKey != nil:
		var err error
		if project, _, err = graphstore.ScopeOf(ctx, tx, issueKey, nil); err != nil {
			return graphstore.Uncaused{}, err
		}
	case inReplyTo != nil:
		var rootAuthor []byte
		var rootTarget *string
		if err := tx.QueryRow(ctx, messageThreadCTE+`
			select m.author, m.target from messages m join thread t on t.id = m.id
			where t.in_reply_to is null
		`, *inReplyTo).Scan(&rootAuthor, &rootTarget); err != nil {
			return graphstore.Uncaused{}, fmt.Errorf("read the thread root of a reply to message %s: %w", *inReplyTo, err)
		}
		var root model.Actor
		if err := json.Unmarshal(rootAuthor, &root); err != nil {
			return graphstore.Uncaused{}, fmt.Errorf("decode the author of the thread root of message %s: %w", *inReplyTo, err)
		}
		parties = graphstore.ThreadParties(root, rootTarget)
	default:
		parties = graphstore.ThreadParties(author, target)
	}
	return graphstore.PlanUncaused([]graphstore.Node{graphstore.MessageNode(id, project, parties, author, at)}, nil), nil
}

// createStoredMessage writes one message in a single transaction, with it the keyed request's
// recovery point (bindRequestKeyRecovery), so a repeat under the key is answered with this message
// however this request ends, then sends a targeted message's first attempt. connectedOnly is the
// request's connected_only, which only the agents route takes
// (createAgentMessageInput.ConnectedOnly). actor is the writer the message records, and reader is
// that writer as a read binds it (readerAs), which a caller resolves before this opens its
// transaction: the issue the message is on, and the reply block it names, are read as it.
func (s *server) createStoredMessage(
	ctx context.Context,
	issueKey *string,
	input createMessageInput,
	delivery string,
	connectedOnly bool,
	actor, reader model.Actor,
) (model.Message, *writeAdvice, error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return model.Message{}, nil, err
	}
	defer tx.Rollback(ctx)
	// The issue's owner lock is the first lock this write takes, the order
	// recordPendingMessageDelivery states, and the issue is read in the statement after it, without
	// the closed refusal: a closed issue still takes a message. The path names the issue by its key
	// alone, so the issue is read as the writer reads it before the lock is asked for
	// (lockedOwnerReadOnceFound): an issue of a project the writer does not see is a key no issue
	// holds (issueStatus), refused without waiting behind that project's writers.
	var status string
	if issueKey != nil {
		status, _, err = issueStatus(ctx, reader, *issueKey, lockedOwnerReadOnceFound(tx))
		if err != nil {
			return model.Message{}, nil, err
		}
	}
	parent, err := loadMessageReplyParent(ctx, tx, reader, issueKey, input.Target, input.InReplyTo)
	if err != nil {
		return model.Message{}, nil, err
	}
	// A block the reply names is read here, with the index its parent owes planned beside it: the
	// parent's lock and that index are queued with this message's own writes, in that order
	// (below), so neither holds a transaction id while the reply is read (INFRA-636).
	var block *namedBlock
	var parentRepair refs.ItemRepair
	if input.InReplyToBlock != nil {
		if parentRepair, block, err = s.planReplyBlock(ctx, tx, reader, refs.ItemMessage, parent.ID, *input.InReplyToBlock, "in_reply_to_block", "INVALID_MESSAGE"); err != nil {
			return model.Message{}, nil, err
		}
		parent.ReplyBody = messageReplyPreview(block.excerpt)
	}
	// A reply's conversation is its thread root's, so every reply event names the root's
	// target. A human's reply that names no target also continues the thread the way it was
	// last delivered: when the thread's root was targeted at a session, the reply reaches
	// that session too, which needs the thread's delivery mode as well as its target. A
	// session's own reply never inherits - the target would be itself - but the event still
	// has to name the conversation the reply lands in.
	thread := messageReplyThread{ReplyBody: parent.ReplyBody}
	if input.InReplyTo != nil {
		if input.Target == nil && actor.Kind == "user" {
			var rootTarget *string
			rootTarget, delivery, err = threadRootDelivery(ctx, tx, *input.InReplyTo)
			if err != nil {
				return model.Message{}, nil, err
			}
			input.Target = rootTarget
			thread = messageReplyThread{
				ReplyBody:    parent.ReplyBody,
				ThreadTarget: messageTarget(rootTarget),
			}
		} else {
			thread, err = parent.thread(ctx, tx)
			if err != nil {
				return model.Message{}, nil, err
			}
		}
	}
	// A session target the writer may not see is answered as one nobody holds, "no live session"
	// (resolveDeliveryTarget), and this write settles its attempt 1 so, beside the message: a
	// hidden target never has an attempt committed pending, which a sender - the delivery sweep
	// above all, which resolves a session route as the server's reader - would send it.
	hiddenTarget, err := hiddenSessionTarget(ctx, tx, reader, input.Target)
	if err != nil {
		return model.Message{}, nil, err
	}
	author, err := json.Marshal(actor)
	if err != nil {
		return model.Message{}, nil, err
	}
	// The message's index, its references and the advice are read here, before its first write, so
	// none holds a transaction id (INFRA-636). Its id, which both are planned for, and its
	// created_at, the transaction's now() its column defaults to, are taken before the insert
	// (neither assigns one), and the advice counts the message.created this write appends after it
	// (writeAdvice).
	var messageID string
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&messageID, &createdAt); err != nil {
		return model.Message{}, nil, err
	}
	eventType := "message.created"
	if input.InReplyTo != nil {
		eventType = "message.answered"
	}
	// An issue message's thread_logins are read by its event's insert, after the message's insert
	// and the user_threads trigger it fires, on the thread root found here. A direct conversation
	// keeps no Replies row: its Inbox row is the thread root author's Agent row, read here, since
	// nothing the insert fires decides it (directThreadLogins).
	var logins *events.Logins
	var advice *writeAdvice
	if issueKey != nil {
		root, err := messageThreadRoot(ctx, tx, messageID, input.InReplyTo)
		if err != nil {
			return model.Message{}, nil, err
		}
		logins = events.ThreadLoginsOf(root)
		advice = s.writeAdvice(
			ctx, tx, "POST /api/v1/issues/{key}/messages", *issueKey, actor, adviceAsks{}, status, eventType, false,
		)
	}
	message, err := insertedMessage(messageID, issueKey, author, input, createdAt)
	if err != nil {
		return model.Message{}, nil, err
	}
	message.RepliesToBlock = block.nodeID()
	message.ReplyBlock = block.replyBlock(issueKey, "", "")
	var directLogins []string
	if issueKey == nil {
		if directLogins, err = directThreadLogins(ctx, tx, message); err != nil {
			return model.Message{}, nil, err
		}
	}
	// An issue message's blocks, the mentions each holds and its own references are indexed as
	// one write (planPendingItem); a direct message, which writes no mention, has its blocks
	// indexed alone and moves nothing. The message's insert is queued below, so the issue the
	// row will name is handed over: it gives the document its `#id`s resolve against, its
	// issue's spec, which no read of the row can answer yet.
	referenceChanges, indexWrite, err := s.planPendingItem(
		ctx, tx, actor, refs.ItemMessage, messageID, input.Body, refs.PendingSource{IssueKey: issueKey}, createdAt, issueKey != nil,
	)
	if err != nil {
		return model.Message{}, nil, err
	}
	// The message's node, in its issue's project or between its thread's parties, read here in
	// the read phase; its write goes in before the row's insert, and what it states waits for the
	// event below (graphstore.QueueCaused).
	messageNode, err := planMessageNode(ctx, tx, messageID, issueKey, input.InReplyTo, actor, input.Target, createdAt)
	if err != nil {
		return model.Message{}, nil, err
	}
	// The parent's lock, the index its blocks owe, the message's node, the insert, the recovery
	// point, this message's own index, the event, its stamp, what the node write stated and a
	// targeted message's first delivery attempt go in one batch (store.WriteBatch): the message is
	// known before the insert runs, and the stamp and the node's cause name the event's id as the
	// id sequence left it.
	var event model.Event
	var receipt *model.Event
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		if block != nil {
			queueReplyParentMessageLock(b, parent.ID)
			parentRepair.Queue(b)
		}
		graphstore.QueueNodes(b, messageNode)
		b.Queue("", `
			insert into messages (id, issue_key, author, body, target, in_reply_to, replies_to_block, created_at)
			values ($1, $2, $3, $4, $5, $6, $7, $8)
		`, messageID, issueKey, author, input.Body, input.Target, input.InReplyTo, message.RepliesToBlock, createdAt)
		// The recovery point right after its message and before the event append: a repeat that
		// took the key over meanwhile refuses it, and this transaction rolls back.
		if err := queueRequestKeyRecovery(ctx, b, messageRecovery{MessageID: messageID, Delivery: delivery}); err != nil {
			return err
		}
		if err := indexWrite(b); err != nil {
			return err
		}
		payload := thread.payload(message, referenceChanges)
		payload.ThreadLogins = directLogins
		if err := s.deps.Events.QueueAppendWithLogins(b, messageEvent(message, eventType, actor, payload), logins, func(appended model.Event) {
			event = appended
		}); err != nil {
			return err
		}
		if issueKey != nil {
			refs.QueueStamp(b, "message", message.ID)
		}
		graphstore.QueueCaused(b, messageNode, graphstore.LatestEvent)
		// A targeted message's attempt 1 is queued here, with the writes of the transaction that
		// stores the message, unclaimed and naming no session yet, as writeBroadcast opens each
		// recipient's: a process that dies, or a request whose connection is cut, between this
		// commit and the send then leaves a pending attempt the delivery sweep
		// (delivery_sweep.go) sends. An earlier release opened it after the listener lookup, so a
		// message it stored could have no attempt at all, and the recovery that opens a first
		// attempt for one (owedKeyedSend, openFirstMessageDeliveryAttempt) serves only the
		// messages such a release stored. It records who asked for it, the urgency they chose and
		// the request's own connected_only (connectedOnly, the agents route's alone), which every
		// later attempt of the message inherits (insertMessageDeliveryAttempt). No attempt row
		// records the switch rule (isSessionSwitch): each send judges it from the message and the
		// mode it is sent in (deliverMessageResuming), so this request's own send of a switch asks
		// for a running session while a later Use BTW instead, which changes no model, inherits
		// nothing of it. The target is read here and not at validation, since a person's reply
		// that names none takes its thread root's above. A target hidden from the writer has its
		// attempt 1 written failed instead, naming that session as its resolve would
		// (hiddenSessionTarget), with the receipt the settle of such an attempt appends: nothing is
		// sent for it, and nothing is left owed.
		switch {
		case message.Target != nil && hiddenTarget != "":
			failure := noLiveSessionPrefix + " " + hiddenTarget
			b.Queue("", `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state, error, requested_by,
				                                urgency, connected_only)
				values ($1, 1, $2, $6, 'failed', $7, $3, $4, $5)
			`, messageID, delivery, author, input.Urgency, model.ConnectedOnlyColumn(connectedOnly), hiddenTarget, failure)
			return s.deps.Events.QueueAppend(b, messageDeliveryReceipt(message, actor, model.MessageDelivery{
				MessageID: messageID, Attempt: 1, Delivery: delivery, SessionID: hiddenTarget,
				State: model.DeliveryStateFailed, Error: &failure,
			}, ""), func(appended model.Event) { receipt = &appended })
		case message.Target != nil:
			b.Queue("", `
				insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by,
				                                urgency, connected_only)
				values ($1, 1, $2, '', 'pending', $3, $4, $5)
			`, messageID, delivery, author, input.Urgency, model.ConnectedOnlyColumn(connectedOnly))
		}
		return nil
	}); err != nil {
		return model.Message{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.Message{}, nil, err
	}
	s.publish(ctx, event)
	if receipt != nil {
		s.publish(ctx, *receipt)
	}
	s.storeDisplayTrees(ctx, message.ID, input.Body)
	if message.Target != nil && hiddenTarget != "" {
		deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, reader, []string{message.ID})
		if err != nil {
			return model.Message{}, nil, err
		}
		message.Deliveries = deliveries[message.ID]
		return message, advice, nil
	}
	if message.Target != nil {
		// The send keeps the request's values (its transaction tracking) and drops its
		// cancellation: a connection cut after the commit - a closed tab, a front door's pod
		// roll - must not take the send with it, and nor must a deploy: http.Server.Shutdown
		// waits for this handler, so the send finishes inside the drain, and the listener
		// client's receipt timeout bounds it there.
		sending := context.WithoutCancel(ctx)
		// Bound to attempt 1, never the unbound send, so this request cannot open a second
		// attempt beside one the delivery sweep or a person's Retry already took. Another sender
		// owning attempt 1 - a repeat of a keyed send that found this message committed
		// (sendHeldMessageOn) among them - means the message's delivery is under way there: the
		// message is stored, so it is answered with the attempts as that sender has left them.
		attempt, err := s.deliverMessageResuming(sending, message, delivery, actor, &thread, 1)
		switch {
		case errors.Is(err, errAttemptNotPending):
			deliveries, err := s.loadMessageDeliveries(sending, s.deps.Store.Pool, reader, []string{message.ID})
			if err != nil {
				return model.Message{}, nil, err
			}
			message.Deliveries = deliveries[message.ID]
		case err != nil:
			return model.Message{}, nil, err
		default:
			message.Deliveries = []model.MessageDelivery{attempt}
		}
	}
	return message, advice, nil
}

// hiddenSessionTarget answers the session a message's target names when reader may not see it
// (sessionHiddenThrough, read through q), and "" for a session it sees, a role, and no target. A
// role's holder is chosen when the message is sent, and its resolve answers a hidden holder as a
// role nobody claims (resolveDeliveryTarget).
func hiddenSessionTarget(ctx context.Context, q queryer, reader model.Actor, target *string) (string, error) {
	if target == nil {
		return "", nil
	}
	route, err := model.ParseRoute(*target)
	if err != nil || route.Kind != "session" {
		return "", nil
	}
	hidden, err := sessionHiddenThrough(ctx, q, reader, route.ID)
	if err != nil || !hidden {
		return "", err
	}
	return route.ID, nil
}

func messageEvent(message model.Message, eventType string, actor model.Actor, payload any) model.Event {
	if message.IssueKey != nil {
		return issueOwner(*message.IssueKey).event(eventType, actor, payload)
	}
	return model.Event{Type: eventType, Actor: actor, Payload: payload}
}

// validateCreateMessage holds a send to what a message may carry and leaves input ready to store:
// the body with line feeds alone, and the target as validateMessageDelivery resolved it. A body is
// markdown a reader is shown, so it reaches the store the way text written into a document reaches
// it (pmdoc.LineFeeds), and every surface that reads it back - the message row, its event, its
// delivery and the tree the display read makes of it - sees one line ending.
func validateCreateMessage(input *createMessageInput) (string, error) {
	input.Body = pmdoc.LineFeeds(input.Body)
	if strings.TrimSpace(input.Body) == "" {
		return "", errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if err := blockBoundError(itemblocks.Check("body", input.Body)); err != nil {
		return "", err
	}
	target, delivery, err := validateMessageDelivery(input.Target, input.Delivery)
	if err != nil {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "%s", err)
	}
	input.Target = target
	if input.InReplyToBlock != nil && input.InReplyTo == nil {
		return "", errorf(http.StatusBadRequest, "INVALID_MESSAGE", "in_reply_to_block requires in_reply_to, the message whose block it names")
	}
	if input.Urgency != nil && !validMessageUrgency(*input.Urgency) {
		return "", errorf(http.StatusBadRequest, "MESSAGE_INPUT", "urgency must be one of low, med, high, blocking")
	}
	return delivery, nil
}

// isSessionSwitchCommand reports whether body is a model or thinking switch line, the line the
// live view's switch writes: the whole body, trimmed, is one `/model <value>` or `/effort <value>`
// line, its value not empty and holding no whitespace (switchMessageInput in
// packages/dispatch/web/src/features/agent-view/use-session-switch.ts, and switchCommand there
// in the release before it). A body that only mentions /model in its prose is not one. The body
// alone decides nothing: isSessionSwitch judges whether a send of it would switch the session.
func isSessionSwitchCommand(body string) bool {
	command := strings.TrimSpace(body)
	value, found := strings.CutPrefix(command, "/model ")
	if !found {
		value, found = strings.CutPrefix(command, "/effort ")
	}
	return found && value != "" && !strings.ContainsFunc(value, unicode.IsSpace)
}

// isSessionSwitch reports whether a send of message in mode delivery is a model or thinking switch
// the session runs as its own turn: a message a person wrote themselves (an author of kind "user"
// with no act, the agent an author records when it wrote for its person) on no issue, no
// broadcast's copy, sent as Send (steer) or Aside, whose body is one switch line
// (isSessionSwitchCommand). It mirrors isUserTurnCandidate in
// packages/pi-shared/src/accept-user-turn.ts and the accept route's own checks (acceptDelivery,
// which refuses a message an agent wrote for its person as ACCEPT_WRITTEN_BY_AGENT), which admit
// only that shape to run as the session's own turn. Every other message reaches the session as a
// card and changes no model - an issue message, a mention, a broadcast copy, a BTW, a session's
// message and a message an agent wrote for its person among them, whatever its body - so a stopped
// session's listener stores it like any other message (spec R6).
//
// A send that is one asks the listener for a session that is running now whatever its request
// said, at every attempt and every send, each judged in the mode that attempt is sent in and never
// stored on a row (deliverMessageResuming): a switch an earlier release stored with no flag on its
// rows included, and a Retry or a Use Aside instead of a switch included, while a Use BTW instead
// of it is queued. This rule decides a switch, not the client's connected_only, which a send to a
// session may still set to ask the same (createAgentMessageInput.ConnectedOnly). A tab loaded
// before connected_only existed sends the switch without the field, and a switch stored for a
// stopped session would change that session's model or thinking level at an unknown later time,
// long after the person chose it. The spec keeps the switch out of the queued path (its non-goal,
// dispatch://INFRA-626/spec), and plan D9 and its switch rule table refuse it for a stopped session
// with nothing stored (dispatch://INFRA-626/artifact/plan-md).
func isSessionSwitch(message model.Message, delivery string) bool {
	return message.IssueKey == nil && message.BroadcastID == nil &&
		message.Author.Kind == "user" && message.Author.Act == nil &&
		(delivery == "aside" || delivery == "steer") && isSessionSwitchCommand(message.Body)
}

func validMessageUrgency(value string) bool {
	return value == "low" || value == "med" || value == "high" || value == "blocking"
}

func validDelivery(value string) bool {
	return value == "btw" || value == "aside" || value == "steer"
}

func validateMessageDelivery(target *string, delivery *string) (*string, string, error) {
	if target == nil && delivery == nil {
		return nil, "", nil
	}
	if target == nil || strings.TrimSpace(*target) == "" {
		return nil, "", errors.New("delivery requires target")
	}
	if delivery == nil || !validDelivery(*delivery) {
		return nil, "", errors.New("delivery must be one of btw, aside, steer")
	}
	parsed, err := model.ParseRoute(*target)
	if err != nil || (parsed.Kind != "role" && parsed.Kind != "session") {
		return nil, "", errors.New("target must be role:<name> or session:<id>")
	}
	canonical := parsed.Kind + ":" + parsed.ID
	return &canonical, *delivery, nil
}

// messageReplyParent is the message a reply names: its id, the preview its event carries, and
// the parent's own place in the thread, which is the thread root whenever the parent answers
// nothing itself.
type messageReplyParent struct {
	ID        string
	ReplyBody string
	Target    *string
	InReplyTo *string
}

// messageReplyPreview is how much of a parent's body a reply's event and delivery frame carry.
func messageReplyPreview(body string) string {
	return text.HeadRunes(body, maxMessageReplyPreview16)
}

// thread is what a reply to this parent says about the thread it joins.
func (p messageReplyParent) thread(ctx context.Context, tx pgx.Tx) (messageReplyThread, error) {
	threadTarget, err := replyThreadTarget(ctx, tx, p)
	if err != nil {
		return messageReplyThread{}, err
	}
	return messageReplyThread{ReplyBody: p.ReplyBody, ThreadTarget: threadTarget}, nil
}

// messageReplyParentOf is the reply parent an already-loaded message makes, for a caller that
// holds the row rather than an id a request supplied.
func messageReplyParentOf(message model.Message) messageReplyParent {
	return messageReplyParent{
		ID:        message.ID,
		ReplyBody: messageReplyPreview(message.Body),
		Target:    message.Target,
		InReplyTo: message.InReplyTo,
	}
}

// loadMessageReplyParent validates a reply's parent and returns it. A reply stays in its
// parent's conversation: on an issue, the parent is a message of that issue; in an issue-less
// agent conversation, the parent is issue-less and its thread root is targeted at the same
// session the reply is being sent to. The parent is read as who reads it (access.MessageClause):
// one who may not read, such as a direct message of a conversation who is no party of, read by a
// viewer that is not open, is refused exactly as an id no message holds, and nothing of it is
// previewed.
func loadMessageReplyParent(
	ctx context.Context, tx pgx.Tx, who model.Actor, issueKey, target, inReplyTo *string,
) (messageReplyParent, error) {
	if inReplyTo == nil {
		return messageReplyParent{}, nil
	}
	invalid := errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message on this issue")
	if issueKey == nil {
		invalid = errorf(http.StatusBadRequest, "MESSAGE_INPUT", "in_reply_to must identify a message of this session's conversation")
	}
	if strings.TrimSpace(*inReplyTo) == "" {
		return messageReplyParent{}, invalid
	}
	if _, err := uuid.Parse(*inReplyTo); err != nil {
		return messageReplyParent{}, invalid
	}
	var parent messageReplyParent
	var parentBody string
	var err error
	viewer := refs.VisibilityArg(who)
	if issueKey != nil {
		err = tx.QueryRow(ctx, `
			select m.body, m.target, m.in_reply_to::text from messages m
			where m.id = $1 and m.issue_key = $2 and `+access.MessageClause("m", "$3")+`
		`, *inReplyTo, *issueKey, viewer).Scan(&parentBody, &parent.Target, &parent.InReplyTo)
	} else {
		if target == nil {
			return messageReplyParent{}, invalid
		}
		err = tx.QueryRow(ctx, `
			with recursive thread as (
				select m.id, m.body, m.issue_key, m.target, m.in_reply_to, 0 as depth from messages m
				where m.id = $1 and `+access.MessageClause("m", "$3")+`
				union all
				select m.id, m.body, m.issue_key, m.target, m.in_reply_to, t.depth + 1
				from messages m join thread t on m.id = t.in_reply_to
			)
			select
				(select body from thread where depth = 0),
				(select target from thread where depth = 0),
				(select in_reply_to::text from thread where depth = 0)
			from thread
			where in_reply_to is null and issue_key is null and target = $2
			  and not exists (select 1 from thread where issue_key is not null)
		`, *inReplyTo, *target, viewer).Scan(&parentBody, &parent.Target, &parent.InReplyTo)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return messageReplyParent{}, invalid
		}
		return messageReplyParent{}, err
	}
	parent.ID = *inReplyTo
	parent.ReplyBody = messageReplyPreview(parentBody)
	return parent, nil
}

// messageReplyThread is what a message's event and its delivery frame both say about the
// thread it joins: the preview of the parent it answers and the target of that thread's root.
// Both are empty for a root message, which answers nothing and is its own thread.
type messageReplyThread struct {
	ReplyBody    string
	ThreadTarget string
}

// payload is the event or delivery-frame payload for a message in this thread, so the two
// always say the same thing about it.
func (t messageReplyThread) payload(
	message model.Message,
	changes model.ReferenceChanges,
) model.MessageEventPayload {
	payload := model.MessageEventPayload{
		Message:                 message,
		ReferenceChangesPayload: model.NewReferenceChangesPayload(changes),
		ReplyBody:               t.ReplyBody,
		ThreadTarget:            t.ThreadTarget,
	}
	// A body is bounded block by block, so it can be longer than one event carries: the event
	// takes its first part and says so, and a reader gets the rest from the message itself.
	payload.Body, payload.BodyTruncated = model.EventBody(message.Body)
	return payload
}

// loadMessageReplyThread derives that for a stored message, for a caller that did not derive
// it while writing the message, with the parent read as who reads it (loadMessageReplyParent). A
// reply naming a block previews that block's text, as its write did, read from the message's
// ReplyBlock, which the caller hydrates.
func loadMessageReplyThread(ctx context.Context, tx pgx.Tx, who model.Actor, message model.Message) (messageReplyThread, error) {
	parent, err := loadMessageReplyParent(ctx, tx, who, message.IssueKey, message.Target, message.InReplyTo)
	if err != nil {
		return messageReplyThread{}, err
	}
	// A message that answers nothing has no parent to preview: loadMessageReplyParent returns
	// the zero value for a nil in_reply_to, so there is nothing to carry forward.
	if message.InReplyTo == nil {
		return messageReplyThread{}, nil
	}
	if message.ReplyBlock != nil {
		parent.ReplyBody = messageReplyPreview(message.ReplyBlock.Text)
	}
	return parent.thread(ctx, tx)
}

// messageThreadCTE names `thread`: the message $1 and every ancestor it answers, up to the
// thread root. A message's in_reply_to is set once at insert and never updated, so the chain
// only ever points at older rows and the walk terminates.
const messageThreadCTE = `
		with recursive thread as (
			select id, target, in_reply_to from messages where id = $1
			union all
			select m.id, m.target, m.in_reply_to from messages m join thread t on m.id = t.in_reply_to
		)`

// threadRootDelivery walks a reply's ancestry to the thread root and returns that root's
// target - the conversation the whole thread belongs to - together with the mode of the
// thread's most recent delivery attempt, so a reply can be delivered like the thread was.
// An untargeted thread yields nothing.
func threadRootDelivery(ctx context.Context, tx pgx.Tx, inReplyTo string) (*string, string, error) {
	var target *string
	var delivery *string
	err := tx.QueryRow(ctx, messageThreadCTE+`
		select
			(select target from thread where in_reply_to is null),
			(select d.delivery from message_deliveries d join thread t on d.message_id = t.id
			 order by d.created_at desc, d.attempt desc limit 1)
	`, inReplyTo).Scan(&target, &delivery)
	if err != nil {
		return nil, "", err
	}
	if target == nil {
		return nil, "", nil
	}
	if delivery == nil {
		return target, "steer", nil
	}
	return target, *delivery, nil
}

// replyThreadTarget is the target of the thread root a reply to parent lands under: the
// conversation the reply joins, empty under an untargeted thread. The parent is that root
// unless it answers something itself, so only a deeper reply walks.
func replyThreadTarget(ctx context.Context, tx pgx.Tx, parent messageReplyParent) (string, error) {
	if parent.InReplyTo == nil {
		return messageTarget(parent.Target), nil
	}
	var rootTarget *string
	if err := tx.QueryRow(ctx, messageThreadCTE+`
		select (select target from thread where in_reply_to is null)
	`, parent.ID).Scan(&rootTarget); err != nil {
		return "", err
	}
	return messageTarget(rootTarget), nil
}

func messageIssueKey(message model.Message) string {
	if message.IssueKey == nil {
		return ""
	}
	return *message.IssueKey
}

func messageTarget(target *string) string {
	if target == nil {
		return ""
	}
	return *target
}

func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "message") {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	read, err := s.readMessage(r.Context(), who, r.PathValue("key"), r.PathValue("id"), wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// getMessageThread reads the conversation a message belongs to by the id of any message in it:
// the thread root with its deliveries, and every reply, oldest first. It takes no issue, so it
// is how a session reads back a human's direct message and its own replies to it, which belong
// to no issue. A thread on an issue follows the issue's rule, as GET /issues/{key}/messages/{id}
// does. An issue-less thread is a direct conversation, read only by its parties
// (access.PartyClause), and a thread the caller may not read answers as an id nobody holds.
// Within that rule a human reads any of their conversations, and a bearer names its session in
// ?session= and reads only one that session is in (the root targets it, or it authored a reply):
// the session is the caller's own claim, like `actor`, so this keeps a session from reading
// another session's direct conversation by mistake.
func (s *server) getMessageThread(w http.ResponseWriter, r *http.Request) {
	read, _, ok := s.readableMessageThread(w, r)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// readableMessageThread is the conversation message {id} belongs to, when the caller may read it
// under getMessageThread's rule, with the reader it was read for. The thread's root is read first
// through its rule (access.MessageClause): a thread on an issue of a project the caller does not see, or
// a conversation it is no party of, answers as an id no message holds, before any
// SESSION_REQUIRED or THREAD_FORBIDDEN answer could say it is there. Then a person's own
// credential reads it whole, and every other reads an issue thread and only a direct
// conversation its ?session= is in. Every route that reads a message by its id alone (the
// conversation, the message's blocks) asks it. ok is false once it has written the refusal.
func (s *server) readableMessageThread(w http.ResponseWriter, r *http.Request) (messageRead, model.Actor, bool) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return messageRead{}, model.Actor{}, false
	}
	if !requireUUIDPath(w, r, "message") {
		return messageRead{}, model.Actor{}, false
	}
	var rootID string
	if err := s.deps.Store.Pool.QueryRow(r.Context(), messageThreadCTE+`
		select root.id::text from thread join messages root on root.id = thread.id
		where thread.in_reply_to is null and `+access.MessageClause("root", "$2")+`
	`, r.PathValue("id"), refs.VisibilityArg(who)).Scan(&rootID); err != nil {
		s.writeHandlerError(w, err)
		return messageRead{}, model.Actor{}, false
	}
	read, err := s.readMessage(r.Context(), who, "", rootID, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return messageRead{}, model.Actor{}, false
	}
	// A direct conversation is the person's own, so a credential of theirs reads it whole and
	// names no session in it, as their browser does. The scope is the request's credential's;
	// this rule records no actor, so there is nothing else to resolve.
	if !s.callerHoldsScope(r, model.ScopePerson) && read.Message.IssueKey == nil {
		session := strings.TrimSpace(r.URL.Query().Get("session"))
		if session == "" {
			writeError(w, "SESSION_REQUIRED", http.StatusBadRequest,
				"a direct conversation is read as a session in it: name yours in ?session=")
			return messageRead{}, model.Actor{}, false
		}
		if !read.hasSession(session) {
			writeError(w, "THREAD_FORBIDDEN", http.StatusForbidden,
				"session "+session+" is not in this direct conversation: its root targets another session and it has not replied in it")
			return messageRead{}, model.Actor{}, false
		}
	}
	return read, who, true
}

// hasSession reports whether session is in this conversation: the root targets it, or it
// authored a reply.
func (read messageRead) hasSession(session string) bool {
	if messageTarget(read.Message.Target) == "session:"+session {
		return true
	}
	for _, reply := range read.Replies {
		if reply.Author.Kind == "session" && reply.Author.ID == session {
			return true
		}
	}
	return false
}

// readMessage is one message, on issueKey when it is not empty, with its deliveries and every
// message transitively replying to it, read for who: a message who may not read is
// pgx.ErrNoRows, as an id no message holds is (loadMessage).
func (s *server) readMessage(ctx context.Context, who model.Actor, issueKey, id string, trees bool) (messageRead, error) {
	message, err := s.loadMessage(ctx, s.deps.Store.Pool, who, issueKey, id)
	if err != nil {
		return messageRead{}, err
	}
	reads, err := s.hydrateMessageThreads(ctx, who, []model.Message{message}, trees)
	if err != nil {
		return messageRead{}, err
	}
	return reads[0], nil
}

// hydrateMessageThreads is each root as a thread: its own deliveries, every message transitively
// replying to it, and the block each of those messages answers, read for who. Each side table is
// read once over every root, never once per root, so a list of threads costs the same three
// queries one does. trees is whether the caller asked for the display trees (wantsDisplayTrees).
// A message's target and its delivery attempts name sessions, and one who does not see is left
// out (leaveOutHiddenMessageSessions).
//
// Every read of a message passes here, so this is where a message's trees are taken from the
// table and what the table did not hold is stored back (displayTreeReader).
func (s *server) hydrateMessageThreads(ctx context.Context, who model.Actor, roots []model.Message, trees bool) ([]messageRead, error) {
	rootIDs := make([]string, len(roots))
	for index, root := range roots {
		rootIDs[index] = root.ID
	}
	deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, who, rootIDs)
	if err != nil {
		return nil, err
	}
	replies, err := s.loadMessageReplyChains(ctx, s.deps.Store.Pool, who, rootIDs)
	if err != nil {
		return nil, err
	}
	if err := hydrateThreadReplyBlocks(ctx, s.deps.Store.Pool, who, roots, replies); err != nil {
		return nil, err
	}
	messages := make([]*model.Message, 0, len(roots))
	for index := range roots {
		roots[index].Deliveries = deliveries[roots[index].ID]
		messages = append(messages, &roots[index])
		for reply := range replies[roots[index].ID] {
			messages = append(messages, &replies[roots[index].ID][reply])
		}
	}
	if err := leaveOutHiddenMessageSessions(ctx, s.deps.Store.Pool, who, messages); err != nil {
		return nil, err
	}
	reader := s.readDisplayTrees(ctx, trees)
	reads := make([]messageRead, len(roots))
	for index, root := range roots {
		reads[index].Message.Message = root
		reads[index].Message.shape(reader)
		reads[index].Replies = shapedMessages(replies[root.ID], reader)
	}
	reader.resolve()
	return reads, nil
}

// leaveOutHiddenMessageSessions writes messages as who reads them (the rule of references,
// references_seen.go): a target who does not see - a `session:<id>` of a hidden session, or a
// `role:legion-<project>-…` of a private project who does not see - is null, and a delivery
// attempt to a hidden session is left out. Its one read is made whatever the messages name
// (seenReferences); no message reads nothing.
func leaveOutHiddenMessageSessions(ctx context.Context, q queryer, who model.Actor, messages []*model.Message) error {
	if seesEveryObject(who) || len(messages) == 0 {
		return nil
	}
	named := references{}
	for _, message := range messages {
		named.nameRoute(message.Target)
		for _, delivery := range message.Deliveries {
			named.name(sessionReference, delivery.SessionID)
		}
	}
	seen, err := seenReferences(ctx, q, who, named)
	if err != nil {
		return err
	}
	for _, message := range messages {
		seen.leaveOutRoute(&message.Target)
		message.Deliveries = slices.DeleteFunc(message.Deliveries, func(delivery model.MessageDelivery) bool {
			return delivery.SessionID != "" && !seen.holds(sessionReference, delivery.SessionID)
		})
	}
	return nil
}

// listIssueRootMessagesQuery is every root message of one issue, oldest first, when the viewer
// bound at $2 sees the issue's project. It is named rather than written into its handler so that
// the plan test and the equality test read the statement the route runs, not a copy of it:
// messages_issue_roots (20261101182433) is keyed on exactly this predicate and this order, and a
// test that held a copy would keep passing after an edit that took the statement off the index.
// The project's test reads the issue once, by the key the statement is keyed on.
var listIssueRootMessagesQuery = `
		select ` + messageColumns + `
		from messages
		where issue_key = $1 and in_reply_to is null and ` + access.IssueProjectClause("$1", "$2") + `
		order by created_at, id
	`

// listIssueMessages is every root message of one issue, oldest first, each with its deliveries
// and every message replying to it: one row per thread, the list a dashboard shows one card
// from. It is unpaged, as an issue's comments are - the threads on an issue are the whole
// conversation held there. A targeted message is an ordinary root and is listed like any
// other. An issue of a project the caller does not see lists nothing, as a key no issue holds.
func (s *server) listIssueMessages(w http.ResponseWriter, r *http.Request) {
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), listIssueRootMessagesQuery, r.PathValue("key"), refs.VisibilityArg(who))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	roots := []model.Message{}
	for rows.Next() {
		root, err := scanMessage(rows)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		roots = append(roots, root)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	result, err := s.hydrateMessageThreads(r.Context(), who, roots, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

// listAgentMessages is one session's targeted conversations, newest activity first, as the
// caller may read them: the list of a session the caller may not see (access.SessionClause) is
// the empty list a session nobody knows has, and of the rest each conversation passes its own rule
// (access.MessageClause), so a restricted member reads only its own direct conversations and those on
// issues of its projects.
func (s *server) listAgentMessages(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	who, ok := s.readerOf(w, r)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, "MESSAGE_INPUT", http.StatusBadRequest, "session id is required")
		return
	}
	// Newest activity first: a conversation moves up when anyone replies anywhere in it, so the
	// newest one the Agents page opens on is the one that last moved, not the one last started.
	// The window is the 50 that moved last, plus every conversation holding a reply this viewer
	// has not read, in one activity order. The unread term has no ceiling: it is exactly what
	// `unread_replies` counts (both read the shared unreadDirectRepliesCTE), so the count and the
	// window can never disagree. Bounding it would reopen LEGION-301's defect at a higher
	// threshold - a reply counted, never shown, and then marked read by a watermark that only
	// moves forward.
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		with recursive`+unreadDirectRepliesCTE+`,
		-- Two indexed branches rather than one scan with an OR across two tables: the roots
		-- targeted at this session (messages_session_roots) and the roots one of its deliveries
		-- names (message_deliveries_session). An OR spanning messages and message_deliveries can
		-- use neither index, and Postgres reads every root there is.
		candidates as (
			select m.id, m.issue_key, m.author, m.body, m.target, m.in_reply_to, m.broadcast_id, m.created_at, m.replies_to_block
			from messages m
			where m.in_reply_to is null and m.target = 'session:' || $3::text
			  and `+access.SessionClause("$3::text", "$4")+` and `+access.MessageClause("m", "$4")+`
			union
			select m.id, m.issue_key, m.author, m.body, m.target, m.in_reply_to, m.broadcast_id, m.created_at, m.replies_to_block
			from messages m
			join message_deliveries d on d.message_id = m.id
			where m.in_reply_to is null and d.session_id = $3::text
			  and `+access.SessionClause("$3::text", "$4")+` and `+access.MessageClause("m", "$4")+`
		),
		-- One walk down every candidate's replies, not one per candidate. A lateral per root
		-- estimates at the recursive walk's cost times the number of roots, which put the plan
		-- a million units over jit_above_cost: on production's own data the read spent 780 ms
		-- compiling and 3 ms running (LEGION-301).
		descendants as (
			select candidates.id as root_id, reply.id, reply.created_at
			from messages reply join candidates on reply.in_reply_to = candidates.id
			union all
			select descendants.root_id, reply.id, reply.created_at
			from messages reply join descendants on reply.in_reply_to = descendants.id
		),
		activity as (
			select root_id, max(created_at) as latest from descendants group by root_id
		),
		roots as (
			select c.id::text as id, c.issue_key, c.author, c.body, c.target,
			       c.in_reply_to::text as in_reply_to, c.broadcast_id, c.created_at, c.replies_to_block,
			       greatest(c.created_at, activity.latest) as moved,
			       unread.root_id is not null as unread
			from candidates c
			left join activity on activity.root_id = c.id
			left join (select distinct root_id from unread_direct_replies) unread
			  on unread.root_id = c.id
		),
		ranked as (
			select *, row_number() over (order by moved desc, id desc) as rank from roots
		)
		select `+messageColumns+`, unread
		from ranked
		where rank <= 50 or unread
		order by moved desc, id desc
	`, append(unreadDirectRepliesArgs(actor.ID, &sessionID), refs.VisibilityArg(who))...)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	roots := []model.Message{}
	unread := map[string]bool{}
	for rows.Next() {
		var holdsUnread bool
		message, err := scanMessage(rows, &holdsUnread)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		roots = append(roots, message)
		unread[message.ID] = holdsUnread
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows.Close()
	result, err := s.hydrateMessageThreads(r.Context(), who, roots, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// Only this list has an unread verdict to carry; a root the query left out of the map is
	// read, which `json:"unread,omitempty"` omits.
	for index := range result {
		result[index].Unread = unread[result[index].Message.ID]
	}
	WriteJSON(w, http.StatusOK, result)
}

// loadMessage reads message id, on issueKey when it is not empty, as who may read it
// (access.MessageClause): a message who may not read is pgx.ErrNoRows, as an id no message holds is.
func (s *server) loadMessage(ctx context.Context, q queryer, who model.Actor, issueKey, id string) (model.Message, error) {
	query := `
		select ` + messageColumns + `
		from messages m where m.id = $1 and ` + access.MessageClause("m", "$2")
	args := []any{id, refs.VisibilityArg(who)}
	if issueKey != "" {
		query += " and m.issue_key = $3"
		args = append(args, issueKey)
	}
	return scanMessage(q.QueryRow(ctx, query, args...))
}

// insertedMessage is the message createStoredMessage inserts as messageColumns reads it back: its
// author as the jsonb column decodes, its in_reply_to in the uuid type's text form, and its
// created_at the transaction's now(), its column's default. The insert runs in a batch with the
// event that carries this message, so nothing reads the row back before that event is queued.
func insertedMessage(id string, issueKey *string, author []byte, input createMessageInput, createdAt time.Time) (model.Message, error) {
	message := model.Message{
		ID: id, IssueKey: issueKey, Body: input.Body, Target: input.Target, CreatedAt: createdAt,
		Deliveries: []model.MessageDelivery{},
	}
	if input.InReplyTo != nil {
		parent, err := uuid.Parse(*input.InReplyTo)
		if err != nil {
			return model.Message{}, fmt.Errorf("message in_reply_to: %w", err)
		}
		message.InReplyTo = new(parent.String())
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.Message{}, fmt.Errorf("decode message author: %w", err)
	}
	return message, nil
}

// messageColumns is the messages select list scanMessage reads, in scan order.
const messageColumns = `id::text, issue_key, author, body, target, in_reply_to::text, broadcast_id::text, created_at, replies_to_block`

// scanMessage decodes one messageColumns row; extra receives any columns selected after them.
func scanMessage(row pgx.Row, extra ...any) (model.Message, error) {
	var message model.Message
	var author []byte
	dest := append([]any{
		&message.ID, &message.IssueKey, &author, &message.Body, &message.Target, &message.InReplyTo, &message.BroadcastID, &message.CreatedAt, &message.RepliesToBlock,
	}, extra...)
	if err := row.Scan(dest...); err != nil {
		return model.Message{}, err
	}
	if err := json.Unmarshal(author, &message.Author); err != nil {
		return model.Message{}, fmt.Errorf("decode message author: %w", err)
	}
	message.Deliveries = []model.MessageDelivery{}
	return message, nil
}

// loadMessageReplyChains reads, for each seed message, every message transitively replying to
// it, oldest first, in one recursive query, of those who may read (access.MessageClause); each reply
// carries its own deliveries. A seed with no replies maps to an empty (never nil) slice. Seeds
// must not be replies of one another: a reply is grouped under exactly one seed.
func (s *server) loadMessageReplyChains(ctx context.Context, q queryer, who model.Actor, seedIDs []string) (map[string][]model.Message, error) {
	chains := make(map[string][]model.Message, len(seedIDs))
	for _, id := range seedIDs {
		chains[id] = []model.Message{}
	}
	if len(seedIDs) == 0 {
		return chains, nil
	}
	rows, err := q.Query(ctx, `
		with recursive replies as (
			select id, issue_key, author, body, target, in_reply_to, broadcast_id, created_at, replies_to_block, in_reply_to as seed_id
			from messages where in_reply_to = any($1::uuid[])
			union all
			select m.id, m.issue_key, m.author, m.body, m.target, m.in_reply_to, m.broadcast_id, m.created_at, m.replies_to_block, r.seed_id
			from messages m join replies r on m.in_reply_to = r.id
		)
		select `+messageColumns+`, seed_id::text
		from replies
		where `+access.MessageClause("replies", "$2")+`
		order by created_at, id
	`, seedIDs, refs.VisibilityArg(who))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var replyIDs []string
	for rows.Next() {
		var seedID string
		reply, err := scanMessage(rows, &seedID)
		if err != nil {
			return nil, err
		}
		chains[seedID] = append(chains[seedID], reply)
		replyIDs = append(replyIDs, reply.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// A reply in the thread may itself have been delivered (a human's follow-up on a targeted
	// thread); its attempts belong to the chain a reader sees.
	deliveries, err := s.loadMessageDeliveries(ctx, q, who, replyIDs)
	if err != nil {
		return nil, err
	}
	for _, chain := range chains {
		for index := range chain {
			chain[index].Deliveries = deliveries[chain[index].ID]
		}
	}
	return chains, nil
}
