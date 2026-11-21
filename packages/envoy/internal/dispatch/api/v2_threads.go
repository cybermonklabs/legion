package api

// v2_threads.go is the comments, messages, broadcasts and handoffs of /api/v2 (R1 of
// dispatch://INFRA-4071/spec): a comment and a message read flat, the item's own fields at top level
// with its replies beside them and its blocks with include=blocks (the route map's J4); an
// issue's comments, messages and handoffs, a session's conversations and the broadcasts, each an
// envelope; the state of a comment and of a message's delivery attempt, each a PUT of a body
// keyed by state; a session's replies to its deliveries as collections (J21); and the creates.
// Every write runs the work its /api/v1 route runs (writeComment, editCommentAs,
// reopenCommentAs, commentActionAs, retryCommentMention, answerCommentDelivery,
// postIssueMessage, sendAgentMessage, retryMessageDelivery, acceptDeliveryAs,
// answerMessageDelivery, sendBroadcast, recordHandoffInput), each path object checked first and
// answered with its own 404, and a {session_id} keeps the answer /api/v1 gives it (R7).

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

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

// messageThreadText is what every description of a route answering message threads says of the
// row it answers.
const messageThreadText = "Each thread is one row: its root message's own fields at top level, with the root's delivery " +
	"attempts in deliveries, and replies, every message replying to it, oldest first, each with its own attempts."

// v2ThreadRows are the comments, messages, broadcasts and handoffs: rows 48 and 51; 49;
// 52; 55; 50, 53, 54 and 56; 98; 99; 109, 137 and 138; 139; 140; 141; 107; 108; 7; 8; 44; 45;
// 46; 78; 104 and 106; and 105 of the route map.
func (s *server) v2ThreadRows() []routeRow {
	commentIDs := func() *idRoute { return &idRoute{Kind: "comment", NotFound: apicode.CommentNotFound} }
	messageIDs := func() *idRoute { return &idRoute{Kind: "message", NotFound: apicode.MessageNotFound} }
	person := routeScopeSet{model.ScopePerson}
	// handoffList is the List of an issue's handoffs: no bounds of its own, so 50 and 200
	// (listBounds.limits), which its description words its page from.
	handoffList := listBounds{}
	return []routeRow{
		{
			Method: http.MethodGet, Pattern: "/comments/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Read one comment: its own fields at top level, with its mentions, its delivery attempts and its " +
				"anchor's block position, and replies, every comment replying to it, oldest first (a reply or an ask thread's " +
				"comment has none of its own; an ask's thread is read on GET /api/v2/asks/{id}?include=thread). include=blocks " +
				"adds blocks, the comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, " +
				"ref, from, to, changed_revision, changed_at, embedding}]}, from and to UTF-16 code units into its stored body; " +
				"a comment owed a block index answers indexed false and no blocks, and the read writes nothing. An id written " +
				"short - 8 characters or more of the hex digits and hyphens an id starts with - names the one comment that " +
				"carries it (409 ID_AMBIGUOUS where two or more do). 404 COMMENT_NOT_FOUND for an id no comment has.",
			Handler:  s.getCommentV2,
			Resource: "comments", Kind: kindItem, Key: "{id}",
			Query:    commentReadV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: commentReadV2{}},
			IDs:      commentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
		},
		{
			Method: http.MethodPatch, Pattern: "/comments/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Edit a comment's body {body, base_revision?, blocks?}, which needs the person scope and is the " +
				"comment's author's alone (403 NOT_AUTHOR otherwise). The dashboard editor also sends blocks ([{id, from}], " +
				"from in UTF-16 code units into the new body) and base_revision, the revision GET /api/v2/comments/{id}?" +
				"include=blocks gave, so each block keeps its id; without them the ids follow the text, and blocks without " +
				"base_revision is 400 INVALID_COMMENT. Answers the comment and appends comment.edited. 400 INVALID_COMMENT for " +
				"an empty body, 400 CAP_EXCEEDED past a bound, 409 ISSUE_CLOSED on a closed issue, 404 COMMENT_NOT_FOUND.",
			Handler:  s.editCommentV2,
			Resource: "comments", Kind: kindItem, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: editCommentInput{}},
			Shape:       routeShape{Status: http.StatusOK, Response: shapedComment{}},
			OAuthScopes: person,
			Scope:       scopeProject, ProjectOf: projectPathComment,
			IDs:      commentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/comments/{id}/deliveries", Auth: authAny, Versions: onlyV2,
			Description: "Retry delivering one of a comment's mentions {delivery, target?}: a new attempt in the mode delivery " +
				"names (btw, aside or steer; 400 MENTION_INPUT otherwise) to the target, one of the comment's mentions, which " +
				"a comment of several mentions must name (400 MENTION_INPUT). A bearer names its session in actor. Answers 201 " +
				"the attempt as its send settled; a comment with no mentions is 404 COMMENT_NOT_FOUND, as is an id no comment " +
				"has.",
			Handler:  s.createCommentDeliveryV2,
			Resource: "deliveries", Kind: kindCollection, Key: "{attempt}",
			Body:  requestBody{Kind: bodyJSON, Type: createCommentDeliveryInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: model.CommentDelivery{}},
			Scope: scopeProject, ProjectOf: projectPathComment,
			IDs:      commentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/comments/{id}/replies", Auth: authBearer, Versions: onlyV2,
			Description: "The mentioned session's answer to one delivery attempt of a comment {actor, attempt, target?, " +
				"body | error, reply_to_block?}, under a bearer credential (a cookie caller is 403 REPLY_FORBIDDEN): the " +
				"session names itself in actor, and the attempt, of the mention target names (required where the comment has " +
				"several mentions, 400 MENTION_INPUT), must be that session's own (403 REPLY_FORBIDDEN, as is a body naming " +
				"neither or both of body and error). For a mention of a role (role:<name>), the role's live holder, as the " +
				"Envoy listener names it, may answer the attempt too; a listener that does not answer is 503 " +
				"ENVOY_UNAVAILABLE, and a failed read of the mention is 500, never a refusal. A body is stored as a reply in " +
				"the comment's thread, its reply_to_block naming one block of the comment it answers, and marks the attempt " +
				"sent; answers 201 the reply with ask_waiting_on, and appends comment.answered. An attempt already answered " +
				"posts nothing and answers 200 the stored reply. An error marks the attempt failed and answers 200 the " +
				"attempt. 404 COMMENT_NOT_FOUND for an id no comment has, or an attempt the mention does not have.",
			Handler:  s.replyCommentV2,
			Resource: "replies", Kind: kindCollection, Key: "{id}",
			Body: requestBody{Kind: bodyJSON, Type: commentReplyInput{}},
			Shape: routeShape{
				Status: http.StatusCreated, Response: commentWriteResponse{},
				Also: []routeAnswer{
					{Status: http.StatusOK, Body: model.Comment{}},
					{Status: http.StatusOK, Body: model.CommentDelivery{}},
				},
			},
			Scope: scopeProject, ProjectOf: projectPathComment,
			IDs:      commentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
		},
		{
			Method: http.MethodPut, Pattern: "/comments/{id}/state", Auth: authAny, Versions: onlyV2,
			Description: "Move a comment's thread to a state, its body by state. {state: resolved} resolves it, for any " +
				"caller, a bearer naming its session in actor; it is also how a suggestion that can no longer be accepted or " +
				"rejected is closed. {state: open} reopens a resolved thread root, which needs the person scope (a resolved " +
				"reply or ask thread's comment is 400 INVALID_COMMENT). {state: accepted} accepts a suggestion, which needs " +
				"the person scope: its replacement is applied to the live document and a version is named after the comment, " +
				"and the answer carries lost, whether the document already lacks the accepted text (null when the check " +
				"reached no verdict); an unanchored suggestion is 400 INVALID_SUGGESTION, and an anchor the document no longer " +
				"holds is recorded orphaned and refused. {state: rejected} rejects a suggestion, which needs the person scope, " +
				"and removes its mark. Accepting or rejecting a comment with no suggestion is 400 INVALID_SUGGESTION. A " +
				"suggestion already accepted or rejected is a thread resolved by whoever decided it: accepting, rejecting or " +
				"resolving it is 409 ALREADY_ACTIONED, but for the body equal to its state from the person who decided it. A " +
				"body equal to the comment's present state, from the caller who set it, answers 200 and writes nothing (an " +
				"accept's answer with lost null), and {state: resolved} from the person who accepted or rejected a suggestion " +
				"is such a body; an open thread records no setter, so a body of open on an open thread writes nothing whoever " +
				"sends it. Answers the comment; 400 STATE_INVALID for any other state, 409 ISSUE_CLOSED on a closed issue, 404 " +
				"COMMENT_NOT_FOUND.",
			Handler:  s.putCommentStateV2,
			Resource: "state", Kind: kindState,
			Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
				"open":     commentStateInput{},
				"resolved": resolveCommentStateInput{},
				"accepted": commentStateInput{},
				"rejected": commentStateInput{},
			}},
			Shape: routeShape{
				Status: http.StatusOK, Response: shapedComment{},
				Also: []routeAnswer{{Status: http.StatusOK, Body: acceptedComment{}}},
			},
			Authorities: []authorityRule{commentOpenAuthority, commentAcceptedAuthority, commentRejectedAuthority},
			Scope:       scopeProject, ProjectOf: projectPathComment,
			IDs:      commentIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/comments", Auth: authAny, Versions: onlyV2,
			Description: "List an issue's comments, oldest first: its own, those anchored in its documents, every reply and " +
				"every ask thread's comment, each as GET /api/v2/comments/{id} answers its fields, without replies. " +
				"artifact=<document id> keeps the comments anchored in one document of the issue, which are its root comments " +
				"and suggestions alone, since a reply carries no anchor. limit 1 to 200 (default 50) and offset; {items, total, " +
				"limit, offset}. The answer carries an ETag, which moves for every write the list shows, a delivery attempt " +
				"that appends no event included, and for every page; If-None-Match answers 304. A comment's author carries " +
				"proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and " +
				"no proven when the request only named it. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueCommentsV2,
			Resource: "comments", Kind: kindCollection, Key: "{id}",
			Query: issueCommentsV2Query{},
			Shape: routeShape{
				Status: http.StatusOK, Response: page[shapedComment]{},
				Also: []routeAnswer{{Status: http.StatusNotModified}},
			},
			Refusals: listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/comments", Auth: authAny, Versions: onlyV2,
			Description: "Comment on an issue, a quote of one of its documents, or an ask {body, anchor?, reply_to?, " +
				"reply_to_block?, ask_id?, mentions?, delivery?, turn?, suggestion?}, a bearer naming its session in actor: a " +
				"reply names the comment it answers in reply_to and one block of it in reply_to_block; with ask_id in place of " +
				"reply_to, reply_to_block names one part of that ask. mentions [{target}] (at most 10, role:<name> or " +
				"session:<id>) deliver the comment in delivery's mode (btw, aside or steer, the default), and a suggestion " +
				"{replace_with} proposes text for its anchored quote. A reply to a resolved thread reopens it, unless the root " +
				"is a decided suggestion. A closed issue takes a comment but refuses a suggestion, 409 ISSUE_CLOSED. Answers " +
				"201 the comment with ask_waiting_on and advice beside it; a repeat under its Idempotency-Key answers the " +
				"comment as it stands, its owed mentions sent again. 400 INVALID_COMMENT, MENTION_INPUT or TURN_REQUIRES_ASK " +
				"for a comment it refuses, 400 CAP_EXCEEDED past a bound, 409 ROUTE_CHANGED when the issue's route moves under " +
				"the write, 404 ISSUE_NOT_FOUND.",
			Handler:  s.createIssueCommentV2,
			Resource: "comments", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: commentInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: commentWriteResponse{}, Advice: writeAdvice{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Replay:   replayRecovered(s, s.replayComment),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/messages/{id}", Auth: authAny, Versions: onlyV2,
			Description: "Read the conversation a message belongs to, by the id of any message in it, issue-less or not: " +
				"the thread root's own fields at top level with its delivery attempts, and replies, every message replying to " +
				"it, oldest first, each with its own attempts. include=blocks adds blocks, the current blocks of the message " +
				"the id names, in body order: {item, indexed, blocks: [{id, type, text, ref, from, to, embedding}]}, from and " +
				"to UTF-16 code units into its stored body, ref absent for a direct message's block; a message owed a block " +
				"index answers indexed false and no blocks. An issue thread is read by any caller. A direct (issue-less) " +
				"thread is read whole by a credential holding the person scope, since the conversation is that person's own; " +
				"every other credential names its session in session= and reads only one whose root targets that session or " +
				"that it replied in (400 SESSION_REQUIRED, 403 THREAD_FORBIDDEN), a guard against reading another session's " +
				"conversation by mistake, not an authorization boundary. An id written short - 8 characters or more of the " +
				"hex digits and hyphens an id starts with - names the one message that carries it (409 ID_AMBIGUOUS where two " +
				"or more do). 404 MESSAGE_NOT_FOUND for an id no message has.",
			Handler:  s.getMessageV2,
			Resource: "messages", Kind: kindItem, Key: "{id}",
			Query:    messageReadV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: messageReadV2{}},
			IDs:      messageIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, messageMissingRefusal},
		},
		{
			Method: http.MethodPost, Pattern: "/messages/{id}/deliveries", Auth: authAny, Versions: onlyV2,
			Description: "Retry delivering a targeted message {delivery}: btw, aside or steer (400 MESSAGE_INPUT otherwise); a " +
				"bearer names its session in actor. It resumes the attempt a stopped send left owed in the same mode, carrying " +
				"that attempt's urgency, and otherwise opens an attempt of its own, carrying none. Answers 201 the attempt as " +
				"its send settled. A message that targets nobody is 404 MESSAGE_NOT_FOUND, as is an id no message has.",
			Handler:  s.createMessageDeliveryV2,
			Resource: "deliveries", Kind: kindCollection, Key: "{attempt}",
			Body:  requestBody{Kind: bodyJSON, Type: createDeliveryInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: model.MessageDelivery{}},
			Scope: scopeProject, ProjectOf: projectPathMessage,
			IDs:      messageIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, messageMissingRefusal},
		},
		{
			Method: http.MethodPut, Pattern: "/messages/{id}/deliveries/{attempt}/state", Auth: authBearer, Versions: onlyV2,
			Description: "The attempt's session records that it took the message as its user's own turn {state: accepted, " +
				"actor}, under a bearer credential (a cookie caller is 403 ACCEPT_FORBIDDEN): the session names itself in " +
				"actor, and another session's attempt is 403 ACCEPT_FORBIDDEN. One compare-and-set: only a direct message to " +
				"that session, on no issue, neither a broadcast's copy nor a reply in a broadcast's thread, in a thread whose " +
				"root targets it (409 ACCEPT_NOT_DIRECT), that a person wrote (409 ACCEPT_NOT_WRITTEN_BY_PERSON) themselves, " +
				"not an agent for them (409 ACCEPT_WRITTEN_BY_AGENT), sent as a Send or an Aside (409 " +
				"ACCEPT_NOT_ASIDE_OR_STEER); only the message's latest attempt (409 ACCEPT_SUPERSEDED), when no attempt of it " +
				"was accepted before (409 ACCEPT_ALREADY_ACCEPTED), which a person requested (409 " +
				"ACCEPT_NOT_REQUESTED_BY_PERSON), that person being the message's author (409 " +
				"ACCEPT_NOT_REQUESTED_BY_AUTHOR), which did not fail (409 ACCEPT_FAILED; a pending, sent or queued attempt is " +
				"taken), within the last minute (409 ACCEPT_STALE). Answers the attempt with accepted_at and accepted_as and " +
				"the message's stored body; leaves the attempt's state to the send, and appends message.accepted. The same " +
				"session accepting an attempt it already accepted answers 200 the same and writes nothing. 400 STATE_INVALID " +
				"for any other state, 404 MESSAGE_NOT_FOUND, 404 DELIVERY_NOT_FOUND for an attempt the message does not have.",
			Handler:  s.putDeliveryStateV2,
			Resource: "state", Kind: kindState,
			Body: requestBody{Kind: bodyStateUnion, Arms: map[string]any{
				"accepted": acceptDeliveryStateInput{},
			}},
			Shape: routeShape{Status: http.StatusOK, Response: acceptedDelivery{}},
			Scope: scopeSession,
			IDs:   messageIDs(),
			Refusals: []apicode.Refusal{
				undeclaredKeyRefusal, messageMissingRefusal, deliveryMissingRefusal,
			},
		},
		{
			Method: http.MethodPost, Pattern: "/messages/{id}/replies", Auth: authBearer, Versions: onlyV2,
			Description: "The targeted session's answer to one delivery attempt of a message {actor, attempt, body | error, " +
				"in_reply_to_block?}, under a bearer credential (a cookie caller is 403 REPLY_FORBIDDEN): the session names " +
				"itself in actor, and the attempt must be its own (403 REPLY_FORBIDDEN, as is a body naming neither or both of " +
				"body and error). For an attempt sent for a route to a role (role:<name>), the role's live holder, as the " +
				"Envoy listener names it, may answer it too; a listener that does not answer is 503 ENVOY_UNAVAILABLE, and a " +
				"failed read of the attempt is 500, never a refusal. A body is posted as the session's reply in the message's " +
				"conversation, its in_reply_to_block naming one block of the message it answers, and marks the attempt sent; " +
				"answers 201 the reply and appends message.answered. Once the attempt is answered, follow_up=true posts other " +
				"text as the session's follow-up, threaded under its first reply (201); with follow_up=false (the default), or " +
				"with text the session already posted there, nothing is posted and the stored message answers 200 marked " +
				"duplicate. An error marks the attempt failed and answers 200 the attempt. 404 MESSAGE_NOT_FOUND for an id no " +
				"message has, or an attempt it does not have.",
			Handler:  s.replyMessageV2,
			Resource: "replies", Kind: kindCollection, Key: "{id}",
			Query: messageRepliesV2Query{},
			Body:  requestBody{Kind: bodyJSON, Type: messageReplyInput{}},
			Shape: routeShape{
				Status: http.StatusCreated, Response: replyRead{},
				Also: []routeAnswer{
					{Status: http.StatusOK, Body: replyRead{}},
					{Status: http.StatusOK, Body: model.MessageDelivery{}},
				},
			},
			Scope:    scopeSession,
			IDs:      messageIDs(),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, messageMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/messages", Auth: authAny, Versions: onlyV2,
			Description: "List an issue's message threads, oldest first: every root message of the issue, a targeted one " +
				"listed like any other. " + messageThreadText + " limit 1 to 200 (default 50) and offset; {items, total, " +
				"limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
			Handler:  s.listIssueMessagesV2,
			Resource: "messages", Kind: kindCollection, Key: "{id}",
			Query:    issueMessagesV2Query{},
			Shape:    routeShape{Status: http.StatusOK, Response: page[messageThreadV2]{}},
			Refusals: listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/messages", Auth: authAny, Versions: onlyV2,
			Description: "Post a message on an issue {body, in_reply_to?, in_reply_to_block?, target?, delivery?, urgency?}, " +
				"a bearer naming its session in actor: a reply names the message of this issue it answers in in_reply_to and " +
				"one block of it in in_reply_to_block; target (role:<name> or session:<id>) with delivery (btw, aside or " +
				"steer) delivers it, and a person's reply that names no target is delivered as its thread was; urgency is " +
				"low, med, high or blocking. A closed issue takes it. Answers 201 the message once a targeted one's first " +
				"attempt has settled, with advice beside it; a repeat under its Idempotency-Key answers the message as it " +
				"stands, its owed attempt sent first. 400 INVALID_MESSAGE or MESSAGE_INPUT for a message it refuses, 404 " +
				"ISSUE_NOT_FOUND.",
			Handler:  s.createIssueMessageV2,
			Resource: "messages", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: createMessageInput{}},
			Shape: routeShape{Status: http.StatusCreated, Response: shapedMessage{}, Advice: writeAdvice{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			Replay:   replayRecovered(s, s.replayIssueMessage),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/agents/{session_id}/messages", Auth: authAny, Versions: onlyV2,
			Description: "A session's conversations, which need the person scope, the one that moved last first: every root " +
				"message targeted at the session or one of whose attempts was delivered to it, issue messages, role messages " +
				"it held and broadcast copies among them. " + messageThreadText + " unread is whether the thread holds a " +
				"reply the caller has not read, and unread=true keeps only those. limit 1 to 200 (default 50) and offset; " +
				"{items, total, limit, offset}. A session Dispatch holds nothing for answers an empty page.",
			Handler:  s.listAgentMessagesV2,
			Resource: "messages", Kind: kindCollection, Key: "{id}",
			Query:       agentMessagesV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[agentMessageV2]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/agents/{session_id}/messages", Auth: authAny, Versions: onlyV2,
			Description: "Send an issue-less targeted message to a session {body, delivery, in_reply_to?, in_reply_to_block?, " +
				"connected_only?}, which needs the person scope: delivery is btw, aside or steer, and a reply names a message " +
				"of this session's conversation in in_reply_to (400 MESSAGE_INPUT otherwise). It is answered 201 once its own " +
				"attempt has settled. A session the Envoy listener knows and that is not running is a recipient: the listener " +
				"stores the message and hands it over when that session runs again, and the attempt is recorded queued; one " +
				"the listener knows nothing about fails the attempt with \"no live session <id>\". connected_only asks for a " +
				"session running now and fails the attempt with \"the session is stopped\", nothing stored, as a person's own " +
				"Send or Aside whose whole body is one /model or /effort line always does. A repeat under its Idempotency-Key " +
				"answers the message as it stands, its owed attempt sent first. 400 INVALID_MESSAGE or MESSAGE_INPUT for a " +
				"message it refuses.",
			Handler:  s.createAgentMessageV2,
			Resource: "messages", Kind: kindCollection, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: createAgentMessageInput{}},
			Shape:       routeShape{Status: http.StatusCreated, Response: shapedMessage{}},
			OAuthScopes: person,
			Scope:       scopeSession, SessionPath: "session_id",
			Replay:   replayRecovered(s, s.replayAgentMessage),
			Refusals: []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/broadcasts", Auth: authAny, Versions: onlyV2,
			Description: "The broadcasts, which need the person scope, newest first, each {id, author, body, delivery, " +
				"created_at} with recipients, how many sessions it reached, and replies, how many of them answered. limit 1 " +
				"to 200 (default 50) and offset; {items, total, limit, offset}.",
			Handler:  s.listBroadcastsV2,
			Resource: "broadcasts", Kind: kindCollection, Key: "{id}",
			Query:       broadcastsV2Query{},
			Shape:       routeShape{Status: http.StatusOK, Response: page[broadcastSummary]{}},
			OAuthScopes: person,
			Refusals:    listRefusals(),
		},
		{
			Method: http.MethodPost, Pattern: "/broadcasts", Auth: authAny, Versions: onlyV2,
			Description: "Send one message to many sessions {body, delivery, session_ids}, which needs the person scope: one " +
				"targeted message per recipient under a shared broadcast, answered 201 once it is committed while each " +
				"recipient's delivery runs behind the answer. A selected session the Envoy listener knows and that is not " +
				"running is a recipient like any other, its copy stored for it; one the listener knows nothing about, or that " +
				"does not advertise the mode, is excluded and named in excluded, never switched to another mode, and a send " +
				"none of whose sessions can take it is 400 BROADCAST_EMPTY. A repeat under its Idempotency-Key answers 201 the " +
				"broadcast the key made, every recipient's attempts as they stand and the first answer's exclusions. 400 " +
				"BROADCAST_INPUT or INVALID_MESSAGE for a send it refuses, 503 ENVOY_UNAVAILABLE without a listener.",
			Handler:  s.createBroadcastV2,
			Resource: "broadcasts", Kind: kindCollection, Key: "{id}",
			Body:        requestBody{Kind: bodyJSON, Type: createBroadcastV2Input{}},
			Shape:       routeShape{Status: http.StatusCreated, Response: broadcastCreated{}},
			OAuthScopes: person,
			Scope:       scopeDeployment,
			Replay:      replayRecovered(s, s.replayBroadcast),
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/broadcasts/{id}", Auth: authAny, Versions: onlyV2,
			Description: "One broadcast, which needs the person scope, with every recipient {session_id, message, replies}: " +
				"the copy it was sent with its delivery attempts, and the replies threaded under it, recipients in the order " +
				"the send named them (a broadcast from before that order was stored falls back to created_at, id). 404 " +
				"BROADCAST_NOT_FOUND for an id no broadcast has.",
			Handler:  s.getBroadcast,
			Resource: "broadcasts", Kind: kindItem, Key: "{id}",
			Shape:       routeShape{Status: http.StatusOK, Response: broadcastRead{}},
			OAuthScopes: person,
			IDs:         &idRoute{Reason: "a broadcast id: one send to many sessions"},
			Refusals:    []apicode.Refusal{undeclaredKeyRefusal, broadcastMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/handoffs/{id}", Auth: authAny, Versions: onlyV2,
			Description: "One handoff by id with its body, the exact text written: {id, issue_key, phase, version, author, " +
				"created_at, body}; a closed issue's too. 404 HANDOFF_NOT_FOUND for an id no handoff has.",
			Handler:  s.getHandoffV2,
			Resource: "handoffs", Kind: kindItem, Key: "{id}",
			Shape:    routeShape{Status: http.StatusOK, Response: model.Handoff{}},
			IDs:      &idRoute{Reason: "a handoff id: one phase's record on an issue"},
			Refusals: []apicode.Refusal{undeclaredKeyRefusal, handoffMissingRefusal},
		},
		{
			Method: http.MethodGet, Pattern: "/issues/{key}/handoffs", Auth: authAny, Versions: onlyV2,
			Description: fmt.Sprintf("List an issue's handoffs, each {id, issue_key, phase, version, author, created_at}, a "+
				"closed issue's too. phase=<phase> keeps one phase's (a lowercase slug, %s, 400 HANDOFF_INPUT otherwise). "+
				"include=body adds each handoff's body, the exact text written. sort=-created_at (the default: newest first, "+
				"then by phase, then the newest version), created_at, version or -version (a phase's newest version first: "+
				"phase=<phase>&sort=-version&limit=1&include=body reads the phase's latest handoff). %s {items, total, "+
				"limit, offset}. 404 ISSUE_NOT_FOUND for an issue that does not exist.",
				contracts.HandoffPhasePattern, includedPageLimitText(handoffList, issueHandoffsV2Query{})),
			Handler:  s.listIssueHandoffsV2,
			Resource: "handoffs", Kind: kindCollection, Key: "{id}",
			Query:    issueHandoffsV2Query{},
			List:     handoffList,
			Shape:    routeShape{Status: http.StatusOK, Response: page[handoffV2]{}},
			Refusals: listRefusals(issueMissingRefusal),
		},
		{
			Method: http.MethodPost, Pattern: "/issues/{key}/handoffs", Auth: authBearer, Versions: onlyV2,
			Description: fmt.Sprintf("Record a session's handoff for one phase of an issue {actor, phase, body}, under a "+
				"bearer credential (a cookie caller is 403 HANDOFF_FORBIDDEN): the session names itself in actor (400 "+
				"ACTOR_KIND otherwise), phase is a lowercase slug (%s, 400 HANDOFF_INPUT otherwise), and body is one JSON "+
				"object as a string (400 HANDOFF_INPUT otherwise) of at most %d bytes (413 CAP_EXCEEDED), stored exactly as "+
				"sent. The record is the phase's next version, from 1; a closed issue is 409 ISSUE_CLOSED. Answers 201 {id, "+
				"issue_key, phase, version, author, created_at} and appends handoff.recorded {handoff_id, phase, version}, "+
				"which never carries the body. 404 ISSUE_NOT_FOUND.", contracts.HandoffPhasePattern, contracts.DispatchHandoffBodyMax),
			Handler:  s.createIssueHandoffV2,
			Resource: "handoffs", Kind: kindCollection, Key: "{id}",
			Body:  requestBody{Kind: bodyJSON, Type: createHandoffV2Input{}},
			Shape: routeShape{Status: http.StatusCreated, Response: model.HandoffSummary{}},
			Scope: scopeProject, ProjectOf: projectPathIssue,
			BodyLimit: handoffBodyLimit,
			Refusals:  []apicode.Refusal{undeclaredKeyRefusal, issueMissingRefusal},
		},
	}
}

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
// include=body, which GET /api/v1's list never carried.
type handoffV2 struct {
	model.HandoffSummary
	// Body is the handoff's body, the exact text written, with include=body.
	Body *string `json:"body,omitempty"`
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
	read, err := s.readCommentThread(r.Context(), id, wantsDisplayTrees(r))
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	answer := commentReadV2{shapedComment: read.Comment, Replies: read.Replies}
	if slices.Contains(v2Query[commentReadV2Query](r).Include, "blocks") {
		blocks, err := s.itemBlocks(r.Context(), refs.ItemComment, id)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.Blocks = &blocks
	}
	WriteJSON(w, http.StatusOK, answer)
}

// editCommentV2 answers PATCH /api/v2/comments/{id}: the edit PATCH /api/v1/comments/{id} makes
// (editCommentAs), by the person the row's scope holds the caller to.
func (s *server) editCommentV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) || !s.requireCommentV2(w, r, r.PathValue("id")) {
		return
	}
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	s.editCommentAs(w, r, actor, v2Body[editCommentInput](r))
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
// issue, the artifact= scope, the list's watermark and the issue's event sequence, and the page,
// under a line naming the version, so no validator one version gave matches the other's answer.
func (s *server) issueCommentsV2ETag(key, artifactFilter string, watermark, lastSeq int64, query pageQuery, trees bool) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "version=v2\nissue=%s\nfilter=%s\nwatermark=%d\nlast_seq=%d\nlimit=%d\noffset=%d\n",
		key, artifactFilter, watermark, lastSeq, query.Limit, query.Offset)
	return s.listETag(hash, trees)
}

// listIssueCommentsV2 answers GET /api/v2/issues/{key}/comments: one page of the comment list GET
// /api/v1/issues/{key}/comments answers (answerCommentList), its validator read first, under a
// validator of its own and kept under the /api/v2 route's own key (commentListV2Route), so a v1
// read and a v2 read of one issue's comments each answer their own version's body. The validator's
// own statement says whether the issue is there (commentListWatermark), as an issue's event log
// reads it (listIssueEventsV2), and only the page it answers is hydrated.
func (s *server) listIssueCommentsV2(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key := r.PathValue("key")
	query := v2Query[issueCommentsV2Query](r)
	artifact := strings.TrimSpace(query.Artifact)
	owner := issueOwner(key)
	watermark, lastSeq, ownerHeld, err := s.commentListWatermark(r.Context(), owner)
	if err != nil {
		s.writeHandlerError(w, r, err)
		return
	}
	if !ownerHeld {
		s.writeHandlerError(w, r, errorf(apicode.IssueNotFound, "no issue %s", key))
		return
	}
	trees := wantsDisplayTrees(r)
	etag := s.issueCommentsV2ETag(key, artifact, watermark, lastSeq, query.pageQuery, trees)
	if answerNotModified(w, r, etag) {
		return
	}
	kept := listAnswerKeyFor(r, commentListV2Route, key, etag)
	if s.listAnswers.answer(w, kept) {
		return
	}
	listed, err := s.commentPageV2(r.Context(), owner, artifact, query.pageQuery)
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
	s.writeListAnswer(w, kept, encoded, ownerHeld)
}

// commentPageV2 is the page query asks for of owner's comments, oldest first, artifactFilter
// keeping one document's where it names one: every comment's row read, and the side tables of
// the page's alone (hydrateCommentSideTables), which reads each comment by itself.
func (s *server) commentPageV2(ctx context.Context, owner owner, artifactFilter string, query pageQuery) (page[model.Comment], error) {
	comments, err := readOwnerComments(ctx, s.deps.Store.Pool, owner, artifactFilter)
	if err != nil {
		return page[model.Comment]{}, err
	}
	listed := pageOf(comments, query)
	if listed.Items, err = s.hydrateCommentSideTables(ctx, s.deps.Store.Pool, listed.Items); err != nil {
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
		blocks, err := s.itemBlocks(r.Context(), refs.ItemMessage, id)
		if err != nil {
			s.writeHandlerError(w, r, err)
			return
		}
		answer.Blocks = &blocks
	}
	WriteJSON(w, http.StatusOK, answer)
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
// such attempt - number is no whole number of 1 or more, or names none - it writes 404
// DELIVERY_NOT_FOUND (dispatch://INFRA-4071/spec R7) and answers false.
func (s *server) deliveryAttemptV2(w http.ResponseWriter, r *http.Request, messageID, number string) (model.MessageDelivery, bool) {
	missing := errorf(apicode.DeliveryNotFound, "message %s has no delivery attempt %s", messageID, number)
	attempt, err := strconv.Atoi(number)
	if err != nil || attempt < 1 {
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
	reads, err := s.hydrateMessageThreads(r.Context(), listed.Items, wantsDisplayTrees(r))
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
	reads, err := s.readAgentConversations(r.Context(), agentConversationRootsCTE+`
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
// /api/v1/issues/{key}/handoffs/{phase}/latest (the route map's row 106). A page with the bodies
// holds at most includedPageMax handoffs, each body up to contracts.DispatchHandoffBodyMax, which
// the decoder holds before the handler runs (issueHandoffsV2Query's renders tag).
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
