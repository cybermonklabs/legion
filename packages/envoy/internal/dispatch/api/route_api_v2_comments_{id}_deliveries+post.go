package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodPost, Pattern: "/comments/{id}/deliveries", Auth: authAny, Versions: onlyV2,
		Description: "Retry delivering one of a comment's mentions {delivery, target?}: a new attempt in the mode delivery " +
			"names (btw, aside or steer) to the target, one of the comment's mentions, which a comment of several " +
			"mentions must name; another mode, and no target on a comment of several mentions, are 400 MENTION_INPUT. A " +
			"bearer names its session in actor. Answers 201 the attempt as its send settled; a comment with no mentions " +
			"is 404 COMMENT_NOT_FOUND, as is an id no comment has.",
		Handler:  s.createCommentDeliveryV2,
		Resource: "deliveries", Kind: kindCollection, Key: "{attempt}",
		Body:  requestBody{Kind: bodyJSON, Type: createCommentDeliveryInput{}},
		Shape: routeShape{Status: http.StatusCreated, Response: model.CommentDelivery{}},
		Scope: scopeProject, ProjectOf: projectPathComment,
		IDs:      v2CommentIDs(),
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal, mentionInvalidRefusal, postDeletedRefusal},
	}
})
