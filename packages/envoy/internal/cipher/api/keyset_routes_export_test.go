// keyset_routes_export_test.go hands this package's external tests the rows of the broker's route
// table whose caller presents an identity token the broker checks against an issuer's key set, so
// a test that calls each such route reads them from routes() and calls a route added later to one
// of those classes too.
package api

// The class of a KeySetRoutes row, which names the authentication of routes() that serves it.
const (
	KeySetAccount      = "account"       // accountAuth
	KeySetService      = "service"       // serviceAuth
	KeySetEdgeListener = "edge listener" // edgeListenerAuth
	KeySetOperator     = "operator"      // operatorAuth
)

// keySetClasses is each authentication of routes() whose caller's bearer token is checked against
// an issuer's key set, by its KeySetRoutes class.
var keySetClasses = map[routeAuth]string{
	authAccount:      KeySetAccount,
	authService:      KeySetService,
	authEdgeListener: KeySetEdgeListener,
	authOperator:     KeySetOperator,
}

// KeySetRoute is one row of routes() whose caller's bearer token the broker checks against an
// issuer's key set: its method, its pattern and its class (KeySetAccount, KeySetService,
// KeySetEdgeListener or KeySetOperator).
type KeySetRoute struct {
	Method, Pattern, Class string
}

// KeySetRoutes is every row of routes() that accountAuth, serviceAuth, edgeListenerAuth or
// operatorAuth serves, in the table's order.
func KeySetRoutes() []KeySetRoute {
	var rows []KeySetRoute
	for _, route := range routes() {
		if class, ok := keySetClasses[route.Handler.auth]; ok {
			rows = append(rows, KeySetRoute{Method: route.Method, Pattern: route.Pattern, Class: class})
		}
	}
	return rows
}
