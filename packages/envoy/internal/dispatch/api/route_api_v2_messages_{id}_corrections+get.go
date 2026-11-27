package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/messages/{id}/corrections", Auth: authAny, Versions: onlyV2,
		Description: "Every body an edit or a delete of this message replaced, each with the actor that replaced it " +
			"and the time, beside the message's own deleted_at and deleted_by, null on " +
			"a message that stands. Read under GET /api/v2/messages/{id}'s rule: a credential without the person " +
			"scope reading a direct message names its session in session= (400 SESSION_REQUIRED, 403 " +
			"THREAD_FORBIDDEN). A message nobody edited or deleted answers no item; a delete writes an item of its " +
			"own, so a message deleted with no edit lists one, and a deleted message answers every body empty, since " +
			"no read of a deleted post gives its text. " + postCorrectionsText + " 404 MESSAGE_NOT_FOUND for an id no " +
			"message has.",
		Handler:  s.listMessageCorrectionsV2,
		Resource: "corrections", Kind: kindLog, Position: correctionPosition,
		Query:    postCorrectionsV2Query{},
		List:     listBounds{Default: contracts.PostHistoryPageLimit, Max: contracts.PostHistoryPageLimit},
		Shape:    routeShape{Status: http.StatusOK, Response: postCorrectionsV2{}},
		IDs:      v2MessageIDs(),
		Refusals: listRefusals(positionRefusal, messageMissingRefusal, directThreadNoSessionRefusal, directThreadOutsideRefusal),
	}
})
