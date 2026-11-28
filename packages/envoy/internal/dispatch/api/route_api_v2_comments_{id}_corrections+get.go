package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/comments/{id}/corrections", Auth: authAny, Versions: onlyV2,
		Description: "Every body an edit or a delete of this comment replaced, each with the actor that replaced it " +
			"and the time, beside the comment's own deleted_at and deleted_by, null on " +
			"a comment that stands. Read by whoever reads the comment, which is any authenticated caller. A comment " +
			"nobody edited or deleted answers no item; a delete writes an item of its own, so a comment deleted with no " +
			"edit lists one, and a deleted comment answers every body empty, since no read of a deleted post gives its " +
			"text. " + postCorrectionsText + " 404 COMMENT_NOT_FOUND for an id no comment has.",
		Handler:  s.listCommentCorrectionsV2,
		Resource: "corrections", Kind: kindLog, Position: correctionPosition,
		Query:    postCorrectionsV2Query{},
		List:     listBounds{Default: contracts.PostHistoryPageLimit, Max: contracts.PostHistoryPageLimit},
		Shape:    routeShape{Status: http.StatusOK, Response: postCorrectionsV2{}},
		IDs:      v2CommentIDs(),
		Refusals: listRefusals(positionRefusal, commentMissingRefusal),
	}
})
