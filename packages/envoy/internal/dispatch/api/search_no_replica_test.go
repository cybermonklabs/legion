package api

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestTheBarAddsNoReplicaRoute holds the command bar to reading Dispatch rather than copying it.
// The bar type-aheads off the keyword path of GET /api/v1/search and keeps nothing of the graph
// in the browser but the person's own recent items and the result sets it fetched, so the three
// routes a browser-held replica of the issues and documents would need are routes this table
// does not have:
//
//   - a delta route, which a replica asks for what changed since it last read;
//   - a cursor, which a replica pages a snapshot by;
//   - a bulk-item route, which hands a replica the items themselves in one read.
//
// A replica is a snapshot to build, deltas to apply, a cursor to keep and a per-actor cache to
// invalidate, and it is instant only for what the browser already holds; the keyword path is 28
// ms over the whole graph (dispatch://INFRA-1042, R2, "Keep the server keyword path"). This test
// is what makes that decision visible to whoever adds the first of those routes: a route added
// for some other reason that matches one of these names has to be read against R2 and either
// renamed or argued for, which is exactly the conversation the decision is owed.
func TestTheBarAddsNoReplicaRoute(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	Register(mux, t.Context(), Deps{AgentTokens: sharedAgentTokens(t, "agent-token")})

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	index := decodeBody[routeIndex](t, response)
	if len(index.Routes) == 0 {
		t.Fatal("the route index is empty; this test would then prove nothing")
	}
	// Each name a replica's own route would carry, as a path segment or a query parameter the
	// table's own descriptions name. "/delta" and "/items" are matched as whole segments, so
	// /api/v1/delivery and an item route of one id are left alone.
	//
	// One route carrying "items" is argued for rather than renamed. GET
	// /api/v1/projects/{key}/evals/{slug}/items (dispatch://INFRA-2916/spec#b-r7) pages, by limit
	// and offset, the rows behind one eval's number: the pull requests, runs or issues it counted
	// in a window. It serves no snapshot of the issues and documents, no delta and no cursor, so no
	// browser can build a replica of the graph from it, and its caller is the Delivery page, not
	// the bar. R2 of dispatch://INFRA-1042 refuses a replica, not the list behind one number. Only
	// that method and path, matched exactly, are exempt: every other "items" segment is refused.
	// The argument is held below as far as the route's own shapes state it: the query keys it
	// declares (evalItemsQueryKeys, every other of which it refuses) page by limit and offset and
	// name no cursor, mark or delta, and its answer (evalItemsResponse) carries none either.
	const evalItems = http.MethodGet + " /api/v1/projects/{key}/evals/{slug}/items"
	replicaWord := func(name string) bool {
		return slices.ContainsFunc([]string{"cursor", "since", "after", "before", "delta", "next", "token", "changed"}, func(word string) bool {
			return strings.Contains(name, word)
		})
	}
	declared := evalItemsQueryKeys.declared
	if !slices.Contains(declared, "limit") || !slices.Contains(declared, "offset") || slices.ContainsFunc(declared, replicaWord) {
		t.Errorf("%s declares the query keys %v, want it paged by limit and offset and walked by no cursor, mark or delta", evalItems, declared)
	}
	answer := reflect.TypeFor[evalItemsResponse]()
	for index := range answer.NumField() {
		if field := strings.Split(answer.Field(index).Tag.Get("json"), ",")[0]; replicaWord(field) {
			t.Errorf("%s answers the field %q, a cursor, mark or delta a replica would walk", evalItems, field)
		}
	}
	for _, route := range index.Routes {
		if route.Method+" "+route.Path == evalItems {
			continue
		}
		for _, segment := range strings.Split(route.Path, "/") {
			if segment == "delta" || segment == "deltas" || segment == "store" || segment == "items" {
				t.Errorf("%s %s: the bar holds no replica of the issues and documents, so no delta,"+
					" snapshot-store or bulk-item route serves one (dispatch://INFRA-1042, R2)",
					route.Method, route.Path)
			}
		}
	}
	for _, route := range index.Routes {
		if !strings.HasPrefix(route.Path, "/api/v1/search") {
			continue
		}
		if strings.Contains(route.Description, "cursor") {
			t.Errorf("%s %s names a cursor: the bar pages search by ?offset=, and a cursor is what"+
				" a replica walks a snapshot with (dispatch://INFRA-1042, R2)", route.Method, route.Path)
		}
	}
}
