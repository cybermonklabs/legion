package api

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// holdsReplyBlock reports whether a value of ty can hold a model.ReplyBlock as encoding/json
// writes it: ty is one, or holds one in a member of a struct, through pointers, slices, arrays
// and maps.
func holdsReplyBlock(ty reflect.Type) bool {
	block := reflect.TypeFor[model.ReplyBlock]()
	seen := map[reflect.Type]bool{}
	var holds func(ty reflect.Type) bool
	holds = func(ty reflect.Type) bool {
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
			if field := ty.Field(index); field.Tag.Get("json") != "-" && holds(field.Type) {
				return true
			}
		}
		return false
	}
	return holds(ty)
}

// replyQuoteAnswerExceptions are the answers, by route and status, whose Go type can hold a
// reply_block and that never carry the block of a reply, each with the reason.
var replyQuoteAnswerExceptions = map[string]string{
	"POST /api/v1/broadcasts 201": "the create answers each copy it sent with no replies yet: a copy is a root message, " +
		"which names no block, and a reply to it is written after the create answers",
	"POST /api/v1/comments/{id}/reopen 200": "a reply is refused its reopen (INVALID_COMMENT, reopen the thread root), " +
		"so the answer is a root's, and a root names no block: reply_to_block requires reply_to or ask_id",
	"POST /api/v1/comments/{id}/accept 200": "only a suggestion is accepted, and a reply carries none " +
		"(INVALID_COMMENT, replies cannot carry suggestions), so the answer is a root's, which names no block",
	"POST /api/v1/comments/{id}/reject 200": "only a suggestion is rejected, and a reply carries none " +
		"(INVALID_COMMENT, replies cannot carry suggestions), so the answer is a root's, which names no block",
	"GET /api/v1/issues/{key}/decisions 200": "its open questions are ask rows without their threads " +
		"(specOpenQuestions sets no thread), so it holds no reply",
}

// deliveryFrameKind is the delivery frame's message.created (finishPendingMessageDelivery): the
// event a frame replays a message to its session as, which the event part holds beside the kinds
// of the log.
const deliveryFrameKind = "message.created, the delivery frame"

// replyQuoteEventExceptions are the event kinds of postBodyEventKinds that no reply's write
// appends, each with the reason.
var replyQuoteEventExceptions = map[string]string{
	"comment.reopened": "a reply is refused its reopen (reopen the thread root), so no reply writes one",
	"comment.anchor_refreshed": "a refresh is of a comment's anchor, and a reply carries none " +
		"(replies cannot carry anchors)",
	"suggestion.accepted": "a suggestion is a root's (replies cannot carry suggestions), so no reply writes one",
	"suggestion.rejected": "a suggestion is a root's (replies cannot carry suggestions), so no reply writes one",
	"message.created": "a message naming in_reply_to is written as message.answered (createStoredMessage), so a " +
		"reply's own event in the log is never message.created; the delivery frame that replays it is, and is a row",
}

// R8 over every answer that can hold a reply_block, as routeShapes declares each route's answers
// (the type walk of TestEveryKeyedAnswerThatCanHoldAReplyBlockIsReadAfresh): each such answer is a
// row here, read through its route, or an exception with its reason, and a route that comes to
// answer a reply's block fails until it is one or the other. The fixture's replies quote a word of
// a comment, a document's comment, an ask's part, an issue message, a direct message and a
// broadcast's copy (quotedPosts), and each row finds the quote and the occurrence of the replies
// it carries. Then the comment, the document's comment and the message are deleted, and each row,
// read again, gives neither member and the deleted_at of a reply to them, and both of a reply to
// the ask and to the direct posts, which no route deletes.
//
// The event part holds every kind of postBodyEventKinds and the delivery frame's message.created
// the same way: each is written for a quoting reply by a row, or excepted with its reason. Each
// event of a reply written before the delete carries both members and still carries them in the
// feed after it; each written after the delete carries neither; and a reply's own comment.deleted
// or message.deleted carries neither whenever it is written.
func TestEveryAnswerThatCanHoldAReplyBlockCarriesItsQuote(t *testing.T) {
	t.Parallel()
	f := newQuotedPosts(t)
	handler := f.handler
	reply := func(name string) string { return f.replies[name].id }
	get := func(path string) func(*testing.T, int) *httptest.ResponseRecorder {
		return func(t *testing.T, _ int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		}
	}
	repeat := func(name string) func(*testing.T, int) *httptest.ResponseRecorder {
		return func(t *testing.T, _ int) *httptest.ResponseRecorder {
			t.Helper()
			return f.replies[name].repeat(t)
		}
	}
	// spare is the spare reply a pass's delete takes: one while its post stands, one after.
	spare := func(post string) func(int) string {
		return func(pass int) string { return post + " spare " + strconv.Itoa(pass) }
	}
	always := func(names ...string) func(int) []string { return func(int) []string { return names } }
	issueComments, issueMessages := "/api/v1/issues/"+f.issue+"/comments", "/api/v1/issues/"+f.issue+"/messages"
	agentMessages := "/api/v1/agents/" + quoteSession + "/messages"
	getPage := func(path string) func(*testing.T, int) *httptest.ResponseRecorder {
		return func(t *testing.T, pass int) *httptest.ResponseRecorder {
			t.Helper()
			response := get(path)(t, pass)
			decodeThreadPage(t, response.Body.Bytes(), response.Code, path)
			return response
		}
	}
	answers := map[string]struct {
		read func(t *testing.T, pass int) *httptest.ResponseRecorder
		// carries is the fixture's replies the read gives the block of, in each pass.
		carries func(pass int) []string
	}{
		"POST /api/v1/issues/{key}/comments 201":        {repeat("comment"), always("comment")},
		"POST /api/v1/artifacts/{id}/comments 201":      {repeat("document"), always("document")},
		"POST /api/v1/comments/{id}/reply 201":          {repeat("callback"), always("callback")},
		"POST /api/v1/comments/{id}/reply 200":          {repeat("callback again"), always("callback")},
		"POST /api/v1/issues/{key}/messages 201":        {repeat("message"), always("message")},
		"POST /api/v1/messages/{id}/reply 201":          {repeat("session"), always("session")},
		"POST /api/v1/messages/{id}/reply 200":          {repeat("session again"), always("session")},
		"POST /api/v1/agents/{session_id}/messages 201": {repeat("direct"), always("direct")},
		"GET /api/v1/comments/{id} 200":                 {get("/api/v1/comments/" + reply("comment")), always("comment")},
		"GET /api/v1/issues/{key}/comments 200":         {get(issueComments), always("comment", "callback")},
		"GET /api/v1/artifacts/{id}/comments 200":       {get("/api/v1/artifacts/" + f.document + "/comments"), always("document")},
		"GET /api/v1/issues/{key}/messages 200":         {get(issueMessages), always("message", "session")},
		"GET /api/v1/issues/{key}/messages/{id} 200":    {get(issueMessages + "/" + reply("message")), always("message")},
		"GET /api/v1/messages/{id} 200":                 {get("/api/v1/messages/" + reply("direct")), always("direct")},
		"GET /api/v1/agents/{session_id}/messages 200":  {get(agentMessages), always("direct", "copy")},
		"GET /api/v1/broadcasts/{id} 200":               {get("/api/v1/broadcasts/" + f.broadcast), always("copy")},
		"GET /api/v1/asks/{id} 200":                     {get("/api/v1/asks/" + f.posts["ask"]), always("ask")},
		"GET /api/v1/issues/{key}/asks 200":             {get("/api/v1/issues/" + f.issue + "/asks?thread=1"), always("ask")},
		"GET /api/v1/inbox 200":                         {get("/api/v1/inbox?project=TEST"), always("ask")},
		"PATCH /api/v1/comments/{id} 200": {func(t *testing.T, pass int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/comments/"+reply("comment"),
				map[string]any{"body": fmt.Sprintf("On the second alpha, edit %d.", pass)}, "alice")
		}, always("comment")},
		"PATCH /api/v1/messages/{id} 200": {func(t *testing.T, pass int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodPatch, "/api/v1/messages/"+reply("message"),
				map[string]any{"body": fmt.Sprintf("On the message's second alpha, edit %d.", pass)}, "alice")
		}, always("message")},
		"POST /api/v1/comments/{id}/resolve 200": {func(t *testing.T, _ int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+reply("comment")+"/resolve", map[string]any{}, "alice")
		}, always("comment")},
		"DELETE /api/v1/comments/{id} 200": {func(t *testing.T, pass int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/comments/"+reply(spare("comment")(pass)), nil, "alice")
		}, func(pass int) []string { return []string{spare("comment")(pass)} }},
		"DELETE /api/v1/messages/{id} 200": {func(t *testing.T, pass int) *httptest.ResponseRecorder {
			t.Helper()
			return dispatchRequest(t, handler, http.MethodDelete, "/api/v1/messages/"+reply(spare("message")(pass)), nil, "alice")
		}, func(pass int) []string { return []string{spare("message")(pass)} }},

		"GET /api/v1/issues/{key}/comments commentThreadPage 200": {getPage(issueComments + "?order=activity"), always("comment", "callback")},
		"GET /api/v1/issues/{key}/messages messageThreadPage 200": {getPage(issueMessages + "?order=activity"), always("message", "session")},
		"GET /api/v1/issues/{key}/asks askThreadPage 200":         {getPage("/api/v1/issues/" + f.issue + "/asks?order=activity&thread=1"), always("ask")},
	}

	found := map[string]bool{}
	for route, shape := range routeShapes {
		for _, body := range []any{shape.Response, shape.Advice} {
			if body != nil && holdsReplyBlock(reflect.TypeOf(body)) {
				found[fmt.Sprintf("%s %d", route, shape.Status)] = true
			}
		}
		for _, answer := range shape.Also {
			ty := reflect.TypeOf(answer.Body)
			if ty == nil || !holdsReplyBlock(ty) {
				continue
			}
			// A second shape at the same status needs its own row, not the list's.
			name := route
			if answer.Status == shape.Status {
				name += " " + ty.Name()
			}
			found[fmt.Sprintf("%s %d", name, answer.Status)] = true
		}
	}
	for _, answer := range slices.Sorted(maps.Keys(found)) {
		_, read := answers[answer]
		if _, excepted := replyQuoteAnswerExceptions[answer]; !read && !excepted {
			t.Errorf("%s can hold a reply_block, and is neither a row of this test nor an exception with its reason", answer)
		}
	}
	for _, listed := range append(slices.Collect(maps.Keys(answers)), slices.Collect(maps.Keys(replyQuoteAnswerExceptions))...) {
		if !found[listed] {
			t.Errorf("%s is listed here, and is no answer routeShapes says can hold a reply_block", listed)
		}
	}

	issueFeed := "/api/v1/issues/" + f.issue + "/events"
	inFeed := func(kind string) func(*testing.T, string) []json.RawMessage {
		return func(t *testing.T, id string) []json.RawMessage {
			t.Helper()
			return feedPayloads(t, handler, issueFeed, kind, id)
		}
	}
	events := map[string]struct {
		payloads func(t *testing.T, id string) []json.RawMessage
		replies  []string
		// again is the replies the second pass writes another event of this kind for, after the
		// delete: an edit, a resolve, a retried delivery.
		again []string
	}{
		"comment.created":  {inFeed("comment.created"), []string{"comment"}, nil},
		"comment.answered": {inFeed("comment.answered"), []string{"callback"}, nil},
		"comment.edited":   {inFeed("comment.edited"), []string{"comment"}, []string{"comment"}},
		"comment.resolved": {inFeed("comment.resolved"), []string{"comment"}, []string{"comment"}},
		"message.answered": {inFeed("message.answered"), []string{"message", "session"}, nil},
		"message.edited":   {inFeed("message.edited"), []string{"message"}, []string{"message"}},
		deliveryFrameKind: {func(t *testing.T, id string) []json.RawMessage {
			t.Helper()
			return eventsAbout(f.listener.frames(t), "message.created", id)
		}, []string{"message", "direct", "copy"}, []string{"message"}},
	}
	kinds := append(slices.Collect(maps.Keys(postBodyEventKinds)), deliveryFrameKind)
	for _, kind := range kinds {
		_, written := events[kind]
		if _, excepted := replyQuoteEventExceptions[kind]; !written && !excepted {
			t.Errorf("event kind %s carries a post's reply_block, and is neither a row of this test nor an exception with its reason", kind)
		}
	}
	for _, listed := range append(slices.Collect(maps.Keys(events)), slices.Collect(maps.Keys(replyQuoteEventExceptions))...) {
		if !slices.Contains(kinds, listed) {
			t.Errorf("event kind %s is listed here, and is no kind of postBodyEventKinds or the delivery frame", listed)
		}
	}
	if t.Failed() {
		return
	}

	deleted := map[string]bool{"comment": true, "document comment": true, "message": true}
	kept := quoting(quotedWord, 1)
	before := map[string]int{}
	for pass := range 2 {
		if pass == 1 {
			deleteThreadComment(t, handler, f.posts["comment"], "alice")
			deleteThreadComment(t, handler, f.posts["document comment"], "alice")
			deleteIssueMessage(t, handler, f.posts["message"], "alice")
		}
		for _, answer := range slices.Sorted(maps.Keys(answers)) {
			row := answers[answer]
			status, _ := strconv.Atoi(answer[strings.LastIndex(answer, " ")+1:])
			response := row.read(t, pass)
			if response.Code != status {
				t.Fatalf("pass %d, %s: status=%d body=%s", pass, answer, response.Code, response.Body.String())
			}
			for _, name := range row.carries(pass) {
				step := fmt.Sprintf("pass %d, %s, the %s reply", pass, answer, name)
				if pass == 1 && deleted[f.replies[name].answers] {
					requireQuoted(t, step, response.Body.Bytes(), reply(name), unquoted)
					requireBlockMark(t, step, response.Body.Bytes(), reply(name), "deleted_at")
					continue
				}
				requireQuoted(t, step, response.Body.Bytes(), reply(name), kept)
			}
		}
		if pass == 1 {
			// The message reply's delivery, sent again after the message it answers is deleted.
			if retried := dispatchRequest(t, handler, http.MethodPost, "/api/v1/messages/"+reply("message")+"/deliveries",
				map[string]any{"delivery": "steer"}, "alice"); retried.Code != http.StatusCreated {
				t.Fatalf("retry the message reply's delivery: status=%d body=%s", retried.Code, retried.Body.String())
			}
		}
		for _, kind := range slices.Sorted(maps.Keys(events)) {
			row := events[kind]
			for _, name := range row.replies {
				payloads := row.payloads(t, reply(name))
				key := kind + " " + name
				if pass == 0 {
					before[key] = len(payloads)
				}
				if len(payloads) == 0 || pass == 1 && slices.Contains(row.again, name) && len(payloads) <= before[key] {
					t.Fatalf("pass %d: the %s reply's %s: %d, want one written in this pass", pass, name, kind, len(payloads))
				}
				for index, payload := range payloads {
					step := fmt.Sprintf("pass %d: the %s reply's %s %d", pass, name, kind, index)
					if index >= before[key] && deleted[f.replies[name].answers] {
						requireQuoted(t, step+", written after the delete", payload, reply(name), unquoted)
						continue
					}
					requireQuoted(t, step, payload, reply(name), kept)
				}
			}
		}
		for _, own := range []struct{ kind, name string }{
			{"comment.deleted", spare("comment")(pass)}, {"message.deleted", spare("message")(pass)},
		} {
			payloads := feedPayloads(t, handler, issueFeed, own.kind, reply(own.name))
			if len(payloads) != 1 {
				t.Fatalf("pass %d: the %s reply's %s: %d, want its one", pass, own.name, own.kind, len(payloads))
			}
			requireQuoted(t, fmt.Sprintf("pass %d: the %s reply's own %s", pass, own.name, own.kind), payloads[0], reply(own.name), unquoted)
		}
	}
}
