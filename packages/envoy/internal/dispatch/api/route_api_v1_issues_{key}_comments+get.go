package api

import (
	"net/http"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/issues/{key}/comments",
	Auth:        authAny,
	Description: "List an issue's comments; ?artifact= scopes to a document. The answer carries an ETag, which moves for every write the list shows, a delivery attempt that appends no event included; If-None-Match answers 304. A comment's author carries proven: true when the write's credential proved it (a browser's person, a broker-issued token's session), and no proven when the request only named it. " + threadPageDescription("A comment thread is a comment with no reply_to and no ask_id, and each comment whose reply_to names it; its row is {comment, replies, last_activity_at}, the replies oldest first, each shaped as the list shapes it. A comment with an ask_id is an entry of its ask's thread on GET /api/v1/issues/{key}/asks, and of no comment thread.", "?person=in keeps each thread with an entry a person wrote, and each thread on text, whoever wrote it: its first comment has an anchor, or one of its comments is a reply that stores a quote; the route takes no other person value. With ?order=activity, ?artifact= keeps each thread whose first comment is anchored in that document, with all its replies, and the page's ETag hashes every parameter of the request, If-None-Match answering 304 as for the list."),
	Handler:     (*server).listComments,
	Read:        readEntry{Writes: readWritesNothing},
	Shape: routeShape{
		Status: http.StatusOK, Response: []shapedComment{}, Params: issueCommentsQuery{},
		Also: []routeAnswer{{Status: http.StatusOK, Body: commentThreadPage{}}},
	},
})
