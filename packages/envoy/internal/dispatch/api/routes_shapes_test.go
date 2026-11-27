package api

import (
	"net/http"
	"testing"
)

// Every mounted route has a shape - a route file its declaration's Shape, the mount of a routeRow
// its Shape column (shapelessRoutes). A shape is a field of its route's own declaration or row, so
// none can name a route the table does not mount.
//
// No count of the table's rows is pinned, for the reason routes_table_test.go gives above
// TestRoutesTablePinsEveryRegisteredRoute: a count is one number two branches cannot both be
// right about. A route added or removed is a route file or a routeRow added or removed, which a
// reviewer sees. Of the test-hook routes only their number is pinned: len(routes)+2*15 in
// TestRoutesTablePinsEveryRegisteredRoute, fifteen hooks at two paths each.
func TestRouteShapesCoverEveryRouteTheTableMounts(t *testing.T) {
	t.Parallel()
	if shapeless := shapelessRoutes((&server{deps: Deps{TestHooksEnabled: true}}).routes()); len(shapeless) > 0 {
		t.Fatalf("mounted routes with no shape: %v", shapeless)
	}
}

// A shape describes what a success answers, so its status must be one a success can carry, and a
// route that answers a body must name that body's type: a shape with no Response, no other
// answer and no reason its bytes carry no schema is a route that answers nothing at all, which
// is 204 and no other status. A route that marks its request optional and names no body to leave
// out is the emitter's own refusal (openapi.go, requestBody), which holds every route the
// document is written from, so the predicate for a body is stated there alone. Every route is
// read, the shapes the route files state and the ones a routeRow declares in its own columns
// alike.
func TestRouteShapesDescribeASuccess(t *testing.T) {
	t.Parallel()
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
