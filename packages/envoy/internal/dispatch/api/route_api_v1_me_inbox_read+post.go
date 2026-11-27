package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodPost,
	Pattern:     "/api/v1/me/inbox/read",
	Auth:        authAny,
	Description: "Mark every unread Inbox thread read in one write: {read_through} (RFC3339, required; stored as no later than now; missing, unparsable or more than a minute ahead of the server is 400 INVALID_STATE), with the Inbox's own narrowing - kind (reply, agent; both by default), priority (P0, P1, P2, P3 and none), urgency (low, med, high, blocking and none), project (a project key and none), and issue or artifact - each absent list meaning every value. It marks exactly the threads GET /api/v1/me/replies?state=unread answers under that narrowing whose newest reply is at or before read_through: a thread written in since stays unread, and one already read past the mark keeps its later mark. Marking read takes no thread out of the Inbox; POST /api/v1/me/inbox/dismiss does. An Agent row has no issue, document, project, priority or urgency, so any of those narrowings leaves it out; a Reply row has no urgency either, so an urgency list without none marks nothing. An agent reply is marked read by its id, so no session's own read mark moves and the live conversation keeps what it had. issue with artifact, an unknown kind, priority or urgency value, and an artifact that is not a document id are 400 INVALID_QUERY. Answers {marked}, the number of threads marked; a request that moves nothing answers 0 and appends no event.",
	Handler:     (*server).markInboxRead,
	Successor:   successorIs(http.MethodPatch, "/me/marks"),
	WriteClass:  scopeSession,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Shape: routeShape{
		Status: http.StatusOK, Response: inboxReadResponse{}, Request: inboxReadRequest{},
	},
})
