package api

// rls_test_helpers_test.go holds the three helpers every test of this package builds a server or
// calls a method on under row security with (plan D1, "Every existing api, routes and docs test
// runs on the request store"): testDeps for the stores, viewed for a handler served without the
// route table's chain, and testViewer for a method called with a bare context that does request
// work.

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// testDeps is the stores a test server of this package runs on, as production's do: Store is the
// request store of database (storetest.Request), whose statements run as dispatch_app under the
// viewer each request's context carries, and System is database itself, the system store the
// work before the viewer and the API's own work run on. A test fills the rest of the input; a
// Deps literal takes its Store and System from the same answer.
func testDeps(t testing.TB, database *store.Store) DepsInput {
	t.Helper()
	return DepsInput{Store: storetest.Request(t, database), System: database}
}

// viewed is handler behind the viewer layer production mounts every row of the route table behind
// (withViewer), for a test that serves a handler of s directly rather than through the mux: the
// handler then reads under the viewer of the request's own caller, as it does in production.
func viewed(s *server, handler http.HandlerFunc) http.HandlerFunc {
	return s.withViewer(handler)
}

// testViewer is ctx carrying viewer (store.WithViewer), for a test that calls a method doing
// request work with a bare context: viewer is the one the test's caller resolves to, so the
// method's statements on the request store run as they run for that caller in production.
func testViewer(t testing.TB, ctx context.Context, viewer model.Viewer) context.Context {
	t.Helper()
	return store.WithViewer(ctx, viewer)
}
