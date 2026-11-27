package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/me/inbox/dismiss",
	Auth:        authAny,
	Description: "Take threads out of the caller's Inbox, or put them back: {threads} (the row ids reply:<thread id> and agent:<root message id>) with {seen_replies}, or the Inbox's own narrowing - kind, priority, urgency, project, and issue or artifact, as POST /api/v1/me/inbox/read reads them - with {dismiss_through} (RFC3339, required with the narrowing; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), never both (400 INVALID_QUERY). seen_replies is what the page drew in each row threads names, keyed by that same row id, and a dismissal naming threads without it is 400 INVALID_QUERY: a reply: row's entry is {version}, that row's version as GET /api/v1/me/replies answered it, and an agent: row's is {newest_reply, unread_replies}, that row's reply.id and the ids it drew as unread. The mark is written only while the row still holds what the page drew, so a dismissal buries no write nobody has read. A restore takes no seen_replies, and neither does the narrowing, where the server draws the rows itself (each 400 INVALID_QUERY). With the narrowing it changes exactly the threads GET /api/v1/me/replies answers under it whose newest reply, or whose dismissal, is at or before dismiss_through. {restore: true} clears the marks instead of writing them, so the threads are in the Inbox again. Dismissing marks a thread read as well, so a dismissed thread never counts as unread, and a later write by somebody else clears the mark and brings the thread back unread, while the caller's own reply leaves it dismissed. An Ask row's id, a row id of neither shape and an id that is not a uuid are 400 INVALID_QUERY. Answers {dismissed}, the number of threads it changed; a request that changes nothing answers 0 and appends no event. It takes the lock order and appends the events POST /api/v1/me/inbox/read does: one user_thread_state.updated per project and one user_agent_state.updated per session it touched.",
	Handler:     (*server).dismissInbox,
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: inboxDismissResponse{}, Request: inboxDismissRequest{},
	},
})
