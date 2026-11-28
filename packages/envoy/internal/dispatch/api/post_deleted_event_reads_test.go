package api

// post_deleted_event_reads_test.go holds R2 and R3 of dispatch://INFRA-1523 on the reads of the
// event log the census found no test of after a post's delete: each kind of a post's own event
// that a test of the issue's feed had not written before the delete (rows 3.2-3.6, and the
// reply's own body of 3.11), and each read of the log other than the issue's feed (1.26-1.28),
// with the issue's feed under a validator given before a message's delete (2.3).
//
// Every one of them reads through readEventRows, which masks the events of a deleted post at the
// read (attachPostMarks): the stored payload is never rewritten.
//
// The row numbers in subtest names and comments are the rows of the issue's surface census,
// dispatch://INFRA-1523/artifact/surface-census-md.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Each kind of event a post's own writes append carries the post's body. Every row writes one
// such kind, reads it in the issue's feed with the post's words (the control), deletes the post,
// and reads the feed again: every event of the post gives none of its words and carries the mark.
// The one copy of the words that stays is the document event of an accepted suggestion, which is
// no event of the post and keeps its payload as written (R3).
//
// Each row relies on attachPostMarks taking the body, body_truncated and suggestion out of
// every event of the deleted post (postBodyEventKinds) and adding its mark.
func TestEveryKindOfADeletedPostsOwnEventReadsWithTheMarkAndNoBody(t *testing.T) {
	t.Parallel()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	defer listener.Close()
	handler, database := newTargetedMessageHandler(t, listener.URL)
	createInteractionIssue(t, handler, "TEST", "The project's first issue", "before")
	comment := func(t *testing.T, issue string, input map[string]any) (model.Comment, func()) {
		created := createThreadComment(t, handler, issue, input, "alice")
		return created, func() { deleteThreadComment(t, handler, created.ID, "alice") }
	}
	act := func(t *testing.T, id, action string) {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+id+"/"+action, map[string]any{}, "alice"); response.Code != http.StatusOK {
			t.Fatalf("%s comment %s: status=%d body=%s", action, id, response.Code, response.Body.String())
		}
	}

	rows := []struct {
		kind string
		// carried is each key beside the body that the post's events carried while it stood and
		// that gives part of it: a suggestion's replacement text.
		carried []string
		// keeps is how many of the issue's events keep the words after the delete: the
		// artifact.version of the accept, which named the version it made after the body.
		keeps int
		// write makes the post on issue and writes the row's kind of event about it, and answers
		// the post, the id of the actor that deletes it and the delete.
		write func(t *testing.T, issue, phrase string) (id, by string, withdraw func())
	}{
		// 3.2: the edit's event carries the new body (editComment).
		{kind: "comment.edited", write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			created, withdraw := comment(t, issue, map[string]any{"body": "The comment as first written."})
			editThreadComment(t, handler, created.ID, phrase+" as corrected.", "alice")
			return created.ID, "alice", withdraw
		}},
		// 3.3: a session's answer to the comment that mentioned it is its own comment, whose
		// one event is comment.answered (replyComment); the session deletes it.
		{kind: "comment.answered", write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			mentioned := decodeMentionedComment(t, postMentionedComment(t, handler, issue, map[string]any{
				"body": "Can this ship?", "mentions": []map[string]any{{"target": "session:s1"}},
			}))
			settledCommentDeliveries(t, database, mentioned.ID)
			session := map[string]any{"kind": "session", "id": "s1"}
			answered := bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+mentioned.ID+"/reply", map[string]any{
				"actor": session, "attempt": 1, "body": phrase + ", it can ship.",
			})
			if answered.Code != http.StatusCreated {
				t.Fatalf("the session's answer: status=%d body=%s", answered.Code, answered.Body.String())
			}
			reply := decodeBody[model.Comment](t, answered)
			return reply.ID, "s1", func() {
				if deleted := bearerRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+reply.ID, map[string]any{"actor": session}); deleted.Code != http.StatusOK {
					t.Fatalf("the session deletes its answer: status=%d body=%s", deleted.Code, deleted.Body.String())
				}
			}
		}},
		// 3.4: a reopen's event carries the comment as it stands, body and all.
		{kind: "comment.reopened", write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			created, withdraw := comment(t, issue, map[string]any{"body": phrase + " to reopen."})
			act(t, created.ID, "resolve")
			act(t, created.ID, "reopen")
			return created.ID, "alice", withdraw
		}},
		// 3.5: an anchor refresh rewrites the comment's event payload from the stored row
		// (docs/anchor_refresh.go).
		{kind: "comment.anchor_refreshed", write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			created, withdraw := comment(t, issue, map[string]any{
				"body": phrase + " on the fox.", "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
			})
			// Another suggestion's accept takes the anchored text out of the document, which
			// refreshes the anchor of this comment.
			competing, _ := comment(t, issue, map[string]any{
				"body": "Rename the animal.", "anchor": map[string]any{"artifact": "spec", "quote": "brown fox"},
				"suggestion": map[string]string{"replace_with": "brown dog"},
			})
			act(t, competing.ID, "accept")
			return created.ID, "alice", withdraw
		}},
		// 3.6: the accept's event carries the body and the suggestion; the version the accept
		// named after the body keeps its artifact.version event as written (R3).
		{kind: "suggestion.accepted", carried: []string{"suggestion"}, keeps: 1, write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			created, withdraw := comment(t, issue, map[string]any{
				"body": "Say this instead, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": "quick"},
				"suggestion": map[string]string{"replace_with": "slow"},
			})
			act(t, created.ID, "accept")
			return created.ID, "alice", withdraw
		}},
		// 3.6: the reject's event carries the body and the suggestion.
		{kind: "suggestion.rejected", carried: []string{"suggestion"}, write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			created, withdraw := comment(t, issue, map[string]any{
				"body": "Say this instead, " + phrase + ".", "anchor": map[string]any{"artifact": "spec", "quote": "quick"},
				"suggestion": map[string]string{"replace_with": "slow"},
			})
			act(t, created.ID, "reject")
			return created.ID, "alice", withdraw
		}},
		// 3.11: a reply's message.answered carries its own body, which its own delete takes;
		// the preview it froze of the message it answers is not the deleted post's.
		{kind: "message.answered", write: func(t *testing.T, issue, phrase string) (string, string, func()) {
			root := createIssueMessage(t, handler, issue, map[string]any{"body": "Is the build green?"}, "bob")
			reply := createIssueMessage(t, handler, issue, map[string]any{"body": phrase + ", it is green.", "in_reply_to": root.ID}, "alice")
			return reply.ID, "alice", func() { deleteIssueMessage(t, handler, reply.ID, "alice") }
		}},
	}
	for index, row := range rows {
		t.Run(row.kind, func(t *testing.T) {
			issue := createProjectIssue(t, handler, "TEST", "The events of "+row.kind, "The quick brown fox")
			phrase := "zarquonkind" + string(rune('a'+index))
			id, by, withdraw := row.write(t, issue, phrase)
			written := 0
			for _, event := range decodedIssueEvents(t, handler, issue) {
				if event.Type != row.kind || event.Payload["id"] != id {
					continue
				}
				written++
				if encoded, _ := json.Marshal(event.Payload); !strings.Contains(string(encoded), phrase) {
					t.Fatalf("the %s event of %s before the delete gives no %q, so the read after it cannot fail: %s", row.kind, id, phrase, encoded)
				}
			}
			if written == 0 {
				t.Fatalf("the issue holds no %s event of %s before the delete", row.kind, id)
			}

			withdraw()

			kept, read := 0, 0
			for _, event := range issueEvents(t, handler, issue) {
				payload := map[string]any{}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatalf("decode the %s payload %s: %v", event.Type, event.Payload, err)
				}
				if payload["id"] == id {
					if event.Type == row.kind {
						read++
					}
					if strings.Contains(string(event.Payload), phrase) {
						t.Fatalf("the %s event of %s after the delete gives the deleted text %q: %s", event.Type, id, phrase, event.Payload)
					}
					requireNoBody(t, event.Type, payload, row.carried...)
					requireDeletedMark(t, event.Type, payload, by)
					continue
				}
				if strings.Contains(string(event.Payload), phrase) {
					kept++
				}
			}
			if read != written {
				t.Fatalf("the issue holds %d %s events of %s after the delete, want the %d it held before, each kept in its place", read, row.kind, id, written)
			}
			if kept != row.keeps {
				t.Fatalf("%d events of the issue keep the deleted text %q, want %d: only an event of no post keeps its payload as written", kept, phrase, row.keeps)
			}
		})
	}
}

// Every read of the event log other than an issue's feed, made while a comment on a project
// document and a message on an issue stand and again after their author deletes them: the
// document's feed, the whole log, a live socket's replay, and the issue's feed under the
// validator it gave before the message's delete. Before the delete each read gives the posts'
// words; after it each gives none of them, and every event of either post carries the mark.
func TestEveryReadOfTheEventLogGivesADeletedPostsEventsWithTheMarkAndNoBody(t *testing.T) {
	// Not parallel: it reads the event log through awaitHorizonRead, whose horizontest.Listen
	// changes slog's default logger for the whole process (slog.SetDefault).
	rig := newLiveSocketRig(t, testServerOptions{}, nil)
	handler, database := rig.handler, rig.database
	issue := createInteractionIssue(t, handler, "TEST", "Posts the log carries", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Logged notes", "# Logged notes\n\nAlpha.\n")
	since := eventHead(t, database)
	type logged struct {
		post   postCopies
		phrase string
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments",
		map[string]any{"body": "zarquonloggedcomment on the document."}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("comment on the document: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := logged{
		post:   postCopies{kind: "comment", id: decodeBody[model.Comment](t, created).ID, by: "alice", posts: 1},
		phrase: "zarquonloggedcomment",
	}
	message := logged{
		post:   postCopies{kind: "message", id: createIssueMessage(t, handler, issue.Key, map[string]any{"body": "zarquonloggedmessage on the issue."}, "alice").ID, by: "alice", posts: 1},
		phrase: "zarquonloggedmessage",
	}

	// socketReplay is a socket of alice's subscribed to the event feed after since, and every
	// event it is sent until one of each kind of want has arrived, as one JSON array.
	socketReplay := func(t *testing.T, sub string, want ...string) []byte {
		t.Helper()
		client := rig.dial(t, asUser("alice"))
		subscribeEvents(t, client, sub, &since)
		missing := map[string]bool{}
		for _, kind := range want {
			missing[kind] = true
		}
		events := []json.RawMessage{}
		for len(missing) > 0 {
			frame, event := nextEvent(t, client, sub)
			events = append(events, frame.Event)
			delete(missing, event.Type)
		}
		encoded, err := json.Marshal(events)
		if err != nil {
			t.Fatalf("encode the replayed events: %v", err)
		}
		return encoded
	}
	// logRead is GET /api/v1/events/log, read once the horizon holds an event of each kind of
	// want, as one JSON array of the log's events.
	logRead := func(t *testing.T, want ...string) []byte {
		t.Helper()
		rows, byID := readEventLogBehindHorizon(t, handler, database, "the event log holding "+strings.Join(want, " and "), func(rows []eventLogRow) bool {
			seen := map[string]bool{}
			for _, row := range rows {
				seen[row.Type] = true
			}
			for _, kind := range want {
				if !seen[kind] {
					return false
				}
			}
			return true
		})
		events := make([]json.RawMessage, len(rows))
		for index, row := range rows {
			events[index] = byID[row.ID]
		}
		encoded, err := json.Marshal(events)
		if err != nil {
			t.Fatalf("encode the event log: %v", err)
		}
		return encoded
	}
	// feedETag is the validator the issue's feed gave before the delete, which the read after it
	// is made under.
	var feedETag string
	feedRead := func(t *testing.T, deleted bool) []byte {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil)
		request.Header.Set("X-Dispatch-User", "alice")
		if deleted {
			request.Header.Set("If-None-Match", feedETag)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		etag := response.Header().Get("ETag")
		if response.Code != http.StatusOK || etag == "" || (deleted && etag == feedETag) {
			t.Fatalf("the issue's feed (after the delete: %t, under %q): status=%d ETag=%q, want 200 under a new ETag", deleted, feedETag, response.Code, etag)
		}
		feedETag = etag
		return response.Body.Bytes()
	}

	rows := []struct {
		read  string
		posts []logged
		// answer is the read made before the delete, or after it where deleted is set.
		answer func(t *testing.T, deleted bool) []byte
	}{
		// attachPostMarks, through readEvents of the document's owner.
		{"1.26 GET /api/v1/artifacts/{id}/events", []logged{comment}, func(t *testing.T, _ bool) []byte {
			response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/events", nil, "alice")
			if response.Code != http.StatusOK {
				t.Fatalf("the document's feed: status=%d body=%s", response.Code, response.Body.String())
			}
			return response.Body.Bytes()
		}},
		// attachPostMarks, through readEventsAfterID.
		{"1.27 GET /api/v1/events/log", []logged{comment, message}, func(t *testing.T, deleted bool) []byte {
			if deleted {
				return logRead(t, "comment.deleted", "message.deleted")
			}
			return logRead(t, "comment.created", "message.created")
		}},
		// attachPostMarks, through replayThrough's readEventsAfterID and the relay's.
		{"1.28 GET /ws/live, the events view's replay", []logged{comment, message}, func(t *testing.T, deleted bool) []byte {
			if deleted {
				return socketReplay(t, "after", "comment.deleted", "message.deleted")
			}
			return socketReplay(t, "before", "comment.created", "message.created")
		}},
		// attachPostMarks, through readEvents of the issue; the delete appends message.deleted,
		// which moves the validator (eventFeedETag).
		{"2.3 GET /api/v1/issues/{key}/events under the ETag given before a message's delete", []logged{message}, feedRead},
	}
	for _, row := range rows {
		answer := row.answer(t, false)
		for _, post := range row.posts {
			requireStandingPostRead(t, row.read, answer, post.phrase, post.post)
		}
	}

	deleteThreadComment(t, handler, comment.post.id, "alice")
	deleteIssueMessage(t, handler, message.post.id, "alice")

	for _, row := range rows {
		t.Run(row.read, func(t *testing.T) {
			answer := row.answer(t, true)
			for _, post := range row.posts {
				requireDeletedPostRead(t, row.read, answer, post.phrase, post.post)
			}
		})
	}
}
