package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// searchNarrowingMembers are the five query members GET /api/v1/search/results declares and
// GET /api/v1/search no longer does: a build released before them answers the new path 404 and
// would answer the old one 200 with the member dropped, which is unnarrowed rows under the
// filters a person chose.
var searchNarrowingMembers = []string{"number_project", "status", "author", "updated_since", "keywords_only"}

// searchResultsRequest is searchRequest's twin (search_test.go) on the path the five members are
// declared on: the same caller, the same query, the other path.
func searchResultsRequest(t *testing.T, handler http.Handler, query string) *httptest.ResponseRecorder {
	t.Helper()
	return dispatchRequest(t, handler, http.MethodGet, "/api/v1/search/results?"+query, nil, "alice")
}

// searchBothPaths answers one query on both paths: first GET /api/v1/search/results, where a
// member is declared, then GET /api/v1/search, where the handler still reads it for a page
// served by a release before this one. Each must be 200.
func searchBothPaths(t *testing.T, handler http.Handler, query string) (results, old model.SearchResponse) {
	t.Helper()
	return searchPathResponse(t, searchResultsRequest(t, handler, query), "/api/v1/search/results", query),
		searchPathResponse(t, searchRequest(t, handler, query), "/api/v1/search", query)
}

func searchPathResponse(t *testing.T, response *httptest.ResponseRecorder, path, query string) model.SearchResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s?%s: status=%d body=%s", path, query, response.Code, response.Body.String())
	}
	return decodeBody[model.SearchResponse](t, response)
}

// searchRows is a page as the comparison below reads it: each row as `kind:id`, in the order the
// answer put them.
func searchRows(body model.SearchResponse) []string {
	rows := make([]string, 0, len(body.Results))
	for _, result := range body.Results {
		rows = append(rows, result.Kind+":"+result.ID)
	}
	return rows
}

// requireTheSameAnswer holds the old path to what the new one answered. It is the half of every
// row below that fails if anyone takes the five reads out of the handler: the declaration moved
// to GET /api/v1/search/results, the reads did not, so a page of an earlier release that still
// sends ?status= to GET /api/v1/search is still answered narrowed.
func requireTheSameAnswer(t *testing.T, query string, results, old model.SearchResponse) {
	t.Helper()
	if got, want := searchRows(old), searchRows(results); !slices.Equal(got, want) {
		t.Fatalf("GET /api/v1/search?%s answered %v, want %v, the rows GET /api/v1/search/results answered",
			query, got, want)
	}
	if old.Total != results.Total || old.Reachable != results.Reachable || old.Filters != results.Filters {
		t.Fatalf("GET /api/v1/search?%s answered total=%d reachable=%d filters=%#v, want %d, %d and %#v",
			query, old.Total, old.Reachable, old.Filters, results.Total, results.Reachable, results.Filters)
	}
}

// searchResultsCorpus is one project holding two issues a filter can tell apart - a different
// status, a different author and a different write time each - and one issue of a second project
// carrying the same number as the first, which is what ?number_project= is read against.
type searchResultsCorpus struct {
	// survey is SRCH-1042, alice's, moved to todo and written before cut; early is her comment
	// on it.
	survey, early string
	// atlas is SRCH-1043, bob's, left in the status a new issue takes and written after cut;
	// late is his comment on it.
	atlas, late string
	// twin is OTHER-1042, the other project's issue of the same number.
	twin string
	// cut is the database's own clock between the two, the clock every row's time is written by.
	cut time.Time
}

func seedSearchResultsCorpus(t *testing.T, handler http.Handler, database *store.Store) searchResultsCorpus {
	t.Helper()
	ctx := context.Background()
	for _, project := range []string{"SRCH", "OTHER"} {
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
			"key": project, "name": project + " project",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create project %s: status=%d body=%s", project, response.Code, response.Body.String())
		}
	}
	// Both projects are moved to the same four-digit number, as the bar's own number tests move
	// them (search_bar_test.go): an issue's number is its project's next, and a one-digit query
	// is below the route's two-character minimum.
	if _, err := database.Pool.Exec(ctx,
		"update projects set next_number = 1042 where key in ('SRCH', 'OTHER')"); err != nil {
		t.Fatalf("move both projects to number 1042: %v", err)
	}

	survey := createSearchIssue(t, handler, "SRCH", "Astrolabe survey", "")
	if patch := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+survey,
		map[string]any{"status": "todo"}, "alice"); patch.Code != http.StatusOK {
		t.Fatalf("move the survey to todo: status=%d body=%s", patch.Code, patch.Body.String())
	}
	early := searchComment(t, handler, survey, "alice", "The astrolabe needs a rete.")

	var cut time.Time
	if err := database.Pool.QueryRow(ctx, "select now()").Scan(&cut); err != nil {
		t.Fatalf("read the database's clock: %v", err)
	}

	// bob's issue, so the author filter has two actors to tell apart: createSearchIssue files
	// every issue as alice.
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "SRCH", "title": "Astrolabe atlas", "spec": "", "force": true,
	}, "bob")
	if created.Code != http.StatusCreated {
		t.Fatalf("create bob's issue: status=%d body=%s", created.Code, created.Body.String())
	}
	atlas := decodeBody[model.Issue](t, created).Key
	late := searchComment(t, handler, atlas, "bob", "An astrolabe reading from the atlas.")

	twin := createSearchIssue(t, handler, "OTHER", "Harbour logbook", "")
	if survey != "SRCH-1042" || atlas != "SRCH-1043" || twin != "OTHER-1042" {
		t.Fatalf("seeded issues are %q, %q and %q, want SRCH-1042, SRCH-1043 and OTHER-1042", survey, atlas, twin)
	}
	return searchResultsCorpus{survey: survey, early: early, atlas: atlas, late: late, twin: twin, cut: cut}
}

// A path the server does not serve answering the JSON 404 {NOT_FOUND, "no route for GET <path>",
// "GET /api/v1 lists every route"} - the answer an older build gives GET /api/v1/search/results,
// and the whole reason the five members moved onto a path of their own - is pinned in
// packages/envoy/internal/dispatch/routes/router_test.go, by
// TestUnknownReservedPathIsJSONNotFoundWithHint, which asserts that code, that error and that
// hint for an unknown path under /api/v1. It is pinned there and not again here.

// Each of the five members, applied on GET /api/v1/search/results and answered exactly the same
// on GET /api/v1/search. The first half is the new path carrying the contract; the second is
// what keeps a page served by an earlier release correct, and it fails the moment anyone takes
// the reads out of the handler because the declaration moved.
func TestSearchResultsRouteAppliesEveryMemberAndTheOldPathAnswersTheSame(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	corpus := seedSearchResultsCorpus(t, handler, database)
	processAllPending(t, database, embedder)

	// The control: with no member at all every row is on the page, so each filter below has
	// something to leave out.
	unfiltered, _ := searchBothPaths(t, handler, "q=astrolabe")
	for _, want := range []struct{ kind, id string }{
		{"issue", corpus.survey}, {"issue", corpus.atlas},
		{"comment", corpus.early}, {"comment", corpus.late},
	} {
		if searchResultOf(unfiltered, want.kind, want.id) == nil {
			t.Fatalf("unfiltered results = %#v, want the %s %s, or the members below decide nothing",
				unfiltered.Results, want.kind, want.id)
		}
	}

	t.Run("status", func(t *testing.T) {
		query := url.Values{"q": {"astrolabe"}, "status": {"todo"}}.Encode()
		results, old := searchBothPaths(t, handler, query)
		if searchResultOf(results, "issue", corpus.survey) == nil ||
			searchResultOf(results, "comment", corpus.early) == nil {
			t.Fatalf("results = %#v, want the todo issue %s and its comment %s", results.Results, corpus.survey, corpus.early)
		}
		for _, result := range results.Results {
			if result.Owner.Kind != "issue" || result.Owner.Key != corpus.survey {
				t.Fatalf("result = %#v, want only rows of %s, the one issue in todo", result, corpus.survey)
			}
		}
		requireTheSameAnswer(t, query, results, old)
	})

	t.Run("author", func(t *testing.T) {
		query := url.Values{"q": {"astrolabe"}, "author": {"bob"}}.Encode()
		results, old := searchBothPaths(t, handler, query)
		if searchResultOf(results, "issue", corpus.atlas) == nil ||
			searchResultOf(results, "comment", corpus.late) == nil {
			t.Fatalf("results = %#v, want bob's issue %s and comment %s", results.Results, corpus.atlas, corpus.late)
		}
		for _, gone := range []struct{ kind, id string }{
			{"issue", corpus.survey}, {"comment", corpus.early},
		} {
			if searchResultOf(results, gone.kind, gone.id) != nil {
				t.Fatalf("results = %#v, want nothing of alice's %s %s", results.Results, gone.kind, gone.id)
			}
		}
		requireTheSameAnswer(t, query, results, old)
	})

	t.Run("updated_since", func(t *testing.T) {
		query := url.Values{
			"q": {"astrolabe"}, "updated_since": {corpus.cut.Format(time.RFC3339Nano)},
		}.Encode()
		results, old := searchBothPaths(t, handler, query)
		if searchResultOf(results, "issue", corpus.atlas) == nil ||
			searchResultOf(results, "comment", corpus.late) == nil {
			t.Fatalf("results = %#v, want the issue %s and the comment %s, both written after the watermark",
				results.Results, corpus.atlas, corpus.late)
		}
		if searchResultOf(results, "comment", corpus.early) != nil {
			t.Fatalf("results = %#v, want nothing of the comment %s, written before the watermark",
				results.Results, corpus.early)
		}
		requireTheSameAnswer(t, query, results, old)
	})

	t.Run("number_project", func(t *testing.T) {
		// The context narrows nothing; it decides which project's issue a bare number names.
		// With no context both issues of number 1042 head the list in project order, OTHER
		// before SRCH, so the context moving SRCH's to the front is the member applied.
		without, _ := searchBothPaths(t, handler, url.Values{"q": {"1042"}}.Encode())
		if len(without.Results) == 0 || without.Results[0].ID != corpus.twin {
			t.Fatalf("results = %#v, want %s first with no context, or the context below decides nothing",
				without.Results, corpus.twin)
		}
		query := url.Values{"q": {"1042"}, "number_project": {"SRCH"}}.Encode()
		results, old := searchBothPaths(t, handler, query)
		if len(results.Results) == 0 || results.Results[0].ID != corpus.survey {
			t.Fatalf("results = %#v, want %s first: the page's project is what the number names",
				results.Results, corpus.survey)
		}
		if searchResultOf(results, "issue", corpus.twin) == nil {
			t.Fatalf("results = %#v, want %s too: the context narrows nothing", results.Results, corpus.twin)
		}
		requireTheSameAnswer(t, query, results, old)
	})

	t.Run("keywords_only", func(t *testing.T) {
		// The member is read by what the request costs, as search_keywords_only_test.go reads
		// it: the keyword leg alone makes no embedding call, and the same query without it
		// makes exactly one. Both paths are counted, since both must apply it.
		query := url.Values{"q": {"astrolabe"}, "keywords_only": {"true"}}.Encode()
		for path, request := range map[string]func() *httptest.ResponseRecorder{
			"/api/v1/search/results": func() *httptest.ResponseRecorder { return searchResultsRequest(t, handler, query) },
			"/api/v1/search":         func() *httptest.ResponseRecorder { return searchRequest(t, handler, query) },
		} {
			before := embedderCalls(embedder)
			searchPathResponse(t, request(), path, query)
			if made := embedderCalls(embedder) - before; made != 0 {
				t.Fatalf("a keywords_only search of %s made %d embedding calls, want 0", path, made)
			}
		}
		hybrid := url.Values{"q": {"astrolabe"}}.Encode()
		for path, request := range map[string]func() *httptest.ResponseRecorder{
			"/api/v1/search/results": func() *httptest.ResponseRecorder { return searchResultsRequest(t, handler, hybrid) },
			"/api/v1/search":         func() *httptest.ResponseRecorder { return searchRequest(t, handler, hybrid) },
		} {
			before := embedderCalls(embedder)
			searchPathResponse(t, request(), path, hybrid)
			if made := embedderCalls(embedder) - before; made != 1 {
				t.Fatalf("a search of %s without keywords_only made %d embedding calls, want exactly 1", path, made)
			}
		}
		results, old := searchBothPaths(t, handler, query)
		requireTheSameAnswer(t, query, results, old)
	})
}

// The committed document is what every generated client is written from, so it is read here from
// the package the way TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites reads it
// (openapi_test.go). The five members are offered on GET /api/v1/search/results and on no other
// search operation: this fails if anyone puts one back on the old route's declared contract,
// where an older build would drop it and answer unnarrowed rows 200.
func TestTheCommittedDocumentOffersTheFiveMembersOnTheResultsRouteAlone(t *testing.T) {
	// From this package to the repository root, which OpenAPIDocumentPath is written from.
	path := filepath.Join("..", "..", "..", "..", "..", filepath.FromSlash(OpenAPIDocumentPath))
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed document: %v", err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(committed, &document); err != nil {
		t.Fatalf("read the committed document: %v", err)
	}

	operations := 0
	for route, methods := range document.Paths {
		if !strings.HasPrefix(route, "/api/v1/search") {
			continue
		}
		for method, operation := range methods {
			operations++
			offered := make([]string, 0, len(operation.Parameters))
			for _, parameter := range operation.Parameters {
				offered = append(offered, parameter.Name)
			}
			onTheResultsRoute := route == "/api/v1/search/results" && method == "get"
			for _, member := range searchNarrowingMembers {
				switch {
				case onTheResultsRoute && !slices.Contains(offered, member):
					t.Errorf("%s %s offers %v, want %s among them", method, route, offered, member)
				case !onTheResultsRoute && slices.Contains(offered, member):
					t.Errorf("%s %s offers %s; the five members belong to GET /api/v1/search/results alone",
						method, route, member)
				}
			}
		}
	}
	if operations < 3 {
		t.Fatalf("the committed document holds %d operations under /api/v1/search, want the three the table mounts: "+
			"search, its results and its coverage", operations)
	}
}
