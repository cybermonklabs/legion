package api

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/api/apicode"
)

var _ = declareRow(func(s *server) routeRow {
	return routeRow{
		Method: http.MethodGet, Pattern: "/comments/{id}", Auth: authAny, Versions: onlyV2,
		Description: "Read one comment: its own fields at top level, with its mentions, its delivery attempts and its " +
			"anchor's block position, and replies, every comment replying to it, oldest first (a reply or an ask thread's " +
			"comment has none of its own; an ask's thread is read on GET /api/v2/asks/{id}?include=thread). include=blocks " +
			"adds blocks, the comment's current blocks in body order: {item, revision, indexed, blocks: [{id, type, text, " +
			"ref, from, to, changed_revision, changed_at, embedding}]}, from and to UTF-16 code units into its stored body; " +
			"a comment owed a block index answers indexed false and no blocks, and the read writes nothing. An id written " +
			"short - 8 characters or more of the hex digits and hyphens an id starts with - names the one comment that " +
			"carries it (409 ID_AMBIGUOUS where two or more do). 404 COMMENT_NOT_FOUND for an id no comment has.",
		Handler:  s.getCommentV2,
		Resource: "comments", Kind: kindItem, Key: "{id}",
		Query:    commentReadV2Query{},
		Shape:    routeShape{Status: http.StatusOK, Response: commentReadV2{}},
		IDs:      v2CommentIDs(),
		Refusals: []apicode.Refusal{undeclaredKeyRefusal, commentMissingRefusal},
	}
})
