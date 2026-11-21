// Package unheld is the control of TestEveryRouteAndPathIsHeld, under testdata so that no build
// includes it: each of its three mounts and calls is one the walk must refuse.
package unheld

import (
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
)

// row is a route table's row, mounted in a loop by its Pattern.
type row struct {
	Method  string
	Pattern string
}

// mount mounts a route whose {label} the path rule does not class, once as a literal and once
// from a list in a loop.
func mount(mux *http.ServeMux, h http.HandlerFunc) {
	mux.HandleFunc("GET /api/v1/x/{label}", accesslog.HandlerFunc("/api/v1/x/{label}", h))
	for _, route := range []row{{"GET", "/api/v1/y/{label}/z"}} {
		mux.HandleFunc(route.Method+" "+route.Pattern, accesslog.HandlerFunc(route.Pattern, h))
	}
}

// warn logs a request's path whole.
func warn(r *http.Request) {
	slog.Warn("x", "path", r.URL.Path)
}
