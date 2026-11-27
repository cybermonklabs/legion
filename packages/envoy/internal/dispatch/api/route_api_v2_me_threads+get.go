package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/me/threads", Auth: authAny, Versions: onlyV2,
		Description: "The caller's Inbox threads (the person scope), of the kinds kind=<kind>,<kind> names: reply (the " +
			"default) lists the threads they wrote in - an ask, a comment thread, an issue message thread - that somebody " +
			"else has written in, less an open ask on an open issue that waits on them, which GET /api/v2/asks?waiting_on=me " +
			"lists; agent lists their one-off messages to an agent session (a BTW, a broadcast's copy, or a Send or Aside the " +
			"session did not take as a turn of its live conversation) holding a reply from that session; reply,agent lists " +
			"both in one order. state is unread (the default), the rows somebody else has written in since they read them, " +
			"newest reply first; inbox, every row in their Inbox, read or not, dismissed ones aside, newest reply first; or " +
			"dismissed, what they have dismissed out of it, newest dismissal first. Position, of state=dismissed alone: " +
			"dismissed_at,thread_id, each bound <RFC3339 time>,<thread id>, the position of a row a page answered: " +
			"after keeps the rows dismissed after that row, oldest dismissal first, and before the rows dismissed before " +
			"it, newest first where no after is given (an after or a before that is no time and thread id, and an after " +
			"above the newest dismissal, are 400 POSITION_INVALID naming the parameter and dismissed_at,thread_id). " +
			"issue=<key> or artifact=<document id> narrows to one owner, and an agent row, which has neither, is then in " +
			"no answer; both, and an after or a before under a state other than dismissed, are 400 INVALID_QUERY. " +
			"limit 1 to 200 (default 50) and offset; {items, total, limit, offset, as_of}, total the exact " +
			"count of both kinds the filters keep, and null under state=dismissed, which is read a page at a time from a " +
			"position, as the global event log is, rather than counted; as_of the database time of the one read-only " +
			"repeatable-read snapshot " +
			"every row was read in, which is what a mark-all passes back as its read mark. Each row names the thread, its " +
			"owner, its issue's priority, its project, the thread's subject, its root's deep link (an agent row's is the " +
			"session's live view), the newest reply's author, first line and time, the caller's read mark, whether it is " +
			"unread and how many writes in it they have not read, the root's document anchor when it has one, its dismissal " +
			"mark when it has been dismissed, and, on an agent row, the session and the ids of its unread replies.",
		Handler:  s.listMyThreadsV2,
		Resource: "threads", Kind: kindLog, Position: dismissedPosition,
		Query:       myThreadsV2Query{},
		Shape:       routeShape{Status: http.StatusOK, Response: myThreadsV2{}},
		OAuthScopes: routeScopeSet{model.ScopePerson},
		Refusals:    listRefusals(positionRefusal),
	}
})
