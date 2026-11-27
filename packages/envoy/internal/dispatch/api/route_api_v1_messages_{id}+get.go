package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/messages/{id}",
	Auth:        authAny,
	Description: "Read the conversation a message belongs to by any message id in it, issue-less or not: the thread root with its deliveries and every reply, oldest first. An issue thread follows the issue's rule. A direct (issue-less) thread is read whole by a credential holding the person scope, since the conversation is that person's own; every other credential names its session in ?session= and reads only one whose root targets that session or that it replied in (403 THREAD_FORBIDDEN), a guard against reading another session's conversation by mistake, not an authorization boundary. Direct-conversation text also reaches every authenticated caller through that caller's events view on GET /ws/live; nothing in Dispatch restricts it by session.",
	Handler:     (*server).getMessageThread,
	Successor:   successorIs(http.MethodGet, "/messages/{id}"),
	// A message, by any id in its conversation. The issue form names an owner, so a prefix of
	// it is looked for among that issue's messages alone; the owner-free forms look among
	// every message, a direct conversation's included. The read answers NOT_FOUND.
	ID:    &idRoute{Kind: "message", NotFound: apicode.NotFound},
	Read:  readEntry{Writes: readWritesNothing},
	Shape: routeShape{Status: http.StatusOK, Response: messageRead{}},
})
