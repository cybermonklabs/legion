package api

import (
	"net/http"
	"testing"
)

// The shape map is the table's sixth column: every mounted row has a shape, and every shape
// names a mounted row, so routeShapes and the table the server mounts are the same set.
//
// No count is pinned, for the reason routes_table_test.go gives above
// TestRoutesTablePinsEveryRegisteredRoute: a count is one number two branches cannot both be
// right about. What catches a route added or removed by accident is the committed index
// testdata/routes.txt, which TestRouteIndexListsEveryRouteSortedByPathThenMethod compares the
// table against.
func TestRouteShapesCoverEveryRouteTheTableMounts(t *testing.T) {
	// 176 production rows, the count testdata/routes.txt pins (routes_table_test.go), plus the
	// 20 rows Deps.TestHooksEnabled adds.
	const shapedRoutes = 196
	if len(routeShapes) != shapedRoutes {
		t.Fatalf("routeShapes has %d entries, want %d (update the pin when adding a route)", len(routeShapes), shapedRoutes)
	}
	if shapeless := shapelessRoutes((&server{deps: Deps{TestHooksEnabled: true}}).routes()); len(shapeless) > 0 {
		t.Fatalf("mounted rows with no shape: %v", shapeless)
	}
	if unmounted := shapedRoutesNotMounted(); len(unmounted) > 0 {
		t.Fatalf("routeShapes names routes the table does not mount: %v", unmounted)
	}
}

// A shape describes what a success answers, so its status must be one a success can carry, and a
// route that answers a body must name that body's type: an entry with no Response, no other
// answer and no reason its bytes carry no schema is a route that answers nothing at all, which
// is 204 and no other status. A row that marks its request optional and names no body to leave
// out is the emitter's own refusal (openapi.go, requestBody), which holds every row the document
// is written from, so the predicate for a body is stated there alone.
func TestRouteShapesDescribeASuccess(t *testing.T) {
	for name, shape := range routeShapes {
		if shape.Status < http.StatusOK || shape.Status >= http.StatusMultipleChoices {
			t.Errorf("%s: success status %d is not a 2xx", name, shape.Status)
		}
		if shape.Response == nil && len(shape.Also) == 0 && shape.Opaque == "" && shape.Status != http.StatusNoContent {
			t.Errorf("%s: answers %d with no response type; a route that answers a body must name it", name, shape.Status)
		}
	}
}
