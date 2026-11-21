package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func init() { addTourSteps("index", indexTourSteps) }

// indexTourSteps are the route tour's steps of the route index, GET /api/v2: read whole, as a
// request naming no limit reads it, and a page of it.
func indexTourSteps() []tourStep {
	return []tourStep{
		{
			Method: http.MethodGet, Pattern: "", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest { return tourRequest{} },
		},
		{
			Method: http.MethodGet, Pattern: "", Name: "a page of it", Status: http.StatusOK,
			Setup: func(*testing.T, *routeTour) tourRequest {
				return tourRequest{Query: url.Values{"limit": {"2"}, "offset": {"1"}}}
			},
		},
	}
}

// GET /api/v2 answers every route on one page where the request names no limit, limit the whole
// list, and the page a request names - the same routes, in the same order - where it names a limit
// and an offset; a limit of 0 and a negative offset are refused, naming the bound (R1.4).
func TestTheV2RouteIndexReadsItsPageThroughTheDecoder(t *testing.T) {
	handler := newTestHandler(t)
	read := func(target string) routeIndexV2 {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, target, nil, "")
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, response.Code, response.Body.String())
		}
		return decodeBody[routeIndexV2](t, response)
	}
	whole := read("/api/v2")
	if whole.Total < 3 || len(whole.Items) != whole.Total || whole.Limit != whole.Total || whole.Offset != 0 || whole.Docs == "" {
		t.Fatalf("the whole index answered total %d, %d items, limit %d, offset %d: want every route, limit the whole list",
			whole.Total, len(whole.Items), whole.Limit, whole.Offset)
	}
	paged := read("/api/v2?limit=2&offset=1")
	if paged.Total != whole.Total || paged.Limit != 2 || paged.Offset != 1 ||
		!slices.EqualFunc(paged.Items, whole.Items[1:3], func(left, right routeIndexV2Entry) bool {
			return left.Path == right.Path && left.Method == right.Method
		}) {
		t.Fatalf("the page at limit 2, offset 1 answered %+v, want routes 2 and 3 of %d", paged.page, whole.Total)
	}
	past := read("/api/v2?offset=100000")
	if past.Total != whole.Total || past.Items == nil || len(past.Items) != 0 {
		t.Fatalf("a page past the end answered %d items (nil %v), want an empty list", len(past.Items), past.Items == nil)
	}
	for _, refusal := range []struct{ target, code, bound string }{
		{"/api/v2?limit=0", "INVALID_LIMIT", "1 or more"},
		{"/api/v2?offset=-1", "INVALID_OFFSET", "0 or more"},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, refusal.target, nil, "")
		if refused, text := refusalOf(t, response); response.Code != http.StatusBadRequest || refused != refusal.code || !strings.Contains(text, refusal.bound) {
			t.Errorf("GET %s: %d %s, want 400 %s naming the bound %q", refusal.target, response.Code, response.Body.String(), refusal.code, refusal.bound)
		}
	}
}

// A list whose row names no bounds is read at limit 1 to 200, default 50 (decodeLimit), so the
// route index and the document state those bounds for it as for a list that names its own: every
// route whose query takes a limit carries its bounds in its index entry and in its operation's
// x-list, the two alike, and GET /api/v2/asks, which names none, carries 50 and 200.
func TestTheV2IndexAndDocumentStateTheBoundsOfEveryList(t *testing.T) {
	tree, _ := readOpenAPITreeOf(t, apiV2)
	document, _ := tree.(map[string]any)
	entries := map[string]routeIndexV2Entry{}
	for _, entry := range routeIndexV2Entries(servedAt(apiV2, (&server{deps: Deps{TestHooksEnabled: true}}).routes())) {
		entries[entry.Method+" "+entry.Path] = entry
	}
	lists := 0
	for _, route := range servedAt(apiV2, (&server{deps: Deps{TestHooksEnabled: true}}).routes()) {
		if !route.v2Bound() {
			continue
		}
		name := route.Method + " " + route.Pattern
		query, err := compileV2Query(route.Query)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, pages := query.field("limit"); !pages {
			continue
		}
		lists++
		entry := entries[name]
		if entry.List == nil {
			t.Errorf("%s takes a limit, and its index entry names no bounds", name)
			continue
		}
		stated, err := json.Marshal(openAPIAt(document, "paths", route.Pattern, strings.ToLower(route.Method))["x-list"])
		if err != nil {
			t.Fatal(err)
		}
		indexed, err := json.Marshal(entry.List)
		if err != nil {
			t.Fatal(err)
		}
		if string(stated) != string(indexed) {
			t.Errorf("%s: the document's x-list is %s, the index entry's list %s", name, stated, indexed)
		}
	}
	if lists == 0 {
		t.Fatal("no /api/v2 route takes a limit")
	}
	asks := entries["GET /api/v2/asks"].List
	if asks == nil || asks.DefaultLimit == nil || *asks.DefaultLimit != 50 || asks.MaxLimit == nil || *asks.MaxLimit != 200 || asks.MinLimit != 1 {
		t.Fatalf("GET /api/v2/asks's index entry states the bounds %+v, want default 50, max 200, min 1", asks)
	}
}
