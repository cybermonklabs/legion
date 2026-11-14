package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// embedderCalls is how many times the fake embedder has been called so far. A test reads it
// before and after the one request it is about, because an issue create runs the related-item
// check, which embeds its own text: only the difference belongs to the search.
func embedderCalls(embedder *fakeEmbedder) int {
	embedder.callsMu.Lock()
	defer embedder.callsMu.Unlock()
	return embedder.calls
}

// TestSearchKeywordsOnlyMakesNoEmbeddingCall is the bar's keystroke request: with an embedder
// configured, as production's is, ?keywords_only=true answers from the keyword statement alone
// and the model service is never called. The same query without it embeds, finds the one item
// that shares no word with it, and costs exactly one call - so the parameter, and nothing else
// about the request, is what the saving comes from.
func TestSearchKeywordsOnlyMakesNoEmbeddingCall(t *testing.T) {
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "KWON", title, spec)
	processAllPending(t, database, embedder)

	before := embedderCalls(embedder)
	keywords := searchResponse(t, handler, url.Values{"q": {"wibbleflorp"}, "keywords_only": {"true"}}.Encode())
	if made := embedderCalls(embedder) - before; made != 0 {
		t.Fatalf("a keywords_only search made %d embedding calls, want 0", made)
	}
	if len(keywords.Results) != 0 {
		t.Fatalf("Results = %+v, want none: the query shares no word with the corpus and nothing was embedded", keywords.Results)
	}
	// Nothing about this deployment's meaning search is unavailable: the caller asked for the
	// words alone, so the answer says nothing about a degraded search.
	if keywords.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty: the request asked for keywords only, which is no degradation", keywords.Degraded)
	}

	before = embedderCalls(embedder)
	meaning := searchResponse(t, handler, url.Values{"q": {"wibbleflorp"}}.Encode())
	if made := embedderCalls(embedder) - before; made != 1 {
		t.Fatalf("a search without keywords_only made %d embedding calls, want exactly 1", made)
	}
	if len(meaning.Results) != 1 || meaning.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want exactly one result: the issue %q the meaning leg found", meaning.Results, issue.Key)
	}
}

// TestSearchKeywordsOnlyFalseSearchesByMeaning holds the other accepted value to what it says:
// keywords_only=false is the request a caller makes when it wants everything, and it embeds.
func TestSearchKeywordsOnlyFalseSearchesByMeaning(t *testing.T) {
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "KWOF", title, spec)
	processAllPending(t, database, embedder)

	before := embedderCalls(embedder)
	response := searchResponse(t, handler, url.Values{"q": {"wibbleflorp"}, "keywords_only": {"false"}}.Encode())
	if made := embedderCalls(embedder) - before; made != 1 {
		t.Fatalf("keywords_only=false made %d embedding calls, want exactly 1", made)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the issue %q the meaning leg found", response.Results, issue.Key)
	}
}

// TestSearchKeywordsOnlyAnswersTheKeywordHits proves the parameter narrows what search calls and
// not what it answers: the keyword match a query names is on the page exactly as it is without
// the parameter.
func TestSearchKeywordsOnlyAnswersTheKeywordHits(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "KWHI", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, url.Values{"q": {"astrolabe"}, "keywords_only": {"true"}}.Encode())
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty: this deployment configured no embedder, but the caller asked for keywords only", response.Degraded)
	}
}

// TestSearchRefusesAnUnreadableKeywordsOnly refuses every value that is neither true nor false,
// and a repeated parameter, under a code of its own, exactly as the project and the page
// parameters beside it are refused.
func TestSearchRefusesAnUnreadableKeywordsOnly(t *testing.T) {
	handler := newTestHandler(t)
	createInteractionIssue(t, handler, "KWBA", "Astrolabe calibration guide", "Keyword body text.")

	for name, query := range map[string]url.Values{
		"a word that is neither": {"q": {"astrolabe"}, "keywords_only": {"yes"}},
		"an empty value":         {"q": {"astrolabe"}, "keywords_only": {""}},
		"the number one":         {"q": {"astrolabe"}, "keywords_only": {"1"}},
		"repeated":               {"q": {"astrolabe"}, "keywords_only": {"true", "false"}},
	} {
		t.Run(name, func(t *testing.T) {
			response := searchRequest(t, handler, query.Encode())
			if response.Code != http.StatusBadRequest {
				t.Fatalf("search with %s keywords_only: status=%d body=%s", name, response.Code, response.Body.String())
			}
			body := decodeBody[struct {
				Code string `json:"code"`
			}](t, response)
			if body.Code != "INVALID_KEYWORDS_ONLY" {
				t.Fatalf("code = %q, want INVALID_KEYWORDS_ONLY", body.Code)
			}
		})
	}
}

// TestSearchKeywordsOnlyKeepsTheFiltersAndThePage proves the parameter rides beside the rest of
// the request rather than replacing it: the chips still narrow every leg and the page is still
// the page that was asked for.
func TestSearchKeywordsOnlyKeepsTheFiltersAndThePage(t *testing.T) {
	handler := newTestHandler(t)
	open := createInteractionIssue(t, handler, "KWFI", "Astrolabe calibration guide", "Keyword body text.")
	closed := createSearchIssue(t, handler, "KWFI", "Astrolabe mounting bracket", "Keyword body text.")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+closed,
		map[string]string{"status": "done"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("close %s: status=%d body=%s", closed, response.Code, response.Body.String())
	}

	response := searchResponse(t, handler, url.Values{
		"q": {"astrolabe"}, "keywords_only": {"true"}, "status": {"done"}, "limit": {"5"},
	}.Encode())
	if response.Limit != 5 || response.Offset != 0 {
		t.Fatalf("page = {limit %d, offset %d}, want {5, 0}", response.Limit, response.Offset)
	}
	if (response.Filters != model.SearchFilters{Status: "done"}) {
		t.Fatalf("filters = %#v, want the status chip alone echoed", response.Filters)
	}
	if len(response.Results) == 0 {
		t.Fatalf("results = %#v, want the rows of %s, the one closed issue", response.Results, closed)
	}
	for _, result := range response.Results {
		if result.Owner.Kind != "issue" || result.Owner.Key != closed {
			t.Fatalf("result = %#v, want only rows of %s; %s is open and the filter leaves it out",
				result, closed, open.Key)
		}
	}
}
