package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// quoteSession is the one live session a quoting test's listener holds: the session its mentions,
// its targeted messages, its direct messages and its broadcast reach.
const quoteSession = "s1"

// quotedWord is the word each post a quoting test answers holds twice in its first block,
// quotedTwice.
const (
	quotedWord  = "alpha"
	quotedTwice = "Ship alpha now, alpha later."
)

// farParagraph is a paragraph whose word zarquonfar stands past the runes a block's stored
// excerpt keeps (refs.BlockExcerptRunes), so only the block's whole text holds it.
var farParagraph = strings.Repeat("lorem ipsum ", refs.BlockExcerptRunes/len("lorem ipsum ")+1) + "zarquonfar stands past the excerpt."

// quoteListener is a fake Envoy listener holding quoteSession live, as sessionListener does, that
// records each send under a lock: a mention and a broadcast's copy are sent after their write has
// answered, while the test reads what was sent.
type quoteListener struct {
	mu    sync.Mutex
	sends []map[string]any
}

// newQuoteHandler is a test server whose Envoy listener is a quoteListener.
func newQuoteHandler(t *testing.T) (http.Handler, *store.Store, *quoteListener) {
	t.Helper()
	listener := &quoteListener{}
	record := `{"session_id":"` + quoteSession + `","title":"planner","dir":"/w/legion","machine_id":"m1","roles":[],"capabilities":["aside","btw","steer"],"last_seen":42}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/registry/"+quoteSession:
			_, _ = w.Write([]byte(record))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			_, _ = w.Write([]byte("[" + record + "]"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/send":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode listener send: %v", err)
			}
			listener.mu.Lock()
			listener.sends = append(listener.sends, request)
			listener.mu.Unlock()
			_, _ = w.Write([]byte(`{"event_id":"envelope-1","recipient":"` + quoteSession + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	handler, database := newTargetedMessageHandler(t, server.URL)
	return handler, database, listener
}

// frames is the event of each delivery frame the listener was sent, in the order it was sent.
func (l *quoteListener) frames(t *testing.T) []reviewEvent {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	frames := make([]reviewEvent, 0, len(l.sends))
	for _, send := range l.sends {
		payload, _ := send["payload"].(string)
		if payload == "" {
			continue
		}
		var frame struct {
			Event reviewEvent `json:"event"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			t.Fatalf("decode delivery frame %q: %v", payload, err)
		}
		frames = append(frames, frame.Event)
	}
	return frames
}

// quotedWords is what a reply_block says of the words its reply quotes: quote and occurrence as
// the reply stored them, or, with none set, neither member.
type quotedWords struct {
	quote      string
	occurrence int
	none       bool
}

// quoting is a reply_block that quotes quote at occurrence; unquoted is one with neither member,
// the block of a reply that quotes nothing or whose answered post is deleted.
func quoting(quote string, occurrence int) quotedWords {
	return quotedWords{quote: quote, occurrence: occurrence}
}

var unquoted = quotedWords{none: true}

func (w quotedWords) String() string {
	if w.none {
		return "neither quote nor occurrence"
	}
	return fmt.Sprintf("quote %q at occurrence %d", w.quote, w.occurrence)
}

// replyBlocksIn is every reply_block body carries for post id: the member of each object of the
// JSON whose id is id, at any depth - the post itself, a row of a list, a reply in its thread, an
// event's payload - nil where that object's reply_block is null.
func replyBlocksIn(t *testing.T, step string, body []byte, id string) []map[string]any {
	t.Helper()
	var answer any
	if err := json.Unmarshal(body, &answer); err != nil {
		t.Fatalf("%s: decode %s: %v", step, body, err)
	}
	var blocks []map[string]any
	var walk func(value any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if block, carried := value["reply_block"]; carried && value["id"] == id {
				held, _ := block.(map[string]any)
				blocks = append(blocks, held)
			}
			for _, member := range value {
				walk(member)
			}
		case []any:
			for _, item := range value {
				walk(item)
			}
		}
	}
	walk(answer)
	return blocks
}

// requireQuoted fails unless body carries reply id's reply_block, and each copy of it body
// carries says want of the words the reply quotes.
func requireQuoted(t *testing.T, step string, body []byte, id string, want quotedWords) {
	t.Helper()
	blocks := replyBlocksIn(t, step, body, id)
	if len(blocks) == 0 {
		t.Fatalf("%s: no reply_block of reply %s in %s", step, id, body)
	}
	for _, block := range blocks {
		if block == nil {
			t.Fatalf("%s: reply %s's reply_block is null in %s, want the block it answers", step, id, body)
		}
		quote, quoted := block["quote"]
		occurrence, counted := block["occurrence"]
		if want.none && !quoted && !counted || !want.none && quote == want.quote && occurrence == float64(want.occurrence) {
			continue
		}
		t.Fatalf("%s: reply %s's reply_block = %v, want %s", step, id, block, want)
	}
}

// requireBlockMark fails unless each copy of reply id's reply_block body carries holds mark
// (edited_at, deleted_at), the mark of the post the reply answers.
func requireBlockMark(t *testing.T, step string, body []byte, id, mark string) {
	t.Helper()
	for _, block := range replyBlocksIn(t, step, body, id) {
		if block == nil || block[mark] == nil {
			t.Fatalf("%s: reply %s's reply_block = %v, want the %s of the post it answers", step, id, block, mark)
		}
	}
}

// quoteRead is one answer that carries a reply's reply_block, read for the reply's id.
type quoteRead struct {
	name string
	read func(t *testing.T, id string) []byte
}

// getRead is the quoteRead of a GET of path, where {id} stands for the reply's id.
func getRead(handler http.Handler, name, path string) quoteRead {
	return quoteRead{name, func(t *testing.T, id string) []byte {
		t.Helper()
		return getRaw(t, handler, strings.ReplaceAll(path, "{id}", id))
	}}
}

func readThreadPageAnswer(t *testing.T, handler http.Handler, path string) []byte {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
	decodeThreadPage(t, response.Body.Bytes(), response.Code, path)
	return response.Body.Bytes()
}

func getThreadPageRead(handler http.Handler, path string) quoteRead {
	return quoteRead{"GET " + path, func(t *testing.T, _ string) []byte {
		t.Helper()
		return readThreadPageAnswer(t, handler, path)
	}}
}

// feedPayloads is the payload of each event of eventType about post id in the event feed at path
// (GET /api/v1/issues/{key}/events, GET /api/v1/artifacts/{id}/events), oldest first.
func feedPayloads(t *testing.T, handler http.Handler, path, eventType, id string) []json.RawMessage {
	t.Helper()
	var events []reviewEvent
	if err := json.Unmarshal(getRaw(t, handler, path), &events); err != nil {
		t.Fatalf("decode the events at %s: %v", path, err)
	}
	return eventsAbout(events, eventType, id)
}

// eventsAbout is the payload of each of events of eventType whose payload names post id.
func eventsAbout(events []reviewEvent, eventType, id string) []json.RawMessage {
	var payloads []json.RawMessage
	for _, event := range events {
		var about struct {
			ID string `json:"id"`
		}
		if event.Type == eventType && json.Unmarshal(event.Payload, &about) == nil && about.ID == id {
			payloads = append(payloads, event.Payload)
		}
	}
	return payloads
}

// eventRead is the quoteRead of the payload of a reply's first eventType event in the feed at path.
func eventRead(handler http.Handler, path, eventType string) quoteRead {
	return quoteRead{"its " + eventType + " in " + path, func(t *testing.T, id string) []byte {
		t.Helper()
		payloads := feedPayloads(t, handler, path, eventType, id)
		if len(payloads) == 0 {
			t.Fatalf("no %s event about %s in %s", eventType, id, path)
		}
		return payloads[0]
	}}
}

// frameRead is the quoteRead of the payload of the first delivery frame the listener was sent for
// a reply: its message.created, the event a frame replays a message as.
func frameRead(listener *quoteListener) quoteRead {
	return quoteRead{"its delivery frame's message.created", func(t *testing.T, id string) []byte {
		t.Helper()
		payloads := eventsAbout(listener.frames(t), "message.created", id)
		if len(payloads) == 0 {
			t.Fatalf("no delivery frame of %s", id)
		}
		return payloads[0]
	}}
}

// createdID is the id of the post a write answered 201 with, failing t on any other answer.
func createdID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusCreated {
		t.Fatalf("the write: status=%d body=%s, want 201", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		ID string `json:"id"`
	}](t, response).ID
}

// withQuote is input with the block, quote and occurrence a reply names, the block under field
// (reply_to_block, in_reply_to_block). An occurrence below 0 is none.
func withQuote(input map[string]any, field, block, quote string, occurrence int) map[string]any {
	input[field] = block
	input["quote"] = quote
	if occurrence >= 0 {
		input["occurrence"] = occurrence
	}
	return input
}

// callbackAs is the body of a session's reply to attempt 1 of the delivery it was sent, with
// input's fields.
func callbackAs(session string, input map[string]any) map[string]any {
	input["actor"] = map[string]any{"kind": "session", "id": session}
	input["attempt"] = 1
	return input
}

// R8 through each of its six routes: a reply to a block of a comment on an issue, of a comment on
// a project document, of an ask's part and of a message, each holding the word twice, quotes its
// second match, and every answer that carries the reply's block - the write's 201, the read of the
// reply, its owner's list and the reply's own event or delivery frame - carries the quote and the
// occurrence. The quote is matched against the block's plain text with its overlapping matches
// counted: `aa` at occurrence 1 in `aaa`, the bold words of a heading quoted with no `#` and no
// `**`, and a word past the 480th character of a message block, which the block's excerpt does not
// hold. A quote with one match and no occurrence stores 0, and a reply that quotes nothing carries
// neither member. An edit of the answered post that drops the word changes neither value, and the
// block's edited_at says the text changed.
func TestAReplyToABlockStoresTheWordsItQuotes(t *testing.T) {
	t.Parallel()
	handler, _, listener := newQuoteHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Replies that quote", "before").Key
	document := createProjectDocument(t, handler, "TEST", "Quoted notes", "# Quoted notes\n\nalpha bravo\n").ID
	headed := "\n\n## Ship the **bold words**\n\naaa"
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue, map[string]any{
		"body": quotedTwice + headed, "mentions": []map[string]any{{"target": "session:" + quoteSession}},
	})).ID
	documentComment := createdID(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/comments",
		map[string]any{"body": quotedTwice}, "alice"))
	ask := createPartAsk(t, handler, issue, "Ship alpha now, or alpha later?").ID
	message := createIssueMessage(t, handler, issue, map[string]any{"body": quotedTwice + "\n\n" + farParagraph}, "alice").ID
	targeted := createIssueMessage(t, handler, issue, map[string]any{
		"body": quotedTwice, "target": "session:" + quoteSession, "delivery": "btw",
	}, "alice").ID
	direct := createdID(t, postAgentMessage(t, handler, quoteSession, map[string]any{"body": quotedTwice, "delivery": "btw"}, "alice"))

	issueFeed := "/api/v1/issues/" + issue + "/events"
	commentReads := []quoteRead{
		getRead(handler, "GET /api/v1/comments/{id}", "/api/v1/comments/{id}"),
		getRead(handler, "GET /api/v1/issues/{key}/comments", "/api/v1/issues/"+issue+"/comments"),
		eventRead(handler, issueFeed, "comment.created"),
	}
	commentPage := getThreadPageRead(handler, "/api/v1/issues/"+issue+"/comments?order=activity")
	commentPostReads := append([]quoteRead{commentPage}, commentReads...)
	messageReads := []quoteRead{
		getRead(handler, "GET /api/v1/issues/{key}/messages/{id}", "/api/v1/issues/"+issue+"/messages/{id}"),
		getRead(handler, "GET /api/v1/issues/{key}/messages", "/api/v1/issues/"+issue+"/messages"),
		getThreadPageRead(handler, "/api/v1/issues/"+issue+"/messages?order=activity"),
		eventRead(handler, issueFeed, "message.answered"),
	}
	post := func(path string, input map[string]any) func(*testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodPost, path, input, "alice")
		}
	}
	bearer := func(path string, input map[string]any) func(*testing.T) *httptest.ResponseRecorder {
		return func(t *testing.T) *httptest.ResponseRecorder {
			t.Helper()
			return bearerRequest(t, handler, http.MethodPost, path, callbackAs(quoteSession, input))
		}
	}
	issueComments, issueMessages := "/api/v1/issues/"+issue+"/comments", "/api/v1/issues/"+issue+"/messages"
	rows := []struct {
		name  string
		send  func(*testing.T) *httptest.ResponseRecorder
		want  quotedWords
		reads []quoteRead
		// edited is a reply whose answered post the edit below rewrites without the quoted word.
		edited bool
	}{
		{
			name: "POST /api/v1/issues/{key}/comments, a reply to a comment's block",
			send: post(issueComments, withQuote(map[string]any{"body": "On the second alpha.", "reply_to": comment}, "reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), reads: commentPostReads, edited: true,
		},
		{
			name: "POST /api/v1/issues/{key}/comments, a reply to an ask's part",
			send: post(issueComments, withQuote(map[string]any{"body": "On the ask's second alpha.", "ask_id": ask}, "reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), reads: append([]quoteRead{
				getRead(handler, "GET /api/v1/asks/{id}", "/api/v1/asks/"+ask),
				getThreadPageRead(handler, "/api/v1/issues/"+issue+"/asks?order=activity&thread=1"),
			}, commentReads...),
		},
		{
			name: "POST /api/v1/artifacts/{id}/comments",
			send: post("/api/v1/artifacts/"+document+"/comments", withQuote(map[string]any{"body": "On the document's second alpha.", "reply_to": documentComment}, "reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), edited: true,
			reads: []quoteRead{
				getRead(handler, "GET /api/v1/comments/{id}", "/api/v1/comments/{id}"),
				getRead(handler, "GET /api/v1/artifacts/{id}/comments", "/api/v1/artifacts/"+document+"/comments"),
				eventRead(handler, "/api/v1/artifacts/"+document+"/events", "comment.created"),
			},
		},
		{
			name: "POST /api/v1/comments/{id}/reply",
			send: bearer("/api/v1/comments/"+comment+"/reply", withQuote(map[string]any{"body": "The session on the second alpha."}, "reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), edited: true,
			reads: []quoteRead{commentReads[0], commentReads[1], eventRead(handler, issueFeed, "comment.answered"), commentPage},
		},
		{
			name: "POST /api/v1/issues/{key}/messages",
			send: post(issueMessages, withQuote(map[string]any{"body": "On the message's second alpha.", "in_reply_to": message}, "in_reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), reads: messageReads, edited: true,
		},
		{
			name: "POST /api/v1/messages/{id}/reply",
			send: bearer("/api/v1/messages/"+targeted+"/reply", withQuote(map[string]any{"body": "The session on the second alpha."}, "in_reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1), reads: messageReads, edited: true,
		},
		{
			// A message of a conversation with no issue takes no edit (NOT_AN_ISSUE_MESSAGE).
			name: "POST /api/v1/agents/{session_id}/messages",
			send: post("/api/v1/agents/"+quoteSession+"/messages", withQuote(map[string]any{"body": "On the direct second alpha.", "delivery": "btw", "in_reply_to": direct}, "in_reply_to_block", "b1", quotedWord, 1)),
			want: quoting(quotedWord, 1),
			reads: []quoteRead{
				getRead(handler, "GET /api/v1/messages/{id}", "/api/v1/messages/{id}"),
				getRead(handler, "GET /api/v1/agents/{session_id}/messages", "/api/v1/agents/"+quoteSession+"/messages"),
				frameRead(listener),
			},
		},
		{
			name: "the first of two matches, occurrence 0",
			send: post(issueComments, withQuote(map[string]any{"body": "On the first alpha.", "reply_to": comment}, "reply_to_block", "b1", quotedWord, 0)),
			want: quoting(quotedWord, 0), reads: commentReads, edited: true,
		},
		{
			name: "a quote the block holds once, with no occurrence",
			send: post(issueComments, withQuote(map[string]any{"body": "On now.", "reply_to": comment}, "reply_to_block", "b1", "now", -1)),
			want: quoting("now", 0), reads: commentReads, edited: true,
		},
		{
			name: "aa at occurrence 1 of aaa, the overlapping match",
			send: post(issueComments, withQuote(map[string]any{"body": "On the second aa.", "reply_to": comment}, "reply_to_block", "b3", "aa", 1)),
			want: quoting("aa", 1), reads: commentReads, edited: true,
		},
		{
			name: "a heading's bold words quoted as plain text",
			send: post(issueComments, withQuote(map[string]any{"body": "On the bold words.", "reply_to": comment}, "reply_to_block", "b2", "Ship the bold words", -1)),
			want: quoting("Ship the bold words", 0), reads: commentReads, edited: true,
		},
		{
			name: "a quote past the 480th character of a message block",
			send: post(issueMessages, withQuote(map[string]any{"body": "On the far word.", "in_reply_to": message}, "in_reply_to_block", "b2", "zarquonfar", -1)),
			want: quoting("zarquonfar", 0), reads: messageReads, edited: true,
		},
		{
			name: "a reply naming a block and quoting none of it",
			send: post(issueComments, map[string]any{"body": "On the block.", "reply_to": comment, "reply_to_block": "b1"}),
			want: unquoted, reads: commentReads, edited: true,
		},
	}
	ids := make([]string, len(rows))
	for index, row := range rows {
		written := row.send(t)
		ids[index] = createdID(t, written)
		requireQuoted(t, row.name+": the write's 201", written.Body.Bytes(), ids[index], row.want)
		for _, read := range row.reads {
			requireQuoted(t, row.name+": "+read.name, read.read(t, ids[index]), ids[index], row.want)
		}
	}

	// Each edit drops the quoted word from the block the replies answer, and changes neither value.
	editThreadComment(t, handler, comment, "Ship it now, later."+headed, "alice")
	editThreadComment(t, handler, documentComment, "Ship it now, later.", "alice")
	editIssueMessage(t, handler, message, "Ship it now, later.\n\n"+farParagraph, "alice")
	editIssueMessage(t, handler, targeted, "Ship it now, later.", "alice")
	for index, row := range rows {
		for _, read := range row.reads {
			step := row.name + ": " + read.name + " after the edit of the post it answers"
			body := read.read(t, ids[index])
			requireQuoted(t, step, body, ids[index], row.want)
			if row.edited && !strings.HasPrefix(read.name, "its ") {
				requireBlockMark(t, step, body, ids[index], "edited_at")
			}
		}
	}
}

// R8's refusals, through each of its six routes: occurrence without quote, quote without the
// block field, an empty quote and an occurrence below 0 are each refused 400 with the code the
// route gives a bad block field, its message naming the field; a quote the block does not hold and
// an occurrence past its last match are 404 TARGET_NOT_FOUND; a word the block holds twice, with
// no occurrence, is 409 TARGET_AMBIGUOUS, which names the count and asks for an occurrence. No
// refused write stores a reply. The control: each route then takes the quote it was refused once
// it names the occurrence.
func TestAReplyQuoteIsRefusedWhereTheBlockDoesNotHoldIt(t *testing.T) {
	t.Parallel()
	handler, database, _ := newQuoteHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Quotes refused", "before").Key
	document := createProjectDocument(t, handler, "TEST", "Refused notes", "# Refused notes\n\nalpha bravo\n").ID
	comment := decodeMentionedComment(t, postMentionedComment(t, handler, issue, map[string]any{
		"body": quotedTwice, "mentions": []map[string]any{{"target": "session:" + quoteSession}},
	})).ID
	documentComment := createdID(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/comments",
		map[string]any{"body": quotedTwice}, "alice"))
	message := createIssueMessage(t, handler, issue, map[string]any{"body": quotedTwice}, "alice").ID
	targeted := createIssueMessage(t, handler, issue, map[string]any{
		"body": quotedTwice, "target": "session:" + quoteSession, "delivery": "btw",
	}, "alice").ID
	direct := createdID(t, postAgentMessage(t, handler, quoteSession, map[string]any{"body": quotedTwice, "delivery": "btw"}, "alice"))

	routes := []struct {
		route, code, field string
		send               func(t *testing.T, input map[string]any) *httptest.ResponseRecorder
	}{
		{"POST /api/v1/issues/{key}/comments", "INVALID_COMMENT", "reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			input["reply_to"] = comment
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", input, "alice")
		}},
		{"POST /api/v1/artifacts/{id}/comments", "INVALID_COMMENT", "reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			input["reply_to"] = documentComment
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/comments", input, "alice")
		}},
		{"POST /api/v1/comments/{id}/reply", "INVALID_COMMENT", "reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodPost, "/api/v1/comments/"+comment+"/reply", callbackAs(quoteSession, input))
		}},
		{"POST /api/v1/issues/{key}/messages", "INVALID_MESSAGE", "in_reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			input["in_reply_to"] = message
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/messages", input, "alice")
		}},
		{"POST /api/v1/messages/{id}/reply", "INVALID_MESSAGE", "in_reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodPost, "/api/v1/messages/"+targeted+"/reply", callbackAs(quoteSession, input))
		}},
		{"POST /api/v1/agents/{session_id}/messages", "INVALID_MESSAGE", "in_reply_to_block", func(t *testing.T, input map[string]any) *httptest.ResponseRecorder {
			input["in_reply_to"], input["delivery"] = direct, "btw"
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/agents/"+quoteSession+"/messages", input, "alice")
		}},
	}
	refusals := []struct {
		name string
		// fields are the reply's own fields beside its body, its block named under field.
		fields func(field string) map[string]any
		status int
		// code is the refusal's code, the route's own for a 400; opens is what a 400's message
		// opens with, the field it is about first, {field} standing for the route's block field.
		code, opens string
	}{
		{"occurrence without quote", func(field string) map[string]any { return map[string]any{field: "b1", "occurrence": 1} },
			http.StatusBadRequest, "", "occurrence"},
		{"quote without the block field", func(string) map[string]any { return map[string]any{"quote": quotedWord} },
			http.StatusBadRequest, "", "quote requires {field},"},
		{"an empty quote", func(field string) map[string]any { return map[string]any{field: "b1", "quote": ""} },
			http.StatusBadRequest, "", "quote"},
		{"occurrence -1", func(field string) map[string]any {
			return map[string]any{field: "b1", "quote": quotedWord, "occurrence": -1}
		},
			http.StatusBadRequest, "", "occurrence"},
		{"a quote the block does not hold", func(field string) map[string]any {
			return withQuote(map[string]any{}, field, "b1", "zarquonabsent", -1)
		},
			http.StatusNotFound, "TARGET_NOT_FOUND", ""},
		{"occurrence 2 of a word the block holds twice", func(field string) map[string]any { return withQuote(map[string]any{}, field, "b1", quotedWord, 2) },
			http.StatusNotFound, "TARGET_NOT_FOUND", ""},
		{"a word the block holds twice, with no occurrence", func(field string) map[string]any { return withQuote(map[string]any{}, field, "b1", quotedWord, -1) },
			http.StatusConflict, "TARGET_AMBIGUOUS", ""},
	}
	for routeIndex, route := range routes {
		t.Run(route.route, func(t *testing.T) {
			for refusalIndex, refusal := range refusals {
				body := fmt.Sprintf("zarquonrefused%d%d answers it.", routeIndex, refusalIndex)
				input := refusal.fields(route.field)
				input["body"] = body
				response := route.send(t, input)
				code := refusal.code
				if code == "" {
					code = route.code
				}
				var answer struct {
					Code       string          `json:"code"`
					Error      string          `json:"error"`
					Candidates json.RawMessage `json:"candidates"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || response.Code != refusal.status || answer.Code != code {
					t.Fatalf("%s: status=%d body=%s, want %d %s", refusal.name, response.Code, response.Body.String(), refusal.status, code)
				}
				if opens := strings.ReplaceAll(refusal.opens, "{field}", route.field); opens != "" && !strings.HasPrefix(answer.Error, opens+" ") {
					t.Fatalf("%s: the refusal says %q, want it to open with %q", refusal.name, answer.Error, opens)
				}
				if code == "TARGET_AMBIGUOUS" && (!strings.Contains(answer.Error, "matches 2 places") || !strings.Contains(answer.Error, "occurrence") || answer.Candidates != nil) {
					t.Fatalf("%s: the refusal is %s, want it to name the 2 matches and ask for an occurrence, with no candidates",
						refusal.name, response.Body.String())
				}
				var stored int
				if err := database.Pool.QueryRow(context.Background(), `
					select (select count(*) from comments where body = $1) + (select count(*) from messages where body = $1)
				`, body).Scan(&stored); err != nil || stored != 0 {
					t.Fatalf("%s: %d replies stored (%v), want none", refusal.name, stored, err)
				}
			}
			taken := route.send(t, withQuote(map[string]any{"body": "The control takes it."}, route.field, "b1", quotedWord, 1))
			requireQuoted(t, "the control", taken.Body.Bytes(), createdID(t, taken), quoting(quotedWord, 1))
		})
	}
}

// quotedPosts is an issue's posts of each kind a reply can answer - a comment that mentions
// quoteSession, a project document's comment, an ask, a message targeted at quoteSession, a
// direct message to it and a broadcast's copy to it - each holding quotedWord twice in its first
// block, with replies to them through every route of R8 that quote its second match. Each reply
// is written under an Idempotency-Key of its own, so a repeat of the key answers it as it stands
// now (keyedReplays, keyedRereads).
type quotedPosts struct {
	handler   http.Handler
	database  *store.Store
	listener  *quoteListener
	issue     string
	document  string
	broadcast string
	// posts are the posts' ids by kind: comment, document comment, ask, message, direct and copy.
	posts map[string]string
	// replies are the replies by name, each a reply of the route its name says.
	replies map[string]quotedReply
}

// quotedReply is one reply of quotedPosts: its id, the post it answers (a key of posts), and its
// write sent again under the same key, which fails t unless it is replayed under the first
// answer's status.
type quotedReply struct {
	id      string
	answers string
	repeat  func(t *testing.T) *httptest.ResponseRecorder
}

// table is the table the reply's row is in: a comment's, a document comment's and an ask's
// replies are comments, a message's are messages.
func (r quotedReply) table() string {
	switch r.answers {
	case "comment", "document comment", "ask":
		return "comments"
	}
	return "messages"
}

func newQuotedPosts(t *testing.T) *quotedPosts {
	t.Helper()
	handler, database, listener := newQuoteHandler(t)
	f := &quotedPosts{handler: handler, database: database, listener: listener, posts: map[string]string{}, replies: map[string]quotedReply{}}
	f.issue = createInteractionIssue(t, handler, "TEST", "Replies that quote", "before").Key
	f.document = createProjectDocument(t, handler, "TEST", "Quoted notes", "# Quoted notes\n\nalpha bravo\n").ID
	f.posts["comment"] = decodeMentionedComment(t, postMentionedComment(t, handler, f.issue, map[string]any{
		"body": quotedTwice, "mentions": []map[string]any{{"target": "session:" + quoteSession}},
	})).ID
	f.posts["document comment"] = createdID(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+f.document+"/comments",
		map[string]any{"body": quotedTwice}, "alice"))
	f.posts["ask"] = createPartAsk(t, handler, f.issue, "Ship alpha now, or alpha later?").ID
	f.posts["message"] = createIssueMessage(t, handler, f.issue, map[string]any{
		"body": quotedTwice, "target": "session:" + quoteSession, "delivery": "btw",
	}, "alice").ID
	f.posts["direct"] = createdID(t, postAgentMessage(t, handler, quoteSession, map[string]any{"body": quotedTwice, "delivery": "btw"}, "alice"))
	sent := postBroadcast(t, handler, map[string]any{"body": quotedTwice, "delivery": "btw", "session_ids": []string{quoteSession}}, "alice")
	if sent.Code != http.StatusCreated {
		t.Fatalf("the broadcast: status=%d body=%s", sent.Code, sent.Body.String())
	}
	broadcast := decodeBody[broadcastResponse](t, sent)
	if len(broadcast.Recipients) != 1 {
		t.Fatalf("the broadcast's recipients = %#v, want %s's copy", broadcast.Recipients, quoteSession)
	}
	f.broadcast, f.posts["copy"] = broadcast.ID, broadcast.Recipients[0].Message.ID

	person := func(path string, input map[string]any) func(*testing.T, string) *httptest.ResponseRecorder {
		return func(t *testing.T, key string) *httptest.ResponseRecorder {
			t.Helper()
			return keyedRequest(t, handler, http.MethodPost, path, input, "alice", key)
		}
	}
	session := func(path string, input map[string]any) func(*testing.T, string) *httptest.ResponseRecorder {
		return func(t *testing.T, key string) *httptest.ResponseRecorder {
			t.Helper()
			return keyedSessionRequest(t, handler, http.MethodPost, path, callbackAs(quoteSession, input), key)
		}
	}
	issueComments, issueMessages := "/api/v1/issues/"+f.issue+"/comments", "/api/v1/issues/"+f.issue+"/messages"
	agentMessages := "/api/v1/agents/" + quoteSession + "/messages"
	onComment := func(body string) map[string]any {
		return withQuote(map[string]any{"body": body, "reply_to": f.posts["comment"]}, "reply_to_block", "b1", quotedWord, 1)
	}
	onMessage := func(body string) map[string]any {
		return withQuote(map[string]any{"body": body, "in_reply_to": f.posts["message"]}, "in_reply_to_block", "b1", quotedWord, 1)
	}
	callback := withQuote(map[string]any{"body": "The session on the second alpha."}, "reply_to_block", "b1", quotedWord, 1)
	answer := withQuote(map[string]any{"body": "The session on the message's second alpha."}, "in_reply_to_block", "b1", quotedWord, 1)
	for _, reply := range []struct {
		name, answers string
		status        int
		send          func(*testing.T, string) *httptest.ResponseRecorder
	}{
		{"comment", "comment", http.StatusCreated, person(issueComments, onComment("On the second alpha."))},
		{"comment spare 0", "comment", http.StatusCreated, person(issueComments, onComment("A spare on the second alpha."))},
		{"comment spare 1", "comment", http.StatusCreated, person(issueComments, onComment("Another spare on the second alpha."))},
		{"ask", "ask", http.StatusCreated, person(issueComments, withQuote(map[string]any{
			"body": "On the ask's second alpha.", "ask_id": f.posts["ask"],
		}, "reply_to_block", "b1", quotedWord, 1))},
		{"document", "document comment", http.StatusCreated, person("/api/v1/artifacts/"+f.document+"/comments", withQuote(map[string]any{
			"body": "On the document's second alpha.", "reply_to": f.posts["document comment"],
		}, "reply_to_block", "b1", quotedWord, 1))},
		{"callback", "comment", http.StatusCreated, session("/api/v1/comments/"+f.posts["comment"]+"/reply", callback)},
		// The same callback again, which hands back the reply the attempt already holds.
		{"callback again", "comment", http.StatusOK, session("/api/v1/comments/"+f.posts["comment"]+"/reply", callback)},
		{"message", "message", http.StatusCreated, person(issueMessages, onMessage("On the message's second alpha."))},
		{"message spare 0", "message", http.StatusCreated, person(issueMessages, onMessage("A spare on the message's second alpha."))},
		{"message spare 1", "message", http.StatusCreated, person(issueMessages, onMessage("Another spare on the message's second alpha."))},
		{"session", "message", http.StatusCreated, session("/api/v1/messages/"+f.posts["message"]+"/reply", answer)},
		{"session again", "message", http.StatusOK, session("/api/v1/messages/"+f.posts["message"]+"/reply", answer)},
		{"direct", "direct", http.StatusCreated, person(agentMessages, withQuote(map[string]any{
			"body": "On the direct second alpha.", "delivery": "btw", "in_reply_to": f.posts["direct"],
		}, "in_reply_to_block", "b1", quotedWord, 1))},
		{"copy", "copy", http.StatusCreated, person(agentMessages, withQuote(map[string]any{
			"body": "On the broadcast's second alpha.", "delivery": "btw", "in_reply_to": f.posts["copy"],
		}, "in_reply_to_block", "b1", quotedWord, 1))},
	} {
		key := "quoted-" + strings.ReplaceAll(reply.name, " ", "-")
		first := reply.send(t, key)
		if first.Code != reply.status {
			t.Fatalf("the %s reply: status=%d body=%s, want %d", reply.name, first.Code, first.Body.String(), reply.status)
		}
		send := reply.send
		f.replies[reply.name] = quotedReply{
			id: decodeBody[struct {
				ID string `json:"id"`
			}](t, first).ID,
			answers: reply.answers,
			repeat: func(t *testing.T) *httptest.ResponseRecorder {
				t.Helper()
				again := send(t, key)
				if again.Code != reply.status || !replayed(again) {
					t.Fatalf("the repeat of the %s reply: status=%d body=%s, want its %d, replayed", reply.name, again.Code, again.Body.String(), reply.status)
				}
				return again
			},
		}
	}
	return f
}

// deleteAsAnOlderBuild marks post id of table (comments, messages) deleted with the statement a
// delete writes the row with (clearDeletedPostBody) and nothing else: no event, no index, and no
// write to any reply of it.
func deleteAsAnOlderBuild(t *testing.T, database *store.Store, table, id string) {
	t.Helper()
	statement := `update messages set body = '', deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'::jsonb where id = $1`
	if table == "comments" {
		statement = `update comments set body = '', suggestion = null, deleted_at = now(), deleted_by = '{"kind":"user","id":"alice"}'::jsonb where id = $1`
	}
	tag, err := database.Pool.Exec(context.Background(), statement, id)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("mark %s %s deleted: %d rows (%v), want 1", table, id, tag.RowsAffected(), err)
	}
}

// R8 once the answered post is deleted: every answer that carries a reply's reply_block leaves
// out quote and occurrence, and its deleted_at is set. The comment and the issue message are
// deleted through their routes; a direct message and a broadcast's copy belong to no issue, which
// no route deletes (NOT_AN_ISSUE_MESSAGE), so each is marked deleted with the store's statement, as
// the project document's comment is, as an older build deletes it. The reads are the reply's own,
// its owner's lists, the broadcast, the agent's conversations, and the answer of each write,
// delivery and reply route, read by a repeat of the write's key. The store still holds both
// values. The events written before the delete keep them, as they keep the block's text; an edit
// of a reply written after the delete writes an event with neither; and the delete event of a
// reply to a post that stands carries neither either, as it carries no text.
func TestADeletedPostLeavesNoQuoteOfItsWords(t *testing.T) {
	t.Parallel()
	f := newQuotedPosts(t)
	handler := f.handler
	reply := func(name string) string { return f.replies[name].id }
	standing := createThreadComment(t, handler, f.issue, map[string]any{"body": quotedTwice}, "alice").ID
	standingReply := createdID(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/comments",
		withQuote(map[string]any{"body": "On the standing alpha.", "reply_to": standing}, "reply_to_block", "b1", quotedWord, 1), "alice"))
	standingMessage := createIssueMessage(t, handler, f.issue, map[string]any{"body": quotedTwice}, "alice").ID
	standingMessageReply := createdID(t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+f.issue+"/messages",
		withQuote(map[string]any{"body": "On the standing message's alpha.", "in_reply_to": standingMessage}, "in_reply_to_block", "b1", quotedWord, 1), "alice"))
	// Each reply's first edit is written while the post it answers stands.
	editThreadComment(t, handler, reply("comment"), "On the second alpha, first edited.", "alice")
	editIssueMessage(t, handler, reply("message"), "On the message's second alpha, first edited.", "alice")
	// The control: the reply reads its quote until the delete, so a read that lost it is seen.
	requireQuoted(t, "GET the comment's reply before the delete", getRaw(t, handler, "/api/v1/comments/"+reply("comment")),
		reply("comment"), quoting(quotedWord, 1))

	deleteThreadComment(t, handler, f.posts["comment"], "alice")
	deleteIssueMessage(t, handler, f.posts["message"], "alice")
	for _, issueless := range []string{"direct", "copy"} {
		refused := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+f.posts[issueless], nil, "alice")
		if refused.Code != http.StatusConflict || !strings.Contains(refused.Body.String(), `"code":"NOT_AN_ISSUE_MESSAGE"`) {
			t.Fatalf("delete the %s message: status=%d body=%s, want 409 NOT_AN_ISSUE_MESSAGE", issueless, refused.Code, refused.Body.String())
		}
		deleteAsAnOlderBuild(t, f.database, "messages", f.posts[issueless])
	}
	deleteAsAnOlderBuild(t, f.database, "comments", f.posts["document comment"])

	get := func(path string) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			t.Helper()
			return getRaw(t, handler, path)
		}
	}
	repeat := func(name string) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			t.Helper()
			return f.replies[name].repeat(t).Body.Bytes()
		}
	}
	issueComments, issueMessages := "/api/v1/issues/"+f.issue+"/comments", "/api/v1/issues/"+f.issue+"/messages"
	agentMessages, broadcast := "/api/v1/agents/"+quoteSession+"/messages", "/api/v1/broadcasts/"+f.broadcast
	getPage := func(path string) func(*testing.T) []byte {
		return func(t *testing.T) []byte {
			t.Helper()
			return readThreadPageAnswer(t, handler, path+"?order=activity")
		}
	}
	reads := []struct {
		name, reply string
		read        func(*testing.T) []byte
	}{
		{"GET /api/v1/comments/{id}", "comment", get("/api/v1/comments/" + reply("comment"))},
		{"GET /api/v1/issues/{key}/comments", "comment", get(issueComments)},
		{"the repeat of POST /api/v1/issues/{key}/comments", "comment", repeat("comment")},
		{"GET /api/v1/comments/{id}", "callback", get("/api/v1/comments/" + reply("callback"))},
		{"GET /api/v1/issues/{key}/comments", "callback", get(issueComments)},
		{"the repeat of POST /api/v1/comments/{id}/reply", "callback", repeat("callback")},
		{"the repeat of POST /api/v1/comments/{id}/reply for an attempt already answered", "callback", repeat("callback again")},
		{"GET /api/v1/comments/{id}", "document", get("/api/v1/comments/" + reply("document"))},
		{"GET /api/v1/artifacts/{id}/comments", "document", get("/api/v1/artifacts/" + f.document + "/comments")},
		{"the repeat of POST /api/v1/artifacts/{id}/comments", "document", repeat("document")},
		{"GET /api/v1/issues/{key}/messages/{id}", "message", get(issueMessages + "/" + reply("message"))},
		{"GET /api/v1/issues/{key}/messages", "message", get(issueMessages)},
		{"GET /api/v1/messages/{id}", "message", get("/api/v1/messages/" + reply("message"))},
		{"the repeat of POST /api/v1/issues/{key}/messages", "message", repeat("message")},
		{"GET /api/v1/issues/{key}/messages/{id}", "session", get(issueMessages + "/" + reply("session"))},
		{"GET /api/v1/issues/{key}/messages", "session", get(issueMessages)},
		{"the repeat of POST /api/v1/messages/{id}/reply", "session", repeat("session")},
		{"the repeat of POST /api/v1/messages/{id}/reply for an attempt already answered", "session", repeat("session again")},
		{"GET /api/v1/messages/{id}", "direct", get("/api/v1/messages/" + reply("direct"))},
		{"GET /api/v1/agents/{session_id}/messages", "direct", get(agentMessages)},
		{"the repeat of POST /api/v1/agents/{session_id}/messages", "direct", repeat("direct")},
		{"GET /api/v1/broadcasts/{id}", "copy", get(broadcast)},
		{"GET /api/v1/agents/{session_id}/messages", "copy", get(agentMessages)},
		{"GET /api/v1/messages/{id}", "copy", get("/api/v1/messages/" + reply("copy"))},
		{"the comment thread page", "comment", getPage(issueComments)},
		{"the comment thread page", "callback", getPage(issueComments)},
		{"the message thread page", "message", getPage(issueMessages)},
		{"the message thread page", "session", getPage(issueMessages)},
	}
	for _, row := range reads {
		step := row.name + ", the " + row.reply + " reply"
		body := row.read(t)
		requireQuoted(t, step, body, reply(row.reply), unquoted)
		requireBlockMark(t, step, body, reply(row.reply), "deleted_at")
	}
	// The ask no route deletes: its reply keeps both.
	requireQuoted(t, "GET /api/v1/asks/{id}, the ask's reply", getRaw(t, handler, "/api/v1/asks/"+f.posts["ask"]),
		reply("ask"), quoting(quotedWord, 1))
	askPage := "/api/v1/issues/" + f.issue + "/asks?order=activity&thread=1"
	requireQuoted(t, "the ask thread page", readThreadPageAnswer(t, handler, askPage), reply("ask"), quoting(quotedWord, 1))

	// The delete writes nothing to its replies: each row keeps the quote it stored.
	for name, stored := range f.replies {
		var quote *string
		var occurrence *int
		if err := f.database.Pool.QueryRow(context.Background(),
			`select replies_to_quote, replies_to_occurrence from `+stored.table()+` where id = $1`, stored.id,
		).Scan(&quote, &occurrence); err != nil || quote == nil || *quote != quotedWord || occurrence == nil || *occurrence != 1 {
			t.Fatalf("the %s reply's stored quote = %v at %v (%v), want %q at 1", name, quote, occurrence, err, quotedWord)
		}
	}

	// An edit of a reply written after the delete reads its block as it stands then.
	editThreadComment(t, handler, reply("comment"), "On the second alpha, edited after the delete.", "alice")
	editIssueMessage(t, handler, reply("message"), "On the message's second alpha, edited after the delete.", "alice")
	editThreadComment(t, handler, reply("document"), "On the document's second alpha, edited after the delete.", "alice")
	issueFeed, documentFeed := "/api/v1/issues/"+f.issue+"/events", "/api/v1/artifacts/"+f.document+"/events"
	kept := quoting(quotedWord, 1)
	for _, row := range []struct {
		name, feed, eventType, reply string
		// want is what each of the reply's events of eventType says, oldest first.
		want []quotedWords
	}{
		{"the creation, written before the delete", issueFeed, "comment.created", "comment", []quotedWords{kept}},
		{"an edit before the delete and one after it", issueFeed, "comment.edited", "comment", []quotedWords{kept, unquoted}},
		{"the callback's creation", issueFeed, "comment.answered", "callback", []quotedWords{kept}},
		{"the creation, written before the delete", issueFeed, "message.answered", "message", []quotedWords{kept}},
		{"an edit before the delete and one after it", issueFeed, "message.edited", "message", []quotedWords{kept, unquoted}},
		{"the session's reply, written before the delete", issueFeed, "message.answered", "session", []quotedWords{kept}},
		{"the creation, written before an older build's delete", documentFeed, "comment.created", "document", []quotedWords{kept}},
		{"an edit after an older build's delete", documentFeed, "comment.edited", "document", []quotedWords{unquoted}},
	} {
		payloads := feedPayloads(t, handler, row.feed, row.eventType, reply(row.reply))
		if len(payloads) != len(row.want) {
			t.Fatalf("the %s reply's %s events: %d, want %d", row.reply, row.eventType, len(payloads), len(row.want))
		}
		for index, payload := range payloads {
			requireQuoted(t, fmt.Sprintf("the %s reply's %s %d, %s", row.reply, row.eventType, index, row.name), payload, reply(row.reply), row.want[index])
		}
	}

	// A reply's own delete carries no quote of the post it answers, which stands, as it carries no text.
	deleteThreadComment(t, handler, standingReply, "alice")
	deleteIssueMessage(t, handler, standingMessageReply, "alice")
	for _, own := range []struct{ created, deleted, id string }{
		{"comment.created", "comment.deleted", standingReply},
		{"message.answered", "message.deleted", standingMessageReply},
	} {
		created := feedPayloads(t, handler, issueFeed, own.created, own.id)
		if len(created) != 1 {
			t.Fatalf("the %s events of the standing post's reply: %d, want 1", own.created, len(created))
		}
		requireQuoted(t, "the control: the reply's "+own.created, created[0], own.id, kept)
		deleted := feedPayloads(t, handler, issueFeed, own.deleted, own.id)
		if len(deleted) != 1 {
			t.Fatalf("the %s events of the standing post's reply: %d, want 1", own.deleted, len(deleted))
		}
		requireQuoted(t, "the reply's own "+own.deleted, deleted[0], own.id, unquoted)
	}
}
