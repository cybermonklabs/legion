// as_of_threads_test.go holds the comment, message and ask routes, the inbox, open asks and an
// agent's answers at a cut to the cases the equality harness
// (as_of_equality_test.go) does not write: a post deleted after the cut in every thread a route
// shows, a message edited before and after the cut, a post's history at a cut, an older build's
// long edit, a delivery attempt no event dates, the inbox's asks at a cut, an answers poll later
// than the cut, the block a reply names, an anchored block's place, an ask's route reach, an old
// approval request's documents and its turn after a version moved it, and a direct thread's session
// rule.
package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// asOfThreadWorld is a server for one test of this file, with its project and issue written.
type asOfThreadWorld struct {
	handler http.Handler
	served  *server
	issue   string
}

// newAsOfThreadWorld starts a server whose Envoy listener holds asOfListenerSession live, and
// writes project with alice as its default assignee and one issue on it.
func newAsOfThreadWorld(t *testing.T, project string) (*asOfThreadWorld, func() asof.Cut) {
	t.Helper()
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{envoyURL: newAsOfListener(t)})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": project, "name": "Threads at a cut"}, "alice")
	asOfWrite(t, handler, http.MethodPut, "/api/v1/projects/"+project+"/default-assignee", map[string]any{"assignee": "alice"}, "alice")
	world := &asOfThreadWorld{handler: handler, served: served}
	world.issue = asOfIssue(t, handler, map[string]any{"project": project, "title": "Threads read at a cut"}).Key
	// cut settles what the writes so far left running and takes the database's clock as the cut.
	cut := func() asof.Cut {
		t.Helper()
		world.settle(t)
		return databaseCut(t, database)
	}
	return world, cut
}

// settle finishes the server's detached work and waits for the event horizon.
func (w *asOfThreadWorld) settle(t *testing.T) {
	t.Helper()
	awaitBackgroundWrites(t, w.served)
	safeHorizon(t, w.served)
}

// comment writes a comment on the world's issue as alice and answers its id.
func (w *asOfThreadWorld) comment(t *testing.T, body map[string]any) string {
	t.Helper()
	return decodeBody[model.Comment](t, asOfWrite(t, w.handler, http.MethodPost, "/api/v1/issues/"+w.issue+"/comments", body, "alice")).ID
}

// message writes a message on the world's issue as login and answers it.
func (w *asOfThreadWorld) message(t *testing.T, body map[string]any, login string) model.Message {
	t.Helper()
	return decodeBody[model.Message](t, asOfWrite(t, w.handler, http.MethodPost, "/api/v1/issues/"+w.issue+"/messages", body, login))
}

// readAt reads target as alice at cut, and fails the test unless the answer is 200 and names the
// cut in Dispatch-As-Of. It answers the decoded body and the pointers the answer names absent.
func (w *asOfThreadWorld) readAt(t *testing.T, target string, cut asof.Cut) (any, []string) {
	t.Helper()
	response := dispatchRequest(t, w.handler, http.MethodGet, withAsOf(t, target, cut), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s as of the cut: status=%d body=%s", target, response.Code, response.Body.String())
	}
	if named := response.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
		t.Fatalf("GET %s as of the cut: %s is %q, want the cut %s: the route answers the present", target, contracts.DispatchAsOfHeader, named, cut)
	}
	return decodeAnswer(t, target, response), absentPointers(response.Header().Get(contracts.DispatchAsOfAbsentHeader))
}

// readNow reads target as alice at the present and answers the decoded body.
func (w *asOfThreadWorld) readNow(t *testing.T, target string) any {
	t.Helper()
	response := dispatchRequest(t, w.handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s at the present: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeAnswer(t, target, response)
}

func decodeAnswer(t *testing.T, target string, response *httptest.ResponseRecorder) any {
	t.Helper()
	doc, err := decodeAsOfBody(response.Body.Bytes())
	if err != nil {
		t.Fatalf("GET %s: decode the answer: %v: %s", target, err, abbreviateAnswer(response.Body.Bytes()))
	}
	return doc
}

// postsByID is each comment or message object of doc, by its id, with its pointer.
func postsByID(doc any) map[string][]postAt {
	posts := map[string][]postAt{}
	eachPost(doc, "", func(path string, object map[string]any) {
		if id, _ := object["id"].(string); id != "" {
			posts[id] = append(posts[id], postAt{path: path, object: object})
		}
	}, func(string, map[string]any, string) {})
	return posts
}

// postAt is one post object of an answer and its pointer.
type postAt struct {
	path   string
	object map[string]any
}

// namedAbsent reports whether one of absent names the concrete member pointer.
func namedAbsent(absent []string, member string) bool {
	return slices.ContainsFunc(absent, func(pointer string) bool { return pointerMatches(pointer, member) })
}

// stringsHolding names each string of doc that holds one of words, by its pointer.
func stringsHolding(doc any, words ...string) []string {
	var held []string
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			for key, member := range node {
				walk(member, pointerJoin(path, key))
			}
		case []any:
			for index, element := range node {
				walk(element, pointerJoin(path, fmt.Sprint(index)))
			}
		case string:
			if slices.ContainsFunc(words, func(word string) bool { return strings.Contains(node, word) }) {
				held = append(held, orRoot(path))
			}
		}
	}
	walk(doc, "")
	return held
}

// TestAPostDeletedAfterTheCutReadsAsDeletedInEveryThread: a post its author deletes after the cut
// reads at the cut as deleted, with no part of its body, in every thread of a post or ask route
// (decision 14). The comment and message routes, and the threads of the ask routes and of the
// inbox, leave its body out and name it absent (reason T); an agent's answers give the deleted
// mark and an empty text; the history routes give the post's present mark and every body empty
// (decision 17). The posts: a reply in an issue's comment thread, a reply in an open ask's
// thread, a comment on a project document, and an issue message sent to a live session that a
// person quoted in a reply, each edited before the cut so its history holds an entry then.
func TestAPostDeletedAfterTheCutReadsAsDeletedInEveryThread(t *testing.T) {
	const project = "ASOD"
	world, takeCut := newAsOfThreadWorld(t, project)
	handler := world.handler
	root := world.comment(t, map[string]any{"body": "The thread's root stands."})
	reply := world.comment(t, map[string]any{"body": "A reply its author withdraws later: pelican.", "reply_to": root})
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+reply, map[string]any{"body": "A reply its author withdraws later, corrected: pelican."}, "alice")
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/asks",
		map[string]any{"question": "Is the withdrawn reply gone at the cut?"})).ID
	askReply := world.comment(t, map[string]any{"body": "An answer its author withdraws later: ocelot.", "ask_id": ask})
	document := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/projects/"+project+"/artifacts",
		map[string]string{"name": "notes.md", "content": "# Notes\n\nThe notes.\n"}, "alice")).Artifact.ID
	remark := decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/artifacts/"+document+"/comments",
		map[string]any{"body": "A remark its author withdraws later: walrus."}, "alice")).ID
	message := world.message(t, map[string]any{
		"body": "A message its author withdraws later: narwhal.", "target": "session:" + asOfListenerSession, "delivery": "btw",
	}, "alice").ID
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": "A message its author withdraws later, corrected: narwhal."}, "alice")
	quote := world.message(t, map[string]any{"body": "Bob answers the message.", "in_reply_to": message}, "bob").ID
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	for _, id := range []string{reply, askReply, remark} {
		asOfWrite(t, handler, http.MethodDelete, "/api/v1/comments/"+id, nil, "alice")
	}
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/messages/"+message, nil, "alice")
	world.settle(t)
	deleted := map[string]string{reply: "pelican", askReply: "ocelot", remark: "walrus", message: "narwhal"}
	words := asOfComparison{world: &asOfWorld{deleted: deleted}}

	issue := "/api/v1/issues/" + world.issue
	for _, read := range []struct {
		target string
		// named is whether the route's absent list holds the body of the posts it shows, so the
		// answer leaves it out and names it; elsewhere a post object gives the empty body.
		named bool
		posts []string
	}{
		{issue + "/comments", true, []string{reply, askReply}},
		{"/api/v1/artifacts/" + document + "/comments", true, []string{remark}},
		{"/api/v1/comments/" + root, true, []string{reply}},
		{"/api/v1/comments/" + reply, true, []string{reply}},
		{"/api/v1/comments/" + askReply, true, []string{askReply}},
		{issue + "/messages", true, []string{message}},
		{issue + "/messages/" + message, true, []string{message}},
		{"/api/v1/messages/" + message, true, []string{message}},
		{"/api/v1/messages/" + quote, true, []string{message}},
		{issue + "/asks?thread=1", true, []string{askReply}},
		{"/api/v1/asks/" + ask, true, []string{askReply}},
		{"/api/v1/inbox?assignee=me", true, []string{askReply}},
		{"/api/v1/asks/open?project=" + project, false, nil},
	} {
		t.Run(read.target, func(t *testing.T) {
			doc, absent := world.readAt(t, read.target, cut)
			for _, problem := range words.deletedBodies(doc) {
				t.Error(problem)
			}
			posts := postsByID(doc)
			for _, id := range read.posts {
				if len(posts[id]) == 0 {
					t.Errorf("post %s, deleted after the cut, is not in the answer at the cut: %s", id, abbreviateValue(doc))
				}
			}
			for id := range deleted {
				for _, post := range posts[id] {
					if post.object["deleted_at"] == nil {
						t.Errorf("%s is post %s, deleted after the cut, and reads at the cut with no deleted_at", orRoot(post.path), id)
					}
					body, holdsBody := post.object["body"]
					member := pointerJoin(post.path, "body")
					switch {
					case read.named && holdsBody:
						t.Errorf("%s is post %s, deleted after the cut, and gives a body %v at the cut: the route leaves it out", orRoot(post.path), id, body)
					case read.named && !namedAbsent(absent, member):
						t.Errorf("%s is post %s, deleted after the cut, and leaves out its body without naming %s in %s %v",
							orRoot(post.path), id, member, contracts.DispatchAsOfAbsentHeader, absent)
					case !read.named && body != "":
						t.Errorf("%s is post %s, deleted after the cut, and gives the body %v at the cut, want the empty body of a deleted post",
							orRoot(post.path), id, body)
					}
				}
			}
		})
	}

	t.Run("the session's answers", func(t *testing.T) {
		doc, _ := world.readAt(t, "/api/v1/agents/"+asOfListenerSession+"/answers", cut)
		for _, problem := range words.deletedBodies(doc) {
			t.Error(problem)
		}
		items, _ := doc.(map[string]any)["items"].([]any)
		found := false
		for _, element := range items {
			item, _ := element.(map[string]any)
			if item["id"] != message {
				continue
			}
			found = true
			if item["deleted_at"] == nil || item["text"] != "" {
				t.Errorf("the message deleted after the cut answers %s at the cut, want its deleted_at and an empty text", abbreviateValue(item))
			}
		}
		if !found {
			t.Errorf("the session's answers at the cut do not list the message it took before the cut: %s", abbreviateValue(doc))
		}
	})

	for _, history := range []string{"/api/v1/comments/" + reply + "/history", "/api/v1/messages/" + message + "/history"} {
		t.Run(history, func(t *testing.T) {
			doc, _ := world.readAt(t, history, cut)
			read, _ := doc.(map[string]any)
			entries, _ := read["entries"].([]any)
			if read["deleted_at"] == nil || len(entries) != 1 {
				t.Fatalf("the history at the cut is %s, want the post's deleted_at and the one entry its edit before the cut wrote", abbreviateValue(doc))
			}
			if body := entries[0].(map[string]any)["body"]; body != "" {
				t.Errorf("the entry at the cut gives the body %v, want the empty body of a deleted post", body)
			}
		})
	}
}

// TestAnIssueMessageEditedBeforeAndAfterTheCutReadsItsTextThen: an issue message its author edits
// before the cut, to a body longer than an event carries whole (32,000 UTF-16 units), and again
// after the cut reads at the cut, on every message route, with the text of the first edit and
// that edit's time, and its history at the cut holds the one body that edit replaced.
func TestAnIssueMessageEditedBeforeAndAfterTheCutReadsItsTextThen(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOE")
	// Two paragraphs, each under the block limit, together past what an event carries whole.
	paragraph := strings.Repeat("harbour ", 2500)
	long := "The draft as edited before the cut.\n\n" + paragraph + "\n\n" + paragraph
	if _, truncated := model.EventBody(long); !truncated {
		t.Fatalf("a body of %d bytes rides whole in an event; the test needs one the event cuts", len(long))
	}
	message := world.message(t, map[string]any{"body": "The draft as first written."}, "alice").ID
	edited := decodeBody[model.Message](t, asOfWrite(t, world.handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": long}, "alice"))
	if edited.EditedAt == nil {
		t.Fatalf("the edit before the cut answers no edited_at: %+v", edited)
	}
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, world.handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": "The draft as edited after the cut."}, "alice")
	world.settle(t)

	issue := "/api/v1/issues/" + world.issue
	for _, target := range []string{issue + "/messages", issue + "/messages/" + message, "/api/v1/messages/" + message} {
		t.Run(target, func(t *testing.T) {
			if posts := postsByID(world.readNow(t, target))[message]; len(posts) != 1 || posts[0].object["body"] != "The draft as edited after the cut." {
				t.Fatalf("the present read gives %v, want the body of the edit after the cut", posts)
			}
			doc, absent := world.readAt(t, target, cut)
			posts := postsByID(doc)[message]
			if len(posts) != 1 {
				t.Fatalf("the answer at the cut holds the message %d times, want once: %s", len(posts), abbreviateValue(doc))
			}
			post := posts[0]
			if body, _ := post.object["body"].(string); body != long {
				t.Errorf("%s/body at the cut is %q, want the body of the edit before the cut (%d bytes)", post.path, abbreviateAnswer([]byte(body)), len(long))
			}
			if !namesInstant(fmt.Sprint(post.object["edited_at"]), *edited.EditedAt) {
				t.Errorf("%s/edited_at at the cut is %v, want the time of the edit before the cut, %s", post.path, post.object["edited_at"], edited.EditedAt.Format(time.RFC3339Nano))
			}
			if len(absent) != 0 {
				t.Errorf("the answer at the cut names %v absent, want nothing: the message's body at the cut is kept", absent)
			}
		})
	}
	t.Run("its history", func(t *testing.T) {
		doc, _ := world.readAt(t, "/api/v1/messages/"+message+"/history", cut)
		entries, _ := doc.(map[string]any)["entries"].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["body"] != "The draft as first written." {
			t.Errorf("the history at the cut is %s, want the one body the edit before the cut replaced", abbreviateValue(doc))
		}
	})
}

// TestAPostsHistoryAtACutHoldsTheEntriesThen: both history routes answer at a cut the entries the
// post's edit and delete events at or before the cut wrote, newest first, paged by ?before=
// within them (decision 17). A comment and an issue message edited before the cut and again after
// it hold one entry at the cut; a comment deleted after the cut answers its present mark with
// every body empty; and a comment an older build edited, before post_body_history began (a
// comment.edited event older than the time schema_migrations records for the history's
// migration, with no history row), and edited again after the cut holds no entry at the cut, and
// reads at the cut with the body of the older build's edit.
func TestAPostsHistoryAtACutHoldsTheEntriesThen(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOH")
	handler := world.handler
	database := world.served.deps.Store

	// The comment an older build edited: its edit's history row goes, and the history begins
	// after its edit's event.
	legacy := world.comment(t, map[string]any{"body": "The comment as first written."})
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+legacy, map[string]any{"body": "The comment as an older build edited it."}, "alice")
	world.settle(t)
	if _, err := database.Pool.Exec(context.Background(), `delete from post_body_history where comment_id = $1`, legacy); err != nil {
		t.Fatalf("take out the history row of the older build's edit: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update schema_migrations set applied_at = clock_timestamp() where version = 20261110224508`); err != nil {
		t.Fatalf("begin the history after the older build's edit: %v", err)
	}

	comment := world.comment(t, map[string]any{"body": "The comment as first written."})
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+comment, map[string]any{"body": "The comment as edited before the cut."}, "alice")
	message := world.message(t, map[string]any{"body": "The message as first written."}, "alice").ID
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": "The message as edited before the cut."}, "alice")
	doomed := world.comment(t, map[string]any{"body": "The comment its author deletes later."})
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+doomed, map[string]any{"body": "The comment its author deletes later, edited."}, "alice")
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+comment, map[string]any{"body": "The comment as edited after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": "The message as edited after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodDelete, "/api/v1/comments/"+doomed, nil, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+legacy, map[string]any{"body": "The comment as edited after the cut."}, "alice")
	world.settle(t)

	bodies := func(doc any) []any {
		entries, _ := doc.(map[string]any)["entries"].([]any)
		bodies := []any{}
		for _, entry := range entries {
			bodies = append(bodies, entry.(map[string]any)["body"])
		}
		return bodies
	}
	for _, row := range []struct {
		what, target  string
		present, then []any
		deleted       bool
	}{
		{"a comment edited before and after the cut", "/api/v1/comments/" + comment + "/history",
			[]any{"The comment as edited before the cut.", "The comment as first written."}, []any{"The comment as first written."}, false},
		{"a message edited before and after the cut", "/api/v1/messages/" + message + "/history",
			[]any{"The message as edited before the cut.", "The message as first written."}, []any{"The message as first written."}, false},
		{"a comment deleted after the cut", "/api/v1/comments/" + doomed + "/history", []any{"", ""}, []any{""}, true},
		{"a comment an older build edited before the history began", "/api/v1/comments/" + legacy + "/history",
			[]any{"The comment as an older build edited it."}, []any{}, false},
	} {
		t.Run(row.what, func(t *testing.T) {
			if got := bodies(world.readNow(t, row.target)); !slices.Equal(got, row.present) {
				t.Fatalf("the present history is %v, want %v", got, row.present)
			}
			doc, _ := world.readAt(t, row.target, cut)
			if got := bodies(doc); !slices.Equal(got, row.then) {
				t.Errorf("the history at the cut is %v, want %v: the entries the events at the cut wrote", got, row.then)
			}
			if marked := doc.(map[string]any)["deleted_at"] != nil; marked != row.deleted {
				t.Errorf("the history at the cut carries a deleted_at %t, want %t: the mark is the post's present one", marked, row.deleted)
			}
			// A page before a time later than every row holds the same entries: ?before= pages
			// within the entries at the cut.
			paged, _ := world.readAt(t, row.target+"?"+url.Values{"before": {time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)}}.Encode(), cut)
			if got := bodies(paged); !slices.Equal(got, row.then) {
				t.Errorf("the page before a later time at the cut is %v, want %v", got, row.then)
			}
		})
	}
	t.Run("the older build's edit is the body at the cut", func(t *testing.T) {
		doc, _ := world.readAt(t, "/api/v1/comments/"+legacy, cut)
		if body := doc.(map[string]any)["comment"].(map[string]any)["body"]; body != "The comment as an older build edited it." {
			t.Errorf("the comment's body at the cut is %v, want the body of the older build's edit", body)
		}
	})
}

// TestAnInboxAtACutListsTheAsksOpenThen: the inbox at a cut lists the asks open at the cut
// (decision 10). An ask answered after the cut is listed, one opened after the cut is not, and
// the ask's waiting_on follows its thread's newest reply at the cut, a person's (the agent's
// turn), not the session's reply after the cut. Snoozes keep no history, so the snooze is left
// out and named absent.
func TestAnInboxAtACutListsTheAsksOpenThen(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOI")
	handler := world.handler
	asks := "/api/v1/issues/" + world.issue + "/asks"
	answered := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, asks,
		map[string]any{"question": "Which cut does the inbox read?"})).ID
	world.comment(t, map[string]any{"body": "Let me look at the cut first.", "ask_id": answered})
	asOfWrite(t, handler, http.MethodPut, "/api/v1/me/asks/"+answered+"/snooze",
		map[string]any{"snoozed_until": time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)}, "alice")
	world.settle(t)
	waitingOn := func(doc any) map[string]any {
		turns := map[string]any{}
		rows, _ := doc.([]any)
		for _, element := range rows {
			row, _ := element.(map[string]any)
			turns[fmt.Sprint(row["id"])] = row["waiting_on"]
		}
		return turns
	}
	if turn := waitingOn(world.readNow(t, "/api/v1/inbox?assignee=me"))[answered]; turn != "agent" {
		t.Fatalf("the ask's waiting_on is %v after the person's reply, want agent", turn)
	}
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/comments",
		map[string]any{"body": "The cut is the database's clock.", "ask_id": answered})
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{"text": "Read it at the cut."}, "alice")
	opened := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, asks,
		map[string]any{"question": "Is an ask opened after the cut in the inbox at it?"})).ID
	world.settle(t)

	present := waitingOn(world.readNow(t, "/api/v1/inbox?assignee=me"))
	if _, listed := present[answered]; listed || present[opened] == nil {
		t.Fatalf("the present inbox lists %v, want the ask opened after the cut and not the one answered", present)
	}
	for _, target := range []string{"/api/v1/inbox?assignee=me", "/api/v1/inbox"} {
		t.Run(target, func(t *testing.T) {
			doc, absent := world.readAt(t, target, cut)
			if got := waitingOn(doc); len(got) != 1 || got[answered] != "agent" {
				t.Errorf("the inbox at the cut lists %v (by waiting_on), want only the ask open at the cut, %s, on the agent's turn: the person's reply is its newest at the cut",
					got, answered)
			}
			if !slices.Contains(absent, "/*/snoozed_until") {
				t.Errorf("the inbox at the cut names %v absent, want /*/snoozed_until: a snooze keeps no history", absent)
			}
			if rows, _ := doc.([]any); len(rows) == 1 {
				if _, given := rows[0].(map[string]any)["snoozed_until"]; given {
					t.Errorf("the inbox at the cut gives the snooze %v, which keeps no history", rows[0].(map[string]any)["snoozed_until"])
				}
			}
		})
	}
}

// TestALongCommentEditedAfterTheCutNamesItsBodyAbsent: a comment as an older build wrote it, before
// post_body_history began: its edit before the cut was longer than an event carries whole, so the
// comment.edited event carries the body cut at 32,000 UTF-16 units (body_truncated), and its edit
// after the cut kept no history row. No record holds its body at the cut, so the comment routes
// leave the body out and name it absent (reason T), and give no part of either body. The comment
// is a reply in an open ask's thread, so the ask's read, the issue's ask list with its threads and
// the inbox leave it out of the thread too.
func TestALongCommentEditedAfterTheCutNamesItsBodyAbsent(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOT")
	database := world.served.deps.Store
	long := func(word string) string {
		paragraph := strings.Repeat(word+" ", 2500)
		return "A long comment.\n\n" + paragraph + "\n\n" + paragraph
	}
	if _, truncated := model.EventBody(long("puffin")); !truncated {
		t.Fatalf("a body of %d bytes rides whole in an event; the test needs one the event cuts", len(long("puffin")))
	}
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, world.handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/asks",
		map[string]any{"question": "Which body did the long reply hold?"})).ID
	comment := world.comment(t, map[string]any{"body": long("gannet"), "ask_id": ask})
	asOfWrite(t, world.handler, http.MethodPatch, "/api/v1/comments/"+comment, map[string]any{"body": long("puffin")}, "alice")
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, world.handler, http.MethodPatch, "/api/v1/comments/"+comment, map[string]any{"body": "Cut short after the cut: cormorant."}, "alice")
	world.settle(t)
	// As an older build wrote it: the edits kept no body, and the history began after them.
	if _, err := database.Pool.Exec(context.Background(), `delete from post_body_history where comment_id = $1`, comment); err != nil {
		t.Fatalf("take out the comment's history rows: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(),
		`update schema_migrations set applied_at = clock_timestamp() where version = 20261110224508`); err != nil {
		t.Fatalf("begin the history after the edits: %v", err)
	}

	issue := "/api/v1/issues/" + world.issue
	for _, target := range []string{
		issue + "/comments", "/api/v1/comments/" + comment,
		"/api/v1/asks/" + ask, issue + "/asks?thread=1", "/api/v1/inbox",
	} {
		t.Run(target, func(t *testing.T) {
			doc, absent := world.readAt(t, target, cut)
			for _, held := range stringsHolding(doc, "gannet", "puffin", "cormorant") {
				t.Errorf("%s gives part of a body the comment did not hold at the cut, or that no record holds", held)
			}
			posts := postsByID(doc)[comment]
			if len(posts) != 1 {
				t.Fatalf("the answer at the cut holds the comment %d times, want once: %s", len(posts), abbreviateValue(doc))
			}
			post := posts[0]
			if body, given := post.object["body"]; given {
				t.Errorf("%s/body at the cut is %s, which no record holds: the answer leaves it out", post.path, abbreviateValue(body))
			}
			if member := pointerJoin(post.path, "body"); !namedAbsent(absent, member) {
				t.Errorf("the answer at the cut names %v absent, want %s", absent, member)
			}
			if post.object["deleted_at"] != nil {
				t.Errorf("%s reads as deleted at the cut; nobody deleted it", post.path)
			}
		})
	}
}

// TestADeliveryWhoseStateNoEventDatesIsNamedAbsent: a delivery attempt that no receipt event and no
// reply event dates, as attempts written before the receipts read (here stored as sent with an
// envelope and no event), holds no state the records at the cut give. The comment and message
// routes leave out every member of it the send writes, and name each absent (reason E): state,
// reply_id, envelope_id, created_at and the comment's resolve_error or the message's
// requested_by, with the delivery mode and the session the send resolves. Its place (the comment
// or message, the target and the attempt) and what an unsettled attempt holds (no error, no
// duplicate) are given. An attempt its receipt dates is given whole. The comment is a reply in an
// open ask's thread, so the ask's read, the issue's ask list with its threads and the inbox leave
// out the same members of it.
func TestADeliveryWhoseStateNoEventDatesIsNamedAbsent(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOS")
	database := world.served.deps.Store
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, world.handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/asks",
		map[string]any{"question": "Was the mention sent?"})).ID
	comment := world.comment(t, map[string]any{"body": "A mention sent before the receipts.", "ask_id": ask})
	message := world.message(t, map[string]any{"body": "A message sent before the receipts."}, "alice").ID
	dated := world.message(t, map[string]any{
		"body": "A message its receipt dates.", "target": "session:" + asOfListenerSession, "delivery": "btw",
	}, "alice").ID
	if _, err := database.Pool.Exec(context.Background(), `
		with mention as (
			insert into comment_mentions (comment_id, target, delivery, resolved_session_id)
			values ($1, 'session:' || $2, 'steer', $2)
		)
		insert into comment_deliveries (comment_id, target, attempt, delivery, session_id, envelope_id, state)
		values ($1, 'session:' || $2, 1, 'steer', $2, 'asof-legacy-envelope', 'sent')
	`, comment, asOfListenerSession); err != nil {
		t.Fatalf("store the mention's attempt with no event: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into message_deliveries (message_id, attempt, delivery, session_id, envelope_id, state)
		values ($1, 1, 'steer', $2, 'asof-legacy-envelope', 'sent')
	`, message, asOfListenerSession); err != nil {
		t.Fatalf("store the message's attempt with no event: %v", err)
	}
	cut := takeCut()

	commentMembers := []string{"state", "reply_id", "envelope_id", "resolve_error", "created_at", "delivery", "session_id"}
	messageMembers := []string{"state", "reply_id", "envelope_id", "created_at", "requested_by", "delivery", "session_id"}
	issue := "/api/v1/issues/" + world.issue
	for _, read := range []struct {
		target, post    string
		left, given     []string
		datedAttemptsOf string
	}{
		{issue + "/comments", comment, commentMembers, []string{"comment_id", "target", "attempt", "duplicate", "error"}, ""},
		{"/api/v1/comments/" + comment, comment, commentMembers, []string{"comment_id", "target", "attempt", "duplicate", "error"}, ""},
		{"/api/v1/asks/" + ask, comment, commentMembers, []string{"comment_id", "target", "attempt", "duplicate", "error"}, ""},
		{issue + "/asks?thread=1", comment, commentMembers, []string{"comment_id", "target", "attempt", "duplicate", "error"}, ""},
		{"/api/v1/inbox", comment, commentMembers, []string{"comment_id", "target", "attempt", "duplicate", "error"}, ""},
		{issue + "/messages", message, messageMembers, []string{"message_id", "attempt", "duplicate", "error"}, dated},
		{issue + "/messages/" + message, message, messageMembers, []string{"message_id", "attempt", "duplicate", "error"}, ""},
		{"/api/v1/messages/" + message, message, messageMembers, []string{"message_id", "attempt", "duplicate", "error"}, ""},
	} {
		t.Run(read.target, func(t *testing.T) {
			attemptsOf := func(doc any, id string) (string, []any) {
				posts := postsByID(doc)[id]
				if len(posts) != 1 {
					t.Fatalf("the answer holds post %s %d times, want once: %s", id, len(posts), abbreviateValue(doc))
				}
				attempts, _ := posts[0].object["deliveries"].([]any)
				return posts[0].path, attempts
			}
			if _, attempts := attemptsOf(world.readNow(t, read.target), read.post); len(attempts) != 1 || attempts[0].(map[string]any)["state"] != "sent" {
				t.Fatalf("the present read gives the attempts %s, want the one stored as sent", abbreviateValue(attempts))
			}
			doc, absent := world.readAt(t, read.target, cut)
			path, attempts := attemptsOf(doc, read.post)
			if len(attempts) != 1 {
				t.Fatalf("the answer at the cut gives the attempts %s, want the one stored before the cut", abbreviateValue(attempts))
			}
			attempt := attempts[0].(map[string]any)
			place := pointerJoin(pointerJoin(path, "deliveries"), "0")
			for _, member := range read.left {
				if value, given := attempt[member]; given {
					t.Errorf("%s/%s at the cut is %s, which no event at the cut dates: the answer leaves it out", place, member, abbreviateValue(value))
				}
				if !namedAbsent(absent, pointerJoin(place, member)) {
					t.Errorf("the answer at the cut names %v absent, want %s", absent, pointerJoin(place, member))
				}
			}
			for _, member := range read.given {
				if _, given := attempt[member]; !given {
					t.Errorf("%s/%s is not in the answer at the cut: the attempt's place and its unsettled members are given", place, member)
				}
			}
			if read.datedAttemptsOf != "" {
				_, attempts := attemptsOf(doc, read.datedAttemptsOf)
				if len(attempts) != 1 || attempts[0].(map[string]any)["state"] != "sent" {
					t.Errorf("the attempt its receipt dates reads at the cut as %s, want it whole, sent", abbreviateValue(attempts))
				}
			}
		})
	}
}

// TestAnAnswersPollWithASinceAfterTheCutAnswersNothing: an agent's answers poll whose since is
// later than the cut answers no items as of the cut (decision 12), since nothing after the cut
// exists at it: a since between the cut and now, where the present read lists the message sent
// after the cut, and a since later than now, which the present read refuses as a time in the
// future. Under a token that carries a cut, pi-envoy's own poll so gets an empty answer, not 400.
func TestAnAnswersPollWithASinceAfterTheCutAnswersNothing(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOP")
	send := func(body string) model.Message {
		t.Helper()
		return world.message(t, map[string]any{"body": body, "target": "session:" + asOfListenerSession, "delivery": "btw"}, "alice")
	}
	send("Sent before the cut.")
	cut := takeCut()
	time.Sleep(1100 * time.Millisecond)
	after := send("Sent after the cut.")
	world.settle(t)

	answers := "/api/v1/agents/" + asOfListenerSession + "/answers"
	afterTheCut := after.CreatedAt.Add(-time.Millisecond)
	if !afterTheCut.After(cut.Time()) {
		t.Fatalf("the message after the cut was made at %s, not after the cut %s", after.CreatedAt, cut)
	}
	listed := func(doc any) []any {
		items, _ := doc.(map[string]any)["items"].([]any)
		ids := []any{}
		for _, item := range items {
			ids = append(ids, item.(map[string]any)["id"])
		}
		return ids
	}
	t.Run("a since between the cut and now", func(t *testing.T) {
		target := answers + "?" + url.Values{"since": {afterTheCut.Format(time.RFC3339Nano)}}.Encode()
		if ids := listed(world.readNow(t, target)); !slices.Equal(ids, []any{after.ID}) {
			t.Fatalf("the present poll lists %v, want the message sent after the cut, %s", ids, after.ID)
		}
		doc, _ := world.readAt(t, target, cut)
		if ids := listed(doc); len(ids) != 0 || fmt.Sprint(doc.(map[string]any)["total"]) != "0" {
			t.Errorf("the poll as of the cut answers %s, want no items", abbreviateValue(doc))
		}
	})
	t.Run("a since later than now", func(t *testing.T) {
		later := time.Now().UTC().Add(time.Hour)
		target := answers + "?" + url.Values{"since": {later.Format(time.RFC3339Nano)}}.Encode()
		if present := dispatchRequest(t, world.handler, http.MethodGet, target, nil, "alice"); present.Code != http.StatusBadRequest {
			t.Fatalf("the present poll of a since in the future: status=%d body=%s, want 400", present.Code, present.Body.String())
		}
		doc, _ := world.readAt(t, target, cut)
		if ids := listed(doc); len(ids) != 0 {
			t.Errorf("the poll as of the cut answers %v, want no items", ids)
		}
		if since := fmt.Sprint(doc.(map[string]any)["since"]); !namesInstant(since, later) {
			t.Errorf("the poll as of the cut names since %s, want the since it was sent, %s", since, later.Format(time.RFC3339Nano))
		}
	})
}

// TestAReplyBlockAtACutReadsTheBlockThen: the block a reply names reads at the cut as the block
// index holds it now where the item that holds it did not change after the cut, which is how it
// stood at the cut: its text and that it is not removed. Where the item changed after the cut (a
// comment or a message edited, an ask answered), the change rewrote the item's blocks and no
// record keeps them at the cut, so every route that shows the reply leaves out the block's text
// and whether it is removed, and names both absent (reason E). The block's node and id are the
// reply's own, and are given.
func TestAReplyBlockAtACutReadsTheBlockThen(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOB")
	handler := world.handler
	edited := world.comment(t, map[string]any{"body": "Alpha.\n\nBeta."})
	kept := world.comment(t, map[string]any{"body": "Gamma.\n\nDelta."})
	onEdited := world.comment(t, map[string]any{"body": "On beta.", "reply_to": edited, "reply_to_block": "b2"})
	onKept := world.comment(t, map[string]any{"body": "On delta.", "reply_to": kept, "reply_to_block": "b2"})
	message := world.message(t, map[string]any{"body": "One.\n\nTwo."}, "alice").ID
	onMessage := world.message(t, map[string]any{"body": "On two.", "in_reply_to": message, "in_reply_to_block": "b2"}, "bob").ID
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/asks", map[string]any{
		"question": "Which day do we ship?", "options": []map[string]string{{"label": "Monday"}, {"label": "Tuesday"}},
	})).ID
	onAsk := world.comment(t, map[string]any{"body": "Monday works.", "ask_id": ask, "reply_to_block": "b2"})
	cut := takeCut()
	time.Sleep(5 * time.Millisecond)
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/comments/"+edited, map[string]any{"body": "Alpha.\n\nBeta, rewritten after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPatch, "/api/v1/messages/"+message, map[string]any{"body": "One.\n\nTwo, rewritten after the cut."}, "alice")
	asOfWrite(t, handler, http.MethodPost, "/api/v1/asks/"+ask+"/answer", map[string]any{"selected": []string{"Monday"}}, "alice")
	world.settle(t)

	replyBlock := func(t *testing.T, doc any, id string) (string, map[string]any) {
		t.Helper()
		posts := postsByID(doc)[id]
		if len(posts) != 1 {
			t.Fatalf("the answer holds the reply %s %d times, want once: %s", id, len(posts), abbreviateValue(doc))
		}
		block, _ := posts[0].object["reply_block"].(map[string]any)
		if block == nil || block["block_id"] != "b2" || block["node"] == nil {
			t.Fatalf("%s/reply_block is %s, want the block b2 the reply names, with its node", posts[0].path, abbreviateValue(posts[0].object["reply_block"]))
		}
		return pointerJoin(posts[0].path, "reply_block"), block
	}
	issue := "/api/v1/issues/" + world.issue
	for _, read := range []struct {
		target              string
		changed, changeless []string
	}{
		{issue + "/comments", []string{onEdited, onAsk}, []string{onKept}},
		{"/api/v1/comments/" + onEdited, []string{onEdited}, nil},
		{"/api/v1/comments/" + edited, []string{onEdited}, nil},
		{"/api/v1/comments/" + kept, nil, []string{onKept}},
		{"/api/v1/comments/" + onAsk, []string{onAsk}, nil},
		{issue + "/messages", []string{onMessage}, nil},
		{issue + "/messages/" + onMessage, []string{onMessage}, nil},
		{"/api/v1/messages/" + message, []string{onMessage}, nil},
		{"/api/v1/asks/" + ask, []string{onAsk}, nil},
		{issue + "/asks?thread=1", []string{onAsk}, nil},
		{"/api/v1/inbox", []string{onAsk}, nil},
	} {
		t.Run(read.target, func(t *testing.T) {
			doc, absent := world.readAt(t, read.target, cut)
			for _, id := range read.changed {
				place, block := replyBlock(t, doc, id)
				for _, member := range []string{"text", "removed"} {
					if value, given := block[member]; given {
						t.Errorf("%s/%s at the cut is %s, and the item that holds the block changed after the cut: the answer leaves it out",
							place, member, abbreviateValue(value))
					}
					if !namedAbsent(absent, pointerJoin(place, member)) {
						t.Errorf("the answer at the cut names %v absent, want %s", absent, pointerJoin(place, member))
					}
				}
			}
			for _, id := range read.changeless {
				place, block := replyBlock(t, doc, id)
				_, present := replyBlock(t, world.readNow(t, read.target), id)
				// The header names a pointer for every reply of a list at once, so the block of an
				// unchanged item shows that it is given by carrying both members.
				if block["text"] != present["text"] || block["removed"] != false {
					t.Errorf("%s at the cut is %s, want the block as its unchanged item holds it, %s", place, abbreviateValue(block), abbreviateValue(present))
				}
			}
		})
	}
}

// TestAnAnchoredReadAtACutLeavesTheBlocksPlaceOut: the read of one comment and of one ask gives
// the place of its anchored block in the document (anchor_block), which it reads from the live
// document. At a cut the read reads no document: a stored version is markdown, which holds the id
// of a typed block only, so no record gives the place of this paragraph at the cut. The read
// leaves anchor_block out and names it absent (reason E).
func TestAnAnchoredReadAtACutLeavesTheBlocksPlaceOut(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOA")
	handler := world.handler
	issue := asOfIssue(t, handler, map[string]any{
		"project": "ASOA", "title": "An anchored spec", "spec": "Intro.\n\nThe second paragraph.\n",
	}).Key
	comment := decodeBody[model.Comment](t, asOfWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Intro."}, "body": "On the intro.",
	}, "alice")).ID
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/asks", map[string]any{
		"anchor": map[string]any{"artifact": "spec", "quote": "Intro."}, "question": "Is the intro right?",
	})).ID
	cut := takeCut()
	for _, read := range []struct{ target, object string }{
		{"/api/v1/comments/" + comment, "comment"},
		{"/api/v1/asks/" + ask, "ask"},
	} {
		t.Run(read.target, func(t *testing.T) {
			if present, _ := world.readNow(t, read.target).(map[string]any)[read.object].(map[string]any); present["anchor_block"] == nil {
				t.Fatalf("the present read gives no anchor_block: %s", abbreviateValue(present))
			}
			doc, absent := world.readAt(t, read.target, cut)
			object, _ := doc.(map[string]any)[read.object].(map[string]any)
			if value, given := object["anchor_block"]; given {
				t.Errorf("/%s/anchor_block at the cut is %s, which no record at the cut gives: the read leaves it out", read.object, abbreviateValue(value))
			}
			if !namedAbsent(absent, "/"+read.object+"/anchor_block") {
				t.Errorf("the answer at the cut names %v absent, want /%s/anchor_block", absent, read.object)
			}
			if object["anchor"] == nil {
				t.Errorf("/%s/anchor at the cut is not given: the anchor is the row's", read.object)
			}
		})
	}
}

// TestAnAsksRouteReachAtACutIsLeftOutWhole: the reach of an ask's route is read from the route
// roster now, which keeps no past state. At a cut every read of the ask gives the route, which the
// issue held then, and leaves out the route's status, its holder and the reason for its status,
// and names the three absent (reason L): the ask's read, the issue's ask list with its threads and
// the inbox.
func TestAnAsksRouteReachAtACutIsLeftOutWhole(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOR")
	route := "session:" + asOfListenerSession
	asOfWrite(t, world.handler, http.MethodPatch, "/api/v1/issues/"+world.issue, map[string]any{"route": route}, "alice")
	ask := decodeBody[model.Ask](t, asOfSessionWrite(t, world.handler, http.MethodPost, "/api/v1/issues/"+world.issue+"/asks",
		map[string]any{"question": "Who holds the route?"})).ID
	cut := takeCut()
	for _, read := range []struct{ target, thread string }{
		{"/api/v1/asks/" + ask, ""},
		{"/api/v1/issues/" + world.issue + "/asks?thread=1", "/0/thread"},
		{"/api/v1/inbox", "/0/thread"},
	} {
		t.Run(read.target, func(t *testing.T) {
			reach := func(doc any) map[string]any {
				tokens := pointerTokens(read.thread)
				var found map[string]any
				eachParent(doc, tokens, "", func(_ string, object map[string]any) { found = object })
				if found == nil {
					t.Fatalf("the answer holds no %q: %s", orRoot(read.thread), abbreviateValue(doc))
				}
				return found
			}
			if present := reach(world.readNow(t, read.target)); present["route"] != route || present["route_status"] == nil {
				t.Fatalf("the present read gives the reach %s, want the route with its status", abbreviateValue(present))
			}
			doc, absent := world.readAt(t, read.target, cut)
			object := reach(doc)
			if object["route"] != route {
				t.Errorf("%s/route at the cut is %v, want %s, the issue's route then", read.thread, object["route"], route)
			}
			for _, member := range []string{"route_status", "route_holder", "route_status_reason"} {
				if value, given := object[member]; given {
					t.Errorf("%s/%s at the cut is %s, which the roster gives now: the answer leaves it out", read.thread, member, abbreviateValue(value))
				}
				if !namedAbsent(absent, read.thread+"/"+member) {
					t.Errorf("the answer at the cut names %v absent, want %s/%s", absent, read.thread, member)
				}
			}
		})
	}
}

// TestAnInboxAtACutNamesAnOldApprovalsDocumentsAbsent: an approval request written before
// approval requests held their documents (its events carry an approval with no documents, which
// migration 20261102110100 filled in the row with no event) gives no documents at the cut. The
// inbox at the cut leaves approval.documents out and names it absent (reason B), as the ask's own
// read does, rather than give the empty list of documents.
func TestAnInboxAtACutNamesAnOldApprovalsDocumentsAbsent(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOV")
	database := world.served.deps.Store
	spec := asOfIssue(t, world.handler, map[string]any{
		"project": "ASOV", "title": "An approval from before its documents", "spec": "# Spec\n\nApprove this.\n",
	}).PrimaryArtifactID
	requested := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, asOfSessionWrite(t, world.handler, http.MethodPost, "/api/v1/artifacts/"+spec+"/approval-requests",
		map[string]any{"summary": "Approve the spec."})).Ask.ID
	world.settle(t)
	// As a build before approval requests held their documents wrote it.
	if _, err := database.Pool.Exec(context.Background(), `
		update events set payload = payload #- '{approval,documents}'
		where payload->>'id' = $1 and type like 'ask.%'`, requested); err != nil {
		t.Fatalf("write the request's events as an older build wrote them: %v", err)
	}
	cut := takeCut()
	for _, target := range []string{"/api/v1/inbox", "/api/v1/asks/" + requested} {
		t.Run(target, func(t *testing.T) {
			doc, absent := world.readAt(t, target, cut)
			var approval map[string]any
			switch root := doc.(type) {
			case []any:
				for _, element := range root {
					if row, _ := element.(map[string]any); row["id"] == requested {
						approval, _ = row["approval"].(map[string]any)
					}
				}
			case map[string]any:
				ask, _ := root["ask"].(map[string]any)
				approval, _ = ask["approval"].(map[string]any)
			}
			if approval == nil {
				t.Fatalf("the answer at the cut holds no approval of the request %s: %s", requested, abbreviateValue(doc))
			}
			if value, given := approval["documents"]; given {
				t.Errorf("approval/documents at the cut is %s, which no event at the cut gives: the answer leaves it out", abbreviateValue(value))
			}
			want := "/ask/approval/documents"
			if target == "/api/v1/inbox" {
				want = "/*/approval/documents"
			}
			if !slices.Contains(absent, want) {
				t.Errorf("the answer at the cut names %v absent, want %s", absent, want)
			}
		})
	}
}

// TestAnOldApprovalRequestMovedBeforeTheCutWaitsOnItsAgentThen: an approval request written before
// approval requests held their documents gives its version pair at the top level of its approval
// alone (approval.version and approval.requested_version, which migration 20261102110100 copied
// into its first document with no event). A version moved it past the version its agent handed
// over before the cut, and its agent handed it back after the cut. At the cut it waited on its
// agent, as the turn rule reads that pair, so each route that gives the request's turn gives agent
// as of the cut, where it gives human now: the issue read, for its open ask and for its spec's
// approval, the issue's asks, the ask's own read, open asks and the inbox.
func TestAnOldApprovalRequestMovedBeforeTheCutWaitsOnItsAgentThen(t *testing.T) {
	world, takeCut := newAsOfThreadWorld(t, "ASOM")
	database := world.served.deps.Store
	issue := asOfIssue(t, world.handler, map[string]any{
		"project": "ASOM", "title": "An old approval moved before the cut", "spec": "# Spec\n\nApprove this.\n",
	})
	summary := map[string]any{"summary": "Approve the spec."}
	request := "/api/v1/artifacts/" + issue.PrimaryArtifactID + "/approval-requests"
	requested := decodeBody[struct {
		Ask model.Ask `json:"ask"`
	}](t, asOfSessionWrite(t, world.handler, http.MethodPost, request, maps.Clone(summary))).Ask.ID
	asOfWrite(t, world.handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops":     []map[string]string{{"op": "insert", "after": "end", "markdown": "A paragraph that moves the request.\n"}},
		"summary": "The spec's second version",
	}, "alice")
	world.settle(t)
	// As a build before approval requests held their documents wrote them.
	if _, err := database.Pool.Exec(context.Background(), `
		update events set payload = payload #- '{approval,documents}'
		where payload->>'id' = $1 and type like 'ask.%'`, requested); err != nil {
		t.Fatalf("write the request's events as an older build wrote them: %v", err)
	}
	var version, requestedVersion int
	if err := database.Pool.QueryRow(context.Background(), `
		select (payload->'approval'->>'version')::int, (payload->'approval'->>'requested_version')::int
		from events where payload->>'id' = $1 and type like 'ask.%' order by id desc limit 1`, requested).Scan(&version, &requestedVersion); err != nil ||
		version != 2 || requestedVersion != 1 {
		t.Fatalf("the request's newest event names version %d, requested at %d (%v); want version 2 requested at 1, which the edit moved it to",
			version, requestedVersion, err)
	}
	cut := takeCut()
	asOfSessionWrite(t, world.handler, http.MethodPost, request, maps.Clone(summary))
	world.settle(t)

	for _, target := range []string{
		"/api/v1/issues/" + issue.Key,
		"/api/v1/issues/" + issue.Key + "/asks",
		"/api/v1/asks/" + requested,
		"/api/v1/asks/open?project=ASOM",
		"/api/v1/inbox",
	} {
		t.Run(target, func(t *testing.T) {
			if turns := requestTurns(world.readNow(t, target), requested); !turnsAre(turns, "human") {
				t.Fatalf("now the request's turns are %v, want human at each place: its agent handed it back", turns)
			}
			doc, _ := world.readAt(t, target, cut)
			if turns := requestTurns(doc, requested); !turnsAre(turns, "agent") {
				t.Errorf("at the cut the request's turns are %v, want agent at each place: a version had moved it past the version its agent handed over", turns)
			}
		})
	}
}

// requestTurns is the waiting_on of each object of doc that gives the turn of the ask id: the ask
// itself, and the approval of a document that awaits it, by its pointer.
func requestTurns(doc any, id string) map[string]string {
	turns := map[string]string{}
	var walk func(value any, path string)
	walk = func(value any, path string) {
		switch node := value.(type) {
		case map[string]any:
			if turn, ok := node["waiting_on"].(string); ok && (node["id"] == id || node["ask_id"] == id) {
				turns[orRoot(path)] = turn
			}
			for key, member := range node {
				walk(member, pointerJoin(path, key))
			}
		case []any:
			for index, element := range node {
				walk(element, pointerJoin(path, fmt.Sprint(index)))
			}
		}
	}
	walk(doc, "")
	return turns
}

// turnsAre reports whether turns holds a turn, and each is want.
func turnsAre(turns map[string]string, want string) bool {
	for _, turn := range turns {
		if turn != want {
			return false
		}
	}
	return len(turns) > 0
}

// TestADirectThreadAtACutIsReadByTheSessionRuleThen: a direct (issue-less) conversation is read by
// a credential without the person scope as a session that names itself in ?session=, and only
// where that session is in it: the root targets it, or it wrote a reply (readableMessageThread).
// At a cut the rule reads the conversation as it stood then. A session whose reply came after
// the cut is refused 403 THREAD_FORBIDDEN at the cut and reads the thread now; a conversation
// written after the cut is not found at the cut; the root's own session reads the thread then
// and now; and a read that names no session is refused 400 SESSION_REQUIRED at the cut too. No
// session ever leaves a conversation it is in: a root's target does not change, and no route
// deletes a direct message, so nothing a session could read at the cut is refused it now. No
// route of this build lets a session other than the root's target reply in a direct
// conversation, so that reply is stored as the rule reads it, a message of that session in the
// thread.
func TestADirectThreadAtACutIsReadByTheSessionRuleThen(t *testing.T) {
	handler, database, root, reply, _ := directConversationFrom(t, "alice")
	if first := reply("On it."); first.Code != http.StatusCreated {
		t.Fatalf("s1's reply: status=%d body=%s", first.Code, first.Body.String())
	}
	cut := databaseCut(t, database)
	time.Sleep(5 * time.Millisecond)
	later := postAgentMessage(t, handler, "s1", map[string]any{"body": "Written after the cut.", "delivery": "aside"}, "alice")
	if later.Code != http.StatusCreated {
		t.Fatalf("a direct message after the cut: status=%d body=%s", later.Code, later.Body.String())
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into messages (author, body, in_reply_to, target)
		values ('{"kind":"session","id":"s2"}', 'S2 joins after the cut.', $1, 'session:s1')
	`, root.ID); err != nil {
		t.Fatalf("store s2's reply in the conversation: %v", err)
	}
	laterID := decodeBody[model.Message](t, later).ID

	read := func(t *testing.T, target string, atCut bool) *httptest.ResponseRecorder {
		t.Helper()
		if atCut {
			target = withAsOf(t, target, cut)
		}
		return bearerRequest(t, handler, http.MethodGet, target, nil)
	}
	for _, row := range []struct {
		what, target string
		thenStatus   int
		thenCode     string
		nowStatus    int
	}{
		{"the root's session", "/api/v1/messages/" + root.ID + "?session=s1", http.StatusOK, "", http.StatusOK},
		{"a session whose reply came after the cut", "/api/v1/messages/" + root.ID + "?session=s2", http.StatusForbidden, "THREAD_FORBIDDEN", http.StatusOK},
		{"a conversation written after the cut", "/api/v1/messages/" + laterID + "?session=s1", http.StatusNotFound, "", http.StatusOK},
		{"a read that names no session", "/api/v1/messages/" + root.ID, http.StatusBadRequest, "SESSION_REQUIRED", http.StatusBadRequest},
	} {
		t.Run(row.what, func(t *testing.T) {
			if now := read(t, row.target, false); now.Code != row.nowStatus {
				t.Fatalf("the present read: status=%d body=%s, want %d", now.Code, now.Body.String(), row.nowStatus)
			}
			then := read(t, row.target, true)
			if then.Code != row.thenStatus || (row.thenCode != "" && decodeBody[refusalAnswer](t, then).Code != row.thenCode) {
				t.Fatalf("the read at the cut: status=%d body=%s, want %d %s", then.Code, then.Body.String(), row.thenStatus, row.thenCode)
			}
			if then.Code == http.StatusForbidden && strings.Contains(then.Body.String(), "On it.") {
				t.Errorf("the refusal at the cut gives the conversation's text: %s", then.Body.String())
			}
			if then.Code == http.StatusOK {
				if named := then.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
					t.Errorf("the read at the cut names %s %q, want the cut %s", contracts.DispatchAsOfHeader, named, cut)
				}
				thread := decodeBody[messageRead](t, then)
				if thread.Message.ID != root.ID || len(thread.Replies) != 1 {
					t.Errorf("the thread at the cut is %s with %d replies, want %s with s1's one reply", thread.Message.ID, len(thread.Replies), root.ID)
				}
			}
		})
	}
}
