package api

import (
	"net/http"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPatch, Pattern: "/comments/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Edit a comment's body {body, base_revision?, blocks?, actor?}: its author alone (403 " +
			"NOT_AUTHOR). A session names itself in actor where its credential proves none; a person's own " +
			"credential - a browser cookie, or a personal token holding the person scope - is that person and names " +
			"nothing. The body it replaces is kept and GET /api/v2/comments/{id}/corrections reads it. The dashboard " +
			"editor also sends blocks ([{id, from}], from in UTF-16 code units into the new body) and base_revision, " +
			"the revision GET /api/v2/comments/{id}?include=blocks gave, so each block keeps its id; without them the " +
			"ids follow the text. A comment of a closed issue is edited too, and a deleted one is 409 POST_DELETED. " +
			"Answers the comment and appends comment.edited. 400 INVALID_COMMENT for an empty body or blocks without " +
			"base_revision, 400 CAP_EXCEEDED past a bound, 404 COMMENT_NOT_FOUND.",
		Handler:  s.editCommentV2,
		Resource: "comments", Kind: kindItem, Key: "{id}",
		Body:  requestBody{Kind: bodyJSON, Type: editCommentInput{}},
		Shape: routeShape{Status: http.StatusOK, Response: shapedComment{}},
		Scope: scopeProject, ProjectOf: projectPathComment,
		IDs:    v2CommentIDs(),
		Reread: (*server).rereadComment,
		// The thread's margin record is projected again (projectThreadMark), held to its size bound.
		Refusals: documentRefusals(
			undeclaredKeyRefusal, commentMissingRefusal, notAuthorRefusal, commentInvalidRefusal, valuePastBoundRefusal,
			bodyDecisionBlockRefusal, postDeletedRefusal, documentPastSizeBoundRefusal,
		),
	}
})
