// post_deleted_replay_test.go holds R3 of dispatch://INFRA-1523 on the keyed path: a repeat of a
// key whose first answer holds a post's body is answered with the post as it stands now
// (keyedRereads, request_keys.go), so a post deleted since gives the repeat no part of its body.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// keyedSessionRequest is sessionRequest under an Idempotency-Key: a session's write as its
// client sends one.
func keyedSessionRequest(t *testing.T, handler http.Handler, method, target string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode session request: %v", err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set(contracts.IdempotencyKeyHeader, key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// Every keyed route whose answer holds a post's body, one row each: the route's write is sent
// under a key and answers the phrase, the post holding it is deleted, and the same key is sent
// again. The repeat is a replay - the first answer's status, Idempotency-Replayed, the post the
// first answer named, the members only the write's answer carries kept - and it gives no part of
// the deleted body. Each reply route has two rows: the reply it posts (201), and the reply it
// hands back for an attempt the session already answered (200, a message's marked duplicate).
// The comment reply's rows delete the comment the reply answers, whose block text the reply
// carries (reply_block); every other row deletes the post the answer is.
func TestAKeyedRepeatAfterADeleteGivesThePostAsItStandsNow(t *testing.T) {
	t.Parallel()
	_, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Keyed repeats of deleted posts", "alpha bravo charlie")
	session := sessionActor()["id"].(string)
	// The mention and the attempt a session answers are written as a send to a live listener
	// would leave them, since the reply is what these rows are about.
	seedMention := func(t *testing.T, commentID string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `
			with mention as (
				insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
				values ($1, 'session:' || $2, 'steer', $2)
			)
			insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, state)
			values ($1, 'session:' || $2, 1, 'steer', $2, 'pending')
		`, commentID, session); err != nil {
			t.Fatalf("seed the mention the session answers: %v", err)
		}
	}
	seedAttempt := func(t *testing.T, messageID string) {
		t.Helper()
		if _, err := database.Pool.Exec(context.Background(), `
			insert into message_deliveries (message_id, attempt, delivery, session_id, state)
			values ($1, 1, 'steer', $2, 'pending')
		`, messageID, session); err != nil {
			t.Fatalf("seed the attempt the session answers: %v", err)
		}
	}

	rows := []struct {
		route string
		// write makes what the route's write needs and returns that write, sent under key, and
		// the delete of the post whose text the first answer holds.
		write func(t *testing.T, phrase string) (send func(key string) *httptest.ResponseRecorder, withdraw func(first *httptest.ResponseRecorder))
	}{
		{"PATCH /api/v1/comments/{id}", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Before the edit."}, "alice")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+comment.ID,
						map[string]any{"body": "Edited, " + phrase + "."}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/resolve", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Resolve me, " + phrase + "."}, "alice")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", map[string]any{}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/reopen", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Reopen me, " + phrase + "."}, "alice")
			if resolved := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/resolve", map[string]any{}, "alice"); resolved.Code != http.StatusOK {
				t.Fatalf("resolve the thread before its reopen: status=%d body=%s", resolved.Code, resolved.Body.String())
			}
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reopen", map[string]any{}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/accept", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Accept me, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": "alpha"},
				"suggestion": map[string]string{"replace_with": "omega"},
			}, "alice")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/accept", map[string]any{}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/reject", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{
				"body": "Reject me, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": "bravo"},
				"suggestion": map[string]string{"replace_with": "omega"},
			}, "alice")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reject", map[string]any{}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/reply", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Over to the session, " + phrase + "."}, "alice")
			seedMention(t, comment.ID)
			return func(key string) *httptest.ResponseRecorder {
					return keyedSessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", map[string]any{
						"actor": sessionActor(), "attempt": 1, "body": "Answered.", "reply_to_block": "b1",
					}, key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"POST /api/v1/comments/{id}/reply, an attempt already answered", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Over to the session again, " + phrase + "."}, "alice")
			seedMention(t, comment.ID)
			reply := map[string]any{"actor": sessionActor(), "attempt": 1, "body": "Answered.", "reply_to_block": "b1"}
			if answered := sessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", reply); answered.Code != http.StatusCreated {
				t.Fatalf("the session answers the attempt: status=%d body=%s", answered.Code, answered.Body.String())
			}
			return func(key string) *httptest.ResponseRecorder {
					return keyedSessionRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment.ID+"/reply", reply, key)
				}, func(*httptest.ResponseRecorder) {
					deleteThreadComment(t, handler, comment.ID, "alice")
				}
		}},
		{"PATCH /api/v1/messages/{id}", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Before the edit."}, "alice")
			return func(key string) *httptest.ResponseRecorder {
					return keyedRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+message.ID,
						map[string]any{"body": "Edited, " + phrase + "."}, "alice", key)
				}, func(*httptest.ResponseRecorder) {
					deleteIssueMessage(t, handler, message.ID, "alice")
				}
		}},
		{"POST /api/v1/messages/{id}/reply", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Over to the session."}, "alice")
			seedAttempt(t, message.ID)
			return func(key string) *httptest.ResponseRecorder {
					return keyedSessionRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", map[string]any{
						"actor": sessionActor(), "attempt": 1, "body": "Answered, " + phrase + ".",
					}, key)
				}, func(first *httptest.ResponseRecorder) {
					deleteIssueMessage(t, handler, decodeBody[replyResponse](t, first).ID, "")
				}
		}},
		{"POST /api/v1/messages/{id}/reply, a duplicate", func(t *testing.T, phrase string) (func(string) *httptest.ResponseRecorder, func(*httptest.ResponseRecorder)) {
			message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Over to the session again."}, "alice")
			seedAttempt(t, message.ID)
			reply := map[string]any{"actor": sessionActor(), "attempt": 1, "body": "Answered again, " + phrase + "."}
			if answered := sessionRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", reply); answered.Code != http.StatusCreated {
				t.Fatalf("the session answers the attempt: status=%d body=%s", answered.Code, answered.Body.String())
			}
			return func(key string) *httptest.ResponseRecorder {
					return keyedSessionRequest(t, handler, http.MethodPost, "/api/v1/messages/"+message.ID+"/reply", reply, key)
				}, func(first *httptest.ResponseRecorder) {
					deleteIssueMessage(t, handler, decodeBody[replyResponse](t, first).ID, "")
				}
		}},
	}
	for index, row := range rows {
		t.Run(row.route, func(t *testing.T) {
			phrase := "zarquonkeyedrepeat" + string(rune('a'+index))
			key := "repeat-after-delete-" + string(rune('a'+index))
			send, withdraw := row.write(t, phrase)
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
			if string(againMembers["id"]) != string(firstMembers["id"]) {
				t.Fatalf("the repeat answers post %s, want %s, the one the first answer named", againMembers["id"], firstMembers["id"])
			}
			for _, name := range replayKeptMembers {
				if value, carried := firstMembers[name]; carried && string(againMembers[name]) != string(value) {
					t.Fatalf("the repeat's %s = %s, want the first answer's %s", name, againMembers[name], value)
				}
			}
			// The post is the one a read gives now: the deleted post's own mark, or for a reply,
			// the mark on the block it answers.
			if !strings.Contains(again.Body.String(), `"deleted_at":"`) {
				t.Fatalf("the repeat carries no delete mark, so it is not the post as it stands: %s", again.Body.String())
			}
		})
	}
}
