// as_of_undated_writes_test.go holds the comment and ask routes at a cut to the two writes that
// change a field with no event that dates the change: the orphaning of a comment's anchor, which
// an accept or a reject that found the mark gone wrote with no event before its 409 appended one,
// and the restore of a decided ask's wording, which an agent's answers and a search give too. A
// field such a write changed reads at a cut as unknown, named absent, wherever the records cannot
// say whether the write came before the cut.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// memberOf is the member of doc, a decoded answer, at the object keys and array indexes path
// names, and whether doc holds it.
func memberOf(doc any, path ...string) (any, bool) {
	value := doc
	for _, step := range path {
		switch held := value.(type) {
		case map[string]any:
			next, ok := held[step]
			if !ok {
				return nil, false
			}
			value = next
		case []any:
			index := 0
			for _, digit := range step {
				index = index*10 + int(digit-'0')
			}
			if index >= len(held) {
				return nil, false
			}
			value = held[index]
		default:
			return nil, false
		}
	}
	return value, true
}

// commentAnchorEvents are the comment.anchor_refreshed events of the comment id after the event
// afterID, each with its anchor's orphaned flag.
func commentAnchorEvents(t *testing.T, database *store.Store, id string, afterID int64) []bool {
	t.Helper()
	var orphaned []bool
	for _, event := range storedCommentEvents(t, database, id, afterID) {
		if event.eventType != "comment.anchor_refreshed" {
			continue
		}
		anchor, _ := event.payload["anchor"].(map[string]any)
		flag, _ := anchor["orphaned"].(bool)
		orphaned = append(orphaned, flag)
	}
	return orphaned
}

// Only a comment.anchor_refreshed dates the orphaning of a comment's anchor. The accept and the
// reject of a suggestion whose mark is gone answer 409 ANCHOR_ORPHANED and commit the orphaning
// with such an event, so a read at a cut before the 409 gives the anchor as not orphaned and one
// after it as orphaned, as each read then gave it; so does an anchor refresh that a document
// write runs. An orphaning such a 409 wrote with no event before it appended one (the row written
// here as that code wrote it) has no time in the record: a read at a cut after the comment's last
// event leaves the flag out and names it absent, and still does once the comment's delete, which
// carries the flag however it came to be set, is written after the cut.
func TestAnAnchorOrphanedWithNoEventThatDatesItIsNamedAbsentAtTheCut(t *testing.T) {
	documentService, handler, database, served := browserDocumentServer(t)
	world := &asOfThreadWorld{handler: handler, served: served}
	takeCut := func() asof.Cut {
		t.Helper()
		world.settle(t)
		return databaseCut(t, database)
	}
	orphanedAt := func(t *testing.T, target string, cut asof.Cut, path ...string) (bool, []string) {
		t.Helper()
		doc, absent := world.readAt(t, target, cut)
		flag, ok := memberOf(doc, path...)
		if !ok {
			t.Fatalf("GET %s at the cut holds no %s: %v", target, strings.Join(path, "/"), doc)
		}
		return flag.(bool), absent
	}

	t.Run("an accept that finds the mark gone dates the orphaning with its event", func(t *testing.T) {
		const words = "zarquonaccept"
		issue := createInteractionIssue(t, handler, "AOAA", "Accepted at a 409", "The "+words+" brown fox")
		writeBrowserMark(t, database, documentService, issue.PrimaryArtifactID, docs.MarkSpec{
			Kind:  docs.MarkSuggestion,
			ID:    "m-accept",
			Attrs: pmdoc.Attrs{"id": "m-accept", "by": "user:alice", "kind": "insert"},
		}, words+" ")
		suggestion := createThreadComment(t, handler, issue.Key, map[string]any{
			"body":       "Add this word.",
			"anchor":     map[string]any{"artifact": "spec", "mark_id": "m-accept"},
			"suggestion": map[string]string{"replace_with": words + " "},
		}, "alice")
		// A browser edit takes the words out of the live document, and no refresh runs before
		// the accept.
		writeBrowserDocument(t, documentService, issue.PrimaryArtifactID, "The brown fox\n")
		before := takeCut()
		beforeAccept := lastEventID(t, database)
		accepted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+suggestion.ID+"/accept", map[string]any{}, "alice")
		if accepted.Code != http.StatusConflict || !strings.Contains(accepted.Body.String(), `"code":"ANCHOR_ORPHANED"`) {
			t.Fatalf("accept a suggestion whose mark is gone: status=%d body=%s, want 409 ANCHOR_ORPHANED", accepted.Code, accepted.Body.String())
		}
		if refreshed := commentAnchorEvents(t, database, suggestion.ID, beforeAccept); !slices.Equal(refreshed, []bool{true}) {
			t.Fatalf("the 409 committed the comment.anchor_refreshed events with orphaned %v, want one, orphaned", refreshed)
		}
		after := takeCut()
		target := "/api/v1/comments/" + suggestion.ID
		if flag, absent := orphanedAt(t, target, before, "comment", "anchor", "orphaned"); flag || namedAbsent(absent, "/comment/anchor/orphaned") {
			t.Errorf("at a cut before the 409 the anchor reads orphaned %v, with %v absent; want not orphaned, and given", flag, absent)
		}
		if flag, absent := orphanedAt(t, target, after, "comment", "anchor", "orphaned"); !flag || namedAbsent(absent, "/comment/anchor/orphaned") {
			t.Errorf("at a cut after the 409 the anchor reads orphaned %v, with %v absent; want orphaned, and given", flag, absent)
		}
	})

	t.Run("an anchor refresh dates the orphaning with its event", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "AOAB", "Orphaned by a refresh", "The quick brown fox")
		anchored := createThreadComment(t, handler, issue.Key, map[string]any{
			"body": "Look here.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
		}, "alice")
		before := takeCut()
		beforeWrite := lastEventID(t, database)
		if _, err := replaceDocumentText(database, documentService, issue.PrimaryArtifactID, "The quick fox", model.Actor{Kind: "user", ID: "alice"}); err != nil {
			t.Fatalf("take the anchored words out of the document: %v", err)
		}
		if refreshed := commentAnchorEvents(t, database, anchored.ID, beforeWrite); !slices.Equal(refreshed, []bool{true}) {
			t.Fatalf("the document write committed the comment.anchor_refreshed events with orphaned %v, want one, orphaned", refreshed)
		}
		after := takeCut()
		target := "/api/v1/issues/" + issue.Key + "/comments"
		if flag, absent := orphanedAt(t, target, before, "0", "anchor", "orphaned"); flag || namedAbsent(absent, "/0/anchor/orphaned") {
			t.Errorf("at a cut before the refresh the anchor reads orphaned %v, with %v absent; want not orphaned, and given", flag, absent)
		}
		if flag, absent := orphanedAt(t, target, after, "0", "anchor", "orphaned"); !flag || namedAbsent(absent, "/0/anchor/orphaned") {
			t.Errorf("at a cut after the refresh the anchor reads orphaned %v, with %v absent; want orphaned, and given", flag, absent)
		}
	})

	t.Run("an orphaning with no event, then a delete, is named absent", func(t *testing.T) {
		issue := createInteractionIssue(t, handler, "AOAC", "Orphaned by an old 409", "The quick brown fox")
		anchored := createThreadComment(t, handler, issue.Key, map[string]any{
			"body": "Look here.", "anchor": map[string]any{"artifact": "spec", "quote": "brown"},
		}, "alice")
		// What an accept or a reject that found the mark gone wrote before its 409 appended a
		// comment.anchor_refreshed: the flag alone, with no event.
		if _, err := database.Pool.Exec(context.Background(), `
			update comments set anchor = jsonb_set(anchor, '{orphaned}', to_jsonb(true)) where id = $1
		`, anchored.ID); err != nil {
			t.Fatalf("orphan the anchor as an old 409 did: %v", err)
		}
		cut := takeCut()
		requireLeftOut := func(t *testing.T, when string) {
			t.Helper()
			for _, read := range []struct{ target, member string }{
				{"/api/v1/comments/" + anchored.ID, "/comment/anchor/orphaned"},
				{"/api/v1/issues/" + issue.Key + "/comments", "/0/anchor/orphaned"},
			} {
				doc, absent := world.readAt(t, read.target, cut)
				path := strings.Split(strings.TrimPrefix(read.member, "/"), "/")
				if !namedAbsent(absent, read.member) {
					t.Errorf("%s: GET %s at the cut names %v absent, want %s among them", when, read.target, absent, read.member)
				}
				if _, held := memberOf(doc, path...); held {
					t.Errorf("%s: GET %s at the cut gives %s, which no record dates", when, read.target, read.member)
				}
				if _, held := memberOf(doc, path[:len(path)-1]...); !held {
					t.Errorf("%s: GET %s at the cut gives no anchor at %s: %v", when, read.target, read.member, doc)
				}
			}
		}
		requireLeftOut(t, "before the delete")
		deleteThreadComment(t, handler, anchored.ID, "alice")
		world.settle(t)
		requireLeftOut(t, "after the delete")
	})
}

// appendAskEventAsAnEarlierRelease appends to issueKey's log an ask event of kind with payload,
// as events.Broker.QueueAppend does: the issue's next seq, and its last_seq and updated_at moved
// to the event.
func appendAskEventAsAnEarlierRelease(t *testing.T, database *store.Store, issueKey, kind string, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the %s payload: %v", kind, err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		with appended as (
			insert into events (issue_key, seq, type, actor, payload, notify)
			values ($1, (select last_seq from issues where key = $1) + 1, $2,
				'{"kind":"session","id":"session-0123456789abcdef"}', $3::jsonb, false)
			returning seq, created_at
		)
		update issues set last_seq = appended.seq, updated_at = greatest(issues.updated_at, appended.created_at)
		from appended where issues.key = $1
	`, issueKey, kind, encoded); err != nil {
		t.Fatalf("append the %s: %v", kind, err)
	}
}

// readTreesAt is readAt for a request that sends Dispatch-Trees: 1, whose answer carries the
// display tree of each text it gives.
func (w *asOfThreadWorld) readTreesAt(t *testing.T, target string, cut asof.Cut) (any, []string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, withAsOf(t, target, cut), nil)
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(displayTreesHeader, "1")
	response := httptest.NewRecorder()
	w.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s with trees as of the cut: status=%d body=%s", target, response.Code, response.Body.String())
	}
	if named := response.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
		t.Fatalf("GET %s with trees as of the cut: %s is %q, want the cut %s", target, contracts.DispatchAsOfHeader, named, cut)
	}
	return decodeAnswer(t, target, response), absentPointers(response.Header().Get(contracts.DispatchAsOfAbsentHeader))
}

// elementWhere is the index of the first element of the array list whose member key is value,
// and -1 where none is.
func elementWhere(list any, value string, key ...string) int {
	elements, _ := list.([]any)
	for index, element := range elements {
		if member, _ := memberOf(element, key...); member == value {
			return index
		}
	}
	return -1
}

// The restore of a decided ask's wording (docs.Service.RestoreDecidedWordings) writes the
// question, the options, multiple and the urgency back with no event, so the newest event at a
// cut after it holds the wording it undid, and no record dates it. The ask read at such a cut
// leaves the four out, with the ask's revision and its part ids, which the present tables hold for
// the restored wording, and names them absent; with display trees it leaves out the question's
// tree too, which it names only then. So does each other answer that gives that wording: an
// agent's answers leave out the question of the ask that the answer is on, and a search for a
// word only the undone wording holds finds the ask by it and leaves out the hit's snippet, and its
// tree with display trees. At a cut before the rewording the edit's previous wording is the
// wording then, which each read gives. An ask edited after the cut by the edit route, which
// records the wording it replaced, keeps that wording at the cut and names none of the four
// absent.
func TestAnAsksWordingARestoreChangedIsNamedAbsentAtTheCut(t *testing.T) {
	documentService, handler, database, served := browserDocumentServer(t)
	world := &asOfThreadWorld{handler: handler, served: served}
	takeCut := func() asof.Cut {
		t.Helper()
		world.settle(t)
		return databaseCut(t, database)
	}
	wording := []string{"/ask/question", "/ask/options", "/ask/multiple", "/ask/urgency"}

	issue := createInteractionIssue(t, handler, "AOWR", "Restored wording", "A spec")
	ask := createPartAsk(t, handler, issue.Key, "Ship on Monday?",
		map[string]string{"label": "Yes", "description": "Monday."},
		map[string]string{"label": "No", "description": "Later."})
	answered := answerPartAsk(t, handler, ask, "alice", []string{"Yes"}, "")
	// An agent's answers are the ask.answered events whose published_destinations hold its topic,
	// which the outbox records as it publishes the answer to the ask's followers, the asking
	// session among them. This server runs no outbox, so the answer is marked delivered as the
	// outbox marks it.
	if tag, err := database.Pool.Exec(context.Background(), `
		update events set published_destinations = array[$2::text] where type = 'ask.answered' and payload->>'id' = $1
	`, ask.ID, contracts.LegacyAgentSubject(asOfAgentSession)); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("mark the answer delivered to the asking session: %s, %v", tag, err)
	}
	beforeRewording := takeCut()

	// What a task of the release before LEGION-651 left: the document's rewording in the decided
	// row, and an ask.edited that carries the reworded ask and the wording it replaced. Only the
	// reworded wording holds the word "Tuesday".
	reworded := answered
	reworded.Question = "Ship on Tuesday?"
	reworded.Options = []model.AskOption{{Label: "Tuesday", Description: "Ship it."}, {Label: "No", Description: "Later."}}
	options, err := json.Marshal(reworded.Options)
	if err != nil {
		t.Fatalf("encode the rewording's options: %v", err)
	}
	if _, err := database.Pool.Exec(context.Background(), `
		update asks set question = $2, options = $3, edited_at = now() where id = $1::uuid
	`, ask.ID, reworded.Question, options); err != nil {
		t.Fatalf("reword the decided ask's row: %v", err)
	}
	appendAskEventAsAnEarlierRelease(t, database, issue.Key, "ask.edited", model.NewAskEditEventPayload(
		reworded, model.NewAskEditPrevious(answered), model.Actor{Kind: "session", ID: "session-0123456789abcdef"}, model.ReferenceChanges{}))
	if restored, _, err := documentService.RestoreDecidedWordings(context.Background()); err != nil || restored != 1 {
		t.Fatalf("the restoration restored %d asks (%v), want the one ask", restored, err)
	}
	afterRestore := takeCut()

	target := "/api/v1/asks/" + ask.ID
	doc, absent := world.readAt(t, target, afterRestore)
	for _, member := range append(slices.Clone(wording), "/ask/revision", "/ask/options/0/id", "/ask/answer/id") {
		if !namedAbsent(absent, member) {
			t.Errorf("at a cut after the restore the ask read names %v absent, want %s among them", absent, member)
		}
	}
	for _, member := range wording {
		if value, held := memberOf(doc, strings.Split(strings.TrimPrefix(member, "/"), "/")...); held {
			t.Errorf("at a cut after the restore the ask read gives %s = %v, which no record gives", member, value)
		}
	}
	if namedAbsent(absent, "/ask/question_tree") {
		t.Errorf("at a cut after the restore the ask read with no display trees names %v absent, the question's tree among them, which it does not hold", absent)
	}
	if question, _ := memberOf(world.readNow(t, target), "ask", "question"); question != "Ship on Monday?" {
		t.Errorf("the present ask reads the question %v, want the restored one", question)
	}

	doc, absent = world.readAt(t, target, beforeRewording)
	if question, _ := memberOf(doc, "ask", "question"); question != "Ship on Monday?" {
		t.Errorf("at a cut before the rewording the ask reads the question %v, want the decided one", question)
	}
	for _, member := range wording {
		if namedAbsent(absent, member) {
			t.Errorf("at a cut before the rewording the ask read names %s absent, which the edit's previous wording gives", member)
		}
	}

	t.Run("the ask read with display trees leaves out the question's tree", func(t *testing.T) {
		doc, absent := world.readTreesAt(t, target, afterRestore)
		if !namedAbsent(absent, "/ask/question_tree") {
			t.Errorf("at a cut after the restore the ask read with trees names %v absent, want /ask/question_tree among them", absent)
		}
		if tree, held := memberOf(doc, "ask", "question_tree"); held {
			t.Errorf("at a cut after the restore the ask read with trees gives the question's tree %v, which no record gives", tree)
		}
		doc, absent = world.readTreesAt(t, target, beforeRewording)
		if _, held := memberOf(doc, "ask", "question_tree"); !held || namedAbsent(absent, "/ask/question_tree") {
			t.Errorf("at a cut before the rewording the ask read with trees holds the question's tree %t, with %v absent; want it given", held, absent)
		}
	})

	t.Run("an agent's answers leave out the question of the answered ask", func(t *testing.T) {
		answers := "/api/v1/agents/" + asOfAgentSession + "/answers"
		questionAt := func(t *testing.T, cut asof.Cut) (any, bool, string, []string) {
			t.Helper()
			doc, absent := world.readAt(t, answers, cut)
			items, _ := memberOf(doc, "items")
			index := elementWhere(items, ask.ID, "ask", "id")
			if index < 0 {
				t.Fatalf("at the cut %s the asking session's answers hold no item on the ask: %v", cut, doc)
			}
			question, held := memberOf(doc, "items", strconv.Itoa(index), "ask", "question")
			return question, held, "/items/" + strconv.Itoa(index) + "/ask/question", absent
		}
		if question, held, member, absent := questionAt(t, afterRestore); held || !namedAbsent(absent, member) {
			t.Errorf("at a cut after the restore the session's answers give the ask's question %v (%t), with %v absent; want it left out, and %s named",
				question, held, absent, member)
		}
		if question, _, member, absent := questionAt(t, beforeRewording); question != "Ship on Monday?" || namedAbsent(absent, member) {
			t.Errorf("at a cut before the rewording the session's answers give the ask's question %v, with %v absent; want the decided one, given", question, absent)
		}
	})

	t.Run("a search for a word of the undone wording leaves out the ask hit's snippet", func(t *testing.T) {
		fillTextVersionsPast(t, database, afterRestore)
		search := "/api/v1/search/results?" + keywords("tuesday").Encode()
		for _, trees := range []bool{false, true} {
			read := world.readAt
			if trees {
				read = world.readTreesAt
			}
			doc, absent := read(t, search, afterRestore)
			results, _ := memberOf(doc, "results")
			index := elementWhere(results, ask.ID, "id")
			if index < 0 {
				t.Fatalf("the search at a cut after the restore (trees %t) does not find the ask by the wording of its newest event: %v", trees, doc)
			}
			hit := strconv.Itoa(index)
			if snippet, held := memberOf(doc, "results", hit, "snippet"); held || !namedAbsent(absent, "/results/"+hit+"/snippet") {
				t.Errorf("the search at a cut after the restore (trees %t) gives the ask hit's snippet %v (%t), with %v absent; want it left out, and named",
					trees, snippet, held, absent)
			}
			tree, held := memberOf(doc, "results", hit, "snippet_tree")
			switch named := namedAbsent(absent, "/results/"+hit+"/snippet_tree"); {
			case trees && (held || !named):
				t.Errorf("the search with trees at a cut after the restore gives the ask hit's snippet tree %v (%t), with %v absent; want it left out, and named",
					tree, held, absent)
			case !trees && named:
				t.Errorf("the search with no trees at a cut after the restore names %v absent, the snippet tree among them, which it does not hold", absent)
			}
		}
	})

	// An open ask edited after the cut by the edit route.
	open := createPartAsk(t, handler, issue.Key, "Ship on Wednesday?",
		map[string]string{"label": "Yes"}, map[string]string{"label": "No"})
	beforeEdit := takeCut()
	if edited := sessionRequest(t, handler, http.MethodPatch, "/api/v1/asks/"+open.ID, map[string]any{
		"actor": sessionActor(), "question": "Ship on Thursday?", "urgency": "high",
	}); edited.Code != http.StatusOK {
		t.Fatalf("edit the open ask: status=%d body=%s", edited.Code, edited.Body.String())
	}
	world.settle(t)
	doc, absent = world.readAt(t, "/api/v1/asks/"+open.ID, beforeEdit)
	if question, _ := memberOf(doc, "ask", "question"); question != "Ship on Wednesday?" {
		t.Errorf("at a cut before the edit the ask reads the question %v, want the one it had then", question)
	}
	if urgency, _ := memberOf(doc, "ask", "urgency"); urgency != "med" {
		t.Errorf("at a cut before the edit the ask reads the urgency %v, want the one it had then", urgency)
	}
	for _, member := range wording {
		if namedAbsent(absent, member) {
			t.Errorf("at a cut before the edit the ask read names %s absent, which the edit's previous wording gives", member)
		}
	}
	if !namedAbsent(absent, "/ask/revision") {
		t.Errorf("at a cut before the edit the ask read names %v absent, want its revision, which the edit changed", absent)
	}
}
