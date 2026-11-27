package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
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
		Query: messageReadV2Query{},
		Shape: routeShape{Status: http.StatusOK, Response: messageReadV2{}},
		IDs:   v2MessageIDs(),
		Refusals: []apicode.Refusal{
			undeclaredKeyRefusal, messageMissingRefusal, directThreadNoSessionRefusal, directThreadOutsideRefusal,
		},
	}
})
