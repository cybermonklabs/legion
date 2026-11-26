package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	dispatchenvoy "github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/inboxwrite"
	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A broadcast is one human's message to many sessions. It is a grouping over the targeted
// messages Dispatch already sends, not a second delivery mechanism: every recipient gets an
// ordinary issue-less message aimed at its own session, carrying the broadcast's id, so each
// recipient's thread, retry and reply work exactly as they do for a message sent from one
// agent card. Nothing reads a broadcast to deliver it; the broadcast row only records what
// the recipients share, and GET /api/v1/broadcasts/{id} reads their messages back.
//
// A selected session that does not advertise the chosen mode is never switched to another
// one: it is excluded before anything is written, and named in the create response. The mode
// a human picked is part of what they said - a steer interrupts an agent and a btw waits for
// an answer - so silently downgrading it would deliver a different message than the one the
// sender composed. Exclusions are not stored: the browser excludes them at compose time from
// the same capability list, and the response covers the race where a session's capabilities
// or liveness changed in between. What persists is who was actually sent to.
//
// Broadcasting needs the person scope (routeScopes), like the one-session route it is built from.

// broadcastDeliveryWorkers is how many recipients are delivered to at once, once the create
// request has already answered. Each worker takes a tracking context of its own
// (store.ForConcurrentRead): the pool's one-connection guard is per context, so workers sharing
// one would both trip it and race on its flag. Four keeps a large send's wall time down without
// holding more than four of the shared pool's connections for one broadcast.
const broadcastDeliveryWorkers = 4

// broadcastSend is one recipient's message and the attempt the sender owes it: the attempt the
// write opened for a new send, and the attempt a repeat found owed (owedMessageAttempts).
// Every send is bound to an attempt, so two senders that both find a recipient owed deliver one
// frame between them rather than one each.
type broadcastSend struct {
	message model.Message
	attempt int
}

// broadcastResolveConcurrency is how many per-session listener reads a send has in flight
// while it resolves its recipients, and broadcastResolveTimeout is how long the whole
// resolution may take. See resolveBroadcastSessions for why a send needs both.
const broadcastResolveConcurrency = 8

const broadcastResolveTimeout = 5 * time.Second

// broadcastResolveAllowance is the budget a send's recipient resolution runs under:
// broadcastResolveTimeout on every server the deployment builds, since register sets no
// broadcastResolveBudget, and whatever a test set otherwise.
func (s *server) broadcastResolveAllowance() time.Duration {
	return cmp.Or(s.broadcastResolveBudget, broadcastResolveTimeout)
}

// broadcast is what a send's recipients share: who sent it, the body they were all sent, the
// mode it was sent in, and when.
type broadcast struct {
	ID     string      `json:"id"`
	Author model.Actor `json:"author"`
	Body   string      `json:"body"`
	// BodyTree is the ProseMirror tree Body is shown as, present only for a caller that sent
	// `Dispatch-Trees: 1`.
	BodyTree  model.DisplayTree `json:"body_tree,omitempty"`
	Delivery  string            `json:"delivery"`
	CreatedAt time.Time         `json:"created_at"`
}

// attachTrees registers the broadcast's body with the request's reader, which fills the tree
// from the table's row of that text or, where there is none, from a reading of it.
func (b *broadcast) attachTrees(reader *displayTreeReader) {
	reader.text(&b.BodyTree, b.Body)
}

// broadcastSummary is one row of the broadcast list: the send, how many sessions it reached,
// and how many of them have answered.
type broadcastSummary struct {
	broadcast
	Recipients int `json:"recipients"`
	Replies    int `json:"replies"`
}

// broadcastRecipient is one session's copy of a broadcast: the message it was sent, with its
// delivery attempts, and the replies threaded under it.
type broadcastRecipient struct {
	SessionID string          `json:"session_id"`
	Message   shapedMessage   `json:"message"`
	Replies   []shapedMessage `json:"replies"`
}

// broadcastRead is a broadcast with every recipient's state.
type broadcastRead struct {
	broadcast
	Recipients []broadcastRecipient `json:"recipients"`
}

// broadcastExclusion is a session the sender selected that was not sent to, and why.
type broadcastExclusion struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
}

// broadcastCreated is the create response: what was sent, and what was left out of it.
type broadcastCreated struct {
	broadcastRead
	Excluded []broadcastExclusion `json:"excluded"`
}

// broadcastTarget is one selected session the send resolved: the live row it matched, or the
// reason it was excluded.
type broadcastTarget struct {
	sessionID string
	title     string
	reason    string
}

// createBroadcastInput is the body of POST /api/v1/broadcasts.
type createBroadcastInput struct {
	Body       string   `json:"body" required:"true"`
	Delivery   string   `json:"delivery" required:"true"`
	SessionIDs []string `json:"session_ids" required:"true"`
	// `idempotency_key` stays declared, though the key is the Idempotency-Key header now,
	// so that a body still carrying one - a tab opened before the cutover, which sends the
	// member and no header - is refused by name rather than ignored: a send whose key was
	// dropped would be sent a second time by the press that meant to replay the first.
	IdempotencyKey json.RawMessage `json:"idempotency_key"`
}

// POST /api/v1/broadcasts. A send under an Idempotency-Key commits its broadcast as the key's
// recovery point, and a repeat is answered from it (replayBroadcast) before the registry is read:
// a retry of a send that landed owes nothing to the registry and must not fail because the
// listener is down.
func (s *server) createBroadcast(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input createBroadcastInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.IdempotencyKey != nil {
		writeError(w, "INVALID_JSON", http.StatusBadRequest,
			"idempotency_key is the Idempotency-Key header, not a body member")
		return
	}
	requested, err := validateBroadcastInput(input.Body, input.Delivery, input.SessionIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	// Each selected session is judged against the session store by its own id, one read each.
	// The listener's live list is one process's in-memory cache of the registry, so a session
	// that is working while that cache refills is simply absent from it, and judging a whole
	// selection against that one view excluded such a session with "no live session" - a
	// message the human composed for it, dropped because Dispatch asked the wrong question.
	// A per-session read asks the store the question the exclusion claims to answer, and the
	// selection is the sender's, not a set the list has to be scanned for, so there is nothing
	// a single list read buys that is worth that.
	sessions, err := s.resolveBroadcastSessions(r.Context(), requested)
	if err != nil {
		// Nothing is written when a read did not answer, or the budget ended before every
		// selected session was read, not even the recipients whose reads answered:
		// a broadcast commits under its idempotency key, whose recovery point keeps the send's
		// exclusions and answers every repeat of that key with them, so a transient refusal
		// turned into a recipient's absence would be frozen into that key forever - the sender's
		// retry would be told the session was left out on purpose, and no retry of theirs could
		// ever reach it.
		if errors.Is(err, dispatchenvoy.ErrUnavailable) {
			writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
			return
		}
		s.writeHandlerError(w, err)
		return
	}
	targets := resolveBroadcastTargets(requested, input.Delivery, sessions)
	recipients := make([]broadcastTarget, 0, len(targets))
	excluded := make([]broadcastExclusion, 0)
	for _, target := range targets {
		if target.reason != "" {
			excluded = append(excluded, broadcastExclusion{
				SessionID: target.sessionID, Title: target.title, Reason: target.reason,
			})
			continue
		}
		recipients = append(recipients, target)
	}
	if len(recipients) == 0 {
		writeError(w, "BROADCAST_EMPTY", http.StatusBadRequest,
			"no selected session can receive this message: "+excludedSummary(excluded))
		return
	}

	created, events, err := s.writeBroadcast(r.Context(), actor, input.Body, input.Delivery, recipients, excluded)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(r.Context(), events...)
	// The send answers as soon as the messages are committed, and delivery runs behind it: a
	// fan-out to many sessions, each bounded by the listener client's receipt timeout, is too
	// long to hold an answer for. A single send to one session answers once its own attempt has
	// settled instead (createStoredMessage), which is what keeps a person's sends to one session
	// in the order they pressed them; a broadcast is not ordered against a later single send to
	// one of its recipients. Every recipient's attempt, receipt and reply reaches an open
	// broadcast view over the live event feed, so nothing is lost by answering first.
	//
	// The delivery runs on a context detached from the request, and the server's background
	// wait holds shutdown for it (waitBackground): SIGTERM cancels the server's lifetime before
	// http.Server.Shutdown drains, so a delivery bound to the lifetime was cut off by a deploy
	// with recipients it had not reached. A deploy that kills the process before they all settle
	// leaves owed recipients the delivery sweep resumes (resumeOwedDeliveries), and a repeat under
	// the key resumes them too (replayBroadcast).
	sends := make([]broadcastSend, 0, len(created.Recipients))
	for _, recipient := range created.Recipients {
		// The write opened each recipient's first attempt, unclaimed, so every worker is bound
		// to that attempt rather than opening one of its own.
		sends = append(sends, broadcastSend{message: recipient.Message.Message, attempt: 1})
	}
	sending := context.WithoutCancel(r.Context())
	s.detached.goDetached(func() { s.deliverBroadcast(sending, sends, input.Delivery, actor) })
	// The send's own answer carries the body it has just stored the tree of, so it is read where
	// it stands rather than from the table.
	shapeBuilt(wantsDisplayTrees(r), created.attachTrees)
	WriteJSON(w, http.StatusCreated, broadcastCreated{broadcastRead: created, Excluded: excluded})
}

// validateBroadcastInput checks one send's shape and returns its recipients, in the order the
// caller listed them and with the duplicates a multi-select can produce removed: a session
// named twice is one recipient, not two messages.
func validateBroadcastInput(body, delivery string, sessionIDs []string) ([]string, error) {
	if strings.TrimSpace(body) == "" {
		return nil, errorf(http.StatusBadRequest, "INVALID_MESSAGE", "message body is required")
	}
	if err := blockBoundError(itemblocks.Check("body", body)); err != nil {
		return nil, err
	}
	if !validDelivery(delivery) {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT", "delivery must be one of btw, aside, steer")
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	requested := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		trimmed := strings.TrimSpace(sessionID)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		requested = append(requested, trimmed)
	}
	if len(requested) == 0 {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT", "session_ids must name at least one session")
	}
	// contracts.MaxBroadcastRecipients bounds one send, and the dashboard refuses the same number
	// before it asks. A runaway guard rather than a product limit.
	if len(requested) > contracts.MaxBroadcastRecipients {
		return nil, errorf(http.StatusBadRequest, "BROADCAST_INPUT",
			"a broadcast reaches at most %d sessions (%d selected)", contracts.MaxBroadcastRecipients, len(requested))
	}
	return requested, nil
}

// broadcastRecovery is the recovery point a keyed broadcast commits with its broadcast
// (queueRequestKeyRecovery): the broadcast, and the sessions the send left out and why, which a
// repeat answers as the send did.
type broadcastRecovery struct {
	BroadcastID string               `json:"broadcast_id"`
	Excluded    []broadcastExclusion `json:"excluded"`
}

// replayBroadcast answers a repeat of a keyed broadcast whose broadcast committed: the broadcast as
// GET /api/v1/broadcasts/{id} reads it, every recipient's attempts as they stand, with the send's
// own exclusions, under the send's own 201 - answered at once, as the send was, while
// sendOwedBroadcastRecipients puts every recipient it still owes on its way behind the answer.
func (s *server) replayBroadcast(w http.ResponseWriter, r *http.Request, point broadcastRecovery, _ []byte) {
	who, ok := s.actingReader(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	read, err := s.readBroadcast(ctx, who, point.BroadcastID, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := s.sendOwedBroadcastRecipients(ctx, read); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	excluded := point.Excluded
	if excluded == nil {
		excluded = []broadcastExclusion{}
	}
	WriteJSON(w, http.StatusCreated, broadcastCreated{broadcastRead: read, Excluded: excluded})
}

// resolveBroadcastSessions reads each selected session's own record from the session store,
// through the listener's per-session route, and returns the ones it holds a record for; an id
// the store has no record of is simply missing from the map, which is the one absence a send
// is allowed to turn into an exclusion. It answers only once it has decided every requested
// id - found its record, or had the store say it holds none - and any other outcome ends the
// whole resolution with an error, because a read that did not answer, or was never made, says
// nothing about whether the session is working.
//
// It reads the stopped sessions too (SessionIncludingStopped), so a session the listener knows
// and that is not running is a recipient whose copy the listener stores for it, not an exclusion:
// a broadcast to a team of suspended Legion sessions reaches each of them when it runs again. A
// session the listener knows nothing about is still missing from the map, and still excluded.
//
// The reads run broadcastResolveConcurrency at a time under one budget taken from the
// request's context (broadcastResolveAllowance), and those two together are what keep a send's
// resolution short: one request may name contracts.MaxBroadcastRecipients sessions, so a
// hundred reads that each wait out a five-second listener timeout, run one after another,
// would hold the request for the better part of ten minutes. Bounded at eight in flight and
// cut off at five seconds altogether, the resolution costs a send five seconds at worst,
// whatever the listener does.
func (s *server) resolveBroadcastSessions(
	ctx context.Context, requested []string,
) (map[string]dispatchenvoy.Session, error) {
	budget := s.broadcastResolveAllowance()
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var mu sync.Mutex
	live := make(map[string]dispatchenvoy.Session, len(requested))
	decided := 0
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(broadcastResolveConcurrency)
	for _, sessionID := range requested {
		if groupCtx.Err() != nil {
			break
		}
		group.Go(func() error {
			session, err := s.deps.Envoy.SessionIncludingStopped(groupCtx, sessionID)
			if err != nil && !errors.Is(err, dispatchenvoy.ErrNotFound) {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			decided++
			if err == nil {
				live[sessionID] = session
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	// Wait reports only the errors the reads returned, never the budget's: a budget that ends
	// after every launched read has answered, and before the loop launches the next, leaves
	// the rest of the selection unread with no error to show for it. Answering then would let
	// resolveBroadcastTargets exclude each unread session as "no live session", and a send
	// with one recipient left would commit that exclusion under its idempotency key, whose
	// recovery point answers every retry of that key with it - a session dropped because time
	// ran out, recorded as left out on purpose. The count
	// is the invariant the caller depends on, that every selected session was judged against
	// its own record, so it is checked rather than the budget's context: it also refuses an id
	// a future change drops for some other reason, which a context test would let through.
	// The reason names the budget's own context, not groupCtx, which Wait cancels even when
	// every read succeeded.
	if decided != len(requested) {
		return nil, fmt.Errorf(
			"%w: decided %d of %d selected sessions within the %s resolution budget (%v)",
			dispatchenvoy.ErrUnavailable, decided, len(requested), budget, ctx.Err(),
		)
	}
	return live, nil
}

// resolveBroadcastTargets judges each selected session against the record the session store
// holds for it: it is a recipient when the store holds its session, that session has not gone,
// and it advertises the chosen mode, and otherwise carries the reason it was left out.
//
// An unresponsive session is a recipient like any other: the listener stores what is sent to
// it and hands it over when it answers again, which is exactly what a broadcast to a session
// that missed its heartbeats should do. So is a session the store holds as stopped, by its
// record alone (advertisesDelivery judges its mode): the listener stores its copy for it and
// hands it over when it runs again. Only a record the registry itself calls gone is no session
// to send to, as a missing record is.
func resolveBroadcastTargets(
	requested []string, delivery string, live map[string]dispatchenvoy.Session,
) []broadcastTarget {
	targets := make([]broadcastTarget, 0, len(requested))
	for _, sessionID := range requested {
		session, ok := live[sessionID]
		switch {
		case !ok, session.State == dispatchenvoy.StateGone:
			targets = append(targets, broadcastTarget{sessionID: sessionID, reason: "no live session"})
		case !advertisesDelivery(session, delivery):
			targets = append(targets, broadcastTarget{
				sessionID: sessionID, title: session.Title, reason: "does not advertise " + delivery,
			})
		default:
			targets = append(targets, broadcastTarget{sessionID: sessionID, title: session.Title})
		}
	}
	return targets
}

// excludedSummary names every excluded session and its reason, for the error a send with no
// reachable recipient answers with.
func excludedSummary(excluded []broadcastExclusion) string {
	reasons := make([]string, 0, len(excluded))
	for _, item := range excluded {
		name := item.Title
		if name == "" {
			name = item.SessionID
		}
		reasons = append(reasons, name+" "+item.Reason)
	}
	return strings.Join(reasons, "; ")
}

// writeBroadcast commits the broadcast, its keyed request's recovery point, and one targeted
// message per recipient in a single transaction, so a send is never half-written, and returns the
// events its messages owe the stream. Nothing is delivered here: a listener call never runs with a
// transaction open (envoy_resolve.go). excluded is what the send left out, which the recovery point
// keeps for a repeat's answer.
//
// The transaction runs in two phases (INFRA-636). The read phase holds no transaction id: it draws
// the broadcast's id and its created_at, the transaction's now() its column defaults to, and plans
// every recipient's page (planBroadcastPages) and every copy (planBroadcastCopy). The write phase
// is one batch: the broadcast's row, its recovery point, the pages, and then each copy in turn.
func (s *server) writeBroadcast(
	ctx context.Context,
	actor model.Actor,
	body, delivery string,
	recipients []broadcastTarget,
	excluded []broadcastExclusion,
) (broadcastRead, []model.Event, error) {
	author, err := json.Marshal(actor)
	if err != nil {
		return broadcastRead{}, nil, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return broadcastRead{}, nil, err
	}
	defer tx.Rollback(ctx)
	sent := broadcastRead{broadcast: broadcast{Author: actor, Body: body, Delivery: delivery}}
	if err := tx.QueryRow(ctx, `select gen_random_uuid()::text, now()`).Scan(&sent.ID, &sent.CreatedAt); err != nil {
		return broadcastRead{}, nil, err
	}
	// Every copy hangs from its recipient's page (graphstore.MessageFacts), and a node write locks
	// a page the store holds already and waits on one another transaction is making: two
	// broadcasts to [A, B] and [B, A] that each took their recipients' pages copy by copy would
	// each hold one page the other waits for, and Postgres would end one of them. So every
	// recipient's page is resolved here and written in ascending id, before any copy's node; each
	// copy then hangs from its page without writing it again (planMessageNode). A page that write
	// makes is caused by the event of the first copy that hangs from it, as it was when that copy's
	// own write made it.
	pages, planned, err := planBroadcastPages(ctx, tx, recipients, actor, sent.CreatedAt)
	if err != nil {
		return broadcastRead{}, nil, err
	}
	copies := make([]broadcastCopy, len(recipients))
	for position, recipient := range recipients {
		if copies[position], err = s.planBroadcastCopy(ctx, tx, sent, recipient, pages); err != nil {
			return broadcastRead{}, nil, err
		}
	}
	events := make([]model.Event, len(copies))
	attempts := make([]model.MessageDelivery, len(copies))
	if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
		b.Queue("", `
			insert into broadcasts (id, author, body, delivery, created_at)
			values ($1::uuid, $2, $3, $4, $5)
		`, sent.ID, author, body, delivery, sent.CreatedAt)
		// The recovery point right after its broadcast: a repeat that took the key over meanwhile
		// refuses it here, and this transaction rolls back before it writes a message.
		if err := queueRequestKeyRecovery(ctx, b, broadcastRecovery{BroadcastID: sent.ID, Excluded: excluded}); err != nil {
			return err
		}
		planned.queue(b)
		for position := range copies {
			if err := s.queueBroadcastCopy(b, copies[position], position, sent, author, planned,
				&events[position], &attempts[position]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return broadcastRead{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return broadcastRead{}, nil, err
	}
	sent.Recipients = make([]broadcastRecipient, 0, len(copies))
	for position, copied := range copies {
		message := copied.message
		message.Deliveries = []model.MessageDelivery{attempts[position]}
		sent.Recipients = append(sent.Recipients, broadcastRecipient{
			SessionID: copied.sessionID, Message: newShapedMessage(message, false), Replies: []shapedMessage{},
		})
	}
	// One text, however many recipients: every copy holds the body the broadcast was sent with,
	// and all of them share the one row keying on the text alone gives them.
	s.storeDisplayTrees(ctx, sent.ID, body)
	return sent, events, nil
}

// broadcastCopy is one recipient's copy of a broadcast as the read phase planned it, for the write
// phase to queue (queueBroadcastCopy): the message as its insert will store it, its target's page,
// its node and facts (planMessageNode), its block index and what that states (planPendingItem),
// its record in the thread it opens (inboxwrite.PlanRecord), the logins its event names, and its
// first delivery attempt's node (graphstore.AttemptOpened).
type broadcastCopy struct {
	sessionID, page string
	message         model.Message
	stored          messageStoreWrite
	indexed         graphstore.Uncaused
	index           docs.WriteStep
	recording       inboxwrite.Recording
	logins          []string
	attemptKey      graphstore.AttemptKey
	attemptKind     *string
	attemptAddress  *string
	attemptOpened   graphstore.Uncaused
}

// planBroadcastCopy plans recipient's copy of the broadcast sent, by its author with its body,
// written at the broadcast's own time. Each copy's node goes in before its row, between the sender
// and the recipient, the parties of the thread the copy opens, on the recipient's page and with
// its target and its broadcast stated as facts that wait for its event; the row names the node in
// its insert. Each copy is a direct message: its blocks are indexed, and it writes no mention, so
// its blocks hold none either and the write moves nothing. Its insert is queued, so its body is
// planned before it, as every other message write plans it (planPendingItem).
func (s *server) planBroadcastCopy(
	ctx context.Context, tx pgx.Tx, sent broadcastRead, recipient broadcastTarget, pages map[string]string,
) (broadcastCopy, error) {
	target := "session:" + recipient.sessionID
	messageID := uuid.NewString()
	planned := broadcastCopy{
		sessionID: recipient.sessionID, page: pages[target],
		message: model.Message{
			ID: messageID, Author: sent.Author, Body: sent.Body, Target: &target, BroadcastID: &sent.ID,
			CreatedAt: sent.CreatedAt, Deliveries: []model.MessageDelivery{},
		},
	}
	var err error
	if planned.stored, err = planMessageNode(ctx, tx, graphstore.MessageRow{ID: messageID, Target: target, BroadcastID: sent.ID}, sent.Author, sent.CreatedAt, pages); err != nil {
		return broadcastCopy{}, err
	}
	if _, planned.index, planned.indexed, err = s.planPendingItem(ctx, tx, sent.Author, refs.ItemMessage, messageID, sent.Body, refs.PendingSource{},
		refs.WrittenItem(planned.stored.node), sent.CreatedAt, false); err != nil {
		return broadcastCopy{}, err
	}
	// The sender takes part in the thread the copy opens (inboxwrite.PlanRecord): a copy is on no
	// issue, so its thread keeps no user_threads row and names no one more. A copy answers
	// nothing, so the store keys every pointer of its row (graphstore.MessageFacts' projected) and
	// its record always states its tuples.
	if planned.recording, err = inboxwrite.PlanRecord(ctx, tx, planned.stored.inbox(planned.message, sent.Author)); err != nil {
		return broadcastCopy{}, err
	}
	// thread_logins is the copy's author - the sender, the one person whose Inbox an Agent row of
	// this conversation can sit in once the session replies (api/thread_logins.go,
	// directThreadLogins) - as every other direct message's names its root author: a reader that
	// refetches an Inbox only for the logins an event names would otherwise refetch every viewer's
	// on every broadcast. A copy answers nothing, so this reads nothing.
	if planned.logins, err = directThreadLogins(ctx, tx, planned.message); err != nil {
		return broadcastCopy{}, err
	}
	planned.attemptKey = graphstore.MessageAttempt(messageID, 1)
	planned.attemptKind, planned.attemptAddress, planned.attemptOpened = graphstore.AttemptOpened(planned.attemptKey, sent.Author, sent.CreatedAt)
	return planned, nil
}

// queueBroadcastCopy queues copied, the copy at position of the broadcast sent, on the broadcast's
// write phase: its node, its row, its block index, its message.created, and under that event what
// its node write stated, the page it is the first copy to hang from, what its index stated, who
// wrote it and its record in its thread, then its first delivery attempt. author is the sender
// as the rows store them, encoded once for every copy. event and attempt receive the appended
// event and the attempt row once the batch has run.
//
// The recipient's first delivery attempt is opened here, unclaimed, rather than by the worker that
// will send it. A recipient then always has an attempt to show - "Sending" while a worker holds
// it, and, if the process dies before the send, a lapsed pending row the delivery sweep
// (delivery_sweep.go) and the ordinary same-mode retry both resume under its own idempotency key.
// Opening it later left a stranded recipient with no attempt at all, which only a delivery in a
// DIFFERENT mode could move, and that is a genuine second frame where the first landed.
// claimed_at stays null, so recordPendingMessageDelivery reads the row as free and resumes it
// instead of opening a second attempt beside it. The author asked for it, and the resume keeps
// that. The attempt's node goes in before its row, and it and what the row holds are caused by the
// copy's message.created, as the copy's own node is.
func (s *server) queueBroadcastCopy(
	b *store.WriteBatch, copied broadcastCopy, position int, sent broadcastRead, author []byte,
	pages broadcastPages, event *model.Event, attempt *model.MessageDelivery,
) error {
	graphstore.QueueNodes(b, copied.stored.written)
	nodeKind, nodeAddress := copied.stored.rowNode()
	// kept for one release: user-threads-triggers
	b.Queue("", `
		insert into messages (id, issue_key, author, body, target, broadcast_id, broadcast_position, created_at, node_kind, node_address, threads_recorded)
		values ($1, null, $2, $3, $4, $5, $6, $7, $8, $9, true)
	`, copied.message.ID, author, copied.message.Body, *copied.message.Target, sent.ID, position, sent.CreatedAt, nodeKind, nodeAddress)
	if err := copied.index(b); err != nil {
		return err
	}
	// A broadcast message is a thread root that answers nothing, so its event says what every
	// other root's does; the broadcast it belongs to is read from the message row.
	payload := messageReplyThread{}.payload(copied.message, model.ReferenceChanges{})
	payload.ThreadLogins = copied.logins
	if err := s.deps.Events.QueueAppend(b, messageEvent(copied.message, "message.created", sent.Author, payload), func(appended model.Event) {
		*event = appended
	}); err != nil {
		return err
	}
	graphstore.QueueCaused(b, copied.stored.written, graphstore.LatestEvent)
	pages.queueCause(b, copied.page)
	// What the copy's body is in the store, under the same event (planPendingItem).
	graphstore.QueueCaused(b, copied.indexed, graphstore.LatestEvent)
	if err := graphstore.QueueAuthored(b, graphstore.KindMessage, copied.message.ID, sent.Author, sent.CreatedAt, graphstore.LatestEvent); err != nil {
		return err
	}
	if err := copied.recording.Queue(b, graphstore.LatestEvent); err != nil {
		return err
	}
	graphstore.QueueNodes(b, copied.attemptOpened)
	b.Queue("", `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, requested_by, node_kind, node_address)
		values ($1, 1, $2, $3, 'pending', $4, $5, $6)
		returning `+messageDeliveryColumns, copied.message.ID, sent.Delivery, copied.sessionID, author, copied.attemptKind, copied.attemptAddress,
	).QueryRow(func(row pgx.Row) error {
		written, err := scanMessageDelivery(row)
		*attempt = written
		return err
	})
	graphstore.QueueCaused(b, copied.attemptOpened, graphstore.LatestEvent)
	graphstore.QueueAttempted(b, sent.Author, graphstore.LatestEvent, copied.attemptKey)
	return nil
}

// broadcastPages is the pages of a broadcast's recipients as its read phase planned them: each
// page's own node write (graphstore.PlanUncaused), made by the sender at the broadcast's own time,
// queued in ascending id before any copy's node (queue), and caused by the event of the first copy
// that hangs from it (queueCause).
type broadcastPages struct {
	order   []string
	written map[string]graphstore.Uncaused
}

// planBroadcastPages is the page each recipient's copy hangs from, by the copy's target: the page
// the store gives the agent its session is, read for every recipient in one statement
// (graphstore.AgentPages), or "" where it gives none; and beside it the write of each such page
// once, as a copy's own write would make it (graphstore.PageNode). Two sessions that claimed one
// role hang from that role's page, which is one node, written once.
func planBroadcastPages(ctx context.Context, tx pgx.Tx, recipients []broadcastTarget, actor model.Actor, at time.Time) (map[string]string, broadcastPages, error) {
	targets := make([]string, 0, len(recipients))
	named := make(map[string]struct{}, len(recipients))
	for _, recipient := range recipients {
		target := "session:" + recipient.sessionID
		if _, held := named[target]; !held {
			named[target] = struct{}{}
			targets = append(targets, target)
		}
	}
	resolved, err := graphstore.AgentPages(ctx, tx, targets)
	if err != nil {
		return nil, broadcastPages{}, err
	}
	byTarget := make(map[string]string, len(targets))
	pages := broadcastPages{written: make(map[string]graphstore.Uncaused, len(targets))}
	for _, target := range targets {
		page := resolved[target]
		byTarget[target] = page
		if page == "" {
			continue
		}
		if _, held := pages.written[page]; !held {
			pages.order = append(pages.order, page)
			pages.written[page] = graphstore.PlanUncaused([]graphstore.Node{graphstore.PageNode(page, actor, at, graphstore.NoEvent)}, nil)
		}
	}
	slices.Sort(pages.order)
	return byTarget, pages, nil
}

// queue queues the write of every page's node, in ascending id.
func (p broadcastPages) queue(b *store.WriteBatch) {
	for _, page := range p.order {
		graphstore.QueueNodes(b, p.written[page])
	}
}

// queueCause queues the cause of page's write under the event the batch appended last, the
// event of the first copy that hangs from it, and nothing for a page an earlier copy caused
// already or for no page.
func (p broadcastPages) queueCause(b *store.WriteBatch, page string) {
	held, owed := p.written[page]
	if !owed {
		return
	}
	delete(p.written, page)
	graphstore.QueueCaused(b, held, graphstore.LatestEvent)
}

// deliverBroadcast sends each recipient's owed attempt, broadcastDeliveryWorkers at a time, and
// returns once every one of them has settled. It runs on ctx, which its callers detach from the
// request (context.WithoutCancel): a browser that goes away, and a deploy's signal, must not cut
// a recipient off mid-flight. One recipient's failure never stops another's: a send the listener
// refused settles that recipient's attempt as failed, a recipient whose attempt was taken over
// from the agent card is skipped, and a delivery this cannot record at all - a shutdown, a killed
// process, a database that refuses the write - leaves the attempt pending and unclaimed, which
// the broadcast view shows as sending and which the delivery sweep (delivery_sweep.go,
// resumeOwedDeliveries), or a repeat under the send's key (sendOwedBroadcastRecipients), resumes
// once the claim lease has passed.
func (s *server) deliverBroadcast(ctx context.Context, sends []broadcastSend, delivery string, actor model.Actor) {
	// A hold mark of this delivery's own (store.ForConcurrentWorker), as hardening: nothing sets
	// the request's mark during a delivery today. ctx is the request's, detached from its
	// cancellation but not from its mark, and the request goroutine lives on past the hand-off -
	// it writes its JSON, records the answer and releases the key (settleRequestKey,
	// releaseRequestKey) - but those run through store.Pool.Exec, which only checks the mark and
	// never sets it; only Pool.Query, whose cursor holds its connection, and Pool.Begin do. Were
	// that bookkeeping to move into a transaction, or a handler to read through Pool.Query after
	// the hand-off, the shared mark would refuse a worker's ForConcurrentRead below
	// (store.ErrNestedAcquire) and leave that recipient's send to the delivery sweep. Both callers
	// hand the delivery over with no connection of their own held - the create's transaction
	// committed before it detached, and a repeat's reads are closed before it resumes anyone -
	// which is what a mark of its own needs.
	ctx = store.ForConcurrentWorker(ctx)
	workers := min(broadcastDeliveryWorkers, len(sends))
	pending := make(chan broadcastSend)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for send := range pending {
				// A mark of this worker's own: workers sharing one would refuse each other a
				// connection (store.ErrNestedAcquire), and nothing but this delivery's own
				// workers holds the mark it runs on.
				sending, err := store.ForConcurrentRead(ctx)
				if err != nil {
					slog.Error("dispatch: broadcast delivery",
						"message", send.message.ID, "target", messageTarget(send.message.Target), "error", err)
					continue
				}
				// Bound to the attempt this send owes. A human who changes the mode from the
				// agent card while this worker is queued settles that attempt and sends their
				// own; the worker must then send nothing, because an attempt of its own would
				// put a second frame on the session's subject. Of two senders that both found
				// the attempt owed, the one that claims it sends and the other stops here.
				_, err = s.deliverMessageResuming(sending, send.message, delivery, actor, nil, send.attempt)
				if errors.Is(err, errAttemptNotPending) {
					continue
				}
				if err != nil {
					slog.Error("dispatch: broadcast delivery",
						"message", send.message.ID, "target", messageTarget(send.message.Target), "error", err)
				}
			}
		}()
	}
	for _, send := range sends {
		pending <- send
	}
	close(pending)
	wait.Wait()
}

// sendOwedBroadcastRecipients puts every recipient the broadcast still owes on its way
// (owedMessageAttempts), as the original send did, behind the repeat's answer: the repeat is
// answered as soon as it has read the broadcast, as the original was answered as soon as it
// committed. The process that answered the original send may have been stopped, or have died,
// with recipients it had not reached; the delivery sweep resumes those too (resumeOwedDeliveries),
// and a repeat only puts them on their way sooner. Only the read of what is owed can fail here:
// a listener the delivery cannot reach is recorded on the attempt.
func (s *server) sendOwedBroadcastRecipients(ctx context.Context, read broadcastRead) error {
	messageIDs := make([]string, 0, len(read.Recipients))
	for _, recipient := range read.Recipients {
		messageIDs = append(messageIDs, recipient.Message.ID)
	}
	owed, err := s.owedMessageAttempts(ctx, messageIDs)
	if err != nil || len(owed) == 0 {
		return err
	}
	sends := make([]broadcastSend, 0, len(owed))
	for _, recipient := range read.Recipients {
		if attempt, ok := owed[recipient.Message.ID]; ok {
			sends = append(sends, broadcastSend{message: recipient.Message.Message, attempt: attempt})
		}
	}
	// Detached from the request and counted in the server's background wait, as the original
	// send's delivery is; the listener client's receipt timeout bounds each recipient's send.
	sending := context.WithoutCancel(ctx)
	s.detached.goDetached(func() { s.deliverBroadcast(sending, sends, read.Delivery, read.Author) })
	return nil
}

// GET /api/v1/broadcasts
func (s *server) listBroadcasts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select b.id::text, b.author, b.body, b.delivery, b.created_at,
		       count(distinct m.id),
		       -- Recipients who answered, not answered attempts: one recipient that answered a
		       -- retry as well as the original attempt is one answer, never two.
		       count(distinct m.id) filter (where d.reply_id is not null)
		from broadcasts b
		left join messages m on m.broadcast_id = b.id
		left join message_deliveries d on d.message_id = m.id and d.reply_id is not null
		group by b.id
		order by b.created_at desc, b.id desc
		limit 50
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	reader := s.readDisplayTrees(r.Context(), wantsDisplayTrees(r))
	summaries := []broadcastSummary{}
	for rows.Next() {
		var summary broadcastSummary
		var author []byte
		if err := rows.Scan(
			&summary.ID, &author, &summary.Body, &summary.Delivery, &summary.CreatedAt,
			&summary.Recipients, &summary.Replies,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := json.Unmarshal(author, &summary.Author); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The bodies are registered once the list is whole: a row of a slice still being appended to
	// is a row the reader must not hold the address of.
	for index := range summaries {
		summaries[index].attachTrees(reader)
	}
	reader.resolve()
	WriteJSON(w, http.StatusOK, summaries)
}

// GET /api/v1/broadcasts/{id}
//
// A broadcast's route resolves no id prefix, but the prefix layer canonicalizes its {id} all the
// same (canonicalizeUUIDPath), so this handler parses the one spelling of it: Postgres refuses
// `urn:uuid:<uuid>` on the uuid column, and the raw value would answer 500 where the canonical
// one answers this route's own 404.
func (s *server) getBroadcast(w http.ResponseWriter, r *http.Request) {
	who, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, "BROADCAST_NOT_FOUND", http.StatusNotFound, "broadcast not found")
		return
	}
	read, err := s.readBroadcast(r.Context(), who, id, wantsDisplayTrees(r))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, "BROADCAST_NOT_FOUND", http.StatusNotFound, "broadcast not found")
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// readBroadcast reads one broadcast with every recipient's state, as GET /api/v1/broadcasts/{id}
// answers it to who; an unknown id is pgx.ErrNoRows.
func (s *server) readBroadcast(ctx context.Context, who model.Actor, id string, trees bool) (broadcastRead, error) {
	var read broadcastRead
	var author []byte
	if err := s.deps.Store.Pool.QueryRow(ctx, `
		select id::text, author, body, delivery, created_at from broadcasts where id = $1
	`, id).Scan(&read.ID, &author, &read.Body, &read.Delivery, &read.CreatedAt); err != nil {
		return broadcastRead{}, err
	}
	if err := json.Unmarshal(author, &read.Author); err != nil {
		return broadcastRead{}, err
	}
	// The body, every recipient's message and every reply under them are one read of the stored
	// trees, and what the table did not hold is stored on a goroutine of its own, beside the
	// rest of the request.
	reader := s.readDisplayTrees(ctx, trees)
	read.attachTrees(reader)
	recipients, err := s.loadBroadcastRecipients(ctx, who, read.ID, reader)
	if err != nil {
		return broadcastRead{}, err
	}
	read.Recipients = recipients
	reader.resolve()
	return read, nil
}

// loadBroadcastRecipients reads every message the broadcast sent in the sender's requested
// order, each reply's block read for who. Messages created before positions existed, or by an
// old server during a rolling deploy, have no position; their request order was never stored, so
// they retain the created_at and id fallback order. created_at cannot order one current broadcast
// because all of its rows share the one transaction's now().
func (s *server) loadBroadcastRecipients(ctx context.Context, who model.Actor, broadcastID string, reader *displayTreeReader) ([]broadcastRecipient, error) {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select `+messageColumns+`
		from messages
		where broadcast_id = $1
		order by broadcast_position nulls last, created_at, id
	`, broadcastID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []model.Message{}
	for rows.Next() {
		message, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	ids := make([]string, len(messages))
	for index, message := range messages {
		ids[index] = message.ID
	}
	deliveries, err := s.loadMessageDeliveries(ctx, s.deps.Store.Pool, ids)
	if err != nil {
		return nil, err
	}
	replies, err := s.loadMessageReplyChains(ctx, s.deps.Store.Pool, ids)
	if err != nil {
		return nil, err
	}
	if err := hydrateThreadReplyBlocks(ctx, s.deps.Store.Pool, who, messages, replies); err != nil {
		return nil, err
	}
	recipients := make([]broadcastRecipient, len(messages))
	for index, message := range messages {
		message.Deliveries = deliveries[message.ID]
		recipients[index].SessionID = strings.TrimPrefix(messageTarget(message.Target), "session:")
		recipients[index].Message.Message = message
		recipients[index].Message.shape(reader)
		recipients[index].Replies = shapedMessages(replies[message.ID], reader)
	}
	return recipients, nil
}
