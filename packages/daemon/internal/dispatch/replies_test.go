package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// strictBody decodes r's body as Dispatch's handlers do (decodeJSON: unknown fields refused) into
// into, and fails the test on a field the route does not take.
func strictBody(t *testing.T, r *http.Request, into any) {
	t.Helper()
	if r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("%s %s body: %v", r.Method, r.URL.Path, err)
	}
}

// Each answer is written as the agent's own session, on the route and with exactly the fields that
// route's handler takes (packages/envoy/internal/dispatch/api: replyMessage, replyComment,
// createComment, the last for a reply on an ask's thread and for one in a comment's thread).
func TestAnAnswerIsPostedAsTheAgentsSession(t *testing.T) {
	type sessionActor struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		switch r.URL.Path {
		case "/api/v1/messages/m-1/reply":
			var body struct {
				Actor   sessionActor `json:"actor"`
				Attempt int          `json:"attempt"`
				Body    *string      `json:"body"`
				Error   *string      `json:"error"`
			}
			strictBody(t, r, &body)
			if body.Actor != (sessionActor{"session", "ses_tester"}) || body.Attempt != 2 || body.Body == nil || *body.Body != "Plum." || body.Error != nil {
				t.Fatalf("message reply = %+v", body)
			}
		case "/api/v1/comments/c-1/reply":
			var body struct {
				Actor   sessionActor `json:"actor"`
				Target  *string      `json:"target"`
				Attempt int          `json:"attempt"`
				Body    *string      `json:"body"`
				Error   *string      `json:"error"`
			}
			strictBody(t, r, &body)
			if body.Actor != (sessionActor{"session", "ses_tester"}) || body.Target == nil || *body.Target != "role:legion-legion-legion-9-tester" ||
				body.Attempt != 1 || body.Body == nil || *body.Body != "Pear." {
				t.Fatalf("comment reply = %+v", body)
			}
		case "/api/v1/issues/LEGION-9/comments":
			var body struct {
				Body    string        `json:"body"`
				ReplyTo *string       `json:"reply_to"`
				AskID   *string       `json:"ask_id"`
				Actor   *sessionActor `json:"actor"`
			}
			strictBody(t, r, &body)
			if body.Actor == nil || *body.Actor != (sessionActor{"session", "ses_architect"}) || body.AskID == nil || *body.AskID != "a-1" ||
				body.ReplyTo != nil || body.Body != "Fig." {
				t.Fatalf("ask comment = %+v", body)
			}
		case "/api/v1/issues/LEGION-10/comments":
			var body struct {
				Body    string        `json:"body"`
				ReplyTo *string       `json:"reply_to"`
				AskID   *string       `json:"ask_id"`
				Actor   *sessionActor `json:"actor"`
			}
			strictBody(t, r, &body)
			if body.Actor == nil || *body.Actor != (sessionActor{"session", "ses_tester"}) || body.ReplyTo == nil || *body.ReplyTo != "c-6" ||
				body.AskID != nil || body.Body != "Kiwi." {
				t.Fatalf("thread comment = %+v", body)
			}
		default:
			t.Fatalf("request %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		got = append(got, r.URL.Path)
		writeJSONStatus(t, w, http.StatusCreated, map[string]any{"id": "reply"})
	}))
	defer server.Close()
	client := New(server.URL, BearerTransport(dispatchToken, nil))
	ctx := context.Background()
	if err := client.ReplyToMessage(ctx, "m-1", 2, "ses_tester", "Plum."); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplyToComment(ctx, "c-1", "role:legion-legion-legion-9-tester", 1, "ses_tester", "Pear."); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplyOnAsk(ctx, "LEGION-9", "a-1", "ses_architect", "Fig."); err != nil {
		t.Fatal(err)
	}
	if err := client.ReplyInThread(ctx, "LEGION-10", "c-6", "ses_tester", "Kiwi."); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("routes called = %v, want all four", got)
	}
}

// An ask reads with its author and every reply, for the loop's read-back before an ask-thread post.
func TestAnAskReadsWithItsAuthorAndReplies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/asks/a-1" {
			t.Fatalf("request = %s %s, want GET /api/v1/asks/a-1", r.Method, r.URL.Path)
		}
		var buf bytes.Buffer
		buf.WriteString(`{"ask":{"id":"a-1","issue_key":"LEGION-9","author":{"kind":"session","id":"ses_architect"},"question":"Which?"},` +
			`"replies":[{"id":"c-1","author":{"kind":"user","id":"p@example.com"},"body":"Why?"}],"edits":[],"answers":[],"followers":[]}`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buf.Bytes())
	}))
	defer server.Close()
	ask, err := New(server.URL, BearerTransport(dispatchToken, nil)).AskThread(context.Background(), "a-1")
	if err != nil {
		t.Fatal(err)
	}
	if ask.ID != "a-1" || ask.IssueKey == nil || *ask.IssueKey != "LEGION-9" || ask.Author != (Actor{"session", "ses_architect"}) ||
		len(ask.Replies) != 1 || ask.Replies[0].Body != "Why?" || ask.Replies[0].Author.Kind != "user" {
		t.Fatalf("ask = %+v", ask)
	}
}

// A comment reads with every reply in its thread, oldest first, each with its id, its author and
// its body: what the answer loop decides a reply in that thread by, and reads back before it
// posts there.
func TestACommentReadsWithItsThread(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearer(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/comments/c-1" {
			t.Fatalf("request = %s %s, want GET /api/v1/comments/c-1", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"comment":{"id":"c-1","issue_key":"LEGION-9","artifact_id":null,"author":{"kind":"session","id":"ses_tester"},` +
			`"body":"Tested: plum.","reply_to":null,"answers":null,"ask_id":null,"created_at":"2026-10-26T12:00:00.123456Z","mentions":[],"body_tree":{}},` +
			`"replies":[{"id":"c-2","author":{"kind":"user","id":"p@example.com"},"body":"Why plum?","reply_to":"c-1","answers":"c-1",` +
			`"created_at":"2026-10-26T12:01:00Z"},{"id":"c-3","author":{"kind":"session","id":"ses_reviewer"},"body":"Because.",` +
			`"reply_to":"c-1","answers":"c-2","created_at":"2026-10-26T12:02:00Z"}]}`))
	}))
	defer server.Close()
	thread, err := New(server.URL, BearerTransport(dispatchToken, nil)).Thread(context.Background(), "c-1")
	if err != nil {
		t.Fatal(err)
	}
	want := Thread{
		Root: Comment{ID: "c-1", Author: Actor{"session", "ses_tester"}, Body: "Tested: plum."},
		Replies: []Comment{
			{ID: "c-2", Author: Actor{"user", "p@example.com"}, Body: "Why plum?"},
			{ID: "c-3", Author: Actor{"session", "ses_reviewer"}, Body: "Because."},
		},
	}
	if !reflect.DeepEqual(thread, want) {
		t.Fatalf("thread = %+v\nwant %+v", thread, want)
	}
}
