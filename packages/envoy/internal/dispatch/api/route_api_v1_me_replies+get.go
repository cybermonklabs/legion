package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// kept for one release: inbox-old-reads
var _ = declareRoute(routeDecl{
	Method:      http.MethodGet,
	Pattern:     "/api/v1/me/replies",
	Auth:        authAny,
	Description: "The caller's Inbox threads, of the kinds ?kind= names: reply (the default) lists the threads they wrote in - an ask, a comment thread, an issue message thread - that somebody else has written in, less an open ask on an open issue that waits on them, which the Inbox already lists; agent lists their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or Aside the session did not take as a turn of its live conversation) holding a reply from that session; reply,agent lists both in one order. ?state= is unread (the default), which lists the rows somebody else has written in since they read them, newest reply first; inbox, every row in their Inbox, read or not, dismissed ones aside, newest reply first; or dismissed, what they have dismissed out of it, newest dismissal first, 200 rows at a time from ?cursor= (the next_cursor of the page before it; limit and offset are 400 INVALID_QUERY there, and cursor is 400 INVALID_QUERY under any other state). Each row names the thread, its owner, its issue's priority, its project, the thread's subject, its root's deep link (an agent row's is the session's live view), the newest reply's author, first line and time, the caller's read mark, whether it is unread and how many writes in it they have not read, the root's document anchor when it has one, its dismissal mark when it has been dismissed, and, on an agent row, the session and the ids of its unread replies. ?issue= or ?artifact= narrows to one owner (both is 400 INVALID_QUERY), and an agent row, which has neither, is then in no answer. An absent limit answers every row, as the Inbox reads them; a named limit is 1-200 (limit=0 is 400 INVALID_QUERY) from ?offset=. {rows, total (both kinds, and under state=dismissed the page the read answers rather than the whole pile, which nothing counts), limit (0 when none was named), offset, as_of, next_cursor (a dismissed page that filled)}: as_of is the database time of the one read-only repeatable-read snapshot every row was read in, which is what a mark-all passes back as its read mark.",
	Handler:     (*server).listMyReplies,
	OAuthScopes: routeScopeSet{model.ScopePerson},
	Read:        readEntry{Writes: readWritesNothing},
	Shape:       routeShape{Status: http.StatusOK, Response: model.MyRepliesResponse{}, Params: myRepliesQuery{}},
})
