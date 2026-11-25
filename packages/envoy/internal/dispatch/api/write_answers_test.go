package api

// The answer of a comment's edit, delete, resolve, reopen, accept and reject lists the mentions
// and the delivery attempts a read of the comment lists right after it, and an issue message's
// edit and delete list its attempts; such a write whose answer cannot be read answers an error
// and leaves the post as it was.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// writeAnswersFixture is a server whose stand-in Envoy listener knows no holder of
// role:reviewer. A comment that mentions the role, or an issue message sent to it, writes one
// attempt that fails "no holder for role reviewer", as
// TestMentionedCommentDoesNotReresolveMissingRoleHolder and
// TestTargetedMessageResolvesRolesAndEnforcesReplyBoundaries find, so every post below holds a
// mention row and a delivery row, or a delivery row, with no live session to wait on.
type writeAnswersFixture struct {
	handler  http.Handler
	database *store.Store
}

func newWriteAnswersFixture(t *testing.T) writeAnswersFixture {
	t.Helper()
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/roles/reviewer" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no holder for role reviewer"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	return writeAnswersFixture{handler: handler, database: database}
}

// mentioningComment posts alice's comment of input on the issue, mentioning role:reviewer, and
// waits until its attempt has settled, so no write below races the mention sender.
func (f writeAnswersFixture) mentioningComment(t *testing.T, issueKey string, input map[string]any) string {
	t.Helper()
	input["mentions"] = []map[string]any{{"target": "role:reviewer"}}
	created := decodeMentionedComment(t, postMentionedComment(t, f.handler, issueKey, input))
	settledCommentDeliveries(t, f.database, created.ID)
	return created.ID
}

// jsonFields is the fields of one JSON object, each as the bytes it was written with.
func jsonFields(t *testing.T, what string, object []byte) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(object, &fields); err != nil {
		t.Fatalf("decode %s: %v (%s)", what, err, object)
	}
	return fields
}

// requireAnswerListsTheRead fails t unless the answer of write lists, in each of fields, exactly
// what the read lists in it: the same bytes, since both are written by WriteJSON's one encoder.
func requireAnswerListsTheRead(t *testing.T, write, read string, answer *httptest.ResponseRecorder, stored map[string]json.RawMessage, fields ...string) {
	t.Helper()
	answered := jsonFields(t, "the answer of "+write, answer.Body.Bytes())
	for _, field := range fields {
		got, listed := answered[field]
		if !listed {
			t.Fatalf("the answer of %s has no %s, want %s, what %s lists", write, field, stored[field], read)
		}
		if want := stored[field]; !bytes.Equal(got, want) {
			t.Fatalf("the answer of %s lists %s %s, want %s, what %s lists", write, field, got, want, read)
		}
	}
}

// requireCommentAnswerListsTheRead fails t unless answer, the 200 of write to comment id, lists
// the mentions and the deliveries GET /api/v1/comments/{id} lists right after it. The read is
// the control: it has to list the mention of role:reviewer and its one attempt, which proves the
// comment holds the rows the answer must list.
func (f writeAnswersFixture) requireCommentAnswerListsTheRead(t *testing.T, write, id string, answer *httptest.ResponseRecorder) {
	t.Helper()
	if answer.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", write, answer.Code, answer.Body.String())
	}
	response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/comments/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read comment %s after %s: status=%d body=%s", id, write, response.Code, response.Body.String())
	}
	stored := jsonFields(t, "the comment read", jsonFields(t, "the comment read", response.Body.Bytes())["comment"])
	var mentions []commentMentionRead
	var deliveries []commentDeliveryRead
	if err := json.Unmarshal(stored["mentions"], &mentions); err != nil {
		t.Fatalf("decode the read's mentions %s: %v", stored["mentions"], err)
	}
	if err := json.Unmarshal(stored["deliveries"], &deliveries); err != nil {
		t.Fatalf("decode the read's deliveries %s: %v", stored["deliveries"], err)
	}
	if len(mentions) != 1 || mentions[0].Target != "role:reviewer" ||
		len(deliveries) != 1 || deliveries[0].Target != "role:reviewer" || deliveries[0].Attempt != 1 {
		t.Fatalf("the read after %s lists mentions %s and deliveries %s, want the mention of role:reviewer and its one attempt",
			write, stored["mentions"], stored["deliveries"])
	}
	requireAnswerListsTheRead(t, write, "GET /api/v1/comments/"+id, answer, stored, "mentions", "deliveries")
}

// requireMessageAnswerListsTheRead is requireCommentAnswerListsTheRead for an issue message: the
// answer lists the deliveries GET /api/v1/messages/{id} lists right after it, and that read lists
// the message's one attempt.
func (f writeAnswersFixture) requireMessageAnswerListsTheRead(t *testing.T, write, id string, answer *httptest.ResponseRecorder) {
	t.Helper()
	if answer.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", write, answer.Code, answer.Body.String())
	}
	response := dispatchRequest(t, f.handler, http.MethodGet, "/api/v1/messages/"+id, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read message %s after %s: status=%d body=%s", id, write, response.Code, response.Body.String())
	}
	stored := jsonFields(t, "the message read", jsonFields(t, "the message read", response.Body.Bytes())["message"])
	var deliveries []model.MessageDelivery
	if err := json.Unmarshal(stored["deliveries"], &deliveries); err != nil {
		t.Fatalf("decode the read's deliveries %s: %v", stored["deliveries"], err)
	}
	if len(deliveries) != 1 || deliveries[0].MessageID != id || deliveries[0].Attempt != 1 {
		t.Fatalf("the read after %s lists deliveries %s, want the message's one attempt", write, stored["deliveries"])
	}
	requireAnswerListsTheRead(t, write, "GET /api/v1/messages/"+id, answer, stored, "deliveries")
}

// Each write to a comment answers the comment with the mention it carries and the attempt that
// mention wrote, as a read made right after the write lists them: the author's edit and delete,
// a thread's resolve and its reopen, and a suggestion's accept and its reject.
func TestCommentWriteAnswersListTheMentionsAndDeliveriesAReadLists(t *testing.T) {
	t.Parallel()
	f := newWriteAnswersFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "A comment's writes answer", "before")
	type write struct {
		method, route string
		body          any
	}
	for _, test := range []struct {
		name    string
		comment map[string]any
		writes  []write
	}{
		{
			name:    "edit",
			comment: map[string]any{"body": "Reviewers, read the spec."},
			writes:  []write{{method: http.MethodPatch, body: map[string]any{"body": "Reviewers, read the new spec."}}},
		},
		{
			name:    "delete",
			comment: map[string]any{"body": "Reviewers, read the spec."},
			writes:  []write{{method: http.MethodDelete}},
		},
		{
			name:    "resolve and reopen",
			comment: map[string]any{"body": "Reviewers, is this settled?"},
			writes:  []write{{method: http.MethodPost, route: "/resolve"}, {method: http.MethodPost, route: "/reopen"}},
		},
		{
			name: "accept",
			comment: map[string]any{
				"body": "Say after.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
				"suggestion": map[string]string{"replace_with": "after"},
			},
			writes: []write{{method: http.MethodPost, route: "/accept"}},
		},
		{
			name:    "reject",
			comment: map[string]any{"body": "Say ignored.", "suggestion": map[string]string{"replace_with": "ignored"}},
			writes:  []write{{method: http.MethodPost, route: "/reject"}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := f.mentioningComment(t, issue.Key, test.comment)
			for _, write := range test.writes {
				path := "/api/v1/comments/" + id + write.route
				answer := dispatchRequest(t, f.handler, write.method, path, write.body, "alice")
				f.requireCommentAnswerListsTheRead(t, write.method+" "+path, id, answer)
			}
		})
	}
}

// The edit and the delete of an issue message sent to a role answer the message with the attempt
// that send wrote, as a read made right after each lists it.
func TestMessageCorrectionAnswersListTheDeliveriesAReadLists(t *testing.T) {
	t.Parallel()
	f := newWriteAnswersFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "A message's corrections answer", "before")
	message := createIssueMessage(t, f.handler, issue.Key, map[string]any{
		"body": "Who reviews this?", "target": "role:reviewer", "delivery": "btw",
	}, "alice")
	path := "/api/v1/messages/" + message.ID

	edited := dispatchRequest(t, f.handler, http.MethodPatch, path, map[string]any{"body": "Who reviews this now?"}, "alice")
	f.requireMessageAnswerListsTheRead(t, "PATCH "+path, message.ID, edited)

	deleted := dispatchRequest(t, f.handler, http.MethodDelete, path, nil, "alice")
	f.requireMessageAnswerListsTheRead(t, "DELETE "+path, message.ID, deleted)
}

// unreadableAttempt makes the post's one settled delivery attempt unreadable to Dispatch: it sets
// the row's created_at to 'infinity', a timestamptz Postgres stores, compares and orders, which
// pgx refuses to scan into the time.Time every read of an attempt decodes it into
// (scanCommentDelivery, scanMessageDelivery: "cannot scan Infinity into *time.Time"). No write
// below scans a settled attempt of its post but its answer's read - the delete settles the
// pending ones alone, which it takes from that read (queueDeletedPostSettlement) - so that read is
// the one it fails, wherever it runs: in the write's read phase, or on the pool after its commit.
func (f writeAnswersFixture) unreadableAttempt(t *testing.T, attempts, owner, id string) {
	t.Helper()
	tag, err := f.database.Pool.Exec(context.Background(), `
		update `+attempts+` set created_at = 'infinity' where `+owner+` = $1 and state <> 'pending'
	`, id)
	if err != nil {
		t.Fatalf("make the attempt of %s unreadable: %v", id, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("made %d settled attempts of %s unreadable, want its one", tag.RowsAffected(), id)
	}
}

// rowColumns is columns of row id of table rows as one JSON object, read straight from the
// database: a GET of a post whose attempt is unreadable would fail on that attempt too.
func (f writeAnswersFixture) rowColumns(t *testing.T, rows, columns, id string) string {
	t.Helper()
	var read string
	if err := f.database.Pool.QueryRow(context.Background(), `
		select to_jsonb(r)::text from (select `+columns+` from `+rows+` where id = $1) r
	`, id).Scan(&read); err != nil {
		t.Fatalf("read %s of %s %s: %v", columns, rows, id, err)
	}
	return read
}

// A write whose answer cannot be read answers an error and leaves the post as it was: the edit
// and the delete of a comment and of an issue message, and a comment's resolve, reopen, accept
// and reject. A write that read the attempts its answer lists after its commit answered 500 for
// a write that had landed, and a retried delete then met 409 POST_DELETED. Each row reads, before
// and after the write, columns of the post's row that show whether the write landed, and wants
// them equal: for the edits and the deletes body, edited_at and deleted_at, of which each write
// moves some.
func TestAWriteWhoseAnswerCannotBeReadDoesNotLand(t *testing.T) {
	t.Parallel()
	f := newWriteAnswersFixture(t)
	issue := createInteractionIssue(t, f.handler, "TEST", "A write whose answer cannot be read", "before")
	type post struct {
		route, rows, attempts, owner string
	}
	comments := post{route: "/api/v1/comments/", rows: "comments", attempts: "comment_deliveries", owner: "comment_id"}
	messages := post{route: "/api/v1/messages/", rows: "messages", attempts: "message_deliveries", owner: "message_id"}
	remark := func(t *testing.T) string {
		return f.mentioningComment(t, issue.Key, map[string]any{"body": "Reviewers, read the spec."})
	}
	message := func(t *testing.T) string {
		return createIssueMessage(t, f.handler, issue.Key, map[string]any{
			"body": "Reviewers, read the spec.", "target": "role:reviewer", "delivery": "btw",
		}, "alice").ID
	}
	// resolvedRemark is a remark whose thread alice resolved while its attempt still read.
	resolvedRemark := func(t *testing.T) string {
		id := remark(t)
		path := "/api/v1/comments/" + id + "/resolve"
		if answer := dispatchRequest(t, f.handler, http.MethodPost, path, nil, "alice"); answer.Code != http.StatusOK {
			t.Fatalf("POST %s with its attempt readable: status=%d body=%s", path, answer.Code, answer.Body.String())
		}
		return id
	}
	anchoredSuggestion := func(t *testing.T) string {
		return f.mentioningComment(t, issue.Key, map[string]any{
			"body": "Say after.", "anchor": map[string]any{"artifact": "spec", "quote": "before"},
			"suggestion": map[string]string{"replace_with": "after"},
		})
	}
	suggestion := func(t *testing.T) string {
		return f.mentioningComment(t, issue.Key, map[string]any{
			"body": "Say ignored.", "suggestion": map[string]string{"replace_with": "ignored"},
		})
	}
	const (
		correction = "body, edited_at, deleted_at"
		resolution = "resolved, resolved_by, resolved_at"
		decision   = "suggestion->>'accepted' as accepted, resolved, resolved_by, resolved_at"
	)
	edit := map[string]any{"body": "Reviewers, read the new spec."}
	for _, test := range []struct {
		name           string
		post           post
		create         func(t *testing.T) string
		method, action string
		body           any
		// columns show whether the write landed in the post's row.
		columns string
	}{
		{name: "comment delete", post: comments, create: remark, method: http.MethodDelete, columns: correction},
		{name: "comment edit", post: comments, create: remark, method: http.MethodPatch, body: edit, columns: correction},
		{name: "message delete", post: messages, create: message, method: http.MethodDelete, columns: correction},
		{name: "message edit", post: messages, create: message, method: http.MethodPatch, body: edit, columns: correction},
		{name: "resolve", post: comments, create: remark, method: http.MethodPost, action: "/resolve", columns: resolution},
		{name: "reopen", post: comments, create: resolvedRemark, method: http.MethodPost, action: "/reopen", columns: resolution},
		{name: "accept", post: comments, create: anchoredSuggestion, method: http.MethodPost, action: "/accept", columns: decision},
		{name: "reject", post: comments, create: suggestion, method: http.MethodPost, action: "/reject", columns: decision},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := test.create(t)
			f.unreadableAttempt(t, test.post.attempts, test.post.owner, id)
			path := test.post.route + id + test.action
			write := test.method + " " + path
			before := f.rowColumns(t, test.post.rows, test.columns, id)
			answer := dispatchRequest(t, f.handler, test.method, path, test.body, "alice")
			if answer.Code != http.StatusInternalServerError {
				t.Fatalf("%s with its attempt unreadable: status=%d body=%s, want 500, the answer's read failing",
					write, answer.Code, answer.Body.String())
			}
			if after := f.rowColumns(t, test.post.rows, test.columns, id); after != before {
				t.Fatalf("%s answered %d and landed: the row reads %s, want %s, as before the write",
					write, answer.Code, after, before)
			}
		})
	}
}
