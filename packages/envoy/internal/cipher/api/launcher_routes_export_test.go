// launcher_routes_export_test.go hands this package's external tests the rows of the broker's
// route table that take a launcher proof, so a test that calls each such route reads them from
// routes() and calls a launcher route added later too.
package api

// LauncherRoutes is the method and pattern of every row of routes() that launcherAuth serves, in
// the table's order.
func LauncherRoutes() [][2]string {
	var rows [][2]string
	for _, route := range routes() {
		if route.Handler.auth == authLauncher {
			rows = append(rows, [2]string{route.Method, route.Pattern})
		}
	}
	return rows
}
