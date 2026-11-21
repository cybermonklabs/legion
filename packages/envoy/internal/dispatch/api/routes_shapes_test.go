package api

import (
	"net/http"
	"testing"
)

// Every mounted route has a shape - a route of the v1 table its routeShapes entry, the mount of a
// routeRow its Shape column (shapelessRoutes) - and every entry of routeShapes names a mounted
// route, so routeShapes and the v1 table the server mounts are the same set.
//
// No count of the table's rows is pinned, for the reason routes_table_test.go gives above
// TestRoutesTablePinsEveryRegisteredRoute: a count is one number two branches cannot both be
// right about. What catches a row added or removed by accident is the committed index
// testdata/routes.txt, which TestRouteIndexListsEveryRouteSortedByPathThenMethod compares the
// table against. That index lists no test hook, since a server mounts them only when its hooks are
// enabled, so of the test-hook routes only their number is pinned: len(routes)+2*15 in
// TestRoutesTablePinsEveryRegisteredRoute, fifteen hooks at two paths each.
func TestRouteShapesCoverEveryRouteTheTableMounts(t *testing.T) {
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
// is written from, so the predicate for a body is stated there alone. Every route is read, the
// shapes routeShapes holds and the ones a routeRow declares in its own columns alike.
func TestRouteShapesDescribeASuccess(t *testing.T) {
	for _, route := range (&server{deps: Deps{TestHooksEnabled: true}}).routes() {
		name, shape := route.Method+" "+route.Pattern, route.shape()
		if shape.Status < http.StatusOK || shape.Status >= http.StatusMultipleChoices {
			t.Errorf("%s: success status %d is not a 2xx", name, shape.Status)
		}
		if shape.Response == nil && len(shape.Also) == 0 && shape.Opaque == "" && shape.Status != http.StatusNoContent {
			t.Errorf("%s: answers %d with no response type; a route that answers a body must name it", name, shape.Status)
		}
	}
}
