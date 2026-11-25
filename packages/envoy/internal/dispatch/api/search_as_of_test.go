// search_as_of_test.go holds a search at a past time (decisions 7, 8 and 14 and departures D1 and
// D2 of INFRA-4620's plan; the check t-search): a search with as_of reads the text each item had
// at the cut, from the keyword table that textversions fills from the events, and never later
// text. Each test writes through the routes, takes the cut from the database's clock, writes
// again after it, runs the fill that reads those events (textversions.FillToHorizon), and reads
// the search with as_of at the cut.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/textversions"
)

// searchAtCutQuery is a query no text of these tests holds a word of; the stand-in embedder maps
// it to queryVector(), so only the meaning arms can find anything for it.
const searchAtCutQuery = "wibbleflorp"

// fillTextVersions runs the keyword table's fill once, up to the event horizon
// (textversions.FillToHorizon): every event committed before the call has its rows after it.
func fillTextVersions(t *testing.T, database *store.Store) {
	t.Helper()
	if err := textversions.FillToHorizon(t.Context(), database, events.NewHorizon(database.Pool)); err != nil {
		t.Fatalf("fill the keyword table: %v", err)
	}
}

// fillTextVersionsPast runs the keyword table's fill until the reach it stores or computes is at
// or past cut, so a search at cut reads the table and is not refused (TextVersionsAt). A reach
// stays a second behind the clock, and behind every transaction still open, so the loop waits
// for it.
func fillTextVersionsPast(t *testing.T, database *store.Store, cut asof.Cut) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		fillTextVersions(t, database)
		reach, reached, err := database.TextVersionsReach(t.Context())
		if err != nil {
			t.Fatalf("read the keyword table's reach: %v", err)
		}
		if reached && !reach.Before(cut.Time()) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the keyword table's fill reached %s (reached: %t) 30 s on, before the cut %s", reach, reached, cut)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// searchAtCutWorld is a server with a stand-in embedder (fakeEmbedder) and one issue of its own
// project, which every post of a test is written on.
type searchAtCutWorld struct {
	handler  http.Handler
	database *store.Store
	served   *server
	embedder *fakeEmbedder
	issue    string
}

// newSearchAtCutWorld starts the server and writes project with an issue of title and spec.
func newSearchAtCutWorld(t *testing.T, project, title, spec string) *searchAtCutWorld {
	t.Helper()
	embedder := &fakeEmbedder{vectors: map[string][]float32{searchAtCutQuery: queryVector()}}
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, project, title, spec)
	return &searchAtCutWorld{handler: handler, database: database, served: served, embedder: embedder, issue: issue.Key}
}

// settle finishes the server's detached work and waits for the event horizon.
func (w *searchAtCutWorld) settle(t *testing.T) {
	t.Helper()
	awaitBackgroundWrites(t, w.served)
	safeHorizon(t, w.served)
}

// cut settles the writes so far and takes the database's clock as the cut.
func (w *searchAtCutWorld) cut(t *testing.T) asof.Cut {
	t.Helper()
	w.settle(t)
	return databaseCut(t, w.database)
}

// fill settles the writes so far and fills the keyword table past cut.
func (w *searchAtCutWorld) fill(t *testing.T, cut asof.Cut) {
	t.Helper()
	w.settle(t)
	fillTextVersionsPast(t, w.database, cut)
}

// comment writes a comment of body on the issue as alice and answers its id.
func (w *searchAtCutWorld) comment(t *testing.T, body string) string {
	t.Helper()
	return decodeBody[model.Comment](t, asOfWrite(t, w.handler, http.MethodPost, "/api/v1/issues/"+w.issue+"/comments",
		map[string]any{"body": body}, "alice")).ID
}

// message writes an issue message of body on the issue as alice and answers its id.
func (w *searchAtCutWorld) message(t *testing.T, body string) string {
	t.Helper()
	return decodeBody[model.Message](t, asOfWrite(t, w.handler, http.MethodPost, "/api/v1/issues/"+w.issue+"/messages",
		map[string]any{"body": body}, "alice")).ID
}

// edit writes body as the new body of the post at path (/api/v1/comments/{id} or
// /api/v1/messages/{id}), as alice, its author.
func (w *searchAtCutWorld) edit(t *testing.T, path, body string) {
	t.Helper()
	asOfWrite(t, w.handler, http.MethodPatch, path, map[string]any{"body": body}, "alice")
}

// embedPost maps every window of the comment or message id of kind ("comment" or "message") to
// vector, so the stand-in embedder answers it once the queue drains.
func (w *searchAtCutWorld) embedPost(t *testing.T, kind, id string, vector []float32) {
	t.Helper()
	embedWindowsAs(t, w.database, w.embedder, kind+"_block", kind+"/"+id, vector)
}

// searchAnswer is a search as the tests read it: the decoded answer, its JSON as an object, and
// the pointers Dispatch-As-Of-Absent names.
type searchAnswer struct {
	model.SearchResponse
	raw    map[string]any
	absent []string
}

// hit is the result of kind and id, or nil.
func (a searchAnswer) hit(kind, id string) *model.SearchResult {
	for index := range a.Results {
		if a.Results[index].Kind == kind && a.Results[index].ID == id {
			return &a.Results[index]
		}
	}
	return nil
}

// searchAt reads GET /api/v1/search/results with query at cut as alice, and fails the test unless
// it answers 200 and names the cut in Dispatch-As-Of.
func (w *searchAtCutWorld) searchAt(t *testing.T, query url.Values, cut asof.Cut) searchAnswer {
	t.Helper()
	target := withAsOf(t, "/api/v1/search/results?"+query.Encode(), cut)
	response := dispatchRequest(t, w.handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	if named := response.Header().Get(contracts.DispatchAsOfHeader); named != cut.String() {
		t.Fatalf("GET %s: %s is %q, want the cut %s", target, contracts.DispatchAsOfHeader, named, cut)
	}
	answer := searchAnswer{SearchResponse: decodeBody[model.SearchResponse](t, response),
		absent: absentPointers(response.Header().Get(contracts.DispatchAsOfAbsentHeader))}
	if err := json.Unmarshal(response.Body.Bytes(), &answer.raw); err != nil {
		t.Fatalf("GET %s: decode the answer: %v", target, err)
	}
	return answer
}

// searchNow reads GET /api/v1/search/results with query at the present as alice.
func (w *searchAtCutWorld) searchNow(t *testing.T, query url.Values) model.SearchResponse {
	t.Helper()
	return searchResponseOn(t, w.handler, "/api/v1/search/results", query.Encode())
}

// keywords is a keyword-only search of q.
func keywords(q string) url.Values { return url.Values{"q": {q}, "keywords_only": {"true"}} }

// meaning is a search of q with its meaning arms.
func meaning(q string) url.Values { return url.Values{"q": {q}} }

// requireNoWord fails t when any result or snippet of answer holds word: no result may be an
// item whose text in the answer, its owner's title or name, holds it, and no snippet may show it.
func requireNoWord(t *testing.T, answer searchAnswer, word, why string) {
	t.Helper()
	for _, result := range answer.Results {
		texts := []string{result.Snippet, result.Owner.Title, result.Owner.Name}
		if result.Issue != nil {
			texts = append(texts, result.Issue.Title)
		}
		if result.Artifact != nil {
			texts = append(texts, result.Artifact.Name)
		}
		for _, text := range texts {
			if strings.Contains(strings.ToLower(text), word) {
				t.Errorf("%s: the %s %s holds %q in %q", why, result.Kind, result.ID, word, text)
			}
		}
	}
}

// TestAPastTimeKeywordSearchFindsTheTextThen: a keyword search at a cut finds each item by the
// text it had at the cut and by no later text. An issue retitled after the cut is found by its
// old title and not its new one; a comment edited after the cut by its old words and not its new
// ones; a comment written after the cut, and an answer given after the cut, not at all; a
// document by the version it had at the cut, not by a version made after it; and a comment the
// cut left alone is found as it stood. Each snippet holds only text at the cut.
//
// A search that narrows by status, author or updated_since, or whose query has no word to rank
// by, reads each post kind's matches whole (ranksFirstAtACut, searchAsOfPosts), and each of its
// filters reads the state at the cut: the status the issue had at the cut (text_issues reads the
// issues shadow), the edited_at a comment had at the cut (searchAsOfEditedAt reads the comments
// shadow), the post's author, and, for a query of stop words and an excluded word, the post's
// text at the cut. The issue's status and the comment's edit change after the cut. So the present
// answers the status and updated_since rows otherwise, and so would a search at the cut that read
// the present status or edited_at.
func TestAPastTimeKeywordSearchFindsTheTextThen(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASKW", "Chart the lagoon soundings", "The survey's first notes.")
	kept := w.comment(t, "The beacon tower stands on the point.")
	changed := w.comment(t, "The ferry leaves at dawn.")
	ask := createSearchAsk(t, w.handler, w.issue, "Which sextant do we keep?", "Brass", "Steel")
	notes := asOfUpload(t, w.handler, "/api/v1/issues/"+w.issue, "notes.md", "# Notes\n\nThe almanac lists the tides.\n")
	// The tugboat comment is edited once before the cut and once after it.
	tugboat := w.comment(t, "The tugboat idles.")
	time.Sleep(5 * time.Millisecond)
	w.edit(t, "/api/v1/comments/"+tugboat, "The tugboat idles by the pier.")
	var tugboatWritten, tugboatEdited time.Time
	if err := w.database.Pool.QueryRow(t.Context(), `select created_at, edited_at from comments where id = $1`, tugboat).
		Scan(&tugboatWritten, &tugboatEdited); err != nil {
		t.Fatalf("read the tugboat comment's times: %v", err)
	}
	cut := w.cut(t)
	time.Sleep(5 * time.Millisecond)

	asOfWrite(t, w.handler, http.MethodPatch, "/api/v1/issues/"+w.issue, map[string]any{"title": "Chart the estuary soundings"}, "alice")
	w.edit(t, "/api/v1/comments/"+changed, "The hovercraft leaves at dusk.")
	added := w.comment(t, "The zeppelin arrives on Tuesday.")
	answerSearchAsk(t, w.handler, ask, map[string]any{"text": "Keep the quadrant instead."})
	asOfUpload(t, w.handler, "/api/v1/issues/"+w.issue, "notes.md", "# Notes\n\nThe almanac lists the tides and the gazetteer.\n")
	asOfWrite(t, w.handler, http.MethodPatch, "/api/v1/issues/"+w.issue, map[string]any{"status": "backlog"}, "alice")
	w.edit(t, "/api/v1/comments/"+tugboat, "The tugboat idles by the pier at noon.")
	w.fill(t, cut)

	// The present finds the text written after the cut, so its absence at the cut is the cut's.
	present := map[string][2]string{
		"estuary": {"issue", w.issue}, "hovercraft": {"comment", changed}, "zeppelin": {"comment", added},
		"quadrant": {"ask", ask}, "gazetteer": {"document", notes.ID},
	}
	for word, item := range present {
		now := searchAnswer{SearchResponse: w.searchNow(t, keywords(word))}
		if now.hit(item[0], item[1]) == nil {
			t.Fatalf("the present search for %q misses the %s %s that holds it now", word, item[0], item[1])
		}
		at := w.searchAt(t, keywords(word), cut)
		if len(at.Results) != 0 || at.Total != 0 {
			t.Errorf("the search at the cut for %q, which only text written after the cut holds, answers %+v", word, at.Results)
		}
	}

	found := []struct {
		word, kind, id string
		later          []string
	}{
		{"lagoon", "issue", w.issue, []string{"estuary"}},
		{"ferry", "comment", changed, []string{"hovercraft", "dusk"}},
		{"beacon", "comment", kept, nil},
		{"sextant", "ask", ask, []string{"quadrant"}},
		{"almanac", "document", notes.ID, []string{"gazetteer"}},
	}
	for _, row := range found {
		at := w.searchAt(t, keywords(row.word), cut)
		hit := at.hit(row.kind, row.id)
		if hit == nil {
			t.Errorf("the search at the cut for %q misses the %s %s, whose text at the cut holds it: %+v", row.word, row.kind, row.id, at.Results)
			continue
		}
		if !strings.Contains(hit.Snippet, "<mark>") {
			t.Errorf("the %s %s found for %q at the cut has the snippet %q, which marks no word", row.kind, row.id, row.word, hit.Snippet)
		}
		for _, later := range row.later {
			requireNoWord(t, at, later, "the search at the cut for "+row.word)
		}
	}
	if gone := w.searchAt(t, keywords("ferry"), cut); w.searchNow(t, keywords("ferry")).Total != 0 || gone.Total == 0 {
		t.Errorf("the comment's old word answers %d hits at the cut; the present must answer none and the cut one", gone.Total)
	}

	// Each row is a search that reads the matches whole, and whether it finds the post at the cut
	// and at the present.
	filtered := func(q, filter, value string) url.Values {
		query := keywords(q)
		query.Set(filter, value)
		return query
	}
	betweenEdits := tugboatWritten.Add(tugboatEdited.Sub(tugboatWritten) / 2).Format(time.RFC3339Nano)
	wholeReads := []struct {
		name       string
		query      url.Values
		kind, id   string
		atCut, now bool
	}{
		{"status: the issue's status at the cut", filtered("beacon", "status", "triage"), "comment", kept, true, false},
		{"status: the issue's status after the cut", filtered("beacon", "status", "backlog"), "comment", kept, false, true},
		{"updated_since between the comment's writing and its edit before the cut", filtered("tugboat", "updated_since", betweenEdits), "comment", tugboat, true, true},
		{"updated_since at the cut, between the comment's edit before it and its edit after it", filtered("tugboat", "updated_since", cut.Time().Format(time.RFC3339Nano)), "comment", tugboat, false, true},
		{"author: the post's author", filtered("beacon", "author", "alice"), "comment", kept, true, true},
		{"author: another author", filtered("beacon", "author", "bob"), "comment", kept, false, false},
		{"stop words and an excluded word that the post's text at the cut does not hold", keywords("the -ferry"), "comment", kept, true, true},
		{"stop words and an excluded word that the post's text at the cut holds", keywords("the -ferry"), "comment", changed, false, true},
	}
	for _, row := range wholeReads {
		if found := w.searchAt(t, row.query, cut).hit(row.kind, row.id) != nil; found != row.atCut {
			t.Errorf("%s: the search at the cut for %s finds the %s %s: %t, want %t", row.name, row.query.Encode(), row.kind, row.id, found, row.atCut)
		}
		now := searchAnswer{SearchResponse: w.searchNow(t, row.query)}
		if found := now.hit(row.kind, row.id) != nil; found != row.now {
			t.Errorf("%s: the present search for %s finds the %s %s: %t, want %t", row.name, row.query.Encode(), row.kind, row.id, found, row.now)
		}
	}
}

// TestAPastTimeMeaningSearchSkipsAnItemChangedSinceTheCut (D1): at a cut, the meaning arms keep a
// window only when its item did not change after the cut and the window was embedded at or
// before the cut. A comment edited after the cut, whose window from before the edit is still
// stored, is found by the keyword arm alone, ranking as a keyword-only search ranks it; a comment
// whose window was embedded after the cut is not found by meaning; a comment the cut left alone is
// found by meaning. The present search finds all three by meaning, so the cut is what leaves them
// out.
func TestAPastTimeMeaningSearchSkipsAnItemChangedSinceTheCut(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASME", "Rig the schooner", "The rigging plan.")
	w.embedder.vectors["rope"] = queryVector()
	unchanged := w.comment(t, "The keel needs fresh varnish.")
	changed := w.comment(t, "The mast needs new rope.")
	w.embedPost(t, "comment", unchanged, angledVector(0.9, 2))
	w.embedPost(t, "comment", changed, angledVector(0.85, 3))
	processAllPending(t, w.database, w.embedder)
	late := w.comment(t, "The hull needs caulking.")
	w.embedPost(t, "comment", late, angledVector(0.8, 4)) // windowed, not embedded, at the cut
	cut := w.cut(t)
	time.Sleep(5 * time.Millisecond)

	// The edit leaves the comment's windows as they were, embedded before the cut, until the
	// block-index tick windows its new text; the embedder takes the late comment's window now.
	w.edit(t, "/api/v1/comments/"+changed, "The mast needs new rope and tar.")
	processAllPending(t, w.database, w.embedder)
	w.fill(t, cut)

	now := searchAnswer{SearchResponse: w.searchNow(t, meaning(searchAtCutQuery))}
	for _, id := range []string{unchanged, changed, late} {
		if now.hit("comment", id) == nil {
			t.Fatalf("the present meaning search misses the comment %s: %+v", id, now.Results)
		}
	}
	at := w.searchAt(t, meaning(searchAtCutQuery), cut)
	if at.Degraded != "" {
		t.Fatalf("the search at the cut is degraded (%s): its meaning arms did not run", at.Degraded)
	}
	if at.hit("comment", unchanged) == nil {
		t.Errorf("the meaning search at the cut misses the comment the cut left alone: %+v", at.Results)
	}
	if at.hit("comment", changed) != nil {
		t.Errorf("the meaning search at the cut finds the comment edited after the cut by its window")
	}
	if at.hit("comment", late) != nil {
		t.Errorf("the meaning search at the cut finds the comment whose window was embedded after the cut")
	}

	// "rope" matches the edited comment's words at the cut, and its window by meaning at the
	// present: at the cut it ranks as the keyword arm alone ranks it.
	nowRope := searchAnswer{SearchResponse: w.searchNow(t, meaning("rope"))}
	nowKeywords := searchAnswer{SearchResponse: w.searchNow(t, keywords("rope"))}
	if nowRope.hit("comment", changed) == nil || nowKeywords.hit("comment", changed) == nil ||
		nowRope.hit("comment", changed).Rank <= nowKeywords.hit("comment", changed).Rank {
		t.Fatalf("at the present the edited comment's window does not add to its rank for \"rope\": %+v, %+v", nowRope.Results, nowKeywords.Results)
	}
	atRope := w.searchAt(t, meaning("rope"), cut)
	atKeywords := w.searchAt(t, keywords("rope"), cut)
	byMeaning, byKeyword := atRope.hit("comment", changed), atKeywords.hit("comment", changed)
	switch {
	case byMeaning == nil || byKeyword == nil:
		t.Errorf("the search at the cut for \"rope\" misses the comment whose words held it then: %+v, %+v", atRope.Results, atKeywords.Results)
	case byMeaning.Rank != byKeyword.Rank:
		t.Errorf("the edited comment ranks %v at the cut with its meaning arms and %v with its keywords alone: its window counted", byMeaning.Rank, byKeyword.Rank)
	}
	if atRope.hit("comment", unchanged) == nil {
		t.Errorf("the search at the cut for \"rope\" misses the comment the cut left alone, which its window finds")
	}
	if atRope.hit("comment", late) != nil {
		t.Errorf("the search at the cut for \"rope\" finds the comment whose window was embedded after the cut")
	}
}

// TestASearchAtACutTheFillHasNotReachedIsRefused (decision 7): a search at a cut the keyword
// table's fill has not reached answers 409 AS_OF_SEARCH_NOT_FILLED, which names the time the fill
// reached in its message and in Dispatch-As-Of-Filled-Through, and no result: on a database whose
// fill has reached no time, it names none; on one whose fill stands before a comment written
// after it, the reach is before that comment, and a cut the fill has reached is answered.
func TestASearchAtACutTheFillHasNotReachedIsRefused(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASNF", "Fill the keyword table", "The fill's notes.")
	w.comment(t, "The lighthouse log, before the fill.")
	before := w.cut(t)
	for _, path := range []string{"/api/v1/search", "/api/v1/search/results"} {
		refusal := requireNotFilled(t, w.handler, withAsOf(t, path+"?"+keywords("lighthouse").Encode(), before))
		if through := refusal.Header().Get(contracts.DispatchAsOfFilledThroughHeader); through != "" {
			t.Errorf("%s with no fill names %s %q, want none: the fill has reached no time", path, contracts.DispatchAsOfFilledThroughHeader, through)
		}
		if !strings.Contains(refusal.Body.String(), "no time") {
			t.Errorf("%s with no fill says %s, want that the fill has reached no time", path, refusal.Body.String())
		}
	}

	fillTextVersionsPast(t, w.database, before)
	unfilled := w.comment(t, "The lighthouse log, after the fill.")
	w.settle(t)
	var written time.Time
	if err := w.database.Pool.QueryRow(t.Context(), `
		select created_at from events where type = 'comment.created' and payload->>'id' = $1`, unfilled).Scan(&written); err != nil {
		t.Fatalf("read the comment's event: %v", err)
	}
	cut := databaseCut(t, w.database)
	for _, path := range []string{"/api/v1/search", "/api/v1/search/results"} {
		refusal := requireNotFilled(t, w.handler, withAsOf(t, path+"?"+keywords("lighthouse").Encode(), cut))
		through := refusal.Header().Get(contracts.DispatchAsOfFilledThroughHeader)
		reach, err := time.Parse(time.RFC3339Nano, through)
		if err != nil || !reach.Before(written) || !reach.Before(cut.Time()) {
			t.Fatalf("%s names %s %q, want a time before the unfilled comment's event at %s and the cut %s",
				path, contracts.DispatchAsOfFilledThroughHeader, through, written.Format(time.RFC3339Nano), cut)
		}
		if !strings.Contains(refusal.Body.String(), through) {
			t.Errorf("%s refuses with %s, which does not name the reach %s", path, refusal.Body.String(), through)
		}
	}
	// The refusal is the fill's: a cut it has reached is answered.
	if at := w.searchAt(t, keywords("lighthouse"), before); at.Total != 1 {
		t.Errorf("the search at a cut the fill reached answers %d results, want the comment written before it", at.Total)
	}
}

// requireNotFilled reads target as alice and fails the test unless it answers 409
// AS_OF_SEARCH_NOT_FILLED with no result.
func requireNotFilled(t *testing.T, handler http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	var refusal struct {
		Code    string          `json:"code"`
		Results json.RawMessage `json:"results"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &refusal)
	if response.Code != http.StatusConflict || refusal.Code != contracts.DispatchAsOfSearchNotFilled || refusal.Results != nil {
		t.Fatalf("GET %s: status=%d body=%s, want 409 %s and no result", target, response.Code, response.Body.String(), contracts.DispatchAsOfSearchNotFilled)
	}
	return response
}

// TestAPastTimeSearchFindsNoPostDeletedAfterTheCut (decision 14): a comment and an issue message
// their author deletes after the cut give no hit and no snippet at the cut, by keyword or by
// meaning, though the search read at the cut found them; the fill read both deletes.
func TestAPastTimeSearchFindsNoPostDeletedAfterTheCut(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASDL", "Count the shorebirds", "The count's notes.")
	comment := w.comment(t, "A note on the zebrafinch survey.")
	message := w.message(t, "The kingfisher count is due.")
	w.embedPost(t, "comment", comment, angledVector(0.9, 2))
	w.embedPost(t, "message", message, angledVector(0.85, 3))
	processAllPending(t, w.database, w.embedder)
	cut := w.cut(t)
	then := map[string]searchAnswer{}
	for _, q := range []url.Values{keywords("zebrafinch"), keywords("kingfisher"), meaning(searchAtCutQuery)} {
		then[q.Encode()] = searchAnswer{SearchResponse: w.searchNow(t, q)}
	}
	if then[keywords("zebrafinch").Encode()].hit("comment", comment) == nil || then[keywords("kingfisher").Encode()].hit("message", message) == nil ||
		then[meaning(searchAtCutQuery).Encode()].hit("comment", comment) == nil || then[meaning(searchAtCutQuery).Encode()].hit("message", message) == nil {
		t.Fatalf("the search read at the cut misses a post it must find: %+v", then)
	}
	time.Sleep(5 * time.Millisecond)

	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/comments/"+comment, nil, "alice")
	asOfWrite(t, w.handler, http.MethodDelete, "/api/v1/messages/"+message, nil, "alice")
	w.fill(t, cut)
	var deletes int
	if err := w.database.Pool.QueryRow(t.Context(), `
		select count(*) from search_text_versions where body = '' and id = any($1)`, []string{comment, message}).Scan(&deletes); err != nil {
		t.Fatalf("read the deletes' versions: %v", err)
	}
	if deletes != 2 {
		t.Fatalf("the fill wrote %d versions of the deletes, want one of each", deletes)
	}

	for _, q := range []url.Values{keywords("zebrafinch"), keywords("kingfisher"), meaning(searchAtCutQuery), meaning("zebrafinch kingfisher")} {
		at := w.searchAt(t, q, cut)
		if at.hit("comment", comment) != nil || at.hit("message", message) != nil {
			t.Errorf("the search at the cut for %q finds a post deleted after the cut: %+v", q.Get("q"), at.Results)
		}
		requireNoWord(t, at, "zebrafinch", "the search at the cut for "+q.Get("q"))
		requireNoWord(t, at, "kingfisher", "the search at the cut for "+q.Get("q"))
	}
}

// TestAPastTimeSearchFindsAnEditedMessageByItsTextThen: an issue message edited before the cut and
// again after it is found at the cut by the text it had then, and never by the text before or
// after. The edit before the cut is longer than an event carries whole, with a word past what its
// event holds, so the fill reads that body whole from the message's body history.
func TestAPastTimeSearchFindsAnEditedMessageByItsTextThen(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASEM", "Count the waders", "The count's notes.")
	message := w.message(t, "The tern count is due.")
	paragraph := strings.Repeat("shoreline ", 2000)
	long := "The plover count is due.\n\n" + paragraph + "\n\n" + paragraph + "\n\nThe avocet count closes it."
	if eventBody, truncated := model.EventBody(long); !truncated || strings.Contains(eventBody, "avocet") {
		t.Fatalf("the edit before the cut rides whole in its event; the test needs one whose last word the event cuts")
	}
	w.edit(t, "/api/v1/messages/"+message, long)
	cut := w.cut(t)
	time.Sleep(5 * time.Millisecond)
	w.edit(t, "/api/v1/messages/"+message, "The curlew count is due.")
	w.fill(t, cut)

	for _, word := range []string{"plover", "avocet"} {
		at := w.searchAt(t, keywords(word), cut)
		hit := at.hit("message", message)
		if hit == nil {
			t.Errorf("the search at the cut for %q misses the message, whose text at the cut holds it: %+v", word, at.Results)
			continue
		}
		requireNoWord(t, at, "curlew", "the search at the cut for "+word)
		requireNoWord(t, at, "tern", "the search at the cut for "+word)
	}
	for _, word := range []string{"tern", "curlew"} {
		if at := w.searchAt(t, keywords(word), cut); at.hit("message", message) != nil {
			t.Errorf("the search at the cut for %q finds the message, whose text at the cut does not hold it", word)
		}
	}
	if now := (searchAnswer{SearchResponse: w.searchNow(t, keywords("curlew"))}); now.hit("message", message) == nil {
		t.Fatalf("the present search for \"curlew\" misses the message edited after the cut")
	}
}

// TestAnEditOfCaseOrPunctuationAloneIsAChangeForMeaningSearch: an edit after the cut that changes
// only case and punctuation keeps the comment's search vector, but its text is not the text at
// the cut, so the meaning search at the cut uses none of its windows, though its window was
// embedded before the cut and the present search finds it. The keyword search at the cut still
// finds it by its words then.
func TestAnEditOfCaseOrPunctuationAloneIsAChangeForMeaningSearch(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASCP", "Rig the schooner", "The rigging plan.")
	comment := w.comment(t, "The keel needs fresh varnish")
	w.embedPost(t, "comment", comment, angledVector(0.9, 2))
	processAllPending(t, w.database, w.embedder)
	cut := w.cut(t)
	time.Sleep(5 * time.Millisecond)
	w.edit(t, "/api/v1/comments/"+comment, "the keel needs fresh varnish!")
	w.fill(t, cut)

	var sameVector, embeddedBefore bool
	if err := w.database.Pool.QueryRow(t.Context(), `
		select (select c.search from comments c where c.id = $1::text::uuid)
		         = (select v.search from search_text_versions v where v.kind = 'comment' and v.id = $1::text order by v.event_seq limit 1),
		       (select bool_and(e.embedded_at <= $2) from embeddings e where e.kind = 'comment_block' and split_part(e.id, '#', 1) = 'comment/' || $1::text)`,
		comment, cut.Time()).Scan(&sameVector, &embeddedBefore); err != nil {
		t.Fatalf("read the comment's vector and windows: %v", err)
	}
	if !sameVector || !embeddedBefore {
		t.Fatalf("the edit changed the comment's vector (%t the same) or its windows' embedding time (%t before the cut): the test needs neither", sameVector, embeddedBefore)
	}
	if now := (searchAnswer{SearchResponse: w.searchNow(t, meaning(searchAtCutQuery))}); now.hit("comment", comment) == nil {
		t.Fatalf("the present meaning search misses the comment by its window: %+v", now.Results)
	}
	if at := w.searchAt(t, meaning(searchAtCutQuery), cut); at.hit("comment", comment) != nil {
		t.Errorf("the meaning search at the cut finds the comment whose case and punctuation changed after the cut")
	}
	at := w.searchAt(t, keywords("varnish"), cut)
	if hit := at.hit("comment", comment); hit == nil || strings.Contains(hit.Snippet, "!") {
		t.Errorf("the keyword search at the cut finds the comment as %+v, want it by its words with its text at the cut", hit)
	}
}

// TestAPastTimeSearchDoesNotBroaden (D2): a query of two words that matched nothing at the cut,
// while text at the cut holds one of them and text written after the cut the other, answers no
// result at the cut and leaves query.broadened out, naming it absent, by keyword and by meaning.
// The search read at the cut broadened it and found the text that holds the one word, so a build
// that broadens at the cut answers a result.
func TestAPastTimeSearchDoesNotBroaden(t *testing.T) {
	w := newSearchAtCutWorld(t, "ASBR", "Keep the light", "The keeper's notes.")
	held := w.comment(t, "The lighthouse keeps its log.")
	cut := w.cut(t)
	thenAnswer := w.searchNow(t, keywords("lighthouse marmalade"))
	if !thenAnswer.Query.Broadened || (searchAnswer{SearchResponse: thenAnswer}).hit("comment", held) == nil {
		t.Fatalf("the search read at the cut answers %+v (broadened %t), want the broadened hit of the comment", thenAnswer.Results, thenAnswer.Query.Broadened)
	}
	time.Sleep(5 * time.Millisecond)
	w.comment(t, "A marmalade jar on the windowsill.")
	w.fill(t, cut)

	for _, q := range []url.Values{keywords("lighthouse marmalade"), meaning("lighthouse marmalade")} {
		at := w.searchAt(t, q, cut)
		if len(at.Results) != 0 || at.Total != 0 || at.Reachable != 0 {
			t.Errorf("the search at the cut for %q answers %+v (total %d): a query that matched nothing at the cut answers no result", q.Encode(), at.Results, at.Total)
		}
		query, _ := at.raw["query"].(map[string]any)
		if broadened, given := query["broadened"]; given {
			t.Errorf("the search at the cut for %q gives query.broadened = %v, want it left out", q.Encode(), broadened)
		}
		if !slices.Contains(at.absent, "/query/broadened") {
			t.Errorf("the search at the cut for %q names %v absent, want /query/broadened among them", q.Encode(), at.absent)
		}
	}
}
