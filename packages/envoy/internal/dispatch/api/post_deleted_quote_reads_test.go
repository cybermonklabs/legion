package api

// post_deleted_quote_reads_test.go holds R3 of dispatch://INFRA-1523 on the reads the census found
// no test of that give a post's words as part of something else: a comment's blocks (row 1.6), a
// related-item check's hit (1.22) and a document's row for a post that cites it (1.24).
//
// The row numbers in subtest names and comments are the rows of the issue's surface census,
// dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"net/http"
	"net/url"
	"testing"
)

// Each read is made while the post it gives the words of stands, and again after its author
// deletes it. A comment's blocks read then says the comment is deleted and gives no block; the
// related-item check and the document's citing rows no longer find the post at all, since the
// delete took the words they found it by.
func TestEveryReadThatGivesADeletedPostsWordsAsPartOfAnotherGivesNoneOfThem(t *testing.T) {
	t.Parallel()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Posts other reads quote", "before")
	cited := createProjectDocument(t, handler, "TEST", "Cited notes", "# Cited notes\n\nAlpha bravo.\n")
	related := func(match string) string {
		return "/api/v1/projects/TEST/related?" + url.Values{"title": {match + " calibration"}, "keywords_only": {"true"}}.Encode()
	}
	comment := func(t *testing.T, body string) (postCopies, func()) {
		created := createThreadComment(t, handler, issue.Key, map[string]any{"body": body}, "alice")
		return postCopies{kind: "comment", id: created.ID, by: "alice"}, func() { deleteThreadComment(t, handler, created.ID, "alice") }
	}
	message := func(t *testing.T, body string) (postCopies, func()) {
		created := createIssueMessage(t, handler, issue.Key, map[string]any{"body": body}, "alice")
		return postCopies{kind: "message", id: created.ID, by: "alice"}, func() { deleteIssueMessage(t, handler, created.ID, "alice") }
	}

	rows := []struct {
		read string
		// write makes the post the read gives the words of and answers what the answer carries of
		// it, the read's path, and the post's delete.
		write func(t *testing.T, letter, phrase string) (post postCopies, path string, withdraw func())
	}{
		// The delete's clear (clearDeletedPostBody) leaves the body no block, and the reference
		// graph marks the comment's own node (refs.loadCommentNodes), which the answer's item is.
		{"1.6 GET /api/v1/comments/{id}/blocks", func(t *testing.T, _, phrase string) (postCopies, string, func()) {
			post, withdraw := comment(t, phrase+" as written.\n\nThe second paragraph.")
			post.posts = 1
			return post, "/api/v1/comments/" + post.id + "/blocks", withdraw
		}},
		// The delete clears the comment's body (clearDeletedPostBody) and with it its search
		// vector, so the keyword arm the check runs finds it no more. A hit carries no mark of its
		// own.
		{"1.22 GET /api/v1/projects/{key}/related, a comment it finds", func(t *testing.T, letter, phrase string) (postCopies, string, func()) {
			match := "quokkafound" + letter
			post, withdraw := comment(t, match+" calibration drifted, "+phrase+".")
			refreshTermStats(t, database, "TEST")
			post.absent = true
			return post, related(match), withdraw
		}},
		// The delete's clear, as for a comment.
		{"1.22 GET /api/v1/projects/{key}/related, a message it finds", func(t *testing.T, letter, phrase string) (postCopies, string, func()) {
			match := "quokkafound" + letter
			post, withdraw := message(t, match+" calibration drifted, "+phrase+".")
			refreshTermStats(t, database, "TEST")
			post.absent = true
			return post, related(match), withdraw
		}},
		// The delete indexes the cleared body again (planEditedItem, in clearDeletedPostBody),
		// which takes the post's mention of the document out of the graph, so no row names the
		// post. A row (model.ReferencedBy) carries no mark of its own.
		{"1.24 GET /api/v1/artifacts/{id}/references, a comment that cites it", func(t *testing.T, _, phrase string) (postCopies, string, func()) {
			post, withdraw := comment(t, phrase+" is in dispatch://TEST/artifact/"+cited.Slug)
			post.absent = true
			return post, "/api/v1/artifacts/" + cited.ID + "/references", withdraw
		}},
		// The delete's index of the cleared body (planEditedItem, in clearDeletedPostBody), as
		// for a comment.
		{"1.24 GET /api/v1/artifacts/{id}/references, a message that cites it", func(t *testing.T, _, phrase string) (postCopies, string, func()) {
			post, withdraw := message(t, phrase+" is in dispatch://TEST/artifact/"+cited.Slug)
			post.absent = true
			return post, "/api/v1/artifacts/" + cited.ID + "/references", withdraw
		}},
	}
	for index, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			letter := string(rune('a' + index))
			phrase := "zarquonquoted" + letter
			post, path, withdraw := row.write(t, letter, phrase)
			before := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
			if before.Code != http.StatusOK {
				t.Fatalf("%s before the delete: status=%d body=%s", row.read, before.Code, before.Body.String())
			}
			requireStandingPostRead(t, row.read, before.Body.Bytes(), phrase, post)

			withdraw()

			after := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
			if after.Code != http.StatusOK {
				t.Fatalf("%s after the delete: status=%d body=%s", row.read, after.Code, after.Body.String())
			}
			requireDeletedPostRead(t, row.read, after.Body.Bytes(), phrase, post)
		})
	}
}
