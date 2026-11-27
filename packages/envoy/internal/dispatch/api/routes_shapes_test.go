package api

import (
	"net/http"
	"testing"
)

// Every route the server mounts states a shape (routeDecl.Shape): a route with none is one the
// generated OpenAPI document would leave out. A shape is a field of its route's own declaration,
// so none can name a route the table does not mount.
//
// No count is pinned, for the reason routes_table_test.go gives above
// TestRoutesTablePinsEveryRegisteredRoute: a count is one number two branches cannot both be
// right about. A route added or removed is a route file added or removed, which a reviewer sees.
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
// document is written from, so the predicate for a body is stated there alone.
func TestRouteShapesDescribeASuccess(t *testing.T) {
	t.Parallel()
	for _, route := range routeRegistry {
		name, shape := route.Method+" "+route.Pattern, route.Shape
		if shape.Status < http.StatusOK || shape.Status >= http.StatusMultipleChoices {
			t.Errorf("%s: success status %d is not a 2xx", name, shape.Status)
		}
		if shape.Response == nil && len(shape.Also) == 0 && shape.Opaque == "" && shape.Status != http.StatusNoContent {
			t.Errorf("%s: answers %d with no response type; a route that answers a body must name it", name, shape.Status)
		}
	}
}
