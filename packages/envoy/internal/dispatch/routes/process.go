package routes

import (
	"net/http"

	"github.com/sjawhar/envoy/internal/dispatch/readclass"
)

// Process is the handler a Dispatch process serves: its two probes, GET /healthz and GET /livez,
// mounted above app, the router New returns, which answers every other path. Go's ServeMux
// prefers the longer pattern, so each probe is answered whatever the router is doing.
//
// health is the readiness probe cmd/dispatch builds (healthzHandler). /livez answers 204 and
// touches nothing: it says only that the process answers, for a liveness or startup probe, which
// must not wait on or fail with Postgres, NATS or the file store. The two probes need no
// credential; every route of app but sign-in and the static web app needs one
// (TestEveryRouteNeedsACredentialButTheNamedOnes), GET /metrics among them, which is a row of the
// API's route table.
//
// Each pattern is mounted with what it reads (readclass.Handle): the probes read the process's own
// health, and app reads what each of its routes declares. The mux is answered as itself so a walk
// of what the process serves can read its patterns (readclass.Patterns).
func Process(app, health http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	none := []readclass.Class{readclass.None}
	readclass.Handle(mux, none, "GET /healthz", health)
	readclass.Handle(mux, none, "GET /livez", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	readclass.Handle(mux, none, "/", app)
	return mux
}
