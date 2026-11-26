package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// correctionDeleteRoutes is the path of the route that deletes each kind of post, read from the
// route table: every DELETE row of idRoutes whose {id} names a kind that keeps a body history
// (postHistoryOwner). That history is where a delete keeps the body it clears
// (clearDeletedPostBody), so a route that deletes a post of a new kind joins both, and
// TestEveryIDRouteIsNamed holds idRoutes to every route with an id the server serves. It fails t
// when two routes delete one kind: each post is deleted once, so a second route needs posts of its
// own in the test that reads this.
func correctionDeleteRoutes(t *testing.T) map[refs.ItemKind]string {
	t.Helper()
	routes := map[refs.ItemKind]string{}
	for pattern, route := range idRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		if method != http.MethodDelete {
			continue
		}
		kind := refs.ItemKind(route.Kind)
		if _, _, err := postHistoryOwner(kind); err != nil {
			continue
		}
		if held, twice := routes[kind]; twice {
			t.Fatalf("two routes delete a %s, %s and %s: give the second posts of its own", kind, held, path)
		}
		routes[kind] = path
	}
	return routes
}

// R3 of dispatch://INFRA-1523, and the architect's decision 3 of 2026-11-20 07:48Z: no correction
// event carries text of another post. The test makes every kind of post a correction deletes - a
// comment and a message, a reply of each both plain and to a block, a reply to a part of an ask, an
// anchored comment, a suggestion, an accepted suggestion, whose delete names the version it
// renamed (renamed_version, the architect's decision of 2026-11-20 09:18Z), a pending insert
// suggestion with a comment anchored on the words it inserted, and a comment of a project
// document with a reply to its first block - each with words of its own. It deletes each one
// through the route the route table names for its kind (correctionDeleteRoutes), every reply
// while the post it answers still stands and the comment on the inserted words while the insert
// does, and takes the event kinds a correction writes from what each delete appended about the
// posts made here, not from a list of names: the insert's delete appends the anchor refresh of the
// comment on its words, which its reject's removal of the words orphans, a correction event about
// another post. Then it reads every event of those kinds the issue and the project document hold,
// as the events table stores it and as GET /api/v1/issues/{key}/events and
// GET /api/v1/artifacts/{id}/events give it - a project document belongs to no issue, so its
// comments' events are the document's own - and fails on any that carries a phrase of a post other
// than the one the event is about. The ask a reply answers is a post no route deletes, so its
// words are only ever another post's.
//
// The inserted words are no phrase of the insert's post here. The spec holds them, and the comment
// on them quoted them as document text; the document's history keeps them, that quote included,
// though the insert's delete takes them out of the live document (the architect's decision of
// 2026-11-21 19:47Z on dispatch://INFRA-1523). So that comment's events, its anchor refresh and its
// own delete among them, may carry them; what none may carry is the insert's body.
//
// The control: before the deletes, the creation of each reply that freezes words of the post it
// answers carries those words in its preview, so a delete event that copied them would be seen.
func TestNoCorrectionEventCarriesTextOfAnotherPost(t *testing.T) {
	t.Parallel()
	documentService, handler, database := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Every correction event", "The zarquoninsertedwords quick brown fox")
	routes := correctionDeleteRoutes(t)

	message := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "zarquonmessageroot opens the thread\n\nIts second paragraph.",
	}, "alice")
	messageReply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "zarquonmessagereply answers it", "in_reply_to": message.ID,
	}, "bob")
	messageBlockReply := createIssueMessage(t, handler, issue.Key, map[string]any{
		"body": "zarquonmessageblockreply answers its first block", "in_reply_to": message.ID, "in_reply_to_block": "b1",
	}, "bob")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquoncommentroot opens the thread\n\nIts second paragraph.",
	}, "alice")
	commentReply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquoncommentreply answers it", "reply_to": comment.ID,
	}, "bob")
	commentBlockReply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquoncommentblockreply answers its first block", "reply_to": comment.ID, "reply_to_block": "b1",
	}, "bob")
	ask := createPartAsk(t, handler, issue.Key, "zarquonaskquestion which day ships?",
		map[string]string{"label": "zarquonaskoption"}, map[string]string{"label": "Tuesday"})
	askReply := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquonaskreply takes the first option", "ask_id": ask.ID, "reply_to_block": "b2",
	}, "alice")
	anchored := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquonanchored is about the fox", "anchor": map[string]any{"artifact": "spec", "quote": "quick"},
	}, "alice")
	suggestion := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "zarquonsuggestion says it better",
		"anchor":     map[string]any{"artifact": "spec", "quote": "brown"},
		"suggestion": map[string]string{"replace_with": "zarquonsuggestedwords"},
	}, "alice")
	accepted := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "zarquonacceptedbody says it better",
		"anchor":     map[string]any{"artifact": "spec", "quote": "fox"},
		"suggestion": map[string]string{"replace_with": "dog"},
	}, "alice")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+accepted.ID+"/accept", map[string]any{}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("accept the suggestion: status=%d body=%s", response.Code, response.Body.String())
	}
	// A pending insert suggestion on a browser's insert mark around words the spec holds, and a
	// comment anchored on those words, which its creation freezes as its quote.
	writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
		Kind:  docs.MarkSuggestion,
		ID:    "m-insert",
		Attrs: pmdoc.Attrs{"id": "m-insert", "by": "user:alice", "kind": "insert"},
	}, "zarquoninsertedwords ")
	insert := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":       "zarquoninsertbody adds a word",
		"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-insert"},
		"suggestion": map[string]string{"replace_with": "zarquoninsertedwords "},
	}, "alice")
	onInsert := createThreadComment(t, handler, issue.Key, map[string]any{
		"body": "zarquonquoter asks about it", "anchor": map[string]any{"artifact": "spec", "quote": "zarquoninsertedwords"},
	}, "bob")
	// A comment of a project document, which belongs to no issue, and a reply to its first block.
	document := createProjectDocument(t, handler, "TEST", "Correction notes", "# Correction notes\n\nalpha bravo charlie\n")
	documentComment := func(input map[string]any, login string) model.Comment {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+document.ID+"/comments", input, login)
		if response.Code != http.StatusCreated {
			t.Fatalf("comment on the project document: status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeBody[model.Comment](t, response)
	}
	documentRoot := documentComment(map[string]any{"body": "zarquondocumentroot opens the thread\n\nIts second paragraph."}, "alice")
	documentReply := documentComment(map[string]any{
		"body": "zarquondocumentreply answers its first block", "reply_to": documentRoot.ID, "reply_to_block": "b1",
	}, "bob")

	// A post this test makes. words are phrases of the post's own text: its body, a suggestion's
	// replacement and an ask's parts. An accepted suggestion's replacement is the document's own
	// text since the accept, and no word of the post; nor is the pending insert's, which the spec
	// holds and another comment quotes as document text, while the plain suggestion's replacement,
	// which no anchor quotes, stands for a suggestion's own words. froze is the phrase of another
	// post the post's creation carries - a reply's preview of the post it answers - "" for a post
	// whose creation carries none. standing is a post no route deletes.
	type madePost struct {
		name, id, author string
		kind             refs.ItemKind
		words            []string
		froze            string
		standing         bool
	}
	// The posts in the order they are deleted, every reply ahead of the post it answers, and the
	// insert ahead of the comment anchored on its words.
	posts := []madePost{
		{name: "the message's plain reply", id: messageReply.ID, author: "bob", kind: refs.ItemMessage,
			words: []string{"zarquonmessagereply"}, froze: "zarquonmessageroot"},
		{name: "the message's block reply", id: messageBlockReply.ID, author: "bob", kind: refs.ItemMessage,
			words: []string{"zarquonmessageblockreply"}, froze: "zarquonmessageroot"},
		{name: "the comment's plain reply", id: commentReply.ID, author: "bob", kind: refs.ItemComment,
			words: []string{"zarquoncommentreply"}},
		{name: "the comment's block reply", id: commentBlockReply.ID, author: "bob", kind: refs.ItemComment,
			words: []string{"zarquoncommentblockreply"}, froze: "zarquoncommentroot"},
		{name: "the ask's reply", id: askReply.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquonaskreply"}, froze: "zarquonaskoption"},
		{name: "the message", id: message.ID, author: "alice", kind: refs.ItemMessage,
			words: []string{"zarquonmessageroot"}},
		{name: "the comment", id: comment.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquoncommentroot"}},
		{name: "the anchored comment", id: anchored.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquonanchored"}},
		{name: "the suggestion", id: suggestion.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquonsuggestion", "zarquonsuggestedwords"}},
		{name: "the accepted suggestion", id: accepted.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquonacceptedbody"}},
		{name: "the pending insert", id: insert.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquoninsertbody"}},
		{name: "the comment on the inserted words", id: onInsert.ID, author: "bob", kind: refs.ItemComment,
			words: []string{"zarquonquoter"}},
		{name: "the document comment's block reply", id: documentReply.ID, author: "bob", kind: refs.ItemComment,
			words: []string{"zarquondocumentreply"}, froze: "zarquondocumentroot"},
		{name: "the document comment", id: documentRoot.ID, author: "alice", kind: refs.ItemComment,
			words: []string{"zarquondocumentroot"}},
		{name: "the ask", id: ask.ID, kind: refs.ItemAsk,
			words: []string{"zarquonaskquestion", "zarquonaskoption"}, standing: true},
	}
	names := map[string]string{}
	// deletedHere is the posts a delete is made of: the kinds read below are of events about them.
	deletedHere := map[string]bool{}
	for _, post := range posts {
		names[post.id] = post.name
		deletedHere[post.id] = !post.standing
		if _, deletable := routes[post.kind]; deletable == post.standing {
			t.Fatalf("a route deletes a %s: %t; %s is made here as standing: %t", post.kind, deletable, post.name, post.standing)
		}
	}
	for kind, path := range routes {
		if !slices.ContainsFunc(posts, func(post madePost) bool { return post.kind == kind }) {
			t.Fatalf("the route table deletes a %s (DELETE %s), and this test makes none: make one", kind, path)
		}
	}
	for _, post := range posts {
		if post.froze == "" {
			continue
		}
		froze := false
		for _, event := range storedCommentEvents(t, database, post.id, 0) {
			froze = froze || strings.Contains(event.raw, post.froze)
		}
		if !froze {
			t.Fatalf("no stored event of %s carries %q before the delete, so no check below could see a copy of it", post.name, post.froze)
		}
	}

	// Each delete, and the kinds of the events it appended about the posts deleted here - its own,
	// and the anchor refresh of the comment on the inserted words, which the insert's delete
	// appends as its reject's removal of the words orphans that comment - which are the kinds a
	// correction writes, as its writer writes them.
	kinds := map[string]bool{}
	refreshedByInsert := false
	for _, post := range posts {
		if post.standing {
			continue
		}
		before := lastEventID(t, database)
		path := strings.Replace(routes[post.kind], "{id}", post.id, 1)
		if response := dispatchRequest(t, handler, http.MethodDelete, path, nil, post.author); response.Code != http.StatusOK {
			t.Fatalf("delete %s through DELETE %s: status=%d body=%s", post.name, routes[post.kind], response.Code, response.Body.String())
		}
		appended, err := database.Pool.Query(context.Background(), `
			select type, coalesce(payload->>'id', '') from events where id > $1 order by id
		`, before)
		if err != nil {
			t.Fatalf("read the events the delete of %s appended: %v", post.name, err)
		}
		own := 0
		for appended.Next() {
			var eventType, about string
			if err := appended.Scan(&eventType, &about); err != nil {
				appended.Close()
				t.Fatalf("scan an event the delete of %s appended: %v", post.name, err)
			}
			if !deletedHere[about] {
				continue
			}
			kinds[eventType] = true
			if about == post.id {
				own++
			}
			refreshedByInsert = refreshedByInsert ||
				(post.id == insert.ID && about == onInsert.ID && eventType == "comment.anchor_refreshed")
		}
		appended.Close()
		if err := appended.Err(); err != nil {
			t.Fatalf("read the events the delete of %s appended: %v", post.name, err)
		}
		if own == 0 {
			t.Fatalf("the delete of %s appended no event about it, so no event of it is read below", post.name)
		}
	}
	if !refreshedByInsert {
		t.Fatalf("the delete of the pending insert appended no comment.anchor_refreshed of the comment on its words, so no check below reads that correction event")
	}

	// carriesAnother fails t when an event about the post about carries words of any other post.
	carriesAnother := func(where, eventType, about, encoded string) {
		t.Helper()
		for _, other := range posts {
			if other.id == about {
				continue
			}
			for _, phrase := range other.words {
				if strings.Contains(encoded, phrase) {
					t.Errorf("%s %s of %s carries %q, words of %s: %s", where, eventType, names[about], phrase, other.name, encoded)
				}
			}
		}
	}
	rows, err := database.Pool.Query(context.Background(), `
		select type, coalesce(payload->>'id', ''), payload::text, artifact_id is not null from events
		where (issue_key = $1 or artifact_id = $2::uuid) and type = any($3::text[]) order by id
	`, issue.Key, document.ID, slices.Sorted(maps.Keys(kinds)))
	if err != nil {
		t.Fatalf("read the stored correction events: %v", err)
	}
	stored, storedOnDocument := 0, 0
	for rows.Next() {
		var eventType, about, raw string
		var onDocument bool
		if err := rows.Scan(&eventType, &about, &raw, &onDocument); err != nil {
			rows.Close()
			t.Fatalf("scan a stored correction event: %v", err)
		}
		stored++
		if onDocument {
			storedOnDocument++
		}
		carriesAnother("the stored", eventType, about, raw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored correction events: %v", err)
	}
	if storedOnDocument == 0 {
		t.Errorf("the events table stores no correction event of the project document, so its comments' corrections go unread")
	}

	events := decodedIssueEvents(t, handler, issue.Key)
	if len(events) >= maxEventPage {
		t.Fatalf("the issue's events fill a whole page (%d), so the read may leave correction events out", len(events))
	}
	// A project document's events are its own, which GET /api/v1/artifacts/{id}/events reads.
	documentLog := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+document.ID+"/events", nil, "alice")
	if documentLog.Code != http.StatusOK {
		t.Fatalf("read the project document's events: status=%d body=%s", documentLog.Code, documentLog.Body.String())
	}
	documentEvents := decodeBody[[]struct {
		Type    string
		Payload map[string]any
	}](t, documentLog)
	if len(documentEvents) >= maxEventPage {
		t.Fatalf("the project document's events fill a whole page (%d), so the read may leave correction events out", len(documentEvents))
	}
	read := 0
	for _, event := range append(events, documentEvents...) {
		if !kinds[event.Type] {
			continue
		}
		read++
		encoded, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("encode the event log's %s: %v", event.Type, err)
		}
		about, _ := event.Payload["id"].(string)
		carriesAnother("the event log's", event.Type, about, string(encoded))
	}
	if read != stored || stored == 0 {
		t.Errorf("the event logs read %d events of the correction kinds %v, and the events table stores %d: want the same events, at least one", read, slices.Sorted(maps.Keys(kinds)), stored)
	}
}
