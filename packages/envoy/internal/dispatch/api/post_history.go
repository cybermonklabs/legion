// post_history.go is what a write of a comment's or an issue message's body does to the body it
// replaces: the one write that keeps it (postBodyHistoryStep), the clear both deletes make of it
// and of the sends still carrying it (clearDeletedPostBody, queueDeletedPostSettlement), and the
// reads that serve the kept bodies, GET /api/v1/comments/{id}/history and GET
// /api/v1/messages/{id}/history, and their /api/v2 successors GET /api/v2/comments/{id}/corrections
// and GET /api/v2/messages/{id}/corrections (postHistory, one statement for all four). One table
// holds the kept bodies of both kinds, keyed by the one of comment_id and message_id its row
// carries (post_body_history, migration 20261110224508).
//
// Every write here is planned in the route's read phase and returned as a step of its write
// phase (docs.WriteStep), which the route queues where it makes the write: on its ledger for a
// comment, whose route joins a document ledger (Docs.Join), and on the one batch of a message's,
// which runs its write phase itself as the message create does (createStoredMessage).
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// postHistoryEntry is one body a write replaced, with the actor that replaced it and the time
// of that write. Body is empty on a deleted post, whose read gives no part of the body
// anywhere, this one included. id is the row's own identity, which /api/v1 never answers and
// /api/v2 answers beside edited_at as the position of a post's corrections (postCorrectionV2):
// edited_at is clock_timestamp(), unique per post in no constraint, and the id breaks a tie.
type postHistoryEntry struct {
	Body     string      `json:"body"`
	EditedBy model.Actor `json:"edited_by"`
	EditedAt time.Time   `json:"edited_at"`
	id       int64
}

// postHistoryRead is what the history reads answer: the post's earlier bodies, newest first, and
// the deleted mark the post carries, as every other read of it carries it. On /api/v1 Entries
// holds at most contracts.PostHistoryPageLimit rows; the page after is the same read under
// ?before= the oldest edited_at this one gave.
type postHistoryRead struct {
	DeletedAt *time.Time         `json:"deleted_at"`
	DeletedBy *model.Actor       `json:"deleted_by"`
	Entries   []postHistoryEntry `json:"entries"`
}

// postHistoryOwner is where a post of this kind is kept: the table the post itself is in and
// the post_body_history column naming it, in the order refs gives them, which owns the map of
// an item kind to its storage (refs.ItemKind.ItemTable): post_body_history keys a post by the
// same column every table of that post's blocks keys it by. Only a comment and a message keep a
// history, so an ask, which is an item kind of its own, is refused here rather than there. kind
// is the handler's own constant and never caller input, so a kind with no history is this
// server's own fault rather than a refusal a caller can provoke.
func postHistoryOwner(kind refs.ItemKind) (items, column string, err error) {
	if kind != refs.ItemComment && kind != refs.ItemMessage {
		return "", "", fmt.Errorf("read the body history of a %q: only a comment and a message keep one", string(kind))
	}
	return kind.ItemTable()
}

// postBodyHistoryStep is the write that keeps the body a write of comment or message itemID is
// about to replace, under the owner lock the caller already holds: one post_body_history row
// with the actor making the write, as a step of the caller's write phase. An edit makes it with
// the body it replaces and a delete with the body it clears; a create makes it never, so the
// history of a post nobody edited or deleted is empty. The row's edited_at is its column's
// clock_timestamp(), which the statement takes when the write phase runs it.
func postBodyHistoryStep(kind refs.ItemKind, itemID, body string, actor model.Actor) (docs.WriteStep, error) {
	_, column, err := postHistoryOwner(kind)
	if err != nil {
		return nil, err
	}
	editedBy, err := encodeJSON(actor)
	if err != nil {
		return nil, err
	}
	return func(b *store.WriteBatch) error {
		b.Queue(fmt.Sprintf("keep the replaced body of %s %s", kind, itemID), `
			insert into post_body_history (`+column+`, body, edited_by) values ($1, $2, $3)
		`, itemID, body, editedBy)
		return nil
	}, nil
}

// queuePostBodyHistory queues postBodyHistoryStep on ledger, for a route whose write phase is its
// ledger's (editCommentAs).
func queuePostBodyHistory(ledger *docs.Ledger, kind refs.ItemKind, itemID, body string, actor model.Actor) error {
	step, err := postBodyHistoryStep(kind, itemID, body, actor)
	if err != nil {
		return err
	}
	return ledger.Queue(step)
}

// deletedPostDeliveryError is what a send of a deleted post still pending is settled with
// (contracts.DeletedPostDeliveryError, one literal with the dashboard's). It says that the post's
// delete ended the attempt and nothing about whether a frame reached the session: a sender whose
// claim the delete settled after its frame left (queueDeletedPostSettlement) never learns which,
// and one that learns it before its send sends nothing (deliverResolvedCommentMention,
// deliverMessageResuming).
const deletedPostDeliveryError = contracts.DeletedPostDeliveryError

// deleteContendedRetryAfter is the Retry-After, in whole seconds, that a delete's 409
// DELETE_CONTENDED carries: a delete whose settle met a send of its post that the send's own
// sender moved after the delete's read found it (queueDeletedPostSettlement). Nothing is written,
// and the same delete sent again reads the attempts as they stand then. What the caller waits for
// is a sender's write already committing - the statement that moved the attempt, which held its
// row while the delete waited on it - and no budget this caller has spent, so the wait is the
// shortest whole second HTTP can ask for, as the results write's checkContendedRetryAfter is for
// the same reason.
const deleteContendedRetryAfter = 1

// errDeleteContended is that refusal: 409 DELETE_CONTENDED with Retry-After: 1. Both deletes
// answer it, in either version, through the settle they both queue.
var errDeleteContended error = &apiError{
	code: apicode.DeleteContended, retryAfter: deleteContendedRetryAfter,
	message: "a send of this post moved while the delete was being written; nothing was written, and the same delete can be sent again",
}

// clearDeletedPostBody plans the clear a delete of either kind makes, under the owner lock the
// caller already holds, and returns it as one step of the caller's write phase with the
// reference changes the delete's event carries. The step writes, in order: the history row
// keeping the body about to go (postBodyHistoryStep); the one update that empties the body and
// stamps the delete on the row at deletedAt, the transaction's now() the caller read in its read
// phase; the index of the empty body that update leaves, planned here (planEditedItem), which
// takes every block out of the post's current revision and every reference out of its edges; and
// the copies of the body that do not follow the cleared column - each block's stored excerpt,
// the text each revision recorded of a block and the post's embedding windows - taken out of
// them (refs.TombstoneItem).
//
// A comment clears its suggestion with its body, since the suggestion's replacement text is the
// comment's text too, and a message carries no suggestion: the kind decides which. quoteIsBody
// says that a comment's anchor quote is its own text as well, the text an insert suggestion put
// in the document, and the update then empties that quote too, leaving every other key of the
// anchor as it is; a message carries no anchor. The clear rides the one update, so the kind's
// search trigger rewrites the row's vector once from the cleared body and the deleted post stops
// matching a keyword search. Each form of that update is one literal statement, so every column
// it sets is read where it is written (api's TestNoProjectionRowIsWrittenWithoutTheStore): none
// is a column a tuple of the store projects, since the anchor's quote key keeps the artifact its
// anchored_to tuple names, so the clear writes no node and no tuple. (A delete that rejects a
// pending insert suggestion also settles the document's version in its transaction, which writes
// the document's block nodes and tuples: comment_delete.go.) The post's embedding
// windows go in this transaction too (refs.TombstoneItem): the cleared body splits into no block,
// so the write enqueues no window and deletes each window the post held, and no meaning search
// finds the post once the delete commits. No path of this build leaves a window of a deleted post
// behind; the mark is the backstop for a hit that reaches one all the same, which search stamps
// with the post's delete mark (stampSearchPostMarks, search.go).
//
// ctx is what the plan's reads run under: the context a comment's route derives from the ledger
// it queues on (docs.Ledger.ReadContext), as queueEditedItemReferences derives it, and the
// request's for a message, whose route queues on no ledger. Only an issue message is deleted, so
// both kinds cite what they mention and the index reconciles the post's own references. author
// is the author the post's row records, whom the index resolves the post's short ids for
// (planEditedItem), and actor the caller the delete is recorded under.
func (s *server) clearDeletedPostBody(
	ctx context.Context,
	tx pgx.Tx,
	kind refs.ItemKind,
	itemID, previousBody string,
	quoteIsBody bool,
	author, actor model.Actor,
	deletedAt time.Time,
) (model.ReferenceChanges, docs.WriteStep, error) {
	statement := `update messages set body = '', deleted_at = $3, deleted_by = $2 where id = $1`
	if kind == refs.ItemComment {
		statement = `update comments set body = '', suggestion = null, deleted_at = $3, deleted_by = $2 where id = $1`
		if quoteIsBody {
			statement = `update comments set body = '', suggestion = null, anchor = jsonb_set(anchor, '{quote}', to_jsonb(''::text)),
				deleted_at = $3, deleted_by = $2 where id = $1`
		}
	}
	// The history step refuses a kind that is neither a comment nor a message, so no other
	// kind's table is ever cleared.
	history, err := postBodyHistoryStep(kind, itemID, previousBody, actor)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	deletedBy, err := encodeJSON(actor)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	// The update that clears the body is queued, so the index is planned from the empty body the
	// write leaves rather than read back, as an edit's is (planEditedItem).
	changes, index, err := s.planEditedItem(ctx, tx, author, kind, itemID, previousBody, "", deletedAt, nil)
	if err != nil {
		return model.ReferenceChanges{}, nil, err
	}
	return changes, func(b *store.WriteBatch) error {
		if err := history(b); err != nil {
			return err
		}
		b.Queue("", statement, itemID, deletedBy, deletedAt)
		if err := index(b); err != nil {
			return err
		}
		return refs.TombstoneItem(b, kind, itemID)
	}, nil
}

// pendingAttemptsRead is what a delete's read found of its post's send attempts still pending,
// for the settle to hold each row it settles to (queueDeletedPostSettlement). Each attempt is
// named by its key and carries every column the delete's receipts and answer state that a writer
// holding no owner lock moves on a pending row. That writer is the attempt's own sender, under
// the claim it holds: a comment's records, before its send, the resolution it sends with
// (recordCommentDeliveryResolution: delivery, session_id, resolve_error), and a message's renews
// its pin before another try (renewMessageDeliveryClaim: session_id).
//
// table is the post's attempts table and owner the column naming the post in it. matches is the
// SQL that is true of an attempt row d the read found as it stands now, over the arrays args
// holds from $3, one element per attempt read; pending is how many were read.
type pendingAttemptsRead struct {
	table, owner string
	matches      string
	args         []any
	pending      int
}

// commentAttemptsRead is pendingAttemptsRead of the pending attempts of a comment, as its
// delete's read found them. session_id and resolve_error are null on an attempt nothing has
// resolved yet, so a row is matched to them null-safely.
func commentAttemptsRead(pending []model.CommentDelivery) pendingAttemptsRead {
	targets := make([]string, len(pending))
	attempts := make([]int32, len(pending))
	modes := make([]string, len(pending))
	sessions := make([]*string, len(pending))
	resolveErrors := make([]*string, len(pending))
	for i, attempt := range pending {
		targets[i], attempts[i], modes[i] = attempt.Target, int32(attempt.Attempt), attempt.Delivery
		sessions[i], resolveErrors[i] = attempt.SessionID, attempt.ResolveError
	}
	return pendingAttemptsRead{
		table: "comment_deliveries", owner: "comment_id",
		matches: `exists (
			select from unnest($3::text[], $4::int[], $5::text[], $6::text[], $7::text[])
				as seen(target, attempt, delivery, session_id, resolve_error)
			where (seen.target, seen.attempt, seen.delivery, seen.session_id, seen.resolve_error)
				is not distinct from (d.target, d.attempt, d.delivery, d.session_id, d.resolve_error)
		)`,
		args:    []any{targets, attempts, modes, sessions, resolveErrors},
		pending: len(pending),
	}
}

// messageAttemptsRead is pendingAttemptsRead of the pending attempts of a message, as its
// delete's read found them.
func messageAttemptsRead(pending []model.MessageDelivery) pendingAttemptsRead {
	attempts := make([]int32, len(pending))
	modes := make([]string, len(pending))
	sessions := make([]string, len(pending))
	for i, attempt := range pending {
		attempts[i], modes[i], sessions[i] = int32(attempt.Attempt), attempt.Delivery, attempt.SessionID
	}
	return pendingAttemptsRead{
		table: "message_deliveries", owner: "message_id",
		matches: `exists (
			select from unnest($3::int[], $4::text[], $5::text[]) as seen(attempt, delivery, session_id)
			where (seen.attempt, seen.delivery, seen.session_id)
				is not distinct from (d.attempt, d.delivery, d.session_id)
		)`,
		args:    []any{attempts, modes, sessions},
		pending: len(pending),
	}
}

// queueDeletedPostSettlement queues the settle of every send of the deleted post still pending:
// failed with deletedPostDeliveryError. A delivery Dispatch already sent keeps what it carried;
// one still waiting would send the text after the delete, so it is settled here rather than left
// to the delivery sweep. claimed_at moves with the settle, as every other settle of an attempt
// moves it. That move tells a sender still holding the old claim that the row is no longer its own
// only in a statement that matches the claim exactly (claimed_at = the claim it holds:
// recordCommentDeliveryResolution, holdCommentDeliveryClaim, the first statement of
// settleDeliveryAttempt, holdMessageDeliveryClaim). A statement that matches it by
// model.DeliveryClaimSinceSQL counts this settle's claimed_at = now() as that sender's own claim
// whenever it lands less than a lease after the claim held, so each such statement leaves the
// settled row alone only because it also requires the attempt pending: state = 'pending' in
// standDown, supersedeCommentDelivery and rereadMentionClaims, and the state it reads in
// claimCommentDeliveryAttempt's read of its bound attempt.
//
// The caller read the post's attempts in its read phase, under the post's owner lock, and builds
// the receipt each settle owes from the pending ones (deliveryOutcome), queued after this
// statement, since no statement's result is read in Go before the next is sent; read is what
// that read found of them. Every settle of an attempt takes that owner's lock first
// (settleDeliveryAttempt, settleUndeliverableMention), so the statement finds as many pending as
// the read did, but an attempt's own sender moves a pending row holding no owner lock
// (pendingAttemptsRead). So the statement settles every attempt still pending and answers of each
// whether the read found it as it stands: were it to settle another count, or a row the read
// found otherwise, its callback refuses with errDeleteContended, Commit returns that refusal, which
// the route answers as it is (409 DELETE_CONTENDED, Retry-After: 1), and the receipts never land
// beside rows they do not describe.
func queueDeletedPostSettlement(b *store.WriteBatch, itemID string, read pendingAttemptsRead) {
	b.Queue("", `
		update `+read.table+` d set state = 'failed', error = $2, claimed_at = now()
		where d.`+read.owner+` = $1 and d.state = 'pending'
		returning `+read.matches,
		append([]any{itemID, deletedPostDeliveryError}, read.args...)...,
	).Query(func(rows pgx.Rows) error {
		settled, matched := 0, 0
		for rows.Next() {
			var seen bool
			if err := rows.Scan(&seen); err != nil {
				return err
			}
			settled++
			if seen {
				matched++
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if settled != read.pending || matched != settled {
			return errDeleteContended
		}
		return nil
	})
}

// getCommentHistory answers GET /api/v1/comments/{id}/history, read by whoever GET
// /api/v1/comments/{id} answers: any authenticated caller.
func (s *server) getCommentHistory(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !requireUUIDPath(w, r, "comment", apicode.CommentIDInput) {
		return
	}
	s.writePostHistory(w, r, refs.ItemComment)
}

// getMessageHistory answers GET /api/v1/messages/{id}/history under the read rule of GET
// /api/v1/messages/{id} (readableMessageThread), as the message's blocks read is. Only an issue
// message is edited or deleted, and that rule asks for a session on a thread with no issue
// alone, so it costs this read nothing.
func (s *server) getMessageHistory(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.readableMessageThread(w, r); !ok {
		return
	}
	s.writePostHistory(w, r, refs.ItemMessage)
}

// writePostHistory answers the /api/v1 history read of the post the path's {id} names: one page of
// contracts.PostHistoryPageLimit kept bodies, newest first, below ?before= where it names one.
func (s *server) writePostHistory(w http.ResponseWriter, r *http.Request, kind refs.ItemKind) {
	before, err := parseOptionalRFC3339(r.URL.Query(), "before", time.Time{})
	if err != nil {
		writeError(w, r, apicode.InvalidQuery, err.Error())
		return
	}
	window := postHistoryWindow{limit: contracts.PostHistoryPageLimit}
	if !before.IsZero() {
		window.before = &postHistoryPosition{at: before}
	}
	read, _, err := s.postHistory(r.Context(), kind, r.PathValue("id"), window)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, read)
}

// postHistoryWindow is the part of one post's kept bodies a read answers, by (edited_at, id): those
// written after `after` and before `before` where each is set, newest first or, with oldestFirst,
// oldest first, at most limit of them past the first offset. /api/v1 reads a page of
// contracts.PostHistoryPageLimit below a before that names a time alone; /api/v2 pages its log by
// both bounds, each a time and an id, limit and offset, and counts it (counted).
type postHistoryWindow struct {
	after, before *postHistoryPosition
	oldestFirst   bool
	limit, offset int
	// counted asks for the tally beside the page (postHistoryTally).
	counted bool
}

// postHistoryPosition is a bound of a postHistoryWindow: a kept body's edited_at and its id. A
// before whose id is 0 keeps every body written before at, since no row's identity is below 1:
// /api/v1's ?before=, which names a time alone.
type postHistoryPosition struct {
	at time.Time
	id int64
}

// args are the bound's time and id as the statement binds them, both null for no bound.
func (position *postHistoryPosition) args() (*time.Time, *int64) {
	if position == nil {
		return nil, nil
	}
	return &position.at, &position.id
}

// postHistoryTally is what a counted read answers beside its page: total, how many kept bodies
// the window's two bounds keep, and reached, whether a kept body was written at or after the
// window's after, true where it names none.
type postHistoryTally struct {
	total   int
	reached bool
}

// postHistory is one post's kept bodies within window, at most window.limit of them past the first
// window.offset. The order is the partial index's (post_body_history_comment,
// post_body_history_message), (edited_at, id) read forward or backward, so the read sorts
// nothing, and edited_at is clock_timestamp(), so two writes of one transaction order by it with
// the identity breaking a tie within one reading, as each bound does.
//
// /api/v1's page is contracts.PostHistoryPageLimit and not maxEventPage, though both are 200
// today. maxEventPage bounds a page of an issue's or a document's event log; this page is a
// different read, and it is also the bound on which `dispatch read --history` decides a page is
// full and names the read of its older rows. Under one name a change to the event page would move
// this page with no client told, so this one is declared in packages/contracts, which both ends
// read.
//
// The post's deleted mark and its kept bodies are one statement, so both are read in one
// snapshot: as two statements a delete that committed between them served the bodies the
// first read found with no mark on them, which is the one answer this read must never give.
// The post itself drives the join, so a post that is not there answers no row at all, as the
// single read of it did (pgx.ErrNoRows, 404); the lateral is its page of kept bodies, read
// through the same partial index under the same order and limit, and a post that kept none
// answers one row with every history column null. A counted read counts the post's kept bodies
// in a lateral of its own, over the same index, in the same snapshot.
func (s *server) postHistory(ctx context.Context, kind refs.ItemKind, id string, window postHistoryWindow) (postHistoryRead, postHistoryTally, error) {
	items, column, err := postHistoryOwner(kind)
	if err != nil {
		return postHistoryRead{}, postHistoryTally{}, err
	}
	order := "edited_at desc, id desc"
	if window.oldestFirst {
		order = "edited_at asc, id asc"
	}
	bounded := `($2::timestamptz is null or (edited_at, id) < ($2::timestamptz, coalesce($3::bigint, 0)))` +
		` and ($4::timestamptz is null or (edited_at, id) > ($4::timestamptz, $5::bigint))`
	tally, tallied := `, null::bigint, null::boolean`, ``
	if window.counted {
		tally = `, c.total, c.reached`
		tallied = `
		left join lateral (
			select count(*) filter (where ` + bounded + `) as total,
			       $4::timestamptz is null
			       or coalesce(bool_or((edited_at, id) >= ($4::timestamptz, $5::bigint)), false) as reached
			from post_body_history
			where ` + column + ` = p.id
		) c on true`
	}
	beforeAt, beforeID := window.before.args()
	afterAt, afterID := window.after.args()
	// A delete gives no part of the body on any read of the post, so the mark stands in place of
	// every body this list would otherwise serve and the read answers none of them.
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select p.deleted_at, p.deleted_by,
		       case when p.deleted_at is null then h.body else '' end, h.edited_by, h.edited_at, h.id`+tally+`
		from `+items+` p
		left join lateral (
			select id, body, edited_by, edited_at
			from post_body_history
			where `+column+` = p.id and `+bounded+`
			order by `+order+`
			limit $6 offset $7
		) h on true`+tallied+`
		where p.id = $1
	`, id, beforeAt, beforeID, afterAt, afterID, window.limit, window.offset)
	if err != nil {
		return postHistoryRead{}, postHistoryTally{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	defer rows.Close()
	read := postHistoryRead{Entries: []postHistoryEntry{}}
	var counted postHistoryTally
	found := false
	for rows.Next() {
		var deletedAt, editedAt *time.Time
		var deletedBy, editedBy []byte
		var body *string
		var total, rowID *int64
		var reached *bool
		if err := rows.Scan(&deletedAt, &deletedBy, &body, &editedBy, &editedAt, &rowID, &total, &reached); err != nil {
			return postHistoryRead{}, postHistoryTally{}, fmt.Errorf("scan a kept body of %s %s: %w", kind, id, err)
		}
		if !found {
			found = true
			if read.DeletedAt, read.DeletedBy, err = model.DeletedPostMark(deletedAt, deletedBy); err != nil {
				return postHistoryRead{}, postHistoryTally{}, err
			}
			if total != nil && reached != nil {
				counted = postHistoryTally{total: int(*total), reached: *reached}
			}
		}
		// The post kept no body this page holds: the left join's one row carries the mark alone.
		if editedAt == nil {
			continue
		}
		entry := postHistoryEntry{Body: *body, EditedAt: editedAt.UTC(), id: *rowID}
		if err := json.Unmarshal(editedBy, &entry.EditedBy); err != nil {
			return postHistoryRead{}, postHistoryTally{}, fmt.Errorf("decode the actor of a kept body of %s %s: %w", kind, id, err)
		}
		read.Entries = append(read.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return postHistoryRead{}, postHistoryTally{}, fmt.Errorf("read the body history of %s %s: %w", kind, id, err)
	}
	if !found {
		return postHistoryRead{}, postHistoryTally{}, pgx.ErrNoRows
	}
	return read, counted, nil
}
