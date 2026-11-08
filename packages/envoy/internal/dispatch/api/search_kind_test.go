package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// seedKindSearchCorpus seeds one issue of its own project whose five indexed kinds all match the
// one word "astrolabe": the issue by its title, its primary document by its spec, and a comment,
// an ask and a message by their text. A query for that word therefore matches one row of every
// kind, so a kind named in the request is told apart from the four rows it leaves out.
func seedKindSearchCorpus(t *testing.T, handler http.Handler, project, title string) searchCorpus {
	t.Helper()
	issue := createInteractionIssue(t, handler, project, title, "The astrolabe measures altitude.\n")

	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The astrolabe needs a new rete.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}

	message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{
		"body": "Astrolabe calibrated.",
	}, "alice")
	if message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}

	return searchCorpus{
		issueKey:          issue.Key,
		primaryArtifactID: issue.PrimaryArtifactID,
		commentID:         decodeBody[model.Comment](t, comment).ID,
		askID:             createSearchAsk(t, handler, issue.Key, "Which astrolabe do we keep?", "Brass"),
		messageID:         decodeBody[model.Message](t, message).ID,
	}
}

// kindOf is each kind's one row of seedKindSearchCorpus, by kind.
func kindOf(corpus searchCorpus) map[string]string {
	return map[string]string{
		"issue":    corpus.issueKey,
		"document": corpus.primaryArtifactID,
		"comment":  corpus.commentID,
		"ask":      corpus.askID,
		"message":  corpus.messageID,
	}
}

// resultKinds is the kind of each hit, in the order the page lists them.
func resultKinds(results []model.SearchResult) []string {
	kinds := make([]string, 0, len(results))
	for _, result := range results {
		kinds = append(kinds, result.Kind)
	}
	return kinds
}

// A kind names the rows the search ranks, counts and pages: results, total, reachable, limit and
// offset are that kind's rows alone, and the four rows of other kinds the same query matches are
// absent. With no kind the route answers what it always has, every kind and the same counts,
// which is this test's negative control. This is the degraded keyword path (no embedder is
// configured); TestSearchKindHoldsInTheMeaningPath is the meaning one.
func TestSearchByKindRanksAndCountsThatKindAlone(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedKindSearchCorpus(t, handler, "KIND", "Astrolabe survey")

	every := searchResponse(t, handler, "q=astrolabe")
	if every.Degraded != contracts.SearchDegradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q: no embedder is configured", every.Degraded, contracts.SearchDegradedEmbedderUnavailable)
	}
	wantEvery := []string{"issue", "document", "ask", "comment", "message"}
	if !slices.Equal(resultKinds(every.Results), wantEvery) || every.Total != 5 || every.Reachable != 5 {
		t.Fatalf("no kind: kinds=%v total=%d reachable=%d, want %v, 5 and 5",
			resultKinds(every.Results), every.Total, every.Reachable, wantEvery)
	}

	for kind, id := range kindOf(corpus) {
		t.Run(kind, func(t *testing.T) {
			body := searchResponse(t, handler, "q=astrolabe&kind="+kind)
			if len(body.Results) != 1 || body.Results[0].Kind != kind || body.Results[0].ID != id {
				t.Fatalf("results = %v, want the one %s %s", resultKinds(body.Results), kind, id)
			}
			if body.Total != 1 || body.Reachable != 1 || body.Limit != searchDefaultLimit || body.Offset != 0 {
				t.Fatalf("total=%d reachable=%d limit=%d offset=%d, want 1, 1, %d and 0",
					body.Total, body.Reachable, body.Limit, body.Offset, searchDefaultLimit)
			}
		})
	}
}

// A repeated kind names a set: both kinds' rows are ranked, counted and paged, and no other
// kind's are.
func TestSearchByTwoKindsRanksBothAndNoOther(t *testing.T) {
	handler := newTestHandler(t)
	corpus := seedKindSearchCorpus(t, handler, "KIND", "Astrolabe survey")

	body := searchResponse(t, handler, "q=astrolabe&kind=issue&kind=message")
	want := []string{"issue", "message"}
	if !slices.Equal(resultKinds(body.Results), want) || body.Total != 2 || body.Reachable != 2 {
		t.Fatalf("kinds=%v total=%d reachable=%d, want %v, 2 and 2",
			resultKinds(body.Results), body.Total, body.Reachable, want)
	}
	if body.Results[0].ID != corpus.issueKey || body.Results[1].ID != corpus.messageID {
		t.Fatalf("results = %s then %s, want the issue %s then the message %s",
			body.Results[0].ID, body.Results[1].ID, corpus.issueKey, corpus.messageID)
	}
}

// The project, limit and offset rules are the ones the route has always had: a kind narrows what
// is ranked and nothing else. Two projects hold an issue matching the query, so the kind alone
// answers both, the project one, and limit and offset page the two in one order while total and
// reachable still count both.
func TestSearchByKindKeepsTheProjectLimitAndOffsetRules(t *testing.T) {
	handler := newTestHandler(t)
	first := seedKindSearchCorpus(t, handler, "KIND", "Astrolabe survey")
	second := seedKindSearchCorpus(t, handler, "OTHER", "Astrolabe census")

	both := searchResponse(t, handler, "q=astrolabe&kind=issue")
	if len(both.Results) != 2 || both.Total != 2 || both.Reachable != 2 {
		t.Fatalf("kind=issue over both projects: results=%d total=%d reachable=%d, want 2, 2 and 2",
			len(both.Results), both.Total, both.Reachable)
	}

	scoped := searchResponse(t, handler, "q=astrolabe&kind=issue&project=KIND")
	if len(scoped.Results) != 1 || scoped.Results[0].ID != first.issueKey || scoped.Total != 1 {
		t.Fatalf("kind=issue&project=KIND: results=%v total=%d, want only the issue %s",
			resultKinds(scoped.Results), scoped.Total, first.issueKey)
	}
	if scoped.Results[0].Owner.Key != first.issueKey {
		t.Fatalf("scoped hit owner = %#v, want %s", scoped.Results[0].Owner, first.issueKey)
	}

	page := func(offset int) model.SearchResponse {
		t.Helper()
		body := searchResponse(t, handler, fmt.Sprintf("q=astrolabe&kind=issue&limit=1&offset=%d", offset))
		if len(body.Results) != 1 || body.Total != 2 || body.Reachable != 2 || body.Limit != 1 || body.Offset != offset {
			t.Fatalf("offset %d: results=%d total=%d reachable=%d limit=%d offset=%d, want 1, 2, 2, 1 and %d",
				offset, len(body.Results), body.Total, body.Reachable, body.Limit, body.Offset, offset)
		}
		return body
	}
	walked := []string{page(0).Results[0].ID, page(1).Results[0].ID}
	slices.Sort(walked)
	wantWalked := []string{first.issueKey, second.issueKey}
	slices.Sort(wantWalked)
	if !slices.Equal(walked, wantWalked) {
		t.Fatalf("the two pages list %v, want the two issues %v", walked, wantWalked)
	}
	if past := searchResponse(t, handler, "q=astrolabe&kind=issue&limit=1&offset=2"); len(past.Results) != 0 || past.Total != 2 {
		t.Fatalf("offset past the kind's rows: results=%d total=%d, want none and 2", len(past.Results), past.Total)
	}
}

// The kind filter holds in the meaning path as it does in the degraded keyword path: the query
// shares no word with anything, so every hit here comes from a meaning leg, and a kind still
// leaves out the item of another kind the same query matches.
func TestSearchKindHoldsInTheMeaningPath(t *testing.T) {
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "MEAN", title, spec)
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The rete turns freely again.",
	}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID
	embedWindowsAs(t, database, embedder, "comment_block", "comment/"+commentID, angledVector(0.9, 3))
	processAllPending(t, database, embedder)

	every := searchResponse(t, handler, "q=wibbleflorp")
	if every.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty: the embedder is configured and did not fail", every.Degraded)
	}
	wantEvery := []string{"issue", "comment"}
	if !slices.Equal(resultKinds(every.Results), wantEvery) || every.Total != 2 || every.Reachable != 2 {
		t.Fatalf("no kind: kinds=%v total=%d reachable=%d, want %v, 2 and 2",
			resultKinds(every.Results), every.Total, every.Reachable, wantEvery)
	}

	only := searchResponse(t, handler, "q=wibbleflorp&kind=comment")
	if only.Degraded != "" {
		t.Fatalf("kind=comment Degraded = %q, want empty", only.Degraded)
	}
	if len(only.Results) != 1 || only.Results[0].Kind != "comment" || only.Results[0].ID != commentID {
		t.Fatalf("kind=comment results = %#v, want the one comment %s", only.Results, commentID)
	}
	if only.Total != 1 || only.Reachable != 1 {
		t.Fatalf("kind=comment total=%d reachable=%d, want 1 and 1: the issue the query matches is left out",
			only.Total, only.Reachable)
	}
}

// A kind the index does not hold is refused by name, as a project that is not a project key is,
// rather than answering an empty page; a party and a component are read from their own routes. A
// query under two characters is refused for the query whatever the kind says.
func TestSearchRefusesAKindItDoesNotIndex(t *testing.T) {
	handler := newTestHandler(t)
	wantKindRefusal := "kind must be one of issue, document, comment, ask, message"

	for _, query := range []string{
		"q=astrolabe&kind=party",
		"q=astrolabe&kind=component",
		"q=astrolabe&kind=issue&kind=person",
		"q=astrolabe&kind=",
		"q=astrolabe&kind=Issue",
		"q=astrolabe&kind=issues",
	} {
		t.Run(query, func(t *testing.T) {
			response := searchRequest(t, handler, query)
			body := decodeBody[struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}](t, response)
			if response.Code != http.StatusBadRequest || body.Code != "INVALID_QUERY" || body.Error != wantKindRefusal {
				t.Fatalf("search %q: status=%d refusal=%+v, want 400 INVALID_QUERY %q", query, response.Code, body, wantKindRefusal)
			}
		})
	}

	response := searchRequest(t, handler, "q=a&kind=issue")
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	if response.Code != http.StatusBadRequest || body.Code != "INVALID_QUERY" || body.Error != "q must be at least 2 characters" {
		t.Fatalf("one-character query with a kind: status=%d refusal=%+v, want 400 INVALID_QUERY for the query", response.Code, body)
	}
}

// A set naming none of the indexed kinds matches no row at all, so runSearch answers it with an
// empty page and reads nothing: a server holding no store proves the read, since any read of its
// nil pool would panic here.
func TestSearchOverNoIndexedKindAnswersEmptyWithNoRead(t *testing.T) {
	response, err := directServer(Deps{}).runSearch(context.Background(), "astrolabe", "", 0, searchDefaultLimit, 3)
	if err != nil {
		t.Fatalf("search over no kind: %v", err)
	}
	if response.Results == nil || len(response.Results) != 0 {
		t.Fatalf("results = %#v, want a non-nil empty slice", response.Results)
	}
	if response.Total != 0 || response.Reachable != 0 || response.Limit != searchDefaultLimit || response.Offset != 3 {
		t.Fatalf("total=%d reachable=%d limit=%d offset=%d, want 0, 0, %d and 3",
			response.Total, response.Reachable, response.Limit, response.Offset, searchDefaultLimit)
	}
}

// The kind filter is literal SQL built from searchIndexedKinds alone - the kinds left out, named
// positively, which searchKindsCTE negates - and it binds nothing: the related-item check's
// derived statements number their own parameters on from search's ($9 to $14), and a bound
// parameter here would renumber them.
func TestSearchKindFilterIsLiteralSQLThatBindsNothing(t *testing.T) {
	cases := []struct {
		values []string
		kinds  searchKinds
		want   string
	}{
		{values: nil, kinds: searchEveryKind, want: ""},
		{values: []string{"issue", "document", "comment", "ask", "message"}, kinds: searchEveryKind, want: ""},
		{values: []string{"issue"}, want: "kind in ('document', 'comment', 'ask', 'message')"},
		{values: []string{"issue", "issue"}, want: "kind in ('document', 'comment', 'ask', 'message')"},
		{values: []string{"ask", "message"}, want: "kind in ('issue', 'document', 'comment')"},
	}
	for _, test := range cases {
		t.Run(strings.Join(test.values, ","), func(t *testing.T) {
			kinds, known := parseSearchKinds(test.values)
			if !known {
				t.Fatalf("parseSearchKinds(%v) refused known kinds", test.values)
			}
			if test.kinds != 0 && kinds != test.kinds {
				t.Fatalf("parseSearchKinds(%v) = %d, want %d", test.values, kinds, test.kinds)
			}
			if got := kinds.exclusion(); got != test.want {
				t.Fatalf("exclusion = %q, want %q", got, test.want)
			}
		})
	}
	if got := searchKinds(0).exclusion(); got != "kind in ('issue', 'document', 'comment', 'ask', 'message')" {
		t.Fatalf("the empty set's exclusion = %q, want every kind left out", got)
	}

	kinds, _ := parseSearchKinds([]string{"ask"})
	statements := kinds.statements()
	for parameter := 9; parameter <= 14; parameter++ {
		if placeholder := fmt.Sprintf("$%d", parameter); strings.Contains(statements.keyword, placeholder) {
			t.Fatalf("the keyword statement over one kind names %s; it binds $1 to $8 and nothing else", placeholder)
		}
	}
	for parameter := 12; parameter <= 14; parameter++ {
		if placeholder := fmt.Sprintf("$%d", parameter); strings.Contains(statements.meaning, placeholder) {
			t.Fatalf("the meaning statement over one kind names %s; it binds $1 to $11 and nothing else", placeholder)
		}
	}
	if !strings.Contains(statements.keyword, "kind in ('issue', 'document', 'comment', 'message')") ||
		!strings.Contains(statements.meaning, "kind in ('issue', 'document', 'comment', 'message')") {
		t.Fatal("neither statement carries the kinds it leaves out as literal SQL")
	}
	if searchEveryKind.statements().keyword != searchQuery || searchEveryKind.statements().meaning != searchQueryMeaning {
		t.Fatal("every kind runs other text than searchQuery and searchQueryMeaning")
	}
}
