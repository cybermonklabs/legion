package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/agents/{session_id}/answers", Auth: authAny, Versions: onlyV2,
		Description: "What people wrote that Dispatch delivered to one session, newest first by at, then by id: " +
			"each ask's current answer (replaced when it changed an earlier answer), each comment at its current body " +
			"(edited_at when edited; a resolved comment stays), each message at its current body (edited_at when " +
			"edited), a deleted comment or message in its place with deleted_at and deleted_by and an empty text, " +
			"and each ask a person declined to answer, once however it arrived, each naming its relation to the " +
			"session, its author, its words (text, " +
			"and on declined_your_ask alone dispatch_words, the sentences Dispatch wrote for the asking agent), the " +
			"ask and its whole question where it has one, whoever asked it, its owner, dispatch:// ref and href. A " +
			"session owns an ask when it wrote the ask or follows it, and only an owner is told that an ask is its " +
			"own: answered_your_ask, declined_your_ask and replied_on_your_ask (which outranks mentioned_you) for an " +
			"owner, answered_an_ask and declined_an_ask for any other session, which reads a reply in an ask's thread " +
			"by the reason that delivered it. It never lists an agent's or the system's words. from and to window " +
			"at, RFC3339, from inclusive and to exclusive: from defaults to 72 " +
			"hours ago, and one older than 7 days or in the future is 400 INVALID_QUERY, as is a to not after from. " +
			"limit 1 to 200 (default 50) and offset page the window; max_chars (1 to 100000) then stops the page " +
			"after the item whose words take the running sum past it, counting the UTF-16 units of text and of " +
			"dispatch_words, while total counts every item in the window. {session_id, from, to, items, total, " +
			"limit, offset}, to null where the request names none. A session_id that is not a session id is 400 " +
			"INVALID_SESSION_ID; a session Dispatch holds nothing for answers an empty page. Not an authorization " +
			"boundary: a person may read any session's list.",
		Handler:  s.listSessionAnswersV2,
		Resource: "answers", Kind: kindCollection, TimeFields: []string{"at"},
		Query:    sessionAnswersV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: sessionAnswersV2{}},
		Refusals: listRefusals(sessionIDMalformedRefusal),
	}
})
