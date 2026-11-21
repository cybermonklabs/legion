// answer_ended_by_delete_test.go holds R3 of dispatch://INFRA-1523 on the two answers a delete's
// own writes do not reach: the answer a key kept before the delete, which a repeat of the key
// would replay (answerEndedByDelete, request_keys.go), and a conditional read whose validator was
// given before the delete, which would answer 304 over the deleted text (referencesETag,
// commentListETag, eventFeedETag). The kept answer is ended whatever time its delete began, and
// only where it holds the deleted text: a refusal, and a success that holds none of it, are
// replayed as they were kept, and the replay of one it ends says whether the request wrote. The
// references validator moves for every change of its answer, a rename of a cited document on
// another issue included.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// acceptSuggestion posts, to target, a suggestion on the document slug whose body is body,
// replacing quote with replacement, and accepts it, so the document gains a version named after
// body (acceptedVersionName). It answers the comment's id.
func acceptSuggestion(t *testing.T, handler http.Handler, target, slug, body, quote, replacement string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, target, map[string]any{
		"body": body, "anchor": map[string]any{"artifact": slug, "quote": quote},
		"suggestion": map[string]string{"replace_with": replacement},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("suggest on %s: status=%d body=%s", slug, created.Code, created.Body.String())
	}
	id := decodeBody[struct {
		ID string `json:"id"`
	}](t, created).ID
	if accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+id+"/accept", map[string]any{}, "alice"); accepted.Code != http.StatusOK {
		t.Fatalf("accept the suggestion on %s: status=%d body=%s", slug, accepted.Code, accepted.Body.String())
	}
	return id
}

// createProjectIssue files an issue on project with title and spec, as alice.
func createProjectIssue(t *testing.T, handler http.Handler, project, title, spec string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": project, "title": title, "spec": spec,
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
}

// keyedSend answers a send of body to target through handler under a key, as alice.
func keyedSend(t *testing.T, handler http.Handler, method, target string, body map[string]any) func(string) *httptest.ResponseRecorder {
	return func(key string) *httptest.ResponseRecorder {
		return keyedRequest(t, handler, method, target, body, "alice", key)
	}
}

// The census: every keyed route whose kept answer can carry the text of a comment or of an issue
// message and is not answered by a reread of the post (keyedRereads, whose rows are
// TestAKeyedRepeatAfterADeleteGivesThePostAsItStandsNow), one row each. A row's write is sent
// under a key and answers the deleted text - a post's snippet in a related-item check, in its own
// route or in a write's advice; a document version an accept named after a suggestion's body; a
// created post, or the block text of the post a reply answers - then the post is deleted and the
// same key sent again.
//
// The repeat is a replay, under the first answer's status, and gives no part of the deleted text.
// A route answered from its stored bytes answers ANSWER_NOT_KEPT, as for an answer it did not
// keep. A route that commits a recovery point answers what it made as it stands now and keeps no
// member of the first answer (replayKeptMembers); a row whose first answer carries one fails
// where the replay copies it. The reply to a block is the one row whose first answer holds the
// deleted text only where the replay reads the post afresh, the block text of the post it
// answers (reply_block): the delete ends nothing of that answer, and its replay keeps the first
// answer's members, which hold none of the text.
func TestAKeyedRepeatOfAnAnswerADeleteEndedGivesNoPartOfTheDeletedText(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	holder := createInteractionIssue(t, handler, "TEST", "Posts the census deletes", "alpha bravo charlie")
	asker := createProjectIssue(t, handler, "TEST", "Where the census asks", "delta echo foxtrot")
	notes := createProjectDocument(t, handler, "TEST", "Census notes", "# Census notes\n\nGolf hotel india.\n")
	asked := func(match string) string { return "Should the " + match + " calibration change?" }

	// commented posts a comment on the holder whose body finds match and carries phrase, for a
	// related-item check to find, and answers its withdrawal.
	commented := func(t *testing.T, match, phrase string) func(*httptest.ResponseRecorder) {
		comment := createThreadComment(t, handler, holder.Key, map[string]any{"body": match + " calibration drifted, " + phrase + "."}, "alice")
		refreshTermStats(t, database, "TEST")
		return func(*httptest.ResponseRecorder) { deleteThreadComment(t, handler, comment.ID, "alice") }
	}
	// withdrawCreated deletes the post the first answer is.
	withdrawCreated := func(t *testing.T, path string) func(*httptest.ResponseRecorder) {
		return func(first *httptest.ResponseRecorder) {
			id := decodeBody[struct {
				ID string `json:"id"`
			}](t, first).ID
			if deleted := dispatchRequest(t, handler, http.MethodDelete, path+id, nil, "alice"); deleted.Code != http.StatusOK {
				t.Fatalf("delete %s%s: status=%d body=%s", path, id, deleted.Code, deleted.Body.String())
			}
		}
	}
	decision := func(match, block string) map[string]any {
		return insertAtEnd(":::ask{#" + block + "}\n" + asked(match) + "\n:::")
	}

	rows := []struct {
		route string
		// recovered is a route that commits a recovery point (keyedReplays), and carries one whose
		// first answer holds a member a replay keeps (replayKeptMembers): an issue's post carries
		// the write's advice, and a project document's comment carries none (createCommentFor).
		// afresh is a recovered row whose first answer holds the deleted text only in what the
		// replay reads again, so its replay keeps the members the first answer carries.
		recovered, carries, afresh bool
		// write makes what the route's write needs and returns that write, sent under a key, and
		// the delete of the post whose text the first answer carries.
		write func(t *testing.T, letter, match, phrase string) (send func(key string) *httptest.ResponseRecorder, withdraw func(first *httptest.ResponseRecorder))
	}{
		{route: "POST /api/v1/projects/{key}/related, a comment it finds", write: func(t *testing.T, _, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/projects/TEST/related", map[string]any{"title": match + " calibration", "keywords_only": true}), withdraw
		}},
		{route: "POST /api/v1/projects/{key}/related, a message it finds", write: func(t *testing.T, _, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			message := createIssueMessage(t, handler, holder.Key, map[string]any{"body": match + " calibration drifted, " + phrase + "."}, "alice")
			refreshTermStats(t, database, "TEST")
			return keyedSend(t, handler, http.MethodPost, "/api/v1/projects/TEST/related", map[string]any{"title": match + " calibration", "keywords_only": true}),
				func(*httptest.ResponseRecorder) { deleteIssueMessage(t, handler, message.ID, "alice") }
		}},
		{route: "POST /api/v1/issues", write: func(t *testing.T, _, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "TEST", "title": match + " calibration review"}), withdraw
		}},
		{route: "POST /api/v1/issues/{key}/asks", write: func(t *testing.T, _, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+asker+"/asks", map[string]any{"question": asked(match)}), withdraw
		}},
		{route: "POST /api/v1/artifacts/{id}/asks", write: func(t *testing.T, _, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/asks", map[string]any{"question": asked(match)}), withdraw
		}},
		{route: "POST /api/v1/artifacts/{id}/edits", write: func(t *testing.T, letter, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			spec := decodeBody[struct {
				PrimaryArtifactID string `json:"primary_artifact_id"`
			}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+asker, nil, "alice")).PrimaryArtifactID
			return keyedSend(t, handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/edits", decision(match, "census-"+letter)), withdraw
		}},
		{route: "POST /api/v1/issues/{key}/artifacts/{slug}/edits", write: func(t *testing.T, letter, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+asker+"/artifacts/spec/edits", decision(match, "census-"+letter)), withdraw
		}},
		{route: "POST /api/v1/projects/{key}/artifacts/{slug}/edits", write: func(t *testing.T, letter, match, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			withdraw := commented(t, match, phrase)
			return keyedSend(t, handler, http.MethodPost, "/api/v1/projects/TEST/artifacts/"+notes.Slug+"/edits", decision(match, "census-"+letter)), withdraw
		}},
		{route: "PATCH /api/v1/artifacts/{id}", write: func(t *testing.T, letter, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			document := uploadIssueDocument(t, handler, holder.Key, "renamed-"+letter+".md", "alpha bravo charlie")
			comment := acceptSuggestion(t, handler, "/api/v1/issues/"+holder.Key+"/comments", document.Slug, "Say this instead, "+phrase+".", "alpha", "omega")
			return keyedSend(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": "renamed-" + letter + "-again.md"}),
				func(*httptest.ResponseRecorder) { deleteThreadComment(t, handler, comment, "alice") }
		}},
		{route: "POST /api/v1/issues/{key}/artifacts, to an existing document", write: func(t *testing.T, letter, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			name := "uploaded-" + letter + ".md"
			document := uploadIssueDocument(t, handler, holder.Key, name, "alpha bravo charlie")
			comment := acceptSuggestion(t, handler, "/api/v1/issues/"+holder.Key+"/comments", document.Slug, "Say this instead, "+phrase+".", "alpha", "omega")
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+holder.Key+"/artifacts", map[string]any{"name": name, "content": "alpha bravo charlie delta", "precondition": uploadBase(t, handler, document.ID)}),
				func(*httptest.ResponseRecorder) { deleteThreadComment(t, handler, comment, "alice") }
		}},
		{route: "POST /api/v1/projects/{key}/artifacts, to an existing document", write: func(t *testing.T, letter, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			name := "Ledger " + letter
			document := createProjectDocument(t, handler, "TEST", name, "# Ledger\n\nkilo lima mike\n")
			comment := acceptSuggestion(t, handler, "/api/v1/artifacts/"+document.ID+"/comments", document.Slug, "Say this instead, "+phrase+".", "kilo", "november")
			return keyedSend(t, handler, http.MethodPost, "/api/v1/projects/TEST/artifacts", map[string]any{"name": name, "content": "# Ledger\n\nkilo lima mike oscar\n", "precondition": uploadBase(t, handler, document.ID)}),
				func(*httptest.ResponseRecorder) { deleteThreadComment(t, handler, comment, "alice") }
		}},
		{route: "POST /api/v1/issues/{key}/comments", recovered: true, carries: true, write: func(t *testing.T, _, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+holder.Key+"/comments", map[string]any{"body": "A census comment, " + phrase + "."}),
				withdrawCreated(t, "/api/v1/comments/")
		}},
		{route: "POST /api/v1/issues/{key}/comments, a reply to a block", recovered: true, carries: true, afresh: true, write: func(t *testing.T, _, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			parent := createThreadComment(t, handler, holder.Key, map[string]any{"body": "A census parent, " + phrase + "."}, "alice")
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+holder.Key+"/comments", map[string]any{"body": "An answer.", "reply_to": parent.ID, "reply_to_block": "b1"}),
				func(*httptest.ResponseRecorder) { deleteThreadComment(t, handler, parent.ID, "alice") }
		}},
		{route: "POST /api/v1/artifacts/{id}/comments", recovered: true, write: func(t *testing.T, _, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			return keyedSend(t, handler, http.MethodPost, "/api/v1/artifacts/"+notes.ID+"/comments", map[string]any{"body": "A census document comment, " + phrase + "."}),
				withdrawCreated(t, "/api/v1/comments/")
		}},
		{route: "POST /api/v1/issues/{key}/messages", recovered: true, carries: true, write: func(t *testing.T, _, _, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			return keyedSend(t, handler, http.MethodPost, "/api/v1/issues/"+holder.Key+"/messages", map[string]any{"body": "A census message, " + phrase + "."}),
				withdrawCreated(t, "/api/v1/messages/")
		}},
	}
	for index, row := range rows {
		t.Run(row.route, func(t *testing.T) {
			letter := string(rune('a' + index))
			match, phrase := "quokkaledger"+letter, "zarquonended"+letter
			key := "ended-by-delete-" + letter
			send, withdraw := row.write(t, letter, match, phrase)
			first := send(key)
			if first.Code < 200 || first.Code > 299 || !strings.Contains(first.Body.String(), phrase) {
				t.Fatalf("the first write: status=%d body=%s, want a success holding %q, so this row can fail",
					first.Code, first.Body.String(), phrase)
			}

			withdraw(first)

			again := send(key)
			if again.Code != first.Code || again.Header().Get(contracts.IdempotencyReplayedHeader) != "?1" {
				t.Fatalf("the repeat: status=%d %s=%q body=%s, want the first answer's %d, replayed",
					again.Code, contracts.IdempotencyReplayedHeader, again.Header().Get(contracts.IdempotencyReplayedHeader),
					again.Body.String(), first.Code)
			}
			if strings.Contains(again.Body.String(), phrase) {
				t.Fatalf("the repeat after the delete gives the deleted text %q: %s", phrase, again.Body.String())
			}
			var firstMembers, againMembers map[string]json.RawMessage
			if err := json.Unmarshal(first.Body.Bytes(), &firstMembers); err != nil {
				t.Fatalf("decode the first answer: %v", err)
			}
			if err := json.Unmarshal(again.Body.Bytes(), &againMembers); err != nil {
				t.Fatalf("decode the repeat: %v", err)
			}
			if !row.recovered {
				var refusal struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(again.Body.Bytes(), &refusal); err != nil || refusal.Code != contracts.IdempotencyAnswerNotKept {
					t.Fatalf("the repeat answers %s, want %s: the key no longer holds an answer it can give", again.Body.String(), contracts.IdempotencyAnswerNotKept)
				}
				return
			}
			if string(againMembers["id"]) != string(firstMembers["id"]) {
				t.Fatalf("the repeat answers %s, want %s, what the write made", againMembers["id"], firstMembers["id"])
			}
			carried := false
			for _, name := range replayKeptMembers {
				value, kept := firstMembers[name]
				carried = carried || kept
				switch still, present := againMembers[name]; {
				case row.afresh && kept && string(still) != string(value):
					t.Fatalf("the repeat answers the first answer's %s as %s, want %s: the delete withdrew nothing of it", name, still, value)
				case !row.afresh && present:
					t.Fatalf("the repeat keeps the first answer's %s (%s) from an answer the delete ended", name, still)
				}
			}
			if carried != row.carries {
				t.Fatalf("the first answer carries a member of %v: %t, want %t: %s", replayKeptMembers, carried, row.carries, first.Body.String())
			}
		})
	}
}

// A delete's deleted_at is when its transaction began (clearDeletedPostBody), before it waited
// for the owner's lock, so a keyed write that claims its key after that, takes the lock first and
// reads the post before the delete commits keeps the body under a key newer than the delete. Each
// row stands for that race, one row for each arm of answerEndedQuery: the write is sent under a
// key and answers the deleted text, the post is deleted through its route, its deleted_at is moved
// an hour back, before the key was claimed, and the same key is sent again. The repeat is a
// replay, under the first answer's status, answered ANSWER_NOT_KEPT, with no part of the deleted
// text: a delete ends the answer whatever time it began (answerEndedByDelete).
func TestADeleteEndsAKeptAnswerWhateverTimeItBegan(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	holder := createInteractionIssue(t, handler, "TEST", "Posts deleted before the key", "alpha bravo charlie")
	// related is a related-item check for match, under a key, as alice.
	related := func(t *testing.T, match string) func(string) *httptest.ResponseRecorder {
		return func(key string) *httptest.ResponseRecorder {
			return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/TEST/related",
				map[string]any{"title": match + " calibration", "keywords_only": true}, "alice", key)
		}
	}

	rows := []struct {
		arm string
		// write makes the post whose text the write's first answer carries and returns that
		// write, sent under a key, and the delete of the post through its route, which answers
		// the table and the id of the post it deleted.
		write func(t *testing.T, letter, phrase string) (send func(key string) *httptest.ResponseRecorder, withdraw func() (table, id string))
	}{
		{"a comment the answer names: a related-item check that finds it", func(t *testing.T, letter, phrase string) (func(string) *httptest.ResponseRecorder, func() (string, string)) {
			match := "quokkabackdated" + letter
			comment := createThreadComment(t, handler, holder.Key, map[string]any{"body": match + " calibration drifted, " + phrase + "."}, "alice")
			refreshTermStats(t, database, "TEST")
			return related(t, match), func() (string, string) {
				deleteThreadComment(t, handler, comment.ID, "alice")
				return "comments", comment.ID
			}
		}},
		{"a message the answer names: a related-item check that finds it", func(t *testing.T, letter, phrase string) (func(string) *httptest.ResponseRecorder, func() (string, string)) {
			match := "quokkabackdated" + letter
			message := createIssueMessage(t, handler, holder.Key, map[string]any{"body": match + " calibration drifted, " + phrase + "."}, "alice")
			refreshTermStats(t, database, "TEST")
			return related(t, match), func() (string, string) {
				deleteIssueMessage(t, handler, message.ID, "alice")
				return "messages", message.ID
			}
		}},
		{"a version a delete renamed: a rename of the document an accept named a version of", func(t *testing.T, letter, phrase string) (func(string) *httptest.ResponseRecorder, func() (string, string)) {
			document := uploadIssueDocument(t, handler, holder.Key, "backdated-"+letter+".md", "alpha bravo charlie")
			comment := acceptSuggestion(t, handler, "/api/v1/issues/"+holder.Key+"/comments", document.Slug, "Say this instead, "+phrase+".", "alpha", "omega")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID,
						map[string]any{"name": "backdated-" + letter + "-again.md"}, "alice", key)
				}, func() (string, string) {
					deleteThreadComment(t, handler, comment, "alice")
					return "comments", comment
				}
		}},
	}
	for index, row := range rows {
		t.Run(row.arm, func(t *testing.T) {
			letter := string(rune('a' + index))
			phrase := "zarquonbackdated" + letter
			key := "backdated-delete-" + letter
			send, withdraw := row.write(t, letter, phrase)
			first := send(key)
			if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), phrase) {
				t.Fatalf("the first write: status=%d body=%s, want 200 holding %q, so this row can fail",
					first.Code, first.Body.String(), phrase)
			}

			table, id := withdraw()
			ctx := context.Background()
			moved, err := database.Pool.Exec(ctx, `update `+table+` set deleted_at = deleted_at - interval '1 hour' where id = $1`, id)
			if err != nil || moved.RowsAffected() != 1 {
				t.Fatalf("move the deleted_at of %s %s an hour back: %v (%d rows)", table, id, err, moved.RowsAffected())
			}
			// The control: the delete now reads as one that began before the key was claimed,
			// which is the race this row stands for.
			var before bool
			if err := database.Pool.QueryRow(ctx, `
				select p.deleted_at < k.created_at from `+table+` p, request_keys k
				where p.id = $1 and k.idempotency_key = $2
			`, id, key).Scan(&before); err != nil || !before {
				t.Fatalf("the deleted_at of %s %s is before the key's created_at: %t (%v), want true", table, id, before, err)
			}

			again := send(key)
			if again.Code != first.Code || again.Header().Get(contracts.IdempotencyReplayedHeader) != "?1" {
				t.Fatalf("the repeat: status=%d %s=%q body=%s, want the first answer's %d, replayed",
					again.Code, contracts.IdempotencyReplayedHeader, again.Header().Get(contracts.IdempotencyReplayedHeader),
					again.Body.String(), first.Code)
			}
			if strings.Contains(again.Body.String(), phrase) {
				t.Fatalf("the repeat after a delete that began before the key gives the deleted text %q: %s", phrase, again.Body.String())
			}
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(again.Body.Bytes(), &refusal); err != nil || refusal.Code != contracts.IdempotencyAnswerNotKept {
				t.Fatalf("the repeat answers %s, want %s: the key no longer holds an answer it can give", again.Body.String(), contracts.IdempotencyAnswerNotKept)
			}
		})
	}
}

// Every conditional read whose answer can carry a post's text, one row each: the read is made,
// its answer holds the text and carries an ETag, the post is deleted, and the read is made again
// under that ETag. It answers 200 with a new ETag and no part of the deleted text, never 304. The
// references read carries a document version an accept named after a suggestion's body, of a
// document on another issue than the one whose references they are, which the delete renames;
// the comment lists and the activity feed carry the post itself.
func TestAConditionalReadAfterADeleteAnswersWithoutTheDeletedText(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	holder := createInteractionIssue(t, handler, "TEST", "Posts the reads quote", "alpha bravo charlie")
	citer := createProjectIssue(t, handler, "TEST", "The issue that cites", "delta echo foxtrot")

	// cited accepts a suggestion carrying phrase on a document of the holder, which the citer's
	// comment cites, and answers the suggestion's withdrawal.
	cited := func(t *testing.T, letter, phrase string) func() {
		document := uploadIssueDocument(t, handler, holder.Key, "cited-"+letter+".md", "alpha bravo charlie")
		comment := acceptSuggestion(t, handler, "/api/v1/issues/"+holder.Key+"/comments", document.Slug, "Say this instead, "+phrase+".", "alpha", "omega")
		createThreadComment(t, handler, citer, map[string]any{"body": "See dispatch://" + holder.Key + "/artifact/" + document.Slug}, "alice")
		return func() { deleteThreadComment(t, handler, comment, "alice") }
	}

	rows := []struct {
		read string
		// write makes what the read carries phrase in and returns the read's path and the
		// delete of the post that holds it.
		write func(t *testing.T, letter, phrase string) (target string, withdraw func())
	}{
		{"GET /api/v1/issues/{key}/references", func(t *testing.T, letter, phrase string) (string, func()) {
			return "/api/v1/issues/" + citer + "/references", cited(t, letter, phrase)
		}},
		{"GET /api/v1/issues/{key}/references?versions=latest", func(t *testing.T, letter, phrase string) (string, func()) {
			return "/api/v1/issues/" + citer + "/references?versions=latest", cited(t, letter, phrase)
		}},
		{"GET /api/v1/issues/{key}/comments", func(t *testing.T, _, phrase string) (string, func()) {
			comment := createThreadComment(t, handler, holder.Key, map[string]any{"body": "A listed comment, " + phrase + "."}, "alice")
			return "/api/v1/issues/" + holder.Key + "/comments", func() { deleteThreadComment(t, handler, comment.ID, "alice") }
		}},
		{"GET /api/v1/artifacts/{id}/comments", func(t *testing.T, letter, phrase string) (string, func()) {
			document := createProjectDocument(t, handler, "TEST", "Listed "+letter, "# Listed\n\nkilo lima mike\n")
			created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments",
				map[string]any{"body": "A listed document comment, " + phrase + "."}, "alice")
			if created.Code != http.StatusCreated {
				t.Fatalf("comment on %s: status=%d body=%s", document.Name, created.Code, created.Body.String())
			}
			id := decodeBody[struct {
				ID string `json:"id"`
			}](t, created).ID
			return "/api/v1/artifacts/" + document.ID + "/comments", func() { deleteThreadComment(t, handler, id, "alice") }
		}},
		{"GET /api/v1/issues/{key}/events", func(t *testing.T, _, phrase string) (string, func()) {
			// An issue of its own, so the comment's events stand on the feed's first page.
			issue := createProjectIssue(t, handler, "TEST", "A feed of its own", "golf hotel india")
			comment := createThreadComment(t, handler, issue, map[string]any{"body": "A logged comment, " + phrase + "."}, "alice")
			return "/api/v1/issues/" + issue + "/events", func() { deleteThreadComment(t, handler, comment.ID, "alice") }
		}},
	}
	read := func(target, etag string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("X-Dispatch-User", "alice")
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	for index, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			letter := string(rune('a' + index))
			phrase := "zarquonconditional" + letter
			target, withdraw := row.write(t, letter, phrase)
			first := read(target, "")
			etag := first.Header().Get("ETag")
			if first.Code != http.StatusOK || etag == "" || !strings.Contains(first.Body.String(), phrase) {
				t.Fatalf("the first read: status=%d ETag=%q body=%s, want 200 under an ETag holding %q, so this row can fail",
					first.Code, etag, first.Body.String(), phrase)
			}

			withdraw()

			again := read(target, etag)
			if again.Code != http.StatusOK || again.Header().Get("ETag") == etag {
				t.Fatalf("the read under the ETag given before the delete: status=%d ETag=%q (was %q), want 200 under a new ETag",
					again.Code, again.Header().Get("ETag"), etag)
			}
			if strings.Contains(again.Body.String(), phrase) {
				t.Fatalf("the read after the delete gives the deleted text %q: %s", phrase, again.Body.String())
			}
		})
	}
}

// The references read's validator is a hash of the answer itself (referencesETag). A member is a
// document of another owner, and a rename of it (PATCH /api/v1/artifacts/{id}) appends its
// artifact.renamed event on that owner, so nothing of the citing issue moves, nor the closure,
// nor any version of the document. The read made again under the ETag given before the rename
// answers 200 under a new ETag with the new name, never 304 over the old one. One row for each
// variant: the full answer and ?versions=latest.
func TestAReferencesReadAfterACitedDocumentIsRenamedAnswersTheNewName(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	holder := createInteractionIssue(t, handler, "TEST", "Documents the references cite", "alpha bravo charlie")
	citer := createProjectIssue(t, handler, "TEST", "The issue that cites", "delta echo foxtrot")
	for index, variant := range []struct{ read, query string }{
		{"GET /api/v1/issues/{key}/references", ""},
		{"GET /api/v1/issues/{key}/references?versions=latest", "versions=latest"},
	} {
		query := variant.query
		t.Run(variant.read, func(t *testing.T) {
			letter := string(rune('a' + index))
			name, renamed := "cited-"+letter+".md", "renamed-"+letter+".md"
			document := uploadIssueDocument(t, handler, holder.Key, name, "alpha bravo charlie")
			createThreadComment(t, handler, citer, map[string]any{"body": "See dispatch://" + holder.Key + "/artifact/" + document.Slug}, "alice")
			first := issueReferencesQueryRequest(t, handler, citer, query, "")
			etag := first.Header().Get("ETag")
			if first.Code != http.StatusOK || etag == "" || !strings.Contains(first.Body.String(), `"name":"`+name+`"`) {
				t.Fatalf("the first read: status=%d ETag=%q body=%s, want 200 under an ETag naming %q",
					first.Code, etag, first.Body.String(), name)
			}
			// The control: before the rename the ETag answers 304, so the 200 below is the
			// rename's doing and not a validator that never matched.
			if unchanged := issueReferencesQueryRequest(t, handler, citer, query, etag); unchanged.Code != http.StatusNotModified {
				t.Fatalf("the read under its own ETag before the rename: status=%d body=%s, want 304", unchanged.Code, unchanged.Body.String())
			}

			if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": renamed}, "alice"); response.Code != http.StatusOK {
				t.Fatalf("rename %s to %s: status=%d body=%s", name, renamed, response.Code, response.Body.String())
			}

			again := issueReferencesQueryRequest(t, handler, citer, query, etag)
			if again.Code != http.StatusOK || again.Header().Get("ETag") == "" || again.Header().Get("ETag") == etag {
				t.Fatalf("the read under the ETag given before the rename: status=%d ETag=%q (was %q), want 200 under a new ETag",
					again.Code, again.Header().Get("ETag"), etag)
			}
			if !strings.Contains(again.Body.String(), `"name":"`+renamed+`"`) {
				t.Fatalf("the read after the rename does not name the document %q: %s", renamed, again.Body.String())
			}
		})
	}
}

// A kept refusal is replayed as it was given, whatever post it names and however it names it:
// answerEndedByDelete reads a success alone. No keyed route's refusal lists a post as an object
// of its own - the one refusal that does, 409 ID_AMBIGUOUS (writeAmbiguousID), is answered before
// the keyed layer - so the row writes that shape into the refusal its key kept: a reply to a block
// its parent lacks, kept as 400 INVALID_COMMENT, made to list the parent as a candidate by its
// id. After the parent's delete the repeat answers those kept bytes under the kept status, and
// never ANSWER_NOT_KEPT.
func TestAKeptRefusalListingAPostThatIsThenDeletedIsReplayedAsKept(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "A refusal listing a parent that is then deleted", "before")
	parent := createThreadComment(t, handler, issue.Key, map[string]any{"body": "A parent of one paragraph."}, "alice")
	target := "/api/v1/issues/" + issue.Key + "/comments"
	reply := map[string]any{"body": "A reply to a block the parent lacks.", "reply_to": parent.ID, "reply_to_block": "b9"}
	const key = "listing-refusal-then-delete"
	if first := keyedRequest(t, handler, http.MethodPost, target, reply, "alice", key); first.Code != http.StatusBadRequest {
		t.Fatalf("the first reply: status=%d body=%s, want 400, so this row can fail", first.Code, first.Body.String())
	}
	listed := `{"code":"INVALID_COMMENT","error":"reply_to_block names no block of the parent","candidates":[{"kind":"comment","id":"` +
		parent.ID + `"}]}` + "\n"
	tag, err := database.Pool.Exec(context.Background(),
		`update request_keys set response = $2 where idempotency_key = $1 and status = 400`, key, []byte(listed))
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("make the kept refusal list the parent: %v (%d rows)", err, tag.RowsAffected())
	}

	deleteThreadComment(t, handler, parent.ID, "alice")

	again := keyedRequest(t, handler, http.MethodPost, target, reply, "alice", key)
	if again.Code != http.StatusBadRequest || !replayed(again) || again.Body.String() != listed {
		t.Fatalf("the repeat after the parent's delete: status=%d replayed=%t body=%s, want the kept refusal, 400 %s",
			again.Code, replayed(again), again.Body.String(), listed)
	}
}

// A kept success that holds no part of a deleted post is replayed as it was kept, whatever uuid it
// carries (answerEndedByDelete). One row for each answer an earlier rule ended for a uuid alone:
// an issue's edit, whose answer names the issue's spec, after the delete of a plain comment
// anchored on that spec; the markdown round trip, whose answer is the caller's own text citing
// the deleted comment; and a document's rename, whose answer is the document, after the delete of
// a plain comment anchored on it, which made no version. Each first answer carries that uuid, and
// the repeat after the delete answers it byte for byte.
func TestAKeptAnswerHoldingNoTextOfADeletedPostIsReplayedAsKept(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Writes beside a deleted comment", "alpha bravo charlie")
	rows := []struct {
		route string
		// write makes the comment the row deletes and returns the write, sent under a key, the
		// uuid its first answer carries, and the comment's id.
		write func(t *testing.T, letter string) (send func(key string) *httptest.ResponseRecorder, carried, comment string)
	}{
		{"PATCH /api/v1/issues/{key}, after the delete of a plain comment anchored on its spec", func(t *testing.T, letter string) (func(string) *httptest.ResponseRecorder, string, string) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "A plain comment on the spec.", "anchor": map[string]any{"artifact": "spec", "quote": "bravo"},
			}, "alice")
			return keyedSend(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{"title": "Writes beside a deleted comment, " + letter}),
				issue.PrimaryArtifactID, comment.ID
		}},
		{"POST /api/v1/markdown/round-trip, of a text citing the deleted comment", func(t *testing.T, _ string) (func(string) *httptest.ResponseRecorder, string, string) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "A cited comment."}, "alice")
			return keyedSend(t, handler, http.MethodPost, "/api/v1/markdown/round-trip", map[string]any{
				"markdown": "See dispatch://" + issue.Key + "/comment/" + comment.ID + " for the reason.\n",
			}), comment.ID, comment.ID
		}},
		{"PATCH /api/v1/artifacts/{id}, of a document a deleted plain comment was anchored on", func(t *testing.T, letter string) (func(string) *httptest.ResponseRecorder, string, string) {
			document := uploadIssueDocument(t, handler, issue.Key, "plain-"+letter+".md", "alpha bravo charlie")
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "A plain comment on the document.", "anchor": map[string]any{"artifact": document.Slug, "quote": "bravo"},
			}, "alice")
			return keyedSend(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID, map[string]any{"name": "plain-" + letter + "-renamed.md"}),
				document.ID, comment.ID
		}},
	}
	for index, row := range rows {
		t.Run(row.route, func(t *testing.T) {
			letter := string(rune('a' + index))
			key := "kept-beside-a-delete-" + letter
			send, carried, comment := row.write(t, letter)
			first := send(key)
			if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), carried) {
				t.Fatalf("the first write: status=%d body=%s, want 200 carrying %s, so this row can fail", first.Code, first.Body.String(), carried)
			}

			deleteThreadComment(t, handler, comment, "alice")

			again := send(key)
			if again.Code != first.Code || !replayed(again) || again.Body.String() != first.Body.String() {
				t.Fatalf("the repeat after the delete: status=%d replayed=%t body=%s, want the kept answer, %d %s",
					again.Code, replayed(again), again.Body.String(), first.Code, first.Body.String())
			}
		})
	}
}

// The key records whether its request wrote (committed_at, markRequestKeyCommitted), and the
// replay of an answer Dispatch does not give says so by it (replayStoredAnswer), so the record
// must hold for the routes whose kept answer a delete ends most: the related-item check and the
// markdown round trip, each sent under a key, write nothing and leave their key unmarked, where a
// document's rename, the control, marks its key.
func TestAKeyedRequestThatWritesNothingLeavesItsKeyUncommitted(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "What a keyed request writes", "alpha bravo charlie")
	document := uploadIssueDocument(t, handler, issue.Key, "measured.md", "alpha bravo charlie")
	for _, row := range []struct {
		route, method, target string
		body                  map[string]any
		wrote                 bool
	}{
		{"POST /api/v1/projects/{key}/related, by keywords", http.MethodPost, "/api/v1/projects/TEST/related",
			map[string]any{"title": "alpha calibration", "keywords_only": true}, false},
		{"POST /api/v1/projects/{key}/related", http.MethodPost, "/api/v1/projects/TEST/related",
			map[string]any{"title": "alpha calibration", "text": "bravo charlie"}, false},
		{"POST /api/v1/markdown/round-trip", http.MethodPost, "/api/v1/markdown/round-trip",
			map[string]any{"markdown": "1. alpha\n1. bravo\n"}, false},
		{"PATCH /api/v1/artifacts/{id}, the control", http.MethodPatch, "/api/v1/artifacts/" + document.ID,
			map[string]any{"name": "measured-renamed.md"}, true},
	} {
		t.Run(row.route, func(t *testing.T) {
			key := "measured-" + strings.NewReplacer(" ", "-", "/", "-", "{", "", "}", "", ",", "").Replace(row.route)
			if response := keyedRequest(t, handler, row.method, row.target, row.body, "alice", key); response.Code != http.StatusOK {
				t.Fatalf("the keyed request: status=%d body=%s, want 200", response.Code, response.Body.String())
			}
			if committed := committedAt(t, database, key); committed != row.wrote {
				t.Fatalf("the key is marked committed: %t, want %t", committed, row.wrote)
			}
		})
	}
}

// The replay of an answer a delete ended says what its request did, as its key records it
// (replayStoredAnswer): one row for each sentence. A related-item check that finds a comment
// wrote nothing, and after the comment's delete its repeat is told the answer is not kept and it
// may be sent again under a new key; a rename of a document whose version an accept named after
// a suggestion's body wrote, and after the suggestion's delete renamed that version its repeat is
// told the request ran and to read what it wrote. Each is a replay under the first answer's
// status, ANSWER_NOT_KEPT, and gives no part of the deleted body.
func TestARepeatOfAnAnswerADeleteEndedSaysWhetherTheRequestWrote(t *testing.T) {
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Answers a delete ends", "alpha bravo charlie")
	rows := []struct {
		sentence string
		wrote    bool
		// write makes the post whose text the first answer carries and returns the write, sent
		// under a key, and the post's delete.
		write func(t *testing.T, letter, phrase string) (send func(key string) *httptest.ResponseRecorder, withdraw func())
	}{
		{"wrote nothing: a related-item check that finds the deleted comment", false, func(t *testing.T, letter, phrase string) (func(string) *httptest.ResponseRecorder, func()) {
			match := "quokkasentence" + letter
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": match + " calibration drifted, " + phrase + "."}, "alice")
			refreshTermStats(t, database, "TEST")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPost, "/api/v1/projects/TEST/related",
						map[string]any{"title": match + " calibration", "keywords_only": true}, "alice", key)
				}, func() {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"wrote: a rename of a document whose version the delete renamed", true, func(t *testing.T, letter, phrase string) (func(string) *httptest.ResponseRecorder, func()) {
			document := uploadIssueDocument(t, handler, issue.Key, "sentence-"+letter+".md", "alpha bravo charlie")
			comment := acceptSuggestion(t, handler, "/api/v1/issues/"+issue.Key+"/comments", document.Slug, "Say this instead, "+phrase+".", "alpha", "omega")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPatch, "/api/v1/artifacts/"+document.ID,
						map[string]any{"name": "sentence-" + letter + "-renamed.md"}, "alice", key)
				}, func() {
					deleteThreadComment(t, handler, comment, "alice")
				}
		}},
	}
	for index, row := range rows {
		t.Run(row.sentence, func(t *testing.T) {
			letter := string(rune('a' + index))
			phrase := "zarquonsentence" + letter
			key := "sentence-" + letter
			send, withdraw := row.write(t, letter, phrase)
			first := send(key)
			if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), phrase) {
				t.Fatalf("the first request: status=%d body=%s, want 200 holding %q, so this row can fail", first.Code, first.Body.String(), phrase)
			}
			if committed := committedAt(t, database, key); committed != row.wrote {
				t.Fatalf("the key is marked committed: %t, want %t", committed, row.wrote)
			}

			withdraw()

			again := send(key)
			code, text := refusalOf(t, again)
			if again.Code != first.Code || !replayed(again) || code != contracts.IdempotencyAnswerNotKept {
				t.Fatalf("the repeat after the delete: status=%d replayed=%t body=%s, want %d %s, replayed",
					again.Code, replayed(again), again.Body.String(), first.Code, contracts.IdempotencyAnswerNotKept)
			}
			if strings.Contains(again.Body.String(), phrase) {
				t.Fatalf("the repeat after the delete gives the deleted text %q: %s", phrase, again.Body.String())
			}
			ran, wroteNothing := "Dispatch ran this request", "the request wrote nothing, so it may be sent again under a new "+contracts.IdempotencyKeyHeader
			says, never := wroteNothing, ran
			if row.wrote {
				says, never = ran, wroteNothing
			}
			if !strings.Contains(text, says) || strings.Contains(text, never) || !strings.Contains(text, "a comment or a message that was deleted") {
				t.Fatalf("the repeat says %q, want it to say %q and why, and never %q", text, says, never)
			}
		})
	}
}

// The objects a kept answer holds, as answerEndedByDelete reads them (answerHoldingsOf): the own
// id of each object - a comment's, a message's, a related-item hit's, in a check or in a write's
// advice, and the message whose body the accept of a delivery carries - and each version a
// document lists. A uuid in a text, in a member naming a post from another object (reply_to,
// answers, reply_block, a delivery's comment_id or message_id) or in a member naming a document
// (primary_artifact_id) is none of them. One row for each shape.
func TestAKeptAnswerHoldsTheObjectsItsOwnIDsName(t *testing.T) {
	const (
		comment  = "0b9e1c2e-6f4a-4d6e-9a51-1c3f6a7b8c01"
		parent   = "0b9e1c2e-6f4a-4d6e-9a51-1c3f6a7b8c02"
		message  = "0b9e1c2e-6f4a-4d6e-9a51-1c3f6a7b8c03"
		document = "0b9e1c2e-6f4a-4d6e-9a51-1c3f6a7b8c04"
	)
	for _, row := range []struct {
		shape, answer string
		ids, versions []string
	}{
		{"a comment answering a block of its parent", `{"id":"` + comment + `","body":"An answer.","author":{"kind":"user","id":"alice"},` +
			`"reply_to":"` + parent + `","answers":"` + parent + `","reply_block":{"node":"comment/` + parent + `#b1","block_id":"b1","text":"The parent's words."},` +
			`"deliveries":[{"comment_id":"` + comment + `","attempt":1,"state":"sent"}]}`, []string{comment}, nil},
		{"a related-item check", `{"outcome":"ok","related":[{"kind":"comment","id":"` + comment + `","snippet":"words"}],` +
			`"other_projects":[{"kind":"message","id":"` + message + `","snippet":"words"}]}`, []string{comment, message}, nil},
		{"a write's advice", `{"key":"TEST-1","primary_artifact_id":"` + document + `","advice":{"suggestions":{"related":[{"kind":"comment","id":"` + comment + `"}]},` +
			`"decision_block_checks":[{"block_id":"b1","suggestions":{"related":[{"kind":"message","id":"` + message + `"}]}}]}}`, []string{comment, message}, nil},
		{"the accept of a delivery", `{"message_id":"` + message + `","attempt":1,"body":"A direct message."}`, []string{message}, nil},
		{"a delivery attempt", `{"message_id":"` + message + `","attempt":1,"state":"sent"}`, nil, nil},
		{"a document", `{"id":"` + document + `","name":"notes.md","versions":[{"number":1,"summary":null},{"number":2,"summary":"Named"}]}`,
			[]string{document}, []string{document + "@1", document + "@2"}},
		{"an upload", `{"artifact":{"id":"` + document + `","versions":[{"number":3}]},"version":{"number":3,"summary":"Named"}}`,
			[]string{document}, []string{document + "@3"}},
		{"the markdown round trip", `{"markdown":"See dispatch://TEST-1/comment/` + comment + ` for the reason.\n"}`, nil, nil},
	} {
		t.Run(row.shape, func(t *testing.T) {
			holdings := answerHoldingsOf([]byte(row.answer))
			versions := make([]string, len(holdings.versionNumbers))
			for index, number := range holdings.versionNumbers {
				versions[index] = fmt.Sprintf("%s@%d", holdings.versionDocuments[index], number)
			}
			slices.Sort(holdings.ids)
			slices.Sort(versions)
			if !slices.Equal(holdings.ids, row.ids) || !slices.Equal(versions, row.versions) {
				t.Fatalf("%s holds ids %v and versions %v, want %v and %v", row.answer, holdings.ids, versions, row.ids, row.versions)
			}
		})
	}
}

// answerEndedByDelete leaves out the block text a reply carries of the post it answers
// (reply_block), as every repeat that could give it reads the post again: each keyed route whose
// answer can hold a reply block, as routeShapes declares the answer's Go type, answers a repeat
// with the post read again (keyedRereads) or from its recovery point (keyedReplays), and no
// refusal, which every route replays as kept, holds one.
func TestEveryKeyedAnswerThatCanHoldAReplyBlockIsReadAfresh(t *testing.T) {
	block := reflect.TypeFor[model.ReplyBlock]()
	var holds func(ty reflect.Type, seen map[reflect.Type]bool) bool
	holds = func(ty reflect.Type, seen map[reflect.Type]bool) bool {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty == block {
			return true
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return false
		}
		seen[ty] = true
		for index := range ty.NumField() {
			if field := ty.Field(index); field.Tag.Get("json") != "-" && holds(field.Type, seen) {
				return true
			}
		}
		return false
	}
	replays := (&server{}).keyedReplays()
	found := 0
	for route, shape := range routeShapes {
		method, _, _ := strings.Cut(route, " ")
		if !keyedMethod(method) {
			continue
		}
		_, rereads := keyedRereads[route]
		_, recovers := replays[route]
		for _, answer := range append([]routeAnswer{{Status: shape.Status, Body: shape.Response}, {Status: shape.Status, Body: shape.Advice}}, shape.Also...) {
			if answer.Body == nil || !holds(reflect.TypeOf(answer.Body), map[reflect.Type]bool{}) {
				continue
			}
			found++
			if !rereads && !recovers {
				t.Errorf("%s answers %T at %d, which can hold a reply block, and a repeat replays its kept bytes", route, answer.Body, answer.Status)
			}
		}
		for _, refusal := range shape.Refusals {
			if holds(reflect.TypeOf(refusal.Body), map[reflect.Type]bool{}) {
				t.Errorf("%s refuses with %T at %d, which can hold a reply block, and a refusal is replayed as kept", route, refusal.Body, refusal.Status)
			}
		}
	}
	if found == 0 {
		t.Fatal("no keyed answer can hold a reply block, so this row cannot fail")
	}
}
