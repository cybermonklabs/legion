package api

import (
	"net/http"
	"net/http/httptest"
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
	for _, route := range index.Routes {
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
