package api

// v2_threads.go is the comments, messages, broadcasts and handoffs of /api/v2 (R1 of
// dispatch://INFRA-4071/spec): a comment and a message read flat, the item's own fields at top level
// with its replies beside them and its blocks with include=blocks (the route map's J4); an
// issue's comments, messages and handoffs, a session's conversations and the broadcasts, each an
// envelope; the state of a comment and of a message's delivery attempt, each a PUT of a body
// keyed by state; a session's replies to its deliveries as collections (J21); and the creates.
// Every write runs the work its /api/v1 route runs (writeComment, editCommentAs, deleteCommentAs,
// reopenCommentAs, commentActionAs, retryCommentMention, answerCommentDelivery,
// postIssueMessage, editMessageAs, deleteMessageAs, sendAgentMessage, retryMessageDelivery,
// acceptDeliveryAs, answerMessageDelivery, sendBroadcast, recordHandoffInput), and every read of a
// post's corrections the read its /api/v1 history runs (postHistory), each path object checked
// first and answered with its own 404, and a {session_id} keeps the answer /api/v1 gives it (R7).

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// The authorities of the thread rows (authorities_v2.go): the comment states a
// person decides need the person scope, and a resolve, which any caller may make, none.
var (
	commentOpenAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "state"}, Value: "open", Scopes: routeScopeSet{model.ScopePerson},
	}
	commentAcceptedAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "state"}, Value: "accepted", Scopes: routeScopeSet{model.ScopePerson},
	}
	commentRejectedAuthority = authorityRule{
		Where: authorityWhere{In: partBody, Name: "state"}, Value: "rejected", Scopes: routeScopeSet{model.ScopePerson},
	}
)

// The refusals of a missing object a thread route's path names (dispatch://INFRA-4071/spec R7).
var (
	commentMissingRefusal   = apicode.Refusal{Condition: apicode.CommentMissing, Code: apicode.CommentNotFound}
	messageMissingRefusal   = apicode.Refusal{Condition: apicode.MessageMissing, Code: apicode.MessageNotFound}
	deliveryMissingRefusal  = apicode.Refusal{Condition: apicode.DeliveryMissing, Code: apicode.DeliveryNotFound}
	broadcastMissingRefusal = apicode.Refusal{Condition: apicode.BroadcastMissing, Code: apicode.BroadcastNotFound}
	handoffMissingRefusal   = apicode.Refusal{Condition: apicode.HandoffMissing, Code: apicode.HandoffNotFound}
)

// notAuthorRefusal is the refusal of a write only the author of what the path names may make, from
// a caller who is not that author: one code on every /api/v2 route that refuses one (R1.8, R7).
var notAuthorRefusal = apicode.Refusal{Condition: apicode.NotTheAuthor, Code: apicode.NotAuthor}

// The refusals of a post's body and its writes that the thread routes share: a comment's input the
// write refuses, a body holding a decision block, a message's input, its target or delivery, a
// mention's input, a send moved under a delete, a message whose target moved, a reply from a
// caller the attempt is not for, and the edit or the delete of a message of a conversation with no
// issue.
var (
	commentInvalidRefusal     = apicode.Refusal{Condition: apicode.CommentInvalid, Code: apicode.InvalidComment}
	bodyDecisionBlockRefusal  = apicode.Refusal{Condition: apicode.BodyHoldsDecisionBlock, Code: apicode.DecisionBlockInBody}
	messageInvalidRefusal     = apicode.Refusal{Condition: apicode.MessageInvalid, Code: apicode.InvalidMessage}
	messageInputRefusal       = apicode.Refusal{Condition: apicode.MessageInputRefused, Code: apicode.MessageInput}
	mentionInvalidRefusal     = apicode.Refusal{Condition: apicode.MentionInvalid, Code: apicode.MentionInput}
	deleteRacedSendRefusal    = apicode.Refusal{Condition: apicode.DeleteRacedSend, Code: apicode.DeleteContended}
	messageTargetMovedRefusal = apicode.Refusal{Condition: apicode.MessageTargetMoved, Code: apicode.MessageTargetChanged}
	replyNotRecipientRefusal  = apicode.Refusal{Condition: apicode.ReplyNotTheRecipient, Code: apicode.ReplyForbidden}
	correctionOfDirectRefusal = apicode.Refusal{Condition: apicode.CorrectionOfDirectMessage, Code: apicode.NotAnIssueMessage}
)

// The refusals of a direct conversation's read and of a handoff that the thread routes share: a
// direct conversation read by a credential without the person scope that names no session, or
// as a session outside it; and a handoff's phase that is no lowercase slug, or a body that is not
// one JSON object as a string.
var (
	directThreadNoSessionRefusal = apicode.Refusal{Condition: apicode.DirectThreadWithoutSession, Code: apicode.SessionRequired}
	directThreadOutsideRefusal   = apicode.Refusal{Condition: apicode.DirectThreadOfAnotherSession, Code: apicode.ThreadForbidden}
	handoffInputRefusal          = apicode.Refusal{Condition: apicode.HandoffInputInvalid, Code: apicode.HandoffInput}
)

// commentWriteRefusals are the refusals of the comment a document's or an issue's comment list
// takes (writeComment), with more, the route's own, after them: its body and thread members, its
// mentions, its anchor and what the document service answers it (anchorRefusals), the post it
// replies to deleted, and a delivery route that moved under it.
func commentWriteRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append(anchorRefusals([]apicode.Refusal{
		undeclaredKeyRefusal, commentInvalidRefusal, valuePastBoundRefusal, bodyDecisionBlockRefusal,
		anchorInvalidRefusal, artifactNotDocumentRefusal, postDeletedRefusal,
		mentionInvalidRefusal,
		{Condition: apicode.AskIDShort, Code: apicode.AskIDInput},
		{Condition: apicode.RouteMoved, Code: apicode.RouteChanged},
		{Condition: apicode.TurnWithoutAsk, Code: apicode.TurnRequiresAsk},
	}...), more...)
}

// messageWriteRefusals are the refusals of the message an issue's or a session's message list
// takes (createStoredMessage), with more, the route's own, after them: its body, its target,
// delivery and in_reply_to, a reply to a deleted message, a target that moved under its first
// attempt, and a quote its in_reply_to_block does not hold or holds more than once
// (replyQuoteRefusal, namedBlock.setQuote).
func messageWriteRefusals(more ...apicode.Refusal) []apicode.Refusal {
	return append([]apicode.Refusal{
		undeclaredKeyRefusal, messageInvalidRefusal, messageInputRefusal, valuePastBoundRefusal,
		bodyDecisionBlockRefusal, postDeletedRefusal, messageTargetMovedRefusal, targetMissingRefusal, quoteAmbiguousRefusal,
	}, more...)
}

// correctionPosition is the position of a post's corrections (R5): the time of the write that
// replaced each body and the correction's own id, `<RFC3339 time>,<id>`. Two corrections of one
// post can carry one edited_at - post_body_history's edited_at is clock_timestamp(), unique in no
// constraint, and its identity breaks the tie - so a position of the time alone would lose or
// repeat a correction at a page boundary between two equal times, as the dismissed threads'
// would (dismissedPosition).
var correctionPosition = positionField{Field: "edited_at,id", Type: positionTimeAndID}

// postCorrectionsText is what the descriptions of a comment's and a message's corrections say
// alike: the position, the order, the bounds and the refusals of a position.
var postCorrectionsText = fmt.Sprintf("Each item is {id, body, edited_by, edited_at}. Position: edited_at,id, "+
	"each bound <RFC3339 time>,<id>, the edited_at and id of an item a page answered, the id breaking a tie "+
	"between two corrections written at one time. before keeps the items written before that item and after "+
	"those written after it, both together a window; the items come newest first, and oldest first from an "+
	"after. limit 1 to %d (default %d) and offset; {deleted_at, deleted_by, items, total, limit, offset}, total "+
	"the exact count of the items the window keeps. An after or a before that is no time and id, and an after "+
	"above the newest item, are 400 POSITION_INVALID naming the parameter and edited_at,id.",
	contracts.PostHistoryPageLimit, contracts.PostHistoryPageLimit)

// messageThreadText is what every description of a route answering message threads says of the
// row it answers.
const messageThreadText = "Each thread is one row: its root message's own fields at top level, with the root's delivery " +
	"attempts in deliveries, and replies, every message replying to it, oldest first, each with its own attempts."

func v2CommentIDs() *idRoute { return &idRoute{Kind: "comment", NotFound: apicode.CommentNotFound} }

func v2MessageIDs() *idRoute { return &idRoute{Kind: "message", NotFound: apicode.MessageNotFound} }

// v2HandoffList is the List of an issue's handoffs: no bounds of its own, so 50 and 200
// (listBounds.limits), which its description words its page from.
var v2HandoffList = listBounds{}

// commentReadV2Query is GET /api/v2/comments/{id}'s query.
type commentReadV2Query struct {
	Include []string `query:"include,comma" enum:"blocks"`
}

// commentReadV2 is GET /api/v2/comments/{id}'s answer (the route map's J4): the comment's own
// fields at top level, as GET /api/v1/comments/{id} answers them under comment, its replies beside
// them, and its blocks with include=blocks, which GET /api/v1/comments/{id}/blocks answered. It is
// a type of its own, so no /api/v1 shape's tags change (dispatch://INFRA-4071/spec R3).
type commentReadV2 struct {
	shapedComment
	Replies []shapedComment `json:"replies"`
	// Blocks are the comment's current blocks, with include=blocks.
	Blocks *model.ItemBlocks `json:"blocks,omitempty"`
}

// commentStateInput is the open, the accepted and the rejected arm of PUT
// /api/v2/comments/{id}/state: the state alone, which a person moves the comment to.
type commentStateInput struct {
	State string `json:"state"`
}

// resolveCommentStateInput is the resolved arm of PUT /api/v2/comments/{id}/state, which any
// caller may send: a bearer names its session in actor.
type resolveCommentStateInput struct {
	State string       `json:"state"`
	Actor *model.Actor `json:"actor"`
}

// issueCommentsV2Query is GET /api/v2/issues/{key}/comments's query.
type issueCommentsV2Query struct {
	pageQuery
	// Artifact keeps the comments anchored in one document of the issue.
	Artifact string `query:"artifact"`
}

// messageReadV2Query is GET /api/v2/messages/{id}'s query.
type messageReadV2Query struct {
	Include []string `query:"include,comma" enum:"blocks"`
}

// messageThreadV2 is one conversation as every /api/v2 read of messages answers it (the route
// map's J4): the root message's own fields at top level, with its delivery attempts, and every
// message replying to it beside them. GET /api/v1's messageRead nests the root under message; this
// is a type of its own, so no /api/v1 shape's tags change (dispatch://INFRA-4071/spec R3).
type messageThreadV2 struct {
	shapedMessage
	Replies []shapedMessage `json:"replies"`
}

// messageThreadOf is read as /api/v2 answers a conversation.
func messageThreadOf(read messageRead) messageThreadV2 {
	return messageThreadV2{shapedMessage: read.Message, Replies: read.Replies}
}

// messageReadV2 is GET /api/v2/messages/{id}'s answer: the conversation, with the blocks of the
// message the id names where include=blocks asks for them.
type messageReadV2 struct {
	messageThreadV2
	// Blocks are the current blocks of the message the path names, with include=blocks.
	Blocks *model.ItemBlocks `json:"blocks,omitempty"`
}

// postCorrectionsV2Query is the query of a post's corrections, GET /api/v2/comments/{id}/corrections
// and GET /api/v2/messages/{id}/corrections: one page of them, by edited_at and id.
type postCorrectionsV2Query struct {
	pageQuery
	// After keeps the corrections written after this position, oldest first, and Before those
	// written before it: each `<RFC3339 time>,<id>`, read by readCorrectionPosition rather than by
	// the decoder, so one that is no position is the log's own refusal, POSITION_INVALID, naming
	// the field (R5).
	After  *string `query:"after"`
	Before *string `query:"before"`
}

// postCorrectionV2 is one item of a post's corrections: a body an edit or a delete replaced
// (postHistoryEntry), with its own id, which with edited_at is its position.
type postCorrectionV2 struct {
	ID int64 `json:"id"`
	postHistoryEntry
}

// postCorrectionsV2 is the body a post's corrections answer: the envelope of the bodies its edits
// and its delete replaced (postHistoryRead's entries), with the delete mark the post carries beside
// it, as every read of the post carries it.
type postCorrectionsV2 struct {
	DeletedAt *time.Time   `json:"deleted_at"`
	DeletedBy *model.Actor `json:"deleted_by"`
	page[postCorrectionV2]
}

// acceptDeliveryStateInput is the accepted arm of PUT /api/v2/messages/{id}/deliveries/{attempt}/state:
// the session the attempt went to names itself in actor.
type acceptDeliveryStateInput struct {
	State string       `json:"state"`
	Actor *model.Actor `json:"actor"`
}

// messageRepliesV2Query is POST /api/v2/messages/{id}/replies's query.
type messageRepliesV2Query struct {
	// FollowUp, true, posts other text as the session's follow-up under its first reply once the
	// attempt is answered; false, the default, posts nothing then.
	FollowUp bool `query:"follow_up"`
}

// issueMessagesV2Query is GET /api/v2/issues/{key}/messages's query: one page.
type issueMessagesV2Query struct {
	pageQuery
}

// agentMessagesV2Query is GET /api/v2/agents/{session_id}/messages's query.
type agentMessagesV2Query struct {
	pageQuery
	// Unread keeps the conversations holding a reply the caller has not read.
	Unread bool `query:"unread"`
}

// agentMessageV2 is one row of GET /api/v2/agents/{session_id}/messages: the conversation, and
// whether it holds a reply the caller has not read, the verdict the Agents page's count reads
// (unreadDirectRepliesCTE).
type agentMessageV2 struct {
	messageThreadV2
	Unread bool `json:"unread"`
}

// broadcastsV2Query is GET /api/v2/broadcasts's query: one page.
type broadcastsV2Query struct {
	pageQuery
}

// createBroadcastV2Input is POST /api/v2/broadcasts's body. The idempotency_key member
// createBroadcastInput refuses by name is no member here: no client of /api/v2 predates the
// Idempotency-Key header, so a body naming it is a member the route does not declare, ignored as
// every such member is (R4).
type createBroadcastV2Input struct {
	Body       string   `json:"body" required:"true"`
	Delivery   string   `json:"delivery" required:"true"`
	SessionIDs []string `json:"session_ids" required:"true"`
}

// issueHandoffsV2Query is GET /api/v2/issues/{key}/handoffs's query.
type issueHandoffsV2Query struct {
	pageQuery
	// Phase keeps one phase's handoffs.
	Phase string `query:"phase"`
	// Include adds each handoff's body (body), which renders each listed row whole, so a page
	// that names it holds at most includedPageMax (renders).
	Include []string `query:"include,comma" enum:"body" renders:"body"`
	// Sort is created_at or version.
	Sort querySort `query:"sort" enum:"created_at,version" default:"-created_at"`
}

// handoffV2 is one row of GET /api/v2/issues/{key}/handoffs: the handoff, and its body with
// include=body, which GET /api/v1's list never carried, with the body's field trees beside it for a
// caller that sent `Dispatch-Trees: 1`, as the read of one handoff gives them (handoffRead).
type handoffV2 struct {
	model.HandoffSummary
	// Body is the handoff's body, the exact text written, with include=body.
	Body *string `json:"body,omitempty"`
	// FieldTrees is handoffRead's FieldTrees of Body: the tree of each string the card draws from
	// it, keyed by that string's JSON Pointer into it, only with include=body and the header.
	FieldTrees map[string]model.DisplayTree `json:"field_trees,omitempty"`
}

// createHandoffV2Input is POST /api/v2/issues/{key}/handoffs's body: createHandoffInput's
// members, with the phase, which /api/v1 names in its path, as a field of the handoff (the route
// map's row 105).
type createHandoffV2Input struct {
	Actor *model.Actor `json:"actor"`
	Phase string       `json:"phase" required:"true"`
	Body  *string      `json:"body" required:"true"`
}

// requireHeldV2 answers whether the object a /api/v2 path names by the uuid id exists, and where
// it does not writes its 404, code with message, the code of the object the path names
// (dispatch://INFRA-4071/spec R7). exists is the select exists that finds it, over $1, the id, and
// more after it; what names the object in the error of a read that fails. An id that is no uuid
// names no object and is not read. The prefix layer has already read an id written short into
// the whole one it names (resolveIDPrefixes).
func (s *server) requireHeldV2(w http.ResponseWriter, r *http.Request, id, what, exists string, code apicode.Code, message string, more ...any) bool {
	held := false
	if _, err := uuid.Parse(id); err == nil {
		if err := s.deps.Store.Pool.QueryRow(r.Context(), exists, append([]any{id}, more...)...).Scan(&held); err != nil {
			s.writeHandlerError(w, r, fmt.Errorf("read whether the %s exists: %w", what, err))
			return false
		}
	}
	if !held {
		writeError(w, r, code, message)
	}
	return held
}

// requireCommentV2 answers whether the comment id names exists, and where it does not writes 404
// COMMENT_NOT_FOUND (requireHeldV2).
func (s *server) requireCommentV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "comment", `select exists (select 1 from comments where id = $1)`,
		apicode.CommentNotFound, "no comment has the id "+id)
}

// requireMessageV2 answers whether the message id names exists, and where it does not writes 404
// MESSAGE_NOT_FOUND (requireHeldV2).
func (s *server) requireMessageV2(w http.ResponseWriter, r *http.Request, id string) bool {
	return s.requireHeldV2(w, r, id, "message", `select exists (select 1 from messages where id = $1)`,
		apicode.MessageNotFound, "no message has the id "+id)
}

// requireBearerCaller answers whether the request carries a bearer credential, and where it does
// not writes refusal, the 403 a route only an agent's session may call answers a person's cookie.
// The caller has already been held to a credential that resolves (requireAuthenticated), so a
// request with none at all is answered 401 NO_IDENTITY before this.
func requireBearerCaller(w http.ResponseWriter, r *http.Request, refusal apicode.Code, message string) bool {
	if strings.TrimSpace(r.Header.Get("Authorization")) == "" {
		writeError(w, r, refusal, message)
		return false
	}
	return true
}

// getCommentV2 answers GET /api/v2/comments/{id}: the comment and its replies, as GET
// /api/v1/comments/{id} reads them (readCommentThread), and its blocks with include=blocks.
func (s *server) getCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireCommentV2(w, r, id) {
		return
	}
	read, err := s.readCommentThread(r.Context(), s.requestReader(r), id, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer := commentReadV2{shapedComment: read.Comment, Replies: read.Replies}
	if slices.Contains(v2Query[commentReadV2Query](r).Include, "blocks") {
		blocks, err := s.itemBlocks(r.Context(), s.requestReader(r), refs.ItemComment, id)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.Blocks = &blocks
	}
	WriteJSON(w, http.StatusOK, answer)
}

// editCommentV2 answers PATCH /api/v2/comments/{id}: the edit PATCH /api/v1/comments/{id} makes
// (editCommentAs), by the caller correctionActor resolves from the body's actor.
func (s *server) editCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	input := v2Body[editCommentInput](r)
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	s.editCommentAs(w, r, actor, input)
}

// deleteCommentV2 answers DELETE /api/v2/comments/{id}: the delete DELETE /api/v1/comments/{id}
// makes (deleteCommentAs), by the caller correctionActor resolves from the body's actor, a body
// the request may leave out.
func (s *server) deleteCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	actor, ok := s.correctionActor(w, r, v2Body[actorInput](r).Actor)
	if !ok {
		return
	}
	s.deleteCommentAs(w, r, actor)
}

// listCommentCorrectionsV2 answers GET /api/v2/comments/{id}/corrections: a page of the bodies GET
// /api/v1/comments/{id}/history reads, read by whoever reads the comment.
func (s *server) listCommentCorrectionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	s.answerPostCorrectionsV2(w, r, refs.ItemComment)
}

// answerPostCorrectionsV2 answers one page of the corrections of the post of kind the path's {id}
// names, which the route has found: the read both versions' history routes make (postHistory),
// within the window the decoded query's after and before name, counted. A window from an after
// reads up from it, oldest first, as a log read forward does; every other reads down, newest
// first, as /api/v1's history does.
func (s *server) answerPostCorrectionsV2(w http.ResponseWriter, r *http.Request, kind refs.ItemKind) {
	query := v2Query[postCorrectionsV2Query](r)
	window := postHistoryWindow{limit: query.Limit, offset: query.Offset, counted: true}
	var err error
	if window.after, err = readCorrectionPosition("after", query.After); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if window.before, err = readCorrectionPosition("before", query.Before); err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	window.oldestFirst = window.after != nil
	read, tally, err := s.postHistory(r.Context(), kind, r.PathValue("id"), window)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !tally.reached {
		s.writeHandlerError(w, r, errorf(apicode.PositionInvalid,
			"after is %q, above this log's last %s: no correction of this %s was written at or after it",
			*query.After, correctionPosition.Field, kind))
		return
	}
	items := make([]postCorrectionV2, len(read.Entries))
	for index, entry := range read.Entries {
		items[index] = postCorrectionV2{ID: entry.id, postHistoryEntry: entry}
	}
	WriteJSON(w, http.StatusOK, postCorrectionsV2{
		DeletedAt: read.DeletedAt, DeletedBy: read.DeletedBy, page: newPage(items, tally.total, query.pageQuery),
	})
}

// readCorrectionPosition reads a position of a post's corrections, `<RFC3339 time>,<id>`, that the
// parameter name gives: the edited_at and id of a correction, as the answer writes them; nil where
// the parameter gives none. One that is no such position is 400 POSITION_INVALID naming the
// parameter and the two fields (R5).
func readCorrectionPosition(name string, raw *string) (*postHistoryPosition, error) {
	if raw == nil {
		return nil, nil
	}
	refusal := errorf(apicode.PositionInvalid,
		"%s reads a post's corrections' %s, an RFC3339 time and a correction's id joined by a comma; %q is none",
		name, correctionPosition.Field, *raw)
	at, id, found := strings.Cut(*raw, ",")
	if !found {
		return nil, refusal
	}
	when, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, refusal
	}
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || number < 1 {
		return nil, refusal
	}
	return &postHistoryPosition{at: when, id: number}, nil
}

// createCommentDeliveryV2 answers POST /api/v2/comments/{id}/deliveries: the retry POST
// /api/v1/comments/{id}/deliveries makes (retryCommentMention).
func (s *server) createCommentDeliveryV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	if !s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	input := v2Body[createCommentDeliveryInput](r)
	if !human {
		if actor, err = bearerSessionActor(r, actor, input.Actor); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	s.retryCommentMention(w, r, actor, input)
}

// replyCommentV2 answers POST /api/v2/comments/{id}/replies: the mentioned session's answer POST
// /api/v1/comments/{id}/reply records (answerCommentDelivery). The caller is held to a bearer
// credential before the comment is read, as /api/v1 holds it, so a person's cookie is refused
// whatever comment the path names.
func (s *server) replyCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) ||
		!requireBearerCaller(w, r, apicode.ReplyForbidden, "reply requires an agent bearer token") ||
		!s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	s.answerCommentDelivery(w, r, v2Body[commentReplyInput](r))
}

// commentStateOf is the state a comment's thread stands in, as PUT /api/v2/comments/{id}/state
// names its states: open while it is not resolved, and once it is, accepted or rejected for a
// suggestion decided that way and resolved for every other.
func commentStateOf(comment model.Comment) string {
	switch {
	case !comment.Resolved:
		return "open"
	case comment.Suggestion != nil && comment.Suggestion.Accepted != nil && *comment.Suggestion.Accepted:
		return "accepted"
	case comment.Suggestion != nil && comment.Suggestion.Accepted != nil:
		return "rejected"
	}
	return "resolved"
}

// commentStateSetter is who set the state a comment stands in (dispatch://INFRA-4071/spec R8): the
// actor that resolved, accepted or rejected it, and nobody recorded for an open thread, which a
// reopen leaves with no resolver and a new comment has never had.
func commentStateSetter(comment model.Comment) putSetter {
	switch {
	case !comment.Resolved:
		return noSetterRecorded
	case comment.ResolvedBy == nil:
		return setBy("")
	}
	return setBy(actorIdentity(*comment.ResolvedBy))
}

// putCommentStateV2 answers PUT /api/v2/comments/{id}/state: each state's arm runs its /api/v1
// route's work - the reopen (reopenCommentAs), the resolve, the accept or the reject
// (commentActionAs) - once the no-op rule of dispatch://INFRA-4071/spec R8 has found the body is a
// change. A body equal to the comment's present state from the caller who set it answers the
// comment and writes nothing (putChangesNothing), an accept's answer with lost null, since nothing
// was applied that could have lost text. A suggestion accepted or rejected is a thread resolved by
// whoever decided it, so {state: resolved} on it is that equal body from them, and from anyone
// else 409 ALREADY_ACTIONED, as an accept or a reject of it is: a resolve would record somebody
// else as the one who decided it.
func (s *server) putCommentStateV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireCommentV2(w, r, id) {
		return
	}
	var actor model.Actor
	var ok bool
	var unchanged func(current string, setter putSetter) bool
	var state string
	switch body := v2BodyArm(r).(type) {
	case resolveCommentStateInput:
		state = body.State
		actor, ok = s.requireActor(w, r, body.Actor)
		unchanged = func(current string, setter putSetter) bool {
			present := body
			present.State = current
			return putChangesNothing(body, present, setter, actorIdentity(actor))
		}
	case commentStateInput:
		state = body.State
		actor, ok = s.actingPerson(w, r)
		unchanged = func(current string, setter putSetter) bool {
			return putChangesNothing(body, commentStateInput{State: current}, setter, actorIdentity(actor))
		}
	default:
		s.writeHandlerError(w, r, fmt.Errorf("PUT %s: the state body was decoded as %T, which no state of a comment is", r.URL.Path, body))
		return
	}
	if !ok {
		return
	}
	comment, err := s.loadComment(r.Context(), s.deps.Store.Pool, id)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	current := commentStateOf(comment)
	decided := current == "accepted" || current == "rejected"
	if state == "resolved" && decided {
		current = "resolved"
	}
	if unchanged(current, commentStateSetter(comment)) {
		shaped := newShapedComment(comment, wantsDisplayTrees(r))
		if state == "accepted" {
			WriteJSON(w, http.StatusOK, acceptedComment{shapedComment: shaped})
			return
		}
		WriteJSON(w, http.StatusOK, shaped)
		return
	}
	if state == "resolved" && decided {
		writeError(w, r, apicode.AlreadyActioned, "suggestion has already been actioned")
		return
	}
	switch state {
	case "open":
		s.reopenCommentAs(w, r, actor)
	case "resolved":
		s.commentActionAs(w, r, actor, "resolve")
	case "accepted":
		s.commentActionAs(w, r, actor, "accept")
	case "rejected":
		s.commentActionAs(w, r, actor, "reject")
	default:
		s.writeHandlerError(w, r, fmt.Errorf("PUT %s: the decoder took the state %q, which no arm of a comment's state is", r.URL.Path, state))
	}
}

// issueCommentsV2ETag is commentListETag for one page of an issue's /api/v2 comment list: the
// issue, its created_at (so an issue key recycled across resets never shares a validator with the
// issue that held it before, as commentListETag hashes it), the artifact= scope, the list's
// watermark and the issue's event sequence, and the page, under a line naming the version, so no
// validator one version gave matches the other's answer.
func (s *server) issueCommentsV2ETag(key, artifactFilter string, basis commentListBasis, query pageQuery, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\nissue=%s\ncreated=%d\nfilter=%s\nwatermark=%d\nlast_seq=%d\nlimit=%d\noffset=%d\n",
		key, basis.createdAt.UnixMicro(), artifactFilter, basis.watermark, basis.lastSeq, query.Limit, query.Offset)
	return s.listETag(hash, trees)
}

// listIssueCommentsV2 answers GET /api/v2/issues/{key}/comments: one page of the comment list GET
// /api/v1/issues/{key}/comments answers (answerCommentList), its validator read first, under a
// validator of its own and kept under the /api/v2 route's own key (commentListV2Route), so a v1
// read and a v2 read of one issue's comments each answer their own version's body. The validator's
// own statement says whether the issue is there (readCommentListBasis), as an issue's event log
// reads it (listIssueEventsV2), and only the page it answers is hydrated.
func (s *server) listIssueCommentsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	query := v2Query[issueCommentsV2Query](r)
	artifact := strings.TrimSpace(query.Artifact)
	owner := issueOwner(key)
	basis, err := s.readCommentListBasis(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !basis.ownerHeld {
		s.writeHandlerError(w, r, errorf(apicode.IssueNotFound, "no issue %s", key))
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.issueCommentsV2ETag(key, artifact, basis, query.pageQuery, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	kept := listAnswerKeyFor(r, commentListV2Route, key, etag)
	if s.listAnswers.answer(w, kept) {
		return
	}
	// The answer is kept and handed to every reader whose request names this owner and ETag
	// (listAnswers), so it is read as everyReader, as /api/v1's comment list is (answerCommentList).
	listed, err := s.commentPageV2(r.Context(), everyReader, owner, artifact, query.pageQuery)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	reader := s.readDisplayTrees(r.Context(), trees)
	reads := shapedComments(listed.Items, reader)
	reader.resolve()
	encoded, err := encodeListAnswer(newPage(reads, listed.Total, query.pageQuery))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	s.writeListAnswer(w, kept, encoded, basis.ownerHeld)
}

// commentPageV2 is the page query asks for of owner's comments, oldest first, artifactFilter
// keeping one document's where it names one: every comment's row read, and the side tables of
// the page's alone (hydrateCommentSideTables), which reads each comment by itself.
func (s *server) commentPageV2(ctx context.Context, who model.Actor, owner owner, artifactFilter string, query pageQuery) (page[model.Comment], error) {
	comments, err := readOwnerComments(ctx, s.deps.Store.Pool, owner, artifactFilter)
	if err != nil {
		return page[model.Comment]{}, err
	}
	listed := pageOf(comments, query)
	if listed.Items, err = s.hydrateCommentSideTables(ctx, s.deps.Store.Pool, who, listed.Items); err != nil {
		return page[model.Comment]{}, err
	}
	return listed, nil
}

// createIssueCommentV2 answers POST /api/v2/issues/{key}/comments: the comment POST
// /api/v1/issues/{key}/comments writes (writeComment).
func (s *server) createIssueCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	s.writeComment(w, r, issueOwner(key), v2Body[commentInput](r))
}

// getMessageV2 answers GET /api/v2/messages/{id}: the conversation the message belongs to, under
// the read rule of GET /api/v1/messages/{id} (readableThreadAs) with the session the decoded query
// names, and the blocks of the message the path names with include=blocks.
func (s *server) getMessageV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireMessageV2(w, r, id) {
		return
	}
	read, ok := s.readableThreadAs(w, r, id, requestSession(r))
	if !ok {
		return
	}
	answer := messageReadV2{messageThreadV2: messageThreadOf(read)}
	if slices.Contains(v2Query[messageReadV2Query](r).Include, "blocks") {
		blocks, err := s.itemBlocks(r.Context(), s.requestReader(r), refs.ItemMessage, id)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.Blocks = &blocks
	}
	WriteJSON(w, http.StatusOK, answer)
}

// editMessageV2 answers PATCH /api/v2/messages/{id}: the edit PATCH /api/v1/messages/{id} makes
// (editMessageAs), by the caller correctionActor resolves from the body's actor.
func (s *server) editMessageV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireMessageV2(w, r, r.PathValue("id")) {
		return
	}
	input := v2Body[editMessageInput](r)
	actor, ok := s.correctionActor(w, r, input.Actor)
	if !ok {
		return
	}
	s.editMessageAs(w, r, actor, input)
}

// deleteMessageV2 answers DELETE /api/v2/messages/{id}: the delete DELETE /api/v1/messages/{id}
// makes (deleteMessageAs), by the caller correctionActor resolves from the body's actor, a body
// the request may leave out.
func (s *server) deleteMessageV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireMessageV2(w, r, r.PathValue("id")) {
		return
	}
	actor, ok := s.correctionActor(w, r, v2Body[actorInput](r).Actor)
	if !ok {
		return
	}
	s.deleteMessageAs(w, r, actor)
}

// listMessageCorrectionsV2 answers GET /api/v2/messages/{id}/corrections: a page of the bodies GET
// /api/v1/messages/{id}/history reads, under the read rule of GET /api/v2/messages/{id}
// (readableThreadAs) with the session the decoded query names.
func (s *server) listMessageCorrectionsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.requireMessageV2(w, r, id) {
		return
	}
	if _, ok := s.readableThreadAs(w, r, id, requestSession(r)); !ok {
		return
	}
	s.answerPostCorrectionsV2(w, r, refs.ItemMessage)
}

// createMessageDeliveryV2 answers POST /api/v2/messages/{id}/deliveries: the retry POST
// /api/v1/messages/{id}/deliveries makes (retryMessageDelivery).
func (s *server) createMessageDeliveryV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	if !s.requireMessageV2(w, r, r.PathValue("id")) {
		return
	}
	input := v2Body[createDeliveryInput](r)
	if !human {
		if actor, err = bearerSessionActor(r, actor, input.Actor); err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
	}
	s.retryMessageDelivery(w, r, actor, input.Delivery)
}

// putDeliveryStateV2 answers PUT /api/v2/messages/{id}/deliveries/{attempt}/state: the accept POST
// /api/v1/messages/{id}/deliveries/{attempt}/accept makes (acceptDeliveryAs), once the no-op rule
// of dispatch://INFRA-4071/spec R8 has found the body is a change. The attempt already accepted as
// the same session answers the attempt and writes nothing (putChangesNothing); every other accept
// of an accepted message is the accept's own 409 ACCEPT_ALREADY_ACCEPTED or 403 ACCEPT_FORBIDDEN.
func (s *server) putDeliveryStateV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) ||
		!requireBearerCaller(w, r, apicode.AcceptForbidden, "accept requires an agent bearer token") {
		return
	}
	id := r.PathValue("id")
	if !s.requireMessageV2(w, r, id) {
		return
	}
	attempt, ok := s.deliveryAttemptV2(w, r, id, r.PathValue("attempt"))
	if !ok {
		return
	}
	body := v2Body[acceptDeliveryStateInput](r)
	actor, ok := s.requireActor(w, r, body.Actor)
	if !ok {
		return
	}
	if attempt.AcceptedAt != nil && deref(attempt.AcceptedAs) == acceptedAsUserTurn {
		present := body
		present.State = "accepted"
		accepter := setBy(actorIdentity(model.Actor{Kind: "session", ID: attempt.SessionID}))
		if putChangesNothing(body, present, accepter, actorIdentity(actor)) {
			message, err := s.loadMessage(r.Context(), s.deps.Store.Pool, "", id)
			if err != nil {
				s.writeHandlerError(w, r, err)
				return
			}
			WriteJSON(w, http.StatusOK, acceptedDelivery{MessageDelivery: attempt, Body: message.Body})
			return
		}
	}
	s.acceptDeliveryAs(w, r, actor)
}

// deliveryAttemptV2 is the attempt number names of message messageID, and where the message has no
// such attempt - number is no whole number of 1 or more, one past message_deliveries.attempt's
// integer column (fitsIntegerColumn), or one that names none - it writes 404 DELIVERY_NOT_FOUND
// (dispatch://INFRA-4071/spec R7) and answers false, making no query with a number past the
// column.
func (s *server) deliveryAttemptV2(w http.ResponseWriter, r *http.Request, messageID, number string) (model.MessageDelivery, bool) {
	missing := errorf(apicode.DeliveryNotFound, "message %s has no delivery attempt %s", messageID, number)
	attempt, err := strconv.Atoi(number)
	if err != nil || attempt < 1 || !fitsIntegerColumn(attempt) {
		s.writeHandlerError(w, r, missing)
		return model.MessageDelivery{}, false
	}
	read, err := scanMessageDelivery(s.deps.Store.Pool.QueryRow(r.Context(), `
		select `+messageDeliveryColumns+` from message_deliveries where message_id = $1 and attempt = $2
	`, messageID, attempt))
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeHandlerError(w, r, missing)
		return model.MessageDelivery{}, false
	}
	if err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("read the delivery attempt: %w", err))
		return model.MessageDelivery{}, false
	}
	return read, true
}

// replyMessageV2 answers POST /api/v2/messages/{id}/replies: the targeted session's answer POST
// /api/v1/messages/{id}/reply records (answerMessageDelivery), its follow_up the decoded query's.
// The caller is held to a bearer credential before the message is read, as /api/v1 holds it.
func (s *server) replyMessageV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) ||
		!requireBearerCaller(w, r, apicode.ReplyForbidden, "reply requires an agent bearer token") ||
		!s.requireMessageV2(w, r, r.PathValue("id")) {
		return
	}
	s.answerMessageDelivery(w, r, v2Body[messageReplyInput](r), v2Query[messageRepliesV2Query](r).FollowUp, nil)
}

// listIssueMessagesV2 answers GET /api/v2/issues/{key}/messages: one page of the threads GET
// /api/v1/issues/{key}/messages lists, the roots read by the one statement both versions run
// (issueRootMessages) and only the page's threads hydrated.
func (s *server) listIssueMessagesV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	query := v2Query[issueMessagesV2Query](r).pageQuery
	roots, err := s.issueRootMessages(r.Context(), key)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	listed := pageOf(roots, query)
	reads, err := s.hydrateMessageThreads(r.Context(), s.requestReader(r), listed.Items, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	items := make([]messageThreadV2, len(reads))
	for index, read := range reads {
		items[index] = messageThreadOf(read)
	}
	WriteJSON(w, http.StatusOK, newPage(items, listed.Total, query))
}

// createIssueMessageV2 answers POST /api/v2/issues/{key}/messages: the message POST
// /api/v1/issues/{key}/messages posts (postIssueMessage).
func (s *server) createIssueMessageV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	if !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	s.postIssueMessage(w, r, actor, human, v2Body[createMessageInput](r))
}

// listAgentMessagesV2 answers GET /api/v2/agents/{session_id}/messages: the session's
// conversations GET /api/v1/agents/{session_id}/messages reads (agentConversationRootsCTE), every
// one of them paged by limit and offset in its one activity order rather than /api/v1's window of
// the 50 that moved last and every unread one, unread=true keeping those holding a reply the
// caller has not read. A session with no conversation answers an empty page, as a {session_id}
// keeps the answer /api/v1 gives it (dispatch://INFRA-4071/spec R7).
func (s *server) listAgentMessagesV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	if sessionID == "" {
		writeError(w, r, apicode.MessageInput, "session id is required")
		return
	}
	query := v2Query[agentMessagesV2Query](r)
	args := append(unreadDirectRepliesArgs(actor.ID, &sessionID), query.Unread)
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), agentConversationRootsCTE+`
		select count(*) from roots where not $4::boolean or unread
	`, args...).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the session's conversations: %w", err))
		return
	}
	reads, err := s.readAgentConversations(r.Context(), actor, agentConversationRootsCTE+`
		select `+messageColumns+`, unread
		from roots
		where not $4::boolean or unread
		order by moved desc, id desc
		limit $5 offset $6
	`, append(args, query.Limit, query.Offset), wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	items := make([]agentMessageV2, len(reads))
	for index, read := range reads {
		items[index] = agentMessageV2{messageThreadV2: messageThreadOf(read), Unread: read.Unread}
	}
	WriteJSON(w, http.StatusOK, newPage(items, total, query.pageQuery))
}

// createAgentMessageV2 answers POST /api/v2/agents/{session_id}/messages: the send POST
// /api/v1/agents/{session_id}/messages makes (sendAgentMessage), by the person the row's scope
// holds the caller to.
func (s *server) createAgentMessageV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.sendAgentMessage(w, r, actor, v2Body[createAgentMessageInput](r))
}

// listBroadcastsV2 answers GET /api/v2/broadcasts: one page of the broadcasts GET
// /api/v1/broadcasts lists the newest 50 of (broadcastSummaries).
func (s *server) listBroadcastsV2(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
		return
	}
	query := v2Query[broadcastsV2Query](r).pageQuery
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select count(*) from broadcasts`).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the broadcasts: %w", err))
		return
	}
	summaries, err := s.broadcastSummaries(r.Context(), wantsDisplayTrees(r), query.Limit, query.Offset)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, newPage(summaries, total, query))
}

// createBroadcastV2 answers POST /api/v2/broadcasts: the send POST /api/v1/broadcasts makes
// (sendBroadcast), by the person the row's scope holds the caller to.
func (s *server) createBroadcastV2(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	input := v2Body[createBroadcastV2Input](r)
	s.sendBroadcast(w, r, actor, input.Body, input.Delivery, input.SessionIDs)
}

// getHandoffV2 answers GET /api/v2/handoffs/{id}: the handoff GET /api/v1/handoffs/{id} reads
// (handoffByIDQuery), an id that is no uuid naming no handoff, 404 HANDOFF_NOT_FOUND, as every
// other id no handoff has (dispatch://INFRA-4071/spec R7).
func (s *server) getHandoffV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	id := r.PathValue("id")
	missing := "handoff " + id + " not found"
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, r, apicode.HandoffNotFound, missing)
		return
	}
	s.writeHandoff(w, r, missing, handoffByIDQuery, id)
}

// handoffListOrder is the ORDER BY of a sort of GET /api/v2/issues/{key}/handoffs: -created_at,
// the default, is /api/v1's order (newest first, then by phase, then the newest version), and
// version orders each phase's versions, the newest first after a -. Within one issue a phase and
// a version name one handoff, so each order is total.
func handoffListOrder(sort querySort) string {
	switch {
	case sort.Field == "version" && sort.Descending:
		return "version desc, created_at desc, phase"
	case sort.Field == "version":
		return "version, created_at, phase"
	case sort.Descending:
		return "created_at desc, phase, version desc"
	}
	return "created_at, phase, version"
}

// listIssueHandoffsV2 answers GET /api/v2/issues/{key}/handoffs: one page of an issue's handoffs,
// one phase's with phase=, each with its body with include=body, which also answers for GET
// /api/v1/issues/{key}/handoffs/{phase}/latest (the route map's row 106), and so with the body's
// field trees for a caller that sent `Dispatch-Trees: 1`, as that read gives them (writeHandoff).
// A page with the bodies holds at most includedPageMax handoffs, each body up to
// contracts.DispatchHandoffBodyMax, which the decoder holds before the handler runs
// (issueHandoffsV2Query's renders tag).
func (s *server) listIssueHandoffsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	query := v2Query[issueHandoffsV2Query](r)
	key := r.PathValue("key")
	if !s.requireIssueV2(w, r, key) {
		return
	}
	if query.Phase != "" && !handoffPhasePattern.MatchString(query.Phase) {
		writeHandoffPhaseError(w, r, query.Phase)
		return
	}
	var total int
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `
		select count(*) from handoffs where issue_key = $1 and ($2 = '' or phase = $2)
	`, key, query.Phase).Scan(&total); err != nil {
		s.writeHandlerError(w, r, fmt.Errorf("count the issue's handoffs: %w", err))
		return
	}
	withBody := slices.Contains(query.Include, "body")
	columns := handoffColumns
	if withBody {
		columns += ", body"
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+columns+`
		from handoffs
		where issue_key = $1 and ($2 = '' or phase = $2)
		order by `+handoffListOrder(query.Sort)+`
		limit $3 offset $4
	`, key, query.Phase, query.Limit, query.Offset)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	handoffs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (handoffV2, error) {
		if !withBody {
			summary, err := scanHandoff(row)
			return handoffV2{HandoffSummary: summary}, err
		}
		var body string
		summary, err := scanHandoff(row, &body)
		return handoffV2{HandoffSummary: summary, Body: &body}, err
	})
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if withBody && wantsDisplayTrees(r) {
		// Every row's strings are read by the one reader, resolved once the page is shaped.
		shapeBuilt(true, func(reader *displayTreeReader) {
			for index := range handoffs {
				shapeHandoffBody(reader, *handoffs[index].Body, &handoffs[index].FieldTrees)
			}
		})
	}
	WriteJSON(w, http.StatusOK, newPage(handoffs, total, query.pageQuery))
}

// createIssueHandoffV2 answers POST /api/v2/issues/{key}/handoffs: the handoff POST
// /api/v1/issues/{key}/handoffs/{phase} records (recordHandoffInput), its phase a member of the
// body. The caller is held to a bearer credential before the issue is read, as /api/v1 holds it.
func (s *server) createIssueHandoffV2(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, r, err)
		return
	}
	if human {
		writeError(w, r, apicode.HandoffForbidden, "a handoff is written by an agent session under a bearer token, naming itself in actor")
		return
	}
	if !s.requireIssueV2(w, r, r.PathValue("key")) {
		return
	}
	input := v2Body[createHandoffV2Input](r)
	if !handoffPhasePattern.MatchString(input.Phase) {
		writeHandoffPhaseError(w, r, input.Phase)
		return
	}
	s.recordHandoffInput(w, r, actor, input.Phase, createHandoffInput{Actor: input.Actor, Body: input.Body})
}
